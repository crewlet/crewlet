package iamdomain_test

import (
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/iamdomain"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/store"
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

// THE BINDINGS READ IS EXACTLY THE BOUND PEOPLE, with what the seat table
// needs of each and the same values a directory page carries.
//
// It is what the dangling-binding alarm reads on every heartbeat instead of
// walking the whole directory, so it must neither miss a binding the page walk
// would have classified nor answer for somebody bound to nothing.
func TestSeatBindingsIsEveryBindingAndNothingElse(t *testing.T) {
	t.Parallel()
	rig := newWriteRig(t)
	reader := rig.reader(t)

	sarah := bindNew(t, rig, "sarah.chen", "sarah-chen")
	priya := bindNew(t, rig, "priya.shah", "platform-lead")
	if _, err := rig.writer.SetStage(t.Context(), priya, iam.StageSuspended,
		"op-suspend", "on leave"); err != nil {
		t.Fatalf("suspend: %v", err)
	}
	rig.drain()
	unbound := bindNew(t, rig, "omar.haddad", "ops-lead")
	if _, err := rig.writer.Release(t.Context(), iamdomain.KindSeat, "ops-lead",
		unbound, "op-unbind", "moved teams"); err != nil {
		t.Fatalf("unbind: %v", err)
	}
	rig.drain()

	bindings, err := reader.SeatBindings(t.Context())
	if err != nil {
		t.Fatalf("SeatBindings: %v", err)
	}
	got := map[string]iamdomain.SeatBinding{}
	for _, b := range bindings {
		got[b.Person] = b
	}
	if len(got) != 2 {
		t.Fatalf("read %d bindings, want the two people still bound: %+v",
			len(bindings), bindings)
	}
	page, err := reader.People(t.Context(), iamdomain.PeopleQuery{Limit: iamdomain.MaxPageSize})
	if err != nil {
		t.Fatalf("People: %v", err)
	}
	for _, row := range page.People {
		if row.Seat == "" {
			continue
		}
		if b := got[row.ID]; b != row.Binding() {
			t.Errorf("person %s: the bindings read says %+v, the directory page %+v",
				row.ID, b, row.Binding())
		}
	}
	if got[sarah].Stage != iam.StageActive || got[priya].Stage != iam.StageSuspended {
		t.Errorf("stages read back as %q and %q", got[sarah].Stage, got[priya].Stage)
	}
}

// A SEAT IS BOUND BY THE HANDLE IT WAS CREATED UNDER (ADR-0020).
//
// Whatever address an administrator types — the handle a seat answers to now,
// one it used to, the one it was created under — the binding names the seat's
// IDENTITY, so one seat is one claim subject for as long as the company exists.
// Keyed on the address typed at the time, a renamed seat was claimable a second
// time under its new handle: the two subjects never contend, and two people
// acted as one seat.
func TestASeatIsBoundByTheHandleItWasCreatedUnder(t *testing.T) {
	t.Parallel()
	rig := newWriteRig(t)
	sarah := bindNew(t, rig, "sarah.chen", "sarah-chen")
	rig.renameSeat("sarah-chen", "sarah")

	// A SECOND PERSON, NAMING THE SEAT BY ITS NEW HANDLE AND BY ITS OLD:
	// both are the same seat, and it is Sarah's.
	tom := uuid.New().String()
	if err := rig.enrol(iamdomain.Enrolment{
		PersonID: tom, Kind: iam.KindPerson, Stage: iam.StageActive,
		Name: "Tom", Email: "tom@example.com", Login: "tom.reyes",
		OpID: "op-enrol-tom", Reason: "a hire",
	}); err != nil {
		t.Fatalf("enrol tom: %v", err)
	}
	rig.drain()
	for _, address := range []string{"sarah", "sarah-chen"} {
		_, err := rig.writer.Claim(t.Context(), iamdomain.KindSeat, address,
			tom, "op-bind-tom-"+address)
		var claimed *iamdomain.ErrClaimed
		if !errors.As(err, &claimed) || claimed.Holder != sarah {
			t.Errorf("binding a second person to %q answered %v, want it "+
				"refused as Sarah's seat", address, err)
		}
		rig.drain()
	}
	if got := rig.column(`SELECT id FROM iam_people WHERE seat_id != ''`); len(got) != 1 {
		t.Errorf("%d people are bound after the renamed seat was claimed "+
			"again: %v", len(got), got)
	}
	if got := rig.column(`SELECT seat_id FROM iam_people WHERE id = ?`,
		sarah); len(got) != 1 || got[0] != "sarah-chen" {
		t.Errorf("sarah's binding reads %v, want the identity sarah-chen", got)
	}

	// AND A MOVE TO THE SEAT SHE HOLDS, BY ITS NEW NAME, IS NOTHING TO DO:
	// applied with no record, rather than a claim and a release that would
	// leave her bound to nothing.
	moved, err := rig.writer.Rebind(t.Context(), sarah, "sarah-chen", "sarah",
		"op-rebind-same", "tidying up")
	if err != nil || moved.Outcome != statelog.OutcomeApplied {
		t.Errorf("a move onto the seat she holds answered %+v, %v", moved, err)
	}
	rig.drain()
	if got := rig.column(`SELECT seat_id FROM iam_people WHERE id = ?`,
		sarah); len(got) != 1 || got[0] != "sarah-chen" {
		t.Errorf("after a move onto her own seat sarah's binding reads %v", got)
	}

	// THE CONTROL: a seat nobody holds, named by its new handle, is bound
	// under the handle it was created under.
	rig.seatOnly("design-lead")
	rig.renameSeat("design-lead", "design-head")
	if _, err := rig.writer.Claim(t.Context(), iamdomain.KindSeat,
		"design-head", tom, "op-bind-tom-design"); err != nil {
		t.Fatalf("bind tom to the renamed design seat: %v", err)
	}
	rig.drain()
	if got := rig.column(`SELECT seat_id FROM iam_people WHERE id = ?`,
		tom); len(got) != 1 || got[0] != "design-lead" {
		t.Errorf("tom's binding reads %v, want the identity design-lead", got)
	}
}

// A REMOVAL'S SAY ENDS AT THE NEXT BIND, WHATEVER THE SEAT IS CALLED BY THEN.
//
// Omar holds the ops seat and is removed, which leaves a tombstone naming it;
// the chart then renames the seat, and Lena is bound to it under the new handle
// — the only one the chart will accept for a live seat. Keyed on the handle
// typed at each bind, the stamp matched only tombstones naming the NEW handle,
// so Omar's went on withholding the seat's contact identities for ever while
// Lena held it.
func TestARemovalsSayEndsAtTheNextBindUnderAnyHandle(t *testing.T) {
	t.Parallel()
	rig := newWriteRig(t)
	reader := rig.reader(t)
	omar := bindNew(t, rig, "omar.haddad", "ops-lead")
	if _, err := rig.writer.Remove(t.Context(), omar, "op-remove",
		"left the company"); err != nil {
		t.Fatalf("remove omar: %v", err)
	}
	rig.drain()
	assertHolders(t, holdersBySeat(t, reader), map[string]iamdomain.SeatHolder{
		"ops-lead": {Seat: "ops-lead", Person: omar, Removed: true},
	})

	rig.renameSeat("ops-lead", "ops-head")
	lena := uuid.New().String()
	if err := rig.enrol(iamdomain.Enrolment{
		PersonID: lena, Kind: iam.KindPerson, Stage: iam.StageActive,
		Name: "Lena", Email: "lena@example.com", Login: "lena.fischer",
		OpID: "op-enrol-lena", Reason: "a hire",
	}); err != nil {
		t.Fatalf("enrol lena: %v", err)
	}
	rig.drain()
	if _, err := rig.writer.Claim(t.Context(), iamdomain.KindSeat, "ops-head",
		lena, "op-bind-lena"); err != nil {
		t.Fatalf("bind lena: %v", err)
	}
	rig.drain()
	assertHolders(t, holdersBySeat(t, reader), map[string]iamdomain.SeatHolder{
		"ops-lead": {Seat: "ops-lead", Person: lena, Stage: iam.StageActive},
	})

	// AND IT STAYS ENDED when she is unbound: the seat goes back to the
	// chart, not to Omar's tombstone.
	if _, err := rig.writer.Release(t.Context(), iamdomain.KindSeat, "ops-lead",
		lena, "op-unbind-lena", "moved teams"); err != nil {
		t.Fatalf("unbind lena: %v", err)
	}
	rig.drain()
	assertHolders(t, holdersBySeat(t, reader), map[string]iamdomain.SeatHolder{})
}

// A RENAME BETWEEN THE RESOLUTION AND THE SNAPSHOT IS A CONFLICT, NEVER A CLAIM.
//
// The subject a bind arbitrates on has to be known before the framework takes
// the snapshot its decide runs in, so the address is turned into an identity
// first and confirmed again inside the decide. Here the resolution reads a
// chart in which `sarah` is still the seat created as `sarah`, and the snapshot
// one in which `sarah` is the seat created as `sarah-chen`: published anyway,
// the bind would claim an identity no seat has.
func TestARenameBetweenTheResolutionAndTheDecideIsAConflict(t *testing.T) {
	t.Parallel()
	rig := newWriteRig(t)
	sarah := bindNew(t, rig, "sarah.chen", "sarah-chen")
	if _, err := rig.writer.Release(t.Context(), iamdomain.KindSeat,
		"sarah-chen", sarah, "op-unbind", "moving"); err != nil {
		t.Fatalf("unbind: %v", err)
	}
	rig.drain()
	rig.renameSeat("sarah-chen", "sarah")

	// THE STALE READ: a second store whose chart still has `sarah` as a
	// seat that was never renamed, standing in for the instant before the
	// rename landed.
	stale, err := store.Open(t.Context(), filepath.Join(t.TempDir(), "stale.db"),
		store.Options{PinnedWriters: 1})
	if err != nil {
		t.Fatalf("open the stale store: %v", err)
	}
	t.Cleanup(func() { _ = stale.Close() })
	staleRig := &writeRig{t: t, db: stale}
	staleRig.seatOnly("sarah")
	writer, err := iamdomain.NewWriter(iamdomain.WriterDeps{
		Publisher: rig.publisher, DB: stale, Actor: "ana.admin",
		ActorKind: iam.KindPerson, Grants: iam.AllGrants,
		Now: func() time.Time { return brokerAt },
	})
	if err != nil {
		t.Fatalf("build the writer: %v", err)
	}
	_, err = writer.Claim(t.Context(), iamdomain.KindSeat, "sarah", sarah,
		"op-bind-raced")
	if !errors.Is(err, statelog.ErrConflict) {
		t.Errorf("a bind whose seat was renamed under it answered %v, want %v",
			err, statelog.ErrConflict)
	}
	rig.drain()
	if got := rig.column(`SELECT seat_id FROM iam_people WHERE id = ?`,
		sarah); len(got) != 1 || got[0] != "" {
		t.Errorf("sarah's binding reads %v after a refused bind, want none", got)
	}
}

// A SEAT NOBODY CREATED IS A VALUE THE CALLER TYPED, NOT A FAULT.
//
// The refusal is what an administrator reads when they mistype a handle, and
// it used to be an unclassified error every surface turned into a 500 — "the
// engine is broken" for a typo.
func TestBindingASeatTheChartDoesNotHoldIsRefusedAsInvalid(t *testing.T) {
	t.Parallel()
	rig := newWriteRig(t)
	person := bindNew(t, rig, "sarah.chen", "sarah-chen")
	_, err := rig.writer.Rebind(t.Context(), person, "sarah-chen",
		"no-such-seat", "op-typo", "a typo")
	if !errors.Is(err, iamdomain.ErrInvalid) {
		t.Errorf("a bind to a seat the chart does not hold answered %v, want %v",
			err, iamdomain.ErrInvalid)
	}
}
