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
// registers an id two seats declare for NEITHER, so a duplicate does not
// collide loudly — it sends that person's wakes to one seat while an inbound
// message from them resolves to nobody, with both entries looking perfectly
// ordinary. It is refused where every other chart-wide collision is refused: at
// validation.
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

	// CASE COUNTS EXACTLY WHERE THE PLATFORM COUNTS IT. A Mattermost
	// username, a GitHub login and a GitLab username name one account
	// however they are capitalised, so two spellings are one collision;
	// a Slack member id and an Atlassian account id are opaque, and two
	// that differ in case are two accounts as far as anything here knows.
	for _, tc := range []struct {
		field string
		a, b  *org.HumanContact
		same  bool
	}{
		{"mattermost_user_id", &org.HumanContact{MattermostUserID: "Jane.Doe"},
			&org.HumanContact{MattermostUserID: "jane.doe"}, true},
		{"github_login", &org.HumanContact{GitHubLogin: "Ada"},
			&org.HumanContact{GitHubLogin: "ada"}, true},
		{"gitlab_username", &org.HumanContact{GitLabUsername: "Ada"},
			&org.HumanContact{GitLabUsername: "ada"}, true},
		{"slack_user_id", &org.HumanContact{SlackUserID: "U0ABC"},
			&org.HumanContact{SlackUserID: "u0abc"}, false},
		{"atlassian_account_id", &org.HumanContact{AtlassianAccountID: "5b10ac8d82e05b22cc7d4ef5"},
			&org.HumanContact{AtlassianAccountID: "5B10AC8D82E05B22CC7D4EF5"}, false},
	} {
		err := two(tc.a, tc.b)
		if got := errors.Is(err, org.ErrDuplicateIdentity); got != tc.same {
			t.Errorf("%s spelled in two cases: duplicate = %v, want %v (%v)",
				tc.field, got, tc.same, err)
		}
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
