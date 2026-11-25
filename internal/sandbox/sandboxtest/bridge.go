package sandboxtest

import (
	"fmt"
	"slices"
	"sync"
	"testing"

	"github.com/crewlet/crewlet/internal/sandbox"
)

// --- the bridged run's durable tool log ------------------------------------

// A BRIDGED RUN'S CALLS HAVE NOWHERE ELSE TO LIVE. An engine phase's calls sit
// on a surface in memory and the turn writes them when it ends; a run's are
// made by its harness, outside the engine, and can outlive the node. A
// reviewer of a resumed run with no log judges a turn that reads as having
// acted on nothing.
func testBridgeCallsAreAppendedInOrder(t *testing.T, s sandbox.PendingStore) {
	ctx := t.Context()
	r := run("t-bridge")
	launch := mustLaunched(t, s, r)

	for _, name := range []string{"read_page", "post_message", "read_page"} {
		recorded, err := s.AppendBridgeCall(ctx, r.TurnID, sandbox.BridgeAppend{
			Launch: launch,
			Call:   sandbox.BridgeCall{Name: name, Args: `{"id":1}`, Output: name + " ok"},
		})
		if err != nil || !recorded {
			t.Fatalf("AppendBridgeCall(%s) = %v, %v", name, recorded, err)
		}
	}

	got := mustLogged(t, s, r.TurnID)
	if len(got.Log.Calls) != 3 {
		t.Fatalf("%d calls recorded: %+v", len(got.Log.Calls), got.Log.Calls)
	}
	want := []string{"read_page", "post_message", "read_page"}
	for i, name := range want {
		if got.Log.Calls[i].Name != name {
			t.Errorf("call %d = %q, want %q", i, got.Log.Calls[i].Name, name)
		}
	}
	// The ARGUMENTS and the OUTCOME ride along, because a log of bare
	// names does not tell a reviewer whether the turn delivered anything.
	if got.Log.Calls[0].Args != `{"id":1}` || got.Log.Calls[0].Output != "read_page ok" {
		t.Errorf("the call does not carry what it did: %+v", got.Log.Calls[0])
	}
	// STAMPED, so a reader can see the shape of a run that stalled.
	if got.Log.Calls[0].At.IsZero() {
		t.Error("the call has no timestamp")
	}
}

// NO FENCE, unlike every other mutation on this row. The call already ran and
// its effect already happened; refusing to record it because the seat's lease
// moved mid-run would lose evidence of something that is true either way.
func testBridgeCallsSurviveWithoutAFence(t *testing.T, s sandbox.PendingStore) {
	ctx := t.Context()
	r := run("t-bridge-fence")
	launch := mustLaunched(t, s, r)
	// Move the run under a NEWER owner, so the caller's own view of the
	// lease is stale by any measure.
	if ok, err := s.ClaimOwnership(ctx, r.TurnID, "node-b", 99); err != nil || !ok {
		t.Fatalf("ClaimOwnership = %v, %v", ok, err)
	}

	recorded, err := s.AppendBridgeCall(ctx, r.TurnID, sandbox.BridgeAppend{
		Launch: launch, Call: sandbox.BridgeCall{Name: "read_page"}})
	if err != nil || !recorded {
		t.Fatalf("a log append was refused by ownership: %v, %v", recorded, err)
	}
	if got := mustLogged(t, s, r.TurnID); len(got.Log.Calls) != 1 {
		t.Errorf("%d calls recorded", len(got.Log.Calls))
	}
}

