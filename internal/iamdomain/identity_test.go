package iamdomain_test

import (
	"errors"
	"slices"
	"sync"
	"testing"

	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/iamdomain"
	"github.com/crewlet/crewlet/internal/statelog"
)

// identityRig is a write rig holding three enrolled principals: the machine a
// Tier A token binds through, acting as itself, and two people, each on a
// human seat of their own — sarah-desk and dana-desk.
type identityRig struct {
	*writeRig
	machine, sarah, dana string
}

func newIdentityRig(t *testing.T) identityRig {
	t.Helper()
	rig := identityRig{writeRig: newWriteRig(t),
		machine: "018f3a9c-0000-7000-8000-0000000006a1",
		sarah:   "018f3a9c-0000-7000-8000-0000000006a2",
		dana:    "018f3a9c-0000-7000-8000-0000000006a3",
	}
	for _, in := range []iamdomain.Creation{
		{PersonID: rig.machine, Kind: iam.KindMachine, Stage: iam.StageActive,
			Name: "The ops credential", Login: "token:ops", OpID: "op-machine"},
		{PersonID: rig.sarah, Kind: iam.KindPerson, Stage: iam.StageActive,
			Name: "Sarah Chen", Email: "sarah@example.com", Login: "sarah.chen",
			Seat: rig.vacantSeat("sarah-desk"), LinkExpiresAt: firstLinkExpiry,
			OpID: "op-sarah"},
		{PersonID: rig.dana, Kind: iam.KindPerson, Stage: iam.StageActive,
			Name: "Dana Sre", Email: "dana@example.com", Login: "dana.sre",
			Seat: rig.vacantSeat("dana-desk"), LinkExpiresAt: firstLinkExpiry,
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

// held is the login and the seat one person's row holds.
func (r identityRig) held(person string) (login, seat string) {
	r.t.Helper()
	logins := r.column(`SELECT login FROM iam_people WHERE id = ?`, person)
	seats := r.column(`SELECT seat_id FROM iam_people WHERE id = ?`, person)
	if len(logins) != 1 || len(seats) != 1 {
		r.t.Fatalf("person %s has %d rows", person, len(logins))
	}
	return logins[0], seats[0]
}

// A REFUSED IDENTITY CHANGE MOVES NOTHING — neither the login nor the seat.
//
// A login and a seat move in ONE record, decided in its own snapshot, so a
// login its holder's grammar refuses — `ops.bot` for a machine, `Jane.Doe` for
// anybody — one somebody else holds, a login cleared, a person's seat cleared
// or a seat that is an agent's, is refused before anything is published, and
// whatever else the edit asked for does not move either. It used to be a
// sequence: a seat that moved before a refused login stayed moved, and a
// release that ran before a refused claim left a person with no login at all.
//
// Mutation: drop the login's grammar check, or the held-login check, from
// SetIdentity's decide, and a refused change lands; check the seat for
// existence alone rather than for a human seat, and the agent's seat is bound.
func TestARefusedIdentityChangeMovesNothing(t *testing.T) {
	t.Parallel()
	rig := newIdentityRig(t)
	rig.seatOnly("platform-lead")
	rig.agentSeat("release-bot")
	login := func(s string) *string { return &s }
	for _, tc := range []struct {
		name   string
		person string
		edit   iamdomain.IdentityEdit
		want   error
	}{
		{"a machine taking a person's login", rig.machine,
			iamdomain.IdentityEdit{Login: login("ops.bot")}, iamdomain.ErrInvalidLogin},
		{"a person taking a login outside the grammar", rig.sarah,
			iamdomain.IdentityEdit{Login: login("Jane.Doe"),
				Seat: login("platform-lead")}, iamdomain.ErrInvalidLogin},
		{"a login cleared", rig.sarah,
			iamdomain.IdentityEdit{Login: login("")}, iamdomain.ErrInvalidLogin},
		{"a person's seat cleared beside a rename", rig.sarah,
			iamdomain.IdentityEdit{Login: login("sarah.c"), Seat: login("")},
			iamdomain.ErrSeatRequired},
		{"a person moved onto an agent's seat", rig.sarah,
			iamdomain.IdentityEdit{Seat: login("release-bot")}, iamdomain.ErrInvalid},
		{"a service account bound to an agent's seat", rig.machine,
			iamdomain.IdentityEdit{Seat: login("release-bot")}, iamdomain.ErrInvalid},
	} {
		beforeLogin, beforeSeat := rig.held(tc.person)
		edit := tc.edit
		edit.PersonID, edit.OpID = tc.person, "op-"+tc.name
		if err := rig.during(func() error {
			_, err := rig.writer.SetIdentity(t.Context(), edit)
			return err
		}); !errors.Is(err, tc.want) {
			t.Errorf("%s: refused with %v, want %v", tc.name, err, tc.want)
		}
		if gotLogin, gotSeat := rig.held(tc.person); gotLogin != beforeLogin ||
			gotSeat != beforeSeat {
			t.Errorf("%s: the row holds (%q, %q) after a refused change, want "+
				"(%q, %q) untouched", tc.name, gotLogin, gotSeat, beforeLogin,
				beforeSeat)
		}
	}

	// A LOGIN SOMEBODY ELSE HOLDS, refused naming them — beside a seat that
	// is free, which does not move either.
	var taken *iamdomain.ErrTaken
	if err := rig.during(func() error {
		_, err := rig.writer.SetIdentity(t.Context(), iamdomain.IdentityEdit{
			PersonID: rig.sarah, Login: login("dana.sre"),
			Seat: login("platform-lead"), OpID: "op-taken"})
		return err
	}); !errors.As(err, &taken) || taken.Field != iamdomain.UniqueLogin ||
		taken.Person != rig.dana {
		t.Errorf("taking a held login answered %v, want it refused naming %s",
			err, rig.dana)
	}
	if gotLogin, gotSeat := rig.held(rig.sarah); gotLogin != "sarah.chen" ||
		gotSeat != "sarah-desk" {
		t.Errorf("a change refused as taken left (%q, %q), want (sarah.chen, "+
			"sarah-desk)", gotLogin, gotSeat)
	}
}

// A SEAT SOMEBODY ELSE HOLDS IS REFUSED NAMING THEM, AND A MOVE LANDS WHOLE.
//
// A move between seats is one record, so a seat this node's chart does not
// hold, one somebody else is bound to, or one an open invitation is on its way
// to, leaves the person in the seat they held — and a move to a free seat
// binds the new one and frees the old in that same record.
//
// Mutation: drop the held-seat check from SetIdentity's decide, and the move
// onto dana's seat lands; drop its open-invitation check and the move onto the
// invited seat does.
func TestASeatSomebodyElseHoldsIsRefusedNamingThem(t *testing.T) {
	t.Parallel()
	rig := newIdentityRig(t)
	rig.seatOnly("platform-lead")
	rig.seatOnly("design-lead")
	seat := func(s string) *string { return &s }
	bind := func(person, to, opID string) error {
		return rig.during(func() error {
			_, err := rig.writer.SetIdentity(t.Context(), iamdomain.IdentityEdit{
				PersonID: person, Seat: seat(to), OpID: opID, Reason: "moved"})
			return err
		})
	}
	if err := bind(rig.sarah, "platform-lead", "op-bind-sarah"); err != nil {
		t.Fatalf("bind sarah: %v", err)
	}
	if err := bind(rig.dana, "design-lead", "op-bind-dana"); err != nil {
		t.Fatalf("bind dana: %v", err)
	}
	if err := bind(rig.sarah, "no-such-seat", "op-typo"); !errors.Is(err,
		iamdomain.ErrInvalid) {
		t.Errorf("a move to a seat the chart does not hold answered %v, want "+
			"%v", err, iamdomain.ErrInvalid)
	}
	var taken *iamdomain.ErrTaken
	if err := bind(rig.sarah, "design-lead", "op-held"); !errors.As(err, &taken) ||
		taken.Field != iamdomain.UniqueSeat || taken.Person != rig.dana {
		t.Errorf("a move onto a held seat answered %v, want it refused "+
			"naming %s", err, rig.dana)
	}
	invited, err := inviteFor(t, rig.writeRig, "lee@example.com",
		rig.vacantSeat("data-lead"))
	if err != nil {
		t.Fatalf("invite onto data-lead: %v", err)
	}
	taken = nil
	if err := bind(rig.sarah, "data-lead", "op-invited"); !errors.As(err, &taken) ||
		taken.Field != iamdomain.UniqueSeat || taken.Invitation != invited.ID ||
		taken.Person != "" {
		t.Errorf("a move onto a seat an open invitation holds answered %v, "+
			"want it refused naming invitation %s", err, invited.ID)
	}
	if _, got := rig.held(rig.sarah); got != "platform-lead" {
		t.Errorf("after three refused moves sarah is bound to %q, want "+
			"platform-lead untouched", got)
	}

	// THE CONTROL: a move to a free seat lands, and frees the old one.
	rig.seatOnly("backend-lead")
	if err := bind(rig.sarah, "backend-lead", "op-move"); err != nil {
		t.Fatalf("move: %v", err)
	}
	if _, got := rig.held(rig.sarah); got != "backend-lead" {
		t.Errorf("sarah is bound to %q, want backend-lead", got)
	}
	if got := rig.column(`SELECT id FROM iam_people
		WHERE seat_id = 'platform-lead'`); len(got) != 0 {
		t.Errorf("the seat sarah left is still held by %v", got)
	}
}

// AN IDENTITY CHANGE FREES THE OLD LOGIN IN THE SAME RECORD.
//
// The record states the login the person holds from now on, so the old one is
// free the moment it lands — another person takes it at the next position —
// and the rename is ONE record on the trail, filed under the person.
//
// Mutation: make writeIdentity leave the old login on the row, and dana's
// rename onto it is refused as taken.
func TestAnIdentityChangeFreesTheOldLoginInTheSameRecord(t *testing.T) {
	t.Parallel()
	rig := newIdentityRig(t)
	login := func(s string) *string { return &s }
	rename := func(person, to, opID string) error {
		return rig.during(func() error {
			_, err := rig.writer.SetIdentity(t.Context(), iamdomain.IdentityEdit{
				PersonID: person, Login: login(to), OpID: opID, Reason: "a rename"})
			return err
		})
	}
	if err := rename(rig.sarah, "sarah.c.chen", "op-rename"); err != nil {
		t.Fatalf("rename: %v", err)
	}
	if got, _ := rig.held(rig.sarah); got != "sarah.c.chen" {
		t.Errorf("the row's login is %q, want sarah.c.chen", got)
	}
	if err := rename(rig.dana, "sarah.chen", "op-reuse"); err != nil {
		t.Fatalf("the freed login could not be taken: %v", err)
	}
	if got, _ := rig.held(rig.dana); got != "sarah.chen" {
		t.Errorf("dana's login is %q, want sarah.chen", got)
	}
	ops := rig.column(`SELECT op FROM iam_history
		WHERE object_kind = 'person' AND object_id = ? ORDER BY version`, rig.sarah)
	if want := []string{"enrol", "identity"}; !slices.Equal(ops, want) {
		t.Errorf("sarah's trail is %v, want %v — the enrolment and one record "+
			"for the rename", ops, want)
	}
}

// AN IDENTITY CHANGE THAT CHANGES NOTHING PUBLISHES NOTHING.
//
// An edit naming the login and the seat a person already holds is the answer
// rather than a refusal — applied, with no record, so the log does not grow by
// a change nobody made.
//
// Mutation: drop the nothing-to-move arm from SetIdentity's decide, and a
// record is published.
func TestAnIdentityChangeThatChangesNothingPublishesNothing(t *testing.T) {
	t.Parallel()
	rig := newIdentityRig(t)
	rig.seatOnly("platform-lead")
	seat := func(s string) *string { return &s }
	if err := rig.during(func() error {
		_, err := rig.writer.SetIdentity(t.Context(), iamdomain.IdentityEdit{
			PersonID: rig.sarah, Seat: seat("platform-lead"), OpID: "op-bind"})
		return err
	}); err != nil {
		t.Fatalf("bind: %v", err)
	}
	end, err := rig.log.End(t.Context())
	if err != nil {
		t.Fatalf("read the log's end: %v", err)
	}
	var result statelog.Result
	if err := rig.during(func() error {
		var err error
		result, err = rig.writer.SetIdentity(t.Context(), iamdomain.IdentityEdit{
			PersonID: rig.sarah, Login: seat("sarah.chen"),
			Seat: seat("platform-lead"), OpID: "op-again"})
		return err
	}); err != nil {
		t.Fatalf("an edit naming what is held answered %v", err)
	}
	if result.Outcome != statelog.OutcomeApplied || result.Position.Seq != 0 {
		t.Errorf("an edit naming what is held answered %+v, want applied at no "+
			"position", result)
	}
	if after, err := rig.log.End(t.Context()); err != nil || after != end {
		t.Errorf("the log moved from %d to %d (%v) for a change that changes "+
			"nothing", end, after, err)
	}
}

// TWO WRITERS RACING FOR ONE LOGIN LEAVE ONE HOLDER.
//
// An administrator enrolling somebody under a login and another renaming a
// colleague onto the same login, at the same instant, from two writers over
// one publisher: both decide from a snapshot in which the login is free, and
// both publish on the directory subject — so the broker accepts exactly one,
// and the loser decides again from rows that hold the winner and is refused
// naming them.
//
// Mutation: publish the identity record on the person's own subject instead,
// and both land.
func TestTwoWritersRacingForOneLoginLeaveOneHolder(t *testing.T) {
	t.Parallel()
	rig := newIdentityRig(t)
	const contested = "lee.park"
	newcomer := "018f3a9c-0000-7000-8000-0000000006a4"
	rig.seatOnly("lee-desk")
	admin := rig.writer.As(principalNamed("ana.admin", iam.KindPerson, iam.AllGrants))
	other := rig.writer.As(principalNamed("bo.admin", iam.KindPerson, iam.AllGrants))

	var (
		wg   sync.WaitGroup
		errs [2]error
	)
	start := make(chan struct{})
	if err := rig.during(func() error {
		wg.Add(2)
		go func() {
			defer wg.Done()
			<-start
			_, errs[0] = admin.Create(t.Context(), iamdomain.Creation{
				PersonID: newcomer, Kind: iam.KindPerson, Stage: iam.StageActive,
				Name: "Lee Park", Email: "lee@example.com", Login: contested,
				Seat: "lee-desk", LinkExpiresAt: firstLinkExpiry,
				OpID: "op-lee", Reason: "a hire"})
		}()
		go func() {
			defer wg.Done()
			<-start
			login := contested
			_, errs[1] = other.SetIdentity(t.Context(), iamdomain.IdentityEdit{
				PersonID: rig.dana, Login: &login, OpID: "op-dana-rename",
				Reason: "a rename"})
		}()
		close(start)
		wg.Wait()
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	won := 0
	for i, err := range errs {
		var taken *iamdomain.ErrTaken
		switch {
		case err == nil:
			won++
		case errors.As(err, &taken) && taken.Field == iamdomain.UniqueLogin:
		default:
			t.Errorf("writer %d answered %v, want a landing or the login "+
				"refused as taken", i, err)
		}
	}
	if won != 1 {
		t.Fatalf("%d of the two writers landed, want exactly one", won)
	}
	if holders := rig.column(`SELECT id FROM iam_people WHERE login = ?`,
		contested); len(holders) != 1 {
		t.Errorf("%s is held by %v, want exactly one person", contested, holders)
	}
}

// A PERSON'S SEAT IS CHANGED, BUT NEVER CLEARED (ADR-0026).
//
// A person holds a human seat for as long as they are here, so an edit that
// clears one is refused — decided on the KIND THE ROW HOLDS, never on one a
// caller states, and before the arm that answers an edit changing nothing, so
// a person recorded with no seat before the rule is told the rule too. Their
// seat is MOVED, in one record that frees the old one; a SERVICE ACCOUNT may
// be unbound, since it acts as itself. And a person recorded with no seat is
// not frozen by the rule: a rename leaving their binding alone lands, and
// binding them is a move like any other.
//
// Mutation: drop the person arm from SetIdentity's decide and Sarah's seat is
// cleared; move it after the nothing-to-move arm and the legacy person's
// unbind answers applied instead of the rule.
func TestAPersonsSeatIsChangedButNeverCleared(t *testing.T) {
	t.Parallel()
	rig := newIdentityRig(t)
	seat := func(s string) *string { return &s }
	edit := func(in iamdomain.IdentityEdit) error {
		return rig.during(func() error {
			_, err := rig.writer.SetIdentity(t.Context(), in)
			return err
		})
	}
	end := func() uint64 {
		t.Helper()
		end, err := rig.log.End(t.Context())
		if err != nil {
			t.Fatalf("read the log's end: %v", err)
		}
		return end
	}

	// CLEARED: refused, with nothing published and nothing moved.
	before := end()
	if err := edit(iamdomain.IdentityEdit{PersonID: rig.sarah, Seat: seat(""),
		OpID: "op-clear", Reason: "moved teams"}); !errors.Is(err,
		iamdomain.ErrSeatRequired) {
		t.Fatalf("clearing a person's seat answered %v, want %v", err,
			iamdomain.ErrSeatRequired)
	}
	if after := end(); after != before {
		t.Errorf("the refused clear published %d records", after-before)
	}
	if _, got := rig.held(rig.sarah); got != "sarah-desk" {
		t.Errorf("after the refused clear sarah holds %q, want sarah-desk", got)
	}

	// CHANGED: a move lands, and frees the seat she left in the same record.
	if err := edit(iamdomain.IdentityEdit{PersonID: rig.sarah,
		Seat: seat(rig.vacantSeat("platform-lead")), OpID: "op-move",
		Reason: "moved teams"}); err != nil {
		t.Fatalf("moving a person to another human seat: %v", err)
	}
	if _, got := rig.held(rig.sarah); got != "platform-lead" {
		t.Errorf("after the move sarah holds %q, want platform-lead", got)
	}
	if got := rig.column(`SELECT id FROM iam_people WHERE seat_id = 'sarah-desk'`); len(got) != 0 {
		t.Errorf("the seat sarah moved off is still held by %v", got)
	}

	// A SERVICE ACCOUNT MAY BE UNBOUND — the control the rule is about
	// people: bound to a seat and then bound to none, both landing.
	if err := edit(iamdomain.IdentityEdit{PersonID: rig.machine,
		Seat: seat("sarah-desk"), OpID: "op-bind-machine",
		Reason: "acts as the desk"}); err != nil {
		t.Fatalf("binding the service account: %v", err)
	}
	if err := edit(iamdomain.IdentityEdit{PersonID: rig.machine, Seat: seat(""),
		OpID: "op-unbind-machine", Reason: "acts as itself"}); err != nil {
		t.Fatalf("unbinding the service account: %v", err)
	}
	if _, got := rig.held(rig.machine); got != "" {
		t.Errorf("the unbound service account holds %q, want none", got)
	}

	// A PERSON RECORDED BEFORE THE RULE, holding no seat, as a build before
	// it left the row: told the rule when asked to clear what they do not
	// hold, renamed with their binding left alone, and bound by a move.
	plant(t, rig.writeRig, `UPDATE iam_people SET seat_id = '' WHERE id = ?`,
		rig.dana)
	if err := edit(iamdomain.IdentityEdit{PersonID: rig.dana, Seat: seat(""),
		OpID: "op-clear-legacy", Reason: "a no-op"}); !errors.Is(err,
		iamdomain.ErrSeatRequired) {
		t.Errorf("clearing a seatless person's seat answered %v, want %v — the "+
			"rule, whatever they hold", err, iamdomain.ErrSeatRequired)
	}
	if err := edit(iamdomain.IdentityEdit{PersonID: rig.dana,
		Login: seat("dana.okafor"), OpID: "op-rename-legacy",
		Reason: "a rename"}); err != nil {
		t.Errorf("renaming a seatless person was refused: %v", err)
	}
	if err := edit(iamdomain.IdentityEdit{PersonID: rig.dana,
		Seat: seat("dana-desk"), OpID: "op-seat-legacy",
		Reason: "given a seat"}); err != nil {
		t.Errorf("binding a seatless person to a seat was refused: %v", err)
	}
	if login, got := rig.held(rig.dana); login != "dana.okafor" ||
		got != "dana-desk" {
		t.Errorf("the legacy person holds (%q, %q), want (dana.okafor, "+
			"dana-desk)", login, got)
	}
}
