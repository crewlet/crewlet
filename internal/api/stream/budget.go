package stream

import (
	"sync"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/iam"
)

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
// # Keyed on the principal's ID, never its login
//
// A login is OPTIONAL: a person the directory enrolled by address alone —
// every invitation redeemed without one, every person an administrator created
// with an email and nothing else — carries an empty login. Keyed on the login,
// every one of them shared ONE four-slot budget, so the second such person to
// open a dashboard queued behind the first, and a burst from any of them was an
// outage for all of them. The id is what every row keys a principal on: it is
// never empty for a resolved one and never shared between two, and a person
// keeps it through a rename, so their tabs go on sharing one budget across
// the change.
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

// budgetKeyOf is the key a principal's in-flight budget is held under: its ID.
//
// A FUNCTION RATHER THAN A FIELD READ AT THE CALL SITE, so the rule has one
// place to be asserted — the socket is the only caller, and the property that
// went wrong (two people enrolled by address alone sharing a budget because
// both had an empty login) is invisible through a socket unless a suite signs
// two such people in.
func budgetKeyOf(p iam.Principal) uuid.UUID { return p.ID }

// budgets hands out one in-flight budget per principal.
//
// The zero value is not usable; build one with [newBudgets]. It is held by the
// service for the process's lifetime, so every socket for one principal
// reaches the same slots channel.
type budgets struct {
	mu   sync.Mutex
	held map[uuid.UUID]*budget
}

// budget is one principal's slots, and how many sockets are holding them.
type budget struct {
	slots   chan struct{}
	sockets int
}

func newBudgets() *budgets {
	return &budgets{held: make(map[uuid.UUID]*budget)}
}

// acquire returns the principal's slots channel and the release to call when
// this socket closes.
//
// THE NIL ID GETS A BUDGET OF ITS OWN, PER SOCKET, rather than sharing one
// keyed on the nil uuid — which is what this used to promise for an empty
// login and did not do: the map simply held one entry for "" that everybody
// without a login shared. A resolved principal always carries an id, so the
// case should be unreachable; but a socket that reached here with none is one
// this package cannot tell from any other, and pooling every such socket
// behind four slots would make them a denial of service against each other
// rather than a bound on any one. It is not entered in the map, so there is
// nothing to leak and nothing for a release to evict.
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
func (b *budgets) acquire(principal uuid.UUID) (chan struct{}, func()) {
	if principal == uuid.Nil {
		return make(chan struct{}, MaxInFlightQueries), func() {}
	}
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
func (b *budgets) release(principal uuid.UUID) {
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
