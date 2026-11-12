package sandbox

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/executorkeep"
	"github.com/crewlet/crewlet/internal/objstore"
)

// A RETIRED SEAT'S RUN IS FINISHED BY THE RETIREMENT, ITS TURN'S END RECORDED.
// The retirement holds the seat's lease and a budget of its own, and the seat
// host holds neither the seat nor a retry for it any more: a loss's ending
// handed to the seat host's retry was dropped with the seat, the run's record
// stayed in the store for good — on the executors board, in a person's
// decisions — and its turn's end never reached the executor log.
//
// Mutation: finish a retirement's ending as a frame's is, handing what waits on
// the log to the seat host's retry, and the record is left and no end recorded.
func TestARetiredSeatsRunIsFinishedWithItsTurnsEnd(t *testing.T) {
	t.Parallel()
	inBubble(t, func(t *testing.T) {
		rig, writes, _ := keepingRig(t)
		rig.launchKept(t, "t1")
		if err := rig.coordinator.RetireSeat(t.Context(), sweSeat(), "swe", rigLease.Owner, rigLease.Epoch); err != nil {
			t.Fatalf("RetireSeat: %v", err)
		}
		if _, found, err := rig.pending.Get(t.Context(), "t1"); err != nil || found {
			t.Fatalf("the retired seat's run is still recorded (found %v, %v)", found, err)
		}
		ends := writes.endedRecords()
		if len(ends) != 1 || ends[0].Ending.Outcome != executorkeep.OutcomeLost {
			t.Fatalf("recorded %+v, want the retired run's turn ended as lost", ends)
		}
	})
}

// A RELAUNCH ABANDONED UNDER ITS RESUMED FRAME LEAVES ITS TURN'S END TO THAT
// FRAME, and its record waits for it. The launch that failed decided the run's
// ending; deleted at once, the record took the turn's capture stamps with it,
// and the frame recorded the turn's end at once — before a capture still being
// kept had landed, and naming an older one as the turn's last. Kept, the frame
// owes its end on it, and the run's ending records that end once the turn's
// captures settle, naming the capture the turn did not keep.
//
// Mutation: let the abandon delete the record while the turn's frame runs, or
// decide the turn's end in the ending while the frame does, and the frame finds
// no record to owe its end on, or the end names no unkept capture.
func TestARelaunchAbandonedUnderItsFrameLeavesTheTurnsEndToIt(t *testing.T) {
	t.Parallel()
	inBubble(t, func(t *testing.T) {
		rig, writes, keep := keepingRig(t)
		keep.putErr = objstore.ErrFull
		rig.launchKept(t, "t1")
		session := rig.runner.Session
		rig.runner.Finish(Result{Success: true, SessionID: "s-1", Captured: &session.Archive})
		var kept, owed bool
		rig.resumer.during = func(ctx context.Context, r PendingRun) {
			rig.runner.StartErr = errors.New("the harness would not start")
			if _, err := rig.coordinator.Launch(ctx, rig.manager, launchReq(r.TurnID)); err == nil {
				t.Error("the relaunch started though its harness would not")
			}
			current, found, err := rig.pending.Get(ctx, r.TurnID)
			kept = err == nil && found && current.Ending != nil
			owed, _ = rig.coordinator.OweEnd(ctx, r.TurnID, executorkeep.Ending{
				Outcome: executorkeep.OutcomeFailed, Failure: "the resumed turn's relaunch failed"})
		}
		rig.resumer.failWith(fmt.Errorf("%w (its relaunch failed): the harness would not start", ErrResumeAbandoned))
		payload, ev := rig.completion("t1")
		if err := rig.coordinator.OnCompleted(t.Context(), payload, ev); err != nil {
			t.Logf("OnCompleted: %v", err)
		}
		rig.awaitKeeps()
		rig.fireRetries()
		rig.fireRetries()

		if !kept || !owed {
			t.Fatalf("the frame found its run's record kept: %v, and owed its end on it: %v; want both", kept, owed)
		}
		ends := writes.endedRecords()
		if len(ends) != 1 || ends[0].Ending.Outcome != executorkeep.OutcomeFailed {
			t.Fatalf("recorded %+v, want the frame's account of the turn", ends)
		}
		if u := ends[0].Ending.Unkept; u == nil || u.LaunchSeq != 1 || !strings.Contains(u.Reason, "reservation is spent") {
			t.Fatalf("the end names %+v unkept, want the launch whose capture the store refused", u)
		}
		if _, found, _ := rig.pending.Get(t.Context(), "t1"); found {
			t.Fatal("the run's record was left behind the turn's end")
		}
	})
}

