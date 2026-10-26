package configapi

import (
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/iam"
)

// A CHARTER WRITE THROUGH THE ASSISTANT IS DECIDED AS THE ADMINISTRATOR'S OWN.
//
// An administrator acting through an approved assistant grant (an exec grant)
// is admitted to a charter write on `config.principles.write`, and to the same
// change asked as an ordinary document write, and to a draft that also renames
// the company or adds a seat: the company document's verbs are the
// administrator's own through their assistant (ADR-0052's parity), each write
// the one they approved. A READ grant is refused every one of them, naming the
// scope, because a write is only ever carried as an approved request. The
// administrator at their keyboard is admitted to all of it, which is the
// control. Mutation: let a read grant take an approve row in authz and the
// read grant writes the charter; keep config.write from the assistant and the
// approved document write is refused.
func TestACharterWriteThroughTheAssistantIsTheAdministratorsOwn(t *testing.T) {
	t.Parallel()
	keyboard := iam.Principal{ID: uuid.New(), Login: "ana.admin", Kind: iam.KindPerson,
		Seat: "ana", Stage: iam.StageActive, Grants: []iam.Grant{iam.GrantAdmin},
		ReauthAt: time.Now().Add(time.Hour)}
	through := func(scope iam.AssistScope) iam.Principal {
		p := keyboard
		p.Via, p.AssistScope = iam.AssistName(uuid.Must(uuid.NewV7())), scope
		return p
	}
	approved, reading := through(iam.AssistScopeExec), through(iam.AssistScopeRead)

	prior := &config.Company{Name: "Acme", Mission: "Make every invoice add up.",
		Policies: []string{"Money is cents."}}
	charter := *prior
	charter.Mission, charter.Policies = "Make every bill add up.", []string{"Money is cents.",
		"Round down."}
	renamed := charter
	renamed.Name = "Acme Two"
	seated := charter
	seated.Roles = []config.Role{{Name: "CEO", Handle: "ceo"}}

	for _, c := range []struct {
		name      string
		p         iam.Principal
		next      *config.Company
		asCharter bool
		admitted  bool
	}{
		{"the charter, approved", approved, &charter, true, true},
		{"the charter as a document write, approved", approved, &charter, false, true},
		{"a charter draft that renames the company, approved", approved, &renamed, true, true},
		{"a charter draft that adds a seat, approved", approved, &seated, true, true},
		{"the charter, under a read grant", reading, &charter, true, false},
		{"a document write, under a read grant", reading, &seated, false, false},
		{"the charter, at the keyboard", keyboard, &charter, true, true},
		{"a renaming draft, at the keyboard", keyboard, &renamed, true, true},
	} {
		err := admit(t.Context(), c.p, prior, c.next, false, c.asCharter)
		var refused *AdmissionError
		switch {
		case c.admitted && err != nil:
			t.Errorf("%s was refused: %v", c.name, err)
		case !c.admitted && !errors.As(err, &refused):
			t.Errorf("%s answered %v, want an admission refusal", c.name, err)
		}
	}
}
