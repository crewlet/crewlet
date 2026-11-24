package sandbox

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"

	"github.com/crewlet/crewlet/internal/events/types"
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
// launch or a resume has taken and whose write is still out, Waiting the runs
// parked on another run whose keys stay held ([Coordinator.waitingRuns]), and
// Cap the most runs the seat holds at once ([CoordinatorOptions.SeatCap]).
type SeatCount struct {
	Holding, Reserved, Awaiting, Waiting, Cap int
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
	waiting := 0
	for _, run := range c.waitingRuns {
		if run.handle == handle {
			waiting++
		}
	}
	return SeatCount{Holding: runs.holding, Reserved: c.reserved[handle], Awaiting: runs.awaiting,
		Waiting: waiting, Cap: limit}
}

// busyLocked reports whether one of handle's runs this node counts in
// [Holding], one whose slot is reserved, or one parked on another run, works
// conversation. Under mu.
func (c *Coordinator) busyLocked(handle, conversation string) bool {
	return c.busyBesideLocked(handle, conversation, "")
}

// busyBesideLocked is [Coordinator.busyLocked] apart from turn's own wait: a
// run parked on another resumes into the conversations it kept busy. Under mu.
func (c *Coordinator) busyBesideLocked(handle, conversation, turn string) bool {
	if conversation == "" {
		return false
	}
	for _, run := range c.heldRuns {
		if run.handle == handle && slices.Contains(run.conversations, conversation) {
			return true
		}
	}
	for waiting, run := range c.waitingRuns {
		if waiting != turn && run.handle == handle && slices.Contains(run.conversations, conversation) {
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
		if slices.Contains(Holding, run.Status) || waitsOnARun(run) {
			out[run.TurnID] = c.served(ctx, run)
		}
	}
	return out
}

// waitsOnARun reports whether run is parked by `wait_for_run` or on its own
// calls ([PendingRun.EngineAnswers]), whose keys stay held while it waits,
// answered or not, until it resumes or ends ([Coordinator.waitingRuns]).
func waitsOnARun(run PendingRun) bool {
	return run.EngineAnswers() && slices.Contains(Active, run.Status) && !slices.Contains(Holding, run.Status)
}

// seatRefusalLocked is why handle's seat starts turn's run of conversation now,
// nil where it does: the conversation worked by another of its runs, named
// first since only that run's leaving lifts it, or every slot held or reserved. The refusal names what to wait on and wraps
// [ErrSeatFull] or [ErrConversationBusy]. Under mu.
func (c *Coordinator) seatRefusalLocked(turn, handle, conversation string) *TurnRefused {
	limit := c.capOf(handle)
	if c.busyBesideLocked(handle, conversation, turn) {
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
func (c *Coordinator) seatRefusal(turn, handle, conversation string) (Refusal, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if refused := c.seatRefusalLocked(turn, handle, conversation); refused != nil {
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
		if refused := c.seatRefusalLocked(run.TurnID, run.AgentHandle, run.ConversationKey); refused != nil {
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
	waited := map[string]bool{}
	for turn, run := range c.waitingRuns {
		waited[turn] = run.handle == handle
	}
	for _, set := range []map[string]heldRun{c.heldRuns, c.waitingRuns} {
		maps.DeleteFunc(set, func(turn string, run heldRun) bool {
			gone := run.handle == handle && c.writing[turn] == nil
			if gone {
				c.freeLocked(turn, run)
			}
			return gone
		})
	}
	for _, run := range runs {
		if c.writing[run.TurnID] != nil {
			continue
		}
		held := heldRun{handle: handle, conversations: conversationsOf(run.ConversationKey, served[run.TurnID])}
		switch {
		case slices.Contains(Holding, run.Status):
			c.heldRuns[run.TurnID] = held
			if waited[run.TurnID] {
				c.resumedLocked(run.TurnID, held)
			}
		case waitsOnARun(run):
			c.waitingRuns[run.TurnID] = held
		}
	}
}

// heldMovedLocked records where the moves so far leave the run of writes, by
// turn: held, with its conversations, while it counts in [Holding], else not.
// Under mu, from [Coordinator.wroteCountLocked].
//
// A RUN PARKED ON ANOTHER RUN is a turn still in flight on its conversation:
// it keeps its keys ([Coordinator.waitingRuns]) and holds no slot, so its mail
// waits aside behind it rather than starting a second turn on the work it
// waits to go on with. Resuming, it hands that mail back to be screened
// against it running, where a follow-up joins it; ending parked, it frees its
// keys (ADR-0176). A run parked on a question frees its keys at once: the
// next message there is the answer its park routes.
func (c *Coordinator) heldMovedLocked(turn string, writes *runWrites) {
	if writes.counted.holding == 0 || writes.handle == "" {
		run, held := c.heldRuns[turn]
		if !held {
			run, held = c.waitingRuns[turn]
		}
		delete(c.heldRuns, turn)
		switch {
		case !held:
		case writes.waits && !writes.gone:
			c.waitingRuns[turn] = run
		default:
			c.freeLocked(turn, run)
			delete(c.waitingRuns, turn)
		}
		return
	}
	held := heldRun{handle: writes.handle, conversations: conversationsOf(writes.conversation, writes.served)}
	if _, waited := c.waitingRuns[turn]; waited {
		delete(c.waitingRuns, turn)
		c.resumedLocked(turn, held)
	}
	c.heldRuns[turn] = held
}

// A delivery of a conversation one of the seat's runs works, or related to one
// of its runs, waits aside on the inbox under a key (internal/queue's
// OutcomeAside, ADR-0169): the conversation, or [RunKey] of the run. The run
// leaving [Holding] records its keys freed, and the transition readmits each
// no run of the seat still holds ([Coordinator.readmitFreed]); a readmit with
// nothing aside hands back nothing, so nothing tracks what was set aside.

// freedKey is one key a run leaving [Holding] freed, and the run, for the line;
// resumed is one a run back from a wait hands back while it still holds it, so
// what waited aside behind it is screened against it running.
type freedKey struct {
	key, turn string
	resumed   bool
}

// runKeyPrefix begins [RunKey]: a slash, which no conversation key a message
// derives holds before its source's colon.
const runKeyPrefix = "run/"

// RunKey is the key a delivery related to turn's run, and not of a busy
// conversation, waits aside under: freed when that run leaves [Holding].
func RunKey(turn string) string { return runKeyPrefix + turn }

// keyBusyLocked reports whether one of handle's runs still holds key: the run
// [RunKey] names in [Holding] or parked on another run, or a conversation
// [Coordinator.busyLocked] finds busy. Under mu.
func (c *Coordinator) keyBusyLocked(handle, key string) bool {
	if turn, ok := strings.CutPrefix(key, runKeyPrefix); ok {
		run, held := c.heldRuns[turn]
		if !held {
			run, held = c.waitingRuns[turn]
		}
		return held && run.handle == handle
	}
	return c.busyLocked(handle, key)
}

// KeyBusy reports whether one of handle's runs still holds key, as a delivery
// about to wait aside under it asks ([RunKey], [Coordinator.ConversationBusy]);
// an error is a store that could not say.
func (c *Coordinator) KeyBusy(ctx context.Context, handle, key string) (bool, error) {
	if _, ok := strings.CutPrefix(key, runKeyPrefix); ok {
		c.mu.Lock()
		defer c.mu.Unlock()
		return c.keyBusyLocked(handle, key), nil
	}
	return c.ConversationBusy(ctx, handle, key)
}

// freeLocked records the keys run, leaving [Holding] on handle's seat, frees:
// each conversation it works and its own [RunKey]. Under mu.
func (c *Coordinator) freeLocked(turn string, run heldRun) {
	for _, key := range append(slices.Clone(run.conversations), RunKey(turn)) {
		c.freed[run.handle] = append(c.freed[run.handle], freedKey{key: key, turn: turn})
	}
}

// resumedLocked records the keys run, back in [Holding] from a wait on another
// run, hands back though it holds them ([freedKey.resumed]). Under mu.
func (c *Coordinator) resumedLocked(turn string, run heldRun) {
	for _, key := range append(slices.Clone(run.conversations), RunKey(turn)) {
		c.freed[run.handle] = append(c.freed[run.handle], freedKey{key: key, turn: turn, resumed: true})
	}
}

// readmitFreed hands back on handle's inbox the mail set aside under each key
// its runs freed that no run of the seat still holds ([SeatHold]): at the
// transition that freed it, as the inbox hold follows it. The keys still held
// are let go too; the run that holds them frees them as it leaves. A readmit
// that handed something back, or a seat no run holds, lifts the hold
// [Coordinator.HoldForAside] took.
func (c *Coordinator) readmitFreed(ctx context.Context, handle string) {
	if c.hold == nil || handle == "" {
		return
	}
	ctx, release := detach(ctx)
	defer release()
	c.mu.Lock()
	var free []freedKey
	for _, f := range c.freed[handle] {
		if (f.resumed || !c.keyBusyLocked(handle, f.key)) &&
			!slices.ContainsFunc(free, func(g freedKey) bool { return g.key == f.key }) {
			free = append(free, f)
		}
	}
	delete(c.freed, handle)
	c.mu.Unlock()
	readmitted := false
	for _, f := range free {
		n, err := c.hold.Readmit(ctx, handle, f.key, f.turn)
		if err != nil {
			log.WarnContext(ctx, "sandbox_aside_readmit_failed", "agent", handle, "key", f.key,
				"turn_id", f.turn, "error", err.Error(), "detail", "what waited aside on the run "+
					"stays there until the seat's release hands it back")
			continue
		}
		readmitted = readmitted || n > 0
	}
	c.mu.Lock()
	unheld := readmitted || c.runs[handle].holding == 0
	if unheld {
		delete(c.asideFull, handle)
	}
	c.mu.Unlock()
}

// readmitFoldIn hands back the wake the fold-in ev answered decides, which waits
// aside under its run's key or its conversation's while one of the seat's runs
// works that ([RunKey]): the inbox's screen then reads the answer at once, an
// approval filing it into the running work, a refusal setting it aside again
// until the run leaves. A wake not aside is handed back nothing.
func (c *Coordinator) readmitFoldIn(ctx context.Context, ev types.ExecutorAnswerGiven) {
	if c.hold == nil || ev.AgentHandle == "" {
		return
	}
	run, found, err := c.pending.Get(ctx, ev.TurnID)
	if err != nil || !found {
		log.DebugContext(ctx, "sandbox_foldin_readmit_unread", "agent", ev.AgentHandle, "turn_id", ev.TurnID,
			"found", found, "error", fmt.Sprint(err))
		return
	}
	keys := []string{RunKey(ev.TurnID)}
	for _, f := range run.FoldIns {
		if f.ID == ev.FoldIn && f.Conversation != "" {
			keys = append(keys, f.Conversation)
		}
	}
	for _, key := range keys {
		if _, err := c.hold.Readmit(ctx, ev.AgentHandle, key, ev.TurnID); err != nil {
			log.WarnContext(ctx, "sandbox_aside_readmit_failed", "agent", ev.AgentHandle, "key", key,
				"turn_id", ev.TurnID, "error", err.Error(), "detail", "the answered fold-in's wake is read "+
					"when its run leaves")
		}
	}
}

// HoldForAside holds handle's inbox for a delivery that would wait aside
// while the inbox holds as many aside as it keeps (internal/queue's MaxAside):
// the mail behind it waits on the broker in order, as at the cap, until a
// readmit hands some back or no run of the seat holds
// ([Coordinator.readmitFreed]). The delivery's deferral stands meanwhile.
func (c *Coordinator) HoldForAside(ctx context.Context, handle string) {
	c.mu.Lock()
	if c.runs[handle].holding > 0 {
		c.asideFull[handle] = true
	}
	c.mu.Unlock()
	c.reconcileHold(ctx, handle)
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
