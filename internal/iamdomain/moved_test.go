package iamdomain_test

import (
	"slices"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/iamdomain"
)

// A COMMITTED BATCH SAYS WHOSE CREDENTIALS IT MOVED, and only theirs.
//
// An open dashboard socket is authenticated once, at its handshake, and then
// held for as long as the tab is; what ends it is the record that ended its
// credential, which every node hears from its own applier. Two failures are
// invisible from the writing side and are what this is for. A record that
// ended somebody's credential and named nobody leaves their open tab receiving
// the company's state until it closes on its own — a revocation that stops at
// the one surface a browser keeps open. And a record that named everybody
// would end every socket on the node for every unbind in the company.
//
// So each gesture is held to exactly what it names: the session it ended, or
// the person whose row it moved WITH THE LOGIN THEY HOLD — a release and a
// rename included, which read whom they took a claim off before taking it —
// because a Tier A token is bound through the directory row under its own
// login and never learns that row's id. Only the fleet-wide generation names
// everyone. A sign-in names nothing, which is the control: it is the bulk of
// this log's traffic and ends nobody's credential.
//
// Mutation: drop the holder read from a release, or the login from a person's
// move, and the unbind or the machine's suspension names nobody.
func TestABatchSaysWhoseCredentialsItMoved(t *testing.T) {
	t.Parallel()
	rig := newWriteRig(t)
	person := bindNew(t, rig, "sarah.chen", "sarah-chen")
	other := bindNew(t, rig, "noor.aziz", "ops-lead")
	machine := uuid.Must(uuid.NewV7()).String()
	if err := rig.enrol(iamdomain.Enrolment{
		PersonID: machine, Kind: iam.KindMachine, Stage: iam.StageActive,
		Login: "token:ops", OpID: "op-enrol-machine", Reason: "break-glass",
	}); err != nil {
		t.Fatalf("enrol the machine: %v", err)
	}
	rig.drain()
	rig.takeMoved()
	lineage := uuid.Must(uuid.NewV7()).String()

	for _, step := range []struct {
		name string
		do   func() error
		want iamdomain.Moved
	}{
		{"a sign-in names nothing", func() error {
			_, err := rig.writer.OpenSession(t.Context(), iamdomain.SessionStart{
				Lineage: lineage, Person: person,
				AbsoluteExpiresAt: brokerAt.Add(12 * time.Hour),
				OpID:              "session:" + lineage,
			})
			return err
		}, iamdomain.Moved{}},
		{"a sign-out names its session", func() error {
			_, err := rig.writer.CloseSession(t.Context(), lineage, person,
				"signed out", "close:"+lineage)
			return err
		}, iamdomain.Moved{Sessions: []string{lineage}}},
		{"a grant change names the person", func() error {
			_, err := rig.writer.UpdatePerson(t.Context(), iamdomain.PersonUpdate{
				PersonID: person, OpID: "op-narrow", Reason: "audit role",
				Apply: func(p iamdomain.Person) (iamdomain.Person, error) {
					p.Grants = []iam.Grant{iam.GrantStateRead}
					return p, nil
				},
			})
			return err
		}, iamdomain.Moved{Seats: true, People: []string{person},
			Logins: []string{"sarah.chen"}}},
		{"a revocation names the person", func() error {
			_, err := rig.writer.Revoke(t.Context(), person, "op-revoke",
				"signed out everywhere")
			return err
		}, iamdomain.Moved{People: []string{person}, Logins: []string{"sarah.chen"}}},
		{"a suspension names the person", func() error {
			_, err := rig.writer.SetStage(t.Context(), person, iam.StageSuspended,
				"op-suspend", "on leave")
			return err
		}, iamdomain.Moved{Seats: true, People: []string{person},
			Logins: []string{"sarah.chen"}}},
		{"a machine's suspension names the login a token binds through",
			func() error {
				_, err := rig.writer.SetStage(t.Context(), machine,
					iam.StageSuspended, "op-suspend-machine", "rotating")
				return err
			}, iamdomain.Moved{Seats: true, People: []string{machine},
				Logins: []string{"token:ops"}}},
		{"an unbind names the person it took the seat off", func() error {
			_, err := rig.writer.Release(t.Context(), iamdomain.KindSeat,
				"sarah-chen", person, "op-unbind", "moved teams")
			return err
		}, iamdomain.Moved{Seats: true, People: []string{person},
			Logins: []string{"sarah.chen"}}},
		{"a bind names the person bound", func() error {
			_, err := rig.writer.Claim(t.Context(), iamdomain.KindSeat,
				"sarah-chen", person, "op-rebind")
			return err
		}, iamdomain.Moved{Seats: true, People: []string{person},
			Logins: []string{"sarah.chen"}}},
		{"a rename names the person, the login it took and the one it left",
			func() error {
				_, err := rig.writer.Rename(t.Context(), person, "sarah.chen",
					"sarah.c", "op-rename", "married")
				return err
			}, iamdomain.Moved{People: []string{person},
				Logins: []string{"sarah.c", "sarah.chen"}}},
		{"ending every session names everyone", func() error {
			_, err := rig.writer.InvalidateAll(t.Context(), "op-invalidate",
				"restored from a backup")
			return err
		}, iamdomain.Moved{Everyone: true}},
		{"a removal names the person removed", func() error {
			_, err := rig.writer.Remove(t.Context(), other, "op-remove", "left")
			return err
		}, iamdomain.Moved{Seats: true, People: []string{other},
			Logins: []string{"noor.aziz"}}},
	} {
		if err := rig.draining(step.do); err != nil {
			t.Fatalf("%s: %v", step.name, err)
		}
		rig.drain()
		got := rig.takeMoved()
		slices.Sort(got.People)
		slices.Sort(got.Logins)
		got.Logins = slices.Compact(got.Logins)
		if got.Seats != step.want.Seats || got.Everyone != step.want.Everyone ||
			!slices.Equal(got.People, step.want.People) ||
			!slices.Equal(got.Logins, step.want.Logins) ||
			!slices.Equal(got.Sessions, step.want.Sessions) {
			t.Errorf("%s: the batch said %+v, want %+v", step.name, got, step.want)
		}
	}
}

