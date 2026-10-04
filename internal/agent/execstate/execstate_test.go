package execstate_test

import (
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
		Uncharged:  &execstate.Uncharged{Turns: 1, Input: 900, Output: 120, Workers: 1},
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
	// WHAT NO ITEM HAS BEEN CHARGED FOR YET. The segment that finishes the
	// turn pays it, and it is often another process on another node, so a
	// row that loses it loses the first half of the turn from its task.
	if got.Uncharged == nil || *got.Uncharged != *want.Uncharged {
		t.Fatalf("the uncharged spend did not survive: %+v", got.Uncharged)
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
