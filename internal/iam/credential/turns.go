package credential

import (
	"container/list"
	"context"
	"sync"
)

// THE VERIFY CAP, SHARED OUT BETWEEN SOURCES IN TURN.
//
// [VerifyCap] bounds how many derivations run at once, and that bounds memory.
// It said nothing about WHOSE derivations run, and that is a second resource
// an unauthenticated stranger was free to take whole: the cap was one queue,
// first come first served, and a derivation waited in it with no bound and no
// way to leave. One address cycling logins it knows queued as many argon2id
// verifications as it could open connections for; every honest sign-in
// anywhere waited behind all of them; a derivation whose caller had long
// since hung up still ran when its slot came; and once the queue pushed
// verifications past the sign-in pad, the pad's timing equalisation stopped
// holding — the residue the throttle states as needing a load spike, produced
// by one address. The throttle's pair curve bounds none of it, because a run
// across names is a fresh pair every time.
//
// # One turn per source, served in turn
//
// So every derivation a request causes is asked for on behalf of its SOURCE —
// the client's address as the trusted proxies resolve it, an IPv6 client by
// its /64 ([sourceKeyOf]) — and a source holds AT MOST ONE turn at a time.
// Its further attempts wait in its own lane, in arrival order, and a source
// whose lane has something waiting rejoins the back of the line when its turn
// ends. Three things follow:
//
//   - ONE ADDRESS CAN NEVER HOLD MORE THAN ONE SLOT, so it cannot fill the cap
//     and cannot delay another source by more than the turns of the other
//     sources in line — never by its own queue's length. Spraying one password
//     across the directory from one address runs one derivation at a time.
//   - A SOURCE'S LANE IS ITS OWN, and that includes everybody sharing its
//     address: an office behind one NAT, a VPN's egress, the whole internet
//     behind a proxy nobody named in `api.trusted_proxies`. Nothing is REFUSED
//     on the address — a refusal there is one a stranger at it holds shut for
//     everybody else, which is why the throttle has no curve on it — but a
//     stranger there with many requests open is ahead of a colleague who
//     arrives after them. That is the price of a key that cannot tell them
//     apart, and it is paid only by the address the stranger is at.
//   - A WAIT GIVES UP ITS PLACE WITH ITS REQUEST: a waiter whose context ends
//     leaves the lane, and nothing is derived for a caller who is not there.
//     A turn once granted runs to its end, because argon2 is not
//     interruptible and a derivation abandoned half-way has spent its cost.
//
// # Not work-conserving, deliberately
//
// A slot free while only one source is waiting stays free rather than going to
// that source's second attempt. The other arrangement lets one address fill
// the cap whenever nobody else is signing in, and the next honest arrival then
// waits for a slot to free — which, with a derivation near the pad's deadline,
// is what pushes an honest verification past it. One slot per source is what
// makes "one address cannot open the timing residue" true rather than usually
// true. Its cost is that one address's own sign-ins are served one at a time
// — at about one derivation per sign-in, far above the rate any address signs
// honest people in at.
//
// # A decoy takes a turn too
//
// A verification for a subject that does not exist is a decoy
// ([Hasher.Decoy]), and it takes the same turn in the same lane and holds its
// slot for as long as a derivation takes. Without that, one address firing a
// handful of attempts at once learns which names are real from nothing but the
// order they come back in: the real ones queue behind each other in its lane
// and answer past the pad, and the decoys, queueing for nothing, answer at it.

// turns is the cap, shared out between sources in turn. SAFE FOR CONCURRENT
// USE.
type turns struct {
	mu sync.Mutex

	// size is the cap, and free how many of its slots are not held.
	size, free int

	// lanes are the sources with a turn held or waited for, and nobody else:
	// a source is forgotten the moment it has neither, so the set is bounded
	// by the requests in flight rather than by every address ever seen.
	lanes map[string]*lane

	// ready are the sources whose next turn waits only for a slot — one is
	// waiting and none is running — in the order they became so.
	ready list.List // of *lane
}

