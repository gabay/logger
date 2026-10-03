package logger

import (
	"bytes"
	"encoding/base64"
	"fmt"
	"io"
	"net"
	"strconv"
	"strings"
	"text/template"
	"time"
)

// clfTimeLayout is the timestamp layout of the Common Log Format.
const clfTimeLayout = "02/Jan/2006:15:04:05 -0700"

// Template delimiters: placeholders are written {.Name} rather than {{.Name}}.
const (
	leftDelim  = "{"
	rightDelim = "}"
)

// empty is written in place of empty or unavailable values.
const empty = "-"

// Default returns the default log line format: the Common Log Format extended
// with the referer and user-agent fields (also known as the Combined Log
// Format).
//
// A format is a Go [text/template] using "{" and "}" as delimiters, executed
// with a [Fields] value: {.Status} prints the status code, and any template
// action works, e.g. {if eq .Status "500"}ERROR {end}. A literal brace is
// written as a string constant: {"{"} or {"}"}.
func Default() string {
	return `{.Ip} - {.User} [{.Time}] "{.Method} {.Path}" {.Status} {.ResponseSize} "{.HttpReferer}" "{.HttpUserAgent}"`
}

// Fields holds the values available to a format, as {.Name}.
//
// Every value is a string. Empty or unavailable values are "-". Quotes,
// backslashes and control characters in request values are escaped
// JSON-style (\", \\, \n, \u0001...), so values can neither break out of a
// quoted field nor inject extra lines.
type Fields struct {
	// Ip is the client IP address: the request's RemoteAddr without the port.
	Ip string //nolint:revive // name kept for compatibility with the documented format.
	// User is the user name sent with HTTP basic authentication.
	User string
	// Time is the request start time, e.g. 10/Oct/2000:13:55:36 -0700.
	Time string
	// Method is the request method, e.g. GET.
	Method string
	// Path is the request URI, including the query string.
	Path string
	// Protocol is the request protocol, e.g. HTTP/1.1.
	Protocol string
	// Host is the request host (Host header or URL host).
	Host string
	// Status is the response status code, e.g. 200.
	Status string
	// ResponseSize is the Content-Length response header, i.e. the size of
	// the response body in bytes. It is "-" when the header is absent, e.g.
	// for chunked or streamed responses.
	ResponseSize string
	// HttpReferer is the Referer request header.
	HttpReferer string //nolint:revive // name kept for compatibility with the documented format.
	// HttpUserAgent is the User-Agent request header.
	HttpUserAgent string //nolint:revive // name kept for compatibility with the documented format.
	// Duration is the time spent in the next handlers, in milliseconds.
	Duration string
}

// usedFields records which [Fields] the format uses: only those are captured
// on the request path and computed by the writer. Plain booleans are used
// rather than a bit set because, under Yaegi, a field read is much cheaper
// than a method call.
type usedFields struct {
	ip, user, time, method, path, protocol, host, status bool
	responseSize, referer, userAgent, duration           bool
}

// detectUsedFields finds the fields referenced by layout.
//
// A textual check is enough: a false positive (e.g. ".Host" in a literal)
// only costs computing an unused value, and unknown names are rejected by
// parseFormat. No name is a suffix of another (".User" does not match
// ".HttpUserAgent").
func detectUsedFields(layout string) usedFields {
	uses := func(name string) bool { return strings.Contains(layout, "."+name) }

	return usedFields{
		ip:           uses("Ip"),
		user:         uses("User"),
		time:         uses("Time"),
		method:       uses("Method"),
		path:         uses("Path"),
		protocol:     uses("Protocol"),
		host:         uses("Host"),
		status:       uses("Status"),
		responseSize: uses("ResponseSize"),
		referer:      uses("HttpReferer"),
		userAgent:    uses("HttpUserAgent"),
		duration:     uses("Duration"),
	}
}

// format is a compiled log line format.
type format struct {
	tmpl *template.Template
	used usedFields
}

// parseFormat compiles a format and checks that it executes.
func parseFormat(layout string) (*format, error) {
	tmpl, err := template.New("format").Delims(leftDelim, rightDelim).Parse(layout)
	if err != nil {
		return nil, fmt.Errorf("invalid format %q: %w", layout, err)
	}

	// Parsing does not resolve field names: execute once to reject unknown
	// ones (e.g. {.Nope}) at configuration time rather than on every request.
	if err := tmpl.Execute(io.Discard, &Fields{}); err != nil {
		return nil, fmt.Errorf("invalid format %q: %w", layout, err)
	}

	f := &format{tmpl: tmpl, used: detectUsedFields(layout)}

	return f, nil
}