// A TURN'S END IS DECIDED ONCE, EVEN AFTER IT IS RECORDED. An attempt at the
// ending that comes after the end was published and cleared — a crash between
// the clear and the delete, or a delete refused — finds the end recorded on the
// record, and neither decides it again nor closes the stopped turn, and answers
// its colleague, a second time.
//
// Mutation: clear the owed end without marking it recorded, or decide the end
// on every attempt that finds none owed, and the stop is closed twice.
func TestATurnsEndIsDecidedOnceEvenAfterItIsRecorded(t *testing.T) {
	t.Parallel()
	inBubble(t, func(t *testing.T) {
		rig, writes, _ := keepingRig(t)
		var closes atomic.Int32
		closeStopped := rig.coordinator.closeStopped
		rig.coordinator.closeStopped = func(ctx context.Context, run PendingRun, calls RunCalls) (executorkeep.Ending, error) {
			closes.Add(1)
			return closeStopped(ctx, run, calls)
		}
		rig.launchKept(t, "t1")
		rig.runner.Finish(Result{Text: "halfway through"})
		if err := rig.coordinator.StopSeat(t.Context(), "swe", RunStop{By: "founder"}); err != nil {
			t.Fatalf("StopSeat: %v", err)
		}
		// THE DELETE REFUSED once the end is recorded and cleared, as a crash
		// between the two leaves the record.
		store := rig.coordinator.pending
		rig.coordinator.pending = &refusingStore{inner: store, refuse: []string{"Finish"}}
		rig.fireRetries()
		rig.awaitKeeps()
		rig.coordinator.pending = store
		rig.fireRetries()

		if n := closes.Load(); n != 1 {
			t.Fatalf("the stopped turn was closed %d times, want once", n)
		}
		if ends := writes.endedRecords(); len(ends) != 1 {
			t.Fatalf("recorded %d ends, want the turn's one", len(ends))
		}
		if _, found, _ := rig.pending.Get(t.Context(), "t1"); found {
			t.Fatal("the stopped run's record was left behind its end")
		}
	})
}

// keepsOutcomeLost is a run store that loses a keep's account of its capture
// (#1022): the first stamp of a capture past begun is held until the stamp's
// own context ends, and answered then as the coordination store answers a
// write it could not make in that time ([coord.ErrUnavailable]) — what a runner
// starved through the stamp's whole budget saw, a write race lost and the
// budget spent. Every other call passes through, and the reads of the run's
// row while its capture stands begun under the end it owes are counted: the
// reads of the end's wait for it.
type keepsOutcomeLost struct {
	PendingStore

	mu    sync.Mutex
	lost  bool
	waits int
}

func (s *keepsOutcomeLost) StampCapture(ctx context.Context, turnID string, stamp SessionCapture) (bool, error) {
	s.mu.Lock()
	lose := !s.lost && stamp.State != CaptureBegun
	s.lost = s.lost || lose
	s.mu.Unlock()
	if !lose {
		return s.PendingStore.StampCapture(ctx, turnID, stamp)
	}
	<-ctx.Done()
	return false, fmt.Errorf("%w: change the sandbox run %s: %w", coord.ErrUnavailable, turnID, ctx.Err())
}

func (s *keepsOutcomeLost) Get(ctx context.Context, turnID string) (PendingRun, bool, error) {
	run, found, err := s.PendingStore.Get(ctx, turnID)
	if err == nil && found && len(run.OwedEnd) > 0 && run.Capture != nil && run.Capture.State == CaptureBegun {
		s.mu.Lock()
		s.waits++
		s.mu.Unlock()
	}
	return run, found, err
}

