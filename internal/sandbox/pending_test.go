package sandbox_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/coord/memory"
	"github.com/crewlet/crewlet/internal/logging"
	"github.com/crewlet/crewlet/internal/queue"
	"github.com/crewlet/crewlet/internal/sandbox"
	"github.com/crewlet/crewlet/internal/sandbox/sandboxtest"
	"github.com/crewlet/crewlet/internal/workkey"
)

// TestMain silences the engine logger. Every Open logs a line per applied
// schema file and the suite opens a database per subtest — several hundred
// lines of successful boot ahead of the one that says what failed.
func TestMain(m *testing.M) {
	logging.Configure(slog.LevelError, logging.FormatText, io.Discard)
	os.Exit(m.Run())
}

func TestPendingStoreContract(t *testing.T) {
	t.Parallel()
	// ONE implementation now — the run record lives in the fleet's
	// coordination store, because a detached run is recovered by whichever
	// node owns its seat NEXT. The record's own semantics are certified
	// against both coordination backends by internal/coord/coordtest; what
	// this suite covers is everything built on top of them, which is all of
	// the conditional flips: the at-most-once tail claim, the epoch fence,
	// the pause expiry.
	sandboxtest.Run(t, func(*testing.T) sandbox.PendingStore {
		return sandbox.NewCoordStore(memory.NewFleet())
	})
}

// A ROW PARKED BEFORE THE SPLIT STILL KNOWS ITS UNIT OF WORK.
//
// Nothing rewrites a parked row, so a run suspended by a build from before
// ADR-0017 carries no `work_key` and its `turn_id` IS one. A resume days later
// has no trigger left to re-derive from, so reading the raw field would dedupe
// its conversation entry and its tracker writes against nothing.
func TestAPreSplitRunStillAnswersForItsUnitOfWork(t *testing.T) {
	t.Parallel()
	key := workkey.Derive([]string{"evt-a"})
	for name, tc := range map[string]struct {
		run  sandbox.PendingRun
		want string
	}{
		"post-split, keyed":  {sandbox.PendingRun{TurnID: uuid.NewString(), WorkKey: key}, key},
		"pre-split row":      {sandbox.PendingRun{TurnID: key}, key},
		"post-split, no key": {sandbox.PendingRun{TurnID: uuid.NewString()}, ""},
	} {
		if got := tc.run.UnitOfWork(); got != tc.want {
			t.Errorf("%s: UnitOfWork = %q, want %q", name, got, tc.want)
		}
	}
}

// --- the bridged-call log on the coordination store ------------------------

// begun opens a launch on a fresh store and returns the row it wrote.
func begun(t *testing.T, store *sandbox.CoordStore, turnID string) sandbox.PendingRun {
	t.Helper()
	if err := store.BeginLaunch(t.Context(), sandbox.PendingRun{
		TurnID: turnID, AgentHandle: "swe", CodingAgent: "claude-code",
	}, sandbox.Fence{}); err != nil {
		t.Fatalf("BeginLaunch(%s): %v", turnID, err)
	}
	run, found, err := store.Get(t.Context(), turnID)
	if err != nil || !found {
		t.Fatalf("Get(%s) = %v, %v", turnID, found, err)
	}
	return run
}

func appended(t *testing.T, store *sandbox.CoordStore, turnID, name string) {
	t.Helper()
	if ok, err := store.AppendBridgeCall(t.Context(), turnID, sandbox.BridgeCall{Name: name}); err != nil || !ok {
		t.Fatalf("AppendBridgeCall(%s, %s) = %v, %v", turnID, name, ok, err)
	}
}

func launchesOf(t *testing.T, fleet coord.BridgeCalls) []coord.BridgeLaunch {
	t.Helper()
	got, err := fleet.BridgeLaunches(t.Context())
	if err != nil {
		t.Fatalf("BridgeLaunches: %v", err)
	}
	return got
}

