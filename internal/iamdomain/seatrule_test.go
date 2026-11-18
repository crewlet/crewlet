package iamdomain_test

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/iam/credential"
	"github.com/crewlet/crewlet/internal/iam/session"
	"github.com/crewlet/crewlet/internal/iamdomain"
	"github.com/crewlet/crewlet/internal/statelog"
)

// EVERY PERSON HOLDS A HUMAN SEAT (ADR-0032): the writer's half (package doc,
// "Every person holds a HUMAN seat"); a person on an agent's seat is refused on
// every request. The writer holds every binding write to the rule, never the
// applier, which writes whatever a record says: refusing a record there stops
// the log on every node.

// A PERSON ENROLS ONLY ONTO A HUMAN SEAT; A SERVICE ACCOUNT ONTO ONE OR NONE.
//
// A person naming no seat is [iamdomain.ErrSeatRequired], whose field a
// surface can name; an agent's seat or one the company lacks is
// [iamdomain.ErrInvalid]. A service account may name none, but one it names
// must be human: a Tier A token bound through its row acts as that seat. Also
// ErrInvalid: a service account handed a password link's expiry, and a person
// created other than active, whose first link would set an unusable password.
// Every refusal publishes nothing.
//
// Mutation: drop the person's seat requirement, check a seat for existence
// rather than HUMAN, or drop the expiry or stage clause, and that row lands.
func TestAPersonEnrolsOnlyOntoAHumanSeat(t *testing.T) {
	t.Parallel()
	rig := newWriteRig(t)
	rig.agentSeat("release-bot")
	person := func(seat string) iamdomain.Creation {
		return iamdomain.Creation{
			PersonID: uuid.Must(uuid.NewV7()).String(), Kind: iam.KindPerson,
			Stage: iam.StageActive, Name: "Sarah Chen",
			Email: "sarah.chen@example.com", Login: "sarah.chen", Seat: seat,
			LinkExpiresAt: firstLinkExpiry, OpID: operationKey(), Reason: "a hire",
		}
	}
	machine := func(seat string) iamdomain.Creation {
		return iamdomain.Creation{
			PersonID: uuid.Must(uuid.NewV7()).String(), Kind: iam.KindMachine,
			Stage: iam.StageActive, Name: "Release pipeline", Login: "svc:release",
			Seat: seat, OpID: operationKey(), Reason: "the pipeline",
		}
	}
	at := func(stage iam.Stage) iamdomain.Creation {
		in := person(rig.vacantSeat("night-desk"))
		in.Stage = stage
		return in
	}
	linked := machine("")
	linked.Login, linked.LinkExpiresAt = "svc:linked", firstLinkExpiry
	before, err := rig.end(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name string
		in   iamdomain.Creation
		want error
	}{
		{"a person on no seat", person(""), iamdomain.ErrSeatRequired},
		{"a person on an agent's seat", person("release-bot"), iamdomain.ErrInvalid},
		{"a person on a seat nobody declared", person("no-such-seat"), iamdomain.ErrInvalid},
		{"a service account on an agent's seat", machine("release-bot"), iamdomain.ErrInvalid},
		{"a service account handed a password link's expiry", linked, iamdomain.ErrInvalid},
		{"a person created suspended", at(iam.StageSuspended), iamdomain.ErrInvalid},
		{"a person created invited", at(iam.StageInvited), iamdomain.ErrInvalid},
	} {
		err := rig.enrol(tc.in)
		if !errors.Is(err, tc.want) {
			t.Errorf("%s answered %v, want %v", tc.name, err, tc.want)
		}
		// THE SEAT THE CALLER TYPED WRONG IS NOT THE ONE THEY LEFT OUT:
		// a surface names a different remedy for each.
		if !errors.Is(tc.want, iamdomain.ErrSeatRequired) && errors.Is(err, iamdomain.ErrSeatRequired) {
			t.Errorf("%s answered %v, which says no seat was named", tc.name, err)
		}
	}
	if after, _ := rig.end(t.Context()); after != before {
		t.Errorf("the refused creates published %d records", after-before)
	}
	if rows := rig.column(`SELECT id FROM iam_people`); len(rows) != 0 {
		t.Errorf("the refused creates left %v", rows)
	}

	// THE CONTROLS: a service account on no seat and on a human one, and a
	// person on a human one, holding it.
	if err := rig.enrol(machine("")); err != nil {
		t.Errorf("a service account on no seat was refused: %v", err)
	}
	unbound := machine(rig.vacantSeat("release-desk"))
	unbound.Login = "svc:deploy"
	if err := rig.enrol(unbound); err != nil {
		t.Errorf("a service account on a human seat was refused: %v", err)
	}
	seated := person(rig.vacantSeat("sarah-chen"))
	if err := rig.enrol(seated); err != nil {
		t.Fatalf("a person on a human seat was refused: %v", err)
	}
	if got := rig.column(`SELECT seat_id FROM iam_people WHERE id = ?`,
		seated.PersonID); len(got) != 1 || got[0] != "sarah-chen" {
		t.Errorf("the person holds %v, want sarah-chen", got)
	}
}

