package node

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sync/atomic"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/queue"
	"github.com/crewlet/crewlet/internal/queue/topics"
)

// The one admission every turn on this node passes ([Node.AdmitTurn]). It is
// one method, not a check each path makes, because turns arrive by more than the
// mailbox: an executor finishing in its box resumes on the seat's CONTROL topic
// and a recorded answer resumes from the sandbox coordinator's timer, and
// neither passed the ownership check, the slot or the drain's closed gate, so a
// draining node kept starting full turns. Whatever road, it is admitted here or
// it does not start. In order:
//
//  1. FRESH OWNERSHIP ([seat.Host.MayStart]): no minutes of work on a seat a peer may own.
//  2. A CONCURRENCY SLOT ([gate]): waits at its ceiling, refuses at once while draining.
//  3. THE SEAT'S CAP, ONE TURN PER CONVERSATION, after the slot so a draining node says so.
//  4. THE DRAIN'S CUT: the context is cancelled if a drain finds acks lost.

// ErrSeatNotOwned is a turn refused because this node cannot prove it still
// holds the seat. The work belongs to whichever node does, and the caller hands
// it back for that node to take.
var ErrSeatNotOwned = errors.New("node: this node cannot prove it holds the seat")

// ErrSeatFull is a turn refused because the seat runs as many turns on this
// node as its cap ([Config.SeatTurns]), ErrConversationBusy one refused
// because another turn of its conversation runs there (ADR-0169). The caller
// hands the work back, or waits for [Config.SeatTurnEnded].
//
// The mailbox dispatches one at a time, but a person's answer resumes on the
// control topic beside it, so the rule lives where every road passes. A
// non-mailbox turn is REFUSED and HOLDS THE SEAT'S INBOX while it runs
// ([HoldSeatTurn]), so the mail waits on the broker intact. A mailbox delivery
// fetched before the hold WAITS rather than being handed back, since each
// hand-back spends a delivery (queue.OutcomeDefer) and a long-busy seat would
// spend its mail into the dead-letter subject.
var (
	ErrSeatFull         = errors.New("node: the seat runs as many turns as its cap")
	ErrConversationBusy = errors.New("node: another turn of the conversation is running on the seat")
)

// ErrDraining is a turn refused because this node is draining. The node is
// handing its seats to peers, and a turn started now would run inside a
// shutdown that waits for it. The caller hands the work back, undone, for the
// seat's next holder.
var ErrDraining = errors.New("node: this node is draining")

// HoldSeatTurn prefixes the reason a seat's inbox is held under while a turn
// that did not come from it runs on the seat ([Node.AdmitTurn]).
//
// ONE REASON PER ADMISSION, `seat_turn:<n>`, never one shared by every such
// turn: a queue's holds are a SET of reasons, and a turn admitted in the
// moment between the previous turn freeing the seat and lifting its hold
// would have its own hold lifted with it, and run with the mailbox fetching
// beside it.
const HoldSeatTurn = "seat_turn"