// EVERY BRIDGED CALL LANDS, HOWEVER MANY WRITE AT ONCE. An executor's tool
// calls reach its row from as many as twenty subagents at once — Claude Code's
// default — and the seat's own lease and status writes race them. Each append
// is a decision the store takes again on a lost race, so none of them is
// dropped, none overwrites another, and none is answered "the record kept
// changing": the sixteen attempts this store once counted could answer exactly
// that of a healthy, busy row.
//
// Mutation: have the store give up on a write after its first lost race, and
// appends fail here.
func testManyBridgedWritersAllLand(t *testing.T, s sandbox.PendingStore) {
	const writers, each = 20, 3
	ctx := t.Context()
	r := run("t-bridge-storm")
	launch := mustLaunched(t, s, r)

	var wg sync.WaitGroup
	errs := make(chan error, writers*each+2)
	for w := range writers {
		wg.Go(func() {
			for n := range each {
				name := fmt.Sprintf("agent-%02d-call-%d", w, n)
				recorded, err := s.AppendBridgeCall(ctx, r.TurnID, sandbox.BridgeAppend{
					Launch: launch, Call: sandbox.BridgeCall{Name: name}})
				switch {
				case err != nil:
					errs <- fmt.Errorf("append %s: %w", name, err)
				case !recorded:
					errs <- fmt.Errorf("append %s was not recorded", name)
				}
			}
		})
	}
	// The seat's own writes, racing the appends: a lease moving onto the
	// run, and the status a recovery writes back.
	wg.Go(func() {
		for epoch := int64(1); epoch <= each; epoch++ {
			switch ok, err := s.ClaimOwnership(ctx, r.TurnID, "node-b", epoch); {
			case err != nil:
				errs <- fmt.Errorf("ClaimOwnership at epoch %d: %w", epoch, err)
			case !ok:
				errs <- fmt.Errorf("ClaimOwnership at epoch %d was refused", epoch)
			}
		}
	})
	wg.Go(func() {
		for range each {
			if err := s.SetStatus(ctx, r.TurnID, sandbox.StatusRunning, sandbox.Fence{}); err != nil {
				errs <- fmt.Errorf("SetStatus: %w", err)
			}
		}
	})
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Errorf("a write racing the bridged calls failed: %v", err)
	}

	got := mustLogged(t, s, r.TurnID)
	var names []string
	for _, call := range got.Log.Calls {
		names = append(names, call.Name)
	}
	slices.Sort(names)
	var want []string
	for w := range writers {
		for n := range each {
			want = append(want, fmt.Sprintf("agent-%02d-call-%d", w, n))
		}
	}
	if !slices.Equal(names, want) {
		t.Fatalf("the log holds %d calls after %d concurrent appends, want every one exactly once: %v",
			len(names), len(want), names)
	}
	if got.Owner != "node-b" || got.OwnerEpoch != each {
		t.Errorf("owner = %q at epoch %d, want node-b at %d: a lease write was lost among the appends",
			got.Owner, got.OwnerEpoch, each)
	}
}

// A LATE CALL FROM A BOX THAT IS SHUTTING DOWN is the ordinary shape here, and
// it must not be an error: the caller cannot fail the box's call over a log
// row, so an answer of "not recorded" has to be as safe to ignore as one.
func testBridgeCallsForAMissingRunAreDropped(t *testing.T, s sandbox.PendingStore) {
	recorded, err := s.AppendBridgeCall(t.Context(), "never-existed", sandbox.BridgeAppend{
		Launch: "a-job-that-was", Call: sandbox.BridgeCall{Name: "read_page"}})
	if err != nil {
		t.Fatalf("a missing run was an error: %v", err)
	}
	if recorded {
		t.Error("an append onto no row reported being recorded")
	}
}

// THE MIDDLE IS WHAT GETS DROPPED. How a run began and how it ended are what
// explain it, and a log truncated to its last N loses the former entirely.
func testBridgeCallsDropTheMiddleNotTheStart(t *testing.T, s sandbox.PendingStore) {
	ctx := t.Context()
	r := run("t-bridge-cap")
	launch := mustLaunched(t, s, r)

	total := sandbox.MaxBridgeCalls + 10
	for i := range total {
		if _, err := s.AppendBridgeCall(ctx, r.TurnID, sandbox.BridgeAppend{
			Launch: launch, Call: sandbox.BridgeCall{Name: fmt.Sprintf("call-%03d", i)},
		}); err != nil {
			t.Fatalf("append %d: %v", i, err)
		}
	}

	got := mustLogged(t, s, r.TurnID)
	if len(got.Log.Calls) != sandbox.MaxBridgeCalls {
		t.Fatalf("%d calls kept, want %d", len(got.Log.Calls), sandbox.MaxBridgeCalls)
	}
	if first := got.Log.Calls[0].Name; first != "call-000" {
		t.Errorf("the first call was dropped: %q — a log cut to its tail loses how the run began", first)
	}
	last := got.Log.Calls[len(got.Log.Calls)-1].Name
	if want := fmt.Sprintf("call-%03d", total-1); last != want {
		t.Errorf("the last call = %q, want %q", last, want)
	}
	// AND THE GAP IS REPORTED: a log that silently skips is a log that
	// lies about what the run did.
	if got.Log.Elided != 10 {
		t.Errorf("elided = %d, want 10", got.Log.Elided)
	}
}

