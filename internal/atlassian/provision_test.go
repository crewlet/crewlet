package atlassian_test

import (
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/atlassian"
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
	for _, seat := range []struct{ role, handle string }{
		{"SRE Lead", "sre-lead"},
		{"", "cto"},
		{"Head of Engineering (interim)", "head-eng"},
	} {
		name := atlassian.AccountName(seat.role, seat.handle)
		if got := atlassian.HandleFrom(name); got != seat.handle {
			t.Errorf("HandleFrom(%q) = %q, want %q", name, got, seat.handle)
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
		if got := atlassian.HandleFrom(name); got != "" {
			t.Errorf("HandleFrom(%q) = %q, want no seat", name, got)
		}
	}
}

// A TOKEN IS RECOGNISED BY THE LABEL ITS MINT STAMPED, and by nothing looser.
//
// The label is what a pass revokes a token nothing holds by, so what this
// guards is the other direction from the account marker's: a token a person
// made on the agent's account for their own use, or the token of a seat whose
// handle merely starts the same way, must never read as this engine's to take.
func TestATokenLabelNamesOnlyItsOwnSeat(t *testing.T) {
	t.Parallel()
	minted := atlassian.TokenLabel("sre-lead", time.Unix(1_767_225_600, 0))
	if !atlassian.MintedFor(minted, "sre-lead") {
		t.Fatalf("MintedFor(%q, sre-lead) = false for the label its own mint stamped", minted)
	}
	for _, c := range []struct{ label, handle string }{
		{minted, "sre"},
		{"sre-lead", "sre-lead"},
		{"sre-lead-", "sre-lead"},
		{"sre-lead-17672x", "sre-lead"},
		{"sre-lead-1767225600-copy", "sre-lead"},
		{"deploy-script", "sre-lead"},
		{"", "sre-lead"},
	} {
		if atlassian.MintedFor(c.label, c.handle) {
			t.Errorf("MintedFor(%q, %q) = true, want a token this engine did not mint "+
				"for that seat", c.label, c.handle)
		}
	}
}
