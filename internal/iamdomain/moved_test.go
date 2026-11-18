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
// Naming nobody leaves an ended credential's open dashboard tab streaming the
// company; naming everybody ends every socket on the node for every unbind.
// So each gesture names the session it ended, or the person whose row it
// moved WITH THEIR LOGIN (a rename, both), since a Tier A token binds through
// the row's login and never learns its id. Only the fleet-wide generation
// names everyone; a sign-in, the bulk of this log's traffic, names nothing.
//
// Mutation: drop the old login from an identity change's move, or the login
// from a person's move, and the rename or the machine's suspension names less
// than it moved.
func TestABatchSaysWhoseCredentialsItMoved(t *testing.T) {
	t.Parallel()
	rig := newWriteRig(t)
	person := bindNew(t, rig, "sarah.chen", "sarah-chen")
	other := bindNew(t, rig, "noor.aziz", "ops-lead")
	machine := uuid.Must(uuid.NewV7()).String()
	if err := rig.enrol(iamdomain.Creation{
		PersonID: machine, Kind: iam.KindMachine, Stage: iam.StageActive,
		Login: "token:ops", Seat: rig.vacantSeat("ops-desk"),
		OpID: "op-enrol-machine", Reason: "break-glass",
	}); err != nil {
		t.Fatalf("enrol the machine: %v", err)
	}
	rig.drain()
	rig.vacantSeat("sarah-desk")
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
					p.Grants = []iam.Grant{iam.GrantWorklet}
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
		{"a move names the person it moved", func() error {
			seat := "sarah-desk"
			_, err := rig.writer.SetIdentity(t.Context(), iamdomain.IdentityEdit{
				PersonID: person, Seat: &seat, OpID: "op-move",
				Reason: "moved teams"})
			return err
		}, iamdomain.Moved{Seats: true, People: []string{person},
			Logins: []string{"sarah.chen"}}},
		{"an unbind names the service account it took the seat off",
			func() error {
				unbound := ""
				_, err := rig.writer.SetIdentity(t.Context(), iamdomain.IdentityEdit{
					PersonID: machine, Seat: &unbound, OpID: "op-unbind",
					Reason: "acts as itself"})
				return err
			}, iamdomain.Moved{Seats: true, People: []string{machine},
				Logins: []string{"token:ops"}}},
		{"a bind names the principal bound", func() error {
			seat := "ops-desk"
			_, err := rig.writer.SetIdentity(t.Context(), iamdomain.IdentityEdit{
				PersonID: machine, Seat: &seat, OpID: "op-rebind"})
			return err
		}, iamdomain.Moved{Seats: true, People: []string{machine},
			Logins: []string{"token:ops"}}},
		{"a rename names the person, the login it took and the one it left",
			func() error {
				login := "sarah.c"
				_, err := rig.writer.SetIdentity(t.Context(), iamdomain.IdentityEdit{
					PersonID: person, Login: &login, OpID: "op-rename",
					Reason: "married"})
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
			Logins: []string{"noor.aziz"}, Removed: []string{other}}},
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
			!slices.Equal(got.Sessions, step.want.Sessions) ||
			!slices.Equal(got.Removed, step.want.Removed) {
			t.Errorf("%s: the batch said %+v, want %+v", step.name, got, step.want)
		}
	}
}

// A BATCH THAT RETAINED A RECORD MOVES EVERYONE'S CREDENTIAL, and no seat.
//
// A retained record (a newer build's, or one under a key this node lacks)
// never reaches Apply, and whose it is sits inside the payload. Its commit
// does make every read of a person in its bucket answer unknown (REST 503),
// so a socket on that person's session must be re-decided: naming everyone
// closes every socket for its handshake, which the guard answers per bucket.
// It moves no seat: routing is rebuilt from rows, and a retained record wrote
// none. The control, before and after, is a batch that applied and retained
// nothing, which hands over nothing.
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
