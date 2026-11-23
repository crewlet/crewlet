package harnessprofile_test

import (
	"errors"
	"testing"

	"github.com/crewlet/crewlet/internal/providers/llm/harness/harnessprofile"
)

// A ROUTE IS ITS HOST, AND A VALUE THAT IS NO ROUTE IS REFUSED, never taken as
// it is: an address keeps its scheme, host and port alone, so a credential in
// its userinfo, path, query or fragment never reaches an entry's identity; a
// plain name (a region, a switch, a file's path) is itself; and a value that
// parses as neither (a fragment inside userinfo, a bare user:pass@host) is
// refused, so nothing unparsed is ever digested.
//
// Mutation: keep the path, the query or the userinfo, or answer an unparsable
// value as it is, and a row goes red.
func TestARouteIsItsHostAndAValueThatIsNoRouteIsRefused(t *testing.T) {
	t.Parallel()
	for value, want := range map[string]string{
		"https://gateway.example.com":                   "https://gateway.example.com",
		"https://gateway.example.com:8443/v1":           "https://gateway.example.com:8443",
		"https://user:planted-1@gateway.example.com/v1": "https://gateway.example.com",
		"https://gateway.example.com/v1?key=planted-2":  "https://gateway.example.com",
		"https://gateway.example.com/key-planted-3/v1":  "https://gateway.example.com",
		"https://gateway.example.com/v1#planted-4":      "https://gateway.example.com",
		"HTTPS://Gateway.Example.com/v1":                "https://gateway.example.com",
		"https://gateway.example.com/v1?key=ab%zz":      "https://gateway.example.com",
		"us-east-1":                           "us-east-1",
		"1":                                   "1",
		"/home/agent/.copilot/providers.json": "/home/agent/.copilot/providers.json",
		"https://user:p#ss@gateway.example.com/v1": "",
		"user:planted-5@gateway.example.com":       "",
		"Authorization: Bearer planted-6":          "",
		"gateway.example.com?key=planted-7":        "",
		"https://":                                 "",
		"":                                         "",
	} {
		got, err := harnessprofile.RouteValue(value)
		switch {
		case want == "" && (!errors.Is(err, harnessprofile.ErrNotRoute) || got != ""):
			t.Errorf("RouteValue(%q) = %q, %v; want it refused", value, got, err)
		case want != "" && (err != nil || got != want):
			t.Errorf("RouteValue(%q) = %q, %v; want %q", value, got, err, want)
		}
	}
}
