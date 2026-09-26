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
// the resume still reads every byte of it. The least form's marks say why its
// texts are not there: a server refused the record, not that they were too
// large for one.
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
	if record.Args != sandbox.RefusedArgsInParts(len(sent.Args)) || record.Output != "…" {
		t.Errorf("the record = args %.60q, output %.40q; want its least form, both texts marked as "+
			"set aside for the refusal and kept in its parts", record.Args, record.Output)
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
	sent := sandbox.BridgeCall{Name: "slack_post", Args: args, Output: strings.Repeat("y", 4<<10)}
	if ok, err := store.AppendBridgeCall(t.Context(), "t-lower", sent); err != nil || !ok {
		t.Fatalf("a call the server refused as too large was not recorded: %v, %v", ok, err)
	}
	call := mustBridgeCalls(t, store, run)[0]
	if call.Name != "slack_post" || call.Failed {
		t.Errorf("the call kept as %q (failed %v), want the post and its outcome", call.Name, call.Failed)
	}
	if call.Args != sandbox.RefusedArgsNotKept(len(args)) || !strings.HasPrefix(call.Output, "…\n[") ||
		!strings.Contains(call.Output, "set aside when a server refused its record, and could not be kept") {
		t.Errorf("the least form = args %.60q, output %q; want both saying they were set aside for the "+
			"refusal and not kept", call.Args, call.Output)
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

	// A TEXT NO LONGER THAN ITS MARK IS KEPT: an output of "ok" is shorter
	// than the mark and note that would stand for it, and set aside with a
	// whole that was not kept it would be lost for nothing.
	short := sandbox.BridgeCall{Name: "slack_post", Args: args, Output: "ok"}
	if ok, err := store.AppendBridgeCall(t.Context(), "t-lower", short); err != nil || !ok {
		t.Fatalf("append: %v, %v", ok, err)
	}
	if got := mustBridgeCalls(t, store, run)[2]; got.Output != "ok" || got.Args != sandbox.RefusedArgsNotKept(len(args)) {
		t.Errorf("the least form of a call that returned %q = args %.60q, output %q; want the output "+
			"as it was beside its arguments' marker", short.Output, got.Args, got.Output)
	}
}

// A LARGE CALL AGAINST A SERVER BELOW THE CEILING IS KEPT WHOLE, BESIDE ITS
// LEAST FORM.
//
// A call past the ceiling files its whole in parts before its record, and a
// server set below the contract refuses the large parts and then the fitted
// record too. The parts are split until that server takes them — down to the
// floor, which halving alone from the ceiling steps past — and the refused
// record gives way to the least form beside the reference, so the resume still
// reads every byte of the call. Its arguments, eighteen bytes, are shorter
// than any marker that could stand for them, so the least form keeps them.
func TestALargeCallAgainstALowServerIsKeptWholeBesideItsLeastForm(t *testing.T) {
	t.Parallel()
	for name, limit := range map[string]int{
		// Halving from the ceiling reaches 130,048 bytes and then 65,024,
		// below the floor: only a split that stops AT the floor offers this
		// server a part it takes.
		"a server between the floor and twice it": 100 << 10,
		// The parts land at the first halving below it; it is the fitted
		// record the server then refuses.
		"a server at a quarter of a mebibyte": 256 << 10,
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			fleet := memory.NewFleet()
			logs := &logLines{}
			store := sandbox.NewCoordStore(lowServer{Fleet: fleet, limit: limit}).WithLogger(logs.logger())
			run := begun(t, store, "t-large-low")
			sent := sandbox.BridgeCall{
				Name: "read_file", Args: `{"path":"big.txt"}`,
				Output: strings.Repeat("z", sandbox.MaxBridgeCallBytes),
				At:     time.Date(2026, 8, 23, 12, 0, 0, 0, time.UTC),
			}
			if ok, err := store.AppendBridgeCall(t.Context(), "t-large-low", sent); err != nil || !ok {
				t.Fatalf("a call past the ceiling was not recorded against a low server: %v, %v", ok, err)
			}

			record := mustBridgeCallPage(t, store, run)[0]
			if record.Args != sent.Args || record.Output != "…" {
				t.Errorf("the record = args %.60q, output %.40q; want its least form: the arguments as "+
					"they were, and the output's mark", record.Args, record.Output)
			}
			if record.WholeParts == 0 {
				t.Fatalf("the record names no parts: its whole was not kept, although the server takes "+
					"parts of %d bytes", limit)
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
			if got := mustBridgeCalls(t, store, run); len(got) != 1 || got[0].Output != sent.Output ||
				got[0].Args != sent.Args || got[0].WholeParts != 0 {
				t.Errorf("the call read back is not the call made, byte for byte")
			}

			// THE LINES SAY WHAT LANDED: the server's refusal of the fitted
			// record, no line describing a fitted record, which never did,
			// and one line for the least form that did, naming its parts.
			if n := len(logs.named(t, "sandbox_bridge_call_refused_within_ceiling")); n != 1 {
				t.Errorf("the refusal was logged %d times, want once", n)
			}
			if fitted := logs.named(t, "sandbox_bridge_call_fitted"); len(fitted) != 0 {
				t.Errorf("a fitted record the server refused was reported as the call's record: %v", fitted)
			}
			filed := logs.named(t, "sandbox_bridge_call_least_form_filed")
			if len(filed) != 1 || filed[0]["whole_parts"] != float64(record.WholeParts) ||
				filed[0]["whole_bytes"] != float64(record.WholeBytes) {
				t.Errorf("the least form was reported as %v; want one line naming its %d bytes in %d parts",
					filed, record.WholeBytes, record.WholeParts)
			}
		})
	}
}

// recordUnreachable is a coordination store behind a server set below the
// contract's ceiling that takes the parts it fits and then cannot be reached
// for any record the server would take.
type recordUnreachable struct{ lowServer }

func (r recordUnreachable) CreateBridgeCall(_ context.Context, _, _ string, _ uint64, value []byte) (bool, error) {
	if err := r.refuses(value); err != nil {
		return false, err
	}
	return false, coord.ErrUnavailable
}

