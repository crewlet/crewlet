package capability_test

import (
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/capability"
	_ "github.com/crewlet/crewlet/internal/cloudflare" // registers the lane that takes grants
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/seataccess"
)

// A SEAT IS TOLD THE GRANTS ITS CREDENTIAL HOLDS, each with where it applies and
// when it lapses, and how to ask for one more instead of asking a person to
// edit the token by hand; a grant stored and not held yet is not said. The
// grant arriving and leaving is told once each, and a baseline from before
// grants were told takes none of them as a change. Mutation: build the lines
// from the stored grants and an unapplied grant is told as held.
func TestASeatIsToldTheGrantsItsCredentialHolds(t *testing.T) {
	t.Parallel()
	lapse := time.Date(2026, 11, 15, 18, 0, 0, 0, time.UTC)
	c := company()
	c.Roles[0].Integrations.Cloudflare.Grants = []config.SeatGrant{
		{ID: "gaaaaaaaaaa", Permission: "Origin Write", Scope: []string{"example.com"}, ExpiresAt: lapse},
		{ID: "gbbbbbbbbbb", Permission: "DNS Write"},
	}
	in := input(c)
	cf := in.Observed[seataccess.LaneCloudflare]
	cf.Grants = []seataccess.Grant{{Permission: "Origin Write", Scope: []string{"example.com"}}}
	in.Observed[seataccess.LaneCloudflare] = cf
	now := capability.Take(in)
	text := capability.YourAccess(now)
	for _, want := range []string{
		"Beside it: Origin Write on example.com until 2026-11-15 18:00 UTC.",
		"When Cloudflare refuses a call for a permission you lack (403 \"request is not authorized\"), ask for " +
			"that one permission with request_access",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("Your access lacks %q:\n%s", want, text)
		}
	}
	if strings.Contains(text, "DNS Write") {
		t.Errorf("a grant the token does not hold yet was told:\n%s", text)
	}

	before := capability.Take(input(company()))
	var kinds []capability.ChangeKind
	for _, ch := range capability.Diff(before, now) {
		kinds = append(kinds, ch.Kind)
	}
	if !slices.Contains(kinds, capability.ChangeGrantAdded) {
		t.Errorf("changes %v, want the grant told as added", kinds)
	}
	var back []capability.ChangeKind
	for _, ch := range capability.Diff(now, before) {
		back = append(back, ch.Kind)
	}
	if !slices.Contains(back, capability.ChangeGrantRemoved) {
		t.Errorf("changes %v, want the grant told as removed", back)
	}
	old := before
	old.V = 3
	for _, ch := range capability.Diff(old, now) {
		if ch.Kind == capability.ChangeGrantAdded {
			t.Errorf("a baseline from before grants were told took one as a change: %+v", ch)
		}
	}
}
