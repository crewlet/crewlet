package execstate_test

import (
	"encoding/json"
	"errors"
	"testing"

	"github.com/crewlet/crewlet/internal/agent/execstate"
	"github.com/crewlet/crewlet/internal/agent/ledger"
	"github.com/crewlet/crewlet/internal/events/types"
	llm "github.com/crewlet/crewlet/internal/providers/llm"
)

// suspended is a well-formed state: a conversation ending with one unanswered
// run_sandbox call.
func suspended() execstate.State {
	return execstate.State{
		Version: execstate.Version,
		Messages: []llm.Message{
			{Role: "system", Content: "you are an engineer"},
			{Role: "user", Content: "fix the flake"},
			{Role: "assistant", ToolCalls: []llm.ToolCall{
				{ID: "call-1", Name: "run_sandbox", Arguments: map[string]any{"brief": "fix it"}},
			}},
		},
		PendingCallID:   "call-1",
		PendingCallName: "run_sandbox",
		ActiveTools:     []string{"run_sandbox", "send_message"},
		LoadedSkills:    []string{"git-auth"},
		Round:           2,
		InputTokens:     1200,
		OutputTokens:    340,
		ToolExecutions:  []types.ToolExecution{{"name": "read_file", "success": true, "round": 1}},
		RoundsUsed:      2,
		RoundNarration: []types.RoundNarration{
			{"round": 1, "reasoning": "read it first", "content": ""},
			{"round": 2, "reasoning": "", "content": "Starting a coding run."},
		},
		Iterations: []ledger.Iteration{{Iteration: 1, Intent: "fix it"}},
		Task:       "fix the flake",
	}
}

func TestAStateRoundTripsThroughTheRow(t *testing.T) {
	blob, err := execstate.Encode(suspended())
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}
	got, ok, err := execstate.Decode(blob)
	if err != nil || !ok {
		t.Fatalf("Decode = %v, %v", ok, err)
	}
	want := suspended()
	if got.PendingCallID != want.PendingCallID || got.PendingCallName != want.PendingCallName {
		t.Fatalf("pending call = %q/%q", got.PendingCallID, got.PendingCallName)
	}
	if len(got.Messages) != len(want.Messages) {
		t.Fatalf("messages = %d, want %d", len(got.Messages), len(want.Messages))
	}
	if got.Messages[2].ToolCalls[0].Name != "run_sandbox" {
		t.Fatalf("the dangling call did not survive: %+v", got.Messages[2])
	}
	if got.Round != 2 || got.InputTokens != 1200 || got.OutputTokens != 340 {
		t.Fatalf("counters lost: %+v", got)
	}
	if len(got.ActiveTools) != 2 || len(got.LoadedSkills) != 1 {
		t.Fatalf("surface state lost: %+v", got)
	}
	if len(got.ToolExecutions) != 1 || len(got.Iterations) != 1 {
		t.Fatalf("prior-work state lost: %+v", got)
	}
	// THE ROUNDS THEMSELVES. Nothing else holds them — a suspending phase
	// publishes no completed event and its progress frames are stream-only —
	// so a row that loses these loses the pre-suspend half of the phase from
	// the store for good, and the resumed half renumbers from 1.
	if got.RoundsUsed != 2 || len(got.RoundNarration) != 2 {
		t.Fatalf("the pre-suspend rounds were lost: %+v", got)
	}
	if got.RoundNarration[0]["reasoning"] != "read it first" {
		t.Fatalf("round 1's thinking did not survive: %+v", got.RoundNarration)
	}
	if got.Task != "fix the flake" {
		t.Fatalf("task = %q", got.Task)
	}
}

// A rolling upgrade means the node that resumes is routinely not the build
// that suspended. A half-understood conversation must not be acted on.
func TestAnUnknownVersionIsRefusedLoudlyRatherThanReadBestEffort(t *testing.T) {
	blob, err := execstate.Encode(suspended())
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}
	blob["version"] = float64(execstate.Version + 7)

	got, ok, err := execstate.Decode(blob)
	if !errors.Is(err, execstate.ErrUnknownVersion) {
		t.Fatalf("Decode = %+v, %v, %v; want ErrUnknownVersion", got, ok, err)
	}
	if ok {
		t.Fatal("a future version was reported as readable")
	}
}

