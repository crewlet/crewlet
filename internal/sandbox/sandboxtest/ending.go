package sandboxtest

import (
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/sandbox"
)

// A FINISHED RUN HAS NO RECORD. The bucket is ageless and every completion
// poll and seat recovery reads all of it, so a settled run that stayed behind
// would be read on every one of those passes for the life of the deployment.
// Nothing may still find it: not the tail claim, not the busy check, not an
// answer matching a question it once asked.
func testAFinishedRunIsGoneForEveryReader(t *testing.T, s sandbox.PendingStore) {
	ctx := t.Context()
	mustLaunched(t, s, run("t1"))
	park(t, s, "t1")
	// Read before the record goes: a claim names the launch it is for, and
	// after the delete there is nothing left to read one from.
	tail := answerTo(t, s, "t1")

	settled, finished, err := end(ctx, s, "t1", everyJob(sandbox.Fence{}, sandbox.Active))
	if err != nil || !finished {
		t.Fatalf("Finish = %v, %v; want the record deleted", finished, err)
	}
	// THE RECORD IT DELETED, not the caller's snapshot of it: a settle that
	// could not read the row first reclaims the box this names, so a store
	// handing back a zero value there would leave a live box named by
	// nothing.
	if settled.TurnID != "t1" || settled.SandboxID != "box-t1" ||
		settled.Status != sandbox.StatusAwaiting {
		t.Errorf("Finish handed back %+v; want the record as the store held it", settled)
	}
	if got, found, err := s.Get(ctx, "t1"); err != nil || found {
		t.Fatalf("Get after Finish = %+v, found %v, %v; want no record", got, found, err)
	}
	if active, err := s.ListActive(ctx); err != nil || len(active) != 0 {
		t.Errorf("ListActive after Finish = %+v, %v; want none", active, err)
	}
	if seat, err := s.ListActiveForSeat(ctx, "swe"); err != nil || len(seat) != 0 {
		t.Errorf("the seat's busy read still sees a finished run: %+v, %v", seat, err)
	}
	if _, found, err := findAwaiting(ctx, s, "swe", answerOnTheDM); err != nil || found {
		t.Errorf("an answer matched the question of a finished run: found %v, %v", found, err)
	}
	if _, won, err := s.ClaimForResume(ctx, "t1", tail, sandbox.Fence{}); err != nil || won {
		t.Errorf("a finished run was claimed: won=%v err=%v", won, err)
	}
	// Two parties reaching the end of one run is ordinary, not an error.
	if _, again, err := end(ctx, s, "t1", everyJob(sandbox.Fence{}, sandbox.Active)); err != nil || again {
		t.Errorf("a second Finish = %v, %v; want false and no error", again, err)
	}
}

// A write that arrives after the run ended is the ordinary shape of a box
// shutting down or a peer a moment behind. Each is a conditional flip on an
// existing record, so none of them may bring the record back: a resurrected
// row is a run that nothing will ever settle again.
func testAFinishedRunIsNotRecreatedByALateWrite(t *testing.T, s sandbox.PendingStore) {
	ctx := t.Context()
	launch := mustLaunched(t, s, run("t1"))
	if _, _, err := end(ctx, s, "t1", everyJob(sandbox.Fence{}, sandbox.Active)); err != nil {
		t.Fatalf("Finish: %v", err)
	}
	late := map[string]func() error{
		"SetStatus": func() error { return s.SetStatus(ctx, "t1", sandbox.StatusRunning, sandbox.Fence{}) },
		"AttachSandbox": func() error {
			return s.AttachSandbox(ctx, "t1", sandbox.BoxRef{SandboxID: "box-1"}, sandbox.Fence{})
		},
		"MarkBoxPaused": func() error { return s.MarkBoxPaused(ctx, "t1", base) },
		"ReleaseBox":    func() error { return s.ReleaseBox(ctx, "t1") },
		"MarkAwaiting": func() error {
			return s.MarkAwaiting(ctx, "t1", sandbox.Clarification{AskedAt: time.Now().UTC()})
		},
		"MarkSuspended": func() error {
			_, err := s.MarkSuspended(ctx, "t1", suspension())
			return err
		},
		"ClaimOwnership": func() error {
			_, err := s.ClaimOwnership(ctx, "t1", "node-b:2", 9)
			return err
		},
		"ExpirePause": func() error {
			_, err := s.ExpirePause(ctx, "t1")
			return err
		},
		"AppendBridgeCall": func() error {
			_, err := s.AppendBridgeCall(ctx, "t1", sandbox.BridgeAppend{
				Launch: launch, Call: sandbox.BridgeCall{Name: "read_page"}})
			return err
		},
	}
	for name, write := range late {
		// AN ATTACH IS THE ONE THAT SAYS SO: its caller is a launch about to
		// start a job in the box, and a row that is gone names no box.
		if err := write(); (err != nil) != (name == "AttachSandbox") {
			t.Errorf("%s on a finished run: %v; want the ordinary no-op, or the attach refused",
				name, err)
		}
		if _, found, err := s.Get(ctx, "t1"); err != nil || found {
			t.Fatalf("%s recreated a finished run's record (found %v, %v)", name, found, err)
		}
	}
}

