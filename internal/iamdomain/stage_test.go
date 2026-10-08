package iamdomain_test

import (
	"errors"
	"testing"
	"time"

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
	id := uuid.Must(uuid.NewV7()).String()
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
	id := uuid.Must(uuid.NewV7()).String()
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
		Apply: func(held []iamdomain.Credential) ([]iamdomain.Credential, error) {
			return held, nil
		},
	}); err != nil {
		t.Fatalf("set credentials: %v", err)
	}
	rig.drain()
	assertStage(t, reader, id, iam.StageSuspended)
}

// AN EDIT OF A PERSON'S DOCUMENT MOVES NEITHER THEIR KIND NOR THEIR STAGE.
//
// The applier writes whatever kind and stage a document states, so the record
// is where both are held: a service account flipped to a person is a person
// holding no seat and a coloned login no person may have, which every other
// write here refuses to produce; and a stage moved by a document edit moved no
// epoch, so whatever a suspended person held came back the day they were
// reinstated. Both are refused with nothing published; an edit of what the
// person holds, the control, lands.
//
// Mutation: drop either clause from the edit's decide and its row lands.
func TestAnEditMovesNeitherAPersonsKindNorTheirStage(t *testing.T) {
	t.Parallel()
	rig := newWriteRig(t)
	id := uuid.Must(uuid.NewV7()).String()
	enrolSarah(t, rig, id)
	before, err := rig.end(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	for name, apply := range map[string]func(iamdomain.Person) iamdomain.Person{
		"a person made a service account": func(p iamdomain.Person) iamdomain.Person {
			p.Kind = iam.KindMachine
			return p
		},
		"a person suspended by an edit": func(p iamdomain.Person) iamdomain.Person {
			p.Stage = iam.StageSuspended
			return p
		},
	} {
		err := rig.during(func() error {
			_, err := rig.writer.UpdatePerson(t.Context(), iamdomain.PersonUpdate{
				PersonID: id, OpID: operationKey(), Reason: "an edit",
				Apply: func(p iamdomain.Person) (iamdomain.Person, error) {
					return apply(p), nil
				},
			})
			return err
		})
		if !errors.Is(err, iamdomain.ErrInvalid) {
			t.Errorf("%s answered %v, want %v", name, err, iamdomain.ErrInvalid)
		}
	}
	if after, _ := rig.end(t.Context()); after != before {
		t.Errorf("the refused edits published %d records", after-before)
	}
	if got := rig.column(`SELECT kind || ' ' || stage FROM iam_people WHERE id = ?`,
		id); len(got) != 1 || got[0] != "person active" {
		t.Errorf("the person reads %v after refused edits, want an active person",
			got)
	}
	// THE CONTROL: an edit of what they hold lands.
	if err := rig.during(func() error {
		_, err := rig.writer.UpdatePerson(t.Context(), iamdomain.PersonUpdate{
			PersonID: id, OpID: operationKey(), Reason: "narrowed",
			Apply: func(p iamdomain.Person) (iamdomain.Person, error) {
				p.Grants = []iam.Grant{iam.GrantStateRead}
				return p, nil
			},
		})
		return err
	}); err != nil {
		t.Errorf("an edit of the person's grants was refused: %v", err)
	}
}

// enrolSarah enrols one active person under the login the stage assertions
// read.
func enrolSarah(t *testing.T, rig *writeRig, id string) {
	t.Helper()
	if err := rig.enrol(iamdomain.Creation{
		PersonID: id, Kind: iam.KindPerson, Stage: iam.StageActive,
		Name: "Sarah Chen", Email: "sarah.chen@example.com",
		Login: "sarah.chen", OpID: "op-" + id, Reason: "the joiner",
		Seat: rig.vacantSeat("sarah-chen"), LinkExpiresAt: firstLinkExpiry,
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

// A SUSPENSION ENDS THE SESSIONS IT CUTS OFF, AND A REACTIVATION REVIVES NONE.
//
// A stage that may not act was only REFUSED at each request, so reactivating
// somebody brought back every session they held when they were suspended —
// whose browser had been answered `401` and had dropped its cookie, so the
// only copy that came back was one somebody else kept. The CONTROL is the
// session before the suspension, which is live. Mutation: state no epoch on
// the status record and the session is live again after the reactivation.
func TestASuspensionEndsTheSessionsItCutsOff(t *testing.T) {
	t.Parallel()
	rig := newWriteRig(t)
	person := enrolForSessions(t, rig)
	lineage := uuid.Must(uuid.NewV7()).String()
	if err := rig.draining(func() error {
		_, err := rig.writer.OpenSession(t.Context(), iamdomain.SessionStart{
			Lineage: lineage, Person: person,
			AbsoluteExpiresAt: time.Now().UTC().Add(time.Hour),
			ProvedAt:          time.Now().UTC(), OpID: "session:" + lineage,
		})
		return err
	}); err != nil {
		t.Fatalf("open: %v", err)
	}
	rig.drain()
	reader := rig.reader(t)
	live := func() bool {
		t.Helper()
		listed, err := reader.Sessions(t.Context(), person)
		if err != nil || len(listed) != 1 {
			t.Fatalf("the listing holds %+v (%v), want the one session", listed, err)
		}
		return listed[0].Live(time.Now())
	}
	if !live() {
		t.Fatal("the session is not live before the suspension")
	}
	for _, stage := range []iam.Stage{iam.StageSuspended, iam.StageActive} {
		if _, err := rig.writer.SetStage(t.Context(), person, stage,
			"op-"+string(stage), "a leave"); err != nil {
			t.Fatalf("set %s: %v", stage, err)
		}
		rig.drain()
	}
	if live() {
		t.Error("the session is live again after a suspension and a reactivation")
	}
}
