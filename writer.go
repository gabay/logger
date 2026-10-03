package logger

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"
)

const (
	// bufferSize is the size of the in-memory write buffer of a log file.
	bufferSize = 64 << 10

	fileMode = 0o644
	dirMode  = 0o755
)

// registry holds one writer per absolute log file path.
//
// Traefik calls New for every router using the middleware and again on every
// dynamic configuration reload, without ever closing previous instances.
// Sharing writers per file guarantees a single goroutine owns each file (no
// interleaved writes or concurrent rotations) and that reloads do not leak
// goroutines or file descriptors. mu is the global lock serializing writer
// creation, reconfiguration and the startup cleanup of backups.
var registry = struct { //nolint:gochecknoglobals // plugins have no lifecycle hooks: shared state must be global
	mu      sync.Mutex
	writers map[string]*fileWriter
}{writers: make(map[string]*fileWriter)}

// acquireWriter returns the writer for path, creating it with a queue of
// queueSize entries if needed, and applies the rotation settings rot (nil
// disables rotation). The queue of an existing writer cannot be resized: a
// different queueSize is reported and ignored until Traefik restarts.
func acquireWriter(path string, rot *rotation, queueSize int) (*fileWriter, error) {
	registry.mu.Lock()
	defer registry.mu.Unlock()

	if w, ok := registry.writers[path]; ok {
		if size := cap(w.entries); size != queueSize {
			logInfo(fmt.Sprintf("queue size of %s changed from %d to %d: restart Traefik to apply it", path, size, queueSize))
		}

		if !w.rot.equal(rot) {
			logInfo(fmt.Sprintf("rotation settings of %s changed to %s, applying them", path, rot.String()))
			w.configure(rot)
		}

		return w, nil
	}

	w, err := newFileWriter(path, rot, queueSize)
	if err != nil {
		return nil, err
	}

	registry.writers[path] = w

	return w, nil
}

// fileWriter owns a log file. A single goroutine (run) formats and writes
// entries, flushes, and rotates the file, so none of these need locking.
type fileWriter struct {
	// dropped counts entries discarded because the queue was full. It is
	// reported (and reset) by the writer goroutine once the queue drains.
	dropped atomic.Uint64

	path    string
	entries chan entry
	control chan func()
	quit    chan struct{}
	stopped chan struct{}
	stop    sync.Once

	// rot is the applied rotation config; guarded by registry.mu.
	rot *rotation

	// maint guards the backup files (<path>.N[.gz]) and retention: held by
	// rotation, the background cleanup and reconfiguration.
	maint     sync.Mutex
	retention *rotation
	// bg tracks background cleanups started by rotations.
	bg sync.WaitGroup

	// Owned by the writer goroutine.
	file     *os.File
	buf      *bufio.Writer
	size     int64
	line     bytes.Buffer
	schedule *rotation
	timer    *time.Timer
	timerC   <-chan time.Time
	next     time.Time
	lastErr  string
	// lastFormatErr deduplicates template execution error reports.
	lastFormatErr string
	closeErr      error
	// droppedTotal counts the dropped entries reported so far.
	droppedTotal uint64
}

// newFileWriter opens path, cleans up existing backups according to rot and
// starts the writer goroutine, consuming a queue of queueSize entries.
func newFileWriter(path string, rot *rotation, queueSize int) (*fileWriter, error) {
	if err := os.MkdirAll(filepath.Dir(path), dirMode); err != nil {
		return nil, fmt.Errorf("creating log directory: %w", err)
	}

	file, size, err := openLogFile(path)
	if err != nil {
		return nil, err
	}

	w := &fileWriter{
		path:    path,
		entries: make(chan entry, queueSize),
		control: make(chan func()),
		quit:    make(chan struct{}),
		stopped: make(chan struct{}),
		file:    file,
		buf:     bufio.NewWriterSize(file, bufferSize),
		size:    size,
	}

	// The writer goroutine is not running yet, so the schedule can be set directly.
	w.rot = rot
	w.schedule = rot
	w.applyRetention(rot)

	go w.run()

	return w, nil
}