// A BATCH THAT RETAINED A RECORD MOVES EVERYONE'S CREDENTIAL, and no seat.
//
// A record this node retains — a newer build's, or one signed under a key it
// was not restarted with — never reaches Apply, and whose it is sits inside
// the payload it could not open. What its commit did change is what this node
// can vouch for: every read of a person in its bucket answers unknown, and the
// guard refuses their REST requests 503. A socket held open on that person's
// session heard nothing, so a revocation a newer peer wrote kept streaming the
// company to the tab on every node still on the older build. So a retention
// names everyone, which closes every socket for its handshake to decide — and
// the guard answers that per bucket. It moves no seat's standing, because the
// contact routing is rebuilt from rows and a retained record wrote none.
//
// The control is a batch that applied nothing and retained nothing, which
// hands over nothing at all — before and after.
//
// Mutation: make Retained a no-op and the move never arrives.
func TestARetainedRecordNamesEveryone(t *testing.T) {
	t.Parallel()
	var got []iamdomain.Moved
	applier := iamdomain.NewApplier("node-a", func(m iamdomain.Moved) {
		got = append(got, m)
	})

	applier.Committed(t.Context())
	if len(got) != 0 {
		t.Fatalf("a batch that moved nothing handed over %+v", got)
	}

	applier.Retained(t.Context(), 1)
	applier.Committed(t.Context())
	if len(got) != 1 {
		t.Fatalf("a batch that retained a record handed over %d move(s), want 1",
			len(got))
	}
	if want := (iamdomain.Moved{Everyone: true}); got[0].Seats != want.Seats ||
		got[0].Everyone != want.Everyone || len(got[0].People) != 0 ||
		len(got[0].Logins) != 0 || len(got[0].Sessions) != 0 {
		t.Fatalf("a batch that retained a record said %+v, want %+v", got[0], want)
	}

	applier.Committed(t.Context())
	if len(got) != 1 {
		t.Fatalf("the retention was handed over again by the next batch: %+v", got)
	}
}