// AN OPEN INVITATION HOLDS ITS SEAT, as it holds its address.
//
// Until redeemed, cancelled or aged out, it refuses everything else that would
// take the seat (a create, a move, a service account's binding, a second
// invitation) naming the invitation, whose remedy is to cancel it. Its own
// redemption binds the seat; a cancellation or expiry frees it at once. Held
// only by people's rows, the invitee would learn at the last step that
// somebody took their seat, and a create beside it would seat two people.
//
// Mutation: drop the open-invitation check from the enrolment's, SetIdentity's
// or the issue's decide and its row lands on the invited seat; drop the column
// from the cancellation's delete and the seat stays held for a dead link.
func TestAnOpenInvitationHoldsItsSeat(t *testing.T) {
	t.Parallel()
	rig := newWriteRig(t)
	seat := rig.vacantSeat("platform-lead")
	invited := issueOnto(t, rig, "priya@example.com", seat,
		brokerAt.Add(168*time.Hour))
	colleague := bindNew(t, rig, "sarah.chen", "sarah-desk")
	machine := uuid.Must(uuid.NewV7()).String()
	if err := rig.enrol(iamdomain.Creation{
		PersonID: machine, Kind: iam.KindMachine, Stage: iam.StageActive,
		Login: "svc:release", OpID: operationKey(), Reason: "the pipeline",
	}); err != nil {
		t.Fatalf("enrol the service account: %v", err)
	}
	move := func(who, op string) error {
		return rig.during(func() error {
			_, err := rig.writer.SetIdentity(t.Context(), iamdomain.IdentityEdit{
				PersonID: who, Seat: seatRef(seat), OpID: op, Reason: "moved"})
			return err
		})
	}
	heldByIt := func(what string, err error) {
		t.Helper()
		var taken *iamdomain.ErrTaken
		if !errors.As(err, &taken) || taken.Field != iamdomain.UniqueSeat ||
			taken.Value != seat || taken.Invitation != invited.ID ||
			taken.Person != "" {
			t.Errorf("%s answered %v, want the seat refused naming invitation %s",
				what, err, invited.ID)
		}
	}
	heldByIt("a create onto the invited seat", rig.enrol(iamdomain.Creation{
		PersonID: uuid.Must(uuid.NewV7()).String(), Kind: iam.KindPerson,
		Stage: iam.StageActive, Name: "Dana", Email: "dana@example.com",
		Login: "dana.sre", Seat: seat, LinkExpiresAt: firstLinkExpiry,
		OpID: operationKey(), Reason: "a hire",
	}))
	heldByIt("a person moved onto it", move(colleague, "op-move"))
	heldByIt("a service account bound to it", move(machine, "op-bind"))
	_, err := inviteFor(t, rig, "dana@example.com", seat)
	heldByIt("a second invitation onto it", err)

	// ITS OWN REDEMPTION binds it — and the seat is then the person's.
	if _, err := redeemAs(t, rig, invited, "priya@example.com", invited.Secret,
		seat); err != nil {
		t.Fatalf("the invitation's own redemption was refused: %v", err)
	}
	rig.drain()
	var taken *iamdomain.ErrTaken
	if err := move(colleague, "op-move-after"); !errors.As(err, &taken) ||
		taken.Person == "" || taken.Invitation != "" {
		t.Errorf("a move onto the seat its redeemer holds answered %v, want it "+
			"refused naming the person", err)
	}

	// CANCELLED, and the seat is free at once.
	cancelled := issueOnto(t, rig, "noor@example.com", "data-lead",
		brokerAt.Add(168*time.Hour))
	taken = nil
	if err := rig.bind("data-lead", colleague, "op-before-cancel"); !errors.As(err,
		&taken) || taken.Invitation != cancelled.ID {
		t.Fatalf("a move onto data-lead before the cancellation answered %v, "+
			"want it held by invitation %s — the control", err, cancelled.ID)
	}
	if _, err := cancel(t, rig, cancelled.ID); err != nil {
		t.Fatalf("cancel: %v", err)
	}
	rig.drain()
	if err := rig.bind("data-lead", colleague, "op-after-cancel"); err != nil {
		t.Errorf("a move onto the seat a cancelled invitation held was refused: %v",
			err)
	}

	// AGED OUT, and so is this one — at a clock past its deadline.
	lapsed := issueOnto(t, rig, "kai@example.com", "ops-lead", brokerAt.Add(time.Hour))
	later := rig.writer.As(principalNamed("ana.admin", iam.KindPerson, iam.AllGrants))
	at := brokerAt.Add(2 * time.Hour)
	later.Now = func() time.Time { return at }
	if err := rig.during(func() error {
		_, err := later.SetIdentity(t.Context(), iamdomain.IdentityEdit{
			PersonID: colleague, Seat: seatRef("ops-lead"),
			OpID: "op-after-lapse", Reason: "moved"})
		return err
	}); err != nil {
		t.Errorf("a move onto the seat of invitation %s, which aged out, was "+
			"refused: %v", lapsed.ID, err)
	}
}