// seen is whether the keep's outcome stamp was lost, and how many times the
// end's wait read the run's row.
func (s *keepsOutcomeLost) seen() (bool, int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.lost, s.waits
}

// AN END WAITS OUT A KEEP WHOSE OUTCOME THE STORE NEVER TOOK, and names its
// capture begun and never finished. The keep stamps what came of the capture
// within one record's budget, and a store that never answers it in that time
// leaves the capture begun on the run's row with nothing to settle it: the
// turn's end waits on it for as long as the turn's last capture could still
// land ([endCaptureWait]), reading the row once a [captureWaitTick], and then
// records the end naming it.
//
// IN THE BUBBLE, as every case of the rig runs ([inBubble]). On the machine's
// clock a runner starved through the rig's 200 ms record budget inside that
// stamp left an end waiting 38 minutes, past the package's timeout (#1022).
// Here the budget is spent by the stamp's own wait alone, and the end's wait
// runs out in virtual time, in as many reads as production's would make.
//
// Mutation: run the case off the bubble, and the rig refuses it; read the row
// on any tick shorter than production's, and the wait reads it more often than
// once a tick; hold the end's wait to a capture's own budget, and the end is
// recorded before its wait ran out.
func TestAnEndWaitsOutAKeepWhoseOutcomeTheStoreNeverTook(t *testing.T) {
	t.Parallel()
	inBubble(t, func(t *testing.T) {
		rig, writes, _ := keepingRig(t)
		store := &keepsOutcomeLost{PendingStore: rig.coordinator.pending}
		rig.coordinator.pending = store
		rig.launchKept(t, "t1")
		rig.runner.Finish(Result{Text: "halfway through"})
		if err := rig.coordinator.StopSeat(t.Context(), "swe", RunStop{By: "founder"}); err != nil {
			t.Fatalf("StopSeat: %v", err)
		}
		began := time.Now()
		rig.fireRetries()
		waited := time.Since(began)
		rig.awaitKeeps()

		lost, waits := store.seen()
		if !lost {
			t.Fatal("the premise: the keep never stamped what came of its capture")
		}
		ends := writes.endedRecords()
		if len(ends) != 1 {
			t.Fatalf("recorded %d ends, want the turn's one", len(ends))
		}
		if u := ends[0].Ending.Unkept; u == nil || !strings.Contains(u.Reason, "begun and never finished") {
			t.Fatalf("the end names %+v unkept, want the capture whose outcome the store never took, begun "+
				"and never finished", u)
		}
		if waited < endCaptureWait {
			t.Errorf("the end was recorded %s into the stop's retry, before its wait of %s for the capture "+
				"ran out", waited, endCaptureWait)
		}
		if most := int(endCaptureWait / captureWaitTick); waits == 0 || waits > most {
			t.Errorf("the end's wait read the row %d times, want it read once a %s for %s: at most %d",
				waits, captureWaitTick, endCaptureWait, most)
		}
		if _, found, _ := rig.pending.Get(t.Context(), "t1"); found {
			t.Fatal("the stopped run's record was left behind its end")
		}
	})
}

