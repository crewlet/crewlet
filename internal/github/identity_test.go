package github_test

import (
	"testing"

	"github.com/crewlet/crewlet/internal/github"
)

// ONE APP, TWO SPELLINGS, and nothing relates them.
//
// A person writing a mention types the app's slug, so a body carries
// `@acme-sre-lead`; every payload reporting what that app did carries the
// account, which is the slug with `[bot]`. A build that registered one
// spelling answered half the deliveries and dropped the other half as a
// stranger's.
func TestAnAppsAccountIsItsSlugPlusBot(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct{ slug, want string }{
		{"acme-sre-lead", "acme-sre-lead[bot]"},
		// FOLDED, because a login is compared against a mention this
		// package has already lowercased.
		{"Acme-SRE-Lead", "acme-sre-lead[bot]"},
		{"  acme-sre-lead  ", "acme-sre-lead[bot]"},
		// ALREADY AN ACCOUNT, and appending a second suffix would make an
		// id no payload ever carries.
		{"acme-sre-lead[bot]", "acme-sre-lead[bot]"},
		// NO APP YET. A bare "[bot]" would map every seat without one
		// onto a single id, and the second registration would be refused
		// as a duplicate of the first.
		{"", ""},
		{"   ", ""},
	} {
		if got := github.BotLogin(tc.slug); got != tc.want {
			t.Errorf("BotLogin(%q) = %q, want %q", tc.slug, got, tc.want)
		}
	}
}

// A LOGIN IS CASE-INSENSITIVE AT GITHUB AND EXACT IN A MAP.
//
// Payloads carry the canonical casing and a mention carries what a person
// typed, so a seat whose account is `SreLead` was woken when the payload
// named it and not when somebody wrote `@srelead`: the same person, the same
// name, one of the two spellings silently unroutable.
func TestALoginIsComparedInOneSpelling(t *testing.T) {
	t.Parallel()
	for _, raw := range []string{"SreLead", "srelead", "  SRELEAD  "} {
		if got := github.NormalizeLogin(raw); got != "srelead" {
			t.Errorf("NormalizeLogin(%q) = %q", raw, got)
		}
	}
}
