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

// EVERY PERSON HOLDS A HUMAN SEAT (ADR-0026): the writer's half.
//
// A person with no seat acted under their bare login, in no unit, led by
// nobody and reached by no contact route, so every rule the org chart decides
// had nothing to say about them — and a person on an AGENT's seat was refused
// 403 on every request they sent, a principal the request path could never
// serve. These cases hold the writer to the rule at every write that binds,
// and the applier to the other half: it writes whatever a record says, a
// seatless person a build before the rule enrolled included, because refusing
// a record there stops the log on every node.

// A PERSON ENROLS ONLY ONTO A HUMAN SEAT; A SERVICE ACCOUNT ONTO ONE OR NONE.
//
// A person's create naming no seat is [iamdomain.ErrSeatRequired] — still an
// invalid value to a surface, and one it can name the field of — and one
// naming an agent's seat, or one the company does not hold, is the value the
// caller typed wrong. A service account acts as itself, so it may name none;
// one it names is held to the same human seat a person's is, since a Tier A
// token bound through its row acts as that seat on every request. Every
// refusal publishes nothing.
//
// TWO MORE REFUSALS RIDE ON THE SAME VALIDATION, and are the value the caller
// typed wrong rather than a missing seat: a service account's create handed a
// password link's expiry — it has no password — and a person created at any
// stage but active, whose first password link would set a password nobody can
// sign in with. Each names a seat the company holds, so nothing but its own
// clause stands in its way.
//
// Mutation: drop the person's seat requirement from the create's validation
// and the seatless create lands; check the seat for existence rather than for
// a HUMAN seat and the agent's seat is bound — for a person and a service
// account alike; drop the service account's expiry clause, or the person's
// stage clause, and that row lands.
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
		if tc.want != iamdomain.ErrSeatRequired && errors.Is(err, iamdomain.ErrSeatRequired) {
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

	// THE APPLIER SALVAGES what a build before the rule published: a person
	// enrolled with no seat lands as one, and the directory says so.
	legacy := uuid.Must(uuid.NewV7()).String()
	applyByHand(t, rig, iamdomain.OpEnrol, legacy, iamdomain.PeopleScope(legacy),
		legacyEnrolment(t, rig, legacy, "lee.legacy", "lee@example.com"), 1)
	page, err := rig.reader(t).People(t.Context(),
		iamdomain.PeopleQuery{Limit: iamdomain.MaxPageSize})
	if err != nil {
		t.Fatalf("People: %v", err)
	}
	seatless := map[string]bool{}
	for _, row := range page.People {
		seatless[row.Login] = row.Seatless()
	}
	if want := map[string]bool{"lee.legacy": true, "sarah.chen": false,
		"svc:release": false, "svc:deploy": false}; fmt.Sprint(seatless) !=
		fmt.Sprint(want) {
		t.Errorf("the directory reads seatless as %v, want %v — only a PERSON "+
			"with no seat is, a service account never", seatless, want)
	}
}

