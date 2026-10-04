package logger

import (
	"os"
	"path/filepath"
	"testing"
)

// idleCheckNow runs an idle check on the writer goroutine, as the idle
// ticker would.
func idleCheckNow(w *fileWriter) {
	w.do(w.onIdleCheck)
}

// isClosed reports whether the writer closed its file for inactivity.
func isClosed(w *fileWriter) bool {
	var closed bool

	w.do(func() { closed = w.file == nil })

	return closed
}

func TestIdleClose(t *testing.T) {
	file := filepath.Join(t.TempDir(), "access.log")
	m := newTestMiddleware(t, &Config{File: file, Format: "{.Path}"}, nil)

	serve(m, "/1")
	flushNow(m.writer)

	// The first check after activity only clears the activity flag.
	idleCheckNow(m.writer)

	if isClosed(m.writer) {
		t.Fatal("file closed right after activity")
	}

	idleCheckNow(m.writer)

	if !isClosed(m.writer) {
		t.Fatal("file not closed after an idle interval")
	}

	// A closed file can be moved away: the next entry reopens the path.
	if err := os.Rename(file, file+".moved"); err != nil {
		t.Fatal(err)
	}

	serve(m, "/2")
	flushNow(m.writer)

	if isClosed(m.writer) {
		t.Error("file not reopened by an entry")
	}

	if got := readFile(t, file+".moved"); got != "/1\n" {
		t.Errorf("moved file contains %q", got)
	}

	if got := readFile(t, file); got != "/2\n" {
		t.Errorf("reopened file contains %q", got)
	}
}

func TestIdleClose_ActivityKeepsFileOpen(t *testing.T) {
	file := filepath.Join(t.TempDir(), "access.log")
	m := newTestMiddleware(t, &Config{File: file}, nil)

	for i := 0; i < 3; i++ {
		serve(m, "/")
		flushNow(m.writer)
		idleCheckNow(m.writer)
	}

	if isClosed(m.writer) {
		t.Error("file closed despite activity in every interval")
	}
}

func TestIdleClose_RecreatesDirectory(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "logs")
	file := filepath.Join(dir, "access.log")
	m := newTestMiddleware(t, &Config{File: file, Format: "{.Path}"}, nil)

	idleCheckNow(m.writer)
	idleCheckNow(m.writer)

	if err := os.RemoveAll(dir); err != nil {
		t.Fatal(err)
	}

	serve(m, "/after")
	flushNow(m.writer)

	if got := readFile(t, file); got != "/after\n" {
		t.Errorf("file contains %q", got)
	}
}

func TestIdleClose_ReopenFailureRetries(t *testing.T) {
	file := filepath.Join(t.TempDir(), "access.log")
	m := newTestMiddleware(t, &Config{File: file, Format: "{.Path}"}, nil)

	idleCheckNow(m.writer)
	idleCheckNow(m.writer)

	// A directory in place of the file makes reopening fail.
	if err := os.Remove(file); err != nil {
		t.Fatal(err)
	}

	if err := os.Mkdir(file, 0o755); err != nil {
		t.Fatal(err)
	}

	serve(m, "/lost")
	flushNow(m.writer)

	if !isClosed(m.writer) {
		t.Fatal("file reported open after a failed reopen")
	}

	if err := os.Remove(file); err != nil {
		t.Fatal(err)
	}

	serve(m, "/kept")
	flushNow(m.writer)

	if got := readFile(t, file); got != "/kept\n" {
		t.Errorf("file contains %q", got)
	}
}

func TestIdleClose_Rotation(t *testing.T) {
	file := filepath.Join(t.TempDir(), "access.log")
	m := newTestMiddleware(t, &Config{File: file, Format: "{.Path}", Rotate: &RotateConfig{}}, nil)

	serve(m, "/1")
	flushNow(m.writer)
	idleCheckNow(m.writer)
	idleCheckNow(m.writer)

	// The scheduled rotation still happens while the file is closed.
	rotateNow(m.writer)
	assertBackups(t, file, map[string]string{".1": "/1\n"})

	// An empty file is still not rotated, even when its size is unknown.
	idleCheckNow(m.writer)
	idleCheckNow(m.writer)
	rotateNow(m.writer)
	assertBackups(t, file, map[string]string{".1": "/1\n"})

	serve(m, "/2")
	flushNow(m.writer)

	if got := readFile(t, file); got != "/2\n" {
		t.Errorf("current file contains %q", got)
	}
}

func TestIdleClose_Shutdown(t *testing.T) {
	file := filepath.Join(t.TempDir(), "access.log")

	w, err := newFileWriter(file, nil, DefaultQueueSize)
	if err != nil {
		t.Fatal(err)
	}

	idleCheckNow(w)
	idleCheckNow(w)

	if err := w.close(); err != nil {
		t.Errorf("close: %v", err)
	}
}
