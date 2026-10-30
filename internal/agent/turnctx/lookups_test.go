package turnctx_test

import (
	"fmt"
	"sync"
	"testing"

	"github.com/crewlet/crewlet/internal/agent/turnctx"
	"github.com/crewlet/crewlet/internal/events/types"
)

// Two consumers' key types, spelled alike: an entry is keyed by its type as
// well as its value, as a context's is.
type (
	repoName  string
	otherName string
)

// A TURN'S LOOKUPS ARE THE WHOLE TURN'S: what one phase looked up is what every
// Turn derived from it reads — a phase session's, a delegated worker's and the
// bridge's for a coding CLI — and what a derived Turn looked up is its parent's
// too, so a repository a turn names is resolved once however many of its
// parts read it. A key is its consumer's own: another type spelling the same
// value names nothing.
//
// Mutation: InPhase, WithCalls or WithAuxSpend dropping the set (a fresh one,
// or nil) → the derived turn misses, red.
func TestATurnsLookupsAreSharedByEveryTurnDerivedFromIt(t *testing.T) {
	t.Parallel()
	turn := &turnctx.Turn{RunID: "run-1", Lookups: turnctx.NewLookups()}
	turn.LookedUp().Store(repoName("site"), "repo-1")

	for name, derived := range map[string]*turnctx.Turn{
		"a phase":         turn.InPhase(types.PhaseExecute),
		"a worker":        turn.WithCalls(turnctx.NewCallLog()),
		"the bridge's":    turn.WithAuxSpend(nil),
		"a worker's step": turn.WithCalls(turnctx.NewCallLog()).InPhase(types.PhaseExecute),
	} {
		got, ok := derived.LookedUp().Load(repoName("site"))
		if !ok || got != "repo-1" {
			t.Errorf("%s reads the turn's lookup as %v (%v), want repo-1", name, got, ok)
		}
	}

	worker := turn.WithCalls(turnctx.NewCallLog())
	worker.LookedUp().Store(repoName("docs"), "repo-2")
	if got, ok := turn.LookedUp().Load(repoName("docs")); !ok || got != "repo-2" {
		t.Errorf("the parent reads its worker's lookup as %v (%v), want repo-2", got, ok)
	}
	if got, ok := turn.LookedUp().Load(otherName("site")); ok {
		t.Errorf("another consumer's key spelling the same name read %v", got)
	}
}

// FORGOTTEN IS LOOKED UP AGAIN, and a set nobody built holds nothing: a turn
// built without one, and the operator surface, which has no turn at all, look
// every name up on every call — never an answer another call stored.
func TestAForgottenLookupAndANilSetHoldNothing(t *testing.T) {
	t.Parallel()
	l := turnctx.NewLookups()
	l.Store(repoName("site"), "repo-1")
	l.Forget(repoName("site"))
	if got, ok := l.Load(repoName("site")); ok {
		t.Errorf("a forgotten lookup still reads %v", got)
	}

	var none *turnctx.Lookups
	none.Store(repoName("site"), "repo-1")
	none.Forget(repoName("site"))
	if got, ok := none.Load(repoName("site")); ok {
		t.Errorf("a nil set read %v", got)
	}
	var noTurn *turnctx.Turn
	if noTurn.LookedUp() != nil || (&turnctx.Turn{}).LookedUp() != nil {
		t.Error("a missing turn, or one built without lookups, holds a set")
	}
}

// THE FIRST TO STORE IS THE ONE EVERY CALLER GETS: callers racing to be first
// for one key each answer the same value — the one stored — and a later one
// finds it held, so a value changed in place under its own lock loses no
// caller's change to another's store; a nil set stores nothing and answers the
// value it was handed. Run under -race.
//
// Mutation: store over whatever the set held → the later caller is answered
// its own value, red.
func TestTheFirstStoreIsTheOneEveryCallerGets(t *testing.T) {
	t.Parallel()
	l := turnctx.NewLookups()
	const callers = 8
	got := make([]any, callers)
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := range callers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			got[i], _ = l.LoadOrStore(repoName("site"), new(int))
		}()
	}
	close(start)
	wg.Wait()
	kept, held := l.Load(repoName("site"))
	if !held {
		t.Fatal("nothing was stored")
	}
	for i, v := range got {
		if v != kept {
			t.Errorf("caller %d answers %p, want the one value kept, %p", i, v, kept)
		}
	}
	if v, loaded := l.LoadOrStore(repoName("site"), new(int)); !loaded || v != kept {
		t.Errorf("a later caller answers %p (held %v), want the value kept, %p", v, loaded, kept)
	}
	var none *turnctx.Lookups
	if v, loaded := none.LoadOrStore(repoName("site"), "repo-1"); loaded || v != "repo-1" {
		t.Errorf("a nil set answers %v (held %v), want the value handed it", v, loaded)
	}
}

// ONE SET, MANY CALLERS AT ONCE: the executor's tools and its workers' run
// concurrently against the turn's one set. Run under -race.
func TestALookupSetTakesConcurrentCallers(t *testing.T) {
	t.Parallel()
	l := turnctx.NewLookups()
	var wg sync.WaitGroup
	for i := range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			key := repoName(fmt.Sprintf("repo-%d", i))
			for range 100 {
				l.Store(key, i)
				if got, ok := l.Load(key); !ok || got != i {
					t.Errorf("%q read %v (%v) after storing %d", key, got, ok, i)
					return
				}
				l.Forget(key)
			}
		}()
	}
	wg.Wait()
}
