package org_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/org"
)

// THE BINDING IS ON THE SEAT, because Tier A is the root of trust and may not
// read Tier B — so the only way from a presented token to a person is this
// lookup, and everything personal in the dashboard hangs off it.
func TestASeatIsFoundByTheTokenBoundToIt(t *testing.T) {
	o := &org.Organization{
		Name: "Nimbus",
		Roles: []*org.Role{
			{Name: "Ada Okonkwo", Kind: org.KindHuman,
				Contact: &org.HumanContact{CrewletOperatorID: "founder"}},
			{Name: "CTO"},
		},
	}
	o.Normalize()

	seat := o.SeatByOperatorID("founder")
	if seat == nil {
		t.Fatal("the seat naming this token id was not found")
	}
	if seat.Name != "Ada Okonkwo" {
		t.Fatalf("resolved to %q", seat.Name)
	}

	// TWO FILES, TWO CHANCES TO DISAGREE ABOUT CASE, which is the reason
	// HumanContact's own doc gives for normalising the value.
	if got := o.SeatByOperatorID("  FOUNDER "); got == nil || got.Name != "Ada Okonkwo" {
		t.Fatalf("a differently-cased id did not resolve: %v", got)
	}

	// AN UNBOUND TOKEN IS AN ORDINARY STATE, not a misconfiguration — so it
	// answers nobody rather than guessing at the first seat.
	if got := o.SeatByOperatorID("someone-else"); got != nil {
		t.Fatalf("an unbound id resolved to %q", got.Name)
	}
	if got := o.SeatByOperatorID(""); got != nil {
		t.Fatalf("an empty id resolved to %q", got.Name)
	}
}

// AND TWO SEATS MAY NOT CLAIM ONE ACCOUNT.
//
// The lookup above answers the FIRST match while notification registration
// keys a map and keeps the LAST, so a duplicate does not collide loudly — it
// sends one person's dashboard and that same person's wakes to two different
// seats, with both entries looking perfectly ordinary. It is refused where
// every other chart-wide collision is refused: at validation.
func TestTwoSeatsMayNotClaimOneExternalAccount(t *testing.T) {
	o := &org.Organization{
		Name: "Nimbus",
		Roles: []*org.Role{
			{Name: "Ada Okonkwo", Kind: org.KindHuman,
				Contact: &org.HumanContact{CrewletOperatorID: "founder"}},
			{Name: "Rui Santos", Kind: org.KindHuman,
				// THE SAME ID, differently cased: the lookups
				// normalise, so this is the same collision.
				Contact: &org.HumanContact{CrewletOperatorID: "FOUNDER"}},
		},
	}
	o.Normalize()

	err := o.Validate()
	if !errors.Is(err, org.ErrDuplicateIdentity) {
		t.Fatalf("two seats bound to one token id validated as %v", err)
	}
	if !strings.Contains(err.Error(), "Ada Okonkwo") ||
		!strings.Contains(err.Error(), "Rui Santos") {
		t.Errorf("the refusal %q names neither pair of seats, so nobody can "+
			"tell which two lines to edit", err)
	}

	// EVERY IDENTITY FIELD, not only the one this change needed: a shared
	// slack_user_id routes one person's messages to the other exactly the
	// same way.
	two := func(a, b *org.HumanContact) error {
		o := &org.Organization{Name: "Nimbus", Roles: []*org.Role{
			{Name: "A", Kind: org.KindHuman, Contact: a},
			{Name: "B", Kind: org.KindHuman, Contact: b},
		}}
		o.Normalize()
		return o.Validate()
	}
	if err := two(
		&org.HumanContact{SlackUserID: "U0FOUNDER"},
		&org.HumanContact{SlackUserID: "U0FOUNDER"},
	); !errors.Is(err, org.ErrDuplicateIdentity) {
		t.Errorf("two seats sharing a Slack id validated as %v", err)
	}

	// AND TWO DIFFERENT ACCOUNTS ARE NOT A COLLISION, or the rule would be
	// "a company may have one contactable person".
	if err := two(
		&org.HumanContact{SlackUserID: "U0FOUNDER"},
		&org.HumanContact{SlackUserID: "U0COLLEAGUE"},
	); errors.Is(err, org.ErrDuplicateIdentity) {
		t.Errorf("two distinct Slack ids were refused as a duplicate: %v", err)
	}

	// NOR IS ONE VALUE IN TWO DIFFERENT FIELDS. A GitHub login and a
	// GitLab username are different namespaces and routinely the same word.
	if err := two(
		&org.HumanContact{GitHubLogin: "ada"},
		&org.HumanContact{GitLabUsername: "ada"},
	); errors.Is(err, org.ErrDuplicateIdentity) {
		t.Errorf("one name in two namespaces was refused as a duplicate: %v", err)
	}
}