// bridged is a session's running total after n calls, every figure distinct.
func bridged(n int) sandbox.EngineSpend {
	return sandbox.EngineSpend{
		Aux:     sandbox.AuxTokens{Input: 100 * n, Output: 10 * n, CacheRead: 40 * n, CacheWrite: 4 * n},
		Workers: n, WorkerInput: 1000 * n, WorkerOutput: 70 * n,
	}
}

// WHAT A BRIDGED RUN'S CALLS COST THE ENGINE RIDES ITS CALLS, AS THE NEWEST
// TOTAL. Each append carries the session's running total, and two calls that
// finish together race to the row — so the total an earlier call read can land
// after a later one's. The job's record keeps the largest of each figure, which
// within one session is the latest, and no append moves it backwards. The
// segment that resumes from the job pays it; without it on the row, a resume on
// another node or after a restart would have nothing to pay from.
func testABridgedRunsSpendIsTheNewestTotal(t *testing.T, s sandbox.PendingStore) {
	ctx := t.Context()
	r := run("t-bridge-spend")
	launch := mustLaunched(t, s, r)

	// The third call's total lands before the second's.
	for _, n := range []int{1, 3, 2} {
		if recorded, err := s.AppendBridgeCall(ctx, r.TurnID, sandbox.BridgeAppend{
			Launch: launch, Call: sandbox.BridgeCall{Name: "refresh_memory"}, Spent: bridged(n),
		}); err != nil || !recorded {
			t.Fatalf("append %d = %v, %v; want it recorded on job %q", n, recorded, err, launch)
		}
	}
	if got := mustGet(t, s, r.TurnID).Launch.Bridged; got != bridged(3) {
		t.Fatalf("the job's bridged spend = %+v, want the newest total %+v", got, bridged(3))
	}
}

// A ROW A LATER JOB MOVED ON READS NONE OF ITS CALLS. A reader holds the row it
// read — a resume's claim, a board's listing — and reads the log after it, by
// which time a relaunch may have opened the log to the next job and that job
// may have called. The log is the next job's, and the earlier row reads it as
// none ([sandbox.PendingStore.BridgeLog]), its own job named: replayed as the
// earlier job's, the next job's calls would be judged as work it did.
//
// Mutation: drop BridgeLog's job filter, and the earlier row reads the next
// job's call.
func testARowALaterJobMovedOnReadsNoneOfItsCalls(t *testing.T, s sandbox.PendingStore) {
	ctx := t.Context()
	const turn = "t-log-moved-on"
	mustLaunched(t, s, run(turn))
	earlier := mustGet(t, s, turn)
	next := mustBeginLaunch(t, s, run(turn))
	if ok, err := s.AppendBridgeCall(ctx, turn, sandbox.BridgeAppend{Launch: next,
		Call: sandbox.BridgeCall{Name: "read_file", Seq: 1, At: base}}); err != nil || !ok {
		t.Fatalf("the next job's call = %v, %v", ok, err)
	}
	got, err := s.BridgeLog(ctx, earlier)
	if err != nil {
		t.Fatalf("BridgeLog: %v", err)
	}
	if len(got.Calls) != 0 || got.Launch != earlier.LaunchID {
		t.Errorf("the earlier row read %d calls under %q, want none under its own job %q",
			len(got.Calls), got.Launch, earlier.LaunchID)
	}
}

