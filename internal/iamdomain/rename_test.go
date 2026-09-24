package iamdomain_test

import (
	"errors"
	"slices"
	"testing"

	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/iamdomain"
	"github.com/crewlet/crewlet/internal/statelog"
)

// renameRig is a write rig holding three enrolled principals: the machine a
// Tier A token binds through, and two people.
type renameRig struct {
	*writeRig
	machine, sarah, dana string
}

func newRenameRig(t *testing.T) renameRig {
	t.Helper()
	return newRenameRigWith(t, nil)
}

// newRenameRigWith is [newRenameRig] with the publisher's appender wrapped,
// for a case about a step the broker never confirms.
func newRenameRigWith(t *testing.T,
	wrap func(statelog.Appender) statelog.Appender) renameRig {

	t.Helper()
	rig := renameRig{writeRig: newWriteRigWith(t, wrap),
		machine: "018f3a9c-0000-7000-8000-0000000006a1",
		sarah:   "018f3a9c-0000-7000-8000-0000000006a2",
		dana:    "018f3a9c-0000-7000-8000-0000000006a3",
	}
	for _, in := range []iamdomain.Enrolment{
		{PersonID: rig.machine, Kind: iam.KindMachine, Stage: iam.StageActive,
			Name: "The ops credential", Login: "token:ops", OpID: "op-machine"},
		{PersonID: rig.sarah, Kind: iam.KindPerson, Stage: iam.StageActive,
			Name: "Sarah Chen", Email: "sarah@example.com", Login: "sarah.chen",
			OpID: "op-sarah"},
		{PersonID: rig.dana, Kind: iam.KindPerson, Stage: iam.StageActive,
			Name: "Dana Sre", Email: "dana@example.com", Login: "dana.sre",
			OpID: "op-dana"},
	} {
		in.Reason = "a hire"
		if err := rig.enrol(in); err != nil {
			t.Fatalf("enrol %s: %v", in.Login, err)
		}
	}
	rig.drain()
	return rig
}

// loginOf is the login one person's row holds.
func (r renameRig) loginOf(person string) string {
	r.t.Helper()
	got := r.column(`SELECT login FROM iam_people WHERE id = ?`, person)
	if len(got) != 1 {
		r.t.Fatalf("person %s has %d rows", person, len(got))
	}
	return got[0]
}

// rename runs one rename through the whole path.
func (r renameRig) rename(person, from, to, opID string) error {
	r.t.Helper()
	return r.draining(func() error {
		_, err := r.writer.Rename(r.t.Context(), person, from, to, opID, "a rename")
		return err
	})
}

// A REFUSED RENAME CHANGES NOTHING.
//
// A rename used to RELEASE the old login and then claim the new one, so a new
// login its holder's grammar refused — `ops.bot` for a machine, `Jane.Doe` for
// anybody — or one somebody else held left the row with NO LOGIN: a person
// recorded as nobody, and the machine a Tier A token binds through silently
// unbound, so the token stopped acting as its seat. Claimed first, every one of
// those refusals is decided before anything is published.
func TestARefusedRenameChangesNothing(t *testing.T) {
	t.Parallel()
	rig := newRenameRig(t)
	for _, tc := range []struct {
		name, person, from, to string
		want                   error
	}{
		{"a machine taking a person's login", rig.machine, "token:ops",
			"ops.bot", iamdomain.ErrInvalidLogin},
		{"a person taking a login outside the grammar", rig.sarah,
			"sarah.chen", "Jane.Doe", iamdomain.ErrInvalidLogin},
	} {
		if err := rig.rename(tc.person, tc.from, tc.to,
			"op-"+tc.to); !errors.Is(err, tc.want) {
			t.Errorf("%s: refused with %v, want %v", tc.name, err, tc.want)
		}
		if got := rig.loginOf(tc.person); got != tc.from {
			t.Errorf("%s: the row's login is %q after a refused rename, want "+
				"%q untouched", tc.name, got, tc.from)
		}
	}
	// A LOGIN SOMEBODY ELSE HOLDS, refused naming them.
	var claimed *iamdomain.ErrClaimed
	if err := rig.rename(rig.sarah, "sarah.chen", "dana.sre",
		"op-taken"); !errors.As(err, &claimed) || claimed.Holder != rig.dana {
		t.Errorf("renaming onto a held login answered %v, want the claim "+
			"refused naming %s", err, rig.dana)
	}
	if got := rig.loginOf(rig.sarah); got != "sarah.chen" {
		t.Errorf("a rename refused as taken left login %q, want sarah.chen", got)
	}
	// AND A LOGIN IS NEVER RELEASED ON ITS OWN, which is the move the old
	// order made first.
	if err := rig.draining(func() error {
		_, err := rig.writer.Release(rig.t.Context(), iamdomain.KindLogin,
			"token:ops", rig.machine, "op-bare-release", "unbind")
		return err
	}); !errors.Is(err, iamdomain.ErrInvalidLogin) {
		t.Errorf("a bare login release answered %v, want %v", err,
			iamdomain.ErrInvalidLogin)
	}
	rig.drain()
	if got := rig.loginOf(rig.machine); got != "token:ops" {
		t.Errorf("the machine's login is %q, want token:ops", got)
	}
}