// A REFUSAL SAYS WHAT WAS REFUSED, AND NOTHING ABOUT A RECORD THAT DID NOT LAND.
//
// The line a server's refusal is logged with names the setting to change. What
// the call's record holds instead is a claim about a record, and one that
// fails to land holds nothing: a line saying the least form was filed, logged
// before its create, would tell an operator the call was kept when the append
// in fact failed.
func TestARefusalSaysNothingAboutARecordThatDidNotLand(t *testing.T) {
	t.Parallel()
	logs := &logLines{}
	store := sandbox.NewCoordStore(recordUnreachable{lowServer{Fleet: memory.NewFleet(), limit: 100 << 10}}).
		WithLogger(logs.logger())
	begun(t, store, "t-least-lost")
	_, err := store.AppendBridgeCall(t.Context(), "t-least-lost", sandbox.BridgeCall{
		Name: "slack_post", Args: `{"channel":"C1"}`, Output: strings.Repeat("y", 300<<10),
		At: time.Date(2026, 8, 23, 12, 0, 0, 0, time.UTC),
	})
	if !errors.Is(err, coord.ErrUnavailable) {
		t.Fatalf("an append whose least form could not land = %v, want the store's failure", err)
	}
	refusals := logs.named(t, "sandbox_bridge_call_refused_within_ceiling")
	if len(refusals) != 1 {
		t.Fatalf("the refusal was logged %d times, want once", len(refusals))
	}
	if detail, _ := refusals[0]["detail"].(string); strings.Contains(detail, "least form") ||
		strings.Contains(detail, "filed") || !strings.Contains(detail, "max_payload") {
		t.Errorf("the refusal's detail = %q; want the setting to change and nothing about what was filed", detail)
	}
	if filed := logs.named(t, "sandbox_bridge_call_least_form_filed"); len(filed) != 0 {
		t.Errorf("a least form that never landed was reported filed: %v", filed)
	}
}

// strayAtTheRecord is a coordination store behind a server set below the
// contract's ceiling in which the first record the server would take finds its
// address already holding one: a stray at the number the call reserved. It
// counts the records the server refused.
type strayAtTheRecord struct {
	lowServer
	stray   atomic.Bool
	refused atomic.Int32
}

func (s *strayAtTheRecord) CreateBridgeCall(ctx context.Context, turnID, launchID string, seq uint64, value []byte) (bool, error) {
	if err := s.refuses(value); err != nil {
		s.refused.Add(1)
		return false, err
	}
	if s.stray.CompareAndSwap(false, true) {
		return false, nil
	}
	return s.lowServer.CreateBridgeCall(ctx, turnID, launchID, seq, value)
}

// A REFUSAL IS CARRIED TO THE NEXT NUMBER, AND SAID ONCE.
//
// A call whose least form meets a stray takes another number. The server that
// refused its fitted record at the first number refuses it at the next, so the
// next pass files the least form without asking again — a second refused
// publish of a record the size of the ceiling, and a second line saying so,
// would be the same answer bought twice.
func TestARefusalIsCarriedToTheNextNumber(t *testing.T) {
	t.Parallel()
	logs := &logLines{}
	fleet := &strayAtTheRecord{lowServer: lowServer{Fleet: memory.NewFleet(), limit: 256 << 10}}
	store := sandbox.NewCoordStore(fleet).WithLogger(logs.logger())
	run := begun(t, store, "t-least-stray")
	sent := sandbox.BridgeCall{
		Name: "read_file", Args: `{"path":"big.txt"}`, Output: strings.Repeat("z", sandbox.MaxBridgeCallBytes),
		At: time.Date(2026, 8, 23, 12, 0, 0, 0, time.UTC),
	}
	if ok, err := store.AppendBridgeCall(t.Context(), "t-least-stray", sent); err != nil || !ok {
		t.Fatalf("append: %v, %v", ok, err)
	}
	if !fleet.stray.Load() {
		t.Fatal("the least form never met the stray; the case tests nothing")
	}
	if n := fleet.refused.Load(); n != 1 {
		t.Errorf("the server was asked for the fitted record %d times; want once, the refusal carried", n)
	}
	if n := len(logs.named(t, "sandbox_bridge_call_refused_within_ceiling")); n != 1 {
		t.Errorf("the refusal was logged %d times, want once", n)
	}
	filed := logs.named(t, "sandbox_bridge_call_least_form_filed")
	if len(filed) != 1 || filed[0]["seq"] != float64(2) {
		t.Errorf("the least form was reported as %v; want one line, for the record at the number after "+
			"the stray's", filed)
	}
	if got := mustBridgeCalls(t, store, run); len(got) != 1 || got[0].Output != sent.Output || got[0].Seq != 2 {
		t.Errorf("the log = %d calls; want the call whole, at the number after the stray's", len(got))
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
	logs := &logLines{}
	store := sandbox.NewCoordStore(fleet).WithLogger(logs.logger())
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
	// And the operator is told, once, with how far the parts got.
	lost := logs.named(t, "sandbox_bridge_call_whole_not_kept")
	if len(lost) != 1 || lost[0]["parts_written"] != float64(1) || lost[0]["seq"] != float64(record.Seq) {
		t.Errorf("the loss was logged as %v; want one line, for the call's record, naming the one part "+
			"written", lost)
	}
}

// partsThenRecordFail is a coordination store that cannot be reached for a
// part or a record.
type partsThenRecordFail struct{ *memory.Fleet }

func (partsThenRecordFail) CreateBridgeCallPart(context.Context, string, string, uint64, int, []byte) (bool, error) {
	return false, coord.ErrUnavailable
}

func (partsThenRecordFail) CreateBridgeCall(context.Context, string, string, uint64, []byte) (bool, error) {
	return false, coord.ErrUnavailable
}

// strayAfterABlip is a coordination store whose first part write cannot reach
// the store and whose first record write finds its address taken: a call that
// met a blip on a number a stray holds.
type strayAfterABlip struct {
	*memory.Fleet
	part, record atomic.Bool
}

func (s *strayAfterABlip) CreateBridgeCallPart(ctx context.Context, turnID, launchID string, seq uint64, part int, value []byte) (bool, error) {
	if s.part.CompareAndSwap(false, true) {
		return false, coord.ErrUnavailable
	}
	return s.Fleet.CreateBridgeCallPart(ctx, turnID, launchID, seq, part, value)
}

func (s *strayAfterABlip) CreateBridgeCall(ctx context.Context, turnID, launchID string, seq uint64, value []byte) (bool, error) {
	if s.record.CompareAndSwap(false, true) {
		return false, nil
	}
	return s.Fleet.CreateBridgeCall(ctx, turnID, launchID, seq, value)
}

// WHAT A CALL'S RECORD HOLDS IS REPORTED ONCE IT HAS LANDED, AND ONLY THEN.
//
// The parts are filed before the record, so their failure is known first —
// but a line saying the record holds its cut form with its whole not kept is a
// claim about a record that does not exist yet. The record may fail to land
// at all, and then the append fails and says so; or its number may turn out to
// be a stray's, and the next number keep the whole after all. Either way that
// line, logged early, would tell an operator a loss that did not happen.
func TestAWholeNotKeptIsReportedOnlyOnceItsRecordHasLanded(t *testing.T) {
	t.Parallel()
	sent := sandbox.BridgeCall{
		Name: "read_file", Output: strings.Repeat("z", sandbox.MaxBridgeCallBytes+10),
		At: time.Date(2026, 8, 23, 12, 0, 0, 0, time.UTC),
	}

	t.Run("the record cannot land either", func(t *testing.T) {
		t.Parallel()
		logs := &logLines{}
		store := sandbox.NewCoordStore(partsThenRecordFail{memory.NewFleet()}).WithLogger(logs.logger())
		begun(t, store, "t-nothing-landed")
		_, err := store.AppendBridgeCall(t.Context(), "t-nothing-landed", sent)
		if !errors.Is(err, coord.ErrUnavailable) {
			t.Fatalf("an append whose record could not land = %v, want the store's failure", err)
		}
		if !strings.Contains(err.Error(), "had not been kept in parts") {
			t.Errorf("the append's error drops the parts' failure before it: %v", err)
		}
		for _, msg := range []string{"sandbox_bridge_call_whole_not_kept", "sandbox_bridge_call_fitted"} {
			if got := logs.named(t, msg); len(got) != 0 {
				t.Errorf("%s was logged about a record that never landed: %v", msg, got)
			}
		}
	})

	t.Run("a stray's number, then the whole kept at the next", func(t *testing.T) {
		t.Parallel()
		logs := &logLines{}
		fleet := &strayAfterABlip{Fleet: memory.NewFleet()}
		store := sandbox.NewCoordStore(fleet).WithLogger(logs.logger())
		run := begun(t, store, "t-kept-after-all")
		if ok, err := store.AppendBridgeCall(t.Context(), "t-kept-after-all", sent); err != nil || !ok {
			t.Fatalf("append: %v, %v", ok, err)
		}
		if !fleet.part.Load() || !fleet.record.Load() {
			t.Fatal("the first pass met neither the blip nor the stray; the case tests nothing")
		}
		got := mustBridgeCalls(t, store, run)
		if len(got) != 1 || got[0].Output != sent.Output || got[0].Seq != 2 {
			t.Fatalf("the log = %d calls; want the call whole, at the number after the stray's", len(got))
		}
		if lost := logs.named(t, "sandbox_bridge_call_whole_not_kept"); len(lost) != 0 {
			t.Errorf("a whole that was kept was reported lost: %v", lost)
		}
		fitted := logs.named(t, "sandbox_bridge_call_fitted")
		if len(fitted) != 1 || fitted[0]["seq"] != float64(2) || fitted[0]["whole_parts"] == float64(0) {
			t.Errorf("the fitted record was reported as %v; want one line, for the record that landed, "+
				"naming its parts", fitted)
		}
	})
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
			// NOTHING POINTS AT THE PARTS THAT COULD NOT BE READ: the note
			// carries their length and count, and the reference is gone.
			if got.WholeBytes != 0 || got.WholeParts != 0 {
				t.Errorf("the call read back still refers to %d bytes in %d parts, the parts that could "+
					"not be read", got.WholeBytes, got.WholeParts)
			}
			if note := fmt.Sprintf("%d bytes filed in %d parts", record.WholeBytes, record.WholeParts); !strings.Contains(got.Output, note) {
				t.Errorf("the note does not say %q, the length and count the reference carried", note)
			}
		})
	}
}

