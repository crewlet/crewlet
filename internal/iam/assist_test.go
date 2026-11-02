package iam

import (
	"strings"
	"testing"

	"github.com/google/uuid"
)

// AN ASSISTANT GRANT'S NAME IS NOBODY'S LOGIN, AND NAMES NO OTHER CREDENTIAL.
//
// `assist:<grant>` is what the operator column records for a request the
// assistant made for a person (ADR-0050). A service account enrolled under the
// class would read as the assistant in every trail that records one, and a
// name that also read as a session or a token would send a reader resolving it
// to the wrong table. Mutation: drop the class from credentialClasses and
// `assist:…` enrols as a machine handle; drop it from ValidCredentialName and
// an assisted person fails to validate.
func TestAnAssistGrantsNameIsNobodysLogin(t *testing.T) {
	t.Parallel()
	grant := uuid.Must(uuid.NewV7())
	name := AssistName(grant)
	if !ValidAssistName(name) || !ValidCredentialName(name) {
		t.Fatalf("%q does not name the grant it was built from", name)
	}
	for _, s := range []string{name, "assist:ci", "assist:" + strings.ToUpper(grant.String())} {
		if ValidLoginFor(KindMachine, s) || ValidLoginFor(KindPerson, s) {
			t.Errorf("%q may be enrolled as somebody's login", s)
		}
	}
	for _, s := range []string{"assist:", "assist:ci", grant.String(),
		SessionName(grant.String()), MachineTokenName(grant.String()),
		"assist:" + strings.ToUpper(grant.String())} {
		if ValidAssistName(s) {
			t.Errorf("%q is taken for an assistant grant's name", s)
		}
	}
	if ValidSessionName(name) || ValidMachineTokenName(name) {
		t.Errorf("%q reads as another credential class", name)
	}
}

// ONLY AN ASSISTANT GRANT'S NAME SAYS THE ASSISTANT MADE THE REQUEST.
//
// Assisted is the one read internal/authz refuses the assistant on, so a
// session, a machine token, a bare class and a name in another case must not
// read as one. Mutation: match the prefix alone and `assist:` with no id is
// assisted.
func TestOnlyAnAssistNameIsAssisted(t *testing.T) {
	t.Parallel()
	grant := uuid.Must(uuid.NewV7())
	for _, c := range []struct {
		via  string
		want uuid.UUID
		ok   bool
	}{
		{AssistName(grant), grant, true},
		{SessionName(grant.String()), uuid.Nil, false},
		{MachineTokenName(grant.String()), uuid.Nil, false},
		{AssistPrefix, uuid.Nil, false},
		{"assist:" + strings.ToUpper(grant.String()), uuid.Nil, false},
		{"", uuid.Nil, false},
	} {
		got, ok := Principal{Via: c.via}.Assisted()
		if got != c.want || ok != c.ok {
			t.Errorf("via %q answered (%s, %v), want (%s, %v)", c.via, got, ok, c.want, c.ok)
		}
	}
}

// AN ASSISTED PRINCIPAL IS A PERSON, AND ITS SCOPE TRAVELS ONLY WITH ITS GRANT.
//
// The grant is derived only from a person's own session, so the assistant
// acting as a machine or a seat is a principal nobody composed, and a scope on
// a principal acting through no grant would narrow nothing anybody can name.
// The author is still the person's seat; the grant is the operator. Mutation:
// remove any one Validate clause and its row admits.
func TestAnAssistedPrincipalIsAPersonCarryingItsScope(t *testing.T) {
	t.Parallel()
	name := AssistName(uuid.Must(uuid.NewV7()))
	person := Principal{ID: uuid.New(), Kind: KindPerson, Login: "jane.doe",
		Seat: "founder", Stage: StageActive, Via: name, AssistScope: AssistScopeRead}
	if err := person.Validate(); err != nil {
		t.Fatalf("a person acting through the assistant: %v", err)
	}
	if got := ActorFor(person); got.Name != "founder" || got.Kind != ActorHuman ||
		got.OperatorID != name {
		t.Errorf("an assisted person is recorded as %+v, want the seat as the author "+
			"and the grant as the operator", got)
	}

	refused := map[string]Principal{
		"a machine through a grant": {ID: uuid.New(), Kind: KindMachine,
			Login: "ci:release", Stage: StageActive, Via: name, AssistScope: AssistScopeRead},
		"a grant with no scope": {ID: uuid.New(), Kind: KindPerson, Login: "jane.doe",
			Seat: "founder", Stage: StageActive, Via: name},
		"a grant with an unknown scope": {ID: uuid.New(), Kind: KindPerson,
			Login: "jane.doe", Seat: "founder", Stage: StageActive, Via: name,
			AssistScope: "write"},
		"a scope with no grant": {ID: uuid.New(), Kind: KindPerson, Login: "jane.doe",
			Seat: "founder", Stage: StageActive,
			Via: SessionName(uuid.Must(uuid.NewV7()).String()), AssistScope: AssistScopeExec},
	}
	for what, p := range refused {
		if err := p.Validate(); err == nil {
			t.Errorf("%s validates", what)
		}
	}
	for _, s := range AssistScopes {
		if !s.Valid() {
			t.Errorf("declared scope %q is not valid", s)
		}
	}
	if AssistScope("").Valid() {
		t.Error("the zero scope is valid, so a grant nobody scoped reaches as far as it likes")
	}
}