// Every party that ends a run decides its ending and deletes the record it
// read. Racing ends must agree on ONE ending — every decider handed the same
// one, whatever it asked to announce — and exactly one of them deletes the
// record, so that the announcement is the same event whoever makes it.
func testARunIsFinishedExactlyOnce(t *testing.T, s sandbox.PendingStore) {
	mustLaunched(t, s, run("t1"))
	const racers = 10
	var wins atomic.Int32
	var mu sync.Mutex
	endings := map[string]string{}
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := range racers {
		wg.Go(func() {
			<-start
			decided, ok, err := s.DecideEnding(t.Context(), "t1", sandbox.Decision{
				License: everyJob(sandbox.Fence{}, sandbox.Active),
				Reason:  fmt.Sprintf("reason-%d", i),
			})
			if err != nil {
				t.Errorf("DecideEnding: %v", err)
				return
			}
			if !ok {
				return
			}
			mu.Lock()
			endings[decided.Ending.ID] = decided.Ending.Reason
			mu.Unlock()
			_, finished, err := s.Finish(t.Context(), "t1", decided.Ending.ID)
			if err != nil {
				t.Errorf("Finish: %v", err)
				return
			}
			if finished {
				wins.Add(1)
			}
		})
	}
	close(start)
	wg.Wait()
	if got := wins.Load(); got != 1 {
		t.Fatalf("%d of %d concurrent endings reported deleting the record, want exactly 1", got, racers)
	}
	if len(endings) != 1 {
		t.Fatalf("the racers were handed %d endings %v, want one: two decisions are two "+
			"announcements of one lost run", len(endings), endings)
	}
}

// A settle that could NOT read the row hands the decision to the store: it
// asks for the run to be ended only while it is still the status its claim
// left it in. So a row that moved on under it — a relaunch the resumed turn
// made, which takes the run back through launching and reuses the very box
// this settle would kill — has to survive the call, and a row still in the
// claim has to be ended by it.
func testAnEndingIsRefusedOutsideItsLicense(t *testing.T, s sandbox.PendingStore) {
	ctx := t.Context()
	mustLaunched(t, s, run("t1"))
	claimed := []string{sandbox.StatusResumed}

	if got, ok, err := s.DecideEnding(ctx, "t1", sandbox.Decision{License: everyJob(sandbox.Fence{}, claimed)}); err != nil || ok {
		t.Errorf("a running run was ended under a claimed-only licence: %+v, %v, %v", got.Ending, ok, err)
	}
	if got, found, err := s.Get(ctx, "t1"); err != nil || !found {
		t.Fatalf("the record is gone after a refused ending (found %v, %v)", found, err)
	} else if got.Status != sandbox.StatusRunning || got.Ending != nil {
		t.Errorf("a refused ending left the record at %q, ending %+v", got.Status, got.Ending)
	}
	// An empty licence ends nothing at all, which is the safe reading of a
	// caller that stated none.
	if _, ok, err := s.DecideEnding(ctx, "t1", sandbox.Decision{License: everyJob(sandbox.Fence{}, nil)}); err != nil || ok {
		t.Errorf("an ending with no licence was decided: %v, %v", ok, err)
	}

	if _, _, err := s.ClaimForResume(ctx, "t1", completionOf(t, s, "t1"), sandbox.Fence{}); err != nil {
		t.Fatalf("claim: %v", err)
	}
	if _, ended, err := end(ctx, s, "t1", everyJob(sandbox.Fence{}, claimed)); err != nil || !ended {
		t.Errorf("the run the claim left behind was not ended: %v, %v", ended, err)
	}
	if _, found, err := s.Get(ctx, "t1"); err != nil || found {
		t.Errorf("the claimed run still has a record (found %v, %v)", found, err)
	}
}

