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
// ways (case, a default port, dot segments, a trailing slash). A schemeless
// host:port keeps both; a plain name and a file's path are kept, case
// included; a schemeless name holding '/' and what parses as none of these
// are refused, never with the value.
//
// Mutation: keep userinfo, query, default port or trailing slash, drop the path,
// skip dot segments, fold a name's or file's case, refuse a host:port or admit
// one with a path, admit a schemeless '/' or an unparsable value: red.
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
		"GitHub.Example.com":                            "GitHub.Example.com",
		"Prod":                                          "Prod",
		"127.0.0.1:11434":                               "127.0.0.1:11434",
		"Ollama.Example.com:11434":                      "ollama.example.com:11434",
		"[::1]:11434":                                   "[::1]:11434",
		"127.0.0.1:65536":                               "",
		"127.0.0.1:0":                                   "",
		"::1":                                           "",
		"https://gateway.example.com:65536/v1":          "",
		"https://gateway.example.com:65535/v1":          "https://gateway.example.com:65535/v1",
		"ollama.example.com:11434/v1":                   "",
		"user:planted-9@ollama.example.com:11434":       "",
		"ollama.example.com:port":                       "",
		"prod":                                          "prod",
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

// A VALUE SHAPED LIKE A KEY IS REFUSED, WITHOUT IT: an address's path segment,
// a plain value or a file path's segment that begins as a registered runner's
// credential does (the declared list, every profile's own prefixes, and each
// family below written out) and runs a key's length would put the key in the
// entry's identity. A name that only begins so (xai-gw, a GCP project) is kept.
//
// Mutation: drop a prefix, skip the profile's own, check address paths alone or
// a host only once lower-cased, drop the length, or echo the value: red.
func TestAValueShapedLikeAKeyIsRefusedWithoutIt(t *testing.T) {
	t.Parallel()
	profile, ok := harnessprofile.Builtin("claude-code")
	if !ok {
		t.Fatal("no built-in claude-code profile")
	}
	body := strings.Repeat("planted0", 5)
	prefixes := []string{"sk-ant-api03-", "sk-", "sk-proj-", "xai-", "ghp_", "gho_", "ghu_", "ghs_", "ghr_",
		"github_pat_", "AIza"}
	prefixes = append(prefixes, harnessprofile.CredentialPrefixes...)
	for _, name := range harnessprofile.BuiltinNames() {
		p, _ := harnessprofile.Builtin(name)
		prefixes = append(prefixes, p.TokenPrefix, p.APIKeyPrefix)
	}
	refused := func(p harnessprofile.Profile, value string) {
		t.Helper()
		got, err := p.RouteValue(value)
		if !errors.Is(err, harnessprofile.ErrRouteHoldsCredential) || got != "" {
			t.Errorf("RouteValue(%q) = %q, %v; want it refused as holding a credential", value, got, err)
		} else if strings.Contains(err.Error(), "planted") {
			t.Errorf("the refusal of %q holds the value: %v", value, err)
		}
	}
	for _, prefix := range prefixes {
		if prefix == "" {
			continue
		}
		key := prefix + body
		for _, value := range []string{"https://gateway.example.com/v1/" + key + "/anthropic", key,
			"/run/secrets/" + key} {
			refused(profile, value)
		}
	}
	refused(harnessprofile.Profile{APIKeyPrefix: "mk-"}, "https://gateway.example.com/mk-"+body+"/v1")
	refused(harnessprofile.Profile{TokenPrefix: "mt-"}, "mt-"+body)
	refused(profile, "sk-"+strings.Repeat("a", harnessprofile.KeyBody))
	// A HOST SHAPED LIKE A KEY, as written: a host:port's and an address's.
	refused(profile, "sk-"+body+":11434")
	refused(profile, "AIza"+body+":443")
	refused(profile, "https://AIza"+body+":443/v1")
	refused(profile, "sk-"+body+".gw.example.com")
	refused(profile, "https://gw.example.com/v1/sk-"+body+".json")
	refused(profile, "https://AIza"+body+".proxy.example.com/v1")
	for _, name := range []string{"https://gateway.ai.cloudflare.com/v1/acct/xai-gw/openai",
		"https://proxy.example.com/xai-proxy/v1", "sk-my-gcp-project", "/etc/xai-proxy/providers.json",
		"sk-" + strings.Repeat("a", harnessprofile.KeyBody-1)} {
		if _, err := profile.RouteValue(name); err != nil {
			t.Errorf("RouteValue(%q), a name, was refused: %v", name, err)
		}
	}
}
