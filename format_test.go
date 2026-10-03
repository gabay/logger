package logger

import (
	"bytes"
	"strings"
	"testing"
	"text/template"
	"time"
)

// render executes layout for e.
func render(t *testing.T, layout string, e *entry) string {
	t.Helper()

	f, err := parseFormat(layout, nil)
	if err != nil {
		t.Fatalf("parseFormat(%q): %v", layout, err)
	}

	e.format = f

	var buf bytes.Buffer
	if err := f.execute(&buf, e); err != nil {
		t.Fatalf("execute: %v", err)
	}

	return buf.String()
}

// sampleEntry returns an entry with every value set.
func sampleEntry() *entry {
	return &entry{
		start:         time.Date(2000, time.October, 10, 13, 55, 36, 0, time.FixedZone("", -7*3600)),
		duration:      1500 * time.Millisecond,
		remoteAddr:    "127.0.0.1:54321",
		method:        "GET",
		uri:           "/apache_pb.gif?a=1",
		proto:         "HTTP/1.0",
		host:          "example.com",
		referer:       "http://www.example.com/start.html",
		userAgent:     "Mozilla/4.08",
		authorization: "Basic ZnJhbms6cGFzc3dvcmQ=", // frank:password
		contentLength: "2326",
		status:        200,
	}
}

func TestDefault(t *testing.T) {
	got := render(t, Default(), sampleEntry())
	want := `127.0.0.1 - frank [10/Oct/2000:13:55:36 -0700] "GET /apache_pb.gif?a=1" 200 2326 ` +
		`"http://www.example.com/start.html" "Mozilla/4.08"`

	if got != want {
		t.Errorf("got  %s\nwant %s", got, want)
	}
}

func TestAllFields(t *testing.T) {
	layout := "{.Ip}|{.ClientIp}|{.User}|{.Time}|{.Method}|{.Path}|{.Protocol}|{.Host}|{.Status}|" +
		"{.ResponseSize}|{.Referer}|{.UserAgent}|{.Duration}"
	want := "127.0.0.1|127.0.0.1|frank|10/Oct/2000:13:55:36 -0700|GET|/apache_pb.gif?a=1|HTTP/1.0|example.com|200|" +
		"2326|http://www.example.com/start.html|Mozilla/4.08|1500"

	if got := render(t, layout, sampleEntry()); got != want {
		t.Errorf("got  %s\nwant %s", got, want)
	}
}

func TestEmptyValues(t *testing.T) {
	e := &entry{status: 404}

	got := render(t, Default()+" {.ClientIp} {.Host} {.Protocol} {.Duration}", e)
	want := `- - - [01/Jan/0001:00:00:00 +0000] "- -" 404 - "-" "-" - - - 0`

	if got != want {
		t.Errorf("got  %s\nwant %s", got, want)
	}
}

func TestTemplateFeatures(t *testing.T) {
	e := sampleEntry()

	tests := map[string]string{
		// Literal braces through string constants, e.g. for JSON lines.
		`{"{"}"ip":"{.Ip}","status":{.Status}{"}"}`: `{"ip":"127.0.0.1","status":200}`,
		// Actions and functions.
		`{if eq .Status "200"}OK{else}KO{end}`: "OK",
		`{printf "%-6s|" .Method}`:             "GET   |",
		// Trim markers.
		"a {- .Method -} b": "aGETb",
		"no placeholders":   "no placeholders",
		"":                  "",
	}

	for layout, want := range tests {
		if got := render(t, layout, e); got != want {
			t.Errorf("%s: got %q, want %q", layout, got, want)
		}
	}
}

func TestParseFormat_Errors(t *testing.T) {
	tests := map[string]string{
		"{.Nope}":                  "unknown field {.Nope}",
		"{.ip}":                    "unknown field {.ip}",
		"{.HttpUserAgent}":         "unknown field {.HttpUserAgent}",
		"{if .Status}{.Nope}{end}": "unknown field {.Nope}", // branch not taken at validation
		"{$f := .}{$f.Nope}":       "unknown field {.Nope}",
		"x {.Ip":                   "unclosed action",
		"{if .Ip}":                 "unexpected EOF",
		"{nofunc .X}":              `function "nofunc" not defined`,
	}

	for layout, wantErr := range tests {
		_, err := parseFormat(layout, nil)
		if err == nil || !strings.Contains(err.Error(), wantErr) {
			t.Errorf("parseFormat(%q): got error %v, want it to contain %q", layout, err, wantErr)
		}
	}
}

