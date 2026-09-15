package queries_test

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/api/queries"
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

func askRunsWith(t *testing.T, store queries.PendingRuns,
	params map[string]any) []map[string]any {

	t.Helper()
	r := queries.NewRegistry()
	queries.Register(r, queries.Sources{Sandbox: store})
	got, err := r.Answer(t.Context(), "sandbox_runs", params, "")
	if err != nil {
		t.Fatalf("sandbox_runs%v: %v", params, err)
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

// A terminal run is not something anybody can act on, and a board that showed
// them would grow without bound.
func TestASettledRunLeavesTheBoard(t *testing.T) {
	store := seedRuns(t, sandbox.PendingRun{
		TurnID: "t1", AgentHandle: "swe", Status: sandbox.StatusRunning, CreatedAt: runBase,
	})
	if err := store.SetStatus(t.Context(), "t1", sandbox.StatusDone, sandbox.Fence{}); err != nil {
		t.Fatalf("SetStatus: %v", err)
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

// Without a sandbox backend no run can be parked, so there is nothing this
// question could describe — unknown is the honest answer, not an empty board.
func TestANodeWithNoSandboxDoesNotAnswerTheQuestion(t *testing.T) {
	r := queries.NewRegistry()
	queries.Register(r, queries.Sources{})
	if _, err := r.Answer(t.Context(), "sandbox_runs", nil, ""); err == nil {
		t.Fatal("a node with no sandbox answered the question")
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

// A RUN'S RECORD OUTLIVES ITS RUN, and the board never served it.
//
// The answer read `ListActive`, which is the RECOVERY path's question — what
// still owns engine state — so a finished or failed run left the screen at the
// exact moment somebody would go looking for it. "What did the coding runs do
// today" had no answer anywhere in the product, while the store held every one
// of them.
func TestTheRetainedRunsAreServableAndActiveIsTheDefault(t *testing.T) {
	t.Parallel()
	store := seedRuns(t,
		sandbox.PendingRun{TurnID: "t-live", Role: "Dev", Status: sandbox.StatusRunning},
		sandbox.PendingRun{TurnID: "t-done", Role: "Dev", Status: sandbox.StatusDone},
		sandbox.PendingRun{TurnID: "t-bad", Role: "Dev", Status: sandbox.StatusFailed},
	)
	ids := func(params map[string]any) []string {
		t.Helper()
		out := []string{}
		for _, row := range askRunsWith(t, store, params) {
			out = append(out, fmt.Sprint(row["turn_id"]))
		}
		slices.Sort(out)
		return out
	}

	// ACTIVE BY DEFAULT, which is what a board watching a working company
	// is for — and is what already shipped, so a caller naming nothing sees
	// no change.
	if got := ids(nil); !slices.Equal(got, []string{"t-live"}) {
		t.Errorf("the default gave %v, want only the active run", got)
	}
	if got := ids(map[string]any{"status": "done"}); !slices.Equal(got, []string{"t-done"}) {
		t.Errorf("status=done gave %v", got)
	}
	if got := ids(map[string]any{"status": "failed"}); !slices.Equal(got, []string{"t-bad"}) {
		t.Errorf("status=failed gave %v", got)
	}
	if got := ids(map[string]any{"status": "all"}); !slices.Equal(got,
		[]string{"t-bad", "t-done", "t-live"}) {

		t.Errorf("status=all gave %v, want every run this store holds", got)
	}
}

// A STATUS NOBODY DEFINES IS REFUSED NAMING THE FOUR, rather than silently
// answering the default — which would hand a caller the active runs under a
// heading saying "failed".
func TestAnUnknownRunStatusIsRefusedNamingTheSets(t *testing.T) {
	t.Parallel()
	r := queries.NewRegistry()
	queries.Register(r, queries.Sources{Sandbox: seedRuns(t)})
	_, err := r.Answer(t.Context(), "sandbox_runs", map[string]any{"status": "parked"}, "")
	if !errors.Is(err, queries.ErrBadParams) {
		t.Fatalf("status=parked answered %v, want bad params", err)
	}
	if !strings.Contains(err.Error(), "active") || !strings.Contains(err.Error(), "all") {
		t.Errorf("the refusal is %q and does not name what would have worked", err)
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
		BridgeCalls: []sandbox.BridgeCall{
			{Name: "get_work_item", Args: `{"id":"ENG-1"}`, At: called},
		},
		BridgeCallsElided: 4,
	})
	row := askRuns(t, store)[0]
	switch {
	case row["reply"] != "chat:C1":
		t.Errorf("reply = %v, want who is waiting", row["reply"])
	case row["session_id"] != "sess-9":
		t.Errorf("session_id = %v", row["session_id"])
	case row["command_id"] != "cmd-3":
		t.Errorf("command_id = %v", row["command_id"])
	case row["bridge_calls_elided"] != 4:
		t.Errorf("bridge_calls_elided = %v — a log that silently skips is a "+
			"log that lies about what the run did", row["bridge_calls_elided"])
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
