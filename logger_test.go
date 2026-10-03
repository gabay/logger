package logger

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"
)

// intPtr returns a pointer to v.
func intPtr(v int) *int { return &v }

// newTestMiddleware creates a middleware for cfg and registers a cleanup that
// stops its writer.
func newTestMiddleware(t *testing.T, cfg *Config, next http.Handler) *Middleware {
	t.Helper()

	if next == nil {
		next = http.HandlerFunc(func(http.ResponseWriter, *http.Request) {})
	}

	handler, err := New(context.Background(), next, cfg, "test")
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	m, ok := handler.(*Middleware)
	if !ok {
		t.Fatalf("New returned %T", handler)
	}

	t.Cleanup(func() { releaseWriter(t, m.writer) })

	return m
}

// releaseWriter stops w and removes it from the registry.
func releaseWriter(tb testing.TB, w *fileWriter) {
	tb.Helper()

	registry.mu.Lock()
	if registry.writers[w.path] == w {
		delete(registry.writers, w.path)
	}
	registry.mu.Unlock()

	if err := w.close(); err != nil {
		tb.Errorf("closing writer: %v", err)
	}
}

// rotateNow rotates synchronously, including the background cleanup.
func rotateNow(w *fileWriter) {
	w.do(w.rotate)
	w.bg.Wait()
}

// flushNow waits until every queued entry is written to disk.
func flushNow(w *fileWriter) {
	w.do(w.drain)
}

// serve sends a GET request for target through h.
func serve(h http.Handler, target string) {
	req := httptest.NewRequest(http.MethodGet, target, nil)
	h.ServeHTTP(httptest.NewRecorder(), req)
}

// readFile returns the content of name, failing the test on error.
func readFile(t *testing.T, name string) string {
	t.Helper()

	data, err := os.ReadFile(name)
	if err != nil {
		t.Fatal(err)
	}

	return string(data)
}

func TestNew_InvalidConfig(t *testing.T) {
	file := filepath.Join(t.TempDir(), "access.log")

	tests := []struct {
		name    string
		cfg     *Config
		wantErr string
	}{
		{name: "nil config", cfg: nil, wantErr: "missing configuration"},
		{name: "missing file", cfg: &Config{}, wantErr: "file is required"},
		{name: "blank file", cfg: &Config{File: "  "}, wantErr: "file is required"},
		{name: "bad format", cfg: &Config{File: file, Format: "{.Bad}"}, wantErr: "can't evaluate field Bad"},
		{
			name:    "bad schedule",
			cfg:     &Config{File: file, Rotate: &RotateConfig{Schedule: "every day"}},
			wantErr: "invalid rotate.schedule",
		},
		{
			name:    "negative keep",
			cfg:     &Config{File: file, Rotate: &RotateConfig{Keep: intPtr(-1)}},
			wantErr: "must not be negative",
		},
		{
			name:    "negative keepCompressed",
			cfg:     &Config{File: file, Rotate: &RotateConfig{KeepCompressed: intPtr(-1)}},
			wantErr: "must not be negative",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := New(context.Background(), http.NotFoundHandler(), test.cfg, "test")
			if err == nil || !strings.Contains(err.Error(), test.wantErr) {
				t.Fatalf("got error %v, want it to contain %q", err, test.wantErr)
			}
		})
	}
}

func TestNew_CreatesDirectory(t *testing.T) {
	file := filepath.Join(t.TempDir(), "nested", "dir", "access.log")
	newTestMiddleware(t, &Config{File: file}, nil)

	if _, err := os.Stat(file); err != nil {
		t.Fatalf("log file was not created: %v", err)
	}
}

func TestServeHTTP_DefaultFormat(t *testing.T) {
	file := filepath.Join(t.TempDir(), "access.log")
	next := http.HandlerFunc(func(rw http.ResponseWriter, _ *http.Request) {
		rw.Header().Set("Content-Length", "5")
		rw.WriteHeader(http.StatusCreated)
		_, _ = rw.Write([]byte("hello"))
	})

	cfg := CreateConfig()
	cfg.File = file
	m := newTestMiddleware(t, cfg, next)

	req := httptest.NewRequest(http.MethodPost, "/path?q=1", nil)
	req.RemoteAddr = "192.0.2.10:1234"
	req.SetBasicAuth("alice", "secret")
	req.Header.Set("Referer", "https://ref.example/")
	req.Header.Set("User-Agent", `curl/8 "quoted"`)

	rw := httptest.NewRecorder()
	m.ServeHTTP(rw, req)

	if rw.Code != http.StatusCreated || rw.Body.String() != "hello" {
		t.Fatalf("response altered: %d %q", rw.Code, rw.Body.String())
	}

	flushNow(m.writer)

	pattern := `^192\.0\.2\.10 - alice \[\d{2}/\w{3}/\d{4}:\d{2}:\d{2}:\d{2} [+-]\d{4}\] "POST /path\?q=1" 201 5 ` +
		`"https://ref\.example/" "curl/8 \\"quoted\\""\n$`
	if got := readFile(t, file); !regexp.MustCompile(pattern).MatchString(got) {
		t.Errorf("unexpected log line %q", got)
	}
}

