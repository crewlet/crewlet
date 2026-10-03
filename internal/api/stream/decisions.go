package stream

import (
	"context"
	"net/http"
	"sync"
	"sync/atomic"

	"github.com/crewlet/crewlet/internal/api/auth"
)

// EVERY OPEN SOCKET IS DECIDED AGAIN THROUGH ONE QUEUE PER NODE, ONE DECISION
// PER CREDENTIAL.
//
// lifetime.go says WHEN a socket is decided again. What one decision costs is
// an identity read — a session's rows, a machine token's, the binding a Tier A
// token acts through — and every one of those runs on the ONE connection the
// store reserves for identity work (internal/store's DB.Read on a context
// marked store.Identity), serialised behind a lock that ignores contexts. That
// connection is also where every REST request on the node resolves who is
// asking.
//
// The events that decide sockets are not narrow. A published company — every
// chart edit, hire and configuration apply — decides every socket; so does a
// move naming everyone: a release, an invalidation, a batch that retained a
// record (every batch, during a rolling upgrade), a replaced estate, a node
// that stopped vouching. Decided on each socket's own goroutine, the moment
// the signal landed, N open tabs queued N identity reads on that connection at
// once, ahead of every request's own authentication and every new handshake on
// the node; the per-socket timer this replaced had spread the same reads over
// a minute.
//
// # One at a time
//
// So a decision takes the node's one turn ([decisionsAtOnce]) for its read,
// waiting for it on its own socket's context. A request's identity read waits
// behind at most the one socket decision holding the reserved connection,
// rather than behind every open tab; the sockets take their turns behind each
// other, which costs a tab nothing it can see — a decision is a keyed lookup.
//
// # One decision per credential per move
//
// And sockets opened with the SAME credential share it: a person's tabs on one
// cookie, a script's connections on one token. Two handshakes with equal
// [auth.Guard.PresentedKey]s present the same credential and are resolved
// from nothing else, so one read answers for every socket in the group.
//
// What makes one decision valid for another socket is WHEN ITS READ STARTED.
// Every signal counts a move ([decisions.moved]) BEFORE it wakes anybody, and
// the move is counted after the commit or publish it announces; a woken socket
// reads the count, and any decision of its credential that read the count at
// or past that value began its read after the commit, so it already saw what
// the signal was about. Such a decision is shared — in flight or finished —
// and anything older is not: the socket starts a new one. The count is ONE for
// the node, never per credential, because a move does not say whose
// credentials it moved precisely enough to count per key (a move naming
// everyone moves all of them), and over-sharing is the failure here while
// under-sharing only costs a read.
//
// A CREDENTIAL'S OWN END IS NEVER SHARED. No record is written at a deadline,
// so no move is counted for it, and a decision taken before the deadline would
// answer it as served for ever: the timer counts a move of its own, which no
// decision before it can satisfy.
//
// A DECISION THE SOCKET THAT STARTED IT ABANDONS is still finished for the
// rest of the group: its read runs on a context the starter's close does not
// cancel, so a tab closing mid-read does not hand every other tab on its
// cookie an error to close on. One whose starter left before its turn came was
// never read, and a socket waiting on it starts its own.
type decisions struct {
	// moves counts every signal the service has sent. See the type's doc.
	moves atomic.Uint64

	// turn is the node's one decision running at a time — see
	// [decisionsAtOnce].
	turn chan struct{}

	mu sync.Mutex

	// held is every credential an open socket was opened with, by
	// presented key, and its latest decision. An entry leaves with the
	// last socket holding it, so the map is bounded by the open sockets
	// rather than by every credential ever presented here.
	held map[string]*heldKey
}

// decisionsAtOnce is how many sockets on a node have a credential decision
// running at once.
//
// ONE, because the store reserves ONE connection for identity reads and every
// decision is one of those reads: a second decision running beside the first
// would not run any sooner — it would wait on the reserved connection's lock
// instead, ahead of whichever REST request arrived after it. One at a time
// keeps every request's identity read behind at most one socket decision.
const decisionsAtOnce = 1