// TWO ATTEMPTS AT ONE ENDING ON A NODE FINISH IT ONCE. A stop decided on the
// pause watch is finished by the ending's own retry, and the launch whose
// attach it refused finishes the same ending at the same moment: each read the
// record before either owed the turn's end, each closed the stopped turn, and
// the colleague whose ask the turn took up was answered twice. The second
// attempt leaves the ending to the first.
//
// Mutation: let a second attempt on the node finish an ending the first is
// finishing, and the stopped turn is closed twice.
func TestTwoAttemptsAtOneEndingOnANodeFinishItOnce(t *testing.T) {
	t.Parallel()
	rig := newCoordRig(t)
	run := rig.launch("t1")
	rig.runner.Finish(Result{Text: "halfway through"})
	entered, release := make(chan struct{}), make(chan struct{})
	var closes atomic.Int32
	rig.coordinator.closeStopped = func(context.Context, PendingRun, RunCalls) (executorkeep.Ending, error) {
		if closes.Add(1) == 1 {
			close(entered)
			<-release
		}
		return executorkeep.Ending{Outcome: executorkeep.OutcomeStopped}, nil
	}
	if err := rig.coordinator.StopSeat(t.Context(), "swe", RunStop{By: "founder"}); err != nil {
		t.Fatalf("StopSeat: %v", err)
	}
	// THE FIRST ATTEMPT is the stop's own retry, which the pause watch hands
	// a started job's ending to at once, since ending the job is waiting for
	// it ([Coordinator.endJob]).
	stopped := make(chan int, 1)
	go func() { stopped <- rig.fireRetries() }()
	select {
	case <-entered:
	case <-time.After(10 * time.Second):
		t.Fatal("the stop's retry never reached the stopped turn's close")
	}
	// THE SECOND ATTEMPT, as the launch the stop refused makes it.
	if _, err := rig.coordinator.endRecord(t.Context(), rig.get("t1"), ending{
		fence: fenceOf(run), whileIn: Active, launch: EveryLaunch, reclaim: true,
	}); err != nil {
		t.Logf("endRecord: %v", err)
	}
	close(release)
	if fired := <-stopped; fired == 0 {
		t.Fatal("the stop armed no retry to finish it")
	}
	if n := closes.Load(); n != 1 {
		t.Fatalf("the stopped turn was closed %d times, want once", n)
	}
}

// AN END A NEWER BUILD OWED IS LEFT OWED FOR A NODE THAT READS IT. One holding a
// key this build does not know, or an outcome it does not know, is neither
// published without what it could not read nor cleared as one the log refused:
// the record keeps owing it, nothing reaches the log, and the ending is kept
// until a node of the build that wrote it records it.
//
// Mutation: decode the end into this build's account and publish it, or clear
// it as malformed, and a check below goes red.
func TestAnEndANewerBuildOwedIsLeftForANodeThatReadsIt(t *testing.T) {
	t.Parallel()
	for name, owed := range map[string]string{
		"a key this build does not know":      `{"outcome":"completed","handed_over_to":"agent-pm"}`,
		"an outcome this build does not know": `{"outcome":"cancelled"}`,
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			inBubble(t, func(t *testing.T) {
				rig, writes, _ := keepingRig(t)
				rig.launchKept(t, "t1")
				if _, wrote, err := rig.pending.OweEnd(t.Context(), "t1", []byte(owed)); err != nil || !wrote {
					t.Fatalf("OweEnd = %v, %v", wrote, err)
				}
				if _, err := rig.coordinator.publishEnd(t.Context(), rig.get("t1")); err == nil {
					t.Fatal("published an end this build cannot read")
				}
				if ends := writes.endedRecords(); len(ends) != 0 {
					t.Fatalf("recorded %+v, want nothing until a node that reads the end", ends)
				}
				if run := rig.get("t1"); string(run.OwedEnd) != owed || run.EndRecorded {
					t.Fatalf("the record owes %s (recorded %v), want the newer build's end still owed",
						run.OwedEnd, run.EndRecorded)
				}
			})
		})
	}
}

// loggingRig is a coordinator rig that records every turn's end on an executor
// log, as every engine's coordinator does, and keeps no session (the company's
// switch off): an ending whose record owes its turn's end is finished by this
// node's retry, off the frame that decided it ([errAskAway]).
//
// ONLY IN A BUBBLE ([inBubble]), which it checks ([mustBeBubbled]), for the
// budgets it cuts as [keepingRig] does.
//
//fixedwait:bubble
func loggingRig(t *testing.T) (*coordRig, *sessionWrites) {
	t.Helper()
	mustBeBubbled(t)
	rig := newCoordRig(t)
	writes := &sessionWrites{}
	rig.coordinator.sessions, rig.coordinator.keep = writes, &sessionKeep{now: func() time.Time { return rig.now }}
	rig.coordinator.sessionRetry, rig.coordinator.recordBudget = time.Millisecond, 200*time.Millisecond
	rig.coordinator.captureTick = captureWaitTick
	rig.coordinator.spoolDir = t.TempDir()
	// THE ENGINE'S ACCOUNT OF A LOSS, as production wires TurnLost.
	rig.coordinator.turnLost = func(context.Context, PendingRun, RunCalls) executorkeep.Ending {
		return executorkeep.Ending{Outcome: executorkeep.OutcomeLost}
	}
	return rig, writes
}

