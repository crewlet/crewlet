package sandbox

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"

	"github.com/crewlet/crewlet/internal/events/types"
)

// withCap gives the rig's coordinator a cap of n runs on every seat.
func (r *coordRig) withCap(n int) {
	r.coordinator.seatCap = func(string) int { return n }
}

// land takes the answer to w as landed, as the store's would be taken.
func land(c *Coordinator, w *countWrite) {
	c.mu.Lock()
	moved := c.wroteCountLocked(w, true)
	c.mu.Unlock()
	moved()
}

// enter moves turn's run of conversation into [Holding] on swe's seat through
// the reservation, or reports why it was refused.
func enter(c *Coordinator, turn, conversation string) error {
	w, err := c.reservingCount(PendingRun{TurnID: turn, AgentHandle: "swe", ConversationKey: conversation},
		"", StatusLaunching)
	if err != nil {
		return err
	}
	land(c, w)
	return nil
}

// leave moves turn's running run of conversation out of [Holding], as its
// ending's delete would.
func leave(c *Coordinator, turn, conversation string) {
	land(c, c.writingCount(PendingRun{TurnID: turn, AgentHandle: "swe", ConversationKey: conversation},
		StatusRunning, ""))
}

// A SEAT ADMITS EXACTLY ITS CAP OF RUNS AT ONCE, whichever road each takes into
// [Holding] and however they race (ADR-0169). Launches and answers' claims race
// for the slots under -race; exactly the cap are let through, the rest refused
// naming [SeatFull]; a write that does not land gives its slot back, and a run
// leaving frees one. At the default cap of one a second run is refused, as the
// seat's one run always refused it.
//
// Mutation: check the cap outside the lock and more than the cap pass the race;
// leave the reserved slots out of the check and a launch and a claim both pass
// at cap−1; never give a refused write's slot back and the seat stays full.
func TestASeatAdmitsExactlyItsCapOfRunsAtOnce(t *testing.T) {
	t.Parallel()
	const limit = 3
	rig := newCoordRig(t)
	rig.withCap(limit)
	c := rig.coordinator

	var (
		wg      sync.WaitGroup
		mu      sync.Mutex
		writes  []*countWrite
		refused int
	)
	for i := range limit + 2 {
		from, to := "", StatusLaunching
		if i%2 == 1 {
			from, to = StatusAnswered, StatusResumed
		}
		wg.Go(func() {
			w, err := c.reservingCount(PendingRun{TurnID: fmt.Sprintf("t%d", i), AgentHandle: "swe",
				ConversationKey: fmt.Sprintf("c%d", i)}, from, to)
			mu.Lock()
			defer mu.Unlock()
			switch {
			case err == nil:
				writes = append(writes, w)
			case errors.Is(err, ErrSeatFull):
				refused++
			default:
				t.Errorf("a reservation was refused for %v, want only ErrSeatFull", err)
			}
		})
	}
	wg.Wait()
	if len(writes) != limit || refused != 2 {
		t.Fatalf("%d of %d racing starts were let through and %d refused, want exactly the cap of %d",
			len(writes), limit+2, refused, limit)
	}
	if got := c.SeatCount("swe"); got.Reserved != limit || !got.Full() || got.AtCap() {
		t.Fatalf("count = %+v, want every slot reserved and none yet held", got)
	}

	// ONE WRITE DOES NOT LAND: its slot frees, and another start takes it.
	c.settleWrite(writes[0])
	for _, w := range writes[1:] {
		land(c, w)
	}
	if got := c.SeatCount("swe"); got.Holding != limit-1 || got.Reserved != 0 || got.Full() {
		t.Fatalf("count = %+v after one write failed, want %d held and a slot free", got, limit-1)
	}
	if err := enter(c, "t9", "c9"); err != nil {
		t.Fatalf("a start into the slot a failed write gave back = %v, want admitted", err)
	}
	if err := enter(c, "t10", "c10"); !errors.Is(err, ErrSeatFull) {
		t.Fatalf("a start on a seat at its cap = %v, want ErrSeatFull", err)
	}
	leave(c, "t9", "c9")
	if err := enter(c, "t10", "c10"); err != nil {
		t.Fatalf("a start once a run left = %v, want admitted", err)
	}

	// THE DEFAULT CAP IS ONE.
	one := newCoordRig(t)
	if err := enter(one.coordinator, "a", "x"); err != nil {
		t.Fatalf("the first run on a seat naming no cap = %v, want admitted", err)
	}
	if err := enter(one.coordinator, "b", "y"); !errors.Is(err, ErrSeatFull) {
		t.Fatalf("a second run on a seat naming no cap = %v, want ErrSeatFull", err)
	}
}