// A LAUNCH AN OLDER BUILD RECORDED IS READ FROM THE RUN'S ROW.
//
// A rolling upgrade puts builds that know only the row's list on the same
// coordination store, and the node that collects a run need not be the one
// that launched it. Such a launch has no per-call records at all, so reading
// only the records would hand its resume an empty log — and "this run called
// nothing" is what the delivery check reads as a turn that reached nobody.
func TestALaunchAnOlderBuildRecordedIsReadFromItsRow(t *testing.T) {
	t.Parallel()
	fleet := memory.NewFleet()
	store := sandbox.NewCoordStore(fleet)
	// The row exactly as a build that kept its calls on it wrote it.
	raw, err := json.Marshal(map[string]any{
		"turn_id": "t-older", "status": sandbox.StatusRunning, "launch_id": "launch-older",
		"agent_handle": "swe",
		"bridge_calls": []map[string]any{
			{"name": "slack_post", "output": "posted", "at": "2026-08-23T12:00:00Z"},
			{"name": "submit_work", "args": `{"outcome":"delivered"}`, "at": "2026-08-23T12:01:00Z"},
		},
		"bridge_calls_elided": 3,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fleet.CreateSandboxRun(t.Context(), "t-older", raw); err != nil {
		t.Fatalf("CreateSandboxRun: %v", err)
	}
	run, _, err := store.Get(t.Context(), "t-older")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}

	log, err := store.BridgeCalls(t.Context(), run)
	if err != nil {
		t.Fatalf("BridgeCalls: %v", err)
	}
	calls := log.Calls
	if len(calls) != 2 || calls[0].Name != "slack_post" || calls[1].Name != "submit_work" {
		t.Fatalf("calls = %+v, want the two the older build kept on the row", calls)
	}
	// THE MIDDLE IT DROPPED IS REPORTED, NOT PASSED OFF. Those calls are kept
	// nowhere, and a resume handed the two that survived as every call would
	// read a delivery among the three as never made.
	if log.Dropped != 3 || log.DroppedAfter != 2 {
		t.Errorf("dropped = %d after %d, want the row's 3, after the calls it kept first",
			log.Dropped, log.DroppedAfter)
	}
	page, err := store.BridgeCallPage(t.Context(), run, 0, 1)
	if err != nil {
		t.Fatalf("BridgeCallPage: %v", err)
	}
	if len(page.Calls) != 1 || page.Total != 2 || page.Dropped != 3 || page.Next != 1 {
		t.Errorf("first page = %+v, want one of two calls, the row's dropped three, and a cursor", page)
	}
	if len(page.End) != 1 || page.End[0].Name != "submit_work" || page.Between != 0 {
		t.Errorf("the first page's end = %+v with %d between, want the submission and nothing between",
			page.End, page.Between)
	}
	next, err := store.BridgeCallPage(t.Context(), run, page.Next, 1)
	if err != nil {
		t.Fatalf("BridgeCallPage: %v", err)
	}
	if len(next.Calls) != 1 || next.Calls[0].Name != "submit_work" || next.Next != 0 || len(next.End) != 0 {
		t.Errorf("second page = %+v, want the last call, no cursor and no end beside it", next)
	}
}

// THE DROPPED MIDDLE IS PLACED WHERE THE OLDER BUILD DROPPED IT. Its bounded
// list keeps its first MaxBridgeCalls/2 calls and then its newest, so a gap in
// a full list falls after the first half — which is where a reader has to be
// told it is, or the calls on either side of it read as consecutive.
func TestAFullRowsDroppedMiddleIsPlacedAfterItsFirstHalf(t *testing.T) {
	t.Parallel()
	fleet := memory.NewFleet()
	kept := make([]map[string]any, 0, sandbox.MaxBridgeCalls)
	for i := range sandbox.MaxBridgeCalls {
		kept = append(kept, map[string]any{"name": fmt.Sprintf("c%03d", i), "at": "2026-08-23T12:00:00Z"})
	}
	raw, err := json.Marshal(map[string]any{
		"turn_id": "t-older-full", "status": sandbox.StatusRunning, "launch_id": "launch-older",
		"bridge_calls": kept, "bridge_calls_elided": 41,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fleet.CreateSandboxRun(t.Context(), "t-older-full", raw); err != nil {
		t.Fatalf("CreateSandboxRun: %v", err)
	}
	store := sandbox.NewCoordStore(fleet)
	run, _, err := store.Get(t.Context(), "t-older-full")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	log, err := store.BridgeCalls(t.Context(), run)
	if err != nil {
		t.Fatalf("BridgeCalls: %v", err)
	}
	if len(log.Calls) != sandbox.MaxBridgeCalls || log.Dropped != 41 ||
		log.DroppedAfter != sandbox.MaxBridgeCalls/2 {
		t.Errorf("log = %d calls, %d dropped after %d; want %d, 41 after %d",
			len(log.Calls), log.Dropped, log.DroppedAfter,
			sandbox.MaxBridgeCalls, sandbox.MaxBridgeCalls/2)
	}
}

// A FINISHED RUN TAKES ITS CALLS WITH IT. Nothing asks about a run that has
// ended, and in a bucket with no age a log left behind is kept for the life of
// the deployment.
func TestFinishingARunPurgesItsCalls(t *testing.T) {
	t.Parallel()
	fleet := memory.NewFleet()
	store := sandbox.NewCoordStore(fleet)
	begun(t, store, "t-finish")
	appended(t, store, "t-finish", "read_page")

	if gone, err := store.Finish(t.Context(), "t-finish", sandbox.Fence{}); err != nil || !gone {
		t.Fatalf("Finish = %v, %v", gone, err)
	}
	if got := launchesOf(t, fleet); len(got) != 0 {
		t.Errorf("the finished run's calls survived it: %+v", got)
	}
}

// A RELAUNCH TAKES THE REPLACED LAUNCH'S CALLS, and nothing else. The new
// launch's log is its own from the first call, and the old one is read by
// nothing once the row names the new launch.
func TestARelaunchPurgesTheReplacedLaunchsCalls(t *testing.T) {
	t.Parallel()
	fleet := memory.NewFleet()
	store := sandbox.NewCoordStore(fleet)
	first := begun(t, store, "t-relaunch")
	appended(t, store, "t-relaunch", "submit_work")

	second := begun(t, store, "t-relaunch")
	if second.LaunchID == first.LaunchID {
		t.Fatal("the relaunch kept the first launch's name")
	}
	if got := launchesOf(t, fleet); len(got) != 0 {
		t.Errorf("the replaced launch's calls survived the relaunch: %+v", got)
	}
	appended(t, store, "t-relaunch", "read_page")
	want := []coord.BridgeLaunch{{TurnID: "t-relaunch", LaunchID: second.LaunchID}}
	if got := launchesOf(t, fleet); !slices.Equal(got, want) {
		t.Errorf("launches = %+v, want only the new one", got)
	}
}

// THE SWEEP PURGES WHAT NO RUN NAMES, AND ONLY THAT. A live run's log is what
// its resume will be judged on, however old; a launch whose run finished, or
// that a relaunch replaced, is read by nothing.
func TestTheSweepPurgesOnlyTheLaunchesNoRunNames(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	fleet := memory.NewFleet()
	store := sandbox.NewCoordStore(fleet)
	live := begun(t, store, "t-live")
	appended(t, store, "t-live", "read_page")
	// A run that finished on a node that died before its purge.
	if _, err := fleet.AppendBridgeCall(ctx, "t-gone", "launch-gone", []byte(`{"name":"x"}`)); err != nil {
		t.Fatal(err)
	}
	// A launch of the LIVE run that a relaunch replaced without purging.
	if _, err := fleet.AppendBridgeCall(ctx, "t-live", "launch-replaced", []byte(`{"name":"x"}`)); err != nil {
		t.Fatal(err)
	}

	purged, err := store.SweepBridgeCalls(ctx)
	if err != nil {
		t.Fatalf("SweepBridgeCalls: %v", err)
	}
	if purged != 2 {
		t.Errorf("purged %d launches, want the 2 no run names", purged)
	}
	want := []coord.BridgeLaunch{{TurnID: "t-live", LaunchID: live.LaunchID}}
	if got := launchesOf(t, fleet); !slices.Equal(got, want) {
		t.Errorf("launches after the sweep = %+v, want only the live run's", got)
	}
	if log, err := store.BridgeCalls(ctx, live); err != nil || len(log.Calls) != 1 {
		t.Errorf("the live run's log = %+v, %v; want its one call", log, err)
	}
}

// rowRefused is a coordination store whose run records cannot be updated —
// what a row past the transport's payload ceiling answers.
type rowRefused struct{ *memory.Fleet }

func (rowRefused) UpdateSandboxRun(context.Context, string, []byte, uint64) (bool, error) {
	return false, errors.New("nats: maximum payload exceeded")
}

// THE RECORD IS THE CALL; the row's list is older builds' view of it. A row
// that can no longer be written — its list past the transport's ceiling —
// loses the call from that view only, and the append still succeeds, because
// every reader in this build reads the record.
func TestARowThatCannotBeWrittenDoesNotLoseTheCall(t *testing.T) {
	t.Parallel()
	fleet := rowRefused{memory.NewFleet()}
	store := sandbox.NewCoordStore(fleet)
	// BeginLaunch creates; it does not update.
	run := begun(t, store, "t-full-row")

	ok, err := store.AppendBridgeCall(t.Context(), "t-full-row", sandbox.BridgeCall{Name: "slack_post"})
	if err != nil || !ok {
		t.Fatalf("an append whose row write failed was reported as failed: %v, %v", ok, err)
	}
	log, err := store.BridgeCalls(t.Context(), run)
	if err != nil || len(log.Calls) != 1 || log.Calls[0].Name != "slack_post" {
		t.Fatalf("the log = %+v, %v; want the call, recorded", log, err)
	}
}

// recordRefused is a coordination store that cannot take a call record.
type recordRefused struct{ *memory.Fleet }

func (recordRefused) AppendBridgeCall(context.Context, string, string, []byte) (uint64, error) {
	return 0, coord.ErrUnavailable
}

// A CALL THE RECORDS COULD NOT TAKE IS NOT PUT ON THE ROW EITHER, and the
// append says it failed. The row's list never holds a call the records do not:
// that is what lets a reader take a launch with no records at all to be one an
// older build recorded, and read it from the row.
func TestACallTheRecordsRefusedIsNotOnTheRow(t *testing.T) {
	t.Parallel()
	fleet := recordRefused{memory.NewFleet()}
	store := sandbox.NewCoordStore(fleet)
	begun(t, store, "t-no-record")

	if _, err := store.AppendBridgeCall(t.Context(), "t-no-record", sandbox.BridgeCall{Name: "slack_post"}); err == nil {
		t.Fatal("an append whose record failed reported success")
	}
	run, _, err := store.Get(t.Context(), "t-no-record")
	if err != nil {
		t.Fatal(err)
	}
	if len(run.BridgeCalls) != 0 {
		t.Errorf("the row's list holds %d calls the records do not", len(run.BridgeCalls))
	}
}

// OLDER BUILDS' VIEW NEVER FILLS THE ROW THE LIFECYCLE WRITES.
//
// The row is one message, rewritten whole by every mutation — the claim that
// resumes a run, the park on a question, the release a failed resume hands
// back. A list of calls that grew it to the transport's ceiling would refuse
// every one of those, for every build, and strand the run with its box. So the
// list is kept to a byte budget, dropping from the middle and counting what it
// drops, and the first and newest calls are the ones kept.
func TestTheRowsViewOfTheCallsLeavesTheRowRoom(t *testing.T) {
	t.Parallel()
	store := sandbox.NewCoordStore(memory.NewFleet())
	run := begun(t, store, "t-heavy")
	heavy := strings.Repeat("x", 1<<20)
	const calls = 12
	for i := range calls {
		if ok, err := store.AppendBridgeCall(t.Context(), "t-heavy", sandbox.BridgeCall{
			Name: fmt.Sprintf("c%02d", i), Output: heavy,
		}); err != nil || !ok {
			t.Fatalf("append %d = %v, %v", i, ok, err)
		}
	}
	row, _, err := store.Get(t.Context(), "t-heavy")
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(row)
	if err != nil {
		t.Fatal(err)
	}
	if len(raw) > queue.MaxPayloadBytes/2 {
		t.Errorf("the row weighs %d bytes with the view on it, past half the %d-byte ceiling "+
			"the lifecycle's own writes need room under", len(raw), queue.MaxPayloadBytes)
	}
	if kept := len(row.BridgeCalls); kept == 0 || kept+row.BridgeCallsElided != calls {
		t.Errorf("the view keeps %d calls and counts %d dropped, want every one of %d accounted for",
			kept, row.BridgeCallsElided, calls)
	}
	if row.BridgeCalls[0].Name != "c00" || row.BridgeCalls[len(row.BridgeCalls)-1].Name != "c11" {
		t.Errorf("the view kept %q … %q, want the first call and the newest",
			row.BridgeCalls[0].Name, row.BridgeCalls[len(row.BridgeCalls)-1].Name)
	}
	// And the log this build reads lost nothing to it.
	all, err := store.BridgeCalls(t.Context(), run)
	if err != nil || len(all.Calls) != calls {
		t.Errorf("the log = %d calls, %v; want all %d", len(all.Calls), err, calls)
	}
}

// WHEN THE ROW HAS ROOM FOR ONE CALL, IT IS THE NEWEST. A run ends by
// submitting, and the submission is what an older build's resume replays, so
// of the two ends the view keeps, the one it cannot lose is the last.
func TestARowWithRoomForOneCallKeepsTheNewest(t *testing.T) {
	t.Parallel()
	store := sandbox.NewCoordStore(memory.NewFleet())
	begun(t, store, "t-heaviest")
	heavy := strings.Repeat("x", 3<<20)
	for i := range 3 {
		if ok, err := store.AppendBridgeCall(t.Context(), "t-heaviest", sandbox.BridgeCall{
			Name: fmt.Sprintf("c%d", i), Output: heavy,
		}); err != nil || !ok {
			t.Fatalf("append %d = %v, %v", i, ok, err)
		}
	}
	row, _, err := store.Get(t.Context(), "t-heaviest")
	if err != nil {
		t.Fatal(err)
	}
	if len(row.BridgeCalls) != 1 || row.BridgeCalls[0].Name != "c2" || row.BridgeCallsElided != 2 {
		t.Errorf("the view = %d calls ending %q with %d dropped, want the newest alone and 2 counted",
			len(row.BridgeCalls), row.BridgeCalls[len(row.BridgeCalls)-1].Name, row.BridgeCallsElided)
	}
}