// A CALL WHOSE PARTS DO NOT REASSEMBLE NEVER POINTS AT THEM FOR ITS ARGUMENTS.
//
// A record that set its arguments aside says they went into its parts. When
// those parts do not reassemble, the call read back whole is the record's
// fitted form — and that is what the resume carries into the phase's durable
// record and the reviewer's evidence, while the run's end purges the parts. So
// its arguments' marker says they could not be read back, with the same count,
// and its output ends in the note saying why: the read hands over no mark
// sending a reader to a place it could not reach itself.
func TestACallWhosePartsDoNotReassembleSaysItsArgumentsCouldNotBeReadBack(t *testing.T) {
	t.Parallel()
	args := `{"text":"` + strings.Repeat("x", sandbox.MaxBridgeCallBytes) + `"}`
	for name, tc := range map[string]struct {
		// limit is what the server takes, zero for the contract's ceiling.
		limit  int
		output string
		// inParts and unreadable are the record's marker and the one a
		// read that cannot reassemble the parts puts in its place.
		inParts, unreadable func(int) string
	}{
		"arguments the fit set aside": {output: "posted",
			inParts: sandbox.ArgsInParts, unreadable: sandbox.ArgsUnreadable},
		"a call that returned nothing": {output: "",
			inParts: sandbox.ArgsInParts, unreadable: sandbox.ArgsUnreadable},
		// Output enough that the fitted record is still too large for the
		// server, so what it keeps is the least form, whose markers say
		// the arguments were set aside for the server's refusal.
		"the least form a server below the ceiling left": {limit: 256 << 10, output: strings.Repeat("y", 4<<20),
			inParts: sandbox.RefusedArgsInParts, unreadable: sandbox.RefusedArgsUnreadable},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			fleet := memory.NewFleet()
			var records sandbox.RunRecords = fleet
			if tc.limit > 0 {
				records = lowServer{Fleet: fleet, limit: tc.limit}
			}
			store := sandbox.NewCoordStore(records)
			run := begun(t, store, "t-unreadable-args")
			sent := sandbox.BridgeCall{
				Name: "slack_post", Args: args, Output: tc.output,
				At: time.Date(2026, 8, 23, 12, 0, 0, 0, time.UTC),
			}
			if ok, err := store.AppendBridgeCall(t.Context(), "t-unreadable-args", sent); err != nil || !ok {
				t.Fatalf("append: %v, %v", ok, err)
			}
			record := mustBridgeCallPage(t, store, run)[0]
			if record.Args != tc.inParts(len(args)) || record.WholeParts < 2 {
				t.Fatalf("the record = args %.60q in %d parts; the case needs arguments set aside and a "+
					"whole of several parts", record.Args, record.WholeParts)
			}

			reader := sandbox.NewCoordStore(changesParts{Fleet: fleet, change: func(r []coord.BridgeCallRecord) {
				r[0].Parts = r[0].Parts[1:]
			}})
			got := mustBridgeCalls(t, reader, run)[0]
			if got.Args != tc.unreadable(len(args)) {
				t.Errorf("the arguments read back = %.120q; want the marker saying the %d bytes could "+
					"not be read back", got.Args, len(args))
			}
			var marker map[string]any
			if err := json.Unmarshal([]byte(got.Args), &marker); err != nil || len(marker) != 1 {
				t.Errorf("the marker is not the one-member JSON object a reader decodes: %q (%v)", got.Args, err)
			}
			if !strings.HasPrefix(got.Output, record.Output) || !strings.Contains(got.Output, "could not be read back") ||
				!strings.Contains(got.Output, "part 1 of") {
				t.Errorf("the output read back ends %q; want the record's own and the note saying why",
					got.Output[max(0, len(got.Output)-200):])
			}
			if tc.output == "" && strings.HasPrefix(got.Output, "\n") {
				t.Errorf("a call that returned nothing reads back as a note after an empty line: %q", got.Output)
			}
		})
	}

	// ARGUMENTS A CALL WAS MADE WITH ARE NEVER TAKEN FOR THE MARKER. Only the
	// marker byte for byte gives way; arguments shaped like it but naming
	// another count, or spelled differently, are the call's own.
	for _, own := range []string{
		strings.Replace(sandbox.ArgsInParts(12), "12", "012", 1),
		strings.Replace(sandbox.ArgsInParts(12), "filed", "kept", 1),
		strings.Replace(sandbox.RefusedArgsInParts(12), "refused", "declined", 1),
		`{"…":"12"}`,
	} {
		fleet := memory.NewFleet()
		store := sandbox.NewCoordStore(fleet)
		run := begun(t, store, "t-own-args")
		sent := sandbox.BridgeCall{
			Name: "read_file", Args: own, Output: strings.Repeat("z", sandbox.MaxBridgeCallBytes),
			At: time.Date(2026, 8, 23, 12, 0, 0, 0, time.UTC),
		}
		if ok, err := store.AppendBridgeCall(t.Context(), "t-own-args", sent); err != nil || !ok {
			t.Fatalf("append: %v, %v", ok, err)
		}
		reader := sandbox.NewCoordStore(changesParts{Fleet: fleet, change: func(r []coord.BridgeCallRecord) {
			r[0].Parts = r[0].Parts[1:]
		}})
		if got := mustBridgeCalls(t, reader, run)[0]; got.Args != own {
			t.Errorf("arguments the call was made with, %q, were read back as %q", own, got.Args)
		}
	}
}