// admission is one turn's hold on its seat's admission, carried on the turn's
// context so a nested ask for the same seat reuses it: a recorded answer is
// usually resumed INLINE on a delivery that already holds the admission, and a
// second slot would wait for ever on a node whose ceiling is one. Only while it
// is held: a context detached with [context.WithoutCancel] keeps its values,
// and a released admission it still carried would let a later turn past a drain.
type admission struct {
	node   *Node
	handle string
	// conversation is the conversation identity the turn works in, "" for one
	// it cannot name, which no other turn is refused behind.
	conversation string
	// hold is the reason this admission holds the seat's inbox under, its
	// own ([HoldSeatTurn]), and seat and inbox the mailbox it took that hold
	// on and the attachment it took it for, the zero one for none taken.
	// Under the node's mu.
	hold  string
	seat  uuid.UUID
	inbox queue.Attachment
	// contended is set when another turn was refused for the seat while this
	// admission held it ([ErrSeatFull], [ErrConversationBusy]): somebody is
	// waiting for the seat, and its release says so ([Config.SeatTurnEnded]).
	contended atomic.Bool
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

// seatTurns is the turns one seat has admitted on this node and not yet
// released, and freed, closed and replaced at every release, which a mailbox
// delivery waiting for room waits on.
type seatTurns struct {
	turns []*admission
	freed chan struct{}
}

// refusal is why a seat admits no turn of conversation now, nil where it does:
// it runs one of that conversation, named first since only that turn's end
// lifts it, or as many as its cap.
func (s *seatTurns) refusal(conversation string, limit int) error {
	if s == nil {
		return nil
	}
	if conversation != "" && slices.ContainsFunc(s.turns, func(a *admission) bool {
		return a.conversation == conversation
	}) {
		return ErrConversationBusy
	}
	if len(s.turns) >= limit {
		return ErrSeatFull
	}
	return nil
}

// seatLimit is how many turns handle's seat runs at once on this node
// ([Config.SeatTurns]), one where nothing says.
func (n *Node) seatLimit(handle string) int {
	if n.cfg.SeatTurns == nil {
		return 1
	}
	return max(n.cfg.SeatTurns(handle), 1)
}

// AdmitTurn admits one turn of conversation on handle's seat, or refuses it.
// A turn that does not come from the seat's mailbox (an executor's completion,
// a recorded answer's resume) is refused on a seat at its cap or busy with the
// conversation, and once admitted holds the seat's inbox until it is released
// ([HoldSeatTurn]); a mailbox delivery's turn waits instead. conversation is
// the turn's conversation identity, "" where it names none.
//
// Admitted, it returns the context the turn runs under and the release the
// caller defers. The release is idempotent, and it ends the context it
// returned. Refused, the error wraps [ErrSeatNotOwned], [ErrSeatFull],
// [ErrConversationBusy], [ErrDraining] or the caller's own context error, and
// the release is nil: there is nothing to give back.
func (n *Node) AdmitTurn(ctx context.Context, handle, conversation string) (context.Context, func(), error) {
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
		// THE SEAT, once the turn has a slot: a turn past the seat's cap,
		// or a second of one conversation, gives its slot straight back.
		held := &admission{node: n, handle: handle, conversation: conversation,
			hold: fmt.Sprintf("%s:%d", HoldSeatTurn, n.admissions.Add(1))}
		n.mu.Lock()
		seat := n.running[handle]
		refused := seat.refusal(conversation, n.seatLimit(handle))
		switch {
		case refused == nil:
			if seat == nil {
				seat = &seatTurns{freed: make(chan struct{})}
				n.running[handle] = seat
			}
			seat.turns = append(seat.turns, held)
		case !fromMailbox:
			// MARKED UNDER THE LOCK a running turn's release frees the
			// seat under ([Node.freeSeat]), so that release either finds
			// room for this turn or reads the mark: set once the lock was
			// let go, a release landing in between announced nothing, and
			// the refused turn waited for its backstop. Every running turn,
			// since the release of any can make the room.
			for _, running := range seat.turns {
				running.contended.Store(true)
			}
		}
		var freed <-chan struct{}
		if seat != nil {
			freed = seat.freed
		}
		n.mu.Unlock()
		if refused != nil {
			free()
			if !fromMailbox {
				if n.refusedBusy != nil {
					n.refusedBusy(handle)
				}
				return nil, nil, fmt.Errorf("%w %q", refused, handle)
			}
			// A MAILBOX DELIVERY WAITS, unspent, and asks again once one
			// of the seat's running turns is released.
			if err := n.waitForSeat(ctx, handle, freed); err != nil {
				return nil, nil, err
			}
			continue
		}
		return n.admitted(ctx, held, fromMailbox), held.release(free, fromMailbox), nil
	}
}

