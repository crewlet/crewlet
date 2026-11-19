package turnctx

import "sync"

// Lookups is what a turn has looked up by name once: the code tools' repository
// is resolved at linearizable once and read by id after (internal/agent/
// builtin's CodeDeps.repo), not one barrier per read.
//
// An entry is a fact, never a permission: the node answering each later read
// judges the asker again from its chart, so a stale entry can ask about an
// object that moved, never one the asker may not read. Consumers key entries
// with a comparable type of their own, as context.WithValue does. In memory for
// the turn's life, shared by derived Turns; a resumed segment starts empty. A
// consumer finding an entry untrue FORGETS it, never replaces it with what
// another read said in passing: the ONE lookup seam does more than read.
// Nil is safe: every Load misses.
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
