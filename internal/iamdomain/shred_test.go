package iamdomain_test

import (
	"errors"
	"testing"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/iamdomain"
)

// THE KEY DELETE IS RETRIED UNTIL IT LANDS.
//
// A removal commits its rows and then destroys the person's key, post-commit
// and best effort — so a coordination blip at that instant leaves a person
// removed from every row and still readable from every copy made before: the
// backups, the donated snapshots, the records still on the log. The key duty
// is the only thing that ever finishes that, and this case walks it through:
//
//  1. THE CONTROL, which is the reason the duty exists. With the store
//     refusing deletes, the removal applies and the name sealed into an
//     earlier copy still opens. If it did not, the case below would prove
//     nothing about retrying.
//  2. A pass while the blip lasts finds the person pending, destroys nothing
//     and SAYS so, rather than reporting a clean pass.
//  3. The first pass after the blip destroys the key, and the name that
//     opened a moment ago answers ErrShredded.
//  4. And it leaves everybody else alone: a colleague who was never removed
//     keeps a key that opens, because a duty that destroyed every key it
//     listed would be an outage with a retention policy's name.
func TestTheDekDeleteIsRetriedUntilItLands(t *testing.T) {
	t.Parallel()
	rig := newWriteRig(t)
	reader := rig.reader(t)
	sealer, err := iamdomain.NewSealer(rig.keys)
	if err != nil {
		t.Fatalf("NewSealer: %v", err)
	}

	leaver := uuid.Must(uuid.NewV7()).String()
	stayer := uuid.Must(uuid.NewV7()).String()
	for _, p := range []struct{ id, name, email, login string }{
		{leaver, "Sarah Chen", "sarah.chen@example.com", "sarah.chen"},
		{stayer, "Omar Haddad", "omar.haddad@example.com", "omar.haddad"},
	} {
		if err := rig.enrol(iamdomain.Enrolment{
			PersonID: p.id, Kind: iam.KindPerson, Stage: iam.StageActive,
			Name: p.name, Email: p.email, Login: p.login,
			OpID: "enrol-" + p.login, Reason: "a joiner",
		}); err != nil {
			t.Fatalf("enrol %s: %v", p.login, err)
		}
	}
	// THE EARLIER COPY: what a backup taken before the removal holds.
	sealed := rig.column(`SELECT name_sealed FROM iam_people WHERE id = ?`, leaver)
	kept := rig.column(`SELECT name_sealed FROM iam_people WHERE id = ?`, stayer)
	if len(sealed) != 1 || len(kept) != 1 {
		t.Fatalf("the two people were not both enrolled: %v %v", sealed, kept)
	}

	blip := errors.New("coordination store: no responders")
	rig.keys.blip(blip)
	if err := rig.during(func() error {
		_, err := rig.writer.Remove(t.Context(), leaver, "remove-sarah", "left")
		return err
	}); err != nil {
		t.Fatalf("Remove: %v", err)
	}

	// 1. THE CONTROL.
	if plain, err := sealer.Open(t.Context(), leaver, iamdomain.FieldName,
		sealed[0]); err != nil || plain != "Sarah Chen" {
		t.Fatalf("with the delete refused the name should still open from an "+
			"earlier copy, and it answered (%q, %v) — so this case cannot tell a "+
			"retry from a shred that landed first time", plain, err)
	}

	// 2. A PASS DURING THE BLIP SAYS IT DID NOT FINISH.
	report, err := iamdomain.ShredRemoved(t.Context(), reader, rig.keys, sealer)
	if err == nil {
		t.Fatal("a pass that could destroy nothing reported success")
	}
	if len(report.Pending) != 1 || report.Pending[0] != leaver ||
		len(report.Destroyed) != 0 {
		t.Fatalf("pass during the blip: %+v, want the leaver pending and "+
			"nothing destroyed", report)
	}

	// 3. THE FIRST PASS AFTER IT LANDS.
	rig.keys.blip(nil)
	report, err = iamdomain.ShredRemoved(t.Context(), reader, rig.keys, sealer)
	if err != nil {
		t.Fatalf("ShredRemoved: %v", err)
	}
	if len(report.Destroyed) != 1 || report.Destroyed[0] != leaver {
		t.Fatalf("pass after the blip: %+v, want the leaver's key destroyed", report)
	}
	if _, err := sealer.Open(t.Context(), leaver, iamdomain.FieldName,
		sealed[0]); !errors.Is(err, iamdomain.ErrShredded) {
		t.Fatalf("the leaver's name still opens from an earlier copy after "+
			"the duty ran (err %v)", err)
	}
	if again, err := iamdomain.ShredRemoved(t.Context(), reader, rig.keys,
		sealer); err != nil || len(again.Pending) != 0 {
		t.Errorf("a pass after the key landed still found work: %+v, %v", again, err)
	}

	// 4. NOBODY ELSE'S KEY.
	if plain, err := sealer.Open(t.Context(), stayer, iamdomain.FieldName,
		kept[0]); err != nil || plain != "Omar Haddad" {
		t.Fatalf("a colleague who was never removed lost their key: (%q, %v)",
			plain, err)
	}
}
