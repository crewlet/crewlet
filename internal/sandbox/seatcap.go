package sandbox

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
)

// A seat holds up to its cap of runs in [Holding] at once, at most one per
// conversation (ADR-0169). The two roads that start a run, a launch and an
// answer's resume, each RESERVE A SLOT under c.mu in the step that records the
// write moving the run into [Holding] ([Coordinator.reservingCount]), refused
// at the cap or on a conversation another of the seat's runs works; the slot
// is the run's count once the write lands and given back when it does not
// (countwrite.go). Every other write leaves [Holding] or moves within it, and a
// takeover counts what the store holds, never refused: those runs exist. See
// the package doc, "Seat counts".

// SeatFull is what an owed answer's resume waits on while its seat holds as
// many runs as its cap, and ConversationBusy what it waits on while another of
// the seat's runs works its conversation: putting it beside them would pass
// the cap, or put two of one conversation's turns in flight at once.
//
// THIS PACKAGE'S OWN CONDITIONS, because the counts they wait on are this
// coordinator's: it signals each itself ([Coordinator.Readmit]), SeatFull the
// moment a slot frees below the cap, ConversationBusy the moment any of the
// seat's runs leaves [Holding] ([Coordinator.seatFreed]).
const (
	SeatFull         Condition = "seat_full"
	ConversationBusy Condition = "conversation_busy"
)

// ErrSeatFull is a run refused because its seat holds as many runs as its cap;
// ErrConversationBusy one refused because another of the seat's runs works its
// conversation. Each is the seat's state, not the run's fault: the turn that
// asked waits for the seat ([SeatFull], [ConversationBusy]).
var (
	ErrSeatFull         = errors.New("sandbox: the seat holds as many runs as its cap")
	ErrConversationBusy = errors.New("sandbox: another of the seat's runs works the conversation")
)

// heldRun is one run this node counts in [Holding], by turn id
// ([Coordinator.heldRuns]): its seat and the conversations it works, its own
// and each one a wake its steps read came from ([CoordinatorOptions.Served]).
type heldRun struct {
	handle        string
	conversations []string
}

// conversationsOf is a run's own conversation and the ones it served, each
// once, "" left out: a run naming none blocks no conversation.
func conversationsOf(own string, served []string) []string {
	var out []string
	for _, key := range append([]string{own}, served...) {
		if key != "" && !slices.Contains(out, key) {
			out = append(out, key)
		}
	}
	return out
}

// SeatCount is what this node counts of one seat's runs: Holding and Awaiting
// as [Coordinator.SeatRuns] answers them in two bools, Reserved the slots a
// launch or a resume has taken and whose write is still out, and Cap the most
// runs the seat holds at once ([CoordinatorOptions.SeatCap]).
type SeatCount struct {
	Holding, Reserved, Awaiting, Cap int
}

// Full reports whether no run may start on the seat now: every slot is held
// or reserved.
func (s SeatCount) Full() bool { return s.Holding+s.Reserved >= s.Cap }

// AtCap reports whether the seat's runs hold every slot, which is when its
// inbox is held ([Coordinator.wantsHoldLocked]) and its mail deferred: a
// reservation not yet landed holds no inbox, as it never did.
func (s SeatCount) AtCap() bool { return s.Holding >= s.Cap }

// capOf is the most runs handle's seat holds at once, one where nothing says.
func (c *Coordinator) capOf(handle string) int {
	if c.seatCap == nil {
		return 1
	}
	return max(c.seatCap(handle), 1)
}

// SeatCount is this node's count of handle's seat, read in one step.
func (c *Coordinator) SeatCount(handle string) SeatCount {
	limit := c.capOf(handle)
	c.mu.Lock()
	defer c.mu.Unlock()
	runs := c.runs[handle]
	return SeatCount{Holding: runs.holding, Reserved: c.reserved[handle], Awaiting: runs.awaiting, Cap: limit}
}

