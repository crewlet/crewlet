package turnctx

import "sync"

// Lookups is what a turn has looked up by name once, so a later call in the
// same turn reuses the answer instead of paying for the lookup again: the
// code tools' repository, resolved at linearizable the first time the turn
// names it, and read by its id on every call after (internal/agent/builtin's
// CodeDeps.repo). One linearizable read is one barrier on its log, and a turn
// that reads ten files of one repository would otherwise pay ten for the same
// name.
//
// # It decides nothing about access
//
// An entry is a fact the turn already established — this name named that
// repository — and never a permission: the node that answers each later read
// judges the asker again from its own chart. So a stale entry can make a read
// ask about an object that moved, which the reader refuses in its own words,
// and never one the asker may not read.
//
// # Keyed like a context
//
// A consumer keys its entries with a comparable type of its own, as
// context.WithValue's keys are, so two packages' entries never collide and
// neither can read the other's: this package holds the values without knowing
// their types, because it may not import the domains whose answers they are.
//
// # What it holds, and for how long
//
// The turn's own lookups, in memory, for the turn's life: one Turn and every
// Turn derived from it ([Turn.InPhase], [Turn.WithCalls], [Turn.WithAuxSpend])
// share one, so a delegated worker reuses what its parent looked up and its
// parent what it did. A resumed segment starts empty and looks up again, since
// what a name named may have moved while it was parked. It is bounded by the
// calls the turn makes: an entry is added only by a call that looked something
// up.
//
// Beside what a name named, a consumer may keep a FACT its lookups established
// that a later call builds on — the code tools keep how far the refs a turn
// resolved on a repository had moved, which the turn's writes to it are
// floored at — taken with [Lookups.LoadOrStore], so callers racing to be first
// share one value, which the consumer then changes in place under a lock of
// its own.
//
// A consumer that finds an entry no longer true — the name now names another
// object, or the object it named is gone — FORGETS it, whichever of its reads
// finds that out, so the next call looks up again; it never replaces it with
// what another read said in passing, because an entry is what the ONE lookup
// seam answered, and whatever that seam does beside its read must then be done
// again for the object the name now names.
//
// NIL HOLDS NOTHING: every method is safe on a nil Lookups, which answers
// every Load as a miss — a turn built without one, a tool exercised directly,
// and the operator surface, which has no turn and looks up once a call.
type Lookups struct {
	mu   sync.Mutex
	held map[any]any
}

// NewLookups is an empty set, for a turn that has looked nothing up yet.
func NewLookups() *Lookups { return &Lookups{} }

// Load is what key was looked up as in this turn, and false where it was not.
func (l *Lookups) Load(key any) (any, bool) {
	if l == nil {
		return nil, false
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	v, ok := l.held[key]
	return v, ok
}

// Store records what key was looked up as, over whatever it held.
func (l *Lookups) Store(key, value any) {
	if l == nil {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.held == nil {
		l.held = map[any]any{}
	}
	l.held[key] = value
}

// LoadOrStore is what key was looked up as in this turn, storing value first
// where it held nothing, and whether it held something already — under the
// set's one lock, so two callers racing to be first (a turn's tools and its
// workers run at once) both answer the ONE value kept, never each its own: an
// entry a consumer goes on to change in place under a lock of its own (a
// position it only raises) loses no caller's change to the other's store. A nil
// set stores nothing, and answers value.
func (l *Lookups) LoadOrStore(key, value any) (any, bool) {
	if l == nil {
		return value, false
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if held, ok := l.held[key]; ok {
		return held, true
	}
	if l.held == nil {
		l.held = map[any]any{}
	}
	l.held[key] = value
	return value, false
}

// Forget drops key, so the turn's next call looks it up again.
func (l *Lookups) Forget(key any) {
	if l == nil {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.held, key)
}
