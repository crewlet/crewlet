package iamdomain_test

import (
	"testing"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/iamdomain"
)

// NARROWING SOMEBODY'S REACH TO THE CLOSED END STICKS.
//
// `colleague` is omitempty and its zero value is the closed end — a real
// setting. It was missing from the hand-kept list of this document's names, so
// a decode carried the stored value as unknown AND decoded it, and clearing it
// re-encoded the carried copy straight back: an administrator taking somebody's
// reach into the company's work away to nothing left it exactly where it was.
// Mutation: derive the known names from a marshalled zero value again and the
// person still reaches the company at `write`.
func TestNarrowingAPersonsReachToNothingSticks(t *testing.T) {
	t.Parallel()
	rig := newWriteRig(t)
	person := uuid.Must(uuid.NewV7()).String()
	if err := rig.enrol(iamdomain.Enrolment{
		PersonID: person, Kind: iam.KindPerson, Stage: iam.StageActive,
		Name: "Dana Okafor", Email: "dana@example.com", Login: "dana.sre",
		Colleague: iam.ColleagueWrite, OpID: "op-enrol", Reason: "a hire",
	}); err != nil {
		t.Fatalf("enrol: %v", err)
	}
	rig.drain()
	if err := rig.draining(func() error {
		_, err := rig.writer.UpdatePerson(rig.t.Context(), iamdomain.PersonUpdate{
			PersonID: person,
			Apply: func(p iamdomain.Person) (iamdomain.Person, error) {
				p.Colleague = ""
				return p, nil
			},
			OpID: "op-narrow", Reason: "moved to an audit role",
		})
		return err
	}); err != nil {
		t.Fatalf("update: %v", err)
	}
	rig.drain()
	got := rig.column(`SELECT document FROM iam_people WHERE id = '` + person + `'`)
	if len(got) != 1 {
		t.Fatalf("the person's row is %v", got)
	}
	held, err := iamdomain.DecodePerson([]byte(got[0]))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if held.Colleague != "" {
		t.Errorf("the person still reaches the company's work at %q after "+
			"being narrowed to nothing", held.Colleague)
	}
}