// AN ENDING IS DECIDED ONCE, and a second decider is handed the first: the same
// identity and the same announcement, which is what every attempt that finishes
// it publishes — so a run whose ending was decided by one party and finished by
// another is announced once, for the reason it was decided. And the record goes
// only for the ending named.
func testAnEndingIsDecidedOnce(t *testing.T, s sandbox.PendingStore) {
	ctx := t.Context()
	mustLaunched(t, s, run("t1"))
	license := everyJob(sandbox.Fence{}, sandbox.Active)
	first, ok, err := s.DecideEnding(ctx, "t1", sandbox.Decision{
		License: license, Reason: "collect_unreachable", Detail: "the box died", Reclaim: true,
	})
	if err != nil || !ok || first.Ending == nil || first.Ending.ID == "" || first.Ending.At.IsZero() {
		t.Fatalf("DecideEnding = %+v, %v, %v, want an ending with an identity", first.Ending, ok, err)
	}
	if !first.Ending.Reclaim || first.Ending.Reason != "collect_unreachable" {
		t.Fatalf("recorded %+v, want the decision's terms", first.Ending)
	}
	second, ok, err := s.DecideEnding(ctx, "t1", sandbox.Decision{
		License: license, Reason: "abandoned_tail", Detail: "the node stopped",
	})
	if err != nil || !ok || second.Ending == nil || *second.Ending != *first.Ending {
		t.Fatalf("a second decision = %+v, %v, %v, want the first one handed back: %+v",
			second.Ending, ok, err, first.Ending)
	}
	if got := mustGet(t, s, "t1"); got.Ending == nil || *got.Ending != *first.Ending {
		t.Fatalf("the row records %+v, want the first decision", got.Ending)
	}
	if _, ended, err := s.Finish(ctx, "t1", "another-ending"); err != nil || ended {
		t.Fatalf("Finish for another ending = %v, %v, want refused", ended, err)
	}
	if _, ended, err := s.Finish(ctx, "t1", first.Ending.ID); err != nil || !ended {
		t.Fatalf("Finish for the decided ending = %v, %v", ended, err)
	}
}

// A RUN WHOSE ENDING IS DECIDED TAKES NO OTHER WRITE. The ending is announced
// before its record is deleted, so nothing may move the run off it in between:
// no claim, no take, no release, no relaunch, no answer, no park — a run
// announced lost and then resumed is a person told their work is gone while it
// carries on. A write whose caller acts on it landing is refused ALOUD
// ([sandbox.ErrRunEnding]); the rest are refused as their ordinary no. What
// every node may still write is what is true whoever finishes the ending — a
// lease stamped on the row, a call the box already made, a copy published.
func testADecidedRunTakesNoOtherWrite(t *testing.T, s sandbox.PendingStore) {
	ctx := t.Context()
	mustLaunched(t, s, run("t1"))
	park(t, s, "t1")
	launch := mustGet(t, s, "t1").LaunchID
	if _, ok, err := s.RecordAnswer(ctx, "t1", launch, answerOf("r1", "use main"), sandbox.Fence{}); err != nil || !ok {
		t.Fatalf("RecordAnswer = %v, %v", ok, err)
	}
	if _, ok, err := s.ClaimForResume(ctx, "t1", sandbox.RecordedAnswerTail(launch), sandbox.Fence{}); err != nil || !ok {
		t.Fatalf("ClaimForResume = %v, %v", ok, err)
	}
	decided := decide(t, s, "t1", everyJob(sandbox.Fence{}, sandbox.Active))

	refusedQuietly := map[string]func() (bool, error){
		"TakeAnswer": func() (bool, error) { return s.TakeAnswer(ctx, "t1", launch, sandbox.Fence{}) },
		"ReleaseClaim": func() (bool, error) {
			return s.ReleaseClaim(ctx, "t1", sandbox.Release{Launch: launch, To: sandbox.StatusAnswered})
		},
		"ClaimForResume": func() (bool, error) {
			_, ok, err := s.ClaimForResume(ctx, "t1", sandbox.Tail{Launch: launch, From: sandbox.Active}, sandbox.Fence{})
			return ok, err
		},
		"RecordAnswer": func() (bool, error) {
			_, ok, err := s.RecordAnswer(ctx, "t1", launch, answerOf("r2", "use dev"), sandbox.Fence{})
			return ok, err
		},
		"DeclineAnswer": func() (bool, error) {
			_, ok, err := s.DeclineAnswer(ctx, "t1", launch, []string{"r1"}, nil, sandbox.Fence{})
			return ok, err
		},
		"ExpirePause": func() (bool, error) { return s.ExpirePause(ctx, "t1") },
		"MarkClaimLost": func() (bool, error) {
			return s.MarkClaimLost(ctx, "t1", launch,
				sandbox.JobLoss{Cause: sandbox.LossLaunchNodeStoreSilent, At: base}, sandbox.Fence{})
		},
	}
	for name, write := range refusedQuietly {
		if ok, err := write(); err != nil || ok {
			t.Errorf("%s on a run whose ending is decided = %v, %v, want refused", name, ok, err)
		}
	}
	refusedAloud := map[string]func() error{
		"BeginLaunch": func() error {
			_, err := s.BeginLaunch(ctx, run("t1"), sandbox.Fence{})
			return err
		},
		"AttachSandbox": func() error { return s.AttachSandbox(ctx, "t1", sandbox.BoxRef{SandboxID: "box-2"}, sandbox.Fence{}) },
		"MarkAwaiting": func() error {
			return s.MarkAwaiting(ctx, "t1", sandbox.Clarification{AskedAt: time.Now().UTC()})
		},
		"SetStatus": func() error { return s.SetStatus(ctx, "t1", sandbox.StatusRunning, sandbox.Fence{}) },
		// ALOUD, because the park is the second half of a launch the
		// ending was decided under — a person stopped the seat between the
		// two — and its turn reports itself stopped rather than as a
		// suspension that failed.
		"MarkSuspended": func() error {
			_, err := s.MarkSuspended(ctx, "t1", suspension())
			return err
		},
	}
	for name, write := range refusedAloud {
		if err := write(); !errors.Is(err, sandbox.ErrRunEnding) {
			t.Errorf("%s on a run whose ending is decided = %v, want it refused with ErrRunEnding", name, err)
		}
	}
	got := mustGet(t, s, "t1")
	if got.Status != sandbox.StatusResumed || got.LaunchID != launch || got.Answer == nil ||
		got.Answer.Taken() || got.SandboxID != "box-t1" || *got.Ending != *decided.Ending {
		t.Fatalf("the decided run moved: status %q launch %q answer %+v box %q ending %+v",
			got.Status, got.LaunchID, got.Answer, got.SandboxID, got.Ending)
	}
	if ok, err := s.ClaimOwnership(ctx, "t1", "node-b:1", 5); err != nil || !ok {
		t.Errorf("a lease stamped on a decided run = %v, %v, want it taken", ok, err)
	}
	if ok, err := s.AppendBridgeCall(ctx, "t1", sandbox.BridgeAppend{
		Launch: launch, Call: sandbox.BridgeCall{Name: "read_page"},
	}); err != nil || !ok {
		t.Errorf("a bridged call recorded on a decided run = %v, %v, want it taken", ok, err)
	}
}

