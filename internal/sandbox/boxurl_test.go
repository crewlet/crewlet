package sandbox_test

import (
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/sandbox"
)

// A BOX URL IS AN ABSOLUTE HTTP URL WITH A HOST, or it is unset.
//
// Each caller appends its own path to the value and hands the result to a
// client inside a box that the engine never hears from again: an exporter that
// cannot use its endpoint drops the run's telemetry silently, and a coding CLI
// handed an unusable bridge fails every tool call. So a value that is set and
// is not something a box can dial is refused, naming the variable and the part
// that is wrong — and never repeating the value, whose user information is a
// credential an operator pasted by mistake.
//
// The scheme is judged no further than http or https: plain http is accepted
// to any host, because whether a URL's path leaves the box's own host is a fact
// about the network this grammar cannot see.
//
// WHAT IT RETURNS IS A URL, which a box's client parses again: it is spelled
// by net/url's own serializer, so a non-ASCII host keeps the escape a URI
// needs, and every accepted value's answer parses back to itself. And what a
// box's client cannot read is refused though net/url reads it: an IPv6 zone,
// a host holding a character the WHATWG URL Standard forbids in a domain (an
// unfilled `<host>` placeholder, an escaped '%', a ']'), a host whose escapes
// decode to no UTF-8, a host that ends in a number and is no IPv4 address the
// standard reads (a typo'd 10.0.0.300), and a port past 65535. Every host row
// and every port row but zero has the verdict Node 22's and Bun's `new URL`
// give it — the parsers of the two coding agents this build launches, which
// agreed on every row, measured — and port zero, which both parse, is refused
// because nothing listens on it. The IPv4 shorthand the standard does read
// (127.1, 0x7f.1) boots, since the box dials the address its parser makes of
// it.
func TestABoxURLIsAnAbsoluteHTTPURLWithAHost(t *testing.T) {
	t.Parallel()
	const variable = "CREWLET_TEST_BOX_URL"
	for _, tc := range []struct {
		name, raw, want string
	}{
		{"unset", "", ""},
		{"only space is unset", "  \t", ""},
		{"https with its trailing slash trimmed", "https://engine.example.com/", "https://engine.example.com"},
		{"http to the box's host, with a port and a path",
			"http://172.17.0.1:8080/base/", "http://172.17.0.1:8080/base"},
		{"plain http to a host that is not loopback", "http://engine.example.com", "http://engine.example.com"},
		{"surrounding space", "  https://engine.example.com:8443  ", "https://engine.example.com:8443"},
		{"a scheme in capitals", "HTTPS://engine.example.com", "https://engine.example.com"},
		{"an IPv6 host", "http://[fd00::1]:8000", "http://[fd00::1]:8000"},
		{"a non-ASCII host, kept escaped", "https://ex%C3%A9.example/", "https://ex%C3%A9.example"},
		{"a non-ASCII host written raw, escaped as a URI spells it",
			"https://ex\u00e9.example", "https://ex%C3%A9.example"},
		{"an escaped slash in the path, kept escaped", "http://engine.example.com/a%2Fb/",
			"http://engine.example.com/a%2Fb"},
		{"the lowest port", "http://engine.example.com:1", "http://engine.example.com:1"},
		{"the highest port", "http://engine.example.com:65535", "http://engine.example.com:65535"},
		{"an empty port, omitted as RFC 3986 asks", "http://engine.example.com:/base",
			"http://engine.example.com/base"},
		// The WHATWG IPv4 parser's own spellings: a box reads each as an
		// address (127.0.0.1, 127.0.0.1, 10.0.0.1, 255.255.255.255) and
		// dials it, so none is a value the boot may refuse.
		{"an IPv4 address in shorthand", "http://127.1:8000", "http://127.1:8000"},
		{"an IPv4 address with a hex part", "http://0x7f.1", "http://0x7f.1"},
		{"an IPv4 address with a trailing dot", "http://10.0.0.1.:8000", "http://10.0.0.1.:8000"},
		{"an IPv4 address as one number", "http://4294967295", "http://4294967295"},
		// A last label that only starts like a number is a name.
		{"a name whose last label starts with a digit", "http://engine.1a", "http://engine.1a"},
		{"a name whose last label starts like hex", "http://engine.0xg", "http://engine.0xg"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := sandbox.ParseBoxURL(variable, tc.raw)
			if err != nil {
				t.Fatalf("ParseBoxURL(%q) refused: %v", tc.raw, err)
			}
			if got != tc.want {
				t.Errorf("ParseBoxURL(%q) = %q, want %q", tc.raw, got, tc.want)
			}
			// IDEMPOTENT: the answer is a box URL itself, so a client that
			// parses it — and this grammar, handed it again — reads the
			// same URL back.
			again, err := sandbox.ParseBoxURL(variable, got)
			if err != nil || again != got {
				t.Errorf("ParseBoxURL(%q) = %q, but that answer parses as %q (%v)",
					tc.raw, got, again, err)
			}
		})
	}

	for _, tc := range []struct {
		name, raw string
		// part is what the refusal names as wrong.
		part string
	}{
		{"no scheme, a host and a port", "engine.example.com:80", "scheme"},
		{"no scheme, an address and a port", "127.0.0.1:8080", "scheme"},
		{"no scheme, a bare host", "engine.example.com", "scheme"},
		{"a scheme with no slashes", "http:engine.example.com", "scheme"},
		{"a scheme a box cannot dial", "ftp://engine.example.com", "http"},
		{"no host", "http://", "host"},
		{"no host before a path", "http:///x", "host"},
		{"a port and no host", "http://:8080", "host"},
		{"an IPv6 zone", "http://[fe80::1%25eth0]:8000", "zone"},
		{"an escaped % in a host name", "http://engine%25a.example.com", "host"},
		{"an unfilled placeholder", "http://<engine-host>:8000", "host"},
		{"a closing bracket in a host name", "http://engine]a.example.com", "host"},
		{"a host whose escape decodes to no UTF-8", "https://%FF.example.com", "UTF-8"},
		{"an IPv4 address with a part past 255", "http://10.0.0.300:8000", "IPv4"},
		{"an IPv4 address with a first part past 255", "http://256.0.0.1:8000", "IPv4"},
		{"an IPv4 address with a fifth part", "http://10.0.0.1.2", "IPv4"},
		{"an IPv4 address past 32 bits", "http://4294967296", "IPv4"},
		{"an IPv4 address with an empty part", "http://10..0.1", "IPv4"},
		{"an octal IPv4 part with a digit 8", "http://08.0.0.1", "IPv4"},
		{"a name whose last label is a number", "http://engine.example.123", "IPv4"},
		{"a name whose last label is a number, before a trailing dot",
			"http://engine.example.123.", "IPv4"},
		{"a name whose last label is a hex number", "http://engine.0X1F", "IPv4"},
		{"port zero", "http://engine.example.com:0", "port"},
		{"a port past 65535", "http://engine.example.com:99999", "port"},
		{"a port past 65535 on an IPv6 host", "http://[fd00::1]:65536", "port"},
		{"user information", "https://ops:hunter2@engine.example.com", "user information"},
		{"an empty user", "https://@engine.example.com", "user information"},
		{"a query", "https://engine.example.com/?region=eu", "query"},
		{"an empty query", "https://engine.example.com/?", "query"},
		{"a fragment", "https://engine.example.com/#otlp", "fragment"},
		{"an empty fragment", "https://engine.example.com#", "fragment"},
		{"a host that does not parse", "https://engine example.com", "parse"},
		{"a malformed escape in the user information", "https://ops:%zz@engine.example.com", "parse"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := sandbox.ParseBoxURL(variable, tc.raw)
			if err == nil {
				t.Fatalf("ParseBoxURL(%q) = %q, want a refusal", tc.raw, got)
			}
			if got != "" {
				t.Errorf("a refused value still returned %q", got)
			}
			msg := err.Error()
			if !strings.Contains(msg, variable) {
				t.Errorf("the refusal does not name %s: %v", variable, err)
			}
			if !strings.Contains(msg, tc.part) {
				t.Errorf("the refusal does not name the %s as what is wrong: %v", tc.part, err)
			}
			// NEVER THE VALUE, nor the credential inside it: the refusal
			// goes to a log and to a terminal a deploy script captures.
			for _, secret := range []string{strings.TrimSpace(tc.raw), "hunter2", "%zz", "region=eu",
				"99999", "65536", "eth0", "engine-host", "%FF", "300", "0X1F"} {
				if secret != "" && strings.Contains(msg, secret) {
					t.Errorf("the refusal echoes %q: %v", secret, err)
				}
			}
		})
	}
}