// logLines is a logger a test reads back: every line the store logs, as the
// JSON its handler wrote. Handed to one store with WithLogger, so no test
// points the process-wide sink at a buffer the parallel suite would share.
type logLines struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (l *logLines) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.Write(p)
}

func (l *logLines) logger() *slog.Logger {
	return slog.New(slog.NewJSONHandler(l, &slog.HandlerOptions{Level: slog.LevelDebug}))
}

// named returns every line logged as msg, in order.
func (l *logLines) named(t *testing.T, msg string) []map[string]any {
	t.Helper()
	l.mu.Lock()
	defer l.mu.Unlock()
	var out []map[string]any
	for line := range strings.Lines(l.buf.String()) {
		var rec map[string]any
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			t.Fatalf("a log line is not JSON: %q (%v)", line, err)
		}
		if rec["msg"] == msg {
			out = append(out, rec)
		}
	}
	return out
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

// --- a suspension its row cannot hold -------------------------------------

// largeState is a suspended conversation past what a run's row keeps one
// within, which is half the transport's ceiling.
func largeState() map[string]any {
	return map[string]any{
		"version":              float64(2),
		"messages":             []any{map[string]any{"Role": "assistant", "Content": strings.Repeat("w", 5<<20)}},
		"pending_tool_call_id": "call_1",
		"pending_tool_name":    "run_sandbox",
	}
}

// suspendedInParts launches a run and suspends it with a conversation its row
// cannot hold, returning the row.
func suspendedInParts(t *testing.T, store *sandbox.CoordStore, turnID string, state map[string]any) sandbox.PendingRun {
	t.Helper()
	begun(t, store, turnID)
	if ok, err := store.MarkSuspended(t.Context(), turnID, state); err != nil || !ok {
		t.Fatalf("MarkSuspended(%s) = %v, %v", turnID, ok, err)
	}
	run, found, err := store.Get(t.Context(), turnID)
	if err != nil || !found {
		t.Fatalf("Get(%s) = %v, %v", turnID, found, err)
	}
	return run
}

func sameJSON(t *testing.T, got, want map[string]any) bool {
	t.Helper()
	g, err := json.Marshal(got)
	if err != nil {
		t.Fatal(err)
	}
	w, err := json.Marshal(want)
	if err != nil {
		t.Fatal(err)
	}
	return bytes.Equal(g, w)
}

// A SUSPENSION KEPT IN PARTS ENDS WITH ITS RUN, by every path that ends one:
// the finish, a relaunch that replaces its launch, and the sweep of a launch no
// run names. Kept past them it would sit in a bucket with no age for the life
// of the deployment, megabytes at a time.
func TestASuspensionKeptInPartsEndsWithItsRun(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	fleet := memory.NewFleet()
	store := sandbox.NewCoordStore(fleet)

	finished := suspendedInParts(t, store, "t-finish", largeState())
	if parts, err := fleet.SuspensionParts(ctx, finished.TurnID, finished.LaunchID); err != nil || len(parts) == 0 {
		t.Fatalf("the suspension is in %d parts, %v; want it in parts, which is the premise", len(parts), err)
	}
	if gone, err := store.Finish(ctx, "t-finish", sandbox.Fence{}); err != nil || !gone {
		t.Fatalf("Finish = %v, %v", gone, err)
	}
	if parts, err := fleet.SuspensionParts(ctx, finished.TurnID, finished.LaunchID); err != nil || len(parts) != 0 {
		t.Errorf("the finished run's suspension survived it in %d parts, %v", len(parts), err)
	}

	replaced := suspendedInParts(t, store, "t-relaunch", largeState())
	relaunched := begun(t, store, "t-relaunch")
	if parts, err := fleet.SuspensionParts(ctx, replaced.TurnID, replaced.LaunchID); err != nil || len(parts) != 0 {
		t.Errorf("the replaced launch's suspension survived the relaunch in %d parts, %v", len(parts), err)
	}

	// A launch no run names, holding nothing but a suspension's parts: what a
	// node that died between filing them and the finish leaves behind.
	if _, err := fleet.CreateSuspensionPart(ctx, "t-gone", "launch-gone", 1, []byte("piece")); err != nil {
		t.Fatal(err)
	}
	if purged, err := store.SweepBridgeCalls(ctx); err != nil || purged != 1 {
		t.Fatalf("SweepBridgeCalls = %d, %v; want the one launch no run names", purged, err)
	}
	if parts, err := fleet.SuspensionParts(ctx, "t-gone", "launch-gone"); err != nil || len(parts) != 0 {
		t.Errorf("the sweep left an orphaned suspension's %d parts, %v", len(parts), err)
	}
	if got := launchesOf(t, fleet); len(got) != 0 {
		t.Errorf("launches after the sweep = %+v, want none: the relaunch %s has filed nothing yet",
			got, relaunched.LaunchID)
	}
}

