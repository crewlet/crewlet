package iamdomain_test

import (
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/iamdomain"
)

// THE DIRECTORY SAYS WHO HOLDS EACH SEAT, AT WHAT STAGE — and who held one
// until they were removed.
//
// It is the read contact routing withdraws a suspended person's identities
// from, so each arm is a routing decision somebody downstream makes: an active
// holder routes, a suspended one must not, and a removal must not route MORE
// than the suspension before it. The removal arm is the one that needs the
// tombstone — the removal released the seat with every other claim, so without
// it the seat reads as held by nobody and falls back to the chart's contact
// map, which still names the leaver's own accounts.
func TestSeatHoldersNamesWhoHoldsEachSeatAndAtWhatStage(t *testing.T) {
	t.Parallel()
	rig := newWriteRig(t)
	reader := rig.reader(t)

	active := bindNew(t, rig, "sarah.chen", "sarah-chen")
	suspended := bindNew(t, rig, "priya.shah", "platform-lead")
	removed := bindNew(t, rig, "omar.haddad", "ops-lead")

	if _, err := rig.writer.SetStage(t.Context(), suspended, iam.StageSuspended,
		"op-suspend", "on leave"); err != nil {
		t.Fatalf("suspend: %v", err)
	}
	rig.drain()
	if _, err := rig.writer.Remove(t.Context(), removed, "op-remove",
		"left the company"); err != nil {
		t.Fatalf("remove: %v", err)
	}
	rig.drain()

	holders := holdersBySeat(t, reader)
	want := map[string]iamdomain.SeatHolder{
		"sarah-chen":    {Seat: "sarah-chen", Person: active, Stage: iam.StageActive},
		"platform-lead": {Seat: "platform-lead", Person: suspended, Stage: iam.StageSuspended},
		"ops-lead":      {Seat: "ops-lead", Person: removed, Removed: true},
	}
	assertHolders(t, holders, want)

	// A NEW HOLDER IS THE SEAT'S LAST WORD, and the tombstone stops being
	// read: without that a seat somebody was removed from could never route
	// again, whoever was hired into it.
	successor := bindNew(t, rig, "lena.fischer", "ops-lead")
	want["ops-lead"] = iamdomain.SeatHolder{
		Seat: "ops-lead", Person: successor, Stage: iam.StageActive}
	assertHolders(t, holdersBySeat(t, reader), want)

	// AND UNBINDING THAT SUCCESSOR DOES NOT BRING THE LEAVER BACK. The
	// tombstone spoke for the binding the removal released; the successor's
	// bind was a later binding with a standing of its own, and its unbind
	// hands the seat to the chart like any other unbind. Read from the
	// seat's current rows alone, the removal became the seat's last word
	// again here and withheld it indefinitely — whatever its contact map
	// had since been pointed at.
	if _, err := rig.writer.Release(t.Context(), iamdomain.KindSeat, "ops-lead",
		successor, "op-unbind", "moved teams"); err != nil {
		t.Fatalf("unbind: %v", err)
	}
	rig.drain()
	delete(want, "ops-lead")
	assertHolders(t, holdersBySeat(t, reader), want)

	// AND A SUCCESSOR WHO IS THEMSELVES REMOVED WHILE HOLDING IT leaves a
	// tombstone of their own, which speaks for THEIR binding: the rule is
	// per removal, not a seat that stopped being withheld for ever.
	second := bindNew(t, rig, "noor.aziz", "ops-lead")
	if _, err := rig.writer.Remove(t.Context(), second, "op-remove-second",
		"left the company"); err != nil {
		t.Fatalf("remove the second holder: %v", err)
	}
	rig.drain()
	want["ops-lead"] = iamdomain.SeatHolder{Seat: "ops-lead", Person: second, Removed: true}
	assertHolders(t, holdersBySeat(t, reader), want)
}

