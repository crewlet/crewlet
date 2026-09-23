package iamdomain_test

import (
	"context"
	"errors"
	"slices"
	"sync"
	"testing"

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
// The before is read INSIDE the snapshot the record is formed in, which is why
// the writer and not the route announces it: a route reading the row itself
// would describe a different transaction, and on a contended row the
// difference is a grant the feed says somebody gained that they never held.
// A write that moved no grant — a colleague level here — announces nothing,
// or the audit question "who can do what to this deployment" drowns in
// renames. And a write REFUSED announces nothing: the row it would describe
// never existed.
//
// Mutations: drop the delta check and the colleague write announces an empty
// row; announce before the refusal and the widening shows up; take the before
// from outside the decide and the delta is wrong.
func TestAGrantChangeIsAnnouncedWithWhatItAddedAndRemoved(t *testing.T) {
	t.Parallel()
	rig := newWriteRig(t)
	const person = "018f3a9c-0000-7000-8000-0000000000e1"
	if err := rig.enrol(iamdomain.Enrolment{
		PersonID: person, Kind: iam.KindPerson, Stage: iam.StageActive,
		Name: "Dana Okafor", Email: "dana@example.com", Login: "dana.sre",
		Grants: []iam.Grant{iam.GrantWorkWrite, iam.GrantSecretRead},
		OpID:   "op-enrol", Reason: "a hire",
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
		!slices.Equal(got.Added, []string{"secrets:read", "work:write"}) ||
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

	// A WIDENING AND A NARROWING IN ONE WRITE, by a party holding both.
	broad := rig.writer.As(principalNamed("ana.admin", iam.KindPerson, []iam.Grant{
		iamdomain.AdminGrant, iam.GrantSecretRead, iam.GrantSecretWrite,
		iam.GrantWorkWrite,
	}))
	if err := update(broad, "op-swap", func(p iamdomain.Person) iamdomain.Person {
		p.Grants = []iam.Grant{iam.GrantSecretWrite, iam.GrantSecretRead}
		return p
	}); err != nil {
		t.Fatalf("swap: %v", err)
	}
	rows = grantRows(t, rig.events.take())
	if len(rows) != 1 {
		t.Fatalf("the swap announced %d grant changes, want 1", len(rows))
	}
	if got := rows[0]; !slices.Equal(got.Added, []string{"secrets:write"}) ||
		!slices.Equal(got.Removed, []string{"work:write"}) || got.By != "ana.admin" {
		t.Errorf("the swap announced %+v, want secrets:write added and "+
			"work:write removed", got)
	}

	// A WRITE THAT MOVED NO GRANT announces nothing.
	if err := update(broad, "op-colleague", func(p iamdomain.Person) iamdomain.Person {
		p.Colleague = iam.ColleagueWrite
		return p
	}); err != nil {
		t.Fatalf("colleague: %v", err)
	}
	if seen := rig.events.take(); len(seen) != 0 {
		t.Errorf("a write that moved no grant announced %#v", seen)
	}

	// A WRITE REFUSED announces nothing.
	narrow := rig.writer.As(principalNamed("ana.admin", iam.KindPerson,
		[]iam.Grant{iamdomain.AdminGrant}))
	if err := update(narrow, "op-widen", func(p iamdomain.Person) iamdomain.Person {
		p.Grants = append(p.Grants, iam.GrantWorkWrite)
		return p
	}); !errors.Is(err, iamdomain.ErrRefused) {
		t.Fatalf("a party without work:write conferred it (%v)", err)
	}
	if seen := rig.events.take(); len(seen) != 0 {
		t.Errorf("a refused write announced %#v", seen)
	}
}

// ENDING EVERY SESSION IN THE COMPANY IS ANNOUNCED WITH THE GENERATION IT MOVED
// TO.
//
// The number is the decide's: it is read and incremented in the snapshot the
// record is formed in, so it is the one the applier writes and the one every
// bearer is checked against. A second invalidation names the next.
//
// Mutation: announce the generation read rather than the one written and the
// first row says 0.
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

// A GESTURE MADE THROUGH A TOKEN IS ANNOUNCED AS ONE.
//
// A machine token acts as its owner, so the name on an announced event is the
// owner's; the party's OperatorID is what says a token made it, and it is the
// PARTY's — derived from the principal's own credential by As, and replaced by
// the next derivation, so the next party derived from this writer never
// announces somebody else's token. Mutation: announce the actor alone, or
// carry the operator through As, and one of the two rows is wrong.
func TestAGestureMadeThroughATokenIsAnnouncedAsOne(t *testing.T) {
	t.Parallel()
	rig := newWriteRig(t)
	const via = "pat:0192f00d-0000-7000-8000-00000000000a"
	owner := principalNamed("ana.admin", iam.KindPerson, iam.AllGrants)
	owner.Via = via
	party := rig.writer.As(owner)
	if party.OperatorID != via {
		t.Fatalf("a party acting through a token carries operator %q, want %q",
			party.OperatorID, via)
	}
	if _, err := party.InvalidateAll(rig.t.Context(), "op-through-token",
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
		t.Errorf("announced as by %q through %q, want the owner through %q",
			row.By, row.OperatorID, via)
	}
	// THE NEXT PARTY'S CREDENTIAL IS ITS OWN: dana acts as herself, so
	// her operator is her login, and never the token the party before her
	// acted through.
	if next := party.As(principalNamed("dana.sre", iam.KindPerson, nil)); next.OperatorID != "dana.sre" {
		t.Errorf("a party derived from one acting through a token carries "+
			"operator %q, want its own login", next.OperatorID)
	}
}