// busyLocked reports whether one of handle's runs this node counts in
// [Holding], or one whose slot is reserved, works conversation. Under mu.
func (c *Coordinator) busyLocked(handle, conversation string) bool {
	if conversation == "" {
		return false
	}
	for _, run := range c.heldRuns {
		if run.handle == handle && slices.Contains(run.conversations, conversation) {
			return true
		}
	}
	for _, writes := range c.writing {
		if writes.handle != handle {
			continue
		}
		for _, w := range writes.out {
			if w.slot && w.conversation == conversation {
				return true
			}
		}
	}
	return false
}

// ConversationBusy reports whether one of handle's runs holding the seat works
// conversation: its own, or one a wake its steps read came from. A step may
// have read a wake since this node last read the seat's rows, so a seat with a
// run holding it and no match in memory is read again, the served lists
// refreshed from it; an error is a store that could not say, never "free".
func (c *Coordinator) ConversationBusy(ctx context.Context, handle, conversation string) (bool, error) {
	if conversation == "" {
		return false, nil
	}
	c.mu.Lock()
	busy, holding := c.busyLocked(handle, conversation), c.runs[handle].holding
	c.mu.Unlock()
	if busy || holding == 0 {
		return busy, nil
	}
	runs, err := c.pending.ListActiveForSeat(ctx, handle)
	if err != nil {
		return false, fmt.Errorf("sandbox: reading which conversations %s's runs work: %w", handle, err)
	}
	served := c.servedOf(ctx, runs)
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, run := range runs {
		if held, ok := c.heldRuns[run.TurnID]; ok && held.handle == handle {
			held.conversations = conversationsOf(run.ConversationKey, served[run.TurnID])
			c.heldRuns[run.TurnID] = held
		}
	}
	return c.busyLocked(handle, conversation), nil
}

// servedOf is the conversations each [Holding] run of runs served, by turn id
// ([CoordinatorOptions.Served]). Outside mu: the engine opens each wake's seal.
func (c *Coordinator) servedOf(ctx context.Context, runs []PendingRun) map[string][]string {
	out := map[string][]string{}
	if c.served == nil {
		return out
	}
	for _, run := range runs {
		if slices.Contains(Holding, run.Status) {
			out[run.TurnID] = c.served(ctx, run)
		}
	}
	return out
}

// seatRefusalLocked is why handle's seat starts no run of conversation now,
// nil where it does: the conversation worked by another of its runs, named
// first since only that run's leaving lifts it, or every slot held or reserved. The refusal names what to wait on and wraps
// [ErrSeatFull] or [ErrConversationBusy]. Under mu.
func (c *Coordinator) seatRefusalLocked(handle, conversation string) *TurnRefused {
	limit := c.capOf(handle)
	if c.busyLocked(handle, conversation) {
		reason := "another of the seat's runs works the conversation"
		return &TurnRefused{Refusal: Refusal{Condition: ConversationBusy, Reason: reason},
			Cause: fmt.Errorf("%w: %s", ErrConversationBusy, reason)}
	}
	if c.runs[handle].holding+c.reserved[handle] >= limit {
		reason := fmt.Sprintf("the seat holds as many runs as its cap of %d", limit)
		return &TurnRefused{Refusal: Refusal{Condition: SeatFull, Reason: reason},
			Cause: fmt.Errorf("%w: %s", ErrSeatFull, reason)}
	}
	return nil
}

// seatRefusal is [Coordinator.seatRefusalLocked] for a caller not holding mu:
// an owed resume's early check, before its admission is asked. The resume's
// claim reserves again under the lock, which is the check that holds.
func (c *Coordinator) seatRefusal(handle, conversation string) (Refusal, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if refused := c.seatRefusalLocked(handle, conversation); refused != nil {
		return refused.Refusal, true
	}
	return Refusal{}, false
}

// reservingCount is [Coordinator.writingCount] for a write that moves run INTO
// [Holding] from outside it (a launch's row, an answer's claim): in the same
// step it reserves the seat's slot, or refuses the write with the
// [*TurnRefused] that names what to wait on, wrapping [ErrSeatFull] or
// [ErrConversationBusy]. Nothing is recorded on a refusal.
func (c *Coordinator) reservingCount(run PendingRun, from, to string) (*countWrite, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if countOf(to).holding > countOf(from).holding && !c.slotOfLocked(run.TurnID) {
		if refused := c.seatRefusalLocked(run.AgentHandle, run.ConversationKey); refused != nil {
			return nil, refused
		}
	}
	return c.writingCountLocked(run, from, to), nil
}

