package logger

import (
	"fmt"
	"net/http"
	"strings"
)

// streaming identifies requests whose responses are streamed. Their response
// writer is not wrapped: Yaegi hands a plugin's wrapper back to Traefik
// without its http.Flusher (only http.Hijacker survives), which would hold
// back the streamed response until Traefik's buffer fills or it completes.
type streaming struct {
	// detect enables the detection of Server-Sent Events and gRPC requests.
	detect bool
	// paths are path prefixes of other streaming endpoints.
	paths []string
}

// newStreaming builds the streaming settings of config.
func newStreaming(config *Config) (streaming, error) {
	s := streaming{detect: config.DetectStreaming == nil || *config.DetectStreaming}

	for _, path := range config.StreamingPaths {
		if !strings.HasPrefix(path, "/") {
			return streaming{}, fmt.Errorf("streaming path %q must start with /", path)
		}

		s.paths = append(s.paths, path)
	}

	return s, nil
}

// isStreaming reports whether req expects a streamed response according to s.
//
// It runs on every request, so it reads the header map directly (canonical
// keys) and relies on native strings functions. It is a function rather
// than a method: under Yaegi, calling a method on a struct field allocates
// far more (about 45 more allocations per call, measured).
func isStreaming(s *streaming, req *http.Request) bool {
	if s.detect {
		if accept := req.Header["Accept"]; len(accept) > 0 && strings.Contains(accept[0], "text/event-stream") {
			return true
		}

		if ct := req.Header["Content-Type"]; len(ct) > 0 && strings.HasPrefix(ct[0], "application/grpc") {
			return true
		}
	}

	for _, prefix := range s.paths {
		if strings.HasPrefix(req.URL.Path, prefix) {
			return true
		}
	}

	return false
}