// A STOP IS RECORDED IN THE WRITE THAT DECIDES IT, so whichever attempt finishes
// the ending — the node that stopped the run, or the seat's next holder after a
// crash — announces it as a stop and says who made it. One decision per run: a
// second decider's stop does not replace the first's author.
func testAStopIsRecordedWithTheDecisionThatMakesIt(t *testing.T, s sandbox.PendingStore) {
	ctx := t.Context()
	launch := mustLaunched(t, s, run("t1"))
	stop := sandbox.RunStop{By: "founder", ByKind: "human", OperatorID: "pat:founder-token", Reason: "wrong repo"}
	decided, ok, err := s.DecideEnding(ctx, "t1", sandbox.Decision{
		License: sandbox.License{WhileIn: sandbox.Stoppable, Launch: launch}, Reclaim: true, Stop: &stop,
	})
	if err != nil || !ok || decided.Ending == nil {
		t.Fatalf("DecideEnding = %+v, %v, %v", decided.Ending, ok, err)
	}
	if got := mustGet(t, s, "t1"); got.Stop == nil || *got.Stop != stop {
		t.Fatalf("the decided row carries stop %+v, want %+v", got.Stop, stop)
	}
	other := sandbox.RunStop{By: "somebody-else"}
	again, ok, err := s.DecideEnding(ctx, "t1", sandbox.Decision{
		License: sandbox.License{WhileIn: sandbox.Stoppable, Launch: launch}, Stop: &other,
	})
	if err != nil || !ok || again.Ending.ID != decided.Ending.ID {
		t.Fatalf("a second decision = %+v, %v, %v, want the first handed back", again.Ending, ok, err)
	}
	if got := mustGet(t, s, "t1"); got.Stop == nil || got.Stop.By != stop.By {
		t.Fatalf("a second decider replaced who stopped the run: %+v", got.Stop)
	}
	// AND NO OTHER ENDING CARRIES ONE.
	mustLaunched(t, s, run("t2"))
	if got := decide(t, s, "t2", everyJob(sandbox.Fence{}, sandbox.Active)); got.Stop != nil {
		t.Fatalf("an ending that is not a stop recorded %+v as its author", got.Stop)
	}
}

