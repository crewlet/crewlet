package node

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"
)

// The one admission every turn on this node passes.
//
// # Why it is one method rather than a check each path makes
//
// A turn reaches this node by more than one road. A seat's mailbox delivers
// most of them, and the turn it starts was the only one this package admitted,
// inside the subscription's handler. A coding run that finishes in its box
// resumes the turn that launched it from the seat's CONTROL topic, and a
// person's recorded answer resumes a parked run from the sandbox coordinator's
// own timer: neither is a mailbox delivery, so neither passed the ownership
// check, the concurrency slot or the drain's closed gate. A draining node went
// on starting full turns through them while every mailbox was quiesced and
// every API route refused work, and [Node.Drain] waited for each one before it
// handed the seats back.
//
// So the admission is exported, and the mailbox's handler is one caller of it
// among several rather than the place it lives. Whatever road a turn arrives
// by, it is admitted here or it does not start.
//
// # What it decides
//
// In this order, each for the reason the mailbox path always had:
//
//  1. FRESH OWNERSHIP of the seat ([seat.Host.MayStart]). A node that cannot
//     prove a recent renew does not start minutes of work on a seat a peer
//     may already be entitled to.
//  2. A CONCURRENCY SLOT ([gate]), which waits while the node is at its
//     ceiling and refuses at once while it drains.
//  3. THE DRAIN'S CUT: the admitted context is cancelled if a drain finds the
//     queue's acknowledgements lost, as every mailbox turn's always was.
//
// # A turn inside a turn
//
// A recorded answer is usually resumed INLINE, on the mailbox delivery that
// carried it, and that delivery already holds this seat's admission. Admitting
// it a second time would take a second slot for the one turn that is running,
// and on a node whose ceiling is one it would wait for ever on the slot its own
// delivery holds. So the admitted context carries the admission, and asking
// again for the same seat on this node while that admission is still held is
// answered by it. Only while it is held: a context that outlives its turn
// (detached with [context.WithoutCancel], say) keeps its values, and a
// released admission it still carried would let a later turn past a drain.

// ErrSeatNotOwned is a turn refused because this node cannot prove it still
// holds the seat. The work belongs to whichever node does, and the caller hands
// it back for that node to take.
var ErrSeatNotOwned = errors.New("node: this node cannot prove it holds the seat")

// ErrDraining is a turn refused because this node is draining. The node is
// handing its seats to peers, and a turn started now would run inside a
// shutdown that waits for it. The caller hands the work back, undone, for the
// seat's next holder.
var ErrDraining = errors.New("node: this node is draining")

// admission is one turn's hold on its seat's admission, carried on the turn's
// context so a nested ask for the same seat reuses it.
type admission struct {
	node   *Node
	handle string
	// released is set when the hold is let go, after which a context that
	// still carries it admits nothing.
	released atomic.Bool
}

type admissionKey struct{}

// AdmitTurn admits one turn on handle's seat, or refuses it.
//
// Admitted, it returns the context the turn runs under and the release the
// caller defers. The release is idempotent, and it ends the context it
// returned. Refused, the error wraps [ErrSeatNotOwned], [ErrDraining] or the
// caller's own context error, and the release is nil: there is nothing to give
// back.
func (n *Node) AdmitTurn(ctx context.Context, handle string) (context.Context, func(), error) {
	if held, ok := ctx.Value(admissionKey{}).(*admission); ok &&
		held.node == n && held.handle == handle && !held.released.Load() {
		return ctx, func() {}, nil
	}
	if _, ok := n.host.MayStart(handle); !ok {
		return nil, nil, fmt.Errorf("%w %q", ErrSeatNotOwned, handle)
	}
	// THE SLOT AFTER THE OWNERSHIP CHECK and before the turn, which is
	// before its first model round-trip. Ordered this way because a turn on
	// a seat this node has lost must not sit in the queue for a slot it has
	// no business taking: the successor is already entitled to the work.
	free, ok := n.turns.acquire(ctx)
	if !ok {
		if err := ctx.Err(); err != nil {
			return nil, nil, fmt.Errorf("node: admitting a turn on %q: %w", handle, err)
		}
		return nil, nil, fmt.Errorf("%w, so the turn on %q was not started", ErrDraining, handle)
	}
	held := &admission{node: n, handle: handle}
	ctx, cancel := context.WithCancel(context.WithValue(ctx, admissionKey{}, held))
	// ENDED BY A DRAIN THAT CANNOT WAIT FOR IT, and by nothing else here:
	// once the queue's acknowledgements can no longer land, this turn's
	// outcome will not be recorded and its seat's successor runs the same
	// work again, so every side effect it makes from then on is made twice.
	// See [Node.Drain]. The watcher's lifetime is the admission's: the
	// release ends it however the turn returns.
	go func() {
		select {
		case <-n.cut:
			cancel()
		case <-ctx.Done():
		}
	}()
	return ctx, func() {
		if held.released.CompareAndSwap(false, true) {
			cancel()
			free()
		}
	}, nil
}
