// Package execstate is the wire format for a suspended Execute loop.
//
// An Execute phase that launches a detached sandbox run stops mid-loop with
// the run_sandbox call UNANSWERED, and is re-entered later — after a restart,
// possibly on another node, possibly days later once a person answers a
// question. The conversation cannot be a parked goroutine: the run outlives
// the process. So it is serialized into the pending-run row and re-entered
// from there. See decisions/402-suspend-resume.md.
//
// THIS IS A WIRE FORMAT, not an implementation detail, because it crosses two
// boundaries:
//
//   - Between subsystems. The agent layer writes it; the sandbox coordinator
//     carries it back. (The coordinator never DECODES it — it holds an opaque
//     blob — which is what keeps the sandbox layer free of agent imports.)
//   - Between BUILDS. A rolling upgrade means the node that resumes a run is
//     routinely not the build that suspended it.
//
// So it carries an explicit Version, and decoding an unknown one is a LOUD
// REFUSAL that leaves the row untouched rather than a best-effort read: a
// resumed turn acting on a half-understood conversation is worse than a run
// that waits for a node which understands it. Evolution is additive only —
// new fields get defaults, nothing is removed or repurposed.
package execstate

import (
	"encoding/json"
	"errors"
	"fmt"

	"github.com/crewlet/crewlet/internal/agent/ledger"
	"github.com/crewlet/crewlet/internal/events/types"
	llm "github.com/crewlet/crewlet/internal/providers/llm"
)

// Version is the format this build writes.
//
// Bumped only for a change a previous build could MISREAD. An added field a
// reader can default is not a bump; a changed meaning for an existing field
// is.
const Version = 1

// State is a suspended Execute loop, whole.
//
// JSON tags are the on-the-wire names and are part of the contract: renaming a
// Go field is free, renaming a tag is a format change.
type State struct {
	Version int `json:"version"`

	// Messages is the conversation as it stood, ending with the assistant
	// turn whose run_sandbox tool call is still unanswered.
	Messages []llm.Message `json:"messages"`

	// PendingCallID and PendingCallName identify the dangling call the
	// resume answers.
	PendingCallID   string `json:"pending_tool_call_id"`
	PendingCallName string `json:"pending_tool_name"`

	// ActiveTools is the tool surface the suspended phase had, including
	// anything it activated mid-loop. Replayed rather than re-derived: a
	// resumed phase that rebuilt its surface from the plan would lose every
	// activation the pre-suspend rounds made.
	ActiveTools []string `json:"active_tool_names"`

	// LoadedSkills is the skill-guard state, replayed for the same reason.
	LoadedSkills []string `json:"loaded_skill_keys"`

	// Round is the turn iteration the suspend happened in, so the resumed
	// phase continues numbering rather than restarting at 1.
	Round int `json:"iteration"`

	// InputTokens and OutputTokens are what the pre-suspend rounds spent.
	// Carried so the resumed phase's record is the turn's total rather than
	// only its second half.
	InputTokens  int `json:"input_tokens"`
	OutputTokens int `json:"output_tokens"`

	// ToolExecutions is what the pre-suspend rounds actually ran. The
	// resumed phase's ledger entry is built from these plus its own, which
	// is what stops a resumed turn re-firing a delivery that already went.
	ToolExecutions []types.ToolExecution `json:"tool_executions"`

	// Iterations is the closed-round ledger of the suspended TURN. The
	// resume is the same turn, so without this it would forget every round
	// that closed before the suspend.
	Iterations []ledger.Iteration `json:"iteration_history"`

	// Task is the turn's task description, carried so the resumed turn
	// re-enters with the same brief rather than one rebuilt from a trigger
	// that may no longer be readable.
	Task string `json:"task_description"`
}

// ErrUnknownVersion reports a state this build cannot read.
var ErrUnknownVersion = errors.New("execstate: unknown version")

