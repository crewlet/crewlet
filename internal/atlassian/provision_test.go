package atlassian_test

import (
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/atlassian"
	"github.com/crewlet/crewlet/internal/org"
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

// A SEAT WHOSE TOKEN SLOT HOLDS A CREDENTIAL IS LEFT TO THE COMPANY.
//
// It is planned for nothing — no account, no token — and recorded as managed
// by hand rather than as a caveat, because it is a decision the document
// states rather than something missing from it. The slot's value never
// travels: it is a credential, and the plan is reported where an operator
// pastes it into a ticket.
func TestASeatHoldingItsOwnCredentialIsLeftToTheCompany(t *testing.T) {
	t.Parallel()
	const pasted = "a-token-somebody-pasted-in"
	plan, err := atlassian.PlanFor(&org.Organization{Name: "Nimbus", Roles: []*org.Role{
		{Name: "Zed Ops", MCPEnv: map[string]map[string]string{
			"atlassian": {"ATLASSIAN_API_TOKEN": "Bearer " + pasted},
		}},
		{Name: "SRE Lead", MCPEnv: map[string]map[string]string{
			"jira": {"JIRA_API_TOKEN": "${SRE_TOKEN}", "JIRA_USERNAME": "${SRE_EMAIL}"},
		}},
		{Name: "Data Analyst", MCPEnv: map[string]map[string]string{
			"jira": {"JIRA_API_TOKEN": pasted},
		}},
	}})
	if err != nil {
		t.Fatalf("PlanFor: %v", err)
	}
	// ORDERED BY HANDLE, as the seats are, so a report reads the same pass
	// after pass whatever order the chart declares them in.
	if want := []string{"data-analyst", "zed-ops"}; !slices.Equal(plan.ByHand, want) {
		t.Errorf("by hand = %v, want %v", plan.ByHand, want)
	}
	if len(plan.Seats) != 1 || plan.Seats[0].Handle != "sre-lead" {
		t.Errorf("seats = %+v, want only sre-lead provisioned", plan.Seats)
	}
	if len(plan.Notes) != 0 {
		t.Errorf("notes = %v, want none: a seat managed by hand is not a caveat", plan.Notes)
	}
	for _, said := range slices.Concat(plan.ByHand, plan.Notes) {
		if strings.Contains(said, pasted) {
			t.Errorf("the plan repeats a seat's credential: %q", said)
		}
	}
}