// A REFUSED MOVE BETWEEN SEATS LEAVES THE PERSON IN THE SEAT THEY HELD.
//
// Rebinding had the rename's order and the rename's hole: the old seat was
// released first, so a bind the chart refused (a seat this node's chart does
// not hold) or a seat somebody else held left the person bound to nothing —
// and every rule that asks "do you lead this" answered no for them until an
// administrator noticed.
func TestARefusedRebindLeavesTheSeatHeld(t *testing.T) {
	t.Parallel()
	rig := newRenameRig(t)
	rig.seatOnly("platform-lead")
	rig.seatOnly("design-lead")
	if err := rig.claim(iamdomain.KindSeat, "platform-lead", rig.sarah,
		"op-bind-sarah"); err != nil {
		t.Fatalf("bind sarah: %v", err)
	}
	if err := rig.claim(iamdomain.KindSeat, "design-lead", rig.dana,
		"op-bind-dana"); err != nil {
		t.Fatalf("bind dana: %v", err)
	}
	rig.drain()
	seatOf := func(person string) string {
		got := rig.column(`SELECT seat_id FROM iam_people WHERE id = ?`, person)
		if len(got) != 1 {
			t.Fatalf("person %s has %d rows", person, len(got))
		}
		return got[0]
	}
	rebind := func(to, opID string) error {
		return rig.draining(func() error {
			_, err := rig.writer.Rebind(rig.t.Context(), rig.sarah,
				"platform-lead", to, opID, "moved teams")
			return err
		})
	}
	if err := rebind("no-such-seat", "op-typo"); err == nil {
		t.Error("a move to a seat the chart does not hold was accepted")
	}
	var claimed *iamdomain.ErrClaimed
	if err := rebind("design-lead", "op-held"); !errors.As(err, &claimed) {
		t.Errorf("a move onto a held seat answered %v, want it refused as claimed", err)
	}
	rig.drain()
	if got := seatOf(rig.sarah); got != "platform-lead" {
		t.Errorf("after two refused moves sarah is bound to %q, want "+
			"platform-lead untouched", got)
	}

	// THE CONTROL: a move to a free seat lands, and frees the old one.
	rig.seatOnly("backend-lead")
	if err := rebind("backend-lead", "op-move"); err != nil {
		t.Fatalf("move: %v", err)
	}
	rig.drain()
	if got := seatOf(rig.sarah); got != "backend-lead" {
		t.Errorf("sarah is bound to %q, want backend-lead", got)
	}
	if got := rig.column(`SELECT id FROM iam_people
		WHERE seat_id = 'platform-lead'`); len(got) != 0 {
		t.Errorf("the seat sarah left is still held by %v", got)
	}
}