// owesItsEnd makes the rig's resumed turns owe their end as the engine's do
// before they return ([Coordinator.OweEnd]), then runs then — nil for nothing.
func owesItsEnd(t *testing.T, rig *coordRig, then func()) {
	t.Helper()
	rig.resumer.during = func(ctx context.Context, run PendingRun) {
		if owed, err := rig.coordinator.OweEnd(ctx, run.TurnID, executorkeep.Ending{
			Outcome: executorkeep.OutcomeCompleted, Decision: "done"}); err != nil || !owed {
			t.Errorf("the resumed turn's OweEnd = %v, %v", owed, err)
		}
		if then != nil {
			then()
		}
	}
}

// listsThen is a PendingStore whose next listing of a seat's runs, once armed,
// runs then AFTER the store answered it: the listing's caller holds rows read
// before whatever then does, as a goroutine whose read was answered and which is
// not scheduled again until then has run holds them.
type listsThen struct {
	PendingStore
	mu   sync.Mutex
	then func()
}

func (s *listsThen) arm(then func()) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.then = then
}

func (s *listsThen) ListActiveForSeat(ctx context.Context, handle string) ([]PendingRun, error) {
	s.mu.Lock()
	then := s.then
	s.then = nil
	s.mu.Unlock()
	runs, err := s.PendingStore.ListActiveForSeat(ctx, handle)
	if then != nil {
		then()
	}
	return runs, err
}

// detach drops every hold on handle's inbox, as the detach a seat's release
// makes does.
func (h *holdSpy) detach(handle string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	delete(h.held, handle)
}

// A RECOUNT READ BEFORE AN ENDING'S DELETE NEVER OUTLIVES IT. A resumed turn's
// frame recounts its seat as the resume returns, while the ending it decided is
// finished at once, off the frame, by this node's retry: which records the
// turn's end, deletes the record, recounts the seat and settles. A frame whose
// listing was read before that delete and written after the retry's recount
// counted the deleted run as holding again, and held the seat's inbox with
// nothing left to recount it: the seat took no mail until it moved.
//
// Two moves order the frame's recount after the retry's here, the retry's delete
// ([Coordinator.moved]) and the retry's own recount as it is written
// ([Coordinator.syncSeat]), and either orders it alone; the two cases after this
// one hold each to a race only it orders.
//
// Mutation: drop both moves, and the frame's stale listing is written last, the
// seat counted held and its inbox held for a run that is gone.
func TestARecountReadBeforeAnEndingsDeleteNeverOutlivesIt(t *testing.T) {
	t.Parallel()
	inBubble(t, func(t *testing.T) {
		rig, writes := loggingRig(t)
		holds := rig.withHold()
		stale := &listsThen{PendingStore: rig.coordinator.pending}
		rig.coordinator.pending = stale
		launchWithNoConversation(t, rig, "t1")
		if !holds.holding("swe") {
			t.Fatal("the premise: the running job holds the seat's inbox")
		}
		// THE FRAME'S RECOUNT IS THE SEAT'S NEXT LISTING, and its goroutine is not
		// scheduled again until the ending's retry has run to its settle.
		owesItsEnd(t, rig, func() { stale.arm(func() { rig.fireRetries() }) })
		rig.runner.Finish(Result{Success: true})
		payload, ev := rig.completion("t1")
		if err := rig.coordinator.OnCompleted(t.Context(), payload, ev); err != nil {
			t.Fatalf("OnCompleted: %v", err)
		}
		rig.finished("t1")
		if ends := writes.endedRecords(); len(ends) != 1 || ends[0].Ending.Decision != "done" {
			t.Fatalf("recorded %+v, want the resumed turn's one end", ends)
		}
		if held, _ := rig.coordinator.SeatRuns("swe"); held || holds.holding("swe") {
			t.Fatalf("the seat is counted held (%v) and its inbox held (%v) for a run whose record is gone",
				held, holds.holding("swe"))
		}
	})
}

