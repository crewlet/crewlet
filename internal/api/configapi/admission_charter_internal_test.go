package configapi

import (
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/iam"
)

// A CHARTER WRITE IS ADMITTED ON ITS OWN VERB, AND ONLY FOR THE CHARTER.
//
// An administrator acting through an approved assistant grant is admitted to
// a charter write that changes the mission, the vision or the policies, on
// `config.principles.write`; the same change asked as an ordinary document
// write is refused (`config.write` is never the assistant's), and a charter
// draft that also changes anything else (the company's name, a seat) is
// decided by the document's own rules and refused, so the narrow verb never
// stretches past the three settings. The administrator at their keyboard is
// admitted to all of it, which is the control. Mutation: drop the
// charter-only test in admit and the renaming draft is admitted.
func TestACharterWriteIsAdmittedOnItsVerbOnlyForTheCharter(t *testing.T) {
	t.Parallel()
	keyboard := iam.Principal{ID: uuid.New(), Login: "ana.admin", Kind: iam.KindPerson,
		Seat: "ana", Stage: iam.StageActive, Grants: []iam.Grant{iam.GrantAdmin},
		ReauthAt: time.Now().Add(time.Hour)}
	assisted := keyboard
	assisted.Via, assisted.AssistScope = iam.AssistName(uuid.Must(uuid.NewV7())),
		iam.AssistScopeExec

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
		{"the charter, through the assistant", assisted, &charter, true, true},
		{"the charter as a document write, through the assistant", assisted, &charter, false, false},
		{"a charter draft that renames the company", assisted, &renamed, true, false},
		{"a charter draft that adds a seat", assisted, &seated, true, false},
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
