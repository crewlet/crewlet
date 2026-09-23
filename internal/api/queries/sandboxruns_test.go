package queries_test

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/api/queries"
	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/coord/memory"
	"github.com/crewlet/crewlet/internal/sandbox"
)

var runBase = time.Date(2026, 8, 23, 12, 0, 0, 0, time.UTC)

// seedRuns puts records in the fleet store the engine itself uses, over the
// in-process coordination twin — the same implementation, held to the same
// contract suite, rather than a stand-in for it.
func seedRuns(t *testing.T, runs ...sandbox.PendingRun) *sandbox.CoordStore {
	t.Helper()
	store := sandbox.NewCoordStore(memory.NewFleet())
	for _, run := range runs {
		// Through the launch, because that is the only way a row comes to
		// exist: it opens launching and is moved from there.
		if err := store.BeginLaunch(context.Background(), run, sandbox.Fence{}); err != nil {
			t.Fatalf("BeginLaunch: %v", err)
		}
		if run.Status == "" || run.Status == sandbox.StatusLaunching {
			continue
		}
		if err := store.SetStatus(context.Background(), run.TurnID, run.Status, sandbox.Fence{}); err != nil {
			t.Fatalf("SetStatus %s: %v", run.TurnID, err)
		}
	}
	return store
}

func askRuns(t *testing.T, store queries.PendingRuns) []map[string]any {
	t.Helper()
	return askRunsWith(t, store, nil)
}

func askRunsWith(t *testing.T, store queries.PendingRuns, params map[string]any) []map[string]any {
	t.Helper()
	r := queries.NewRegistry()
	queries.Register(r, queries.Sources{Sandbox: store})
	got, err := r.Answer(t.Context(), "sandbox_runs", params, "")
	if err != nil {
		t.Fatalf("sandbox_runs: %v", err)
	}
	payload, ok := got.(map[string]any)
	if !ok {
		t.Fatalf("answer is %T", got)
	}
	list, _ := payload["runs"].([]any)
	out := make([]map[string]any, 0, len(list))
	for _, row := range list {
		m, _ := row.(map[string]any)
		out = append(out, m)
	}
	return out
}

// A run parked on a question can wait days, and the live projection sweeps
// long before that — which made the states most needing a person the ones
// least likely to be on screen.
func TestTheDurableRecordAnswersForEveryActiveState(t *testing.T) {
	store := seedRuns(t,
		sandbox.PendingRun{TurnID: "t1", AgentHandle: "swe", Role: "SWE",
			Status: sandbox.StatusRunning, CreatedAt: runBase},
		sandbox.PendingRun{TurnID: "t2", AgentHandle: "swe", Role: "SWE",
			Status: sandbox.StatusAwaiting, CreatedAt: runBase.Add(time.Minute)},
		// A reseed run — its pause expired, its box was reclaimed, its
		// work is safe on a pushed branch — had no surface anywhere. It
		// looked exactly like work that had finished.
		sandbox.PendingRun{TurnID: "t3", AgentHandle: "swe", Role: "SWE",
			Status: sandbox.StatusReseed, CreatedAt: runBase.Add(2 * time.Minute)},
	)
	if err := store.SetStatus(t.Context(), "t2", sandbox.StatusAwaiting, sandbox.Fence{}); err != nil {
		t.Fatalf("SetStatus: %v", err)
	}
	if err := store.SetStatus(t.Context(), "t3", sandbox.StatusReseed, sandbox.Fence{}); err != nil {
		t.Fatalf("SetStatus: %v", err)
	}

	rows := askRuns(t, store)
	if len(rows) != 3 {
		t.Fatalf("got %d rows, want every active run", len(rows))
	}
	seen := map[string]bool{}
	for _, row := range rows {
		seen[row["status"].(string)] = true
	}
	for _, want := range []string{sandbox.StatusRunning, sandbox.StatusAwaiting, sandbox.StatusReseed} {
		if !seen[want] {
			t.Fatalf("no %q run on the board; saw %v", want, seen)
		}
	}
}