// lane is one source's waiting attempts.
type lane struct {
	source  string
	waiting list.List // of *waiter, in arrival order
	running bool
	queued  *list.Element // this lane's place in ready, or nil
}

// waiter is one attempt waiting for its turn; granted is closed, under the
// lock, when the turn is its.
type waiter struct {
	granted chan struct{}
	el      *list.Element
}

func newTurns(slots int) *turns {
	return &turns{size: slots, free: slots, lanes: map[string]*lane{}}
}

// take waits for source's next turn and answers what ends it — a function the
// caller calls once, when the derivation is over — or the context's error when
// the wait ended first, in which case the attempt has left the lane and
// nothing is held.
func (t *turns) take(ctx context.Context, source string) (func(), error) {
	t.mu.Lock()
	l := t.lanes[source]
	if l == nil {
		l = &lane{source: source}
		t.lanes[source] = l
	}
	w := &waiter{granted: make(chan struct{})}
	w.el = l.waiting.PushBack(w)
	if !l.running && l.queued == nil {
		l.queued = t.ready.PushBack(l)
	}
	t.dispatchLocked()
	t.mu.Unlock()

	select {
	case <-w.granted:
		return sync.OnceFunc(func() { t.end(l) }), nil
	case <-ctx.Done():
	}

	t.mu.Lock()
	select {
	case <-w.granted:
		// GRANTED AS THE REQUEST WENT AWAY: the turn is this waiter's,
		// and it is handed straight back rather than spent on nobody.
		t.mu.Unlock()
		t.end(l)
		return nil, ctx.Err()
	default:
	}
	l.waiting.Remove(w.el)
	if l.waiting.Len() == 0 {
		if l.queued != nil {
			t.ready.Remove(l.queued)
			l.queued = nil
		}
		if !l.running {
			delete(t.lanes, l.source)
		}
	}
	t.mu.Unlock()
	return nil, ctx.Err()
}

// tryTake takes a slot outside every lane, at once or not at all, answering
// what gives it back. It is for work nobody is waiting on, which must not take
// a place ahead of anybody who is: a slot is free only when no source has a
// turn waiting for one, because turns are handed out the moment a slot frees.
func (t *turns) tryTake() (func(), bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.free <= 0 {
		return nil, false
	}
	t.free--
	return sync.OnceFunc(func() {
		t.mu.Lock()
		defer t.mu.Unlock()
		t.free++
		t.dispatchLocked()
	}), true
}

// end ends the turn l's source holds: the slot goes to whoever is next in line,
// and l rejoins the back of it if it has more waiting.
func (t *turns) end(l *lane) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.free++
	l.running = false
	if l.waiting.Len() > 0 {
		l.queued = t.ready.PushBack(l)
	} else {
		delete(t.lanes, l.source)
	}
	t.dispatchLocked()
}

// dispatchLocked hands every free slot to the source at the front of the line.
// Held under the lock.
func (t *turns) dispatchLocked() {
	for t.free > 0 && t.ready.Len() > 0 {
		l := t.ready.Remove(t.ready.Front()).(*lane)
		l.queued = nil
		w := l.waiting.Remove(l.waiting.Front()).(*waiter)
		l.running = true
		t.free--
		close(w.granted)
	}
}

// held is how many slots are held and how many sources have a lane, for the
// suite that asserts nothing is left behind.
func (t *turns) held() (slots, lanes int) {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.size - t.free, len(t.lanes)
}

// HoldTurn takes source's turn at h's cap and answers what ends it, for a
// suite outside this package that has to hold one.
//
// EXPORTED FOR A TEST AND SAYING SO: that a route hands the hasher the source
// its request came from, rather than nobody's or everybody's, is a property of
// the route's wiring, and the only way to see it hold from outside is to hold
// one source's turn and watch that route's request — and nobody else's — wait
// for it.
func HoldTurn(ctx context.Context, h *Hasher, source string) (func(), error) {
	return h.turns.take(ctx, sourceKeyOf(source))
}