// openLogFile opens path for appending and returns its current size.
func openLogFile(path string) (*os.File, int64, error) {
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND|os.O_CREATE, fileMode)
	if err != nil {
		return nil, 0, fmt.Errorf("opening log file: %w", err)
	}

	info, err := file.Stat()
	if err != nil {
		_ = file.Close()
		return nil, 0, fmt.Errorf("opening log file: %w", err)
	}

	return file, info.Size(), nil
}

// log enqueues an entry without blocking. It is called on request goroutines.
func (w *fileWriter) log(e *entry) {
	select {
	case w.entries <- *e:
	default:
		w.dropped.Add(1)
	}
}

// configure applies new rotation settings: it cleans up the backups for the
// new retention, then updates the writer goroutine's schedule. The caller
// must hold registry.mu.
func (w *fileWriter) configure(rot *rotation) {
	w.rot = rot
	w.applyRetention(rot)
	w.do(func() { w.setSchedule(rot) })
}

// applyRetention records the retention settings and brings the backups in
// line with them. Without rotation, existing backups are left untouched.
func (w *fileWriter) applyRetention(rot *rotation) {
	w.maint.Lock()
	defer w.maint.Unlock()

	w.retention = rot
	if rot == nil {
		return
	}

	if err := cleanupBackups(w.path, rot.keep, rot.keepCompressed); err != nil {
		logError(fmt.Sprintf("cleaning up backups of %s: %v", w.path, err))
	}
}

// do runs fn on the writer goroutine and waits for it to complete. It returns
// immediately if the writer is stopped.
func (w *fileWriter) do(fn func()) {
	done := make(chan struct{})

	select {
	case w.control <- func() { fn(); close(done) }:
	case <-w.stopped:
		return
	}

	select {
	case <-done:
	case <-w.stopped:
	}
}

// close stops the writer goroutine after writing all queued entries, and
// waits for background cleanups. Traefik offers no shutdown hook to plugins:
// this is used by tests.
func (w *fileWriter) close() error {
	w.stop.Do(func() { close(w.quit) })
	<-w.stopped

	return w.closeErr
}

// run is the writer goroutine.
func (w *fileWriter) run() {
	defer close(w.stopped)

	w.setSchedule(w.schedule)

	for {
		select {
		case e := <-w.entries:
			w.write(&e)

			// Flush once the burst is written: idle periods hit the disk
			// immediately while heavy traffic is batched.
			if len(w.entries) == 0 {
				w.flush()
			}
		case now := <-w.timerC:
			w.onTimer(now)
		case fn := <-w.control:
			fn()
		case <-w.quit:
			w.shutdown()
			return
		}
	}
}

// write formats e into the file buffer.
func (w *fileWriter) write(e *entry) {
	// Execute into a separate buffer so that a failing template (e.g. a
	// runtime error in a user action) never leaves a partial line in the file.
	w.line.Reset()

	if err := e.format.execute(&w.line, e); err != nil {
		if msg := err.Error(); msg != w.lastFormatErr {
			w.lastFormatErr = msg
			logError(fmt.Sprintf("formatting a line of %s: %v", w.path, err))
		}

		return
	}

	w.line.WriteByte('\n')

	n, err := w.buf.Write(w.line.Bytes())
	w.size += int64(n)

	if err != nil {
		w.resetBuffer(err)
	}
}

// flush writes buffered data to the file and reports dropped entries.
func (w *fileWriter) flush() {
	if err := w.buf.Flush(); err != nil {
		w.resetBuffer(err)
	} else {
		w.lastErr = ""
	}

	if dropped := w.dropped.Swap(0); dropped > 0 {
		w.droppedTotal += dropped
		logError(fmt.Sprintf("%s: dropped %d entries, the writer could not keep up", w.path, dropped))
	}
}

// resetBuffer reports a write error and discards the buffered data:
// a bufio.Writer stays failed forever once an error occurred, so it must be
// reset to recover from transient errors (e.g. a full disk).
func (w *fileWriter) resetBuffer(err error) {
	if msg := err.Error(); msg != w.lastErr {
		w.lastErr = msg
		logError(fmt.Sprintf("writing %s: %v", w.path, err))
	}

	w.buf.Reset(w.file)
}