// AN APPEND NAMING NO JOB IS RECORDED UNDER NONE. The store used to read an
// empty job as "whichever the row holds", which was how a late first call
// reached the next job; nothing in this build sends one, so an empty name is
// a caller's mistake and lands nowhere rather than somewhere.
func testABridgedCallNamingNoJobIsNotRecorded(t *testing.T, s sandbox.PendingStore) {
	r := run("t-bridge-unnamed")
	mustLaunched(t, s, r)
	recorded, err := s.AppendBridgeCall(t.Context(), r.TurnID, sandbox.BridgeAppend{
		Call: sandbox.BridgeCall{Name: "read_page"}, Spent: bridged(1)})
	if err != nil {
		t.Fatalf("an unnamed append was an error: %v", err)
	}
	got := mustLogged(t, s, r.TurnID)
	if recorded || len(got.Log.Calls) != 0 || got.Launch.Bridged != (sandbox.EngineSpend{}) {
		t.Fatalf("an append naming no job was recorded (%v): calls %+v, spend %+v",
			recorded, got.Log.Calls, got.Launch.Bridged)
	}
}

// A CALL THAT FINISHES AFTER ITS JOB'S RESUME CLAIMED IT, and before any
// relaunch, stays on its own job: the row still holds that job, and the call is
// evidence — a resume that fails and hands its claim back is collected again,
// and that claim reads it. What the claim that already won read is what its
// segment pays, so this call's spend is not in it: a late call leaves the task
// short of the turn's cost, never past it.
func testABridgedCallAfterItsJobsClaimStaysOnItsJob(t *testing.T, s sandbox.PendingStore) {
	ctx := t.Context()
	r := run("t-bridge-after-claim")
	launch := mustLaunched(t, s, r)
	if recorded, err := s.AppendBridgeCall(ctx, r.TurnID, sandbox.BridgeAppend{
		Launch: launch, Call: sandbox.BridgeCall{Name: "read_page"}, Spent: bridged(1),
	}); err != nil || !recorded {
		t.Fatalf("the job's own call = %v, %v", recorded, err)
	}
	claimed := mustClaim(t, s, r.TurnID)

	recorded, err := s.AppendBridgeCall(ctx, r.TurnID, sandbox.BridgeAppend{
		Launch: launch, Call: sandbox.BridgeCall{Name: "delegate"}, Spent: bridged(2)})
	if err != nil || !recorded {
		t.Fatalf("a call after the claim, before any relaunch = %v, %v; want it on its own job",
			recorded, err)
	}
	if got := claimed.Launch.Bridged; got != bridged(1) {
		t.Errorf("the claim read %+v, want what the job held when it was claimed: %+v", got, bridged(1))
	}
	got := mustLogged(t, s, r.TurnID)
	if len(got.Log.Calls) != 2 || got.Launch.Bridged != bridged(2) {
		t.Fatalf("the job's record after the late call: calls %d, spend %+v; want 2 and %+v",
			len(got.Log.Calls), got.Launch.Bridged, bridged(2))
	}
}

// WHAT CONDENSING A PARKED JOB'S COLLECTION COST IS PARKED WITH ITS QUESTION,
// on the job's own record, for the resume the answer drives — which may run
// days later on another node, with nothing collected — exactly as the job's own
// tokens are. A second launch on the turn is a new job, and carries none of it.
func testAParkedJobsCondensationIsKeptForItsAnswer(t *testing.T, s sandbox.PendingStore) {
	ctx := t.Context()
	r := run("t-park-condensed")
	mustLaunched(t, s, r)
	cost := sandbox.AuxTokens{Input: 900, Output: 60, CacheRead: 300}
	if err := s.MarkAwaiting(ctx, r.TurnID, sandbox.Clarification{
		AskedAt: base, InputTokens: 5000, OutputTokens: 400, Condensed: cost,
	}); err != nil {
		t.Fatalf("MarkAwaiting: %v", err)
	}
	if got := mustGet(t, s, r.TurnID).Launch.Condensed; got != cost {
		t.Fatalf("the parked job's condensation = %+v, want %+v", got, cost)
	}
	mustBeginLaunch(t, s, run("t-park-condensed"))
	if got := mustGet(t, s, r.TurnID).Launch.Condensed; got != (sandbox.AuxTokens{}) {
		t.Fatalf("a new job carries the last one's condensation: %+v", got)
	}
}