// A settled run is not something anybody can act on, and a board that showed
// them would grow without bound.
func TestASettledRunLeavesTheBoard(t *testing.T) {
	store := seedRuns(t, sandbox.PendingRun{
		TurnID: "t1", AgentHandle: "swe", Status: sandbox.StatusRunning, CreatedAt: runBase,
	})
	if _, err := store.Finish(t.Context(), "t1", sandbox.Fence{}); err != nil {
		t.Fatalf("Finish: %v", err)
	}
	if rows := askRuns(t, store); len(rows) != 0 {
		t.Fatalf("a finished run is still on the board: %v", rows)
	}
}

// It is by far the largest column in the row, and every prompt in it is
// already reachable through the event store.
func TestTheSuspendedConversationIsNotShipped(t *testing.T) {
	store := seedRuns(t, sandbox.PendingRun{
		TurnID: "t1", AgentHandle: "swe", CreatedAt: runBase,
	})
	// The write that carries the conversation is also the one that moves
	// the run to running, so this leaves the row exactly as a suspended
	// turn leaves it.
	suspended, err := store.MarkSuspended(t.Context(), "t1", map[string]any{
		"messages": []any{map[string]any{"content": "a very long system prompt"}},
	})
	if err != nil || !suspended {
		t.Fatalf("MarkSuspended: suspended=%v err=%v", suspended, err)
	}
	rows := askRuns(t, store)
	for _, key := range []string{"execute_state", "messages", "plan"} {
		if _, present := rows[0][key]; present {
			t.Fatalf("%q reached a board that renders one line per run", key)
		}
	}
}

// The board draws two FACTS, not two ids: whether a box exists at all, and
// whether it is currently held as a snapshot somebody is paying for.
func TestTheBoardIsToldWhetherABoxExistsAndWhetherItIsHeld(t *testing.T) {
	store := seedRuns(t, sandbox.PendingRun{
		TurnID: "t1", AgentHandle: "swe", Status: sandbox.StatusRunning, CreatedAt: runBase,
	})
	rows := askRuns(t, store)
	if rows[0]["box_exists"] != false {
		t.Fatalf("box_exists = %v before a box was attached", rows[0]["box_exists"])
	}
	if rows[0]["paused_at"] != "" {
		t.Fatalf("paused_at = %v with no snapshot held", rows[0]["paused_at"])
	}
	if _, leaked := rows[0]["sandbox_id"]; leaked {
		t.Fatal("the board was given a provider id instead of the fact it draws")
	}

	if err := store.AttachSandbox(t.Context(), "t1", sandbox.BoxRef{
		SandboxID: "box-1", PauseTTLSec: 1800,
	}, sandbox.Fence{}); err != nil {
		t.Fatalf("AttachSandbox: %v", err)
	}
	if err := store.MarkBoxPaused(t.Context(), "t1", runBase); err != nil {
		t.Fatalf("MarkBoxPaused: %v", err)
	}
	rows = askRuns(t, store)
	if rows[0]["box_exists"] != true {
		t.Fatal("box_exists is false with a box attached")
	}
	if rows[0]["paused_at"] == "" {
		t.Fatal("a held snapshot is invisible, so nobody can see what is being paid for")
	}
	if rows[0]["pause_ttl_seconds"] != 1800.0 {
		t.Fatalf("pause_ttl_seconds = %v", rows[0]["pause_ttl_seconds"])
	}
}