// A RECOUNT READ BEFORE A DELETE OF THIS NODE'S READS AGAIN, though nothing
// recounts the seat after the delete: an ending this node finishes deletes the
// run's record and moves the seat ([Coordinator.moved]), as every count it
// applies does, so a listing that straddles the delete is read again rather
// than written over it — which counted the run whose record is gone as holding,
// and held the seat's inbox behind it.
//
// Mutation: drop the move an ending's delete makes ([Coordinator.moved]), and
// the recount writes its stale listing, the seat counted held for a run that is
// gone.
func TestARecountReadBeforeADeleteOfThisNodesReadsAgain(t *testing.T) {
	t.Parallel()
	rig := newCoordRig(t)
	holds := rig.withHold()
	stale := &listsThen{PendingStore: rig.coordinator.pending}
	rig.coordinator.pending = stale
	launchWithNoConversation(t, rig, "t1")
	run := rig.get("t1")
	// THE RUN ENDS HERE, its record deleted, while the recount's listing is on
	// its way back; nothing recounts the seat after the delete.
	stale.arm(func() {
		if decided, err := rig.coordinator.endRecord(t.Context(), run, ending{
			fence: rigLease, whileIn: Active, launch: EveryLaunch, reclaim: true,
		}); err != nil || decided == nil {
			t.Errorf("the premise: the run's ending = %v, %v, want it decided and finished here",
				decided, err)
		}
		if _, found, err := rig.pending.Get(t.Context(), "t1"); err != nil || found {
			t.Errorf("the premise: the ending deleted the record (found %v, %v)", found, err)
		}
	})
	rig.coordinator.syncSeat(t.Context(), "swe")
	if held, _ := rig.coordinator.SeatRuns("swe"); held || holds.holding("swe") {
		t.Fatalf("the seat is counted held (%v) and its inbox held (%v) for a run whose record is gone",
			held, holds.holding("swe"))
	}
}

// A RECOUNT OLDER THAN ONE WRITTEN BEFORE IT READS AGAIN. The store moves under
// a seat's recounts by other nodes' hands too, which move nothing this node
// counts: the seat's last holder finishing an ending it decided, whose steps
// after the decision take no lease, deletes the run's record. A recount whose
// listing was read before that delete, and written after a newer recount that
// read the store without the run, counted the run as holding again over the
// newer count, and held the seat's inbox with nothing left to recount it.
//
// Mutation: drop the move a recount makes as it is written
// ([Coordinator.syncSeat]), and the older listing is written last, the seat
// counted held for a run that is gone.
func TestARecountOlderThanOneWrittenBeforeItReadsAgain(t *testing.T) {
	t.Parallel()
	rig := newCoordRig(t)
	holds := rig.withHold()
	stale := &listsThen{PendingStore: rig.coordinator.pending}
	rig.coordinator.pending = stale
	launchWithNoConversation(t, rig, "t1")
	ctx := t.Context()
	stale.arm(func() {
		// ANOTHER NODE ENDS THE RUN, record and all, in the store alone...
		decided, ok, err := rig.pending.DecideEnding(ctx, "t1",
			ending{whileIn: Active, launch: EveryLaunch}.decision())
		if err != nil || !ok {
			t.Errorf("the premise: another node's ending = %v, %v", ok, err)
			return
		}
		if _, deleted, err := rig.pending.Finish(ctx, "t1", decided.Ending.ID); err != nil || !deleted {
			t.Errorf("the premise: another node's delete = %v, %v", deleted, err)
		}
		// ...AND A NEWER RECOUNT HERE reads the store after it, and is
		// written first.
		rig.coordinator.syncSeat(ctx, "swe")
	})
	rig.coordinator.syncSeat(ctx, "swe")
	if held, _ := rig.coordinator.SeatRuns("swe"); held || holds.holding("swe") {
		t.Fatalf("the seat is counted held (%v) and its inbox held (%v) for a run whose record is gone",
			held, holds.holding("swe"))
	}
}