// waitForSeat waits for one of a seat's running turns to be released (freed
// closing), for a mailbox delivery that met the seat full or its conversation
// busy, telling the queue the delivery is still being
// worked on as often as the queue asks ([queue.Progress]). The running turn
// can last longer than the delivery's acknowledgement window, and a window
// that ran out would hand the delivery back and spend one of its deliveries
// on nothing but the wait.
func (n *Node) waitForSeat(ctx context.Context, handle string, freed <-chan struct{}) error {
	var tick <-chan time.Time
	every, signal, ok := queue.Progress(ctx)
	if ok {
		ticker := time.NewTicker(every)
		defer ticker.Stop()
		tick = ticker.C
	}
	for {
		select {
		case <-freed:
			return nil
		case <-ctx.Done():
			return fmt.Errorf("node: admitting a turn on %q: %w", handle, ctx.Err())
		case <-tick:
			if err := signal(); err != nil {
				// LOGGED AND WAITED ON: the next signal may land, and a
				// window that does run out hands the delivery back, which
				// is what would have happened without one.
				n.log.Warn("seat_wait_progress_failed", "handle", handle, "error", err.Error())
			}
		}
	}
}

// admitted is the context an admitted turn runs under: carrying its admission,
// and ended by a drain that cannot wait for it. A turn that did not come from
// the mailbox holds the seat's inbox from here.
func (n *Node) admitted(ctx context.Context, held *admission, fromMailbox bool) context.Context {
	if !fromMailbox {
		n.holdInbox(ctx, held, true)
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
// the seat's turn announced as ended ([Config.SeatTurnEnded]) where another
// turn was refused behind it.
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
				held.node.holdInbox(context.Background(), held, false)
			}
			// ONLY WHERE SOMEBODY WAITS FOR THE SEAT: an admission nobody
			// was refused behind frees nothing anybody asked for, and one
			// released before any turn ran (a resume the engine's gate
			// refused after the node admitted it) would otherwise wake the
			// seat's other waiting resumes for nothing.
			if hook := held.node.cfg.SeatTurnEnded; hook != nil && held.contended.Load() {
				hook(held.handle)
			}
		}
	}
}

// holdInbox takes or lifts one admission's [HoldSeatTurn] reason on the seat's
// inbox. A seat whose
// mailbox this node has not attached has no inbox here to hold. A queue that
// refuses is logged and left: the mailbox's own wait for the seat
// ([Node.admit]) still keeps its turn from starting beside this one, at the
// cost of one in-flight delivery's ack window rather than a delivery.
//
// TAKEN FOR THE ATTACHMENT THE SEAT HOLDS AS THE TURN IS ADMITTED, and lifted
// for that one: a seat released meanwhile ended it, which dropped the hold, so
// the lift lifts nothing, and a hold asked after that release is refused rather
// than carried onto the mailbox this node attaches if it takes the seat back.
func (n *Node) holdInbox(ctx context.Context, held *admission, hold bool) {
	n.mu.Lock()
	if hold {
		for id, h := range n.attached {
			if h == held.handle {
				held.seat, held.inbox = id, n.inboxes[id]
				break
			}
		}
	}
	seat, at := held.seat, held.inbox
	n.mu.Unlock()
	inbox, group := topics.AgentInbox(seat), topics.AgentInboxGroup(seat)
	if inbox == "" || at == 0 {
		return
	}
	ctx = context.WithoutCancel(ctx)
	var err error
	if hold {
		err = n.cfg.Queue.PauseTopic(ctx, inbox, group, at, held.hold)
	} else {
		err = n.cfg.Queue.ResumeTopic(ctx, inbox, group, at, held.hold)
	}
	switch {
	case errors.Is(err, queue.ErrAttachmentGone):
		// Released under the turn: nothing to hold, and nothing held.
		n.log.Debug("seat_turn_hold_attachment_ended", "handle", held.handle)
	case err != nil:
		n.log.Warn("seat_turn_hold_failed", "handle", held.handle, "hold", hold, "error", err.Error())
	}
}

// freeSeat frees the room on its seat a turn held, and wakes every mailbox
// delivery waiting for room on it.
func (n *Node) freeSeat(held *admission) {
	n.mu.Lock()
	if seat := n.running[held.handle]; seat != nil {
		if i := slices.Index(seat.turns, held); i >= 0 {
			seat.turns = slices.Delete(seat.turns, i, i+1)
			close(seat.freed)
			seat.freed = make(chan struct{})
		}
		if len(seat.turns) == 0 {
			delete(n.running, held.handle)
		}
	}
	n.mu.Unlock()
}