// TWO OPEN INVITATIONS ON ONE SEAT DO NOT DEADLOCK THEIR REDEMPTIONS.
//
// The writer never issues two onto one seat, but a node that retained the
// record cancelling one, or a restore, can hold two, and the applier writes
// what a record says. Held to the seat's OPEN INVITATIONS, each link would be
// refused for the other's sake; so a redemption is held to the PEOPLE alone:
// the first lands, and the second is refused naming the PERSON now on the
// seat, never the invitation just spent.
//
// Mutation: hold the redemption to the seat's other open invitations and the
// first is refused naming the other invitation.
func TestTwoOpenInvitationsOnOneSeatDoNotDeadlockTheirRedemptions(t *testing.T) {
	t.Parallel()
	rig := newWriteRig(t)
	seat := rig.vacantSeat("platform-lead")
	issued := issueOnto(t, rig, "priya@example.com", seat,
		brokerAt.Add(168*time.Hour))
	other := iamdomain.InviteIssued{ID: uuid.Must(uuid.NewV7()).String(),
		Secret: "the-other-links-secret"}
	applyInvitationByHand(t, rig, other.ID, "noor@example.com", other.Secret,
		seat, brokerAt.Add(168*time.Hour), 1)
	claims, err := rig.reader(t).SeatClaims(t.Context(), brokerAt)
	if err != nil {
		t.Fatalf("SeatClaims: %v", err)
	}
	if len(claims.Invitations) != 2 {
		t.Fatalf("the residue holds %d open invitations, want both on %s: %+v",
			len(claims.Invitations), seat, claims.Invitations)
	}

	if _, err := redeemAs(t, rig, other, "noor@example.com", other.Secret,
		seat); err != nil {
		t.Fatalf("the first of two invitations on one seat was refused: %v — "+
			"held to the other one, neither link could ever be redeemed", err)
	}
	rig.drain()
	noor := rig.column(`SELECT id FROM iam_people WHERE seat_id = ?`, seat)
	if len(noor) != 1 {
		t.Fatalf("the seat is held by %v after the redemption, want its redeemer",
			noor)
	}

	_, err = redeemAs(t, rig, issued, "priya@example.com", issued.Secret, seat)
	var taken *iamdomain.ErrTaken
	if !errors.As(err, &taken) || taken.Field != iamdomain.UniqueSeat ||
		taken.Person != noor[0] || taken.Invitation != "" {
		t.Errorf("the second redemption answered %v, want the seat refused "+
			"naming its holder %s", err, noor[0])
	}
	if rows := rig.column(`SELECT id FROM iam_people`); len(rows) != 1 {
		t.Errorf("the directory holds %v, want the first redeemer alone", rows)
	}
}