// A CONVERSATION A RUN WORKS STARTS NO SECOND RUN, WHATEVER THE CAP: its own
// conversation, one whose start is still reserved, and one a wake its steps
// read came from ([CoordinatorOptions.Served]), which the screening also asks
// of a delivery ([Coordinator.ConversationBusy]). Another conversation starts.
//
// Mutation: drop the conversation check from the reservation and the second
// run of "a" starts; leave reserved starts out of the busy set and "r" starts
// twice; drop the served conversations and "c" starts beside the run that
// read its wake.
func TestAConversationARunWorksStartsNoSecondRun(t *testing.T) {
	t.Parallel()
	rig := newCoordRig(t)
	rig.withCap(8)
	c := rig.coordinator

	if err := enter(c, "t1", "a"); err != nil {
		t.Fatalf("the first run of a = %v", err)
	}
	if err := enter(c, "t2", "a"); !errors.Is(err, ErrConversationBusy) {
		t.Fatalf("a second run of a conversation a run works = %v, want ErrConversationBusy", err)
	}
	if err := enter(c, "t3", "b"); err != nil {
		t.Fatalf("a run of another conversation = %v, want admitted", err)
	}
	w, err := c.reservingCount(PendingRun{TurnID: "t4", AgentHandle: "swe", ConversationKey: "r"},
		StatusAnswered, StatusResumed)
	if err != nil {
		t.Fatalf("a claim of r = %v", err)
	}
	if err := enter(c, "t5", "r"); !errors.Is(err, ErrConversationBusy) {
		t.Fatalf("a run of a conversation whose claim is still out = %v, want ErrConversationBusy", err)
	}
	land(c, w)

	// A SERVED CONVERSATION, learned from the seat's rows.
	c.served = func(_ context.Context, run PendingRun) []string {
		if run.TurnID == "t6" {
			return []string{"c"}
		}
		return nil
	}
	rig.launchWith("t6", func(run *PendingRun) { run.ConversationKey = "z" })
	if err := c.OnStarted(t.Context(), types.ExecutorLaunched{AgentHandle: "swe", TurnID: "t6"}); err != nil {
		t.Fatalf("OnStarted: %v", err)
	}
	busy, err := c.ConversationBusy(t.Context(), "swe", "c")
	if err != nil || !busy {
		t.Fatalf("ConversationBusy(c) = %v, %v; want busy: a run read a wake of c", busy, err)
	}
	if err := enter(c, "t7", "c"); !errors.Is(err, ErrConversationBusy) {
		t.Fatalf("a run of a conversation another run served = %v, want ErrConversationBusy", err)
	}
	if busy, err := c.ConversationBusy(t.Context(), "swe", "d"); err != nil || busy {
		t.Fatalf("ConversationBusy(d) = %v, %v; want free", busy, err)
	}
}

// THE INBOX IS HELD AT THE CAP AND RELEASED BELOW IT. At a cap of two one run
// leaves the inbox open and two hold it; one leaving opens it again. Below the
// cap a delivery of a conversation a run works holds it
// ([Coordinator.HoldForConversation]) until that run leaves, not until any
// run does. At the default cap of one, one run holds it, as it always did.
//
// Mutation: hold at the first run and one run holds it; release a
// conversation's hold when any run leaves and "b" leaving opens it.
func TestTheInboxIsHeldAtTheCapAndReleasedBelowIt(t *testing.T) {
	t.Parallel()
	rig := newCoordRig(t)
	rig.withCap(2)
	holds := rig.withHold()
	c := rig.coordinator

	if err := enter(c, "t1", "a"); err != nil {
		t.Fatal(err)
	}
	if holds.holding("swe") {
		t.Fatal("one run of a seat capped at two held its inbox")
	}
	if err := enter(c, "t2", "b"); err != nil {
		t.Fatal(err)
	}
	if !holds.holding("swe") {
		t.Fatal("a seat whose runs hold both slots of its cap left its inbox open")
	}
	leave(c, "t2", "b")
	if holds.holding("swe") {
		t.Fatal("the inbox stayed held after a run left a seat at its cap")
	}

	c.HoldForConversation(t.Context(), "swe", "a")
	if !holds.holding("swe") {
		t.Fatal("a delivery waiting on a conversation a run works did not hold the inbox")
	}
	if err := enter(c, "t3", "b"); err != nil {
		t.Fatal(err)
	}
	leave(c, "t3", "b")
	if !holds.holding("swe") {
		t.Fatal("another run leaving released the hold a delivery waits under on conversation a")
	}
	leave(c, "t1", "a")
	if holds.holding("swe") {
		t.Fatal("the inbox stayed held after the run working the waited-on conversation left")
	}

	one := newCoordRig(t)
	oneHolds := one.withHold()
	if err := enter(one.coordinator, "x", "a"); err != nil {
		t.Fatal(err)
	}
	if !oneHolds.holding("swe") {
		t.Fatal("at the default cap of one a run left its seat's inbox open")
	}
}