// lowRunServer is [lowServer] for what a run keeps outside its calls: a server
// configured below the contract's ceiling refuses a run's own record, and a
// suspension's part, past its limit as too large.
type lowRunServer struct {
	*memory.Fleet
	limit int
}

func (l lowRunServer) refuses(value []byte) error {
	if len(value) > l.limit {
		return fmt.Errorf("the server accepts %d bytes: %w", l.limit, coord.ErrTooLarge)
	}
	return nil
}

func (l lowRunServer) CreateSandboxRun(ctx context.Context, turnID string, value []byte) (bool, error) {
	if err := l.refuses(value); err != nil {
		return false, err
	}
	return l.Fleet.CreateSandboxRun(ctx, turnID, value)
}

func (l lowRunServer) UpdateSandboxRun(ctx context.Context, turnID string, value []byte, version uint64) (bool, error) {
	if err := l.refuses(value); err != nil {
		return false, err
	}
	return l.Fleet.UpdateSandboxRun(ctx, turnID, value, version)
}

func (l lowRunServer) CreateSuspensionPart(ctx context.Context, turnID, launchID string, part int, value []byte) (bool, error) {
	if err := l.refuses(value); err != nil {
		return false, err
	}
	return l.Fleet.CreateSuspensionPart(ctx, turnID, launchID, part, value)
}

// A SERVER BELOW THE CEILING DOES NOT LOSE THE CONVERSATION. It refuses a row
// the contract's ceiling admits — a node does not boot against one, but a
// reconnect can reach one — and the refusal is permanent, so the conversation
// goes to parts split until that server takes them, the same split and the
// same floor a bridged call's whole gets, and the resume reads it back whole.
func TestASuspensionAServerBelowTheCeilingRefusesIsKeptInPartsItTakes(t *testing.T) {
	t.Parallel()
	const limit = 100 << 10
	fleet := memory.NewFleet()
	store := sandbox.NewCoordStore(lowRunServer{Fleet: fleet, limit: limit})
	state := map[string]any{
		"version": float64(2), "pending_tool_call_id": "call_1",
		"messages": []any{map[string]any{"Role": "assistant", "Content": strings.Repeat("w", 300<<10)}},
	}
	run := suspendedInParts(t, store, "t-low", state)
	got, err := store.Suspension(t.Context(), run)
	if err != nil || !sameJSON(t, got, state) {
		t.Fatalf("the conversation read back = %d keys, %v; want the whole one suspended", len(got), err)
	}
	parts, err := fleet.SuspensionParts(t.Context(), run.TurnID, run.LaunchID)
	if err != nil || len(parts) < 4 {
		t.Fatalf("the conversation is in %d parts, %v: a part the server refused was not split", len(parts), err)
	}
	for _, part := range parts {
		if len(part.Value) > limit {
			t.Errorf("part %d is %d bytes, past the %d the server takes", part.Part, len(part.Value), limit)
		}
	}
}

// A CONVERSATION NO PART CAN HOLD IS AN ERROR THAT NAMES THE LIMIT, and the
// run stays launching, where its caller fails it and reclaims its box. A
// conversation dropped instead would open the run to a resume with nothing to
// re-enter, and one cut would re-enter the turn without the call it suspended
// on.
func TestASuspensionNoPartCanHoldIsRefusedNamingTheLimit(t *testing.T) {
	t.Parallel()
	store := sandbox.NewCoordStore(lowRunServer{Fleet: memory.NewFleet(), limit: 1 << 10})
	begun(t, store, "t-lower")
	state := map[string]any{"version": float64(2), "messages": []any{strings.Repeat("w", 200<<10)}}
	ok, err := store.MarkSuspended(t.Context(), "t-lower", state)
	if err == nil || ok {
		t.Fatalf("MarkSuspended = %v, %v; want the conversation refused", ok, err)
	}
	if !errors.Is(err, coord.ErrTooLarge) || !strings.Contains(err.Error(), "not split below 65536 bytes") ||
		!strings.Contains(err.Error(), "the server accepts 1024 bytes") {
		t.Errorf("the refusal = %v; want it to name the floor and the server's limit", err)
	}
	run, _, err := store.Get(t.Context(), "t-lower")
	if err != nil || run.Status != sandbox.StatusLaunching || len(run.ExecuteState) != 0 {
		t.Errorf("the run is %q with %d keys of conversation, %v; want it left launching with none",
			run.Status, len(run.ExecuteState), err)
	}
}

// A REFUSED PART IS SPLIT DOWN TO THE FLOOR AND NO FURTHER.
//
// A refused part larger than 64 KiB is retried at half its size, or at 64 KiB
// when half would be smaller, and only a refused part of 64 KiB or less ends
// the split — the rule a bridged call's whole is split by too. Halving 300 KiB
// passes over the floor, so against a server that takes exactly the floor
// every part but the last is the floor's size; and a server below the floor is
// offered no smaller part, even where two halves would fit it, because its
// max_payload is the setting to change and the refusal names the part it
// refused.
func TestARefusedPartIsSplitToTheFloorAndNoFurther(t *testing.T) {
	t.Parallel()
	const floor = 64 << 10

	fleet := memory.NewFleet()
	store := sandbox.NewCoordStore(lowRunServer{Fleet: fleet, limit: floor})
	state := map[string]any{"version": float64(2), "messages": []any{strings.Repeat("w", 300<<10)}}
	run := suspendedInParts(t, store, "t-floor", state)
	parts, err := fleet.SuspensionParts(t.Context(), run.TurnID, run.LaunchID)
	if err != nil || len(parts) < 2 {
		t.Fatalf("the conversation is in %d parts, %v; want it split", len(parts), err)
	}
	for _, part := range parts[:len(parts)-1] {
		if len(part.Value) != floor {
			t.Errorf("part %d is %d bytes; want every part but the last at the %d-byte floor",
				part.Part, len(part.Value), floor)
		}
	}
	if got, err := store.Suspension(t.Context(), run); err != nil || !sameJSON(t, got, state) {
		t.Errorf("the conversation read back = %d keys, %v; want the whole one suspended", len(got), err)
	}

	below := sandbox.NewCoordStore(lowRunServer{Fleet: memory.NewFleet(), limit: 60 << 10})
	begun(t, below, "t-below")
	ok, err := below.MarkSuspended(t.Context(), "t-below",
		map[string]any{"version": float64(2), "messages": []any{strings.Repeat("w", 100<<10)}})
	if err == nil || ok {
		t.Fatalf("MarkSuspended = %v, %v; want the split to stop at the floor", ok, err)
	}
	if !errors.Is(err, coord.ErrTooLarge) || !strings.Contains(err.Error(), "a part of 65536 bytes was refused") {
		t.Errorf("the refusal = %v; want it to name the refused part at the floor", err)
	}
}