// THE APPLIER TELLS THIS NODE ONLY WHEN A SEAT'S STANDING MAY HAVE MOVED.
//
// Its listener rebuilds the whole notify registry, so the signal is worth
// exactly what it saves: a sign-in is the bulk of this log's traffic and moves
// no seat's standing, and a rebuild per login would be a registry rebuilt per
// login for nothing — while a suspension, a bind, an unbind and a removal each
// must reach it, or contact routing waits for the periodic safety net.
func TestTheApplierSignalsTheDirectoryOnlyWhenAStandingMoved(t *testing.T) {
	t.Parallel()
	rig := newWriteRig(t)

	person := bindNew(t, rig, "sarah.chen", "sarah-chen")
	if rig.directory.Load() == 0 {
		t.Fatal("an enrolment and a bind moved nothing the directory reads")
	}

	for _, step := range []struct {
		name  string
		moves bool
		do    func() error
	}{
		{"a sign-in", false, func() error {
			_, err := rig.writer.OpenSession(t.Context(), iamdomain.SessionStart{
				Lineage: uuid.NewString(), Person: person, OpID: "op-session",
				AbsoluteExpiresAt: brokerAt.Add(12 * time.Hour),
			})
			return err
		}},
		{"a suspension", true, func() error {
			_, err := rig.writer.SetStage(t.Context(), person, iam.StageSuspended,
				"op-suspend", "on leave")
			return err
		}},
		{"an unbind", true, func() error {
			_, err := rig.writer.Release(t.Context(), iamdomain.KindSeat,
				"sarah-chen", person, "op-unbind", "moved teams")
			return err
		}},
		{"a bind", true, func() error {
			_, err := rig.writer.Claim(t.Context(), iamdomain.KindSeat,
				"sarah-chen", person, "op-rebind")
			return err
		}},
		{"a removal", true, func() error {
			_, err := rig.writer.Remove(t.Context(), person, "op-remove", "left")
			return err
		}},
	} {
		rig.directory.Store(0)
		if err := step.do(); err != nil {
			t.Fatalf("%s: %v", step.name, err)
		}
		rig.drain()
		if moved := rig.directory.Load() > 0; moved != step.moves {
			t.Errorf("%s signalled the directory: %v, want %v", step.name,
				moved, step.moves)
		}
	}
}

// bindNew enrols one active person and binds them to a seat, answering their
// id.
func bindNew(t *testing.T, rig *writeRig, login, seat string) string {
	t.Helper()
	id := uuid.New().String()
	if err := rig.enrol(iamdomain.Enrolment{
		PersonID: id, Kind: iam.KindPerson, Stage: iam.StageActive,
		Name: login, Email: login + "@example.com", Login: login,
		OpID: "op-enrol-" + id, Reason: "a hire",
	}); err != nil {
		t.Fatalf("enrol %s: %v", login, err)
	}
	rig.drain()
	rig.seatOnly(seat)
	if _, err := rig.writer.Claim(t.Context(), iamdomain.KindSeat, seat, id,
		"op-bind-"+id); err != nil {
		t.Fatalf("bind %s to %s: %v", login, seat, err)
	}
	rig.drain()
	return id
}

// holdersBySeat is the directory's answer keyed by seat, refusing a seat the
// answer names twice.
func holdersBySeat(t *testing.T, reader *iamdomain.Reader) map[string]iamdomain.SeatHolder {
	t.Helper()
	holders, err := reader.SeatHolders(t.Context())
	if err != nil {
		t.Fatalf("SeatHolders: %v", err)
	}
	out := make(map[string]iamdomain.SeatHolder, len(holders))
	for _, h := range holders {
		if _, dup := out[h.Seat]; dup {
			t.Fatalf("seat %q answered twice: %+v", h.Seat, holders)
		}
		out[h.Seat] = h
	}
	return out
}

func assertHolders(t *testing.T, got, want map[string]iamdomain.SeatHolder) {
	t.Helper()
	if len(got) != len(want) {
		t.Errorf("the directory names %d seats, want %d: %+v", len(got), len(want), got)
	}
	for seat, w := range want {
		if g := got[seat]; g != w {
			t.Errorf("seat %q: got %+v, want %+v", seat, g, w)
		}
	}
}
