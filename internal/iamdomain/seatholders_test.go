package iamdomain_test

import (
	"errors"
	"maps"
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

// A REMOVAL'S SAY ENDS AT THE NEXT BIND.
//
// Omar holds the ops seat and is removed, which leaves a tombstone naming it;
// Lena is then bound to the seat. Unstamped, Omar's tombstone would go on
// withholding the seat's contact identities for ever while Lena held it.
func TestARemovalsSayEndsAtTheNextBind(t *testing.T) {
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

	lena := uuid.New().String()
	if err := rig.enrol(iamdomain.Enrolment{
		PersonID: lena, Kind: iam.KindPerson, Stage: iam.StageActive,
		Name: "Lena", Email: "lena@example.com", Login: "lena.fischer",
		OpID: "op-enrol-lena", Reason: "a hire",
	}); err != nil {
		t.Fatalf("enrol lena: %v", err)
	}
	rig.drain()
	if _, err := rig.writer.Claim(t.Context(), iamdomain.KindSeat, "ops-lead",
		lena, "op-bind-lena"); err != nil {
		t.Fatalf("bind lena: %v", err)
	}
	rig.drain()
	assertHolders(t, holdersBySeat(t, reader), map[string]iamdomain.SeatHolder{
		"ops-lead": {Seat: "ops-lead", Person: lena, Stage: iam.StageActive},
	})

	// AND IT STAYS ENDED when she is unbound: the seat goes back to the
	// company, not to Omar's tombstone.
	if _, err := rig.writer.Release(t.Context(), iamdomain.KindSeat, "ops-lead",
		lena, "op-unbind-lena", "moved teams"); err != nil {
		t.Fatalf("unbind lena: %v", err)
	}
	rig.drain()
	assertHolders(t, holdersBySeat(t, reader), map[string]iamdomain.SeatHolder{})
}

// A SEAT THE RUNNING COMPANY DOES NOT HOLD IS A VALUE THE CALLER TYPED, NOT A
// FAULT.
//
// The refusal is what an administrator reads when they mistype a handle, and
// it used to be an unclassified error every surface turned into a 500 — "the
// engine is broken" for a typo. A seat an applied revision removed is the
// same answer as one nobody ever created: the running company is the only
// organisation there is.
//
// The control is the bind to a human seat the company holds: it lands.
func TestBindingASeatTheRunningCompanyDoesNotHoldIsRefused(t *testing.T) {
	t.Parallel()
	rig := newWriteRig(t)
	person := bindNew(t, rig, "sarah.chen", "sarah-chen")
	rig.seatOnly("departed")
	rig.dropSeat("departed")
	for _, seat := range []string{"no-such-seat", "departed"} {
		_, err := rig.writer.Rebind(t.Context(), person, "sarah-chen",
			seat, "op-typo-"+seat, "a typo")
		if !errors.Is(err, iamdomain.ErrInvalid) {
			t.Errorf("a bind to %s answered %v, want %v", seat, err,
				iamdomain.ErrInvalid)
		}
	}
	if got := rig.column(`SELECT seat_id FROM iam_people WHERE id = ?`,
		person); len(got) != 1 || got[0] != "sarah-chen" {
		t.Errorf("after the refused binds sarah holds %v, want sarah-chen", got)
	}
}

// A WRITER HANDED NO ORGANISATION REFUSES EVERY SEAT BIND AS UNAVAILABLE.
//
// The check is advisory, but a writer with nothing to ask must not decide it
// as "no such seat" — an administrator would be told a real seat is a typo —
// nor bind unchecked. Unavailable is the answer a retry on a node that runs a
// company clears.
//
// The control is the same bind through the rig's own writer, which holds the
// organisation: it lands.
func TestABindWithNoSeatLookupIsUnavailable(t *testing.T) {
	t.Parallel()
	rig := newWriteRig(t)
	person := uuid.New().String()
	if err := rig.enrol(iamdomain.Enrolment{
		PersonID: person, Kind: iam.KindPerson, Stage: iam.StageActive,
		Name: "Sarah", Email: "sarah@example.com", Login: "sarah.chen",
		OpID: "op-enrol-sarah", Reason: "a hire",
	}); err != nil {
		t.Fatalf("enrol: %v", err)
	}
	rig.drain()
	rig.seatOnly("sarah-chen")
	deps := rig.nodeDeps
	deps.Seats = nil
	blind, err := iamdomain.NewWriter(deps)
	if err != nil {
		t.Fatalf("build a writer with no organisation: %v", err)
	}
	_, err = blind.Claim(t.Context(), iamdomain.KindSeat, "sarah-chen", person,
		"op-bind-blind")
	if !errors.Is(err, statelog.ErrUnavailable) {
		t.Errorf("a bind through a writer with no organisation answered %v, "+
			"want %v", err, statelog.ErrUnavailable)
	}
	if _, err := rig.writer.Claim(t.Context(), iamdomain.KindSeat, "sarah-chen",
		person, "op-bind-seen"); err != nil {
		t.Fatalf("the control: a bind through the rig's writer was refused: %v", err)
	}
	rig.drain()
	if got := rig.column(`SELECT seat_id FROM iam_people WHERE id = ?`,
		person); len(got) != 1 || got[0] != "sarah-chen" {
		t.Errorf("after the control sarah holds %v, want sarah-chen", got)
	}
}

// WHICH SEATS ARE HELD IS THREE-VALUED, and a read this node cannot perform is
// the third value rather than "nobody".
//
// It answered not-held on an error, so the chart's continuous report named
// every human seat in the company as held by nobody for as long as a store
// fault lasted, and the seat listing's `unheld` filter listed all of them under
// a parameter promising the vacancies. And only an ACTIVE holder holds: a
// suspended person's seat is one nobody can act as.
func TestWhichSeatsAreHeldIsThreeValued(t *testing.T) {
	t.Parallel()
	rig := newWriteRig(t)
	reader := rig.reader(t)
	bindNew(t, rig, "sarah.chen", "sarah-chen")
	suspended := bindNew(t, rig, "priya.shah", "platform-lead")
	if _, err := rig.writer.SetStage(t.Context(), suspended, iam.StageSuspended,
		"op-suspend", "on leave"); err != nil {
		t.Fatalf("suspend: %v", err)
	}
	rig.drain()

	held, err := reader.HeldSeats(t.Context())
	if err != nil {
		t.Fatalf("HeldSeats: %v", err)
	}
	if want := map[string]bool{"sarah-chen": true}; !maps.Equal(held, want) {
		t.Errorf("held seats = %v, want %v — an active holder only", held, want)
	}

	if err := rig.db.CloseReplicated(); err != nil {
		t.Fatalf("close the replicated estate: %v", err)
	}
	if held, err = reader.HeldSeats(t.Context()); err == nil {
		t.Errorf("an estate this node cannot read answered %v with no error",
			held)
	}
	if !errors.Is(err, store.ErrNoEstate) {
		t.Errorf("the unreadable arm answered %v, want it to carry %v", err,
			store.ErrNoEstate)
	}
}