// A CREATED PERSON GETS A FIRST PASSWORD LINK, in the record that creates them:
// a `reset` credential derived from the person under the company's key, spent
// through the ordinary reset path ([iamdomain.ResetRow.Opens]). The estate
// holds its verifier, never its secret; it reads as FIRST only while they hold
// no password; a retry after it is spent hands out none and says so
// ([iamdomain.Created.LinkClosed]).
//
// THE LINK IS STAMPED IN ITS OWN SNAPSHOT with the epoch and generation, as a
// later move of either ends it; the case moves the generation BEFORE the
// create, as a restore does. Mutation: drop the stamp from the enrolment's
// decide and the link is born ended; answer a collapsed create's link without
// reading the rows and the spent link is handed out again.
func TestACreatedPersonGetsAFirstPasswordLink(t *testing.T) {
	t.Parallel()
	rig := newWriteRig(t)
	if err := rig.during(func() error {
		_, err := rig.writer.InvalidateAll(t.Context(), operationKey(),
			"restored from a backup")
		return err
	}); err != nil {
		t.Fatalf("invalidate: %v", err)
	}
	key := operationKey()
	person, err := iamdomain.CreatedPersonID(key)
	if err != nil {
		t.Fatal(err)
	}
	create := iamdomain.Creation{
		PersonID: person, Kind: iam.KindPerson, Stage: iam.StageActive,
		Name: "Omar Ops", Email: "omar@example.com", Login: "omar.ops",
		Seat: rig.vacantSeat("ops-lead"), LinkExpiresAt: firstLinkExpiry,
		OpID: key, Reason: "created by ana.admin",
	}
	created, err := rig.create(create)
	if err != nil || created.Outcome != statelog.OutcomeApplied {
		t.Fatalf("the create answered %+v (%v), want applied", created.Result, err)
	}
	link := created.Link
	if link == nil {
		t.Fatal("the create of a person answered no first password link")
	}
	blinder, err := iamdomain.NewBlinder(testBlindKey)
	if err != nil {
		t.Fatal(err)
	}
	wantID, _ := blinder.PasswordLinkID(person)
	wantSecret, _ := blinder.PasswordLinkSecret(wantID)
	if link.Credential != wantID || link.Secret != wantSecret ||
		!link.ExpiresAt.Equal(firstLinkExpiry) {
		t.Errorf("the create answered link %+v, want the one derived from "+
			"person %s, expiring at %s", link, person, firstLinkExpiry)
	}
	rig.drain()

	// THE VERIFIER, AND NEVER THE SECRET.
	if got := rig.column(`SELECT CAST(verifier AS TEXT) FROM iam_credentials
		WHERE id = ? AND method = ?`, link.Credential,
		string(iamdomain.MethodReset)); len(got) != 1 ||
		got[0] != credential.ResetVerifier(link.Credential, link.Secret) {
		t.Errorf("the link's stored verifier is %v", got)
	}
	for _, table := range []string{"iam_credentials", "iam_people", "iam_history"} {
		for _, row := range rig.column(`SELECT CAST(document AS TEXT) FROM ` +
			table + ` WHERE document IS NOT NULL`) {
			if strings.Contains(row, link.Secret) {
				t.Errorf("%s holds the link's secret in the clear", table)
			}
		}
	}

	row, err := rig.resetRow(t, link.Credential)
	if err != nil {
		t.Fatalf("ResetByID: %v", err)
	}
	if row.Ended || !row.Opens(link.Secret, brokerAt) {
		t.Fatalf("the first password link does not open (ended %v): %+v — a "+
			"link formed outside its record's snapshot carries no counters, "+
			"and the invalidation before it ended it at birth", row.Ended, row)
	}
	if !row.First || row.Kind != iam.KindPerson || row.Stage != iam.StageActive ||
		row.Seat != "ops-lead" || row.PersonID != person {
		t.Errorf("the link reads %+v, want person %s's first, on ops-lead", row,
			person)
	}

	// SPENT THROUGH THE ORDINARY RESET PATH.
	if err := rig.draining(func() error {
		_, err := nodeWriter(rig).SetPassword(t.Context(), iamdomain.PasswordSet{
			PersonID: person, Verifier: "argon-first", Spends: link.Credential,
			Check: func(p iamdomain.Person, c iamdomain.Counters) error {
				if !iamdomain.ResetOf(p, link.Credential, c).Opens(link.Secret,
					brokerAt) {
					return iamdomain.ErrRefused
				}
				return nil
			},
			OpID: operationKey(), Reason: "set their first password",
		})
		return err
	}); err != nil {
		t.Fatalf("spending the first password link: %v", err)
	}
	rig.drain()
	if row, err := rig.resetRow(t, link.Credential); err != nil ||
		row.Opens(link.Secret, brokerAt) {
		t.Errorf("the spent link still opens (%+v, %v)", row, err)
	}

	// THE CREATE'S RETRY, ANSWERED FROM THE LEDGER: the person, and no link.
	again, err := rig.create(create)
	if err != nil || !again.Collapsed || again.Link != nil || !again.LinkClosed {
		t.Errorf("the create's retry after the link was spent answered link %+v, "+
			"closed %v, collapsed %v (%v) — want no link, and said so",
			again.Link, again.LinkClosed, again.Collapsed, err)
	}

	// A LATER RESET SETS NO FIRST PASSWORD: they hold one now.
	reset, _ := issueReset(t, rig, person)
	if row, err := rig.resetRow(t, reset); err != nil || row.First {
		t.Errorf("a reset issued to somebody holding a password reads first "+
			"(%+v, %v)", row, err)
	}
}