// A SEAT TAKEN BACK BESIDE AN ENDING STILL BEING FINISHED HOLDS ITS MAIL BEHIND
// IT. This node's retry of a resumed turn's ending waits for the turn's capture
// to land before it records the turn's end; meanwhile the seat's lease lapses,
// the seat is released, and this node takes it again. Its recovery finds the
// ending decided and another attempt here finishing it, and left it to that
// attempt holding nothing: the seat's mail ran while its run's record still
// owed the turn's end — the turn the conversation's next turn must never
// overtake. The ending is kept for this node's retry as well, which holds the
// mail behind it and finds the record gone the moment the attempt it waited
// behind returns.
//
// Mutation: leave an ending another attempt on the node is finishing to that
// attempt alone ([Coordinator.endRun]), and the seat taken back is not held; or
// ask the ending kept behind the attempt again only on the hand-back's spacing
// ([Coordinator.finishOnce]), and the seat's mail waits that long after the
// record is gone.
func TestASeatTakenBackBesideAnEndingStillBeingFinishedHoldsItsMail(t *testing.T) {
	t.Parallel()
	inBubble(t, func(t *testing.T) {
		rig, writes, keep := keepingRig(t)
		holds := rig.withHold()
		keep.gate = make(chan struct{})
		rig.launchKept(t, "t1")
		owesItsEnd(t, rig, nil)
		session := rig.runner.Session
		rig.runner.Finish(Result{Success: true, SessionID: "s-1", Captured: &session.Archive})
		payload, ev := rig.completion("t1")
		if err := rig.coordinator.OnCompleted(t.Context(), payload, ev); err != nil {
			t.Fatalf("OnCompleted: %v", err)
		}
		// THE ENDING'S RETRY, in its slot and waiting for the capture to land.
		busy := make(chan struct{})
		go func() {
			defer close(busy)
			rig.fireRetries()
		}()
		// THE KEEP LANDS ONCE THE CASE HAS LOOKED, on every way out of it, so the
		// retry waiting behind it is never left waiting.
		landed := sync.OnceFunc(func() {
			close(keep.gate)
			<-busy
			rig.awaitKeeps()
		})
		defer landed()
		eventually(t, func() bool {
			rig.coordinator.mu.Lock()
			defer rig.coordinator.mu.Unlock()
			return rig.coordinator.finishing["t1"]
		}, "the ending's retry never began finishing it")
		// THE SEAT IS LOST AND TAKEN AGAIN BY THIS NODE: the release detaches the
		// inbox, which drops every hold it carried.
		rig.coordinator.ReleaseSeat("swe")
		holds.detach("swe")
		if err := rig.recoverSeat(t.Context(), "node-a:2", 2); err != nil {
			t.Fatalf("recoverSeat: %v", err)
		}
		if run := rig.get("t1"); run.Ending == nil || len(run.OwedEnd) == 0 {
			t.Fatalf("the premise: the record holds its ending (%v) and owes its turn's end (%s)",
				run.Ending != nil, run.OwedEnd)
		}
		if !holds.holding("swe") {
			t.Fatal("the seat taken back takes its mail while its run's record still owes the turn's end")
		}

		landed()
		// ASKED AGAIN THE MOMENT THE ATTEMPT IT WAITED BEHIND RETURNED, which
		// finished it: the hold behind a record that is gone lifts then, not a
		// hand-back's spacing later.
		if due := rig.retries.delays(); !slices.Equal(due, []time.Duration{0}) {
			t.Fatalf("the ending kept behind the attempt is asked again in %v, want at once", due)
		}
		for range 3 {
			rig.fireRetries()
		}
		rig.finished("t1")
		if ends := writes.endedRecords(); len(ends) != 1 || ends[0].Ending.Decision != "done" {
			t.Fatalf("recorded %+v, want the resumed turn's one end", ends)
		}
		if holds.holding("swe") || rig.coordinator.owes("t1") {
			t.Fatalf("the seat's inbox is held (%v), or the ending still owed (%v), once its record is gone",
				holds.holding("swe"), rig.coordinator.owes("t1"))
		}
	})
}