func TestDetectUsedFields(t *testing.T) {
	all := usedFields{
		ip: true, clientIP: true, user: true, time: true, method: true, path: true, protocol: true,
		host: true, status: true, responseSize: true, referer: true, userAgent: true, duration: true,
	}

	tests := map[string]usedFields{
		Default(): {
			ip: true, user: true, time: true, method: true, path: true, status: true,
			responseSize: true, referer: true, userAgent: true,
		},
		"no fields":                     {},
		"literal .Host and .User text":  {},
		"{.Ip} {.Status}":               {ip: true, status: true},
		"{.UserAgent}":                  {userAgent: true},
		"{.ClientIp}":                   {clientIP: true},
		"{.Host}{.Protocol}{.Duration}": {host: true, protocol: true, duration: true},
		// Every branch counts.
		`{if eq .Status "500"}{.Path}{else}{.Method}{end}`: {status: true, path: true, method: true},
		"{with .Referer}{.}{end}":                          all, // conservative: a bare dot may be the Fields
		// Through variables and pipelines.
		"{$f := .Time}{$f}":            all,
		"{$.User}":                     {user: true},
		`{.UserAgent | printf "%.5s"}`: {userAgent: true},
		// The whole Fields passed to a function.
		`{printf "%v" .}`: all,
		// Defined templates.
		`{define "x"}{.Host}{end}{template "x" .}`: all,
	}

	for layout, want := range tests {
		tmpl, err := template.New("t").Delims(leftDelim, rightDelim).Parse(layout)
		if err != nil {
			t.Fatal(err)
		}

		got, err := detectUsedFields(tmpl)
		if err != nil {
			t.Errorf("%s: %v", layout, err)
			continue
		}

		if got != want {
			t.Errorf("%s:\ngot  %+v\nwant %+v", layout, got, want)
		}
	}
}

func TestEscape(t *testing.T) {
	tests := map[string]string{
		"":                 "-",
		"plain value":      "plain value",
		`say "hi"`:         `say \"hi\"`,
		`back\slash`:       `back\\slash`,
		"line\nbreak\r\t":  `line\nbreak\r\t`,
		"bell\x07del\x7f":  `bell\u0007del\u007f`,
		"nul\x00esc\x1b":   `nul\u0000esc\u001b`,
		"héllo wörld":      "héllo wörld",
		"inject\n1.2.3.4 ": `inject\n1.2.3.4 `,
	}

	for in, want := range tests {
		if got := escape(in); got != want {
			t.Errorf("escape(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestResponseSize(t *testing.T) {
	tests := map[string]string{
		"":      "-",
		"0":     "0",
		"2326":  "2326",
		"-1":    "-",
		"+1":    "-",
		"12abc": "-",
		"1, 2":  "-",
	}

	for in, want := range tests {
		if got := render(t, "{.ResponseSize}", &entry{contentLength: in}); got != want {
			t.Errorf("ResponseSize for Content-Length %q = %q, want %q", in, got, want)
		}
	}
}

func TestIpField(t *testing.T) {
	tests := map[string]string{
		"192.0.2.1:1234": "192.0.2.1",
		"[::1]:80":       "::1",
		"192.0.2.1":      "192.0.2.1",
		"":               "-",
	}

	for in, want := range tests {
		if got := render(t, "{.Ip}", &entry{remoteAddr: in}); got != want {
			t.Errorf("Ip for RemoteAddr %q = %q, want %q", in, got, want)
		}
	}
}

func TestBasicAuthUser(t *testing.T) {
	tests := map[string]string{
		"Basic ZnJhbms6cGFzc3dvcmQ=": "frank",
		"basic ZnJhbms6cGFzc3dvcmQ=": "frank",
		"Basic ZnJhbms=":             "frank", // no password separator
		"Basic !!!":                  "",
		"Bearer token":               "",
		"Basic ":                     "",
		"":                           "",
	}

	for in, want := range tests {
		if got := basicAuthUser(in); got != want {
			t.Errorf("basicAuthUser(%q) = %q, want %q", in, got, want)
		}
	}
}
