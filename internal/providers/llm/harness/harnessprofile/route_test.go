package harnessprofile_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/providers/llm/harness/harnessprofile"
)

// A ROUTE IS ITS ADDRESS CLEANED OF ALL THAT CANNOT MOVE A REQUEST, AND A VALUE
// THAT IS NO ROUTE IS REFUSED. An address keeps its scheme, host, port and path
// (how a gateway such as Cloudflare's routes) and loses its userinfo, query and
// fragment, where a credential may ride, and what only spells one place two
// ways (case, a default port, dot segments, a trailing slash). A plain name is
// lower-cased, a file's path kept; a schemeless name holding '/' and what
// parses as neither are refused, never with the value.
//
// Mutation: keep the userinfo, query, default port or trailing slash, drop the
// path, skip the dot segments, lower-case a file's path, admit a schemeless
// '/', or take an unparsable value as it is, and a row goes red.
func TestARouteIsItsCleanedAddressAndAValueThatIsNoRouteIsRefused(t *testing.T) {
	t.Parallel()
	profile, ok := harnessprofile.Builtin("claude-code")
	if !ok {
		t.Fatal("no built-in claude-code profile")
	}
	const cloudflare = "https://gateway.ai.cloudflare.com/v1/0123456789abcdef0123456789abcdef/gw/anthropic"
	for value, want := range map[string]string{
		"https://gateway.example.com":                   "https://gateway.example.com",
		"https://gateway.example.com/":                  "https://gateway.example.com",
		"https://gateway.example.com:443/v1/":           "https://gateway.example.com/v1",
		"http://gateway.example.com:80/v1":              "http://gateway.example.com/v1",
		"https://gateway.example.com:8443/v1":           "https://gateway.example.com:8443/v1",
		"https://user:planted-1@gateway.example.com/v1": "https://gateway.example.com/v1",
		"https://gateway.example.com/v1?key=planted-2":  "https://gateway.example.com/v1",
		"https://gateway.example.com/v1#planted-4":      "https://gateway.example.com/v1",
		"HTTPS://Gateway.Example.com/v1":                "https://gateway.example.com/v1",
		"https://gateway.example.com/a/./b/../v1":       "https://gateway.example.com/a/v1",
		"https://[::1]:8443/v1":                         "https://[::1]:8443/v1",
		cloudflare:                                      cloudflare,
		"us-east-1":                                     "us-east-1",
		"GitHub.Example.com":                            "github.example.com",
		"1":                                             "1",
		"/home/agent/.copilot/Providers.json":           "/home/agent/.copilot/Providers.json",
		"~/Providers.json":                              "~/Providers.json",
		"https://user:p#ss@gateway.example.com/v1":      "",
		"user:planted-5@gateway.example.com":            "",
		"Authorization: Bearer planted-6":               "",
		"gateway.example.com?key=planted-7":             "",
		"gateway.example.com/sk-planted-8/v1":           "",
		"https://":                                      "",
		"":                                              "",
	} {
		got, err := profile.RouteValue(value)
		switch {
		case want == "" && (!errors.Is(err, harnessprofile.ErrNotRoute) || got != ""):
			t.Errorf("RouteValue(%q) = %q, %v; want it refused", value, got, err)
		case want != "" && (err != nil || got != want):
			t.Errorf("RouteValue(%q) = %q, %v; want %q", value, got, err, want)
		case err != nil && strings.Contains(err.Error(), "planted"):
			t.Errorf("RouteValue(%q) refused with its value: %v", value, err)
		}
	}
}

// AN ADDRESS WHOSE PATH HOLDS A KEY IS REFUSED, without the key: a segment
// beginning as a registered runner's credential does (or as the profile's own
// declares) would put the key in the entry's identity. A gateway's account id,
// 32 hex characters, is no key.
//
// Mutation: drop a prefix, skip the profile's own, or echo the value, and a row
// goes red.
func TestAnAddressWhosePathHoldsAKeyIsRefusedWithoutIt(t *testing.T) {
	t.Parallel()
	profile, ok := harnessprofile.Builtin("claude-code")
	if !ok {
		t.Fatal("no built-in claude-code profile")
	}
	for _, key := range []string{"sk-ant-api03-planted", "sk-planted", "xai-planted", "ghp_planted",
		"github_pat_planted", "AIzaPlanted"} {
		value := "https://gateway.example.com/v1/" + key + "/anthropic"
		got, err := profile.RouteValue(value)
		if !errors.Is(err, harnessprofile.ErrRouteHoldsCredential) || got != "" {
			t.Errorf("RouteValue(%q) = %q, %v; want it refused as holding a credential", value, got, err)
		} else if strings.Contains(err.Error(), "planted") {
			t.Errorf("the refusal of %q holds the value: %v", value, err)
		}
	}
	own := harnessprofile.Profile{APIKeyPrefix: "mk-"}
	if _, err := own.RouteValue("https://gateway.example.com/mk-planted/v1"); !errors.Is(err,
		harnessprofile.ErrRouteHoldsCredential) {
		t.Errorf("a segment shaped like the profile's own key was taken: %v", err)
	}
}