// WHAT A STOP READ OF ITS RUN IS RECORDED ONCE, AS A STEP OF ITS ENDING, with the
// charge and the publish it made — so an attempt after a crash neither reads
// the box again nor charges it twice — and only on the stop it names.
func testAStopsCollectionIsRecordedOnceAsAStepOfItsEnding(t *testing.T, s sandbox.PendingStore) {
	ctx := t.Context()
	launch := mustLaunched(t, s, run("t1"))
	stop := sandbox.RunStop{By: "founder"}
	decided, decidedOK, decideErr := s.DecideEnding(ctx, "t1", sandbox.Decision{
		License: sandbox.License{WhileIn: sandbox.Stoppable, Launch: launch}, Reclaim: true, Stop: &stop,
	})
	if decideErr != nil || !decidedOK {
		t.Fatalf("DecideEnding = %v, %v", decidedOK, decideErr)
	}
	collection := sandbox.StopCollection{
		Ending: decided.Ending.ID, InputTokens: 9000, OutputTokens: 1000,
		CacheReadTokens: 4000, CacheWriteTokens: 500, Condensed: sandbox.AuxTokens{Input: 30, Output: 7},
		Charged: true, CompanyCharged: true, Published: true,
	}
	if _, won, err := s.CollectStop(ctx, "t1", sandbox.StopCollection{Ending: "another-ending"}); err != nil || won {
		t.Fatalf("a collection naming another ending = %v, %v, want refused", won, err)
	}
	written, ok, err := s.CollectStop(ctx, "t1", collection)
	if err != nil || !ok {
		t.Fatalf("CollectStop = %v, %v", ok, err)
	}
	got := mustGet(t, s, "t1")
	if got.Stop == nil || !got.Stop.Collected || got.Stop.InputTokens != 9000 || got.Stop.OutputTokens != 1000 ||
		got.Stop.CacheReadTokens != 4000 || got.Stop.CacheWriteTokens != 500 ||
		got.Stop.Condensed != collection.Condensed || got.Stop.By != "founder" {
		t.Fatalf("the stop reads %+v, want the collection recorded beside who made it", got.Stop)
	}
	if !got.Charged || !got.CompanyCharged || !got.Launch.Published {
		t.Fatalf("charged=%v company=%v published=%v, want all three recorded with the collection",
			got.Charged, got.CompanyCharged, got.Launch.Published)
	}
	if written.Stop == nil || !written.Stop.Collected {
		t.Fatalf("CollectStop handed back %+v, want the row as written", written.Stop)
	}
	if _, ok, err := s.CollectStop(ctx, "t1", collection); err != nil || ok {
		t.Fatalf("a second collection of one stop = %v, %v, want refused", ok, err)
	}
	// NOT A STOP, NOT DECIDED: nothing to collect.
	mustLaunched(t, s, run("t2"))
	plain := decide(t, s, "t2", everyJob(sandbox.Fence{}, sandbox.Active))
	if _, ok, err := s.CollectStop(ctx, "t2", sandbox.StopCollection{Ending: plain.Ending.ID}); err != nil || ok {
		t.Fatalf("a collection on an ending that is not a stop = %v, %v, want refused", ok, err)
	}
	mustLaunched(t, s, run("t3"))
	if _, ok, err := s.CollectStop(ctx, "t3", sandbox.StopCollection{}); err != nil || ok {
		t.Fatalf("a collection on a live run = %v, %v, want refused", ok, err)
	}
}

