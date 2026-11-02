package sandbox

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/crewlet/crewlet/internal/executordomain"
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
	rig, writes, _ := keepingRig(t)
	rig.launchKept(t, "t1")
	if err := rig.coordinator.RetireSeat(t.Context(), sweSeat(), "swe", rigLease.Owner, rigLease.Epoch); err != nil {
		t.Fatalf("RetireSeat: %v", err)
	}
	if _, found, err := rig.pending.Get(t.Context(), "t1"); err != nil || found {
		t.Fatalf("the retired seat's run is still recorded (found %v, %v)", found, err)
	}
	ends := writes.endedRecords()
	if len(ends) != 1 || ends[0].n.Ending.Outcome != executordomain.OutcomeLost {
		t.Fatalf("recorded %+v, want the retired run's turn ended as lost", ends)
	}
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
		owed, _ = rig.coordinator.OweEnd(ctx, r.TurnID, executordomain.Ending{
			Outcome: executordomain.OutcomeFailed, Failure: "the resumed turn's relaunch failed"})
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
	if len(ends) != 1 || ends[0].n.Ending.Outcome != executordomain.OutcomeFailed {
		t.Fatalf("recorded %+v, want the frame's account of the turn", ends)
	}
	if u := ends[0].n.Ending.Unkept; u == nil || u.LaunchSeq != 1 || !strings.Contains(u.Reason, "reservation is spent") {
		t.Fatalf("the end names %+v unkept, want the launch whose capture the store refused", u)
	}
	if _, found, _ := rig.pending.Get(t.Context(), "t1"); found {
		t.Fatal("the run's record was left behind the turn's end")
	}
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
	rig, writes, _ := keepingRig(t)
	var closes atomic.Int32
	closeStopped := rig.coordinator.closeStopped
	rig.coordinator.closeStopped = func(ctx context.Context, run PendingRun) (executordomain.Ending, error) {
		closes.Add(1)
		return closeStopped(ctx, run)
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
}

// TWO ATTEMPTS AT ONE ENDING ON A NODE FINISH IT ONCE. A stop decided on the
// pause watch while the job it stopped was still launching is finished there,
// and the launch whose attach it refused finishes the same ending at the same
// moment: each read the record before either owed the turn's end, each closed
// the stopped turn, and the colleague whose ask the turn took up was answered
// twice. The second attempt leaves the ending to the first.
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
	rig.coordinator.closeStopped = func(context.Context, PendingRun) (executordomain.Ending, error) {
		if closes.Add(1) == 1 {
			close(entered)
			<-release
		}
		return executordomain.Ending{Outcome: executordomain.OutcomeStopped}, nil
	}
	stopped := make(chan error, 1)
	go func() { stopped <- rig.coordinator.StopSeat(t.Context(), "swe", RunStop{By: "founder"}) }()
	<-entered
	// THE SECOND ATTEMPT, as the launch the stop refused makes it.
	if _, err := rig.coordinator.endRecord(t.Context(), rig.get("t1"), ending{
		fence: fenceOf(run), whileIn: Active, launch: EveryLaunch, reclaim: true,
	}); err != nil {
		t.Logf("endRecord: %v", err)
	}
	close(release)
	if err := <-stopped; err != nil {
		t.Logf("StopSeat: %v", err)
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
	}
}
