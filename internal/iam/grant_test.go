package iam

import (
	"slices"
	"strings"
	"testing"
)

// TestEveryGrantIsClassifiedInBothDirections keeps the access table and the
// closed set in step. A grant with no class answers the zero Access, which is
// invalid — so a new capability added without a reader deciding what it opens
// would be refused by every gate at once, and this is what catches that in a
// diff rather than in production.
func TestEveryGrantIsClassifiedInBothDirections(t *testing.T) {
	for _, g := range AllGrants {
		access := g.Access()
		if !access.Valid() {
			t.Errorf("%q has no access class — decide whether it reads or writes", g)
		}
	}
	for g := range grantAccess {
		if !slices.Contains(AllGrants, g) {
			t.Errorf("the access table classifies %q, which is not in AllGrants", g)
		}
	}
	if got := Grant("swarm:conscript").Access(); got.Valid() {
		t.Errorf("an unknown grant classified as %q; this build cannot know what it opens", got)
	}
}

// TestTheReadGrantsAreTheOnesAnOpenPostureCouldEverOpen states which four they
// are, because that is a security decision and not an implementation detail:
// allow_anonymous_read opens reads and only reads, and which grants are reads
// is the list it is reading from.
func TestTheReadGrantsAreTheOnesAnOpenPostureCouldEverOpen(t *testing.T) {
	var reads, writes []Grant
	for _, g := range AllGrants {
		switch g.Access() {
		case AccessRead:
			reads = append(reads, g)
		case AccessWrite:
			writes = append(writes, g)
		}
	}
	wantReads := []Grant{GrantStateRead, GrantAuditRead, GrantConfigRead, GrantSecretRead}
	if !slices.Equal(reads, wantReads) {
		t.Errorf("the read grants are %v, want %v", reads, wantReads)
	}
	if len(writes) != len(AllGrants)-len(wantReads) {
		t.Errorf("%d grants are writes, want %d", len(writes), len(AllGrants)-len(wantReads))
	}
}

// TestAGrantNamesItsDomainAndItsVerb pins the wire spelling. These strings
// land in rows and in config, so a typo'd one is a capability nobody can grant
// and nobody can see is missing.
func TestAGrantNamesItsDomainAndItsVerb(t *testing.T) {
	for _, g := range AllGrants {
		domain, verb, found := strings.Cut(string(g), ":")
		if !found || domain == "" || verb == "" {
			t.Errorf("%q is not spelled <domain>:<verb>", g)
			continue
		}
		if strings.ToLower(string(g)) != string(g) || strings.Contains(verb, ":") {
			t.Errorf("%q is not one lowercase domain and one verb", g)
		}
	}
}
