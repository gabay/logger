package logger

import (
	"strings"
	"testing"
)

func TestParseTrustedForwarders(t *testing.T) {
	trusted, err := parseTrustedForwarders([]string{"10.0.0.0/8", " 192.0.2.1 ", "", "2001:db8::/32", "::ffff:198.51.100.7", "172.16.5.4/12"})
	if err != nil {
		t.Fatal(err)
	}

	got := make([]string, 0, len(trusted))
	for _, prefix := range trusted {
		got = append(got, prefix.String())
	}

	// Single addresses become host prefixes, IPv4-mapped addresses are
	// unmapped and host bits are masked.
	want := "10.0.0.0/8,192.0.2.1/32,2001:db8::/32,198.51.100.7/32,172.16.0.0/12"
	if strings.Join(got, ",") != want {
		t.Errorf("got %s, want %s", strings.Join(got, ","), want)
	}

	for _, invalid := range []string{"10.0.0.0/33", "300.1.1.1", "host.example", "10.0.0.0/x"} {
		if _, err := parseTrustedForwarders([]string{invalid}); err == nil {
			t.Errorf("%q: expected an error", invalid)
		}
	}
}

func TestClientIP(t *testing.T) {
	trusted, err := parseTrustedForwarders([]string{"10.0.0.0/8", "192.0.2.1", "2001:db8::/32"})
	if err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name                       string
		trusted                    trustedForwarders
		peer, forwardedFor, realIP string
		want                       string
	}{
		{name: "no trusted forwarders", peer: "10.0.0.1", forwardedFor: "198.51.100.1", want: "10.0.0.1"},
		{name: "untrusted peer", trusted: trusted, peer: "203.0.113.1", forwardedFor: "198.51.100.1", want: "203.0.113.1"},
		{name: "unparseable peer", trusted: trusted, peer: "@unix", forwardedFor: "198.51.100.1", want: "@unix"},
		{name: "single hop", trusted: trusted, peer: "10.0.0.1", forwardedFor: "198.51.100.1", want: "198.51.100.1"},
		{
			name: "spoofed entries left of the client are ignored", trusted: trusted, peer: "10.0.0.1",
			forwardedFor: "1.2.3.4, 198.51.100.1, 10.0.0.9", want: "198.51.100.1",
		},
		{name: "all trusted: leftmost", trusted: trusted, peer: "10.0.0.1", forwardedFor: "10.0.0.7, 192.0.2.1", want: "10.0.0.7"},
		{name: "empty entries", trusted: trusted, peer: "10.0.0.1", forwardedFor: " , 198.51.100.1 ,, ", want: "198.51.100.1"},
		{name: "only empty entries", trusted: trusted, peer: "10.0.0.1", forwardedFor: " , ", want: "10.0.0.1"},
		{name: "hop with port", trusted: trusted, peer: "10.0.0.1", forwardedFor: "198.51.100.1:5555", want: "198.51.100.1"},
		{name: "IPv6 hop with port", trusted: trusted, peer: "10.0.0.1", forwardedFor: "[2001:db9::1]:80", want: "2001:db9::1"},
		{name: "IPv6 trusted peer", trusted: trusted, peer: "2001:db8::5", forwardedFor: "198.51.100.1", want: "198.51.100.1"},
		{name: "IPv4-mapped peer", trusted: trusted, peer: "::ffff:10.0.0.1", forwardedFor: "198.51.100.1", want: "198.51.100.1"},
		{name: "not an address", trusted: trusted, peer: "10.0.0.1", forwardedFor: "unknown, 10.0.0.2", want: "unknown"},
		{name: "X-Real-Ip fallback", trusted: trusted, peer: "10.0.0.1", realIP: " 198.51.100.8 ", want: "198.51.100.8"},
		{name: "X-Forwarded-For wins", trusted: trusted, peer: "10.0.0.1", forwardedFor: "198.51.100.1", realIP: "198.51.100.8", want: "198.51.100.1"},
		{name: "no headers", trusted: trusted, peer: "10.0.0.1", want: "10.0.0.1"},
	}

	for _, test := range tests {
		if got := test.trusted.clientIP(test.peer, test.forwardedFor, test.realIP); got != test.want {
			t.Errorf("%s: got %q, want %q", test.name, got, test.want)
		}
	}
}