// A RETRIED CREATE THIS NODE HAS NOT APPLIED CANNOT SAY WHETHER ITS LINK
// OPENS, and says so.
//
// Two calls under one key race and decide from one snapshot; the second is
// the broker's duplicate of the first, collapsed onto a position this node's
// applier has not reached, so no row holds the link. Answered as closed, the
// surface would send an administrator to reset a working link; so it is
// unavailable (retry the same key), and once applied the key answers the link.
//
// Mutation: drop the not-applied arm from the read-back and the second call
// answers [iamdomain.Created.LinkClosed].
func TestARetriedCreateThisNodeHasNotAppliedIsUnavailable(t *testing.T) {
	t.Parallel()
	racing := &raceOnOneKey{arrived: make(chan struct{}),
		second: make(chan struct{}), landed: make(chan struct{})}
	rig := newWriteRigWith(t, func(log statelog.Appender) statelog.Appender {
		racing.Appender = log
		return racing
	})
	key := operationKey()
	person, err := iamdomain.CreatedPersonID(key)
	if err != nil {
		t.Fatal(err)
	}
	create := iamdomain.Creation{
		PersonID: person, Kind: iam.KindPerson, Stage: iam.StageActive,
		Name: "Omar Ops", Email: "omar@example.com", Login: "omar.ops",
		Seat: rig.vacantSeat("ops-lead"), LinkExpiresAt: firstLinkExpiry,
		OpID: key, Reason: "created by ana.admin",
	}
	// NO CONSUMER RUNS, so each call waits out its budget for the one
	// record: the first answers pending with the link it carried.
	var (
		first    iamdomain.Created
		firstErr error
		done     = make(chan struct{})
	)
	go func() {
		defer close(done)
		first, firstErr = rig.writer.Create(t.Context(), create)
	}()
	<-racing.arrived
	again, err := rig.writer.Create(t.Context(), create)
	// A SECOND CALL THAT NEVER APPENDED must not leave the first held for
	// ever: the case then fails on what the second answered.
	racing.release()
	<-done
	if firstErr != nil || first.Outcome != statelog.OutcomePending ||
		first.Link == nil {
		t.Fatalf("the first call answered %+v, link %+v (%v), want pending "+
			"with its link", first.Result, first.Link, firstErr)
	}
	if !again.Collapsed {
		t.Fatalf("the second call answered %+v (%v), want it collapsed onto "+
			"the first call's record — the case is about that answer",
			again.Result, err)
	}
	// A WRITE THAT LANDED, NOT A CONDITION: the answer carries what it is,
	// so no surface tells an administrator the create did not happen.
	var unread *iamdomain.NotReadBack
	if !errors.As(err, &unread) || unread.Invitation ||
		unread.Result.Outcome == statelog.OutcomeUnknown ||
		!errors.Is(err, statelog.ErrUnavailable) || again.Link != nil ||
		again.LinkClosed {
		t.Errorf("the second call, on a node that has not applied the create, "+
			"answered link %+v, closed %v (%v), want a person's create not read "+
			"back, unavailable, and neither", again.Link, again.LinkClosed, err)
	}

	rig.drain()
	applied, err := rig.create(create)
	if err != nil || !applied.Collapsed || applied.Link == nil ||
		applied.Link.Credential != first.Link.Credential ||
		applied.Link.Secret != first.Link.Secret {
		t.Errorf("the same key once the node applied the create answered link "+
			"%+v (collapsed %v, %v), want the first call's %+v",
			applied.Link, applied.Collapsed, err, first.Link)
	}
}

// raceOnOneKey orders two appends under one operation id so the second is
// decided before the first lands and sent after it: the first append is held
// until the second's arrives, forwarded, and only then is the second's.
//
// It answers the second as a fleet does: a clustered stream checks the dedupe
// window BEFORE the expected last sequence, so a repeated id is acknowledged
// as a duplicate, where the rig's single server would refuse it as stale. So a
// repeated id goes without its expectation, and the broker's dedupe answers it.
type raceOnOneKey struct {
	statelog.Appender

	arrived, second, landed chan struct{}
	releaseOnce             sync.Once

	mu   sync.Mutex
	sent map[string]bool
	n    int
}

// release lets the first append through, once, whether or not a second ever
// arrived.
func (r *raceOnOneKey) release() { r.releaseOnce.Do(func() { close(r.second) }) }

func (r *raceOnOneKey) Append(ctx context.Context, subject, msgID string,
	expect *uint64, body []byte) (uint64, bool, error) {

	r.mu.Lock()
	r.n++
	call := r.n
	r.mu.Unlock()
	switch call {
	case 1:
		close(r.arrived)
		<-r.second
		defer close(r.landed)
	case 2:
		r.release()
		<-r.landed
	}
	r.mu.Lock()
	if r.sent == nil {
		r.sent = map[string]bool{}
	}
	repeat := r.sent[msgID]
	r.sent[msgID] = true
	r.mu.Unlock()
	if repeat {
		expect = nil
	}
	return r.Appender.Append(ctx, subject, msgID, expect, body)
}

