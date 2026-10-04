package logger

import (
	"bytes"
	"encoding/base64"
	"fmt"
	"io"
	"net"
	"sort"
	"strconv"
	"strings"
	"text/template"
	"text/template/parse"
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
//	{.Ip} - {.User} [{.Time}] "{.Method} {.Path}" {.Status} {.ResponseSize} "{.Referer}" "{.UserAgent}"
//
// A format is a Go [text/template] using "{" and "}" as delimiters, executed
// with a [Fields] value: {.Status} prints the status code, and any template
// action works, e.g. {if eq .Status "500"}ERROR {end}. A literal brace is
// written as a string constant: {"{"} or {"}"}.
func Default() string {
	return `{.Ip} - {.User} [{.Time}] "{.Method} {.Path}" {.Status} {.ResponseSize} "{.Referer}" "{.UserAgent}"`
}

// Fields holds the values available to a format, as {.Name}.
//
// Every value is a string. Empty or unavailable values are "-". Quotes,
// backslashes and control characters in request values are escaped
// JSON-style (\", \\, \n, \u0001...), so values can neither break out of a
// quoted field nor inject extra lines.
type Fields struct {
	// Ip is the address of the direct peer: the request's RemoteAddr without
	// the port. Behind a proxy or load balancer, it is the proxy's address.
	Ip string //nolint:revive // name kept for compatibility with the documented format.
	// ClientIp is the address of the client that originated the request,
	// read from X-Forwarded-For (or X-Real-Ip) when the peer is a trusted
	// forwarder (see Config.TrustedForwarders). Otherwise it equals Ip.
	ClientIp string //nolint:revive // consistent with Ip.
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
	// Status is the response status code, e.g. 200. It is "-" for streaming
	// requests (see Config.DetectStreaming), whose status is not recorded.
	Status string
	// ResponseSize is the Content-Length response header, i.e. the size of
	// the response body in bytes. It is "-" when the header is absent, e.g.
	// for chunked or streamed responses.
	ResponseSize string
	// Referer is the Referer request header.
	Referer string
	// UserAgent is the User-Agent request header.
	UserAgent string
	// Duration is the time spent in the next handlers, in milliseconds.
	Duration string
}

// usedFields records which [Fields] the format uses: only those are captured
// on the request path and computed by the writer. Plain booleans are used
// rather than a bit set because, under Yaegi, a field read is much cheaper
// than a method call.
type usedFields struct {
	ip, clientIP, user, time, method, path, protocol, host bool
	status, responseSize, referer, userAgent, duration     bool
}

// fieldSetters maps every [Fields] name to the flag marking it used.
var fieldSetters = map[string]func(*usedFields){ //nolint:gochecknoglobals // read-only lookup table.
	"Ip":           func(u *usedFields) { u.ip = true },
	"ClientIp":     func(u *usedFields) { u.clientIP = true },
	"User":         func(u *usedFields) { u.user = true },
	"Time":         func(u *usedFields) { u.time = true },
	"Method":       func(u *usedFields) { u.method = true },
	"Path":         func(u *usedFields) { u.path = true },
	"Protocol":     func(u *usedFields) { u.protocol = true },
	"Host":         func(u *usedFields) { u.host = true },
	"Status":       func(u *usedFields) { u.status = true },
	"ResponseSize": func(u *usedFields) { u.responseSize = true },
	"Referer":      func(u *usedFields) { u.referer = true },
	"UserAgent":    func(u *usedFields) { u.userAgent = true },
	"Duration":     func(u *usedFields) { u.duration = true },
}

// fieldDetector walks template parse trees to find the fields they use.
type fieldDetector struct {
	used    usedFields
	unknown []string
}

// detectUsedFields returns the fields referenced by the templates of tmpl,
// and an error if one is not a [Fields] name. Names are checked in every
// branch, unlike execution which only checks the branches taken.
func detectUsedFields(tmpl *template.Template) (usedFields, error) {
	var d fieldDetector

	for _, t := range tmpl.Templates() {
		if t.Tree != nil {
			d.walk(t.Root)
		}
	}

	if len(d.unknown) > 0 {
		names := make([]string, 0, len(fieldSetters))
		for name := range fieldSetters {
			names = append(names, name)
		}

		sort.Strings(names)

		return usedFields{}, fmt.Errorf("unknown field {.%s} (supported: {.%s})", d.unknown[0], strings.Join(names, "}, {."))
	}

	return d.used, nil
}

// walk visits node and its children.
//
//nolint:gocyclo // A flat type switch over the parse node kinds; splitting it would only obscure it.
func (d *fieldDetector) walk(node parse.Node) {
	switch n := node.(type) {
	case *parse.ListNode:
		if n != nil {
			for _, child := range n.Nodes {
				d.walk(child)
			}
		}
	case *parse.ActionNode:
		d.walk(n.Pipe)
	case *parse.IfNode:
		d.walkBranch(&n.BranchNode)
	case *parse.RangeNode:
		d.walkBranch(&n.BranchNode)
	case *parse.WithNode:
		d.walkBranch(&n.BranchNode)
	case *parse.TemplateNode:
		d.walk(n.Pipe)
	case *parse.PipeNode:
		if n != nil {
			for _, cmd := range n.Cmds {
				d.walk(cmd)
			}
		}
	case *parse.CommandNode:
		for _, arg := range n.Args {
			d.walk(arg)
		}
	case *parse.ChainNode:
		d.walk(n.Node)
	case *parse.FieldNode:
		d.mark(n.Ident[0])
	case *parse.VariableNode:
		if len(n.Ident) > 1 {
			d.mark(n.Ident[1]) // $x.Name: assume $x holds the Fields.
		} else {
			d.markAll() // $ or $x may hold the whole Fields.
		}
	case *parse.DotNode:
		d.markAll() // e.g. {printf "%v" .}: every field may be printed.
	}
}

// walkBranch visits an if, range or with node.
func (d *fieldDetector) walkBranch(n *parse.BranchNode) {
	d.walk(n.Pipe)
	d.walk(n.List)
	d.walk(n.ElseList)
}

// mark records name as used.
func (d *fieldDetector) mark(name string) {
	if set, ok := fieldSetters[name]; ok {
		set(&d.used)
	} else {
		d.unknown = append(d.unknown, name)
	}
}

// markAll records every field as used.
func (d *fieldDetector) markAll() {
	for _, set := range fieldSetters {
		set(&d.used)
	}
}

// format is a compiled log line format, with the settings needed to compute
// its fields.
type format struct {
	tmpl    *template.Template
	used    usedFields
	trusted trustedForwarders
}

// parseFormat compiles a format and checks that it executes.
func parseFormat(layout string, trusted trustedForwarders) (*format, error) {
	tmpl, err := template.New("format").Delims(leftDelim, rightDelim).Parse(layout)
	if err != nil {
		return nil, fmt.Errorf("invalid format %q: %w", layout, err)
	}

	used, err := detectUsedFields(tmpl)
	if err != nil {
		return nil, fmt.Errorf("invalid format %q: %w", layout, err)
	}

	// Also execute once, to catch other errors at configuration time rather
	// than on every request.
	if err := tmpl.Execute(io.Discard, &Fields{}); err != nil {
		return nil, fmt.Errorf("invalid format %q: %w", layout, err)
	}

	return &format{tmpl: tmpl, used: used, trusted: trusted}, nil
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
	forwardedFor  string
	realIP        string
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
//nolint:gocyclo,cyclop,gocognit // one flat branch per field: helpers would add interpreted calls.
func (e *entry) fields() *Fields {
	used := &e.format.used
	data := &Fields{
		Ip: empty, ClientIp: empty, User: empty, Time: empty, Method: empty, Path: empty, Protocol: empty,
		Host: empty, Status: empty, ResponseSize: empty, Referer: empty, UserAgent: empty, Duration: empty,
	}

	if (used.ip || used.clientIP) && e.remoteAddr != "" {
		peer := e.remoteAddr
		if host, _, err := net.SplitHostPort(peer); err == nil {
			peer = host
		}

		if used.ip {
			data.Ip = escaper.Replace(peer)
		}

		if used.clientIP {
			data.ClientIp = escape(e.format.trusted.clientIP(peer, e.forwardedFor, e.realIP))
		}
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

	if used.status && e.status > 0 {
		data.Status = strconv.Itoa(e.status)
	}

	if used.responseSize && e.contentLength != "" {
		// Only digits are accepted, so the value needs no escaping.
		if _, err := strconv.ParseUint(e.contentLength, 10, 63); err == nil {
			data.ResponseSize = e.contentLength
		}
	}

	if used.referer && e.referer != "" {
		data.Referer = escaper.Replace(e.referer)
	}

	if used.userAgent && e.userAgent != "" {
		data.UserAgent = escaper.Replace(e.userAgent)
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
