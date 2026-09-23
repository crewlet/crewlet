package iamdomain_test

import (
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/iam/session"
	"github.com/crewlet/crewlet/internal/iamdomain"
	"github.com/crewlet/crewlet/internal/runtoken"
)

// A SESSION OPENS AT THE COUNTERS THAT WOULD END IT, and a sign-in after either
// has moved is a session that WORKS.
//
// The epoch and the generation used to be a caller's field no caller filled in,
// so every bearer carried zero for both. Nothing looked wrong until somebody
// used the gesture each counter exists for: after a person's first "sign out
// everywhere" every new session of theirs validated as already ended, and after
// the first `invalidate-all` nobody in the company could sign in again. The
// cases here go through the real writer, the real applier and the real reader,
// and judge the bearer the way every ingress node does — because each half
// passes on its own and the bug lived in the seam between them.
func TestASessionOpenedAfterARevocationIsLive(t *testing.T) {
	t.Parallel()
	rig := newWriteRig(t)
	person := enrolForSessions(t, rig)
	if err := rig.draining(func() error {
		_, err := rig.writer.Revoke(t.Context(), person, "op-revoke",
			"signed out everywhere")
		return err
	}); err != nil {
		t.Fatalf("revoke: %v", err)
	}
	rig.drain()

	v := signInAndValidate(t, rig, person)
	if v.Row != session.RowValid {
		t.Fatalf("a sign-in after the person signed out everywhere validates "+
			"as %q (%s), want %q — every session they open from now on is "+
			"born ended", v.Row, v.Detail, session.RowValid)
	}
	if v.Bearer.Epoch != 1 {
		t.Errorf("the bearer carries epoch %d, want the person's current 1",
			v.Bearer.Epoch)
	}
}

func TestASessionOpenedAfterAnInvalidationIsLive(t *testing.T) {
	t.Parallel()
	rig := newWriteRig(t)
	person := enrolForSessions(t, rig)
	if err := rig.draining(func() error {
		_, err := rig.writer.InvalidateAll(t.Context(), "op-invalidate",
			"the restore runbook's last step")
		return err
	}); err != nil {
		t.Fatalf("invalidate: %v", err)
	}
	rig.drain()

	v := signInAndValidate(t, rig, person)
	if v.Row != session.RowValid {
		t.Fatalf("a sign-in after a fleet-wide invalidation validates as %q "+
			"(%s), want %q — nobody in the company could sign in again",
			v.Row, v.Detail, session.RowValid)
	}
	if v.Bearer.Generation != 1 {
		t.Errorf("the bearer carries generation %d, want the fleet's current 1",
			v.Bearer.Generation)
	}
}

// AND THE COUNTERS STILL END WHAT PREDATES THEM, which is the control: a
// session opened BEFORE the revocation is over, so the two cases above are
// about when a session was opened rather than about counters that end nothing.
func TestASessionOpenedBeforeARevocationIsEnded(t *testing.T) {
	t.Parallel()
	rig := newWriteRig(t)
	person := enrolForSessions(t, rig)
	signer, cookie := openSession(t, rig, person)
	if err := rig.draining(func() error {
		_, err := rig.writer.Revoke(t.Context(), person, "op-revoke",
			"signed out everywhere")
		return err
	}); err != nil {
		t.Fatalf("revoke: %v", err)
	}
	rig.drain()
	v := signer.Validate(t.Context(), rig.reader(t), cookie)
	if v.Row != session.RowEnded {
		t.Errorf("a session opened before the revocation validates as %q "+
			"(%s), want %q", v.Row, v.Detail, session.RowEnded)
	}
}

// A SUBJECT WITH NO PERSON ROW STILL HAS AN EPOCH.
//
// A session exchanged from a Tier A token names the token's login, which
// nothing enrols, and signing out everywhere from it bumps the epoch under that
// login. The epoch used to be read only beside a person row, so that
// revocation was written and ended nothing: every session the token had opened
// went on validating.
func TestTheEpochIsReadForASubjectNobodyEnrolled(t *testing.T) {
	t.Parallel()
	rig := newWriteRig(t)
	const subject = "token:ops"
	if err := rig.draining(func() error {
		_, err := rig.writer.Revoke(t.Context(), subject, "op-revoke",
			"signed out everywhere")
		return err
	}); err != nil {
		t.Fatalf("revoke: %v", err)
	}
	rig.drain()
	seen, err := rig.reader(t).Resolve(t.Context(), "", subject)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if seen.Person.Found {
		t.Error("a subject nobody enrolled was found")
	}
	if seen.Person.Epoch != 1 {
		t.Errorf("the epoch under %s reads %d, want the 1 the revocation "+
			"moved it to", subject, seen.Person.Epoch)
	}
}

// enrolForSessions enrols one active person and applies it.
func enrolForSessions(t *testing.T, rig *writeRig) string {
	t.Helper()
	person := uuid.Must(uuid.NewV7()).String()
	if err := rig.enrol(iamdomain.Enrolment{
		PersonID: person, Kind: iam.KindPerson, Stage: iam.StageActive,
		Name: "Sarah Chen", Email: "sarah.chen@example.com",
		Login: "sarah.chen", OpID: "op-enrol-" + person, Reason: "a hire",
	}); err != nil {
		t.Fatalf("enrol: %v", err)
	}
	rig.drain()
	return person
}

// signInAndValidate opens a session the way the sign-in surface does and
// judges its bearer the way every ingress node does.
func signInAndValidate(t *testing.T, rig *writeRig, person string) session.Validation {
	t.Helper()
	signer, cookie := openSession(t, rig, person)
	return signer.Validate(t.Context(), rig.reader(t), cookie)
}

// openSession opens one session, applies its start record and mints the bearer
// from exactly what the writer answered.
func openSession(t *testing.T, rig *writeRig, person string) (*session.Signer, string) {
	t.Helper()
	signer, err := session.New(session.Options{
		Material: runtoken.Material{ActiveID: "k1", Keys: []runtoken.KeyMaterial{
			{ID: "k1", Material: "the-session-key-material"},
		}},
	})
	if err != nil {
		t.Fatalf("build a signer: %v", err)
	}
	lineage := uuid.Must(uuid.NewV7())
	expires := time.Now().UTC().Add(time.Hour)
	var opened iamdomain.SessionOpened
	if err := rig.draining(func() error {
		var err error
		opened, err = rig.writer.OpenSession(t.Context(), iamdomain.SessionStart{
			Lineage: lineage.String(), Person: person,
			AbsoluteExpiresAt: expires, OpID: "session:" + lineage.String(),
		})
		return err
	}); err != nil {
		t.Fatalf("open a session: %v", err)
	}
	rig.drain()
	cookie, err := signer.Mint(session.Mint{
		Lineage: lineage, Person: person,
		Epoch: opened.Epoch, Generation: opened.Generation,
		StartPosition:     uint64(opened.Position.Packed()),
		AbsoluteExpiresAt: expires,
	})
	if err != nil {
		t.Fatalf("mint: %v", err)
	}
	return signer, cookie
}