// A REDEMPTION NAMING NO SEAT IS ITS CALLER'S FAULT, never the link's.
//
// The redeeming surface hands on the seat it read off the invitation, so none
// is a call formed wrong: [iamdomain.ErrInvalid] before anything is read or
// published, never [iamdomain.ErrRefused] (telling the invitee their link is
// dead) nor [iamdomain.ErrSeatRequired] (asking for a field their form lacks). The CONTROL names
// the invitation's seat and lands.
//
// Mutation: drop the seat clause from the redemption's validation and the
// record's own comparison refuses it as the link's, ErrRefused.
func TestARedemptionNamingNoSeatIsItsCallersFault(t *testing.T) {
	t.Parallel()
	rig := newWriteRig(t)
	const address = "lee@example.com"
	seat := rig.vacantSeat("platform-lead")
	issued := issueOnto(t, rig, address, seat, brokerAt.Add(168*time.Hour))
	before, _ := rig.end(t.Context())
	_, err := redeemAs(t, rig, issued, address, issued.Secret, "")
	if !errors.Is(err, iamdomain.ErrInvalid) || errors.Is(err, iamdomain.ErrRefused) ||
		errors.Is(err, iamdomain.ErrSeatRequired) {
		t.Errorf("a redemption naming no seat answered %v, want %v and only that",
			err, iamdomain.ErrInvalid)
	}
	if after, _ := rig.end(t.Context()); after != before {
		t.Errorf("the refused redemption published %d records", after-before)
	}
	if _, err := redeemAs(t, rig, issued, address, issued.Secret, seat); err != nil {
		t.Errorf("the same redemption naming its seat was refused: %v", err)
	}
}

// AN INVITATION WITH NO EXPIRY OPENS NOTHING, in every reader.
//
// The issue refuses one, but the applier writes what a record says; read as
// open, it would be the one link nothing could retire.
//
// Mutation: read a zero expiry as `never` in the redemption's check and the
// link creates a person; in [iamdomain.InvitationRow.Spent] and it reads as
// open; in the issue's retry and the key answers it as its own.
func TestAnInvitationWithNoExpiryOpensNothing(t *testing.T) {
	t.Parallel()
	rig := newWriteRig(t)
	const address = "lee@example.com"
	seat := rig.vacantSeat("platform-lead")
	key := operationKey()
	blinder, err := iamdomain.NewBlinder(testBlindKey)
	if err != nil {
		t.Fatal(err)
	}
	id, err := blinder.InvitationID(key)
	if err != nil {
		t.Fatal(err)
	}
	secret, err := blinder.InvitationSecret(id)
	if err != nil {
		t.Fatal(err)
	}
	applyInvitationByHand(t, rig, id, address, secret, seat, time.Time{}, 1)

	row, err := rig.invitationRow(t, id)
	if err != nil || row.ID != id {
		t.Fatalf("InvitationByID: %+v (%v)", row, err)
	}
	if !row.Spent(brokerAt) {
		t.Error("an invitation with no expiry reads as redeemable")
	}
	if _, err := redeemAs(t, rig, iamdomain.InviteIssued{ID: id, Secret: secret},
		address, secret, seat); !errors.Is(err, iamdomain.ErrRefused) {
		t.Errorf("redeeming an invitation with no expiry answered %v, want %v",
			err, iamdomain.ErrRefused)
	}
	if rows := rig.column(`SELECT id FROM iam_people`); len(rows) != 0 {
		t.Errorf("the redemption left %v", rows)
	}
	// THE ISSUE'S RETRY under the key that derived it is not answered with it.
	if err := rig.during(func() error {
		_, err := rig.writer.Invite(t.Context(), iamdomain.InviteMint{
			Email: address, Seat: seat, ExpiresAt: brokerAt.Add(168 * time.Hour),
			OpID: key, Reason: "onboarding",
		})
		return err
	}); !errors.Is(err, iamdomain.ErrOperationReused) {
		t.Errorf("the issue's retry answered %v, want %v — the invitation it "+
			"would answer opens nothing", err, iamdomain.ErrOperationReused)
	}
	// AND IT HOLDS NOTHING: the address and the seat take a new invitation.
	if _, err := inviteFor(t, rig, address, seat); err != nil {
		t.Errorf("an invitation beside one with no expiry was refused: %v", err)
	}
}

