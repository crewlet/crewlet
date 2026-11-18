package iamdomain_test

import (
	"context"
	"errors"
	"slices"
	"sync"
	"testing"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/events"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/iamdomain"
)

// writerEvents keeps what a writer announced, in order.
type writerEvents struct {
	mu   sync.Mutex
	seen []events.Payload
}

func (e *writerEvents) Emit(_ context.Context, payload events.Payload) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.seen = append(e.seen, payload)
}

// take returns what was announced since the last take.
func (e *writerEvents) take() []events.Payload {
	e.mu.Lock()
	defer e.mu.Unlock()
	out := e.seen
	e.seen = nil
	return out
}

// grantRows narrows what was announced to the grant changes.
func grantRows(t *testing.T, seen []events.Payload) []types.IAMGrantsChanged {
	t.Helper()
	var out []types.IAMGrantsChanged
	for _, payload := range seen {
		if row, ok := payload.(types.IAMGrantsChanged); ok {
			out = append(out, row)
		}
	}
	return out
}

// A GRANT CHANGE IS ANNOUNCED WITH WHAT IT ADDED AND WHAT IT TOOK AWAY, AS THE
// LANDED RECORD DECIDED THEM, AND NOTHING ELSE ABOUT A PERSON IS.
//
// The writer, not the route, announces it: the before is read inside the
// record's snapshot, and a route's own read on a contended row could report a
// grant never held. A write that moved no grant announces nothing, or renames
// drown the audit feed; a refused write announces nothing.
//
// Mutations: drop the delta check (an empty row), announce before the refusal
// (the widening shows), take the before outside the decide (a wrong delta).
func TestAGrantChangeIsAnnouncedWithWhatItAddedAndRemoved(t *testing.T) {
	t.Parallel()
	rig := newWriteRig(t)
	const person = "018f3a9c-0000-7000-8000-0000000000e1"
	if err := rig.enrol(iamdomain.Creation{
		PersonID: person, Kind: iam.KindPerson, Stage: iam.StageActive,
		Name: "Dana Okafor", Email: "dana@example.com", Login: "dana.sre",
		Seat:   rig.vacantSeat("dana-okafor"),
		Grants: []iam.Grant{iam.GrantWorklet},
		OpID:   "op-enrol", Reason: "a hire", LinkExpiresAt: firstLinkExpiry,
	}); err != nil {
		t.Fatalf("enrol: %v", err)
	}
	rig.drain()

	// AN ENROLMENT THAT CONFERS ANYTHING is a change from nothing.
	rows := grantRows(t, rig.events.take())
	if len(rows) != 1 {
		t.Fatalf("the enrolment announced %d grant changes, want 1", len(rows))
	}
	if got := rows[0]; got.Person != person || got.By != "ana.admin" ||
		!slices.Equal(got.Added, []string{"worklet"}) ||
		len(got.Removed) != 0 || got.Version == 0 {
		t.Errorf("the enrolment announced %+v", got)
	}

	update := func(w *iamdomain.Writer, op string,
		edit func(iamdomain.Person) iamdomain.Person) error {
		_, err := w.UpdatePerson(rig.t.Context(), iamdomain.PersonUpdate{
			PersonID: person,
			Apply: func(p iamdomain.Person) (iamdomain.Person, error) {
				return edit(p), nil
			},
			OpID: op, Reason: "a change of authority",
		})
		rig.drain()
		return err
	}

	// A WIDENING AND A NARROWING IN ONE WRITE, by an administrator.
	admin := rig.writer.As(principalNamed("ana.admin", iam.KindPerson,
		[]iam.Grant{iam.GrantAdmin}))
	if err := update(admin, "op-swap", func(p iamdomain.Person) iamdomain.Person {
		p.Grants = []iam.Grant{iam.GrantDoclet}
		return p
	}); err != nil {
		t.Fatalf("swap: %v", err)
	}
	rows = grantRows(t, rig.events.take())
	if len(rows) != 1 {
		t.Fatalf("the swap announced %d grant changes, want 1", len(rows))
	}
	if got := rows[0]; !slices.Equal(got.Added, []string{"doclet"}) ||
		!slices.Equal(got.Removed, []string{"worklet"}) || got.By != "ana.admin" {
		t.Errorf("the swap announced %+v, want doclet added and "+
			"worklet removed", got)
	}

	// A WRITE THAT MOVED NO PERMISSION announces nothing: admin granted,
	// then the apps it already includes spelled out beside it.
	if err := update(admin, "op-promote", func(p iamdomain.Person) iamdomain.Person {
		p.Grants = []iam.Grant{iam.GrantAdmin}
		return p
	}); err != nil {
		t.Fatalf("promote: %v", err)
	}
	rig.events.take()
	if err := update(admin, "op-spell", func(p iamdomain.Person) iamdomain.Person {
		p.Grants = []iam.Grant{iam.GrantAdmin, iam.GrantDoclet, iam.GrantWorklet}
		return p
	}); err != nil {
		t.Fatalf("spell out: %v", err)
	}
	if seen := rig.events.take(); len(seen) != 0 {
		t.Errorf("a write that moved no permission announced %#v", seen)
	}

	// A WRITE REFUSED announces nothing.
	if err := update(admin, "op-widen", func(p iamdomain.Person) iamdomain.Person {
		p.Grants = append(p.Grants, "swarm")
		return p
	}); !errors.Is(err, iamdomain.ErrRefused) {
		t.Fatalf("a name this build does not know was conferred (%v)", err)
	}
	if seen := rig.events.take(); len(seen) != 0 {
		t.Errorf("a refused write announced %#v", seen)
	}
}

