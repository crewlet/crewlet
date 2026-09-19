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

	seat := o.SeatByOperatorID("founder", nil)
	if seat == nil {
		t.Fatal("the seat naming this token id was not found")
	}
	if seat.Name != "Ada Okonkwo" {
		t.Fatalf("resolved to %q", seat.Name)
	}

	// TWO FILES, TWO CHANCES TO DISAGREE ABOUT CASE, which is the reason
	// HumanContact's own doc gives for normalising the value.
	if got := o.SeatByOperatorID("  FOUNDER ", nil); got == nil || got.Name != "Ada Okonkwo" {
		t.Fatalf("a differently-cased id did not resolve: %v", got)
	}

	// AN UNBOUND TOKEN IS AN ORDINARY STATE, not a misconfiguration — so it
	// answers nobody rather than guessing at the first seat.
	if got := o.SeatByOperatorID("someone-else", nil); got != nil {
		t.Fatalf("an unbound id resolved to %q", got.Name)
	}
	if got := o.SeatByOperatorID("", nil); got != nil {
		t.Fatalf("an empty id resolved to %q", got.Name)
	}
}

// A `${VAR}` IS A POINTER, AND THE LOOKUP IS WHAT MAKES IT AN ANSWER.
//
// Tier B stores the reference verbatim — that is what a pointer IS — so this
// compared the founder's token against the literal text `${FOUNDER_ID}` and
// matched nothing. The symptom is the worst shape a bug can take: the dashboard
// comes up bound to no seat, so the founder's own inbox, queue and pins are
// empty, and nothing anywhere refuses or explains it.
func TestAnOperatorIDHeldAsAReferenceResolvesToItsSeat(t *testing.T) {
	o := &org.Organization{
		Name: "Nimbus",
		Roles: []*org.Role{
			{Name: "Ada Okonkwo", Kind: org.KindHuman,
				Contact: &org.HumanContact{CrewletOperatorID: "${FOUNDER_ID}"}},
			{Name: "CTO"},
		},
	}
	o.Normalize()

	env := func(name string) (string, bool) {
		if name == "FOUNDER_ID" {
			return "founder", true
		}
		return "", false
	}

	seat := o.SeatByOperatorID("founder", env)
	if seat == nil || seat.Name != "Ada Okonkwo" {
		t.Fatalf("a token id held as a reference did not resolve: %v", seat)
	}

	// The reference is resolved BEFORE the case fold, so the two-files rule
	// covers what the variable holds as well as what the file says.
	if got := o.SeatByOperatorID("  FOUNDER ", env); got == nil {
		t.Fatal("a differently-cased id did not resolve through the reference")
	}

	// AND THE LITERAL TEXT IS NOT AN IDENTITY. Nobody presents `${FOUNDER_ID}`
	// as a token id, but if this ever matched it would bind a dashboard to a
	// seat on a string out of a config file.
	if got := o.SeatByOperatorID("${FOUNDER_ID}", env); got != nil {
		t.Fatalf("the unresolved reference itself matched %q", got.Name)
	}
}

// AN UNRESOLVABLE SEAT DOES NOT TAKE THE LOOKUP DOWN WITH IT.
//
// Resolution is per seat, so one seat whose variable is unset must not stop the
// walk or answer for anybody. The neighbour is the assertion that can fail:
// a `return nil` where the code means `continue` reads identically at the call
// site — the caller gets nil either way — and would unbind every operator in
// the company behind one missing variable.
//
// The two "matches nothing" lines below are a floor rather than the point;
// `TestAnOperatorIDHeldAsAReferenceResolvesToItsSeat` carries the case that
// bites, which is that the raw `${VAR}` text is never itself an identity.
func TestAnUnresolvedOperatorReferenceMatchesNothing(t *testing.T) {
	o := &org.Organization{
		Name: "Nimbus",
		Roles: []*org.Role{
			{Name: "Ada Okonkwo", Kind: org.KindHuman,
				Contact: &org.HumanContact{CrewletOperatorID: "${FOUNDER_ID}"}},
			{Name: "Rui Santos", Kind: org.KindHuman,
				Contact: &org.HumanContact{CrewletOperatorID: "ops"}},
		},
	}
	o.Normalize()

	unset := func(string) (string, bool) { return "", false }
	if got := o.SeatByOperatorID("founder", unset); got != nil {
		t.Fatalf("an unset reference resolved to %q", got.Name)
	}
	// A variable set to nothing is the same fact arriving a different way.
	blank := func(string) (string, bool) { return "   ", true }
	if got := o.SeatByOperatorID("founder", blank); got != nil {
		t.Fatalf("a reference resolving to whitespace matched %q", got.Name)
	}
	// The seat beside it still answers: one unresolvable seat does not take
	// the lookup down with it.
	if got := o.SeatByOperatorID("ops", unset); got == nil || got.Name != "Rui Santos" {
		t.Fatalf("a literal id beside an unresolvable one did not resolve: %v", got)
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
