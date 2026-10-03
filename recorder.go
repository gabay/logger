package logger

import (
	"bufio"
	"fmt"
	"net"
	"net/http"
)

// responseRecorder wraps an [http.ResponseWriter] to record the status code.
// It forwards [http.Flusher] and [http.Hijacker] (needed for streaming and
// WebSockets) and supports [http.ResponseController] through Unwrap.
//
// Write is deliberately not wrapped: under Yaegi every call to an interpreted
// method costs microseconds, and the body may be written in many chunks. The
// status of a response written without WriteHeader is 200, which is the
// default reported by statusCode.
type responseRecorder struct {
	http.ResponseWriter

	status int
}

// WriteHeader records the final status code and forwards it.
func (r *responseRecorder) WriteHeader(code int) {
	// Informational responses (other than 101) are not final: keep waiting
	// for the real status.
	if r.status == 0 && (code < 100 || code > 199 || code == http.StatusSwitchingProtocols) {
		r.status = code
	}

	r.ResponseWriter.WriteHeader(code)
}

// Flush implements [http.Flusher] when the wrapped writer supports it.
func (r *responseRecorder) Flush() {
	flusher, ok := r.ResponseWriter.(http.Flusher)
	if !ok {
		return
	}

	if r.status == 0 {
		r.status = http.StatusOK
	}

	flusher.Flush()
}

// Hijack implements [http.Hijacker] when the wrapped writer supports it.
func (r *responseRecorder) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	hijacker, ok := r.ResponseWriter.(http.Hijacker)
	if !ok {
		return nil, nil, fmt.Errorf("%T does not implement http.Hijacker", r.ResponseWriter)
	}

	conn, rw, err := hijacker.Hijack()
	if err == nil && r.status == 0 {
		// The response (typically a protocol upgrade) is written directly
		// on the connection from now on.
		r.status = http.StatusSwitchingProtocols
	}

	return conn, rw, err
}

// Unwrap returns the wrapped writer, for [http.ResponseController].
func (r *responseRecorder) Unwrap() http.ResponseWriter {
	return r.ResponseWriter
}

// statusCode returns the recorded status, defaulting to 200 like net/http
// does when a handler writes a body without calling WriteHeader, or nothing.
func (r *responseRecorder) statusCode() int {
	if r.status == 0 {
		return http.StatusOK
	}

	return r.status
}