// entry holds the raw values of a single request. It is captured on the
// request goroutine and formatted by the writer goroutine, so the expensive
// work (escaping, decoding, time formatting, template execution) stays off
// the request path.
type entry struct {
	format        *format
	start         time.Time
	duration      time.Duration
	remoteAddr    string
	method        string
	uri           string
	proto         string
	host          string
	referer       string
	userAgent     string
	authorization string
	contentLength string
	status        int
}

// fields builds the template data for e. Values not used by the format are
// left as "-".
//
// This runs interpreted under Yaegi, where every function call and statement
// costs far more than natively, so the code favors direct calls into the
// standard library (which runs compiled) over small helpers: it starts from
// all-"-" values and escapes non-empty strings with escaper.Replace inline,
// and it skips the values the format does not use.
//
//nolint:gocyclo,cyclop // one flat branch per field: helpers would add interpreted calls.
func (e *entry) fields() *Fields {
	used := &e.format.used
	data := &Fields{
		Ip: empty, User: empty, Time: empty, Method: empty, Path: empty, Protocol: empty, Host: empty,
		Status: empty, ResponseSize: empty, HttpReferer: empty, HttpUserAgent: empty, Duration: empty,
	}

	if used.ip && e.remoteAddr != "" {
		ip := e.remoteAddr
		if host, _, err := net.SplitHostPort(ip); err == nil {
			ip = host
		}

		data.Ip = escaper.Replace(ip)
	}

	if used.user && e.authorization != "" {
		data.User = escape(basicAuthUser(e.authorization))
	}

	if used.time {
		data.Time = e.start.Format(clfTimeLayout)
	}

	if used.method && e.method != "" {
		data.Method = escaper.Replace(e.method)
	}

	if used.path && e.uri != "" {
		data.Path = escaper.Replace(e.uri)
	}

	if used.protocol && e.proto != "" {
		data.Protocol = escaper.Replace(e.proto)
	}

	if used.host && e.host != "" {
		data.Host = escaper.Replace(e.host)
	}

	if used.status {
		data.Status = strconv.Itoa(e.status)
	}

	if used.responseSize && e.contentLength != "" {
		// Only digits are accepted, so the value needs no escaping.
		if _, err := strconv.ParseUint(e.contentLength, 10, 63); err == nil {
			data.ResponseSize = e.contentLength
		}
	}

	if used.referer && e.referer != "" {
		data.HttpReferer = escaper.Replace(e.referer)
	}

	if used.userAgent && e.userAgent != "" {
		data.HttpUserAgent = escaper.Replace(e.userAgent)
	}

	if used.duration {
		data.Duration = strconv.FormatInt(e.duration.Milliseconds(), 10)
	}

	return data
}

// execute writes the log line for e, without trailing newline, to buf.
func (f *format) execute(buf *bytes.Buffer, e *entry) error {
	return f.tmpl.Execute(buf, e.fields())
}

// escaper escapes quotes, backslashes and control characters JSON-style.
//
// strings.Replacer is used rather than a hand-written loop because it runs as
// compiled code under Yaegi: with only single-byte patterns it uses a native
// lookup table, and it returns its input unchanged (without allocating) when
// there is nothing to escape.
var escaper = newEscaper() //nolint:gochecknoglobals // immutable and safe for concurrent use.

// newEscaper builds [escaper].
func newEscaper() *strings.Replacer {
	pairs := []string{`"`, `\"`, `\`, `\\`, "\n", `\n`, "\r", `\r`, "\t", `\t`, "\x7f", `\u007f`}

	for c := byte(0); c < 0x20; c++ {
		if c != '\n' && c != '\r' && c != '\t' {
			pairs = append(pairs, string([]byte{c}), fmt.Sprintf(`\u%04x`, c))
		}
	}

	return strings.NewReplacer(pairs...)
}

// escape returns s escaped, or "-" when s is empty.
func escape(s string) string {
	if s == "" {
		return empty
	}

	return escaper.Replace(s)
}

// basicAuthUser extracts the user name from a basic Authorization header value.
func basicAuthUser(authorization string) string {
	const prefix = "Basic "
	if len(authorization) <= len(prefix) || !strings.EqualFold(authorization[:len(prefix)], prefix) {
		return ""
	}

	decoded, err := base64.StdEncoding.DecodeString(authorization[len(prefix):])
	if err != nil {
		return ""
	}

	user, _, _ := strings.Cut(string(decoded), ":")

	return user
}