// AN OPEN INVITATION HOLDS ITS SEAT, as it holds its address.
//
// The invitation is a person on their way to that seat, so from its issue
// until it is redeemed, cancelled or ages out it is refused to everything else
// that would take the seat — a create, a person moved onto it, a service
// account bound to it, a second invitation — naming the invitation, whose
// remedy is to cancel it. Its own redemption is what binds the seat, and a
// cancellation or an expiry frees it at once. Held only by the people's rows,
// the person the link was sent to would be told at the last step that somebody
// took their seat meanwhile, and a create beside the invitation put two people
// on one seat the day the link was followed.
//
// Mutation: drop the seat's open-invitation check from the enrolment's decide,
// SetIdentity's or the issue's, and its row lands on the invited seat; drop
// the column from the cancellation's delete — keep the row — and the seat is
// held for a link that opens nothing.
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
// A build before invitations held their seats could issue a second invitation
// onto a seat one was already outstanding on, and the applier writes both. Were
// a redemption held to the OPEN INVITATIONS on its seat, each link would be
// refused for the other's sake and neither could ever be redeemed. So a
// redemption is held to the PEOPLE alone: the first one redeemed lands, and the
// second is then refused because a PERSON holds the seat — naming them, never
// the invitation that was just spent.
//
// Mutation: hold the redemption to the open invitations on its seat, leaving
// out the one being redeemed, and the first redemption is refused naming the
// other invitation.
func TestTwoOpenInvitationsOnOneSeatDoNotDeadlockTheirRedemptions(t *testing.T) {
	t.Parallel()
	rig := newWriteRig(t)
	seat := rig.vacantSeat("platform-lead")
	issued := issueOnto(t, rig, "priya@example.com", seat,
		brokerAt.Add(168*time.Hour))
	legacy := iamdomain.InviteIssued{ID: uuid.Must(uuid.NewV7()).String(),
		Secret: "a-legacy-links-secret"}
	applyLegacyInvitation(t, rig, legacy.ID, "noor@example.com", legacy.Secret,
		seat, brokerAt.Add(168*time.Hour), 1)
	claims, err := rig.reader(t).SeatClaims(t.Context(), brokerAt)
	if err != nil {
		t.Fatalf("SeatClaims: %v", err)
	}
	if len(claims.Invitations) != 2 {
		t.Fatalf("the residue holds %d open invitations, want both on %s: %+v",
			len(claims.Invitations), seat, claims.Invitations)
	}

	if _, err := redeemAs(t, rig, legacy, "noor@example.com", legacy.Secret,
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

// A CREATED PERSON GETS A FIRST PASSWORD LINK, in the record that creates them.
//
// An administrator's create makes somebody active who has proved nothing, and
// the one thing they lack is a way to set a password: the record carries a
// `reset` credential whose id and secret are derived from the person under
// the company's key, and it is spent through the ordinary reset path — the
// same [iamdomain.ResetRow.Opens] and the same one-record password set — which
// marks it spent and revokes it with every other link. The estate holds its
// verifier and never its secret. The reset read says it sets their FIRST
// password while they hold none, and stops saying so once they do. A retry of
// the create the ledger answers after the link is spent hands out no link and
// says so ([iamdomain.Created.LinkClosed]), since what opens now is a reset.
//
// THE LINK IS STAMPED IN ITS OWN SNAPSHOT. A credential is ended by whatever
// moves its person's epoch or the company's session generation after its
// issue, so it carries both as its snapshot held them — and this case moves
// the generation BEFORE the create, as the last step of every restore does:
// formed outside the decide, the link carried zero for both and was born
// ended.
//
// Mutation: drop the stamp from the enrolment's decide and the link reads
// ended; answer a collapsed create's derived link without reading the rows
// and the spent link is handed out again.
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
// Two calls under one key race — an administrator's retry sent while the first
// request is still in flight — and both decide from the same snapshot, before
// either record is on the log. The first lands; the second is the broker's
// DUPLICATE of it, collapsed onto the first copy's position on a node whose
// applier has not reached it, so there is no row to read the link back from.
// Answered as a link that no longer opens, the surface would tell an
// administrator to issue a reset for a link that works, about a person this
// node cannot show them yet; so it is unavailable — retry the same key — and
// once the node has applied the record, the same key is answered with the link.
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
	if !errors.Is(err, statelog.ErrUnavailable) || again.Link != nil ||
		again.LinkClosed {
		t.Errorf("the second call, on a node that has not applied the create, "+
			"answered link %+v, closed %v (%v), want unavailable and neither",
			again.Link, again.LinkClosed, err)
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
// decided before the first lands and sent after it: the first call's append is
// held until the second's arrives, forwarded, and only then is the second's.
//
// AND IT ANSWERS THE SECOND IN A FLEET'S ORDER. A clustered stream checks a
// message id against its dedupe window BEFORE the subject's expected last
// sequence (the leader's pre-proposal checks), so an append repeating a stored
// id is acknowledged as that copy's duplicate; a single server checks the
// expectation first and refuses it as stale. The rig's broker is a single
// server, so an id already forwarded goes again without its expectation, and
// the broker's own dedupe answers it — the acknowledgement a fleet's log gives.
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

// AN INVITATION ISSUED BEFORE EVERY INVITATION NAMED A SEAT IS REDEEMABLE BY
// NOBODY.
//
// It creates a person, who would hold no seat — so it is refused as the
// LINK's refusal ([iamdomain.ErrRefused]), whose remedy is a new invitation,
// and never as a value the invitee left out or typed wrong: they left nothing
// out. The reads say the same before anybody chooses a password: the row is
// spent, it is listed as open nowhere, and it holds neither its address nor a
// seat. The applier wrote it as it came — refusing an older build's record
// stops the log on every node.
//
// IT IS THE LINK'S REFUSAL ON EVERY NODE, a node that cannot look a seat up
// included: the clause is asked before the seat is, since there is no seat to
// look up, so a node running no company — or one whose lookup failed — still
// tells the invitee the link creates nobody, rather than a 503 telling them to
// retry a link no node will ever redeem.
//
// Mutation: drop the seat clause from the redemption's validation and the
// redemption on a node that cannot look a seat up answers unavailable; drop it
// from [iamdomain.InvitationRow.Spent] and the link reads as open.
func TestALegacySeatlessInvitationIsNotRedeemable(t *testing.T) {
	t.Parallel()
	rig := newWriteRig(t)
	const address = "lee@example.com"
	id := uuid.Must(uuid.NewV7()).String()
	secret := "a-legacy-links-secret"
	applyLegacyInvitation(t, rig, id, address, secret, "", brokerAt.Add(168*time.Hour), 1)

	row, err := rig.invitationRow(t, id)
	if err != nil {
		t.Fatalf("InvitationByID: %v", err)
	}
	if row.ID != id || row.Seat != "" || !row.Admits(secret) {
		t.Fatalf("the legacy invitation reads %+v", row)
	}
	if !row.Spent(brokerAt) {
		t.Error("an invitation naming no seat reads as redeemable")
	}
	page, err := rig.reader(t).Invitations(t.Context(),
		iamdomain.InvitationsQuery{Now: brokerAt})
	if err != nil || len(page.Invitations) != 0 {
		t.Errorf("the open listing holds %+v (%v), want nothing", page.Invitations, err)
	}

	issued := iamdomain.InviteIssued{ID: id, Secret: secret}
	rig.seatOnly("platform-lead")
	for _, seat := range []string{"", "platform-lead"} {
		_, err := redeemAs(t, rig, issued, address, secret, seat)
		if !errors.Is(err, iamdomain.ErrRefused) || errors.Is(err, iamdomain.ErrInvalid) {
			t.Errorf("redeeming it naming seat %q answered %v, want %v and only "+
				"that", seat, err, iamdomain.ErrRefused)
		}
	}
	for _, lookup := range []struct {
		name string
		err  error
	}{
		{"running no company", fmt.Errorf("engine: %w", session.ErrNoCompany)},
		{"whose lookup failed", errors.New("the organisation could not be read")},
	} {
		deps := rig.nodeDeps
		deps.Seats = failingSeats{err: lookup.err}
		writer, err := iamdomain.NewWriter(deps)
		if err != nil {
			t.Fatal(err)
		}
		_, err = writer.Redeem(t.Context(), iamdomain.Redemption{
			PersonID: uuid.Must(uuid.NewV7()).String(), Stage: iam.StageActive,
			Email: address, Login: iam.LoginFromAddress(address),
			Password: aPassword(), Grants: []iam.Grant{iam.GrantStateRead},
			Invitation: id, InvitationSecret: secret,
			OpID: "invite:" + id, Reason: "redeemed an invitation",
		})
		if !errors.Is(err, iamdomain.ErrRefused) ||
			errors.Is(err, statelog.ErrUnavailable) ||
			errors.Is(err, iamdomain.ErrNoCompany) {
			t.Errorf("redeeming it on a node %s answered %v, want %v and only "+
				"that — no node will ever redeem it, so a retry is no remedy",
				lookup.name, err, iamdomain.ErrRefused)
		}
	}
	if rows := rig.column(`SELECT id FROM iam_people`); len(rows) != 0 {
		t.Errorf("a refused redemption left %v", rows)
	}

	// IT HOLDS NOTHING: a new invitation to its address, onto a seat, lands.
	if _, err := inviteFor(t, rig, address, "platform-lead"); err != nil {
		t.Errorf("an invitation to a legacy invitation's address was refused: %v",
			err)
	}
}

// AN INVITATION WITH NO EXPIRY OPENS NOTHING, in every reader.
//
// No writer forms one — the issue refuses it — but the applier writes what a
// record says, and three readers had disagreed about the row that carries
// one: the redemption and the issue's retry read it as open for the life of
// the company, while the hold and the listing read it as closed. Read as open,
// it was the one link in the company nothing could retire. It is aged out
// everywhere now.
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
	applyLegacyInvitation(t, rig, id, address, secret, seat, time.Time{}, 1)

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

// THE APPLIER RE-DERIVES AN INVITATION'S SEAT on the rows an older build applied.
//
// The seat column arrived after the table (replicated migration 0032), so on a
// node upgraded in place every row an older build applied holds the column's
// default — while a node replaying the log writes each row's seat from its
// record. The derivation bump runs [iamdomain.Applier.Rederive] once, from the
// same decode the apply writes the column from, so both nodes hold the same
// rows: an invitation's seat where its document names one, and nothing where
// it names none. Only rows that differ are written, so a second run — the
// rows this build has been maintaining — writes nothing.
//
// Mutation: leave the column alone in Rederive and the planted rows keep
// their default; derive every row's seat as empty and the issued ones lose
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
	legacy := uuid.Must(uuid.NewV7()).String()
	applyLegacyInvitation(t, rig, legacy, "lee@example.com", "a-secret", "",
		brokerAt.Add(168*time.Hour), 1)
	maintained := seatsOf(t, rig)

	// AS AN OLDER BUILD LEFT THEM: the default, and one stale value.
	plant(t, rig, `UPDATE iam_invites SET seat_id = ''`)
	plant(t, rig, `UPDATE iam_invites SET seat_id = 'stale' WHERE id = ?`, legacy)
	rederive := func() int {
		t.Helper()
		var rows int
		if err := rig.db.Replicated().Tx(t.Context(), func(tx *sql.Tx) error {
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
		legacy: ""}; fmt.Sprint(derived) != fmt.Sprint(want) ||
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
// as unknown.
//
// A fresh install that has not created its company holds no human seat at
// all, and no wait changes that — a company declaring one does. Answered as
// [statelog.ErrUnavailable], a surface told the operator to retry, and the
// retry met the same answer on every node for ever; so it is
// [iamdomain.ErrNoCompany], which a surface answers with the remedy. A lookup
// that FAILED is the unknown arm still. And a REDEMPTION on such a node is
// unavailable rather than either: its invitation was issued on a node that ran
// the company, so another node can redeem it, and "create a company first" is
// advice to an administrator, not to the invitee holding the link.
//
// AND THE SEAT IS ASKED BEFORE THE BLINDER, which mints the company's key on
// the first address a fresh node ever blinds: a create or an invitation the
// seat refuses must have minted nothing. Asked after it, the first invitation
// a fresh node was asked for answered whatever the mint's guard said — a 500,
// on a node whose rows had not applied the session that asked.
//
// Mutation: wrap the seam's no-company answer as unavailable in the seat check
// and the first three rows answer it; return it from the redemption and the
// invitee is told to create a company; ask the blinder before the seat in a
// create or an invitation and the blinder is asked.
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
			Grants:   []iam.Grant{iam.GrantStateRead}, Seat: "platform-lead",
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

// --- records a writer of this build no longer forms ------------------------- //

// applyByHand applies one directory record straight through the rig's applier,
// at sequence 1<<30+n — past everything the log holds, as a peer's record a
// build before this one published would arrive. It is how a residue this
// build's writer refuses to produce is put in front of the applier and the
// readers, which must still take it.
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
	if err := rig.db.Replicated().Tx(t.Context(), func(tx *sql.Tx) error {
		_, err := rig.applier.Apply(t.Context(), tx, record,
			statelog.ApplyOptions{Now: brokerAt, StoredAt: brokerAt})
		return err
	}); err != nil {
		t.Fatalf("apply the %s record: %v", op, err)
	}
}

// legacyEnrolment is a person's enrolment as a build before every person held
// a seat formed it: an address, a login, and no seat.
func legacyEnrolment(t *testing.T, rig *writeRig, id, login, address string) []byte {
	t.Helper()
	name, err := rig.sealer.Seal(id, iamdomain.FieldName, "A colleague")
	if err != nil {
		t.Fatal(err)
	}
	email, err := rig.sealer.Seal(id, iamdomain.FieldEmail, address)
	if err != nil {
		t.Fatal(err)
	}
	mutation, err := iamdomain.EncodeEnrolled(iamdomain.Enrolled{
		V: iamdomain.DocumentVersion,
		Person: iamdomain.Person{
			V: iamdomain.DocumentVersion, Kind: iam.KindPerson,
			Stage: iam.StageActive, NameSealed: name, EmailSealed: email,
		},
		Holds: iamdomain.Identifiers{EmailBlind: blindOf(t, address), Login: login},
	})
	if err != nil {
		t.Fatal(err)
	}
	return mutation
}

// applyLegacyInvitation applies an invitation as a record a writer of this
// build no longer forms carries it — naming seat, possibly none, and expiring
// at expires, possibly never — at sequence 1<<30+n.
func applyLegacyInvitation(t *testing.T, rig *writeRig, id, address, secret,
	seat string, expires time.Time, n uint64) {

	t.Helper()
	sealed, err := rig.sealer.SealInvitation(id, address)
	if err != nil {
		t.Fatal(err)
	}
	blind := blindOf(t, address)
	mutation, err := iamdomain.EncodeInvitation(iamdomain.Invitation{
		V: iamdomain.DocumentVersion, ID: id, EmailBlind: blind, Sealed: sealed,
		InvitedBy: "ana.admin", Grants: []iam.Grant{iam.GrantStateRead},
		Verifier: iamdomain.InvitationVerifier(secret), Seat: seat,
		ExpiresAt: expires,
	})
	if err != nil {
		t.Fatal(err)
	}
	applyByHand(t, rig, iamdomain.OpInvite, "",
		iamdomain.BucketScope(iamdomain.BucketOf(blind)), mutation, n)
}