func TestServeHTTP_ResponseSize(t *testing.T) {
	file := filepath.Join(t.TempDir(), "access.log")
	next := http.HandlerFunc(func(rw http.ResponseWriter, req *http.Request) {
		switch req.URL.Path {
		case "/length":
			// Set by the handler, or copied from the upstream by Traefik's proxy.
			rw.Header().Set("Content-Length", "11")
			_, _ = rw.Write([]byte("hello world"))
		case "/chunked":
			// Streamed: no Content-Length. (Flusher is not available under
			// Yaegi: interpreted ResponseWriter wrappers only expose Hijacker.)
			_, _ = rw.Write([]byte("chunk"))
			if flusher, ok := rw.(http.Flusher); ok {
				flusher.Flush()
			}
		case "/invalid":
			rw.Header().Set("Content-Length", "lots")
		}
	})
	m := newTestMiddleware(t, &Config{File: file, Format: "{.Path} {.ResponseSize}"}, next)

	serve(m, "/length")
	serve(m, "/chunked")
	serve(m, "/invalid")
	flushNow(m.writer)

	if got := readFile(t, file); got != "/length 11\n/chunked -\n/invalid -\n" {
		t.Errorf("got %q", got)
	}
}

func TestServeHTTP_FormatErrorSkipsLine(t *testing.T) {
	file := filepath.Join(t.TempDir(), "access.log")
	// Valid at configuration time (Status is "" then), failing at runtime.
	m := newTestMiddleware(t, &Config{File: file, Format: `{if .Status}{index .Status 9}{end}{.Path}`}, nil)

	serve(m, "/a")
	flushNow(m.writer)

	if got := readFile(t, file); got != "" {
		t.Errorf("got %q, want no partial line", got)
	}
}