// THE APPLIER RE-DERIVES AN INVITATION'S SEAT whenever the checkpoint's rule
// set differs from this build's: `seat_id` is a DERIVED column
// ([statelog.Deriver]), and [iamdomain.Applier.Rederive] recomputes it from the
// row's document, by the decode the apply writes it from, over rows derived
// under other rules (a reanchor's checkpoint at derivation 0, rows adopted from
// another build), so every node converges on what a replay writes. It writes
// only rows that differ, so a second run writes nothing.
//
// Mutation: leave the column alone in Rederive and the planted rows keep what
// another rule set left; derive every seat as empty and the issued ones lose
// theirs.
func TestTheApplierRederivesAnInvitationsSeat(t *testing.T) {
	t.Parallel()
	rig := newWriteRig(t)
	if rig.applier.DerivationVersion() < 1 {
		t.Fatalf("the identity applier derives at %d, so no bump runs its "+
			"re-derivation", rig.applier.DerivationVersion())
	}
	first := issueOnto(t, rig, "priya@example.com", "platform-lead",
		brokerAt.Add(168*time.Hour))
	second := issueOnto(t, rig, "noor@example.com", "data-lead",
		brokerAt.Add(168*time.Hour))
	third := issueOnto(t, rig, "lee@example.com", "design-lead",
		brokerAt.Add(168*time.Hour))
	maintained := seatsOf(t, rig)

	// AS ROWS DERIVED UNDER ANOTHER RULE SET MIGHT HOLD THEM: the column's
	// default, and one value the rule here would never derive.
	plant(t, rig, `UPDATE iam_invites SET seat_id = ''`)
	plant(t, rig, `UPDATE iam_invites SET seat_id = 'stale' WHERE id = ?`, third.ID)
	rederive := func() int {
		t.Helper()
		var rows int
		if err := rig.db.Tx(t.Context(), func(tx *sql.Tx) error {
			var err error
			rows, err = rig.applier.Rederive(t.Context(), tx, statelog.ApplyOptions{
				Now: brokerAt, StoredAt: brokerAt})
			return err
		}); err != nil {
			t.Fatalf("Rederive: %v", err)
		}
		return rows
	}
	if rows := rederive(); rows != 3 {
		t.Errorf("the re-derivation wrote %d rows, want the three that differ", rows)
	}
	derived := seatsOf(t, rig)
	if want := map[string]string{first.ID: "platform-lead", second.ID: "data-lead",
		third.ID: "design-lead"}; fmt.Sprint(derived) != fmt.Sprint(want) ||
		fmt.Sprint(derived) != fmt.Sprint(maintained) {
		t.Errorf("the re-derived seats are %v, the applied ones %v, want %v",
			derived, maintained, want)
	}
	if rows := rederive(); rows != 0 {
		t.Errorf("a second re-derivation wrote %d rows over rows it derived", rows)
	}
}

// seatsOf is every invitation's seat column, by id.
func seatsOf(t *testing.T, rig *writeRig) map[string]string {
	t.Helper()
	out := map[string]string{}
	for _, row := range rig.column(`SELECT id || ' ' || seat_id FROM iam_invites`) {
		id, seat, _ := strings.Cut(row, " ")
		out[id] = seat
	}
	return out
}

// A NODE RUNNING NO COMPANY REFUSES EVERY BINDING BY NAME, and a failed lookup
// as unknown: a fresh install holds no human seat until a company declares
// one, so it is [iamdomain.ErrNoCompany], answered with the remedy, not
// [statelog.ErrUnavailable], which no retry clears. A REDEMPTION there is
// unavailable: its invitation came from a node running the company, and
// "create a company" is no advice for the invitee. The seat is asked BEFORE
// the blinder, which mints the company's key on a fresh node's first blind, so
// a write the seat refuses mints nothing.
//
// Mutation: wrap no-company as unavailable in the seat check, return it from
// the redemption, or ask the blinder before the seat, and its row fails.
func TestANodeRunningNoCompanyRefusesEveryBindingByName(t *testing.T) {
	t.Parallel()
	rig := newWriteRig(t)
	person := bindNew(t, rig, "sarah.chen", "sarah-desk")
	issued := issueOnto(t, rig, "priya@example.com", "platform-lead",
		brokerAt.Add(168*time.Hour))
	for _, lookup := range []struct {
		name    string
		err     error
		want    error
		notWant error
	}{
		{"running no company", fmt.Errorf("engine: %w", session.ErrNoCompany),
			iamdomain.ErrNoCompany, statelog.ErrUnavailable},
		{"whose lookup failed", errors.New("the organisation could not be read"),
			statelog.ErrUnavailable, iamdomain.ErrNoCompany},
	} {
		deps := rig.nodeDeps
		deps.Seats = failingSeats{err: lookup.err}
		blinds := &countingBlinds{inner: deps.Blinds}
		deps.Blinds = blinds
		writer, err := iamdomain.NewWriter(deps)
		if err != nil {
			t.Fatal(err)
		}
		for what, write := range map[string]func() error{
			"a create": func() error {
				_, err := writer.Create(t.Context(), iamdomain.Creation{
					PersonID: uuid.Must(uuid.NewV7()).String(), Kind: iam.KindPerson,
					Stage: iam.StageActive, Name: "Dana", Email: "dana@example.com",
					Login: "dana.sre", Seat: "founder", LinkExpiresAt: firstLinkExpiry,
					OpID: operationKey(), Reason: "a hire",
				})
				return err
			},
			"an invitation": func() error {
				_, err := writer.Invite(t.Context(), iamdomain.InviteMint{
					Email: "dana@example.com", Seat: "founder",
					ExpiresAt: brokerAt.Add(time.Hour), OpID: operationKey(),
				})
				return err
			},
			"a move": func() error {
				_, err := writer.SetIdentity(t.Context(), iamdomain.IdentityEdit{
					PersonID: person, Seat: seatRef("founder"), OpID: operationKey(),
				})
				return err
			},
		} {
			if err := write(); !errors.Is(err, lookup.want) || errors.Is(err, lookup.notWant) {
				t.Errorf("%s on a node %s answered %v, want %v and not %v", what,
					lookup.name, err, lookup.want, lookup.notWant)
			}
		}
		if asked := blinds.asked.Load(); asked != 0 {
			t.Errorf("on a node %s the blinder was asked %d times by writes the "+
				"seat refused — each could have minted the company's key for "+
				"nothing", lookup.name, asked)
		}
		_, err = writer.Redeem(t.Context(), iamdomain.Redemption{
			PersonID: uuid.Must(uuid.NewV7()).String(), Stage: iam.StageActive,
			Email: "priya@example.com", Login: iam.LoginFromAddress("priya@example.com"),
			Password: aPassword(),
			Grants:   []iam.Grant{iam.GrantWorklet}, Seat: "platform-lead",
			Invitation: issued.ID, InvitationSecret: issued.Secret,
			OpID: "invite:" + issued.ID, Reason: "redeemed an invitation",
		})
		if !errors.Is(err, statelog.ErrUnavailable) || errors.Is(err, iamdomain.ErrNoCompany) {
			t.Errorf("a redemption on a node %s answered %v, want only %v",
				lookup.name, err, statelog.ErrUnavailable)
		}
	}
}