// A STOP OF A JOB A COLLECTION ALREADY READ IS REFUSED, IN THE DECIDING WRITE.
// A claim whose collection read the box and whose resume then gave it back —
// held by the seat's budget, or for a retry — puts the row back in running, a
// status a stop ends; but the job is over and its spend is its turn's resume's
// to charge, so a stop decided on it charged the work item a second time under
// a segment of its own. The store refuses it whatever the license says, because
// the stop's listing cannot see a claim that came and went before its decision;
// an ending that is not a stop still ends it, a release that did not read the
// box leaves the job stoppable, and a relaunch's job starts stoppable again.
func testAStopOfAJobACollectionReadIsRefused(t *testing.T, s sandbox.PendingStore) {
	ctx := t.Context()
	stopIt := func(turnID, launch string) (sandbox.PendingRun, bool, error) {
		stop := sandbox.RunStop{By: "founder"}
		return s.DecideEnding(ctx, turnID, sandbox.Decision{
			License: sandbox.License{WhileIn: sandbox.Stoppable, Launch: launch}, Reclaim: true, Stop: &stop,
		})
	}

	launch := mustLaunched(t, s, run("t1"))
	release := releaseOf(mustClaim(t, s, "t1"))
	release.Collected = true
	if released, err := s.ReleaseClaim(ctx, "t1", release); err != nil || !released {
		t.Fatalf("release t1 collected = %v, %v", released, err)
	}
	if got := mustGet(t, s, "t1"); got.Status != sandbox.StatusRunning || !got.Launch.Collected {
		t.Fatalf("the collected job's row reads status %q collected=%v, want running and collected",
			got.Status, got.Launch.Collected)
	}
	if decided, ok, err := stopIt("t1", launch); err != nil || ok || decided.Ending != nil {
		t.Fatalf("a stop of a collected job = %+v, %v, %v, want refused", decided.Ending, ok, err)
	}
	if got := mustGet(t, s, "t1"); got.Ending != nil || got.Stop != nil {
		t.Fatalf("the refused stop left ending %+v and stop %+v on the row", got.Ending, got.Stop)
	}
	// AN ENDING THAT IS NOT A STOP STILL ENDS IT: the refusal is the stop's.
	if got := decide(t, s, "t1", everyJob(sandbox.Fence{}, sandbox.Active)); got.Stop != nil {
		t.Fatalf("the ordinary ending of a collected job recorded a stop: %+v", got.Stop)
	}

	// A RELEASE THAT DID NOT READ THE BOX LEAVES THE JOB OUT, and stoppable.
	launch2 := mustLaunched(t, s, run("t2"))
	failed := releaseOf(mustClaim(t, s, "t2"))
	failed.CollectFailedAt = base
	if released, err := s.ReleaseClaim(ctx, "t2", failed); err != nil || !released {
		t.Fatalf("release t2 failed collection = %v, %v", released, err)
	}
	if decided, ok, err := stopIt("t2", launch2); err != nil || !ok || decided.Stop == nil {
		t.Fatalf("a stop of a job no collection read = %+v, %v, %v, want it decided", decided.Stop, ok, err)
	}

	// A RELAUNCH'S JOB IS OUT AGAIN: the record is the job's, not the row's.
	mustLaunched(t, s, run("t3"))
	collected := releaseOf(mustClaim(t, s, "t3"))
	collected.Collected = true
	if released, err := s.ReleaseClaim(ctx, "t3", collected); err != nil || !released {
		t.Fatalf("release t3 collected = %v, %v", released, err)
	}
	relaunch := mustLaunched(t, s, run("t3"))
	if got := mustGet(t, s, "t3"); got.Launch.Collected {
		t.Fatal("a relaunch's job inherited the collection of the job before it")
	}
	if decided, ok, err := stopIt("t3", relaunch); err != nil || !ok || decided.Stop == nil {
		t.Fatalf("a stop of the relaunched job = %+v, %v, %v, want it decided", decided.Stop, ok, err)
	}
}

// A RELEASE COUNTS A FAILED RESUME ONTO THE JOB, dated from the first, so the
// give-up that bounds a resume which keeps failing holds across nodes and
// restarts ([sandbox.ResumeGiveUp]). A release that reports no failed resume
// (one held on a condition) counts nothing, and a relaunch's job starts with
// no failures of the job before it.
func testAReleaseCountsAFailedResumeOntoTheJob(t *testing.T, s sandbox.PendingStore) {
	ctx := t.Context()
	mustLaunched(t, s, run("t1"))
	for i, at := range []time.Time{base, base.Add(20 * time.Minute)} {
		release := releaseOf(mustClaim(t, s, "t1"))
		release.Collected, release.ResumeFailedAt = true, at
		if released, err := s.ReleaseClaim(ctx, "t1", release); err != nil || !released {
			t.Fatalf("release t1 failed resume %d = %v, %v", i+1, released, err)
		}
	}
	got := mustGet(t, s, "t1")
	if got.Launch.ResumeFailures != 2 || !got.Launch.ResumeFailingSince.Equal(base) {
		t.Fatalf("the job counts %d failed resumes since %v, want 2 since %v",
			got.Launch.ResumeFailures, got.Launch.ResumeFailingSince, base)
	}
	held := releaseOf(mustClaim(t, s, "t1"))
	held.Collected, held.Hold = true, &sandbox.Refusal{Condition: "provider", Until: base.Add(time.Hour)}
	if released, err := s.ReleaseClaim(ctx, "t1", held); err != nil || !released {
		t.Fatalf("release t1 held = %v, %v", released, err)
	}
	if got := mustGet(t, s, "t1"); got.Launch.ResumeFailures != 2 {
		t.Fatalf("a held resume moved the failed count to %d, want 2", got.Launch.ResumeFailures)
	}
	mustLaunched(t, s, run("t1"))
	if got := mustGet(t, s, "t1"); got.Launch.ResumeFailures != 0 ||
		!got.Launch.ResumeFailingSince.IsZero() {
		t.Fatal("a relaunch's job inherited the failed resumes of the job before it")
	}
}