func TestServeHTTP_CustomFormat(t *testing.T) {
	file := filepath.Join(t.TempDir(), "access.log")
	m := newTestMiddleware(t, &Config{File: file, Format: `{"{"}"method":"{.Method}","user":"{.User}","status":{.Status}{"}"}`}, nil)

	serve(m, "/a")
	serve(m, "/b")
	flushNow(m.writer)

	want := `{"method":"GET","user":"-","status":200}` + "\n" + `{"method":"GET","user":"-","status":200}` + "\n"
	if got := readFile(t, file); got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestServeHTTP_AppendsToExistingFile(t *testing.T) {
	file := filepath.Join(t.TempDir(), "access.log")
	if err := os.WriteFile(file, []byte("previous\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	m := newTestMiddleware(t, &Config{File: file, Format: "{.Path}"}, nil)
	serve(m, "/new")
	flushNow(m.writer)

	if got := readFile(t, file); got != "previous\n/new\n" {
		t.Errorf("got %q", got)
	}
}

func TestServeHTTP_DoesNotBlockWhenQueueIsFull(t *testing.T) {
	file := filepath.Join(t.TempDir(), "access.log")
	m := newTestMiddleware(t, &Config{File: file, Format: "{.Path}"}, nil)

	// Stall the writer goroutine.
	release := make(chan struct{})
	stalled := make(chan struct{})

	go m.writer.do(func() { close(stalled); <-release })
	<-stalled

	const extra = 10

	done := make(chan struct{})

	go func() {
		defer close(done)

		for i := 0; i < queueSize+extra; i++ {
			serve(m, "/x")
		}
	}()

	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("ServeHTTP blocked on a full queue")
	}

	if got := m.writer.dropped.Load(); got != extra {
		t.Errorf("dropped = %d, want %d", got, extra)
	}

	close(release)
	flushNow(m.writer)

	if got := strings.Count(readFile(t, file), "\n"); got != queueSize {
		t.Errorf("wrote %d lines, want %d", got, queueSize)
	}

	if got := m.writer.dropped.Load(); got != 0 {
		t.Errorf("dropped counter not reset after reporting: %d", got)
	}
}

func TestSharedWriter(t *testing.T) {
	file := filepath.Join(t.TempDir(), "access.log")
	a := newTestMiddleware(t, &Config{File: file, Format: "a {.Path}"}, nil)
	b := newTestMiddleware(t, &Config{File: file, Format: "b {.Path}"}, nil)

	if a.writer != b.writer {
		t.Fatal("middlewares logging to the same file must share a writer")
	}

	serve(a, "/1")
	serve(b, "/2")
	flushNow(a.writer)

	if got := readFile(t, file); got != "a /1\nb /2\n" {
		t.Errorf("got %q", got)
	}
}

func TestRotateConfig(t *testing.T) {
	dir := t.TempDir()

	t.Run("no rotate section disables rotation", func(t *testing.T) {
		m := newTestMiddleware(t, &Config{File: filepath.Join(dir, "none.log")}, nil)

		var armed bool

		m.writer.do(func() { armed = m.writer.timerC != nil })

		if m.writer.rot != nil || armed {
			t.Errorf("rotation enabled: %v, timer armed: %v", m.writer.rot, armed)
		}
	})

	t.Run("empty rotate section uses defaults", func(t *testing.T) {
		m := newTestMiddleware(t, &Config{File: filepath.Join(dir, "defaults.log"), Rotate: &RotateConfig{}}, nil)

		rot := m.writer.rot
		if rot.spec != DefaultSchedule || rot.keep != DefaultKeep || rot.keepCompressed != DefaultKeepCompressed {
			t.Errorf("got %s", rot)
		}

		var next time.Time

		m.writer.do(func() { next = m.writer.next })

		if next.Hour() != 0 || next.Minute() != 0 || !next.After(time.Now()) || next.After(time.Now().Add(24*time.Hour)) {
			t.Errorf("next rotation at %v, want the next midnight", next)
		}
	})

	t.Run("explicit zero values are kept", func(t *testing.T) {
		m := newTestMiddleware(t, &Config{
			File:   filepath.Join(dir, "zero.log"),
			Rotate: &RotateConfig{Schedule: "@hourly", Keep: intPtr(0), KeepCompressed: intPtr(3)},
		}, nil)

		if rot := m.writer.rot; rot.spec != "@hourly" || rot.keep != 0 || rot.keepCompressed != 3 {
			t.Errorf("got %s", rot)
		}
	})
}

func TestReconfigure(t *testing.T) {
	file := filepath.Join(t.TempDir(), "access.log")
	m := newTestMiddleware(t, &Config{File: file, Format: "{.Path}", Rotate: &RotateConfig{Keep: intPtr(3)}}, nil)

	for _, path := range []string{"/1", "/2", "/3", "/4"} {
		serve(m, path)
		flushNow(m.writer)
		rotateNow(m.writer)
	}

	assertBackups(t, file, map[string]string{".1": "/4\n", ".2": "/3\n", ".3": "/2\n"})

	// A configuration reload with a smaller retention cleans up immediately.
	m2 := newTestMiddleware(t, &Config{File: file, Rotate: &RotateConfig{Keep: intPtr(1), KeepCompressed: intPtr(1)}}, nil)
	if m2.writer != m.writer {
		t.Fatal("writer not shared")
	}

	assertBackups(t, file, map[string]string{".1": "/4\n", ".2.gz": "/3\n"})

	// Removing the rotate section stops rotation but keeps the backups.
	newTestMiddleware(t, &Config{File: file}, nil)

	var armed bool

	m.writer.do(func() { armed = m.writer.timerC != nil })

	if armed {
		t.Error("rotation timer still armed")
	}

	serve(m, "/5")
	flushNow(m.writer)
	rotateNow(m.writer)

	assertBackups(t, file, map[string]string{".1": "/4\n", ".2.gz": "/3\n"})
}

func TestScheduledRotation(t *testing.T) {
	if testing.Short() {
		t.Skip("waits for a scheduled rotation")
	}

	file := filepath.Join(t.TempDir(), "access.log")
	m := newTestMiddleware(t, &Config{File: file, Format: "{.Path}", Rotate: &RotateConfig{Schedule: "* * * * * * *"}}, nil)

	serve(m, "/before")

	deadline := time.Now().Add(5 * time.Second)
	for {
		if data, err := os.ReadFile(file + ".1"); err == nil {
			if string(data) != "/before\n" {
				t.Fatalf("rotated content %q", data)
			}

			break
		}

		if time.Now().After(deadline) {
			t.Fatal("the file was not rotated on schedule")
		}

		time.Sleep(50 * time.Millisecond)
	}
}

func TestOnTimer_WaitsForWallClock(t *testing.T) {
	file := filepath.Join(t.TempDir(), "access.log")
	m := newTestMiddleware(t, &Config{File: file, Format: "{.Path}", Rotate: &RotateConfig{Schedule: "@yearly"}}, nil)

	serve(m, "/x")
	flushNow(m.writer)

	// A timer firing before the scheduled wall-clock time must not rotate.
	m.writer.do(func() {
		m.writer.onTimer(m.writer.next.Add(-time.Second))
	})

	if _, err := os.Stat(file + ".1"); err == nil {
		t.Error("rotated before the scheduled time")
	}
}