// setSchedule (re)arms the rotation timer for rot (nil disables rotation).
func (w *fileWriter) setSchedule(rot *rotation) {
	w.schedule = rot

	if w.timer != nil {
		w.timer.Stop()
		w.timer = nil
	}

	w.timerC = nil
	w.next = time.Time{}

	if rot == nil {
		return
	}

	w.next = rot.schedule.Next(time.Now())
	if w.next.IsZero() {
		return // the schedule never fires
	}

	w.armTimer(time.Now())
}

// armTimer starts a timer firing at w.next.
func (w *fileWriter) armTimer(now time.Time) {
	w.timer = time.NewTimer(w.next.Sub(now))
	w.timerC = w.timer.C
}

// onTimer rotates the file when the scheduled time is reached.
func (w *fileWriter) onTimer(now time.Time) {
	// Timers use the monotonic clock: if the wall clock is behind the
	// schedule (clock adjustments), wait for the remaining time instead of
	// rotating early (and twice).
	if now.Before(w.next) {
		w.armTimer(now)
		return
	}

	w.rotate()
	w.setSchedule(w.schedule)
}

// rotate moves the current file to <path>.1, shifting older backups, and
// reopens the log file. Compression and pruning then run in the background so
// that logging resumes immediately. Empty files are not rotated.
func (w *fileWriter) rotate() {
	if w.size == 0 {
		return
	}

	w.flush()

	w.maint.Lock()
	if w.retention == nil {
		// Rotation was disabled by a concurrent reconfiguration.
		w.maint.Unlock()
		return
	}

	err := w.rotateLocked(w.retention.keep + w.retention.keepCompressed)
	w.maint.Unlock()

	if err != nil {
		logError(fmt.Sprintf("rotating %s: %v", w.path, err))
	}

	w.bg.Add(1)

	go func() {
		defer w.bg.Done()

		w.maint.Lock()
		defer w.maint.Unlock()

		// Use the retention current at cleanup time: it may have been
		// reconfigured since the rotation.
		if rot := w.retention; rot != nil {
			if err := cleanupBackups(w.path, rot.keep, rot.keepCompressed); err != nil {
				logError(fmt.Sprintf("cleaning up backups of %s: %v", w.path, err))
			}
		}
	}()
}

// rotateLocked performs the renames of a rotation, keeping at most total
// backups, and reopens the log file. The caller must hold w.maint. The file
// is always reopened, even on error, so that logging continues.
func (w *fileWriter) rotateLocked(total int) error {
	errs := []error{w.file.Close()}
	errs = append(errs, shiftBackups(w.path, total)...)

	file, size, err := openLogFile(w.path)
	if err != nil {
		// Keep the closed file: writes fail and get reported until the next
		// successful rotation.
		return errors.Join(append(errs, err)...)
	}

	w.file = file
	w.size = size
	w.buf.Reset(file)

	return errors.Join(errs...)
}

// shutdown writes the remaining queued entries, closes the file and waits
// for background cleanups.
func (w *fileWriter) shutdown() {
	if w.timer != nil {
		w.timer.Stop()
	}

	w.drain()
	w.bg.Wait()
	w.closeErr = w.file.Close()
}

// drain writes every queued entry and flushes. It must run on the writer goroutine.
func (w *fileWriter) drain() {
	// Receive in a select: Yaegi cannot take the address of a value received
	// by a plain `e := <-ch` statement.
	for drained := false; !drained; {
		select {
		case e := <-w.entries:
			w.write(&e)
		default:
			drained = true
		}
	}

	w.flush()
}

// logError reports an error through Traefik's log: Yaegi plugins can only log
// through stderr, which Traefik logs at the error level.
func logError(msg string) {
	_, _ = os.Stderr.WriteString("logger plugin: " + msg + "\n")
}

// logInfo reports an informational message through stdout, which Traefik
// logs at the debug level.
func logInfo(msg string) {
	_, _ = os.Stdout.WriteString("logger plugin: " + msg + "\n")
}
