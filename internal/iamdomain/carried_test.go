package iamdomain_test

import (
	"testing"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/iamdomain"
)

// STRIPPING SOMEBODY'S LAST GRANT STICKS.
//
// `grants` is omitempty, so an empty list does not marshal at all. A field
// missing from the list of this document's known names is decoded AND carried
// as unknown, and clearing it re-encodes the carried copy straight back: an
// administrator taking somebody's last grant away left it exactly where it
// was. Mutation: derive the known names from a marshalled zero value again and
// the person still carries `state:read`.
func TestStrippingAPersonsLastGrantSticks(t *testing.T) {
	t.Parallel()
	rig := newWriteRig(t)
	person := uuid.Must(uuid.NewV7()).String()
	if err := rig.enrol(iamdomain.Enrolment{
		PersonID: person, Kind: iam.KindPerson, Stage: iam.StageActive,
		Name: "Dana Okafor", Email: "dana@example.com", Login: "dana.sre",
		Grants: []iam.Grant{iam.GrantStateRead},
		OpID:   "op-enrol", Reason: "a hire",
	}); err != nil {
		t.Fatalf("enrol: %v", err)
	}
	rig.drain()
	if err := rig.draining(func() error {
		_, err := rig.writer.UpdatePerson(rig.t.Context(), iamdomain.PersonUpdate{
			PersonID: person,
			Apply: func(p iamdomain.Person) (iamdomain.Person, error) {
				p.Grants = nil
				return p, nil
			},
			OpID: "op-strip", Reason: "left the team",
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
	if len(held.Grants) != 0 {
		t.Errorf("the person still carries %v after their last grant was "+
			"stripped", held.Grants)
	}
}
