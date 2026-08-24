package queries_test

import (
	"context"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/api/queries"
	"github.com/crewlet/crewlet/internal/sandbox"
)

var runBase = time.Date(2026, 8, 23, 12, 0, 0, 0, time.UTC)

// seedRuns puts rows in a memory store, which is the twin the contract suite
// holds to the same behaviour as the SQL one.
func seedRuns(t *testing.T, runs ...sandbox.PendingRun) *sandbox.MemoryStore {
	t.Helper()
	store := sandbox.NewMemoryStore()
	for _, run := range runs {
		if err := store.Create(context.Background(), run); err != nil {
			t.Fatalf("Create: %v", err)
		}
	}
	return store
}

func askRuns(t *testing.T, store queries.PendingRuns) []map[string]any {
	t.Helper()
	r := queries.NewRegistry()
	queries.Register(r, queries.Sources{Sandbox: store})
	got, err := r.Answer(t.Context(), "sandbox_runs", nil, "")
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
		TurnID: "t1", AgentHandle: "swe", Status: sandbox.StatusRunning, CreatedAt: runBase,
	})
	if err := store.SaveExecuteState(t.Context(), "t1", map[string]any{
		"messages": []any{map[string]any{"content": "a very long system prompt"}},
	}); err != nil {
		t.Fatalf("SaveExecuteState: %v", err)
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
