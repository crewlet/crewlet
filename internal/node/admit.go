package node

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"

	"github.com/crewlet/crewlet/internal/queue/topics"
	"github.com/google/uuid"
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
//  3. ONE TURN PER SEAT ([ErrSeatBusy]), refused rather than waited for: see
//     below. After the slot, so a draining node says it is draining (the
//     refusal its callers hand work on for), and a turn that waited for a
//     slot is checked against the seat as it stands once it has one.
//  4. THE DRAIN'S CUT: the admitted context is cancelled if a drain finds the
//     queue's acknowledgements lost, as every mailbox turn's always was.
//
// # One turn per seat
//
// A seat is one agent, and a turn acts for it: it reads the seat's thread and
// memory, posts as it, and writes to the tracker under its name. Two of its
// turns in flight at once each act with no view of the other. The mailbox
// alone could not have caused that, because it dispatches one delivery at a
// time, but the other roads could: a person's answer reaches the run it names
// on the seat's control topic, which no mailbox turn waits for, and its resume
// would start beside whatever turn the mailbox was running. So the admission
// that every road passes is where the rule lives.
//
// WHAT WAITS FOR THE SEAT, AND HOW, without spending a delivery. A turn that
// does not come from the mailbox (a completion's, an answer's) is REFUSED on a
// busy seat, and its caller keeps the work and tries again when
// [Config.SeatTurnEnded] says the seat is free. Once admitted, such a turn
// HOLDS THE SEAT'S INBOX for as long as it runs ([HoldSeatTurn]), so the
// mailbox fetches nothing meanwhile: the mail waits on the broker with its
// order and its delivery count intact, as it does behind a coding run's hold.
// A mailbox delivery that was fetched before that hold took effect WAITS for
// the seat rather than being handed back, because every hand-back spends one
// of the message's deliveries (queue.OutcomeDefer), and a seat busy for hours
// would have spent its own mail into the dead-letter subject.
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

// ErrSeatBusy is a turn refused because another turn is running on the same
// seat on this node. Nothing is wrong with the work: the caller hands it back,
// or waits for [Config.SeatTurnEnded], and it runs once the seat's turn ends.
var ErrSeatBusy = errors.New("node: another turn is running on the seat")

// ErrDraining is a turn refused because this node is draining. The node is
// handing its seats to peers, and a turn started now would run inside a
// shutdown that waits for it. The caller hands the work back, undone, for the
// seat's next holder.
var ErrDraining = errors.New("node: this node is draining")

// HoldSeatTurn is the reason a seat's inbox is held while a turn that did not
// come from it runs on the seat ([Node.AdmitTurn]).
const HoldSeatTurn = "seat_turn"

// admission is one turn's hold on its seat's admission, carried on the turn's
// context so a nested ask for the same seat reuses it.
type admission struct {
	node   *Node
	handle string
	// done is closed when the admission is released, for a mailbox delivery
	// waiting for the seat.
	done chan struct{}
	// released is set when the hold is let go, after which a context that
	// still carries it admits nothing.
	released atomic.Bool
	// cancel ends the context the turn runs under.
	cancel context.CancelFunc
}

type admissionKey struct{}

// mailboxKey marks a context as a mailbox delivery's ([mailboxTurn]).
type mailboxKey struct{}

// mailboxTurn is ctx marked as the context of a turn the seat's own mailbox
// delivered, which [Node.AdmitTurn] admits on the mailbox's terms.
func mailboxTurn(ctx context.Context) context.Context {
	return context.WithValue(ctx, mailboxKey{}, true)
}

