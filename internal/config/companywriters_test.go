package config_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/iam"
)

// WHO MAY CHANGE THE COMPANY DOCUMENT is one reading of one list, judged on
// the CREDENTIAL a write came through and never on its author: an empty list
// leaves every write to the authority table, a listed Tier A token's own
// bearer may, the engine itself may, and every other credential — a person's
// session, a machine token acting as its owner, a session exchanged from the
// very token the list names — may not.
//
// Mutation: compare by.Name instead of by.OperatorID and the exchanged session
// is admitted; drop the system arm and the engine's own write is refused.
func TestOnlyAListedTokenMayWriteAManagedCompany(t *testing.T) {
	t.Parallel()
	operator := iam.Actor{Name: iam.TokenLogin("operator"), Kind: iam.ActorOperator,
		OperatorID: iam.TokenLogin("operator")}
	open := config.APIAuth{}
	if open.CompanyManaged() || !open.MayWriteCompany(operator) ||
		!open.MayWriteCompany(iam.Actor{Name: "ceo", Kind: iam.ActorHuman, OperatorID: "session:s1"}) {
		t.Fatal("with no company_writers the document is unmanaged, and the list judges nobody")
	}
	managed := config.APIAuth{CompanyWriters: []string{"operator"}}
	if !managed.CompanyManaged() {
		t.Fatal("a listed writer did not make the document managed")
	}
	for name, tc := range map[string]struct {
		by   iam.Actor
		want bool
	}{
		"the listed token's own bearer": {operator, true},
		"the engine itself":             {iam.Actor{Name: "crewlet", Kind: iam.ActorSystem}, true},
		"another Tier A token": {iam.Actor{Name: iam.TokenLogin("founder"),
			Kind: iam.ActorOperator, OperatorID: iam.TokenLogin("founder")}, false},
		"a person's session, a lead's included": {iam.Actor{Name: "ceo",
			Kind: iam.ActorHuman, OperatorID: "session:s1"}, false},
		"a machine token acting as its owner": {iam.Actor{Name: "ceo",
			Kind: iam.ActorHuman, OperatorID: "pat:p1"}, false},
		"a session exchanged from the listed token": {iam.Actor{Name: iam.TokenLogin("operator"),
			Kind: iam.ActorOperator, OperatorID: "session:s2"}, false},
		"the bare id, not the login a token acts under": {iam.Actor{Name: "operator",
			Kind: iam.ActorOperator, OperatorID: "operator"}, false},
		"nobody": {iam.Actor{}, false},
	} {
		if got := managed.MayWriteCompany(tc.by); got != tc.want {
			t.Errorf("%s: MayWriteCompany(%+v) = %v, want %v", name, tc.by, got, tc.want)
		}
	}
}

// A WRITER IS A TOKEN ID, held to the grammar a token id is: an entry no token
// could ever be is not a stricter list but a typo that locks out the system it
// was meant to name, so it is refused at its own index.
func TestACompanyWriterOutsideTheTokenGrammarIsRefused(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		writers []string
		at      string
		kind    error
	}{
		"an empty entry":  {[]string{""}, "api.auth.company_writers[0]", config.ErrMissing},
		"a mixed-case id": {[]string{"Operator"}, "api.auth.company_writers[0]", config.ErrUnknownValue},
		"a space inside":  {[]string{"op erator"}, "api.auth.company_writers[0]", config.ErrUnknownValue},
		"a repeat":        {[]string{"operator", "operator"}, "api.auth.company_writers[1]", config.ErrConflict},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			b := config.RunnableBootstrap()
			b.API.Auth.CompanyWriters = tc.writers
			err := b.Validate()
			if !errors.Is(err, tc.kind) {
				t.Fatalf("Validate() = %v, want %v", err, tc.kind)
			}
			if !strings.Contains(err.Error(), tc.at) {
				t.Errorf("the refusal is not at %s: %v", tc.at, err)
			}
		})
	}
}

// AND THE SETTINGS THAT ARE VALID AND ALMOST CERTAINLY A TYPO are said before
// the managing system meets a 403 — each at the field to change.
func TestACompanyWriterThatNamesNoTokenIsWarnedAbout(t *testing.T) {
	t.Parallel()
	value := strings.Repeat("k", 32)
	base := func() *config.Bootstrap {
		b := config.RunnableBootstrap()
		b.Retention.BackupOwner = "platform-oncall"
		b.API.Auth.Tokens = []config.APIToken{
			{ID: "operator", Token: value, Grants: []iam.Grant{iam.GrantAdmin}},
			{ID: "founder", Token: value, Grants: []iam.Grant{iam.GrantAdmin}},
		}
		return &b
	}
	for name, tc := range map[string]struct {
		writers []string
		want    map[string]string // path -> fragment
	}{
		"a configured writer is quiet": {
			writers: []string{"operator"}, want: map[string]string{},
		},
		"a typo names no token, and leaves the document frozen": {
			writers: []string{"operater"},
			want: map[string]string{
				"api.auth.company_writers[0]": `"operater" names no token`,
				"api.auth.company_writers":    "freezes the document",
			},
		},
		"one stray id beside a real one is named alone": {
			writers: []string{"operator", "gitops"},
			want: map[string]string{
				"api.auth.company_writers[1]": `"gitops" names no token`,
			},
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			b := base()
			b.API.Auth.CompanyWriters = tc.writers
			if err := b.Validate(); err != nil {
				t.Fatalf("the fixture does not validate: %v", err)
			}
			got := map[string]string{}
			for _, w := range b.Warnings() {
				got[w.Path] = w.Message
			}
			if len(got) != len(tc.want) {
				t.Errorf("warnings = %v, want paths %v", got, tc.want)
			}
			for path, fragment := range tc.want {
				if !strings.Contains(got[path], fragment) {
					t.Errorf("warning at %s = %q, want it to say %q", path, got[path], fragment)
				}
			}
		})
	}
}
