package sandbox_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

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

// appendedWhole records a call too large for one record, so its whole goes to
// parts filed under its record, and fails the test unless it did.
func appendedWhole(t *testing.T, store *sandbox.CoordStore, turnID string) {
	t.Helper()
	if ok, err := store.AppendBridgeCall(t.Context(), turnID, sandbox.BridgeCall{
		Name: "read_file", Output: strings.Repeat("z", sandbox.MaxBridgeCallBytes+10),
	}); err != nil || !ok {
		t.Fatalf("AppendBridgeCall(%s, the whole of a large read) = %v, %v", turnID, ok, err)
	}
	run, _, err := store.Get(t.Context(), turnID)
	if err != nil {
		t.Fatal(err)
	}
	page, err := store.BridgeCallPage(t.Context(), run, 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	calls := append(page.Calls, page.End...)
	if last := calls[len(calls)-1]; last.WholeParts == 0 {
		t.Fatalf("the large call's record names no parts; the case would test nothing: %+v", last.Name)
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
	// ONE PAGE, whatever the limit, with the dropped stretch placed in it:
	// the list was read whole with the row, and a page that split it would
	// leave the stretch on neither side.
	page, err := store.BridgeCallPage(t.Context(), run, 0, 1)
	if err != nil {
		t.Fatalf("BridgeCallPage: %v", err)
	}
	if got := names(page.Calls); !slices.Equal(got, []string{"slack_post", "submit_work"}) {
		t.Errorf("the page = %q, want both calls the row kept", got)
	}
	if page.Total != 2 || page.Dropped != 3 || page.DroppedAfter != 2 {
		t.Errorf("total = %d, dropped = %d after %d; want 2, and the row's 3 after the calls it kept",
			page.Total, page.Dropped, page.DroppedAfter)
	}
	if page.Next != 0 || len(page.End) != 0 || page.Between != 0 {
		t.Errorf("the page hands out cursor %d, an end of %d and %d between; want none of them",
			page.Next, len(page.End), page.Between)
	}
	later, err := store.BridgeCallPage(t.Context(), run, 1, 1)
	if err != nil {
		t.Fatalf("BridgeCallPage: %v", err)
	}
	if len(later.Calls) != 0 || later.Dropped != 0 {
		t.Errorf("a page past the row's one = %+v, want nothing", later)
	}
}

func names(calls []sandbox.BridgeCall) []string {
	out := make([]string, 0, len(calls))
	for _, call := range calls {
		out = append(out, call.Name)
	}
	return out
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

// A FINISHED RUN TAKES ITS CALLS WITH IT, and the parts of every whole filed
// under them. Nothing asks about a run that has ended, and in a bucket with no
// age a log left behind — or a whole of megabytes under it — is kept for the
// life of the deployment. The twin lists a launch for any key it still holds,
// a lone part included, so an empty listing is nothing left at all.
func TestFinishingARunPurgesItsCalls(t *testing.T) {
	t.Parallel()
	fleet := memory.NewFleet()
	store := sandbox.NewCoordStore(fleet)
	run := begun(t, store, "t-finish")
	appended(t, store, "t-finish", "read_page")
	appendedWhole(t, store, "t-finish")

	if gone, err := store.Finish(t.Context(), "t-finish", sandbox.Fence{}); err != nil || !gone {
		t.Fatalf("Finish = %v, %v", gone, err)
	}
	if got := launchesOf(t, fleet); len(got) != 0 {
		t.Errorf("the finished run's calls survived it: %+v", got)
	}
	if records, err := fleet.BridgeCalls(t.Context(), run.TurnID, run.LaunchID); err != nil || len(records) != 0 {
		t.Errorf("the finished run's launch still holds %d records, %v", len(records), err)
	}
}

// A RELAUNCH TAKES THE REPLACED LAUNCH'S CALLS, their parts with them, and
// nothing else. The new launch's log is its own from the first call, and the
// old one is read by nothing once the row names the new launch.
func TestARelaunchPurgesTheReplacedLaunchsCalls(t *testing.T) {
	t.Parallel()
	fleet := memory.NewFleet()
	store := sandbox.NewCoordStore(fleet)
	first := begun(t, store, "t-relaunch")
	appended(t, store, "t-relaunch", "submit_work")
	appendedWhole(t, store, "t-relaunch")

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
	appendedWhole(t, store, "t-live")
	// A run that finished on a node that died before its purge.
	if _, err := fleet.AppendBridgeCall(ctx, "t-gone", "launch-gone", []byte(`{"name":"x"}`)); err != nil {
		t.Fatal(err)
	}
	// A launch of the LIVE run that a relaunch replaced without purging.
	if _, err := fleet.AppendBridgeCall(ctx, "t-live", "launch-replaced", []byte(`{"name":"x"}`)); err != nil {
		t.Fatal(err)
	}
	// Nothing but a part: one a call filed after its launch was purged.
	if _, err := fleet.CreateBridgeCallPart(ctx, "t-late", "launch-late", 1, 1, []byte("piece")); err != nil {
		t.Fatal(err)
	}

	purged, err := store.SweepBridgeCalls(ctx)
	if err != nil {
		t.Fatalf("SweepBridgeCalls: %v", err)
	}
	if purged != 3 {
		t.Errorf("purged %d launches, want the 3 no run names", purged)
	}
	want := []coord.BridgeLaunch{{TurnID: "t-live", LaunchID: live.LaunchID}}
	if got := launchesOf(t, fleet); !slices.Equal(got, want) {
		t.Errorf("launches after the sweep = %+v, want only the live run's", got)
	}
	// And the live run's log, whole in parts included, is untouched.
	log, err := store.BridgeCalls(ctx, live)
	if err != nil || len(log.Calls) != 2 || len(log.Calls[1].Output) != sandbox.MaxBridgeCallBytes+10 {
		t.Errorf("the live run's log = %d calls, %v; want its two, the second whole", len(log.Calls), err)
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

// lowServer is a coordination store behind a server configured below the
// contract's ceiling: it refuses every record and every part past its limit
// as too large, the way a NATS server with a small max_payload refuses any
// message past it.
type lowServer struct {
	*memory.Fleet
	limit int
}

func (l lowServer) refuses(value []byte) error {
	if len(value) > l.limit {
		return fmt.Errorf("the server accepts %d bytes: %w", l.limit, coord.ErrTooLarge)
	}
	return nil
}

func (l lowServer) AppendBridgeCall(ctx context.Context, turnID, launchID string, value []byte) (uint64, error) {
	if err := l.refuses(value); err != nil {
		return 0, err
	}
	return l.Fleet.AppendBridgeCall(ctx, turnID, launchID, value)
}

func (l lowServer) CreateBridgeCall(ctx context.Context, turnID, launchID string, seq uint64, value []byte) (bool, error) {
	if err := l.refuses(value); err != nil {
		return false, err
	}
	return l.Fleet.CreateBridgeCall(ctx, turnID, launchID, seq, value)
}

func (l lowServer) CreateBridgeCallPart(ctx context.Context, turnID, launchID string, seq uint64, part int, value []byte) (bool, error) {
	if err := l.refuses(value); err != nil {
		return false, err
	}
	return l.Fleet.CreateBridgeCallPart(ctx, turnID, launchID, seq, part, value)
}

// A SERVER BELOW THE CEILING DOES NOT LOSE THE CALL, NOR ITS WHOLE.
//
// A node is refused at boot by a server announcing less than the contract's
// ceiling, but a reconnect can reach one, and it refuses a record the
// contract's ceiling admits. Dropped, the call would leave a gap in the one
// log a resume reads with nothing to say it is there — a post that the
// delivery check never counts. Instead its whole goes to parts SPLIT until the
// server takes them, and its record is its least form beside the reference, so
// the resume still reads every byte of it.
func TestACallAServerBelowTheCeilingRefusesIsKeptWholeInPartsItTakes(t *testing.T) {
	t.Parallel()
	const limit = 100 << 10
	fleet := memory.NewFleet()
	store := sandbox.NewCoordStore(lowServer{Fleet: fleet, limit: limit})
	run := begun(t, store, "t-low")
	sent := sandbox.BridgeCall{
		Name: "slack_post", Args: `{"channel":"C1","text":"` + strings.Repeat("x", 4<<10) + `"}`,
		Output: strings.Repeat("y", 300<<10), At: time.Date(2026, 8, 23, 12, 0, 0, 0, time.UTC),
	}
	if ok, err := store.AppendBridgeCall(t.Context(), "t-low", sent); err != nil || !ok {
		t.Fatalf("a call the server refused as too large was not recorded: %v, %v", ok, err)
	}
	if got := mustBridgeCalls(t, store, run); len(got) != 1 || got[0].Output != sent.Output || got[0].Args != sent.Args {
		t.Fatalf("the call read back = %d calls, want the one call whole", len(got))
	}

	page, err := store.BridgeCallPage(t.Context(), run, 0, 10)
	if err != nil || len(page.Calls) != 1 {
		t.Fatalf("BridgeCallPage = %+v, %v", page, err)
	}
	record := page.Calls[0]
	if record.Args != sandbox.ArgsInParts(len(sent.Args)) || record.Output != "…" {
		t.Errorf("the record = args %.60q, output %.40q; want its least form, both texts marked as "+
			"kept in its parts", record.Args, record.Output)
	}
	if record.WholeParts < 4 {
		t.Errorf("the whole is in %d parts: a part the server refused was not split", record.WholeParts)
	}
	records, err := fleet.BridgeCalls(t.Context(), run.TurnID, run.LaunchID)
	if err != nil || len(records) != 1 {
		t.Fatalf("the fleet holds %d records, %v", len(records), err)
	}
	for _, part := range records[0].Parts {
		if len(part.Value) > limit {
			t.Errorf("part %d is %d bytes, past the %d the server takes", part.Part, len(part.Value), limit)
		}
	}
}

// A SERVER BELOW EVEN THE SPLIT'S FLOOR keeps the call in its least form, and
// the record SAYS the whole was not kept: arguments by their marker, the output
// by a note after its mark, and no reference to parts that are not there.
func TestACallAServerBelowTheFloorRefusesSaysItsWholeWasNotKept(t *testing.T) {
	t.Parallel()
	store := sandbox.NewCoordStore(lowServer{Fleet: memory.NewFleet(), limit: 1 << 10})
	run := begun(t, store, "t-lower")
	args := `{"channel":"C1","text":"` + strings.Repeat("x", 4<<10) + `"}`
	ok, err := store.AppendBridgeCall(t.Context(), "t-lower", sandbox.BridgeCall{
		Name: "slack_post", Args: args, Output: strings.Repeat("y", 4<<10),
	})
	if err != nil || !ok {
		t.Fatalf("a call the server refused as too large was not recorded: %v, %v", ok, err)
	}
	call := mustBridgeCalls(t, store, run)[0]
	if call.Name != "slack_post" || call.Failed {
		t.Errorf("the call kept as %q (failed %v), want the post and its outcome", call.Name, call.Failed)
	}
	if call.Args != sandbox.ArgsNotKept(len(args)) || !strings.HasPrefix(call.Output, "…\n[") ||
		!strings.Contains(call.Output, "could not be kept anywhere else") {
		t.Errorf("the least form = args %.60q, output %q; want both saying they were not kept",
			call.Args, call.Output)
	}
	if call.WholeBytes != 0 || call.WholeParts != 0 {
		t.Errorf("a record whose parts were refused refers to %d bytes in %d parts", call.WholeBytes, call.WholeParts)
	}

	// A text the call did not have gets no mark: arguments that were never
	// passed read as none, not as arguments that were not kept.
	if ok, err := store.AppendBridgeCall(t.Context(), "t-lower", sandbox.BridgeCall{
		Name: "read_page", Output: strings.Repeat("y", 4<<10),
	}); err != nil || !ok {
		t.Fatalf("append: %v, %v", ok, err)
	}
	if bare := mustBridgeCalls(t, store, run)[1]; bare.Args != "" || !strings.HasPrefix(bare.Output, "…\n[") {
		t.Errorf("the least form of a call with no arguments = args %q, output %.40q; want none, and "+
			"the output's mark and note", bare.Args, bare.Output)
	}
}

// partsFail is a coordination store that takes the first `take` parts it is
// asked to file and then cannot be reached.
type partsFail struct {
	*memory.Fleet
	take  int
	mu    sync.Mutex
	taken int
}

func (p *partsFail) CreateBridgeCallPart(ctx context.Context, turnID, launchID string, seq uint64, part int, value []byte) (bool, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.taken >= p.take {
		return false, coord.ErrUnavailable
	}
	p.taken++
	return p.Fleet.CreateBridgeCallPart(ctx, turnID, launchID, seq, part, value)
}

// PARTS THAT CANNOT BE WRITTEN LEAVE NO REFERENCE TO THEM.
//
// The parts are filed before the record, so a store that fails between two
// parts leaves some parts written and the rest not. A record naming them would
// hand the resume a whole that does not reassemble; one naming none, and
// saying its whole was not kept, is the truth. The call is still recorded:
// the tool ran, and its fitted form is evidence of that.
func TestAPartWriteFailureLeavesNoDanglingReference(t *testing.T) {
	t.Parallel()
	fleet := &partsFail{Fleet: memory.NewFleet(), take: 1}
	store := sandbox.NewCoordStore(fleet)
	run := begun(t, store, "t-parts-fail")
	sent := sandbox.BridgeCall{
		Name: "read_file", Output: strings.Repeat("z", sandbox.MaxBridgeCallBytes*2),
		At: time.Date(2026, 8, 23, 12, 0, 0, 0, time.UTC),
	}
	if ok, err := store.AppendBridgeCall(t.Context(), "t-parts-fail", sent); err != nil || !ok {
		t.Fatalf("a call whose parts could not be written was not recorded: %v, %v", ok, err)
	}
	page, err := store.BridgeCallPage(t.Context(), run, 0, 10)
	if err != nil || len(page.Calls) != 1 {
		t.Fatalf("BridgeCallPage = %+v, %v", page, err)
	}
	record := page.Calls[0]
	if record.WholeBytes != 0 || record.WholeParts != 0 {
		t.Fatalf("the record refers to %d bytes in %d parts, of which one was written",
			record.WholeBytes, record.WholeParts)
	}
	// The note names the whole's length encoded, which for a call of plain
	// letters is what the standard encoder writes.
	whole, err := json.Marshal(sent)
	if err != nil {
		t.Fatal(err)
	}
	note := sandbox.WholeNotKept(len(whole))
	if !strings.HasSuffix(record.Output, "…"+note) {
		t.Errorf("the record's output ends %q, want the cut's mark and the note saying the whole "+
			"was not kept", record.Output[max(0, len(record.Output)-160):])
	}
	// The resume reads that fitted form as it is, note and all: nothing
	// refers to parts, so nothing is reassembled from the one left behind.
	if got := mustBridgeCalls(t, store, run); len(got) != 1 || got[0].Output != record.Output {
		t.Errorf("the log read whole = %d calls; want the one call, as its record holds it", len(got))
	}
}

// changesParts is a coordination store whose whole-log read hands back what
// is filed under the calls changed: what a part purged under a reader, lost,
// or filed by somebody else looks like to the store reading it.
type changesParts struct {
	*memory.Fleet
	change func([]coord.BridgeCallRecord)
}

func (c changesParts) BridgeCalls(ctx context.Context, turnID, launchID string) ([]coord.BridgeCallRecord, error) {
	records, err := c.Fleet.BridgeCalls(ctx, turnID, launchID)
	if err == nil {
		c.change(records)
	}
	return records, err
}

// A WHOLE THAT DOES NOT REASSEMBLE IS NEVER HANDED OVER AS THE CALL.
//
// A missing part, a part that is short, a part that is not what was filed,
// parts that make a whole call but ANOTHER one: each would make a "whole" that
// is shorter or other than what the run did. The resume gets the record's
// fitted form instead — honest about being cut — with a note in its output
// saying the whole could not be read back and why.
func TestAWholeThatDoesNotReassembleIsReadAsItsRecordWithANote(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		change func([]coord.BridgeCallRecord)
		why    string
	}{
		"a part is missing": {
			change: func(r []coord.BridgeCallRecord) { r[0].Parts = slices.Delete(r[0].Parts, 1, 2) },
			why:    "part 2 of 3 is missing",
		},
		"a part is short": {
			change: func(r []coord.BridgeCallRecord) {
				r[0].Parts[2].Value = r[0].Parts[2].Value[:len(r[0].Parts[2].Value)-1]
			},
			why: "its parts hold",
		},
		"a part is not what was filed": {
			change: func(r []coord.BridgeCallRecord) {
				r[0].Parts[0].Value = bytes.Repeat([]byte("#"), len(r[0].Parts[0].Value))
			},
			why: "do not decode",
		},
		"the parts make another call": {
			change: func(r []coord.BridgeCallRecord) { r[0].Parts, r[1].Parts = r[1].Parts, r[0].Parts },
			why:    "a different call",
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			fleet := memory.NewFleet()
			store := sandbox.NewCoordStore(fleet)
			run := begun(t, store, "t-unreadable")
			// Two calls whose wholes are the same length, so that the
			// parts of one fit the other's reference to the byte.
			at := time.Date(2026, 8, 23, 12, 0, 0, 0, time.UTC)
			output := strings.Repeat("z", sandbox.MaxBridgeCallBytes*2+10)
			sent := sandbox.BridgeCall{Name: "read_file", Output: output, At: at}
			for _, call := range []sandbox.BridgeCall{sent, {Name: "read_fill", Output: output, At: at}} {
				if ok, err := store.AppendBridgeCall(t.Context(), "t-unreadable", call); err != nil || !ok {
					t.Fatalf("append: %v, %v", ok, err)
				}
			}
			record := mustBridgeCallPage(t, store, run)[0]
			if record.WholeParts != 3 {
				t.Fatalf("the whole is in %d parts; the case needs 3", record.WholeParts)
			}

			reader := sandbox.NewCoordStore(changesParts{Fleet: fleet, change: tc.change})
			got := mustBridgeCalls(t, reader, run)[0]
			if got.Name != sent.Name || got.Output == sent.Output {
				t.Fatalf("a whole that does not reassemble was handed over as the call (%s)", got.Name)
			}
			if !strings.HasPrefix(got.Output, record.Output) || !strings.Contains(got.Output, "could not be read back") ||
				!strings.Contains(got.Output, tc.why) {
				t.Errorf("the call read back ends %q; want the record's fitted output and a note saying %q",
					got.Output[max(0, len(got.Output)-200):], tc.why)
			}
		})
	}
}

func mustBridgeCallPage(t *testing.T, store *sandbox.CoordStore, run sandbox.PendingRun) []sandbox.BridgeCall {
	t.Helper()
	page, err := store.BridgeCallPage(t.Context(), run, 0, 100)
	if err != nil {
		t.Fatalf("BridgeCallPage: %v", err)
	}
	return append(page.Calls, page.End...)
}

// strayAt is a coordination store in which the first number a call reserves
// already holds a part: what a write that landed after its launch was purged
// leaves, under the numbering the purge reset.
type strayAt struct {
	*memory.Fleet
	once sync.Once
	hit  atomic.Bool
}

func (s *strayAt) CreateBridgeCallPart(ctx context.Context, turnID, launchID string, seq uint64, part int, value []byte) (bool, error) {
	stray := false
	s.once.Do(func() { stray = true })
	if stray {
		s.hit.Store(true)
		return false, nil
	}
	return s.Fleet.CreateBridgeCallPart(ctx, turnID, launchID, seq, part, value)
}

// AN ADDRESS ALREADY TAKEN IS NEVER SHARED. A stray at the number a call
// reserved is somebody else's; parts mixed with it would reassemble into
// neither call. The call takes another number and is read back whole.
func TestACallMeetingAStrayTakesAnotherNumber(t *testing.T) {
	t.Parallel()
	fleet := &strayAt{Fleet: memory.NewFleet()}
	store := sandbox.NewCoordStore(fleet)
	run := begun(t, store, "t-stray")
	sent := sandbox.BridgeCall{Name: "read_file", Output: strings.Repeat("z", sandbox.MaxBridgeCallBytes+10)}
	if ok, err := store.AppendBridgeCall(t.Context(), "t-stray", sent); err != nil || !ok {
		t.Fatalf("append: %v, %v", ok, err)
	}
	if !fleet.hit.Load() {
		t.Fatal("the stray was never met; the case tests nothing")
	}
	got := mustBridgeCalls(t, store, run)
	if len(got) != 1 || got[0].Output != sent.Output || got[0].Seq != 2 {
		t.Errorf("the log = %d calls, the first numbered %d; want the call whole at the next number",
			len(got), got[0].Seq)
	}
}

func mustBridgeCalls(t *testing.T, store *sandbox.CoordStore, run sandbox.PendingRun) []sandbox.BridgeCall {
	t.Helper()
	log, err := store.BridgeCalls(t.Context(), run)
	if err != nil {
		t.Fatalf("BridgeCalls: %v", err)
	}
	return log.Calls
}