// Telling somebody to "reply in the thread" when the run was started by a
// schedule tick sends them to a thread that does not exist.
func TestARunNoChatCanAnswerSaysSo(t *testing.T) {
	store := seedRuns(t,
		sandbox.PendingRun{TurnID: "chat", AgentHandle: "swe", Status: sandbox.StatusAwaiting,
			ConversationKey: "slack:C1:1699.1", CreatedAt: runBase},
		sandbox.PendingRun{TurnID: "tick", AgentHandle: "swe", Status: sandbox.StatusAwaiting,
			ConversationKey: "event:018f-…", CreatedAt: runBase.Add(time.Minute)},
		sandbox.PendingRun{TurnID: "none", AgentHandle: "swe", Status: sandbox.StatusAwaiting,
			CreatedAt: runBase.Add(2 * time.Minute)},
	)
	for _, id := range []string{"chat", "tick", "none"} {
		if err := store.SetStatus(t.Context(), id, sandbox.StatusAwaiting, sandbox.Fence{}); err != nil {
			t.Fatalf("SetStatus: %v", err)
		}
	}
	want := map[string]bool{"chat": true, "tick": false, "none": false}
	for _, row := range askRuns(t, store) {
		id := row["turn_id"].(string)
		if row["answerable_in_chat"] != want[id] {
			t.Fatalf("%s: answerable_in_chat = %v, want %v", id, row["answerable_in_chat"], want[id])
		}
	}
}

// With no run record to read there is nothing this question could describe,
// so it is unregistered: unknown is the honest answer, not an empty board.
func TestARegistryWithNoRunRecordDoesNotAnswerTheQuestion(t *testing.T) {
	r := queries.NewRegistry()
	queries.Register(r, queries.Sources{})
	if _, err := r.Answer(t.Context(), "sandbox_runs", nil, ""); err == nil {
		t.Fatal("a registry with no run record answered the question")
	}
}

// unreachableRuns is a run record whose store could not be reached.
type unreachableRuns struct{}

func (unreachableRuns) ListActive(context.Context) ([]sandbox.PendingRun, error) {
	return nil, fmt.Errorf("sandbox: list runs: %w", coord.ErrUnavailable)
}

func (unreachableRuns) BridgeCallPage(context.Context, sandbox.PendingRun, uint64, int) (sandbox.BridgeCallPage, error) {
	return sandbox.BridgeCallPage{}, fmt.Errorf("sandbox: read the bridged calls: %w", coord.ErrUnavailable)
}

// The run board reads the fleet's coordination store, so a blip there is "ask
// again in a moment" like any other, not a failure. It reached the client as
// `query_failed` and a 500, where the reference promised a 503.
func TestAnUnreachableRunRecordIsUnavailableRatherThanFailed(t *testing.T) {
	t.Parallel()
	r := queries.NewRegistry()
	queries.Register(r, queries.Sources{Sandbox: unreachableRuns{}})
	if _, err := r.Answer(t.Context(), "sandbox_runs", nil, ""); !errors.Is(err, queries.ErrUnavailable) {
		t.Fatalf("an unreachable run record answered %v, want ErrUnavailable", err)
	}
}

// WHERE A RUN IS RUNNING IS AN OPERATOR QUESTION now that providers.sandbox is
// a catalogue: one company runs some seats on the engine host and others in a
// remote box, and this board is the only surface that could answer it.
//
// A row written before the field existed answers empty rather than failing —
// a rolling upgrade has one build writing the placement and another not.
func TestTheBoardSaysWhereEachRunIs(t *testing.T) {
	store := seedRuns(t,
		sandbox.PendingRun{TurnID: "t1", AgentHandle: "swe", Role: "SWE",
			Placement: "e2b", Status: sandbox.StatusRunning, CreatedAt: runBase},
		sandbox.PendingRun{TurnID: "t2", AgentHandle: "swe", Role: "SWE",
			Placement: "direct", Status: sandbox.StatusRunning, CreatedAt: runBase},
		sandbox.PendingRun{TurnID: "t3", AgentHandle: "swe", Role: "SWE",
			Status: sandbox.StatusRunning, CreatedAt: runBase},
	)
	where := make(map[string]any)
	for _, row := range askRuns(t, store) {
		where[row["turn_id"].(string)] = row["placement"]
	}
	for turn, want := range map[string]string{"t1": "e2b", "t2": "direct", "t3": ""} {
		if got := where[turn]; got != want {
			t.Errorf("run %s reports placement %v, want %q", turn, got, want)
		}
	}
}

