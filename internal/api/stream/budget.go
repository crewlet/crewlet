package stream

import "sync"

// THE IN-FLIGHT QUERY BUDGET IS PER PRINCIPAL, not per socket.
//
// # What per-socket cost
//
// A socket admitted four concurrent queries, which is what one screen opens
// with (the agent page issues three). Nothing tied a person's SECOND tab to
// their first, so the same person watching a company from three tabs offered
// twelve concurrent scans, and from six, twenty-four — every one of them
// taking a connection from the reader pool the engine shares.
//
// That pool is sized against exactly this: internal/store's minReaderConns is
// 8 because it is "two full dashboards" at four queries each. The sizing was
// right and the unit was wrong, so the assumption behind the floor was false
// from the day it was written — one person with three tabs already exceeded
// what the pool was built to hold, and what queued behind them was the
// engine's own reads: a seat's tool lookups, the coverage probes, the health
// body. The identity reserve exists because that queue feeds itself.
//
// Per principal, the store's derivation is true for the first time: N tabs
// belonging to one person share one budget, so the floor really does hold two
// full dashboards, and a second operator's burst is unaffected by the first's
// — which is the property a shared cap could never have.
//
// # Why a map with a refcount rather than a bare map of channels
//
// The key is a principal, so entries are created by anybody who can
// authenticate and would otherwise accumulate for the life of the process —
// one channel per person who has ever opened a tab, kept alive by nothing. The
// refcount is how an entry leaves: the last socket for a principal to close
// takes it out. It counts SOCKETS rather than in-flight queries because a
// socket is what holds the reference, and a budget released while a query was
// still running would let the next socket start a second full burst beside it.

// MaxInFlightQueries bounds how many queries ONE PRINCIPAL may have running at
// once, across every socket they hold.
//
// FOUR, which is what one screen opens with — the agent page issues three —
// so a single dashboard never queues against itself. It is the same number it
// has always been and it now means something different: per person rather than
// per tab, which is the unit internal/store's reader floor was already sized
// in. A burst past it QUEUES rather than piling into the engine's connection
// pool, because queries run on their own goroutines so a store scan cannot
// stall the live feed.
const MaxInFlightQueries = 4

// budgets hands out one in-flight budget per principal.
//
// The zero value is not usable; build one with [newBudgets]. It is held by the
// service for the process's lifetime, so every socket for one principal
// reaches the same slots channel.
type budgets struct {
	mu   sync.Mutex
	held map[string]*budget
}

// budget is one principal's slots, and how many sockets are holding them.
type budget struct {
	slots   chan struct{}
	sockets int
}

func newBudgets() *budgets {
	return &budgets{held: make(map[string]*budget)}
}

// acquire returns the principal's slots channel and the release to call when
// this socket closes.
//
// AN EMPTY PRINCIPAL GETS ITS OWN BUDGET rather than sharing one keyed on "".
// No socket reaches here without authenticating today, so the case is
// unreachable — but if the socket path ever became exempt, every anonymous
// reader in the world sharing one four-slot budget would be a denial of
// service against the dashboard rather than a bound on one.
//
// THE RELEASE IS ONCE-ONLY, and that is not defensive tidiness. serveSocket
// defers it, and a second path unwinding through the same socket — a panic, a
// caller that releases and then returns through the defer — would decrement a
// count that is no longer this socket's: between the two calls another socket
// for the same principal can have acquired a FRESH entry, and the second
// release takes that live one out. The next acquire then builds a second
// budget beside the queries already running under the first, which is the
// per-socket allowance coming back by a longer route. Guarding the decrement
// against zero does not reach it — the entry at that point is a legitimate
// one with a legitimate count — so the fix has to be that a release cannot
// happen twice at all.
func (b *budgets) acquire(principal string) (chan struct{}, func()) {
	b.mu.Lock()
	defer b.mu.Unlock()
	held, ok := b.held[principal]
	if !ok {
		held = &budget{slots: make(chan struct{}, MaxInFlightQueries)}
		b.held[principal] = held
	}
	held.sockets++
	return held.slots, sync.OnceFunc(func() { b.release(principal) })
}

// release drops one socket's hold, removing the entry with the last of them.
//
// Unexported and reached only through the once-wrapped closure [acquire]
// returns, because the count is what keeps a live principal's budget alive and
// a caller holding the key rather than the closure could decrement one it
// never acquired.
func (b *budgets) release(principal string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	held, ok := b.held[principal]
	if !ok {
		return
	}
	if held.sockets--; held.sockets == 0 {
		delete(b.held, principal)
	}
}

// tracked is how many principals hold a budget, for the case that asserts an
// entry does not outlive the sockets that made it.
func (b *budgets) tracked() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.held)
}