// ENDING EVERY SESSION IN THE COMPANY IS ANNOUNCED WITH THE GENERATION IT MOVED
// TO: the one the decide incremented in the record's snapshot, which the
// applier writes and every bearer is checked against. Mutation: announce the
// generation read, not written, and the first row says 0.
func TestInvalidatingEverySessionIsAnnouncedWithItsGeneration(t *testing.T) {
	t.Parallel()
	rig := newWriteRig(t)
	for i, op := range []string{"op-invalidate-1", "op-invalidate-2"} {
		if _, err := rig.writer.InvalidateAll(rig.t.Context(), op,
			"restored from a backup"); err != nil {
			t.Fatalf("invalidate %d: %v", i+1, err)
		}
		rig.drain()
		seen := rig.events.take()
		if len(seen) != 1 {
			t.Fatalf("invalidation %d announced %d events, want 1", i+1, len(seen))
		}
		row, ok := seen[0].(types.IAMSessionGenerationBumped)
		if !ok || row.Generation != uint64(i+1) || row.By != "ana.admin" ||
			row.Reason != "restored from a backup" {
			t.Errorf("invalidation %d announced %#v, want generation %d",
				i+1, seen[0], i+1)
		}
	}
}

// A GESTURE IS ANNOUNCED WITH THE CREDENTIAL IT CAME THROUGH, AND ONE A TOKEN
// MAY NOT MAKE ANNOUNCES NOTHING.
//
// By names the person; OperatorID names which credential, and is the party's:
// As derives it from the principal's own, so a party derived next never
// carries somebody else's. A token is refused the gesture at the record
// (ADR-0033), and a refused record announces nothing, or the trail shows a
// write nobody can find. Mutation: announce the actor alone, or carry the
// operator through As, and a row is wrong; drop the record-layer lock and the
// token's row appears.
func TestAGestureIsAnnouncedWithTheCredentialItCameThrough(t *testing.T) {
	t.Parallel()
	rig := newWriteRig(t)
	const via = "session:0192f00d-0000-7000-8000-00000000000a"
	owner := principalNamed("ana.admin", iam.KindPerson,
		[]iam.Grant{iam.GrantAdmin})
	owner.Via = via
	party := rig.writer.As(owner)
	if party.OperatorID != via {
		t.Fatalf("a party acting through a session carries operator %q, want %q",
			party.OperatorID, via)
	}
	if _, err := party.InvalidateAll(rig.t.Context(), "op-through-session",
		"restored from a backup"); err != nil {
		t.Fatalf("invalidate: %v", err)
	}
	rig.drain()
	seen := rig.events.take()
	if len(seen) != 1 {
		t.Fatalf("announced %d events, want 1", len(seen))
	}
	if row, _ := seen[0].(types.IAMSessionGenerationBumped); row.By != "ana.admin" ||
		row.OperatorID != via {
		t.Errorf("announced as by %q through %q, want the person through %q",
			row.By, row.OperatorID, via)
	}

	const token = "pat:0192f00d-0000-7000-8000-00000000000b"
	owner.Via = token
	if through := rig.writer.As(owner); through.OperatorID != token {
		t.Fatalf("a party acting through a token carries operator %q, want %q",
			through.OperatorID, token)
	} else if _, err := through.InvalidateAll(rig.t.Context(), "op-through-token",
		"restored from a backup"); !errors.Is(err, iamdomain.ErrTokenRefused) {
		t.Errorf("ending every session through a token answered %v, want "+
			"ErrTokenRefused", err)
	}
	rig.drain()
	if seen := rig.events.take(); len(seen) != 0 {
		t.Errorf("a gesture refused through a token announced %#v", seen)
	}
	// THE NEXT PARTY'S CREDENTIAL IS ITS OWN: dana acts as herself, so her
	// operator is her login, never the previous party's credential.
	if next := party.As(principalNamed("dana.sre", iam.KindPerson, nil)); next.OperatorID != "dana.sre" {
		t.Errorf("a party derived from one acting through a session carries "+
			"operator %q, want its own login", next.OperatorID)
	}
}

