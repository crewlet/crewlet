package sandbox

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sync"
	"testing"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/events"
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
// cap an inbox holding as many aside as it keeps is held
// ([Coordinator.HoldForAside]) until a readmit hands some back, not until a run
// leaves with nothing aside. At the default cap of one, one run holds it.
//
// Mutation: hold at the first run and one run holds it; lift the aside hold on
// any run leaving and "b" leaving opens it.
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

	holds.setAside("swe", "a", 1)
	c.HoldForAside(t.Context(), "swe")
	if !holds.holding("swe") {
		t.Fatal("an inbox holding as many aside as it keeps was left open")
	}
	if err := enter(c, "t3", "b"); err != nil {
		t.Fatal(err)
	}
	leave(c, "t3", "b")
	if !holds.holding("swe") {
		t.Fatal("a run leaving with nothing aside lifted the hold on a full inbox")
	}
	leave(c, "t1", "a")
	if holds.holding("swe") {
		t.Fatal("the inbox stayed held after a readmit handed back what waited aside")
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

// A RUN LEAVING READMITS WHAT WAITED ON IT, AT THE TRANSITION THAT FREES IT, AND
// NEVER A CONVERSATION ANOTHER OF THE SEAT'S RUNS STILL HOLDS. A run's own key
// and each conversation it works are freed as it leaves, here by the recount a
// takeover's recovery makes, reading the store's word that the run no longer
// holds; a conversation another run served stays aside until that run leaves.
//
// Mutation: readmit every freed key whatever still holds it, and "a" is
// readmitted while t3 serves it; free no key on a recount, and t1 parking
// readmits nothing.
func TestARunLeavingReadmitsWhatWaitedOnItAndNeverWhatAnotherHolds(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	rig := newCoordRig(t)
	rig.withCap(3)
	holds := rig.withHold()
	c := rig.coordinator
	c.served = func(_ context.Context, run PendingRun) []string {
		if run.TurnID == "t3" {
			return []string{"a"}
		}
		return nil
	}
	rig.launchWith("t1", func(run *PendingRun) { run.ConversationKey = "a" })
	rig.launchWith("t3", func(run *PendingRun) { run.ConversationKey = "c" })
	for _, turn := range []string{"t1", "t3"} {
		if err := c.OnStarted(ctx, types.ExecutorLaunched{AgentHandle: "swe", TurnID: turn}); err != nil {
			t.Fatalf("OnStarted(%s): %v", turn, err)
		}
	}
	if got := holds.readmitted(); len(got) != 0 {
		t.Fatalf("runs entering the seat readmitted %v", got)
	}

	if err := rig.pending.SetStatus(ctx, "t1", StatusAwaiting, rigLease); err != nil {
		t.Fatalf("SetStatus(t1): %v", err)
	}
	c.syncSeat(ctx, "swe")
	if got := holds.readmitted(); !slices.Equal(got, []string{"swe/" + RunKey("t1")}) {
		t.Fatalf("t1 leaving readmitted %v, want its own key alone: t3 still serves a", got)
	}

	if err := rig.pending.SetStatus(ctx, "t3", StatusAwaiting, rigLease); err != nil {
		t.Fatalf("SetStatus(t3): %v", err)
	}
	c.syncSeat(ctx, "swe")
	got := holds.readmitted()
	slices.Sort(got)
	if want := []string{"swe/a", "swe/c", "swe/" + RunKey("t3")}; !slices.Equal(got, want) {
		t.Fatalf("t3 leaving readmitted %v, want %v", got, want)
	}
}

// AN ANSWERED FOLD-IN HANDS BACK THE WAKE IT DECIDES. A request another writer
// left on a busy seat waits aside on its run (internal/engine's beside) while
// the fold-in put to the run's approvers waits; their answer reaches the seat
// on its control topic, and the wake is readmitted under the run's key and its
// conversation's, so the screen files it on an approval or sets it aside again
// on a refusal, now rather than when the run leaves.
//
// Mutation: readmit nothing on a fold-in's answer, and nothing is readmitted.
func TestAnAnsweredFoldInReadmitsTheWakeItDecides(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	rig := newCoordRig(t)
	holds := rig.withHold()
	run := rig.launchWith("t1", func(run *PendingRun) { run.ConversationKey = "a" })
	f := FoldIn{ID: uuid.NewString(), Launch: run.LaunchID, Wake: uuid.NewString(), Asker: "ceo",
		Conversation: "b", Words: "and fix the logo", PutTo: []string{"lead"}}
	if _, err := rig.pending.AskFoldIn(ctx, "t1", f); err != nil {
		t.Fatalf("AskFoldIn: %v", err)
	}
	_ = holds.readmitted()
	answered := events.New(types.ExecutorAnswerGiven{AgentHandle: "swe", TurnID: "t1", FoldIn: f.ID},
		events.TraceContext{})
	if err := rig.coordinator.OnEvent(ctx, answered); err != nil {
		t.Fatalf("OnEvent: %v", err)
	}
	got := holds.readmitted()
	slices.Sort(got)
	if want := []string{"swe/b", "swe/" + RunKey("t1")}; !slices.Equal(got, want) {
		t.Fatalf("an answered fold-in readmitted %v, want %v", got, want)
	}
}

// A RUN WAITING ON ANOTHER KEEPS ITS KEYS AND NO SLOT UNTIL IT RESUMES OR ENDS
// (ADR-0176). Parked by `wait_for_run`, answered or not, a run is a turn still
// in flight on its conversation: it frees no key, as a run parked on a question
// does, so a second run of that conversation is refused and its mail waits
// aside, while it holds no slot of the seat. Its own resume is never refused by
// its wait, and hands back what waited aside behind it to be screened against
// it running; its ending while parked frees and readmits what it kept. A
// recount from the store rebuilds the waiting set as the writes left it.
//
// Mutation: free a waiting run's keys at its park, as a question's are, and
// they are readmitted at once; readmit nothing at its resume, and the mail
// waits for its end.
func TestARunWaitingOnAnotherKeepsItsKeysUntilItResumesOrEnds(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	rig := newCoordRig(t)
	rig.withCap(3)
	holds := rig.withHold()
	c := rig.coordinator
	waiter := PendingRun{TurnID: "t1", AgentHandle: "swe", ConversationKey: "a",
		WaitsFor: &RunWait{Turn: "t2", Seat: "swe", Reason: "its branch first"}}
	keys := []string{"swe/a", "swe/" + RunKey("t1")}
	readmitted := func() []string {
		got := holds.readmitted()
		slices.Sort(got)
		return got
	}
	if err := enter(c, "t1", "a"); err != nil {
		t.Fatal(err)
	}
	land(c, c.writingCount(waiter, StatusRunning, StatusAwaiting))
	land(c, c.writingCount(waiter, StatusAwaiting, StatusAnswered))
	if got := readmitted(); len(got) != 0 {
		t.Fatalf("a run parking on another, and answered, readmitted %v", got)
	}
	for _, key := range []string{"a", RunKey("t1")} {
		if busy, err := c.KeyBusy(ctx, "swe", key); err != nil || !busy {
			t.Errorf("the waiting run's key %s is busy %v (%v), want held", key, busy, err)
		}
	}
	if count := c.SeatCount("swe"); count.Holding != 0 || count.Reserved != 0 || count.Waiting != 1 {
		t.Errorf("the seat counts %+v, want one run waiting and no slot held", count)
	}
	if err := enter(c, "t9", "a"); !errors.Is(err, ErrConversationBusy) {
		t.Errorf("a second run of the waiting run's conversation entered: %v", err)
	}
	resumed, err := c.reservingCount(waiter, StatusAnswered, StatusResumed)
	if err != nil {
		t.Fatalf("the waiting run's own resume was refused: %v", err)
	}
	land(c, resumed)
	if got := readmitted(); !slices.Equal(got, keys) {
		t.Fatalf("the waiting run resuming readmitted %v, want %v to be screened against it running", got, keys)
	}
	land(c, c.writingCount(waiter, StatusRunning, StatusAwaiting))
	if got := readmitted(); len(got) != 0 {
		t.Fatalf("parking on a run again readmitted %v", got)
	}
	land(c, c.writingCount(waiter, StatusAwaiting, ""))
	if got := readmitted(); !slices.Equal(got, keys) {
		t.Fatalf("the waiting run ending readmitted %v, want %v", got, keys)
	}

	job := rig.launchWith("t3", func(run *PendingRun) { run.ConversationKey = "c" }).LaunchID
	ask := Ask{Question: RunWaitQuestion("its branch first"), Wait: &RunWait{Turn: "t1", Seat: "swe",
		Reason: "its branch first"}}
	if got, err := rig.pending.RecordAsk(ctx, "t3", job, ask); err != nil || got != AskRecorded {
		t.Fatalf("RecordAsk of t3's wait = %v, %v", got, err)
	}
	if err := rig.pending.SetStatus(ctx, "t3", StatusAwaiting, rigLease); err != nil {
		t.Fatalf("SetStatus(t3): %v", err)
	}
	c.syncSeat(ctx, "swe")
	if busy, err := c.KeyBusy(ctx, "swe", "c"); err != nil || !busy {
		t.Errorf("after a recount the waiting run's conversation is busy %v (%v), want held", busy, err)
	}
}