// A REFUSED RELAUNCH'S WINDOW STAYS ON ITS JOB UNTIL THE NEXT LAUNCH. A resume
// whose relaunch the seat's budget refused below its floor is handed back
// naming the window it was refused in, and the release counts that onto the
// job's record, for the turn's next relaunch to wait for its floor no longer
// than that window: written by a release that names one, held or not (an
// answer's wait is its own series', with no hold on the row), replaced by a
// later one, KEPT by a release that names none, since a resume that failed in
// between tried no relaunch, and never a window that is no window. The
// relaunch that goes out is the one it was kept for, and its job's record
// starts without it.
func testARefusedRelaunchsWindowStaysOnItsJobUntilTheNextLaunch(t *testing.T, s sandbox.PendingStore) {
	ctx := t.Context()
	mustLaunched(t, s, run("t1"))
	day := sandbox.BudgetWindow{Period: "day", Label: "2026-09-23"}
	held := releaseOf(mustClaim(t, s, "t1"))
	// PAST ITS INSTANT, so the next claim takes the run: a hold that still
	// stands refuses every claim (testAHeldRunIsClaimedByNoAnnouncementUntilItsHoldLifts).
	held.Hold = &sandbox.Refusal{Condition: "budget", Until: time.Now().UTC().Add(-time.Minute), Window: day}
	held.RelaunchRefusedIn = day
	if released, err := s.ReleaseClaim(ctx, "t1", held); err != nil || !released {
		t.Fatalf("release t1 held = %v, %v", released, err)
	}
	if got := mustGet(t, s, "t1"); got.Launch.RelaunchRefusedIn != day {
		t.Fatalf("a held release left the job's record naming %+v, want the window %+v its relaunch was "+
			"refused in", got.Launch.RelaunchRefusedIn, day)
	}
	failed := releaseOf(mustClaim(t, s, "t1"))
	failed.ResumeFailedAt = base
	if released, err := s.ReleaseClaim(ctx, "t1", failed); err != nil || !released {
		t.Fatalf("release t1 failed = %v, %v", released, err)
	}
	if got := mustGet(t, s, "t1"); got.Launch.RelaunchRefusedIn != day {
		t.Fatalf("a release naming no window left %+v, want the earlier %+v kept for the relaunch it has not "+
			"tried yet", got.Launch.RelaunchRefusedIn, day)
	}
	next := sandbox.BudgetWindow{Period: "week", Label: "2026-W40"}
	turned := releaseOf(mustClaim(t, s, "t1"))
	turned.RelaunchRefusedIn = next
	if released, err := s.ReleaseClaim(ctx, "t1", turned); err != nil || !released {
		t.Fatalf("release t1 turned back = %v, %v", released, err)
	}
	if got := mustGet(t, s, "t1"); got.Launch.RelaunchRefusedIn != next || got.HeldFor != "" {
		t.Fatalf("a release naming a window and no hold left %+v held for %q, want %+v and no hold",
			got.Launch.RelaunchRefusedIn, got.HeldFor, next)
	}
	for _, none := range []sandbox.BudgetWindow{{Period: "day", Label: "2026-02-30"},
		{Period: "fortnight", Label: "2026-09-23"}, {Period: "day", Label: "2026-W40"}} {
		bogus := releaseOf(mustClaim(t, s, "t1"))
		bogus.RelaunchRefusedIn = none
		if released, err := s.ReleaseClaim(ctx, "t1", bogus); err != nil || !released {
			t.Fatalf("release t1 naming %+v = %v, %v", none, released, err)
		}
		if got := mustGet(t, s, "t1"); got.Launch.RelaunchRefusedIn != next {
			t.Fatalf("a release naming %+v, which is no window, left %+v, want %+v kept", none,
				got.Launch.RelaunchRefusedIn, next)
		}
	}
	mustLaunched(t, s, run("t1"))
	if got := mustGet(t, s, "t1"); got.Launch.RelaunchRefusedIn != (sandbox.BudgetWindow{}) {
		t.Fatalf("a relaunch's job inherited the window %+v the job before it was refused in",
			got.Launch.RelaunchRefusedIn)
	}
}