// Encode stamps the current version, so a caller cannot write a state claiming
// to be something else.
func TestEncodeStampsTheCurrentVersion(t *testing.T) {
	state := suspended()
	state.Version = 0
	blob, err := execstate.Encode(state)
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}
	if blob["version"] != float64(execstate.Version) {
		t.Fatalf("version = %v, want %d", blob["version"], execstate.Version)
	}
}

// Zero dangling calls means there is nothing to resume into.
func TestAConversationThatAnswersEveryCallCannotBeResumed(t *testing.T) {
	state := suspended()
	state.Messages = append(state.Messages, llm.Message{
		Role: "tool", ToolCallID: "call-1", Content: "already answered",
	})
	if _, err := execstate.Encode(state); !errors.Is(err, execstate.ErrNoPendingCall) {
		t.Fatalf("Encode = %v, want ErrNoPendingCall", err)
	}
}

// Two dangling calls means the model answers one and strands the other — and
// the stranded one is a box nothing will ever collect.
func TestTwoUnansweredCallsAreRefused(t *testing.T) {
	state := suspended()
	state.Messages[2].ToolCalls = append(state.Messages[2].ToolCalls,
		llm.ToolCall{ID: "call-2", Name: "run_sandbox"})

	if _, err := execstate.Encode(state); !errors.Is(err, execstate.ErrDanglingCalls) {
		t.Fatalf("Encode = %v, want ErrDanglingCalls", err)
	}
}

// The named pending call has to be the one actually left open, or the resume
// answers a call the conversation already closed.
func TestThePendingIdMustBeTheCallThatIsActuallyOpen(t *testing.T) {
	state := suspended()
	state.PendingCallID = "call-99"
	if _, err := execstate.Encode(state); !errors.Is(err, execstate.ErrNoPendingCall) {
		t.Fatalf("Encode = %v, want ErrNoPendingCall", err)
	}
}

func TestAStateWithNoPendingCallAtAllIsRefused(t *testing.T) {
	state := suspended()
	state.PendingCallID = ""
	if _, err := execstate.Encode(state); !errors.Is(err, execstate.ErrNoPendingCall) {
		t.Fatalf("Encode = %v, want ErrNoPendingCall", err)
	}
}

// A crash between launching the job and persisting the suspend leaves an empty
// blob. That is an ordinary condition the caller settles, not a broken store.
func TestAnEmptyBlobIsNotAnError(t *testing.T) {
	got, ok, err := execstate.Decode(nil)
	if err != nil || ok {
		t.Fatalf("Decode(nil) = %+v, %v, %v; want a clean miss", got, ok, err)
	}
	if got, ok, err := execstate.Decode(map[string]any{}); err != nil || ok {
		t.Fatalf("Decode({}) = %+v, %v, %v; want a clean miss", got, ok, err)
	}
}

// The resumed loop must see a completed exchange, not a request the provider
// would reject as unanswered.
func TestAnswerClosesThePendingCall(t *testing.T) {
	state := suspended()
	msgs := state.Answer("the sandbox run succeeded")

	if len(msgs) != len(state.Messages)+1 {
		t.Fatalf("Answer produced %d messages, want %d", len(msgs), len(state.Messages)+1)
	}
	last := msgs[len(msgs)-1]
	if last.Role != "tool" || last.ToolCallID != "call-1" || last.Name != "run_sandbox" {
		t.Fatalf("the answer does not close the call: %+v", last)
	}
	if last.Content != "the sandbox run succeeded" {
		t.Fatalf("content = %q", last.Content)
	}
	// The state's own slice is untouched, so a failed resume can be retried
	// from the same state.
	if len(state.Messages) != 3 {
		t.Fatalf("Answer mutated the state: %d messages", len(state.Messages))
	}
}

// A validated state must stay valid once its answer is appended, or the
// resumed loop starts from a conversation the reader would refuse.
func TestAnAnsweredStateHasNoDanglingCallLeft(t *testing.T) {
	state := suspended()
	answered := state
	answered.Messages = state.Answer("done")
	if err := answered.Validate(); !errors.Is(err, execstate.ErrNoPendingCall) {
		t.Fatalf("Validate after Answer = %v; the call should read as closed", err)
	}
}