// countingBlinds is a blinder source that counts how often it was asked.
type countingBlinds struct {
	inner iamdomain.Blinds
	asked atomic.Int32
}

func (c *countingBlinds) Blinder(ctx context.Context) (*iamdomain.Blinder, error) {
	c.asked.Add(1)
	return c.inner.Blinder(ctx)
}

// failingSeats is a running company that cannot answer: every lookup is err.
type failingSeats struct{ err error }

func (f failingSeats) Seat(context.Context, string) (session.Seat, bool, error) {
	return session.Seat{}, false, f.err
}

// --- records this build's writer refuses to form ---------------------------- //

// applyByHand applies one directory record straight through the rig's applier,
// at sequence 1<<30+n, past everything the log holds: residue this build's
// writer refuses to form (a retained record, a restore, a newer peer's record),
// which the applier and readers must still take, since refusing a record stops
// the log on every node.
func applyByHand(t *testing.T, rig *writeRig, op iamdomain.OpKind, person string,
	scope iamdomain.ScopeSet, mutation []byte, n uint64) {

	t.Helper()
	opID := operationKey()
	payload, err := iamdomain.Encode(iamdomain.MutationRecord{
		RecordEnvelope: iamdomain.RecordEnvelope{
			V: iamdomain.BaseRecordVersion, OpID: opID,
			Subject: iamdomain.DirectorySubject(), Op: op, Writer: "node-b",
			Gen: 1, CreatedAt: brokerAt, Scope: scope,
		},
		Person: person, Actor: "ana.admin", ActorKind: iam.KindPerson,
		Mutation: mutation,
	})
	if err != nil {
		t.Fatalf("encode the %s record: %v", op, err)
	}
	record := statelog.Record{
		Envelope: statelog.Envelope{
			V: iamdomain.BaseRecordVersion, Kind: string(iamdomain.KindDirectory),
			Subject: statelog.Subject{Kind: string(iamdomain.KindDirectory)},
			Op:      string(op), OpID: opID,
		},
		Position: statelog.Position{Stream: iamdomain.Domain{}.Stream().Name,
			Seq: 1<<30 + n},
		Payload: payload, StoredAt: brokerAt,
	}
	if err := rig.db.Tx(t.Context(), func(tx *sql.Tx) error {
		_, err := rig.applier.Apply(t.Context(), tx, record,
			statelog.ApplyOptions{Now: brokerAt, StoredAt: brokerAt})
		return err
	}); err != nil {
		t.Fatalf("apply the %s record: %v", op, err)
	}
}

// applyInvitationByHand applies an invitation the writer's decides would
// refuse — a second one onto a seat an open invitation holds, or one expiring
// never — naming seat and expiring at expires, at sequence 1<<30+n.
func applyInvitationByHand(t *testing.T, rig *writeRig, id, address, secret,
	seat string, expires time.Time, n uint64) {

	t.Helper()
	sealed, err := rig.sealer.SealInvitation(id, address)
	if err != nil {
		t.Fatal(err)
	}
	blind := blindOf(t, address)
	mutation, err := iamdomain.EncodeInvitation(iamdomain.Invitation{
		V: iamdomain.DocumentVersion, ID: id, EmailBlind: blind, Sealed: sealed,
		InvitedBy: "ana.admin", Grants: []iam.Grant{iam.GrantWorklet},
		Verifier: iamdomain.InvitationVerifier(secret), Seat: seat,
		ExpiresAt: expires,
	})
	if err != nil {
		t.Fatal(err)
	}
	applyByHand(t, rig, iamdomain.OpInvite, "",
		iamdomain.BucketScope(iamdomain.BucketOf(blind)), mutation, n)
}