// partLost answers a launch's suspension one part short: a part purged under
// the read, or lost.
type partLost struct{ *memory.Fleet }

func (p partLost) SuspensionParts(ctx context.Context, turnID, launchID string) ([]coord.Part, error) {
	parts, err := p.Fleet.SuspensionParts(ctx, turnID, launchID)
	if len(parts) > 0 {
		parts = parts[:len(parts)-1]
	}
	return parts, err
}

// PARTS THAT DO NOT MAKE THE WHOLE ARE NEVER PASSED OFF AS IT. A conversation
// one part short is not a shorter conversation: re-entered, the turn would be
// missing the call it suspended on. The read says it cannot, and why.
func TestASuspensionWhosePartsDoNotReassembleIsUnreadable(t *testing.T) {
	t.Parallel()
	fleet := memory.NewFleet()
	run := suspendedInParts(t, sandbox.NewCoordStore(fleet), "t-short", largeState())
	_, err := sandbox.NewCoordStore(partLost{fleet}).Suspension(t.Context(), run)
	if !errors.Is(err, sandbox.ErrSuspensionUnreadable) || !strings.Contains(err.Error(), "is missing") {
		t.Errorf("a suspension one part short = %v, want ErrSuspensionUnreadable naming the part", err)
	}
}

// A REFERENCE NAMING NO LAUNCH IS UNREADABLE, NOT A FAILED READ. No write files
// one, and the store would refuse the address its parts would need; read as a
// store failure, the resume would hand the run back for a retry that meets the
// same refusal every time.
func TestASuspensionReferenceWithNoLaunchIsUnreadable(t *testing.T) {
	t.Parallel()
	run := sandbox.PendingRun{TurnID: "t-no-launch", ExecuteState: map[string]any{
		"suspension_in_parts": map[string]any{"bytes": 10, "parts": 1},
	}}
	_, err := sandbox.NewCoordStore(memory.NewFleet()).Suspension(t.Context(), run)
	if !errors.Is(err, sandbox.ErrSuspensionUnreadable) {
		t.Errorf("a reference naming no launch = %v, want ErrSuspensionUnreadable", err)
	}
}

// predatingRun is a run's row as a build that predates suspension parts
// decodes and encodes it: the wire fields its PendingRun carries, frozen here
// so this build's own type cannot stand in for it. A field this build adds is
// one that build drops on its first write, and that loss is what a round trip
// through this build's type can never show.
type predatingRun struct {
	TurnID            string          `json:"turn_id"`
	WorkKey           string          `json:"work_key,omitempty"`
	AgentHandle       string          `json:"agent_handle"`
	AgentID           string          `json:"agent_id"`
	Role              string          `json:"role"`
	SandboxID         string          `json:"sandbox_id"`
	CodingAgent       string          `json:"coding_agent"`
	Placement         string          `json:"placement,omitempty"`
	CommandID         string          `json:"command_id"`
	Status            string          `json:"status"`
	LaunchID          string          `json:"launch_id,omitempty"`
	Owner             string          `json:"owner"`
	OwnerEpoch        int64           `json:"owner_epoch"`
	TaskDescription   string          `json:"task_description"`
	Reply             string          `json:"reply,omitempty"`
	PartitionKey      string          `json:"conversation_key"`
	ConversationKey   string          `json:"conversation_identity,omitempty"`
	Branch            string          `json:"branch"`
	SessionID         string          `json:"session_id"`
	Question          string          `json:"question"`
	Audience          string          `json:"audience"`
	TraceID           string          `json:"trace_id"`
	SpanID            string          `json:"span_id"`
	DelegationDepth   int             `json:"delegation_depth"`
	DelegationChain   []string        `json:"delegation_chain"`
	ExecuteState      map[string]any  `json:"execute_state"`
	BridgeCalls       []predatingCall `json:"bridge_calls,omitempty"`
	BridgeCallsElided int             `json:"bridge_calls_elided,omitempty"`
	Charged           bool            `json:"charged,omitempty"`
	PauseTTLSeconds   float64         `json:"pause_ttl_seconds"`
	PausedAt          time.Time       `json:"paused_at"`
	CreatedAt         time.Time       `json:"created_at"`
	UpdatedAt         time.Time       `json:"updated_at"`
}

// predatingCall is a bridged call as that build's row carries it.
type predatingCall struct {
	Name   string    `json:"name"`
	Args   string    `json:"args,omitempty"`
	Output string    `json:"output,omitempty"`
	Failed bool      `json:"failed,omitempty"`
	At     time.Time `json:"at"`
}

// A WRITE BY A BUILD THAT PREDATES THE PARTS KEEPS THE REFERENCE.
//
// A rolling upgrade puts such a build on the run's row, and it writes the row
// whole on every step of the lifecycle it takes — a claim of ownership during
// recovery, a box attached — decoding it into its own type and encoding what
// it decoded. Whatever that type has no field for is gone after its first
// write. The reference survives because it has no field of its own: it lives
// in execute_state, a map that build carries whole, so what it writes back
// still names the parts and the resume that follows reads the conversation
// whole.
func TestAWriteByABuildThatPredatesThePartsKeepsTheReference(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	fleet := memory.NewFleet()
	store := sandbox.NewCoordStore(fleet)
	state := largeState()
	suspendedInParts(t, store, "t-peer", state)

	record, found, err := fleet.SandboxRun(ctx, "t-peer")
	if err != nil || !found {
		t.Fatalf("SandboxRun = %v, %v", found, err)
	}
	var row predatingRun
	if err := json.Unmarshal(record.Value, &row); err != nil {
		t.Fatal(err)
	}
	row.Owner, row.OwnerEpoch = "peer-incarnation", 7
	raw, err := json.Marshal(row)
	if err != nil {
		t.Fatal(err)
	}
	if ok, err := fleet.UpdateSandboxRun(ctx, "t-peer", raw, record.Version); err != nil || !ok {
		t.Fatalf("the peer's write = %v, %v", ok, err)
	}

	after, _, err := store.Get(ctx, "t-peer")
	if err != nil || after.Owner != "peer-incarnation" {
		t.Fatalf("Get = %+v, %v", after.Owner, err)
	}
	if got, err := store.Suspension(ctx, after); err != nil || !sameJSON(t, got, state) {
		t.Errorf("after the peer's write the conversation reads back as %d keys, %v; want it whole",
			len(got), err)
	}
}