// A RENAME CLAIMS THE NEW LOGIN, FREES THE OLD ONE, AND SAYS SO ON BOTH.
//
// The claim's apply moves the column, so the old login is free the moment the
// new one lands; the release that follows closes the old subject's trail, so
// its last record is a release rather than a claim naming somebody who no
// longer holds it.
func TestARenameClaimsTheNewLoginAndFreesTheOld(t *testing.T) {
	t.Parallel()
	rig := newRenameRig(t)
	if err := rig.rename(rig.sarah, "sarah.chen", "sarah.c.chen",
		"op-rename"); err != nil {
		t.Fatalf("rename: %v", err)
	}
	rig.drain()
	if got := rig.loginOf(rig.sarah); got != "sarah.c.chen" {
		t.Errorf("the row's login is %q, want sarah.c.chen", got)
	}
	// THE OLD NAME IS FREE: somebody else takes it.
	if err := rig.rename(rig.dana, "dana.sre", "sarah.chen",
		"op-reuse"); err != nil {
		t.Fatalf("the freed login could not be taken: %v", err)
	}
	rig.drain()
	if got := rig.loginOf(rig.dana); got != "sarah.chen" {
		t.Errorf("dana's login is %q, want sarah.chen", got)
	}
	ops := rig.column(`SELECT op FROM iam_history
		WHERE object_kind = 'login' AND object_id = 'sarah.chen'
		ORDER BY version`)
	want := []string{"claim", "release", "claim"}
	if !slices.Equal(ops, want) {
		t.Errorf("the old login's trail is %v, want %v — the enrolment's "+
			"claim, the rename's release, the next holder's claim", ops, want)
	}
}

// A RENAME WHOSE RELEASE DID NOT LAND IS STILL A RENAME.
//
// The claim is the move: its apply put the new login on the row and took the
// old one off it. The release after it only closes the old login's trail, and
// one the broker never confirmed used to fail the rename with an error
// advising a retry under the same op id — which nothing could act on, since a
// caller re-reads the person first, finds them renamed and publishes nothing.
// It answers the claim's outcome now, under the gesture's op id; the old login
// is free, and its trail is short the one record, which is the whole residue.
//
// Mutation: fail the move on a release that did not land, and the rename that
// happened answers unknown.
func TestARenameWhoseReleaseDidNotLandIsStillARename(t *testing.T) {
	t.Parallel()
	var broker *silentBroker
	rig := newRenameRigWith(t, func(inner statelog.Appender) statelog.Appender {
		broker = &silentBroker{Appender: inner,
			on: iamdomain.LoginSubject("sarah.chen").Wire()}
		return broker
	})
	broker.silent.Store(true)
	var result statelog.Result
	if err := rig.draining(func() error {
		var err error
		result, err = rig.writer.Rename(t.Context(), rig.sarah, "sarah.chen",
			"sarah.c.chen", "op-rename", "a rename")
		return err
	}); err != nil {
		t.Fatalf("a rename whose claim landed answered %v", err)
	}
	if !broker.asked.Load() {
		t.Fatal("the release was never published, so this case proves nothing")
	}
	if result.Outcome == statelog.OutcomeUnknown || result.OpID != "op-rename" {
		t.Fatalf("answered %+v, want the claim's landed outcome under the "+
			"gesture's own op id", result)
	}
	rig.drain()
	if got := rig.loginOf(rig.sarah); got != "sarah.c.chen" {
		t.Errorf("the row's login is %q, want sarah.c.chen", got)
	}
	// THE OLD LOGIN IS FREE all the same — the claim's apply freed it.
	broker.silent.Store(false)
	if err := rig.rename(rig.dana, "dana.sre", "sarah.chen", "op-reuse"); err != nil {
		t.Fatalf("the freed login could not be taken: %v", err)
	}
	if got := rig.loginOf(rig.dana); got != "sarah.chen" {
		t.Errorf("dana's login is %q, want sarah.chen", got)
	}
}