// THE THREE FACTS A PARKED RUN HAS AND THE BOARD COULD NOT SHOW: who is
// waiting on it, what it called through the bridge, and the identifiers that
// find it in somebody else's system.
//
// `reply` is persisted precisely because the resumed turn does not see its
// trigger; `bridge_calls` is the ONLY copy of a bridged run's tool log, since
// those calls are made by a process outside the engine minutes apart and
// possibly across a restart. Without them the run page's Calls tab had nothing
// to render and "is anybody waiting on this" had no answer.
func TestAParkedRunSaysWhoIsWaitingAndWhatItCalled(t *testing.T) {
	t.Parallel()
	called := time.Date(2026, 6, 1, 10, 0, 0, 0, time.UTC)
	store := seedRuns(t, sandbox.PendingRun{
		TurnID: "t-1", Role: "Dev", Status: sandbox.StatusAwaiting,
		Reply:     "chat:C1",
		SessionID: "sess-9", CommandID: "cmd-3",
		DelegationChain: []string{"agent-pm", "agent-swe"},
	})
	if ok, err := store.AppendBridgeCall(t.Context(), "t-1", sandbox.BridgeCall{
		Name: "get_work_item", Args: `{"id":"ENG-1"}`, At: called,
	}); err != nil || !ok {
		t.Fatalf("AppendBridgeCall = %v, %v", ok, err)
	}
	row := askRuns(t, store)[0]
	switch {
	case row["reply"] != "chat:C1":
		t.Errorf("reply = %v, want who is waiting", row["reply"])
	case row["session_id"] != "sess-9":
		t.Errorf("session_id = %v", row["session_id"])
	case row["command_id"] != "cmd-3":
		t.Errorf("command_id = %v", row["command_id"])
	case row["bridge_calls_total"] != 1:
		t.Errorf("bridge_calls_total = %v, want the one call", row["bridge_calls_total"])
	case row["bridge_calls_elided"] != 0:
		t.Errorf("bridge_calls_elided = %v on a log that drops nothing", row["bridge_calls_elided"])
	}
	if _, more := row["bridge_calls_next"]; more {
		t.Errorf("bridge_calls_next = %v on a log that fits one page", row["bridge_calls_next"])
	}
	calls, ok := row["bridge_calls"].([]sandbox.BridgeCall)
	if !ok || len(calls) != 1 || calls[0].Name != "get_work_item" {
		t.Fatalf("bridge_calls = %#v, want the run's own tool log", row["bridge_calls"])
	}
	chain, ok := row["delegation_chain"].([]string)
	if !ok || !slices.Equal(chain, []string{"agent-pm", "agent-swe"}) {
		t.Fatalf("delegation_chain = %#v", row["delegation_chain"])
	}
}

