package logger

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"
)

// discardWriter is a minimal ResponseWriter, cheaper than httptest.ResponseRecorder,
// so benchmarks measure the middleware rather than the recorder.
type discardWriter struct{ header http.Header }

func (d *discardWriter) Header() http.Header         { return d.header }
func (d *discardWriter) Write(b []byte) (int, error) { return len(b), nil }
func (d *discardWriter) WriteHeader(int)             {}

// newBenchRequest returns a realistic request.
func newBenchRequest() *http.Request {
	req := httptest.NewRequest(http.MethodGet, "/api/v1/items?page=2", nil)
	req.RemoteAddr = "203.0.113.7:51234"
	req.Header.Set("User-Agent", "Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/120.0 Safari/537.36")
	req.Header.Set("Referer", "https://example.com/start")

	return req
}

var benchBody = []byte("hello world")

// BenchmarkServeHTTP measures the overhead added to the request path, in
// parallel. Entries the writer cannot keep up with are dropped (reported as
// dropped/op) rather than slowing requests down.
func BenchmarkServeHTTP(b *testing.B) {
	next := http.HandlerFunc(func(rw http.ResponseWriter, _ *http.Request) {
		rw.Header().Set("Content-Length", "11")
		_, _ = rw.Write(benchBody)
	})

	handler, err := New(context.Background(), next, &Config{File: filepath.Join(b.TempDir(), "access.log")}, "bench")
	if err != nil {
		b.Fatal(err)
	}

	m := handler.(*Middleware)
	defer releaseWriter(b, m.writer)

	b.ReportAllocs()
	b.ResetTimer()

	b.RunParallel(func(pb *testing.PB) {
		req := newBenchRequest()
		rw := &discardWriter{header: http.Header{}}

		for pb.Next() {
			m.ServeHTTP(rw, req)
		}
	})

	b.StopTimer()

	var dropped uint64

	m.writer.do(func() { dropped = m.writer.droppedTotal + m.writer.dropped.Load() })
	b.ReportMetric(float64(dropped)/float64(b.N), "dropped/op")
}

// BenchmarkNextOnly is the baseline for BenchmarkServeHTTP: the same handler
// without the middleware.
func BenchmarkNextOnly(b *testing.B) {
	next := http.HandlerFunc(func(rw http.ResponseWriter, _ *http.Request) { _, _ = rw.Write(benchBody) })

	b.ReportAllocs()

	b.RunParallel(func(pb *testing.PB) {
		req := newBenchRequest()
		rw := &discardWriter{header: http.Header{}}

		for pb.Next() {
			next.ServeHTTP(rw, req)
		}
	})
}

// BenchmarkWriterThroughput measures the writer goroutine side: formatting
// and buffered writing of one entry to a real file.
func BenchmarkWriterThroughput(b *testing.B) {
	w, err := newFileWriter(filepath.Join(b.TempDir(), "access.log"), nil, DefaultQueueSize)
	if err != nil {
		b.Fatal(err)
	}

	defer releaseWriter(b, w)

	f, err := parseFormat(Default(), nil)
	if err != nil {
		b.Fatal(err)
	}

	e := benchEntry(f)

	b.ReportAllocs()
	b.ResetTimer()

	// Run on the writer goroutine, which owns the buffer.
	w.do(func() {
		for i := 0; i < b.N; i++ {
			e.start = e.start.Add(time.Millisecond)
			w.write(e)
		}

		w.flush()
	})
}

// BenchmarkFormat measures formatting a default-format line: building the
// escaped Fields and executing the template.
func BenchmarkFormat(b *testing.B) {
	f, err := parseFormat(Default(), nil)
	if err != nil {
		b.Fatal(err)
	}

	e := benchEntry(f)

	var buf bytes.Buffer

	b.ReportAllocs()
	b.ResetTimer()

	for i := 0; i < b.N; i++ {
		buf.Reset()

		if err := f.execute(&buf, e); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkFormatClientIp measures {.ClientIp} resolution through two
// trusted forwarders.
func BenchmarkFormatClientIp(b *testing.B) {
	trusted, err := parseTrustedForwarders([]string{"10.0.0.0/8", "192.0.2.0/24"})
	if err != nil {
		b.Fatal(err)
	}

	f, err := parseFormat("{.ClientIp}", trusted)
	if err != nil {
		b.Fatal(err)
	}

	e := benchEntry(f)
	e.remoteAddr = "192.0.2.10:1234"
	e.forwardedFor = "198.51.100.1, 10.0.0.5"

	var buf bytes.Buffer

	b.ReportAllocs()
	b.ResetTimer()

	for i := 0; i < b.N; i++ {
		buf.Reset()

		if err := f.execute(&buf, e); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkEscape compares values that need no escaping (returned as-is)
// with values that do.
func BenchmarkEscape(b *testing.B) {
	for name, value := range map[string]string{
		"clean":   "Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36",
		"escaped": `Mozilla/5.0 "evil"\n` + "\x01",
	} {
		b.Run(name, func(b *testing.B) {
			b.ReportAllocs()

			for i := 0; i < b.N; i++ {
				_ = escape(value)
			}
		})
	}
}

// benchEntry returns a realistic entry.
func benchEntry(f *format) *entry {
	return &entry{
		format:        f,
		start:         time.Now(),
		duration:      3 * time.Millisecond,
		remoteAddr:    "203.0.113.7:51234",
		method:        http.MethodGet,
		uri:           "/api/v1/items?page=2",
		proto:         "HTTP/1.1",
		host:          "example.com",
		referer:       "https://example.com/start",
		userAgent:     "Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/120.0 Safari/537.36",
		status:        http.StatusOK,
		contentLength: "11",
	}
}