// AdmitTurn admits one turn on handle's seat, or refuses it. A turn that does
// not come from the seat's mailbox (a coding run's completion, a recorded
// answer's resume) is refused on a busy seat, and once admitted holds the
// seat's inbox until it is released ([HoldSeatTurn]); a mailbox delivery's
// turn waits for the seat instead (see "One turn per seat" above).
//
// Admitted, it returns the context the turn runs under and the release the
// caller defers. The release is idempotent, and it ends the context it
// returned. Refused, the error wraps [ErrSeatNotOwned], [ErrSeatBusy],
// [ErrDraining] or the caller's own context error, and the release is nil:
// there is nothing to give back.
func (n *Node) AdmitTurn(ctx context.Context, handle string) (context.Context, func(), error) {
	// A MAILBOX DELIVERY'S TURN ([mailboxTurn]) waits for a busy seat rather
	// than being refused, and takes no hold on the inbox it is itself being
	// dispatched from.
	fromMailbox := ctx.Value(mailboxKey{}) != nil
	if held, ok := ctx.Value(admissionKey{}).(*admission); ok &&
		held.node == n && held.handle == handle && !held.released.Load() {
		return ctx, func() {}, nil
	}
	for {
		if _, ok := n.host.MayStart(handle); !ok {
			return nil, nil, fmt.Errorf("%w %q", ErrSeatNotOwned, handle)
		}
		// THE SLOT AFTER THE OWNERSHIP CHECK and before the turn, which is
		// before its first model round-trip. Ordered this way because a turn
		// on a seat this node has lost must not sit in the queue for a slot
		// it has no business taking: the successor is already entitled to
		// the work.
		free, ok := n.turns.acquire(ctx)
		if !ok {
			if err := ctx.Err(); err != nil {
				return nil, nil, fmt.Errorf("node: admitting a turn on %q: %w", handle, err)
			}
			return nil, nil, fmt.Errorf("%w, so the turn on %q was not started", ErrDraining, handle)
		}
		// THE SEAT, once the turn has a slot: a second turn on a seat that
		// is mid-turn gives its slot straight back.
		held := &admission{node: n, handle: handle, done: make(chan struct{})}
		n.mu.Lock()
		running, busy := n.running[handle]
		if !busy {
			n.running[handle] = held
		}
		n.mu.Unlock()
		if busy {
			free()
			if !fromMailbox {
				return nil, nil, fmt.Errorf("%w %q", ErrSeatBusy, handle)
			}
			// A MAILBOX DELIVERY WAITS, unspent, and asks again once the
			// running turn is released.
			select {
			case <-running.done:
				continue
			case <-ctx.Done():
				return nil, nil, fmt.Errorf("node: admitting a turn on %q: %w", handle, ctx.Err())
			}
		}
		return n.admitted(ctx, held, fromMailbox), held.release(free, fromMailbox), nil
	}
}

// admitted is the context an admitted turn runs under: carrying its admission,
// and ended by a drain that cannot wait for it. A turn that did not come from
// the mailbox holds the seat's inbox from here.
func (n *Node) admitted(ctx context.Context, held *admission, fromMailbox bool) context.Context {
	if !fromMailbox {
		n.holdInbox(ctx, held.handle, true)
	}
	ctx, cancel := context.WithCancel(context.WithValue(ctx, admissionKey{}, held))
	held.cancel = cancel
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
	return ctx
}

// release is the admission's release: idempotent, and in this order, the
// context ended, the slot freed, the seat freed, the inbox hold lifted, and
// the seat's turn announced as ended ([Config.SeatTurnEnded]).
//
// THE SEAT IS FREED BEFORE THE HOLD IS LIFTED: lifting it can dispatch the
// held mail on this very goroutine (the in-memory broker delivers inline), and
// a delivery that found the seat still taken would wait on this release.
func (held *admission) release(free func(), fromMailbox bool) func() {
	return func() {
		if held.released.CompareAndSwap(false, true) {
			held.cancel()
			free()
			held.node.freeSeat(held)
			if !fromMailbox {
				held.node.holdInbox(context.Background(), held.handle, false)
			}
			if hook := held.node.cfg.SeatTurnEnded; hook != nil {
				hook(held.handle)
			}
		}
	}
}

// holdInbox takes or lifts [HoldSeatTurn] on the seat's inbox. A seat whose
// mailbox this node has not attached has no inbox here to hold. A queue that
// refuses is logged and left: the mailbox's own wait for the seat
// ([Node.admit]) still keeps its turn from starting beside this one, at the
// cost of one in-flight delivery's ack window rather than a delivery.
func (n *Node) holdInbox(ctx context.Context, handle string, hold bool) {
	n.mu.Lock()
	var seatID uuid.UUID
	for id, h := range n.attached {
		if h == handle {
			seatID = id
			break
		}
	}
	n.mu.Unlock()
	inbox, group := topics.AgentInbox(seatID), topics.AgentInboxGroup(seatID)
	if inbox == "" {
		return
	}
	ctx = context.WithoutCancel(ctx)
	var err error
	if hold {
		err = n.cfg.Queue.PauseTopic(ctx, inbox, group, HoldSeatTurn)
	} else {
		err = n.cfg.Queue.ResumeTopic(ctx, inbox, group, HoldSeatTurn)
	}
	if err != nil {
		n.log.Warn("seat_turn_hold_failed", "handle", handle, "hold", hold, "error", err.Error())
	}
}

// freeSeat frees the seat a turn held, and wakes a mailbox delivery waiting
// for it.
func (n *Node) freeSeat(held *admission) {
	n.mu.Lock()
	if n.running[held.handle] == held {
		delete(n.running, held.handle)
	}
	n.mu.Unlock()
	close(held.done)
}
