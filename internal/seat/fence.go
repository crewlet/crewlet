package seat

import (
	"errors"
	"fmt"
)

// ErrSeatMoved reports that the grant a piece of work started under is gone:
// the seat is no longer held by this node, or it is held again under a NEW
// epoch, which is a different grant that this work is not covered by.
//
// Comparable with [errors.Is] all the way out of a turn — every frame between
// the tool loop and the dispatcher wraps with %w — because the delivery
// behind such a turn is healthy and a successor is entitled to it, which is
// not what a broken phase means. See [Host.Fence].
var ErrSeatMoved = errors.New("seat: the seat moved to another node")

// Fence is the per-round ownership check for work starting now on handle.
//
// It answers the question [Host.MayStart] deliberately does not. Admission
// proves the seat was held when the work STARTED, and a turn outlives its
// admission by minutes: it calls models, fires tools, posts to chat and
// writes to a tracker long after the one moment anything asked whether this
// node still owns the seat. Nothing else closes that window — the queue
// contract states in one paragraph both that [internal/queue.Broker.Detach]
// "does NOT wait for a running handler" and that a node which has lost a
// lease "must detach FIRST and abandon whatever is in flight". Detaching was
// implemented; abandoning was not, and this is that half.
//
// The common case is not even a lost lease. The placement sweep hands a seat
// back VOLUNTARILY when the fleet grows (`ReasonPlacement`), with turns still
// running on it: the detach returns, the lease is released, a peer claims the
// seat within a sweep interval, and the loser goes on being that seat until
// its turn happens to end.
//
// # Why the epoch and not just membership
//
// A renew that fails and a sweep that re-claims are milliseconds apart —
// [Host.dropLostSeat] re-checks the epoch under the lock for exactly that
// reason. So "is this seat in the held set" is not the question: work
// admitted at epoch 7 can find this node holding epoch 9, which is a
// different grant, taken back after a peer was entitled to the seat and
// possibly after that peer ran the very trigger this turn is still working
// on. The fence therefore closes on the epoch it was BUILT with, which is
// the fencing token the admission handed out, and a re-claim closes it.
//
// # Why an undead seat is still a grant
//
// A seat whose teardown could not be PROVEN leaves the held set but keeps
// its lease, renewed every heartbeat, precisely so no peer can take a seat
// this process may still be consuming — see [undeadSeat]. The grant has
// therefore not moved, and closing the fence over it would abandon work that
// is racing nobody. A missing map entry is not the predicate; a moved grant
// is.
//
// # Why it never fires on a drain
//
// A drain quiesces, waits for in-flight handlers and only then releases, so
// the seat leaves the held set after the work the fence would stop has
// already finished. An UNREACHABLE store is not a firing either: the
// heartbeat KEEPS a seat whose renew merely failed to answer and drops it
// only past the TTL, so a closed fence means definitively lost, never
// unknown. That is ADR-0005's three-valued rule paying for itself here — a
// fence built on a two-valued "is it held" would tear a healthy company's
// turns down over a two-second store blip.
//
// # What it deliberately does not cover
//
// A DETACHED run — a coding CLI in agent mode, or a sandbox job — is not
// fenced, and that is the design rather than a gap. Such a run outlives its
// turn on purpose: its placement is on its own row, and the process that
// collects it is often not the one that launched it, so closing a fence over
// it would kill an hour of work on an ordinary rebalance and lose its output,
// to guard writes the bridge's own run-scoped, expiring credential already
// bounds. What is fenced is the RESUME — the turn that comes back to collect
// the run runs under the grant the collecting node holds.
//
// A nil Host returns nil, and so does a handle no grant covers at all: with
// no grant to compare against there is nothing to fence, which is the
// single-node case and every test that runs a turn without a fleet. A nil
// fence is an open one.
func (h *Host) Fence(handle string) func() error {
	if h == nil {
		return nil
	}
	granted, ok := h.grantEpoch(handle)
	if !ok {
		return nil
	}
	return func() error {
		now, still := h.grantEpoch(handle)
		switch {
		case !still:
			return fmt.Errorf("%w: %q was held at epoch %d and is not held here any more",
				ErrSeatMoved, handle, granted)
		case now != granted:
			return fmt.Errorf("%w: %q was held at epoch %d and is now held at epoch %d, "+
				"which is a grant this work is not covered by",
				ErrSeatMoved, handle, granted, now)
		}
		return nil
	}
}

// grantEpoch is the epoch of whatever grant this node still holds on handle,
// held or undead, and false when it holds none.
//
// Broader than [Host.EpochFor] by exactly the undead set, and the difference
// is deliberate: EpochFor answers "may I act AS this seat", which an undead
// seat may not, while this answers "does this node still hold the lease",
// which an undead seat does. Fencing work already under way is the second
// question — see [Host.Fence].
func (h *Host) grantEpoch(handle string) (int64, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if held := h.held[handle]; held != nil {
		return held.lease.Epoch, true
	}
	if dead := h.undead[handle]; dead != nil && dead.held != nil {
		return dead.held.lease.Epoch, true
	}
	return 0, false
}
