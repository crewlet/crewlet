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
// Admission ([Host.MayStart]) proves the seat was held when work STARTED, but
// a turn outlives it by minutes and detaching a queue stops no running
// handler: the fence is the abandoning half. Its common firing is the
// placement sweep handing a seat back (`ReasonPlacement`) with turns running.
// A RUNNING executor run is deliberately not fenced (an ordinary rebalance
// would kill an hour of work); its LAUNCH (the engine's seatFence), RESUME and
// each tool-bridge call (internal/api/mcpbridge) are, and a person's stop ends
// it through sandbox.Coordinator.StopSeat, never this fence. A nil Host, or a
// handle no grant covers (the single-node case), returns a nil, open fence.
func (h *Host) Fence(handle string) func() error {
	if h == nil {
		return nil
	}
	granted, ok := h.grantEpoch(handle)
	if !ok {
		return nil
	}
	// The fence closes on the epoch it was BUILT with, not on membership of
	// the held set: a renew failure and a re-claim are milliseconds apart, so
	// work admitted at epoch 7 can find this node holding epoch 9, a
	// different grant. An undead seat ([undeadSeat]) keeps its lease, so its
	// grant has not moved and the fence stays open. It never fires on a drain
	// (in-flight work finishes before release) nor on an unreachable store
	// (the heartbeat keeps a seat past a failed renew): closed means
	// definitively lost (ADR-0005).
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
