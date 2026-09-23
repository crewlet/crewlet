package org_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/org"
)

// TWO SEATS MAY NOT CLAIM ONE ACCOUNT.
//
// A walk of the chart answers the FIRST match while notification registration
// keys a map and keeps the LAST, so a duplicate does not collide loudly — it
// sends that person's wakes to one seat while an inbound message from them
// resolves to the other, with both entries looking perfectly ordinary. It is
// refused where every other chart-wide collision is refused: at validation.
func TestTwoSeatsMayNotClaimOneExternalAccount(t *testing.T) {
	o := &org.Organization{
		Name: "Nimbus",
		Roles: []*org.Role{
			{Name: "Ada Okonkwo", Kind: org.KindHuman,
				Contact: &org.HumanContact{GitHubLogin: "ada-o"}},
			{Name: "Rui Santos", Kind: org.KindHuman,
				// THE SAME LOGIN, differently cased: a code-host
				// login is folded to lower case, so this is the same
				// collision.
				Contact: &org.HumanContact{GitHubLogin: "ADA-O"}},
		},
	}
	o.Normalize()

	err := o.Validate()
	if !errors.Is(err, org.ErrDuplicateIdentity) {
		t.Fatalf("two seats claiming one GitHub login validated as %v", err)
	}
	if !strings.Contains(err.Error(), "Ada Okonkwo") ||
		!strings.Contains(err.Error(), "Rui Santos") {
		t.Errorf("the refusal %q names neither pair of seats, so nobody can "+
			"tell which two lines to edit", err)
	}

	// EVERY IDENTITY FIELD, not only the folded ones: a shared
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