// slotOfLocked reports whether turn's run holds a slot of its seat already,
// counted in [Holding] or reserved by a write still out: a relaunch of it
// takes no second slot, and is never refused for the one it holds. Under mu.
func (c *Coordinator) slotOfLocked(turn string) bool {
	if _, held := c.heldRuns[turn]; held {
		return true
	}
	writes := c.writing[turn]
	return writes != nil && slices.ContainsFunc(writes.out, func(w *countWrite) bool { return w.slot })
}

// heldFromListingLocked brings handle's [Coordinator.heldRuns] into line with a
// listing of its runs (a recount): each run with no write out is held where
// the listing says, with the conversations served gives it; a run with a write
// out keeps what its writes' moves leave it ([Coordinator.wroteCountLocked]).
// Under mu.
func (c *Coordinator) heldFromListingLocked(handle string, runs []PendingRun, served map[string][]string) {
	maps.DeleteFunc(c.heldRuns, func(turn string, run heldRun) bool {
		return run.handle == handle && c.writing[turn] == nil
	})
	for _, run := range runs {
		if c.writing[run.TurnID] != nil || !slices.Contains(Holding, run.Status) {
			continue
		}
		c.heldRuns[run.TurnID] = heldRun{handle: handle,
			conversations: conversationsOf(run.ConversationKey, served[run.TurnID])}
	}
}

// heldMovedLocked records where the moves so far leave the run of writes, by
// turn: held, with its conversations, while it counts in [Holding], else not.
// Under mu, from [Coordinator.wroteCountLocked].
func (c *Coordinator) heldMovedLocked(turn string, writes *runWrites) {
	if writes.counted.holding == 0 || writes.handle == "" {
		delete(c.heldRuns, turn)
		return
	}
	c.heldRuns[turn] = heldRun{handle: writes.handle,
		conversations: conversationsOf(writes.conversation, writes.served)}
}

// HoldForConversation is [Coordinator.HoldSeat] for a delivery screened out
// because one of the seat's runs works its conversation, below the seat's cap:
// the inbox is held while that run holds the seat (waiting), so the delivery
// waits at the head of the inbox at no cost rather than spending a delivery
// per redelivery, and is released the moment no run of the seat works the
// conversation ([Coordinator.seatFreed]). A conversation not busy in memory
// takes no hold: the delivery's deferral alone stands, and its next screening
// reads the seat again.
func (c *Coordinator) HoldForConversation(ctx context.Context, handle, conversation string) {
	c.mu.Lock()
	if c.busyLocked(handle, conversation) {
		if c.waiting[handle] == nil {
			c.waiting[handle] = map[string]struct{}{}
		}
		c.waiting[handle][conversation] = struct{}{}
	}
	c.mu.Unlock()
	c.reconcileHold(ctx, handle)
}

// pruneWaitingLocked drops every conversation handle's inbox is held for that
// no run of the seat works any longer. Under mu.
func (c *Coordinator) pruneWaitingLocked(handle string) {
	for conversation := range c.waiting[handle] {
		if !c.busyLocked(handle, conversation) {
			delete(c.waiting[handle], conversation)
		}
	}
	if len(c.waiting[handle]) == 0 {
		delete(c.waiting, handle)
	}
}

// HeldBeside is how many of handle's runs other than turn's hold the seat or
// have a slot reserved: the runs a launch for turn starts beside, whose spend
// is charged only at their collections (internal/engine's launch floor).
func (c *Coordinator) HeldBeside(handle, turn string) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	n := 0
	for t, run := range c.heldRuns {
		if run.handle == handle && t != turn {
			n++
		}
	}
	for t, writes := range c.writing {
		if writes.handle != handle || t == turn {
			continue
		}
		if _, held := c.heldRuns[t]; !held && slices.ContainsFunc(writes.out, func(w *countWrite) bool { return w.slot }) {
			n++
		}
	}
	return n
}