// A REFERENCE THAT DOES NOT DECODE IS UNREADABLE, NOT A CONVERSATION. A row's
// execute_state holding the reference's one key names a suspension kept in
// parts, whatever its value; one this build cannot decode names parts nothing
// can find. Read as a conversation instead, the resume would re-enter a turn
// whose messages are a single key and no call to answer.
func TestASuspensionReferenceThatDoesNotDecodeIsUnreadable(t *testing.T) {
	t.Parallel()
	run := sandbox.PendingRun{TurnID: "t-garbled", LaunchID: "launch-1", ExecuteState: map[string]any{
		"suspension_in_parts": "not a reference",
	}}
	got, err := sandbox.NewCoordStore(memory.NewFleet()).Suspension(t.Context(), run)
	if !errors.Is(err, sandbox.ErrSuspensionUnreadable) {
		t.Errorf("a reference that does not decode = %d keys, %v; want ErrSuspensionUnreadable", len(got), err)
	}
}

// A SUSPENSION ON THE ROW LEAVES THE ROW ITS LIFECYCLE. The row is one record,
// rewritten whole by every step after the suspension — the park on a question
// above all, which adds a question no fit bounds — and older builds' view of
// the run's calls may already fill it by the time the conversation arrives. So
// the suspension refits that view into what the row's half of the ceiling
// leaves, dropping from the middle and counting what it dropped, and a park
// with a question of megabytes still lands. Without the refit the view keeps
// its size beside the conversation, and that park is refused as too large.
func TestASuspensionOnTheRowRefitsTheCallsViewSoAParkStillLands(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	store := sandbox.NewCoordStore(memory.NewFleet())
	begun(t, store, "t-refit")

	const calls = 30
	for i := range calls {
		if ok, err := store.AppendBridgeCall(ctx, "t-refit", sandbox.BridgeCall{
			Name: fmt.Sprintf("read_%d", i), Output: strings.Repeat("r", 100<<10),
		}); err != nil || !ok {
			t.Fatalf("AppendBridgeCall %d = %v, %v", i, ok, err)
		}
	}
	before, _, err := store.Get(ctx, "t-refit")
	if err != nil {
		t.Fatal(err)
	}
	if len(before.BridgeCalls) != calls || before.BridgeCallsElided != 0 {
		t.Fatalf("the view holds %d calls with %d elided before the suspension, want all %d: the premise",
			len(before.BridgeCalls), before.BridgeCallsElided, calls)
	}

	state := map[string]any{
		"version": float64(2), "pending_tool_call_id": "call_1",
		"messages": []any{map[string]any{"Role": "assistant", "Content": strings.Repeat("w", 3<<20)}},
	}
	if ok, err := store.MarkSuspended(ctx, "t-refit", state); err != nil || !ok {
		t.Fatalf("MarkSuspended = %v, %v", ok, err)
	}
	after, _, err := store.Get(ctx, "t-refit")
	if err != nil {
		t.Fatal(err)
	}
	if !sameJSON(t, after.ExecuteState, state) {
		t.Fatalf("the row holds %d keys of conversation, want the suspension itself: it fits the row",
			len(after.ExecuteState))
	}
	if after.BridgeCallsElided == 0 || len(after.BridgeCalls)+after.BridgeCallsElided != calls {
		t.Errorf("the view holds %d calls with %d elided after the suspension, want its middle "+
			"dropped and counted", len(after.BridgeCalls), after.BridgeCallsElided)
	}

	if err := store.MarkAwaiting(ctx, "t-refit", sandbox.Clarification{
		Question: strings.Repeat("q", 3<<20), Audience: "requester",
	}); err != nil {
		t.Fatalf("a park with a question of 3 MiB = %v, want it landed beside the conversation", err)
	}
	if got, _, err := store.Get(ctx, "t-refit"); err != nil || got.Status != sandbox.StatusAwaiting {
		t.Fatalf("the run is %q, %v; want it parked on its question", got.Status, err)
	}
}

// --- what a run's record carries between builds ------------------------------

// rawMembers is a run's record as its members, straight off the store.
func rawMembers(t *testing.T, fleet *memory.Fleet, turnID string) (map[string]json.RawMessage, uint64) {
	t.Helper()
	record, found, err := fleet.SandboxRun(t.Context(), turnID)
	if err != nil || !found {
		t.Fatalf("SandboxRun %s = %v, %v", turnID, found, err)
	}
	var members map[string]json.RawMessage
	if err := json.Unmarshal(record.Value, &members); err != nil {
		t.Fatalf("decode the record of %s: %v", turnID, err)
	}
	return members, record.Version
}

// writeMembers replaces a run's record with members, as a peer's write would.
func writeMembers(t *testing.T, fleet *memory.Fleet, turnID string, members map[string]json.RawMessage, version uint64) {
	t.Helper()
	raw, err := json.Marshal(members)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	if won, err := fleet.UpdateSandboxRun(t.Context(), turnID, raw, version); err != nil || !won {
		t.Fatalf("UpdateSandboxRun %s = %v, %v", turnID, won, err)
	}
}