// ErrNoPendingCall reports a state with no dangling call to answer.
var ErrNoPendingCall = errors.New("execstate: no pending tool call")

// ErrDanglingCalls reports a conversation whose last assistant turn left more
// than one call unanswered.
var ErrDanglingCalls = errors.New("execstate: more than one unanswered tool call")

// Validate checks the d-402 invariants.
//
// Run on BOTH serialize and resume, because a state that violates one is
// corrupt whichever side produced it — and the two sides are different builds,
// so a writer's check is not a reader's guarantee.
//
// EXACTLY ONE DANGLING tool_use. Zero means there is nothing to resume into;
// two means the model will answer one and strand the other, and the stranded
// one is a sandbox box nothing will ever collect.
func (s State) Validate() error {
	if s.Version != Version {
		return fmt.Errorf("%w: %d (this build writes %d)", ErrUnknownVersion, s.Version, Version)
	}
	if s.PendingCallID == "" {
		return ErrNoPendingCall
	}
	dangling := s.dangling()
	switch len(dangling) {
	case 0:
		return fmt.Errorf("%w: the conversation answers every call it made", ErrNoPendingCall)
	case 1:
		if dangling[0] != s.PendingCallID {
			return fmt.Errorf("%w: the unanswered call is %q but the state names %q",
				ErrNoPendingCall, dangling[0], s.PendingCallID)
		}
	default:
		return fmt.Errorf("%w: %v", ErrDanglingCalls, dangling)
	}
	return nil
}

// dangling is every tool-call id the conversation never answered.
func (s State) dangling() []string {
	answered := map[string]bool{}
	for _, msg := range s.Messages {
		if msg.ToolCallID != "" {
			answered[msg.ToolCallID] = true
		}
	}
	var open []string
	for _, msg := range s.Messages {
		for _, call := range msg.ToolCalls {
			if !answered[call.ID] {
				open = append(open, call.ID)
			}
		}
	}
	return open
}

// Encode serializes the state for the pending-run row, validating first.
//
// A refusal here is better than a row that cannot be resumed: the suspending
// turn still holds the box and can fail loudly, where a resume finding a
// corrupt row has already lost the conversation.
func Encode(s State) (map[string]any, error) {
	s.Version = Version
	if err := s.Validate(); err != nil {
		return nil, err
	}
	blob, err := json.Marshal(s)
	if err != nil {
		return nil, fmt.Errorf("execstate: encode: %w", err)
	}
	var out map[string]any
	if err := json.Unmarshal(blob, &out); err != nil {
		return nil, fmt.Errorf("execstate: encode: %w", err)
	}
	return out, nil
}

// Decode reads a state back out of a pending-run row.
//
// An empty blob is (State{}, false, nil): a run with no suspended conversation
// is an ordinary condition — a crash between launching the job and persisting
// the suspend — and the caller settles it rather than treating it as a broken
// store.
func Decode(blob map[string]any) (State, bool, error) {
	if len(blob) == 0 {
		return State{}, false, nil
	}
	raw, err := json.Marshal(blob)
	if err != nil {
		return State{}, false, fmt.Errorf("execstate: decode: %w", err)
	}
	var s State
	if err := json.Unmarshal(raw, &s); err != nil {
		return State{}, false, fmt.Errorf("execstate: decode: %w", err)
	}
	if err := s.Validate(); err != nil {
		return State{}, false, err
	}
	return s, true, nil
}

// Answer appends the tool result that answers the pending call, returning the
// conversation a resumed loop starts from.
//
// The result goes in as a tool message against the pending call's id, which is
// what makes the resumed loop see a completed exchange rather than a dangling
// request the provider would reject.
func (s State) Answer(content string) []llm.Message {
	out := make([]llm.Message, 0, len(s.Messages)+1)
	out = append(out, s.Messages...)
	return append(out, llm.Message{
		Role:       "tool",
		Content:    content,
		ToolCallID: s.PendingCallID,
		Name:       s.PendingCallName,
	})
}
