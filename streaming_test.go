package logger

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
)

func boolPtr(b bool) *bool { return &b }

func TestNewStreaming(t *testing.T) {
	s, err := newStreaming(&Config{})
	if err != nil || !s.detect || len(s.paths) != 0 {
		t.Errorf("default = %+v, %v; want detection on", s, err)
	}

	s, err = newStreaming(&Config{DetectStreaming: boolPtr(false), StreamingPaths: []string{"/events/"}})
	if err != nil || s.detect || len(s.paths) != 1 {
		t.Errorf("configured = %+v, %v", s, err)
	}

	if _, err := newStreaming(&Config{StreamingPaths: []string{"events"}}); err == nil {
		t.Error("relative streaming path accepted")
	}
}

func TestIsStreaming(t *testing.T) {
	detect := streaming{detect: true}
	paths := streaming{paths: []string{"/events/", "/feed"}}

	tests := []struct {
		name    string
		s       streaming
		path    string
		headers map[string]string
		want    bool
	}{
		{"plain request", detect, "/", nil, false},
		{"server-sent events", detect, "/", map[string]string{"Accept": "text/event-stream"}, true},
		{"accept list", detect, "/", map[string]string{"Accept": "text/html, text/event-stream;q=0.9"}, true},
		{"grpc", detect, "/pkg.Service/Method", map[string]string{"Content-Type": "application/grpc"}, true},
		{"grpc+proto", detect, "/", map[string]string{"Content-Type": "application/grpc+proto"}, true},
		{"json", detect, "/", map[string]string{"Content-Type": "application/json"}, false},
		{"detection disabled", paths, "/", map[string]string{"Accept": "text/event-stream"}, false},
		{"path prefix", paths, "/events/live", nil, true},
		{"path prefix without slash", paths, "/feeds", nil, true},
		{"other path", paths, "/event", nil, false},
	}

	for _, test := range tests {
		req := httptest.NewRequest(http.MethodGet, test.path, nil)
		for key, value := range test.headers {
			req.Header.Set(key, value)
		}

		if got := isStreaming(&test.s, req); got != test.want {
			t.Errorf("%s: isStreaming = %v, want %v", test.name, got, test.want)
		}
	}
}

func TestServeHTTP_StreamingBypass(t *testing.T) {
	file := filepath.Join(t.TempDir(), "access.log")

	// wrapped records whether the next handler got another writer than
	// outer, the one passed to the middleware. (Yaegi rejects a type
	// assertion to *responseRecorder.)
	var (
		outer   http.ResponseWriter
		wrapped bool
	)

	next := http.HandlerFunc(func(rw http.ResponseWriter, _ *http.Request) {
		wrapped = rw != outer

		rw.WriteHeader(http.StatusAccepted)
	})

	m := newTestMiddleware(t, &Config{File: file, Format: "{.Path} {.Status}", StreamingPaths: []string{"/live/"}}, next)

	requests := []struct {
		path, accept string
		wantWrapped  bool
	}{
		{"/page", "", true},
		{"/sse", "text/event-stream", false},
		{"/live/feed", "", false},
	}

	for _, r := range requests {
		req := httptest.NewRequest(http.MethodGet, r.path, nil)
		if r.accept != "" {
			req.Header.Set("Accept", r.accept)
		}

		outer = httptest.NewRecorder()
		m.ServeHTTP(outer, req)

		if wrapped != r.wantWrapped {
			t.Errorf("%s: wrapped = %v, want %v", r.path, wrapped, r.wantWrapped)
		}
	}

	flushNow(m.writer)

	want := "/page 202\n/sse -\n/live/feed -\n"
	if got := readFile(t, file); got != want {
		t.Errorf("got\n%s\nwant\n%s", got, want)
	}
}

func TestServeHTTP_NoWrapperWithoutStatus(t *testing.T) {
	file := filepath.Join(t.TempDir(), "access.log")

	var (
		outer   http.ResponseWriter = httptest.NewRecorder()
		wrapped bool
	)

	next := http.HandlerFunc(func(rw http.ResponseWriter, _ *http.Request) {
		wrapped = rw != outer
	})

	m := newTestMiddleware(t, &Config{File: file, Format: "{.Path}"}, next)
	m.ServeHTTP(outer, httptest.NewRequest(http.MethodGet, "/", nil))

	if wrapped {
		t.Error("response writer wrapped although the format does not use {.Status}")
	}
}