// A MEMBER THIS BUILD DOES NOT KNOW SURVIVES EVERY WRITE IT MAKES.
//
// Every write to a run is a read-modify-write of its whole record, so a build
// that decoded only its own fields would erase, on its first write, whatever a
// newer peer added — a charge record, a held answer — and that peer would then
// act as though it had never been written. Each write the lifecycle makes is
// walked here, and the member a newer build wrote is on the record after it,
// byte for byte.
func TestAMemberThisBuildDoesNotKnowSurvivesEveryWrite(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	fleet := memory.NewFleet()
	store := sandbox.NewCoordStore(fleet)
	if err := store.BeginLaunch(ctx, sandbox.PendingRun{
		TurnID: "t1", AgentHandle: "swe", ConversationKey: "chat:C1",
	}, sandbox.Fence{}); err != nil {
		t.Fatalf("BeginLaunch: %v", err)
	}
	members, version := rawMembers(t, fleet, "t1")
	newer := json.RawMessage(`{"launch":"l-9","counted":true}`)
	members["a_newer_builds_fact"] = newer
	writeMembers(t, fleet, "t1", members, version)

	launch := func() string {
		run, _, err := store.Get(ctx, "t1")
		if err != nil {
			t.Fatalf("Get: %v", err)
		}
		return run.LaunchID
	}
	var claimed sandbox.PendingRun
	for _, step := range []struct {
		name  string
		write func() error
	}{
		{"suspend", func() error {
			_, err := store.MarkSuspended(ctx, "t1", map[string]any{"version": 2})
			return err
		}},
		{"attach a box", func() error {
			return store.AttachSandbox(ctx, "t1", sandbox.BoxRef{SandboxID: "box-1"}, sandbox.Fence{})
		}},
		{"append a bridged call", func() error {
			_, err := store.AppendBridgeCall(ctx, "t1", sandbox.BridgeCall{Name: "read_page"})
			return err
		}},
		{"claim", func() error {
			var err error
			claimed, _, err = store.ClaimForResume(ctx, "t1", sandbox.CompletionTail(launch()))
			return err
		}},
		{"hand the claim back", func() error {
			_, err := store.ReleaseClaim(ctx, "t1", sandbox.Release{
				Launch: claimed.LaunchID, To: claimed.ClaimedFrom, Charged: true,
			})
			return err
		}},
		{"pause the box", func() error { return store.MarkBoxPaused(ctx, "t1", time.Now()) }},
		{"park on a question", func() error {
			return store.MarkAwaiting(ctx, "t1", sandbox.Clarification{Question: "which branch?"})
		}},
		{"hold a reply", func() error {
			_, err := store.HoldAnswer(ctx, "t1", sandbox.HeldAnswer{Launch: launch(), Text: "main"})
			return err
		}},
		{"expire the pause", func() error {
			_, err := store.ExpirePause(ctx, "t1")
			return err
		}},
		{"take ownership", func() error {
			_, err := store.ClaimOwnership(ctx, "t1", "node-b:2", 3)
			return err
		}},
		{"change status", func() error {
			return store.SetStatus(ctx, "t1", sandbox.StatusRunning, sandbox.Fence{})
		}},
		{"release the box", func() error { return store.ReleaseBox(ctx, "t1") }},
		{"launch again", func() error {
			return store.BeginLaunch(ctx, sandbox.PendingRun{TurnID: "t1"}, sandbox.Fence{})
		}},
	} {
		if err := step.write(); err != nil {
			t.Fatalf("%s: %v", step.name, err)
		}
		got, _ := rawMembers(t, fleet, "t1")
		if !bytes.Equal(got["a_newer_builds_fact"], newer) {
			t.Fatalf("%s left the newer build's member as %s, want %s", step.name,
				got["a_newer_builds_fact"], newer)
		}
	}
}

// A MEMBER DECODED INTO A FIELD IS THAT FIELD'S, AND ONLY THAT FIELD'S.
//
// The decoder matches a member to a field case-insensitively, so a member
// spelled "Charged" sets Charged. Carried beside the field as well, it would
// come back on the next read whatever the field was set to since: a launch
// clears the previous job's charge record, and the carried copy would restore
// it, so the new job's spend would never reach the counter.
func TestAMemberDecodedIntoAFieldIsNotCarriedBesideIt(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	fleet := memory.NewFleet()
	store := sandbox.NewCoordStore(fleet)
	if err := store.BeginLaunch(ctx, sandbox.PendingRun{TurnID: "t1", AgentHandle: "swe"},
		sandbox.Fence{}); err != nil {
		t.Fatalf("BeginLaunch: %v", err)
	}
	members, version := rawMembers(t, fleet, "t1")
	members["Charged"] = json.RawMessage(`true`)
	writeMembers(t, fleet, "t1", members, version)
	if run, _, err := store.Get(ctx, "t1"); err != nil || !run.Charged {
		t.Fatalf("the premise: the member sets the field (charged=%v, %v)", run.Charged, err)
	}

	if err := store.BeginLaunch(ctx, sandbox.PendingRun{TurnID: "t1"}, sandbox.Fence{}); err != nil {
		t.Fatalf("BeginLaunch: %v", err)
	}
	if run, _, err := store.Get(ctx, "t1"); err != nil || run.Charged {
		t.Errorf("the second launch reads charged=%v (%v): a copy of the member carried beside "+
			"the field undid the launch's clear", run.Charged, err)
	}
}

// AN ANSWER HELD FOR ANOTHER LAUNCH IS NOBODY'S. A build that does not know the
// field carries it through a relaunch as it carries every member it does not
// know, so it names its launch, and the row's own launch has to match it.
func TestAnAnswerHeldForAnotherLaunchIsNobodys(t *testing.T) {
	t.Parallel()
	run := sandbox.PendingRun{LaunchID: "l-2", HeldAnswer: &sandbox.HeldAnswer{Launch: "l-1", Text: "old"}}
	if held, ok := run.Held(); ok {
		t.Errorf("a relaunched row reads %+v as held for its new launch", held)
	}
	run.HeldAnswer.Launch = "l-2"
	if held, ok := run.Held(); !ok || held.Text != "old" {
		t.Errorf("the row's own launch's answer reads %+v, %v", held, ok)
	}
}

// A RESUME MINTS UNDER THE TURN'S INSTANT, NOT ITS FIRST LAUNCH'S.
//
// A write the turn made before its first launch and makes again after the
// resume derives the same operation id, which the node deciding it compares
// against the instant the resume carries: the first launch's is later than
// that write, and reads it as newer than an adoption it predates. The earlier
// of the two when both are known, and the launch where the row carries no
// instant — one a build that predates the field wrote.
func TestAResumeMintsUnderTheEarlierOfTheTurnsInstantAndItsFirstLaunch(t *testing.T) {
	t.Parallel()
	launched := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name      string
		triggered time.Time
		want      time.Time
	}{
		{"the turn's instant, before its launch", launched.Add(-time.Hour), launched.Add(-time.Hour)},
		{"an instant after the launch", launched.Add(time.Minute), launched},
		{"no instant on the row", time.Time{}, launched},
	} {
		run := sandbox.PendingRun{CreatedAt: launched, TriggeredAt: tc.triggered}
		if got := run.TriggerInstant(); !got.Equal(tc.want) {
			t.Errorf("%s: TriggerInstant = %v, want %v", tc.name, got, tc.want)
		}
	}
	if got := (sandbox.PendingRun{TriggeredAt: launched}).TriggerInstant(); !got.Equal(launched) {
		t.Errorf("a row with no launch instant answers %v, want the turn's own", got)
	}
}