// v1 IS READ FOR EVER, and this is the fixture that keeps it readable.
//
// A run parked when a person was asked a question can sit for as long as its
// box's pause TTL allows. Nothing rewrites those rows — the sandbox layer
// holds the blob opaquely, which is what keeps it free of agent imports — so
// the only thing that can ever read one is a build that still knows how. The
// blob below is verbatim what the three-phase engine wrote; if this test is
// ever "fixed" by regenerating it from the current encoder, it stops
// asserting anything.
const v1Blob = `{
  "version": 1,
  "messages": [
    {"Role": "system", "Content": "you are an engineer"},
    {"Role": "user", "Content": "fix the flake"},
    {"Role": "assistant", "ToolCalls": [
      {"ID": "call-1", "Name": "run_sandbox", "Arguments": {"brief": "fix it"}}
    ]}
  ],
  "pending_tool_call_id": "call-1",
  "pending_tool_name": "run_sandbox",
  "active_tool_names": ["run_sandbox", "send_message"],
  "loaded_skill_keys": ["git-auth"],
  "iteration": 2,
  "input_tokens": 1200,
  "output_tokens": 340,
  "tool_executions": [{"name": "read_file", "success": true}],
  "iteration_history": [
    {
      "iteration": 1,
      "plan_summary": "reproduce the flake, then fix it",
      "plan_tool_calls": [{"Name": "jira_get_issue"}],
      "execute_tool_calls": [{"Name": "slack_post"}],
      "read_only_names": ["jira_get_issue"],
      "execute_text": "posted the plan",
      "review_notes": "reproduce it first",
      "completed_work": "the #eng post landed"
    }
  ],
  "task_description": "fix the flake"
}`

func TestAV1RowStillResumes(t *testing.T) {
	t.Parallel()
	var blob map[string]any
	if err := json.Unmarshal([]byte(v1Blob), &blob); err != nil {
		t.Fatalf("the fixture is not JSON: %v", err)
	}
	got, ok, err := execstate.Decode(blob)
	if err != nil || !ok {
		t.Fatalf("Decode of a v1 row = %v, %v", ok, err)
	}
	if got.Version != execstate.Version {
		t.Errorf("version = %d, want the upgrade to stamp %d", got.Version, execstate.Version)
	}
	// The unchanged half: every field spelled the same must survive, or the
	// upgrade is a parallel format that quietly drops what it did not
	// restate.
	if got.PendingCallID != "call-1" || got.Round != 2 || got.Task != "fix the flake" {
		t.Errorf("unchanged fields lost: %+v", got)
	}
	if len(got.LoadedSkills) != 1 || len(got.ToolExecutions) != 1 || len(got.Messages) != 3 {
		t.Errorf("unchanged collections lost: %+v", got)
	}

	if len(got.Iterations) != 1 {
		t.Fatalf("iterations = %d, want 1", len(got.Iterations))
	}
	round := got.Iterations[0]
	if round.Intent != "reproduce the flake, then fix it" {
		t.Errorf("intent = %q, want the v1 plan summary", round.Intent)
	}
	// PLAN'S CALLS FIRST. They really did run first, and the ledger is read
	// as a timeline — the duplicate-delivery rule depends on the order.
	if len(round.Calls) != 2 || round.Calls[0].Name != "jira_get_issue" ||
		round.Calls[1].Name != "slack_post" {
		t.Errorf("calls = %+v, want plan's then execute's", round.Calls)
	}
	if round.Text != "posted the plan" || round.ReviewNotes != "reproduce it first" ||
		round.CompletedWork != "the #eng post landed" {
		t.Errorf("round prose lost: %+v", round)
	}
}

// The other direction is a REFUSAL, not a best-effort read: a v1 build handed
// a v2 blob would find both of its call lists empty and resume believing every
// round before the suspend had called nothing — so it would re-fire whatever
// they delivered. Asserted here as the version guard that produces it.
func TestAVersionThisBuildDoesNotKnowIsRefused(t *testing.T) {
	t.Parallel()
	blob, err := execstate.Encode(suspended())
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}
	blob["version"] = float64(execstate.Version + 1)
	got, ok, err := execstate.Decode(blob)
	if !errors.Is(err, execstate.ErrUnknownVersion) {
		t.Fatalf("Decode = %+v, %v, %v; want ErrUnknownVersion", got, ok, err)
	}
}
