package config_test

import (
	"testing"

	"github.com/crewlet/crewlet/internal/config"
)

// THE REST BASE IS DERIVED, NEVER THE RAW URL — and on Enterprise Server the
// difference is destructive rather than cosmetic.
//
// [github.ReconcileSeatApps] reads a 404 from the installation endpoints as an
// app GitHub no longer has, and FORGETS the seat's app: the id, the slug and
// the pointer to a private key GitHub issues once and never reissues. Handed
// `https://ghe.example.com` as the API base, every call goes to a path the
// REST API is not served on, every call 404s, and the first pass clears every
// seat's working app.
//
// github.com hides it: an empty url makes the raw value and the derived one
// both resolve to api.github.com, so a caller that skipped the derivation was
// only ever wrong on Enterprise.
func TestGitHubBasesAreDerivedFromAResolvedURL(t *testing.T) {
	t.Parallel()
	env := config.EnvOnly()
	for name, tc := range map[string]struct {
		url              string
		wantAPI, wantWeb string
	}{
		"github.com is the empty default": {
			url:     "",
			wantAPI: "https://api.github.com",
			wantWeb: "https://github.com",
		},
		"enterprise gains the api path": {
			url:     "https://ghe.example.com",
			wantAPI: "https://ghe.example.com/api/v3",
			wantWeb: "https://ghe.example.com",
		},
		"an api base pasted from the docs is not doubled": {
			url:     "https://ghe.example.com/api/v3",
			wantAPI: "https://ghe.example.com/api/v3",
			wantWeb: "https://ghe.example.com",
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			api, web := (&config.GitHub{URL: tc.url}).Bases(env.LookupOK)
			if api != tc.wantAPI {
				t.Errorf("api base = %q, want %q", api, tc.wantAPI)
			}
			if web != tc.wantWeb {
				t.Errorf("web base = %q, want %q", web, tc.wantWeb)
			}
		})
	}
}

// AND THE URL IS RESOLVED BEFORE IT IS DERIVED FROM.
//
// A `${VAR}` host read literally has no `.example.com` in it, so both bases
// fall back to github.com — an Enterprise deployment silently reconciled
// against the public host, which is where a seat's apps do not exist at all.
func TestGitHubBasesResolveTheHostFirst(t *testing.T) {
	t.Parallel()
	held := config.NewResolver(config.MapSource{"GITHUB_HOST": "https://ghe.example.com"})
	api, web := (&config.GitHub{URL: "${GITHUB_HOST}"}).Bases(held.LookupOK)
	if api != "https://ghe.example.com/api/v3" {
		t.Errorf("api base = %q, and a reference was not read before it was derived from", api)
	}
	if web != "https://ghe.example.com" {
		t.Errorf("web base = %q, want the resolved host", web)
	}
}

// A nil block has no bases, and asking for them must not panic: the teardown
// path reads them before it has decided the block is there.
func TestGitHubBasesOfNothingAreEmpty(t *testing.T) {
	t.Parallel()
	var absent *config.GitHub
	if api, web := absent.Bases(config.EnvOnly().LookupOK); api != "" || web != "" {
		t.Errorf("Bases() of a nil block = %q/%q, want empty", api, web)
	}
}
