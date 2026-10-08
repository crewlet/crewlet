package iamdomain_test

import (
	"database/sql"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/iamdomain"
	"github.com/crewlet/crewlet/internal/statelog"
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

	// AND MOVING THAT SUCCESSOR OFF IT DOES NOT BRING THE LEAVER BACK. The
	// tombstone spoke for the binding the removal released; the successor's
	// was a later binding with a standing of its own, and moving them to
	// another seat hands this one to the chart like any other seat nobody
	// holds. Read from the seat's current rows alone, the removal became
	// the seat's last word again here and withheld it indefinitely —
	// whatever its contact map had since been pointed at.
	if _, err := rig.writer.SetIdentity(t.Context(), iamdomain.IdentityEdit{
		PersonID: successor, Seat: seatRef(rig.vacantSeat("support-lead")),
		OpID: "op-move", Reason: "moved teams"}); err != nil {
		t.Fatalf("move: %v", err)
	}
	rig.drain()
	delete(want, "ops-lead")
	want["support-lead"] = iamdomain.SeatHolder{
		Seat: "support-lead", Person: successor, Stage: iam.StageActive}
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
// login for nothing — while a suspension, a move between seats, a service
// account's unbind and a removal each must reach it, or contact routing waits
// for the periodic safety net.
func TestTheApplierSignalsTheDirectoryOnlyWhenAStandingMoved(t *testing.T) {
	t.Parallel()
	rig := newWriteRig(t)

	person := bindNew(t, rig, "sarah.chen", "sarah-chen")
	if rig.directory.Load() == 0 {
		t.Fatal("an enrolment onto a seat moved nothing the directory reads")
	}
	machine := uuid.Must(uuid.NewV7()).String()
	if err := rig.enrol(iamdomain.Creation{
		PersonID: machine, Kind: iam.KindMachine, Stage: iam.StageActive,
		Name: "Release pipeline", Login: "svc:release",
		Seat: rig.vacantSeat("release-desk"), OpID: "op-enrol-machine",
		Reason: "the pipeline",
	}); err != nil {
		t.Fatalf("enrol the service account onto a seat: %v", err)
	}
	rig.drain()
	rig.vacantSeat("sarah-desk")

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
		{"a move to another seat", true, func() error {
			_, err := rig.writer.SetIdentity(t.Context(), iamdomain.IdentityEdit{PersonID: person, Seat: seatRef("sarah-desk"), OpID: "op-move", Reason: "moved teams"})
			return err
		}},
		{"a move back", true, func() error {
			_, err := rig.writer.SetIdentity(t.Context(), iamdomain.IdentityEdit{PersonID: person, Seat: seatRef("sarah-chen"), OpID: "op-move-back"})
			return err
		}},
		{"a service account's unbind", true, func() error {
			_, err := rig.writer.SetIdentity(t.Context(), iamdomain.IdentityEdit{PersonID: machine, Seat: seatRef(""), OpID: "op-unbind", Reason: "acts as itself"})
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

// bindNew creates one active person ON a seat — the one record a person
// arrives in their seat by — answering their id.
func bindNew(t *testing.T, rig *writeRig, login, seat string) string {
	t.Helper()
	id := uuid.Must(uuid.NewV7()).String()
	if err := rig.enrol(iamdomain.Creation{
		PersonID: id, Kind: iam.KindPerson, Stage: iam.StageActive,
		Name: login, Email: login + "@example.com", Login: login,
		Seat: rig.vacantSeat(seat), LinkExpiresAt: firstLinkExpiry,
		OpID: "op-enrol-" + id, Reason: "a hire",
	}); err != nil {
		t.Fatalf("enrol %s onto %s: %v", login, seat, err)
	}
	rig.drain()
	return id
}

// seatRef is a seat an identity edit names, "" for none.
func seatRef(seat string) *string { return &seat }

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

// THE CLAIMS READ IS EXACTLY THE BOUND PRINCIPALS AND THE OPEN INVITATIONS,
// with what a seat listing needs of each and the same values a directory page
// and the invitation listing carry.
//
// It is what the seat listing and a company write that takes a seat away read
// instead of walking the whole directory, so it must neither miss a binding
// the page walk would have found nor answer for somebody bound to nothing —
// nor miss an invitation that holds a seat, nor answer one that holds none: a
// redeemed one, an aged-out one, a cancelled one.
//
// Mutation: drop the open predicate from the invitations' half and the aged
// out and redeemed ones are claims; drop the invitations' half and the open
// one is not.
func TestSeatClaimsIsEveryBindingAndEveryOpenInvitation(t *testing.T) {
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
	// MOVED OFF A SEAT, which then holds nobody.
	omar := bindNew(t, rig, "omar.haddad", "ops-lead")
	if _, err := rig.writer.SetIdentity(t.Context(), iamdomain.IdentityEdit{
		PersonID: omar, Seat: seatRef(rig.vacantSeat("ops-desk")),
		OpID: "op-move", Reason: "moved teams"}); err != nil {
		t.Fatalf("move: %v", err)
	}
	rig.drain()
	// A SERVICE ACCOUNT BOUND TO A SEAT is a binding of its kind, and one
	// acting as itself is none.
	release := uuid.Must(uuid.NewV7()).String()
	for _, m := range []iamdomain.Creation{{
		PersonID: release, Kind: iam.KindMachine, Stage: iam.StageActive,
		Login: "svc:release", Seat: rig.vacantSeat("release-desk"),
		OpID: "op-release", Reason: "the pipeline",
	}, {
		PersonID: uuid.Must(uuid.NewV7()).String(), Kind: iam.KindMachine,
		Stage: iam.StageActive, Login: "svc:ci", OpID: "op-ci",
		Reason: "the pipeline",
	}} {
		if err := rig.enrol(m); err != nil {
			t.Fatalf("enrol %s: %v", m.Login, err)
		}
	}
	rig.drain()

	// THE INVITATIONS: one open, one that ages out before the read, one
	// redeemed and one cancelled — and only the first holds anything.
	open := issueOnto(t, rig, "lena@example.com", "support-lead",
		brokerAt.Add(168*time.Hour))
	issueOnto(t, rig, "noor@example.com", "data-lead", brokerAt.Add(time.Hour))
	redeemed := issueOnto(t, rig, "kai@example.com", "infra-lead",
		brokerAt.Add(168*time.Hour))
	if _, err := redeemAs(t, rig, redeemed, "kai@example.com", redeemed.Secret,
		"infra-lead"); err != nil {
		t.Fatalf("redeem: %v", err)
	}
	cancelled := issueOnto(t, rig, "ida@example.com", "qa-lead",
		brokerAt.Add(168*time.Hour))
	if err := rig.during(func() error {
		_, err := rig.writer.CancelInvitation(t.Context(), cancelled.ID,
			operationKey(), "sent to the wrong address")
		return err
	}); err != nil {
		t.Fatalf("cancel: %v", err)
	}
	rig.drain()

	claims, err := reader.SeatClaims(t.Context(), brokerAt.Add(2*time.Hour))
	if err != nil {
		t.Fatalf("SeatClaims: %v", err)
	}
	got := map[string]iamdomain.SeatBinding{}
	for _, b := range claims.Bindings {
		got[b.Person] = b
	}
	if len(got) != 5 {
		t.Fatalf("read %d bindings, want the five principals still bound — "+
			"three people, the redeemer and a service account: %+v",
			len(claims.Bindings), claims.Bindings)
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
			t.Errorf("person %s: the claims read says %+v, the directory page %+v",
				row.ID, b, row.Binding())
		}
	}
	if got[sarah].Stage != iam.StageActive || got[priya].Stage != iam.StageSuspended {
		t.Errorf("stages read back as %q and %q", got[sarah].Stage, got[priya].Stage)
	}
	if got[release].Kind != iam.KindMachine || got[release].Seat != "release-desk" ||
		got[sarah].Kind != iam.KindPerson {
		t.Errorf("kinds read back as %+v and %+v", got[release], got[sarah])
	}
	if _, bound := got[omar]; !bound || got[omar].Seat != "ops-desk" {
		t.Errorf("the moved person reads %+v, want them on ops-desk", got[omar])
	}

	if len(claims.Invitations) != 1 {
		t.Fatalf("read %d open invitations, want only the one still open: %+v",
			len(claims.Invitations), claims.Invitations)
	}
	inv := claims.Invitations[0]
	listed, err := reader.Invitations(t.Context(), iamdomain.InvitationsQuery{
		Now: brokerAt.Add(2 * time.Hour), Limit: iamdomain.MaxPageSize})
	if err != nil {
		t.Fatalf("Invitations: %v", err)
	}
	if len(listed.Invitations) != 1 || listed.Invitations[0].ID != inv.Invitation {
		t.Fatalf("the invitation listing holds %+v, the claims read %+v — the "+
			"two read one predicate", listed.Invitations, claims.Invitations)
	}
	row := listed.Invitations[0]
	if inv.Invitation != open.ID || inv.Seat != "support-lead" ||
		inv.InvitedBy != row.InvitedBy || inv.Sealed != row.Sealed ||
		!inv.ExpiresAt.Equal(row.ExpiresAt) || !inv.CreatedAt.Equal(row.CreatedAt) {
		t.Errorf("the open invitation reads %+v, the listing %+v", inv, row)
	}
	address, err := rig.sealer.OpenInvitation(inv.Invitation, inv.Sealed)
	if err != nil || address != "lena@example.com" {
		t.Errorf("the claim's address opens as %q (%v), want lena's", address, err)
	}
}

// issueOnto issues one invitation onto a seat the rig's company holds, expiring
// at expires, answering the issue.
func issueOnto(t *testing.T, rig *writeRig, address, seat string,
	expires time.Time) iamdomain.InviteIssued {

	t.Helper()
	var issued iamdomain.InviteIssued
	if err := rig.during(func() error {
		var err error
		issued, err = rig.writer.Invite(t.Context(), iamdomain.InviteMint{
			Email: address, Grants: []iam.Grant{iam.GrantStateRead},
			Seat: rig.vacantSeat(seat), ExpiresAt: expires,
			OpID: operationKey(), Reason: "onboarding",
		})
		return err
	}); err != nil {
		t.Fatalf("invite %s onto %s: %v", address, seat, err)
	}
	rig.drain()
	return issued
}

// ONE CLAIM PER SEAT: WHOEVER IS BOUND TO IT, AND WHOEVER IS ON THEIR WAY.
//
// It is the question a company write asks before it removes a human seat, and
// the one the seat listing renders, so a suspended person still holds theirs —
// suspending somebody is not giving their seat away — while a removed one and
// one moved elsewhere do not; and an open invitation holds the seat it names.
//
// TWO RESIDUES ANSWER DIFFERENTLY, and on purpose. Two rows bound to one seat
// are the unknown arm — this node cannot say which of them holds it. Two OPEN
// INVITATIONS on one seat, which a build before invitations held their seats
// could issue, are answered by the newer: refusing would take the listing
// down for a week over a residue nothing this build writes.
//
// Mutation: answer the first of two rows and the duplicate is a holder; refuse
// two invitations and the second residue is the unknown arm.
func TestClaimsBySeatAnswersOneClaimPerSeat(t *testing.T) {
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
	omar := bindNew(t, rig, "omar.haddad", "ops-lead")
	if _, err := rig.writer.Remove(t.Context(), omar, "op-remove",
		"left the company"); err != nil {
		t.Fatalf("remove: %v", err)
	}
	rig.drain()
	lena := bindNew(t, rig, "lena.fischer", "support-lead")
	if _, err := rig.writer.SetIdentity(t.Context(), iamdomain.IdentityEdit{
		PersonID: lena, Seat: seatRef(rig.vacantSeat("support-desk")),
		OpID: "op-move", Reason: "moved teams"}); err != nil {
		t.Fatalf("move: %v", err)
	}
	rig.drain()
	invited := issueOnto(t, rig, "noor@example.com", "data-lead",
		brokerAt.Add(168*time.Hour))

	claimsAt := func() (map[string]iamdomain.SeatClaim, error) {
		t.Helper()
		claims, err := reader.SeatClaims(t.Context(), brokerAt)
		if err != nil {
			t.Fatalf("SeatClaims: %v", err)
		}
		return iamdomain.ClaimsBySeat(claims)
	}
	bySeat, err := claimsAt()
	if err != nil {
		t.Fatalf("ClaimsBySeat: %v", err)
	}
	if len(bySeat) != 4 {
		t.Errorf("ClaimsBySeat answers %d seats, want four — a removed holder "+
			"and a seat its holder moved off claim nothing: %+v", len(bySeat), bySeat)
	}
	for seat, want := range map[string]string{
		"sarah-chen": sarah, "platform-lead": priya, "support-desk": lena,
	} {
		if c := bySeat[seat]; c.Holder == nil || c.Holder.Person != want ||
			c.Invitation != nil {
			t.Errorf("seat %s is claimed by %+v, want person %s alone", seat, c, want)
		}
	}
	if c := bySeat["platform-lead"]; c.Holder == nil ||
		c.Holder.Stage != iam.StageSuspended || c.Holder.Login != "priya.shah" {
		t.Errorf("the suspended holder reads %+v", c.Holder)
	}
	if c := bySeat["data-lead"]; c.Holder != nil || c.Invitation == nil ||
		c.Invitation.Invitation != invited.ID {
		t.Errorf("the invited seat is claimed by %+v, want invitation %s",
			c, invited.ID)
	}

	// TWO OPEN INVITATIONS ON ONE SEAT, as a build before invitations held
	// their seats could leave them: the newer answers, never the unknown arm.
	older := issueOnto(t, rig, "kai@example.com", "infra-lead",
		brokerAt.Add(168*time.Hour))
	plant(t, rig, `UPDATE iam_invites SET seat_id = 'data-lead',
		created_at = (SELECT created_at - 1000 FROM iam_invites WHERE id = ?)
		WHERE id = ?`, invited.ID, older.ID)
	bySeat, err = claimsAt()
	if err != nil {
		t.Fatalf("two open invitations on one seat answered %v, want the newer", err)
	}
	if c := bySeat["data-lead"]; c.Invitation == nil ||
		c.Invitation.Invitation != invited.ID {
		t.Errorf("two open invitations on one seat answered %+v, want the newer, "+
			"%s", c, invited.ID)
	}

	// TWO ROWS ON ONE SEAT, as a retained record's residue leaves them.
	plant(t, rig, `UPDATE iam_people SET seat_id = 'sarah-chen' WHERE id = ?`, priya)
	if held, err := claimsAt(); !errors.Is(err, statelog.ErrUnavailable) {
		t.Errorf("ClaimsBySeat over a seat two rows bind = %+v (%v), want the "+
			"unknown arm", held, err)
	}
}

// plant writes a residue straight into the estate — a state the writer no
// longer produces, which a retained record, a restore or an older build can
// still leave behind.
func plant(t *testing.T, rig *writeRig, query string, args ...any) {
	t.Helper()
	if err := rig.db.Replicated().Tx(t.Context(), func(tx *sql.Tx) error {
		_, err := tx.ExecContext(t.Context(), query, args...)
		return err
	}); err != nil {
		t.Fatalf("plant %q: %v", query, err)
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

	// LENA ARRIVES ON A SEAT OF HER OWN and is moved onto his.
	lena := bindNew(t, rig, "lena.fischer", "lena-desk")
	if _, err := rig.writer.SetIdentity(t.Context(), iamdomain.IdentityEdit{PersonID: lena, Seat: seatRef("ops-lead"), OpID: "op-bind-lena"}); err != nil {
		t.Fatalf("move lena: %v", err)
	}
	rig.drain()
	assertHolders(t, holdersBySeat(t, reader), map[string]iamdomain.SeatHolder{
		"ops-lead": {Seat: "ops-lead", Person: lena, Stage: iam.StageActive},
	})

	// AND IT STAYS ENDED when she moves off it: the seat goes back to the
	// company, not to Omar's tombstone.
	if _, err := rig.writer.SetIdentity(t.Context(), iamdomain.IdentityEdit{PersonID: lena, Seat: seatRef("lena-desk"), OpID: "op-move-lena", Reason: "moved teams"}); err != nil {
		t.Fatalf("move lena back: %v", err)
	}
	rig.drain()
	assertHolders(t, holdersBySeat(t, reader), map[string]iamdomain.SeatHolder{
		"lena-desk": {Seat: "lena-desk", Person: lena, Stage: iam.StageActive},
	})
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
		_, err := rig.writer.SetIdentity(t.Context(), iamdomain.IdentityEdit{PersonID: person, Seat: seatRef(seat), OpID: "op-typo-" + seat, Reason: "a typo"})
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

// A WRITER HANDED NO ORGANISATION REFUSES EVERY SEAT BIND AS UNAVAILABLE —
// here a move from the seat a person holds to another.
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
	person := bindNew(t, rig, "sarah.chen", "sarah-desk")
	rig.seatOnly("sarah-chen")
	deps := rig.nodeDeps
	deps.Seats = nil
	blind, err := iamdomain.NewWriter(deps)
	if err != nil {
		t.Fatalf("build a writer with no organisation: %v", err)
	}
	_, err = blind.SetIdentity(t.Context(), iamdomain.IdentityEdit{PersonID: person, Seat: seatRef("sarah-chen"), OpID: "op-bind-blind"})
	if !errors.Is(err, statelog.ErrUnavailable) {
		t.Errorf("a bind through a writer with no organisation answered %v, "+
			"want %v", err, statelog.ErrUnavailable)
	}
	if _, err := rig.writer.SetIdentity(t.Context(), iamdomain.IdentityEdit{PersonID: person, Seat: seatRef("sarah-chen"), OpID: "op-bind-seen"}); err != nil {
		t.Fatalf("the control: a bind through the rig's writer was refused: %v", err)
	}
	rig.drain()
	if got := rig.column(`SELECT seat_id FROM iam_people WHERE id = ?`,
		person); len(got) != 1 || got[0] != "sarah-chen" {
		t.Errorf("after the control sarah holds %v, want sarah-chen", got)
	}
}