// heldKey is one credential the open sockets were opened with.
type heldKey struct {
	// sockets is how many open sockets hold it.
	sockets int

	// latest is the newest decision of it, in flight or finished, or nil.
	latest *decision
}

// decision is one read of a credential, shared by every socket whose wake it
// covers.
type decision struct {
	// from is the move count read before the read began: the decision
	// covers every socket woken at or before it.
	from uint64

	// done is closed when the decision is over, read or abandoned. Every
	// field below is written before it closes and read only after.
	done chan struct{}

	// read is false for a decision whose starter left before its turn: no
	// answer is here, and a waiter starts its own.
	read    bool
	r       *http.Request
	refusal *auth.Refusal
}

func newDecisions() *decisions {
	return &decisions{turn: make(chan struct{}, decisionsAtOnce),
		held: map[string]*heldKey{}}
}

// moved counts a move. Called by every signal BEFORE it wakes a listener, so a
// woken socket reads a count that includes it. Never blocks.
func (d *decisions) moved() { d.moves.Add(1) }

// woken is the count a socket woken by a signal needs a decision to cover.
func (d *decisions) woken() uint64 { return d.moves.Load() }

// expired is the count a socket whose credential's own end passed needs a
// decision to cover: a move of its own, which no decision already taken
// covers. See the type's doc.
func (d *decisions) expired() uint64 { return d.moves.Add(1) }

// hold registers an open socket's credential, answering what its decisions are
// asked through and the release its socket calls once, when it closes.
//
// THE RELEASE IS ONCE-ONLY: a second release would decrement a count that is
// no longer this socket's and could take the entry out from under another
// socket on the same credential.
func (d *decisions) hold(key string) (*heldKey, func()) {
	d.mu.Lock()
	defer d.mu.Unlock()
	h := d.held[key]
	if h == nil {
		h = &heldKey{}
		d.held[key] = h
	}
	h.sockets++
	return h, sync.OnceFunc(func() {
		d.mu.Lock()
		defer d.mu.Unlock()
		if h.sockets--; h.sockets == 0 {
			delete(d.held, key)
		}
	})
}

// decide answers the decision of h's credential that covers need: one already
// taken or in flight when its read started late enough, or a new one, taken
// in the node's turn. False when ctx ended first.
func (d *decisions) decide(ctx context.Context, h *heldKey, need uint64,
	decide decideFunc) (*http.Request, *auth.Refusal, bool) {

	for {
		d.mu.Lock()
		current := h.latest
		if !current.covers(need) {
			current = &decision{from: d.moves.Load(), done: make(chan struct{})}
			h.latest = current
			d.mu.Unlock()
			d.take(ctx, current, decide)
		} else {
			d.mu.Unlock()
		}
		select {
		case <-current.done:
		case <-ctx.Done():
			return nil, nil, false
		}
		if current.read {
			return current.r, current.refusal, true
		}
		// ITS STARTER LEFT BEFORE ITS TURN, so nothing was read: this
		// socket starts its own ([decision.covers] no longer holds it),
		// unless it has gone too.
		if ctx.Err() != nil {
			return nil, nil, false
		}
	}
}

// covers reports whether this decision answers a socket that needs one whose
// read began at or after need: one late enough that is in flight — its read,
// when it comes, began after the move — or finished and read. A decision
// abandoned before its turn covers nobody, and is replaced rather than waited
// on again.
func (c *decision) covers(need uint64) bool {
	if c == nil || c.from < need {
		return false
	}
	select {
	case <-c.done:
		return c.read
	default:
		return true
	}
}

// take reads one decision in the node's turn, waiting for the turn on the
// starting socket's context.
//
// THE READ IS DETACHED FROM THAT CONTEXT, because its answer is every waiting
// socket's and not only the starter's — see the type's doc.
func (d *decisions) take(ctx context.Context, current *decision, decide decideFunc) {
	defer close(current.done)
	select {
	case d.turn <- struct{}{}:
	case <-ctx.Done():
		return
	}
	defer func() { <-d.turn }()
	current.r, current.refusal = decide(context.WithoutCancel(ctx))
	current.read = true
}
