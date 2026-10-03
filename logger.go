// Package logger is a Traefik (Yaegi) middleware plugin that writes access logs
// to a file.
//
// The log line layout is a Go template using "{" and "}" as delimiters (see
// [Default] and [Fields]) and the file can optionally be rotated on a cron schedule, keeping
// a number of plain-text backups followed by a number of gzip-compressed ones.
//
// The request path is kept as cheap as possible: the middleware only captures
// the handful of values the configured format needs and hands them to a
// per-file background worker over a buffered channel. Formatting, writing,
// rotation and compression never happen on the request goroutine, and a full
// channel results in a dropped (and counted) log entry instead of a blocked
// response.
package logger

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"path/filepath"
	"strings"
	"time"
)

// Rotation defaults, applied when a rotate section is present but leaves a field unset.
const (
	// DefaultSchedule rotates the log every day at midnight (local time of the Traefik process).
	DefaultSchedule = "0 0 * * *"
	// DefaultKeep is the number of plain-text backups kept by default.
	DefaultKeep = 1
	// DefaultKeepCompressed is the number of gzip-compressed backups kept by default.
	DefaultKeepCompressed = 0
)

// Config is the plugin configuration, decoded by Traefik from the dynamic configuration.
type Config struct {
	// File is the path of the access log. Relative paths are resolved against
	// the working directory of the Traefik process. Required.
	File string `json:"file,omitempty"`
	// Format is the layout of a single log line: a text/template with "{"
	// and "}" delimiters, executed with [Fields]. Empty means [Default].
	Format string `json:"format,omitempty"`
	// Rotate enables scheduled rotation. When nil, the file is never rotated.
	Rotate *RotateConfig `json:"rotate,omitempty"`
}

// RotateConfig configures scheduled log rotation.
//
// Backups are named <file>.1 … <file>.<keep> (plain text, newest first)
// followed by <file>.<keep+1>.gz … <file>.<keep+keepCompressed>.gz. Older
// backups are deleted.
type RotateConfig struct {
	// Schedule is a cron expression: standard 5 fields, 7 fields (with seconds
	// and year) or a descriptor such as "@daily". It is evaluated in the local
	// time zone of the Traefik process. Empty means [DefaultSchedule].
	Schedule string `json:"schedule,omitempty"`
	// Keep is the number of plain-text backups. Nil means [DefaultKeep].
	Keep *int `json:"keep,omitempty"`
	// KeepCompressed is the number of gzip-compressed backups kept after the
	// plain-text ones. Nil means [DefaultKeepCompressed].
	KeepCompressed *int `json:"keepCompressed,omitempty"`
}

// CreateConfig creates the default plugin configuration.
func CreateConfig() *Config {
	return &Config{}
}

// Middleware is the access log middleware returned by [New].
type Middleware struct {
	next   http.Handler
	name   string
	format *format
	writer *fileWriter
}

// New creates the access log middleware.
//
// Middlewares (in this or later configuration generations) that point to the
// same file share a single background writer; the most recently applied
// rotation settings win.
func New(_ context.Context, next http.Handler, config *Config, name string) (http.Handler, error) {
	if config == nil {
		return nil, errors.New("logger: missing configuration")
	}

	if strings.TrimSpace(config.File) == "" {
		return nil, errors.New("logger: file is required")
	}

	path, err := filepath.Abs(config.File)
	if err != nil {
		return nil, fmt.Errorf("logger: resolving file %q: %w", config.File, err)
	}

	layout := config.Format
	if layout == "" {
		layout = Default()
	}

	compiled, err := parseFormat(layout)
	if err != nil {
		return nil, fmt.Errorf("logger: %w", err)
	}

	var rot *rotation
	if config.Rotate != nil {
		rot, err = newRotation(config.Rotate)
		if err != nil {
			return nil, fmt.Errorf("logger: %w", err)
		}
	}

	writer, err := acquireWriter(path, rot)
	if err != nil {
		return nil, fmt.Errorf("logger: %w", err)
	}

	return &Middleware{next: next, name: name, format: compiled, writer: writer}, nil
}

// ServeHTTP calls the next handler and enqueues an access log entry for it.
// It never blocks on logging: if the writer is saturated the entry is dropped.
func (m *Middleware) ServeHTTP(rw http.ResponseWriter, req *http.Request) {
	used := &m.format.used

	// Capture the request side before calling next so that downstream
	// handlers mutating the request do not affect the log line. Strings are
	// shared, not copied.
	e := entry{
		format:     m.format,
		start:      time.Now(),
		remoteAddr: req.RemoteAddr,
		method:     req.Method,
		uri:        req.RequestURI,
		proto:      req.Proto,
		host:       req.Host,
	}

	if e.uri == "" {
		// RequestURI is only empty for client-style requests (e.g. in tests).
		e.uri = req.URL.RequestURI()
	}

	if used.referer {
		e.referer = headerValue(req.Header, "Referer")
	}

	if used.userAgent {
		e.userAgent = headerValue(req.Header, "User-Agent")
	}

	if used.user {
		e.authorization = headerValue(req.Header, "Authorization")
	}

	rec := &responseRecorder{ResponseWriter: rw}
	m.next.ServeHTTP(rec, req)

	e.duration = time.Since(e.start)
	e.status = rec.statusCode()
	// The response size is taken from Content-Length rather than by counting
	// written bytes: wrapping Write would put an interpreted call on every
	// body write under Yaegi.
	e.contentLength = headerValue(rw.Header(), "Content-Length")

	m.writer.log(&e)
}

// headerValue returns the first value of a header whose key is already in
// canonical form, avoiding the canonicalization cost of [http.Header.Get].
func headerValue(h http.Header, canonicalKey string) string {
	if v := h[canonicalKey]; len(v) > 0 {
		return v[0]
	}

	return ""
}
