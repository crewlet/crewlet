package iamdomain_test

import (
	"testing"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/iamdomain"
)

// A SUSPENSION IS WHAT EVERY READ ANSWERS, the session read and the sign-in
// read alike.
//
// The status op moves a stage without authoring a whole document, and the row
// keeps the stage twice — the column every predicate reads, and the document a
// content record wrote. The op used to move the column alone, and both readers
// here decided from the DOCUMENT: a suspended person's sessions went on
// validating as `active` and their sign-in went on succeeding, while the
// directory listing beside them — which reads the column — said suspended.
func TestASuspensionIsWhatEveryReadAnswers(t *testing.T) {
	t.Parallel()
	rig := newWriteRig(t)
	reader := rig.reader(t)
	id := uuid.New().String()
	enrolSarah(t, rig, id)

	// THE CONTROL: before the suspension both reads say active, so the
	// assertions below are about the suspension and not about a reader
	// that answers something other than `active` for everybody.
	assertStage(t, reader, id, iam.StageActive)

	if _, err := rig.writer.SetStage(t.Context(), id, iam.StageSuspended,
		"op-suspend", "left the building"); err != nil {
		t.Fatalf("suspend: %v", err)
	}
	rig.drain()
	assertStage(t, reader, id, iam.StageSuspended)

	// AND THE ROW'S TWO COPIES AGREE, so a read-modify-write that
	// re-publishes the document cannot put the old stage back.
	if got := rig.column(`SELECT stage FROM iam_people WHERE id = ?`, id); len(got) != 1 ||
		got[0] != string(iam.StageSuspended) {
		t.Errorf("the stage column is %v, want suspended", got)
	}
	documents := rig.column(
		`SELECT CAST(document AS TEXT) FROM iam_people WHERE id = ?`, id)
	if len(documents) != 1 {
		t.Fatalf("read the document: %v", documents)
	}
	doc, err := iamdomain.DecodePerson([]byte(documents[0]))
	if err != nil {
		t.Fatalf("decode the stored document: %v", err)
	}
	if doc.Stage != iam.StageSuspended {
		t.Errorf("the stored document still says %q after a suspension: the "+
			"next edit of this person re-publishes it and lets them back in",
			doc.Stage)
	}
}

// EDITING A SUSPENDED PERSON LEAVES THEM SUSPENDED.
//
// A grant change and a credential change are each a read-modify-write of the
// whole document, formed inside the decide. Formed from a document that still
// said `active`, the edit re-published `active` and the applier wrote it back
// into the column — so an administrator narrowing a leaver's grants quietly
// reinstated them.
func TestEditingASuspendedPersonLeavesThemSuspended(t *testing.T) {
	t.Parallel()
	rig := newWriteRig(t)
	reader := rig.reader(t)
	id := uuid.New().String()
	enrolSarah(t, rig, id)
	if _, err := rig.writer.SetStage(t.Context(), id, iam.StageSuspended,
		"op-suspend", "left the building"); err != nil {
		t.Fatalf("suspend: %v", err)
	}
	rig.drain()

	if _, err := rig.writer.UpdatePerson(t.Context(), iamdomain.PersonUpdate{
		PersonID: id, OpID: "op-narrow", Reason: "narrow a leaver",
		Apply: func(p iamdomain.Person) (iamdomain.Person, error) {
			p.Grants = []iam.Grant{iam.GrantStateRead}
			return p, nil
		},
	}); err != nil {
		t.Fatalf("update: %v", err)
	}
	rig.drain()
	assertStage(t, reader, id, iam.StageSuspended)

	if _, err := rig.writer.SetCredentials(t.Context(), iamdomain.CredentialSet{
		PersonID: id, OpID: "op-factor", Reason: "clear a factor",
		Apply: func(held []iamdomain.Credential) []iamdomain.Credential { return held },
	}); err != nil {
		t.Fatalf("set credentials: %v", err)
	}
	rig.drain()
	assertStage(t, reader, id, iam.StageSuspended)
}

// enrolSarah enrols one active person under the login the stage assertions
// read.
func enrolSarah(t *testing.T, rig *writeRig, id string) {
	t.Helper()
	if err := rig.enrol(iamdomain.Enrolment{
		PersonID: id, Kind: iam.KindPerson, Stage: iam.StageActive,
		Name: "Sarah Chen", Email: "sarah.chen@example.com",
		Login: "sarah.chen", OpID: "op-" + id, Reason: "the joiner",
		Grants: []iam.Grant{iam.GrantStateRead, iam.GrantWorkWrite},
	}); err != nil {
		t.Fatalf("enrol: %v", err)
	}
	rig.drain()
}

// assertStage holds the two readers that decide who may act to one stage.
func assertStage(t *testing.T, reader *iamdomain.Reader, id string, want iam.Stage) {
	t.Helper()
	identity, err := reader.Resolve(t.Context(), "", id)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if identity.Person.Stage != want {
		t.Errorf("a session read answers %q, want %q", identity.Person.Stage, want)
	}
	seen, err := reader.PersonByLogin(t.Context(), "sarah.chen")
	if err != nil {
		t.Fatalf("PersonByLogin: %v", err)
	}
	if seen.Stage != want {
		t.Errorf("a sign-in read answers %q, want %q", seen.Stage, want)
	}
}
