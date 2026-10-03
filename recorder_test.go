package logger

import (
	"bufio"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestResponseRecorder_Status(t *testing.T) {
	tests := []struct {
		name   string
		handle func(*responseRecorder)
		status int
	}{
		{name: "nothing written", handle: func(*responseRecorder) {}, status: http.StatusOK},
		{
			name:   "body only",
			handle: func(rw *responseRecorder) { _, _ = rw.Write([]byte("hello")) },
			status: http.StatusOK,
		},
		{
			name: "explicit status",
			handle: func(rw *responseRecorder) {
				rw.WriteHeader(http.StatusNotFound)
				_, _ = rw.Write([]byte("no"))
				_, _ = rw.Write([]byte("pe"))
			},
			status: http.StatusNotFound,
		},
		{
			name: "informational then final",
			handle: func(rw *responseRecorder) {
				rw.WriteHeader(http.StatusEarlyHints)
				rw.WriteHeader(http.StatusAccepted)
			},
			status: http.StatusAccepted,
		},
		{
			name:   "switching protocols is final",
			handle: func(rw *responseRecorder) { rw.WriteHeader(http.StatusSwitchingProtocols) },
			status: http.StatusSwitchingProtocols,
		},
		{
			name: "first final status wins",
			handle: func(rw *responseRecorder) {
				rw.WriteHeader(http.StatusBadGateway)
				rw.WriteHeader(http.StatusOK)
			},
			status: http.StatusBadGateway,
		},
		{
			name:   "flush sends headers",
			handle: func(rw *responseRecorder) { rw.Flush() },
			status: http.StatusOK,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			rec := &responseRecorder{ResponseWriter: httptest.NewRecorder()}
			test.handle(rec)

			if got := rec.statusCode(); got != test.status {
				t.Errorf("status = %d, want %d", got, test.status)
			}
		})
	}
}

func TestResponseRecorder_Flush(t *testing.T) {
	inner := httptest.NewRecorder()
	rec := &responseRecorder{ResponseWriter: inner}

	rec.Flush()

	if !inner.Flushed {
		t.Error("Flush was not forwarded")
	}
}

func TestResponseRecorder_Unwrap(t *testing.T) {
	inner := httptest.NewRecorder()
	rec := &responseRecorder{ResponseWriter: inner}

	if rec.Unwrap() != inner {
		t.Error("Unwrap did not return the wrapped writer")
	}
}

func TestResponseRecorder_HijackUnsupported(t *testing.T) {
	rec := &responseRecorder{ResponseWriter: httptest.NewRecorder()}

	if _, _, err := rec.Hijack(); err == nil {
		t.Error("expected an error when the wrapped writer cannot be hijacked")
	}
}

// hijackableWriter is a ResponseWriter supporting Hijack.
type hijackableWriter struct {
	http.ResponseWriter

	conn net.Conn
	err  error
}

func (h *hijackableWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	return h.conn, nil, h.err
}

func TestResponseRecorder_Hijack(t *testing.T) {
	server, client := net.Pipe()
	defer func() { _ = server.Close(); _ = client.Close() }()

	rec := &responseRecorder{ResponseWriter: &hijackableWriter{ResponseWriter: httptest.NewRecorder(), conn: server}}

	conn, _, err := rec.Hijack()
	if err != nil {
		t.Fatal(err)
	}

	if conn != server {
		t.Error("Hijack did not return the wrapped connection")
	}

	if got := rec.statusCode(); got != http.StatusSwitchingProtocols {
		t.Errorf("status = %d, want %d", got, http.StatusSwitchingProtocols)
	}
}

func TestResponseRecorder_HijackError(t *testing.T) {
	rec := &responseRecorder{ResponseWriter: &hijackableWriter{ResponseWriter: httptest.NewRecorder(), err: errors.New("boom")}}

	if _, _, err := rec.Hijack(); err == nil {
		t.Fatal("expected the hijack error")
	}

	if got := rec.statusCode(); got != http.StatusOK {
		t.Errorf("status = %d, want %d", got, http.StatusOK)
	}
}