// A JOB KEEPS THE INSTANT IT WAS FIRST COLLECTED. A later collection of the
// same job — its resume held by the seat's budget for hours, say — reads the
// same ending, and a reset its harness named as a time of day is read against the
// first collection's instant: read against the later one, a 4:30pm already past
// was the next day's, and the entry sat out for nothing. So a release that read
// the box records the instant once, a later one leaves it, a release that did
// not read the box records none, and a relaunch's job starts without one.
func testAJobKeepsTheInstantItWasFirstCollected(t *testing.T, s sandbox.PendingStore) {
	ctx := t.Context()
	mustLaunched(t, s, run("t1"))
	release := releaseOf(mustClaim(t, s, "t1"))
	release.Collected, release.CollectedAt = true, base
	if released, err := s.ReleaseClaim(ctx, "t1", release); err != nil || !released {
		t.Fatalf("release t1 collected = %v, %v", released, err)
	}
	if got := mustGet(t, s, "t1"); !got.Launch.CollectedAt.Equal(base) {
		t.Fatalf("the first collection's instant reads %v, want %v", got.Launch.CollectedAt, base)
	}
	again := releaseOf(mustClaim(t, s, "t1"))
	again.Collected, again.CollectedAt = true, base.Add(3*time.Hour)
	if released, err := s.ReleaseClaim(ctx, "t1", again); err != nil || !released {
		t.Fatalf("release t1 collected again = %v, %v", released, err)
	}
	if got := mustGet(t, s, "t1"); !got.Launch.CollectedAt.Equal(base) {
		t.Fatalf("a later collection moved the job's first instant to %v, want %v",
			got.Launch.CollectedAt, base)
	}

	mustLaunched(t, s, run("t2"))
	failed := releaseOf(mustClaim(t, s, "t2"))
	failed.CollectFailedAt = base
	if released, err := s.ReleaseClaim(ctx, "t2", failed); err != nil || !released {
		t.Fatalf("release t2 failed collection = %v, %v", released, err)
	}
	if got := mustGet(t, s, "t2"); !got.Launch.CollectedAt.IsZero() {
		t.Fatalf("a release that read no box recorded a collection at %v", got.Launch.CollectedAt)
	}

	mustLaunched(t, s, run("t1"))
	if got := mustGet(t, s, "t1"); !got.Launch.CollectedAt.IsZero() {
		t.Fatal("a relaunch's job inherited the collection instant of the job before it")
	}
}

// AN ENDING SAYS WITH ITS DECISION WHETHER IT HANDS A PERSON'S ANSWER BACK, so
// its announcement says so whichever attempt makes it: an answer no turn took
// goes back, one a turn took does not — unless the ending says it went unused —
// and copies the row already owes go back too.
func testAnEndingSaysWhetherItHandsAnAnswerBack(t *testing.T, s sandbox.PendingStore) {
	carried := func(id string) sandbox.RecordedAnswer {
		a := answerOf(id, "use main")
		a.Events = []json.RawMessage{json.RawMessage(`{"id":"` + id + `"}`)}
		return a
	}
	claimedAnswerOn(t, s, "untaken", carried("untaken-r1"), false)
	if got := decide(t, s, "untaken", everyJob(sandbox.Fence{}, sandbox.Active)); !got.Ending.Returned {
		t.Errorf("an ending over an answer no turn took = %+v, want it handing the answer back", got.Ending)
	}
	claimedAnswerOn(t, s, "taken", carried("taken-r1"), true)
	if got := decide(t, s, "taken", everyJob(sandbox.Fence{}, sandbox.Active)); got.Ending.Returned {
		t.Errorf("an ending over an answer a turn took = %+v, want nothing handed back", got.Ending)
	}
	claimedAnswerOn(t, s, "unused", carried("unused-r1"), true)
	unused := everyJob(sandbox.Fence{}, sandbox.Active)
	unused.Unused = true
	if got := decide(t, s, "unused", unused); !got.Ending.Returned || !got.Ending.Unused {
		t.Errorf("an ending whose answer went unused = %+v, want it handing the answer back", got.Ending)
	}
}

// Done and failed are not states a record can be written in, because a run
// that reached either has no record. Refused at the write, so a caller that
// reaches for the old terminal flip fails loudly instead of leaving a record
// every listing skips and nothing ever deletes.
func testAnEndingIsNotAStatus(t *testing.T, s sandbox.PendingStore) {
	mustLaunched(t, s, run("t1"))
	for _, status := range []string{"done", "failed", ""} {
		if err := s.SetStatus(t.Context(), "t1", status, sandbox.Fence{}); err == nil {
			t.Errorf("SetStatus(%q) was accepted", status)
		}
	}
	if got := mustGet(t, s, "t1"); got.Status != sandbox.StatusRunning {
		t.Errorf("a refused status changed the record to %q", got.Status)
	}
}