// A LONG RUN'S CALLS COME A PAGE AT A TIME, and every one of them is reachable.
//
// A bridged run can make thousands of calls, each carrying its output, so the
// whole log in one answer is an answer of any size at all. The board carries
// the first page and a cursor, and `turn_id` with `calls_after` reads on from
// it — each call exactly once, in the order the run made them.
func TestABridgedRunsCallsArePagedWithACursor(t *testing.T) {
	t.Parallel()
	store := seedRuns(t,
		sandbox.PendingRun{TurnID: "t-long", AgentHandle: "swe", Status: sandbox.StatusRunning, CreatedAt: runBase},
		sandbox.PendingRun{TurnID: "t-other", AgentHandle: "swe", Status: sandbox.StatusRunning, CreatedAt: runBase},
	)
	const total = 7
	for i := range total {
		if ok, err := store.AppendBridgeCall(t.Context(), "t-long", sandbox.BridgeCall{
			Name: fmt.Sprintf("c%d", i),
		}); err != nil || !ok {
			t.Fatalf("AppendBridgeCall = %v, %v", ok, err)
		}
	}

	var seen []string
	params := map[string]any{"turn_id": "t-long", "calls_limit": 3.0}
	for pages := 0; ; pages++ {
		if pages > total {
			t.Fatal("the pages never ended")
		}
		rows := askRunsWith(t, store, params)
		if len(rows) != 1 || rows[0]["turn_id"] != "t-long" {
			t.Fatalf("turn_id did not narrow the answer to the one run: %d rows", len(rows))
		}
		row := rows[0]
		if row["bridge_calls_total"] != total {
			t.Errorf("bridge_calls_total = %v, want %d on every page", row["bridge_calls_total"], total)
		}
		calls, _ := row["bridge_calls"].([]sandbox.BridgeCall)
		if len(calls) > 3 {
			t.Fatalf("a page of limit 3 carried %d calls", len(calls))
		}
		for _, call := range calls {
			seen = append(seen, call.Name)
		}
		next, more := row["bridge_calls_next"]
		if !more {
			break
		}
		params = map[string]any{"turn_id": "t-long", "calls_limit": 3.0,
			"calls_after": float64(next.(uint64))}
	}
	if want := []string{"c0", "c1", "c2", "c3", "c4", "c5", "c6"}; !slices.Equal(seen, want) {
		t.Errorf("paged %q, want %q", seen, want)
	}
}

// A CURSOR BELONGS TO ONE RUN'S LOG. Applied to the whole board it would start
// every run's page at another run's place, so it is refused without a run to
// belong to — as is a cursor no answer could have handed out.
func TestACursorWithoutItsRunIsRefused(t *testing.T) {
	t.Parallel()
	store := seedRuns(t, sandbox.PendingRun{
		TurnID: "t-1", AgentHandle: "swe", Status: sandbox.StatusRunning, CreatedAt: runBase,
	})
	r := queries.NewRegistry()
	queries.Register(r, queries.Sources{Sandbox: store})
	for _, params := range []map[string]any{
		{"calls_after": 3.0},
		{"turn_id": "t-1", "calls_after": -1.0},
	} {
		if _, err := r.Answer(t.Context(), "sandbox_runs", params, ""); !errors.Is(err, queries.ErrBadParams) {
			t.Errorf("params %v answered %v, want ErrBadParams", params, err)
		}
	}
}

// A RUN AN OLDER BUILD RECORDED still shows the calls it kept, and says how
// many of them its bounded list dropped. Its calls are on the run's row rather
// than in records of their own, and the board reads them from there.
func TestARunAnOlderBuildRecordedShowsWhatItsRowDropped(t *testing.T) {
	t.Parallel()
	fleet := memory.NewFleet()
	raw := []byte(`{"turn_id":"t-older","status":"running","launch_id":"launch-older",` +
		`"bridge_calls":[{"name":"get_work_item","at":"2026-06-01T10:00:00Z"}],` +
		`"bridge_calls_elided":4}`)
	if _, err := fleet.CreateSandboxRun(t.Context(), "t-older", raw); err != nil {
		t.Fatalf("CreateSandboxRun: %v", err)
	}
	row := askRuns(t, sandbox.NewCoordStore(fleet))[0]
	if row["bridge_calls_elided"] != 4 {
		t.Errorf("bridge_calls_elided = %v — a log that silently skips is a "+
			"log that lies about what the run did", row["bridge_calls_elided"])
	}
	calls, ok := row["bridge_calls"].([]sandbox.BridgeCall)
	if !ok || len(calls) != 1 || calls[0].Name != "get_work_item" {
		t.Fatalf("bridge_calls = %#v, want the call the row kept", row["bridge_calls"])
	}
}
