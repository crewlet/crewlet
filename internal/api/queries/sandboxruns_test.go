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
	r := queries.NewRegistry()
	queries.Register(r, queries.Sources{Sandbox: store})
	got, err := r.Answer(everyGrant(t), "sandbox_runs", nil, "")
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
	if _, _, err := store.Finish(t.Context(), "t1", sandbox.Fence{}, sandbox.Active); err != nil {
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

// The pause instant is a second, warn-only write after the box is already
// paused, so a run parked on a question can hold a snapshot nothing dated.
// Drawing the raw stamp put that box on the board as a LIVE one — the single
// reading that says nobody is being billed for it — while the reaper was
// counting the same box as held.
func TestAParkedBoxWithNoPauseStampStillReadsAsHeld(t *testing.T) {
	store := seedRuns(t, sandbox.PendingRun{
		TurnID: "t1", AgentHandle: "swe", Status: sandbox.StatusRunning, CreatedAt: runBase,
	})
	if err := store.AttachSandbox(t.Context(), "t1", sandbox.BoxRef{
		SandboxID: "box-1", PauseTTLSec: 1800,
	}, sandbox.Fence{}); err != nil {
		t.Fatalf("AttachSandbox: %v", err)
	}
	// The park lands; the stamp that would have dated it does not.
	if err := store.MarkAwaiting(t.Context(), "t1", sandbox.Clarification{
		Question: "which branch?", Audience: "requester",
	}); err != nil {
		t.Fatalf("MarkAwaiting: %v", err)
	}

	rows := askRuns(t, store)
	if rows[0]["box_exists"] != true {
		t.Fatal("box_exists is false with a box attached")
	}
	if rows[0]["paused_at"] == "" {
		t.Fatal("a box held for an open-ended human wait reads as a live one, so the board " +
			"shows nothing being paid for")
	}
}

// Telling somebody to "reply in the thread" when the run was started by a
// schedule tick sends them to a thread that does not exist — and such a run
// stores no conversation at all, which is what the column has to read.
func TestARunNoChatCanAnswerSaysSo(t *testing.T) {
	store := seedRuns(t,
		sandbox.PendingRun{TurnID: "chat", AgentHandle: "swe", Status: sandbox.StatusAwaiting,
			PartitionKey: "chat:D1:1699.1", ConversationKey: "chat:D1",
			CreatedAt: runBase},
		// THE PER-EVENT FALLBACK NAMESPACE, which this engine mints at
		// READ time for the broker's partition function and writes onto
		// no row — so this row shape is a peer's or a later writer's, and
		// the column has to refuse it either way: no inbound message can
		// reproduce a key derived from an event id.
		sandbox.PendingRun{TurnID: "eventkey", AgentHandle: "swe", Status: sandbox.StatusAwaiting,
			PartitionKey: "event:018f-…", ConversationKey: "event:018f-…",
			CreatedAt: runBase.Add(time.Minute)},
		// WHAT A SCHEDULE TICK ACTUALLY LEAVES: nothing at all. Its
		// trigger names neither key, so neither is stamped and neither is
		// stored.
		sandbox.PendingRun{TurnID: "none", AgentHandle: "swe", Status: sandbox.StatusAwaiting,
			CreatedAt: runBase.Add(2 * time.Minute)},
		// A ROW FROM BEFORE THE SPLIT carries only the partition key, and
		// the column is answered off the conversation — so the identity
		// read has to fall back to it or every parked run written by an
		// older build is reported unanswerable while a person is in fact
		// waiting in that thread.
		sandbox.PendingRun{TurnID: "presplit", AgentHandle: "swe", Status: sandbox.StatusAwaiting,
			PartitionKey: "chat:D1:1699.9", CreatedAt: runBase.Add(3 * time.Minute)},
	)
	for _, id := range []string{"chat", "eventkey", "none", "presplit"} {
		if err := store.SetStatus(t.Context(), id, sandbox.StatusAwaiting, sandbox.Fence{}); err != nil {
			t.Fatalf("SetStatus: %v", err)
		}
	}
	want := map[string]bool{"chat": true, "eventkey": false, "none": false, "presplit": true}
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
	if _, err := r.Answer(everyGrant(t), "sandbox_runs", nil, ""); err == nil {
		t.Fatal("a registry with no run record answered the question")
	}
}

// unreachableRuns is a run record whose store could not be reached.
type unreachableRuns struct{}

func (unreachableRuns) ListActive(context.Context) ([]sandbox.PendingRun, error) {
	return nil, fmt.Errorf("sandbox: list runs: %w", coord.ErrUnavailable)
}

// The run board reads the fleet's coordination store, so a blip there is "ask
// again in a moment" like any other, not a failure. It reached the client as
// `query_failed` and a 500, where the reference promised a 503.
func TestAnUnreachableRunRecordIsUnavailableRatherThanFailed(t *testing.T) {
	t.Parallel()
	r := queries.NewRegistry()
	queries.Register(r, queries.Sources{Sandbox: unreachableRuns{}})
	if _, err := r.Answer(everyGrant(t), "sandbox_runs", nil, ""); !errors.Is(err, queries.ErrUnavailable) {
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