// A REDEMPTION'S GRANTS ARE ANNOUNCED AS ITS ISSUER'S DECISION.
//
// The record is the redeemer's ([iamdomain.Writer.For]; the trail check is
// the control), but what it confers the issuer decided: announced under the
// party's actor, it names the new person granting themselves, on the row an
// audit most needs. Mutation: announce under the party's own actor and By is
// the redeemer; carry its credential and the row says the issuer acted
// through the redeemer's login.
func TestARedemptionsGrantsAreAnnouncedAsItsIssuers(t *testing.T) {
	t.Parallel()
	rig := newWriteRig(t)
	seat := rig.vacantSeat("platform-lead")
	issued, err := inviteFor(t, rig, "sam@example.com", seat)
	if err != nil {
		t.Fatalf("issue an invitation: %v", err)
	}
	rig.events.take()

	person := uuid.Must(uuid.NewV7())
	redeemer := nodeWriter(rig).For(iam.Principal{ID: person,
		Kind: iam.KindPerson, Login: "sam.joiner"})
	op := "invite:" + issued.ID
	if err := rig.draining(func() error {
		_, err := redeemer.Redeem(t.Context(), iamdomain.Redemption{
			PersonID: person.String(), Stage: iam.StageActive,
			Name: "Sam Joiner", Email: "sam@example.com", Login: "sam.joiner",
			Password: aPassword(), Grants: []iam.Grant{iam.GrantWorklet},
			Seat: seat, Invitation: issued.ID, InvitationSecret: issued.Secret,
			OpID: op, Reason: "redeemed an invitation",
		})
		return err
	}); err != nil {
		t.Fatalf("redeem: %v", err)
	}
	rig.drain()

	rows := grantRows(t, rig.events.take())
	if len(rows) != 1 {
		t.Fatalf("the redemption announced %d grant changes, want 1", len(rows))
	}
	if got := rows[0]; got.Person != person.String() || got.By != "ana.admin" ||
		got.OperatorID != "" || !slices.Equal(got.Added, []string{"worklet"}) {
		t.Errorf("the redemption announced %+v, want worklet added by the "+
			"invitation's issuer, ana.admin, through no credential", got)
	}
	if got := rig.column(`SELECT actor FROM iam_history
		WHERE person_id = ? AND op = ?`, person.String(),
		string(iamdomain.OpEnrol)); !slices.Equal(got, []string{"sam.joiner"}) {
		t.Errorf("the redemption's trail names %q as its author, want the "+
			"person who redeemed it", got)
	}
}
