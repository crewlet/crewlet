package atlassian_test

import (
	"testing"

	"github.com/crewlet/crewlet/internal/atlassian"
	"github.com/crewlet/crewlet/internal/provision"
)

// AN ACCOUNT IS RECOGNISED BY ITS DISPLAY NAME, and it has to be.
//
// The description would have been the natural place for a marker, and it is
// where this started: Atlassian accepts one on the create call, answers 200
// and stores nothing. Every pass then failed to recognise the accounts the
// last one made and created another — four accounts for one agent, before a
// listing showed it. The name is the field that survives.
func TestAnAccountNameCarriesItsSeatAndReadsBack(t *testing.T) {
	t.Parallel()
	for _, seat := range []struct {
		role   string
		origin provision.Origin
	}{
		{"SRE Lead", "sre-lead"},
		{"", "cto"},
		{"Head of Engineering (interim)", "head-eng"},
	} {
		name := atlassian.AccountName(seat.role, seat.origin)
		if got := atlassian.OriginFrom(name); got != seat.origin {
			t.Errorf("OriginFrom(%q) = %q, want %q", name, got, seat.origin)
		}
	}
}

// AND SOMEBODY ELSE'S ACCOUNT IS NOT THIS ENGINE'S.
//
// This is what keeps a disconnect from deleting a service account a person
// made for their own script: an organization's accounts are not all Crewlet's,
// and a match that was merely fuzzy would take theirs away too.
func TestAnUnmarkedAccountNamesNoSeat(t *testing.T) {
	t.Parallel()
	for _, name := range []string{
		"", "Deploy bot", "SRE Lead (Crewlet)", "crewlet:sre-lead", "SRE Lead (crewlet:sre-lead",
	} {
		if got := atlassian.OriginFrom(name); got != "" {
			t.Errorf("OriginFrom(%q) = %q, want no seat", name, got)
		}
	}
}
