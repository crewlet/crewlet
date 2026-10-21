package execstate_test

import (
	"encoding/json"
	"errors"
	"reflect"
	"strconv"
	"testing"

	"github.com/crewlet/crewlet/internal/agent/execstate"
	"github.com/crewlet/crewlet/internal/agent/ledger"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/provenance"
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
		ToolExecutions: []types.ToolExecution{
			{"name": "read_file", "success": true, "round": 1, "call_id": "call-0"},
		},
		RoundsUsed: 2,
		RoundNarration: []types.RoundNarration{
			{"round": 1, "content": "Reading it first."},
			{"round": 2, "content": "Starting a coding run."},
		},
		Invocations: []types.PhaseInvocation{{
			DurationMS: 5_000, ProviderKey: "claude", Model: "m", Outcome: "stopped",
			RequestsObserved: true, InputTokens: 1200, OutputTokens: 340, ToolCalls: 2,
		}},
		Iterations: []ledger.Iteration{{Iteration: 1, Intent: "fix it"}},
		Task:       "fix the flake",
		Ask:        "fix the flake on the payment client",
		AskProvenance: &provenance.Label{Source: provenance.SourceChatThread,
			Trust: provenance.TrustExternal, Authors: []provenance.Author{{Kind: iam.ActorHuman}}},
		Uncharged: &execstate.Uncharged{Turns: 1, Input: 900, Output: 120, Workers: 1},
		SoFar:     &execstate.TurnSoFar{DurationMS: 42_000, Reviews: 2, SentBack: 1, Unaccepted: 1},
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
	// THE CALL'S OWN ID, which pairs the row with the call the conversation
	// made: two calls of one response are otherwise the same row twice.
	if got.ToolExecutions[0]["call_id"] != "call-0" {
		t.Fatalf("the execution's call id did not survive: %+v", got.ToolExecutions[0])
	}
	// AND THE CLI RUNS, the unit the engine started and stopped itself.
	if len(got.Invocations) != 1 || got.Invocations[0] != want.Invocations[0] {
		t.Fatalf("the phase's CLI runs did not survive: %+v", got.Invocations)
	}
	// THE ROUNDS THEMSELVES. Nothing else holds them — a suspending phase
	// publishes no completed event and its progress frames are stream-only —
	// so a row that loses these loses the pre-suspend half of the phase from
	// the store for good, and the resumed half renumbers from 1.
	if got.RoundsUsed != 2 || len(got.RoundNarration) != 2 {
		t.Fatalf("the pre-suspend rounds were lost: %+v", got)
	}
	if got.RoundNarration[0]["content"] != "Reading it first." {
		t.Fatalf("round 1's narration did not survive: %+v", got.RoundNarration)
	}
	if got.Task != "fix the flake" {
		t.Fatalf("task = %q", got.Task)
	}
	// WHAT IT WAS ASKED, AND WHO ASKED IT IN WHAT WORDS: the segment that
	// records the ask reads no trigger, so a label lost here is a stranger's
	// words recorded as nobody's.
	if got.Ask != want.Ask || !reflect.DeepEqual(got.AskProvenance, want.AskProvenance) {
		t.Fatalf("the ask came back as %q under %+v", got.Ask, got.AskProvenance)
	}
	// WHAT NO ITEM HAS BEEN CHARGED FOR YET. The segment that finishes the
	// turn pays it, and it is often another process on another node, so a
	// row that loses it loses the first half of the turn from its task.
	if got.Uncharged == nil || *got.Uncharged != *want.Uncharged {
		t.Fatalf("the uncharged spend did not survive: %+v", got.Uncharged)
	}
	// AND WHAT THE TURN MEASURED SO FAR, which the record that ends it states
	// whole: lost, the turn's earlier spans and reviews count nowhere.
	if got.SoFar == nil || *got.SoFar != *want.SoFar {
		t.Fatalf("the turn's measurement so far did not survive: %+v", got.SoFar)
	}
}

// withVersion rewrites the version an encoded blob claims, the one field a
// test of the version guard has to forge.
func withVersion(t *testing.T, blob json.RawMessage, version int) json.RawMessage {
	t.Helper()
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(blob, &fields); err != nil {
		t.Fatalf("the blob is not JSON: %v", err)
	}
	fields["version"] = json.RawMessage(strconv.Itoa(version))
	out, err := json.Marshal(fields)
	if err != nil {
		t.Fatalf("re-encode: %v", err)
	}
	return out
}

// A NUMBER COMES BACK AS THE DIGITS IT WENT IN AS. A model passes ids as
// tool arguments — a Slack timestamp, a database key, a nineteen-digit
// snowflake — and the resumed loop replays that call to the provider and
// reads it back into its own ledger. Read as a float64 the snowflake below
// becomes ...800, which is an argument the model never wrote, a replayed turn
// the provider sees edited, and an id that names something else.
func TestNumbersSurviveASuspensionExactly(t *testing.T) {
	t.Parallel()
	const snowflake = "1234567890123456789"
	state := suspended()
	state.Messages[2].ToolCalls[0].Arguments = map[string]any{
		"brief": "fix it",
		"row":   json.Number(snowflake),
		"ratio": json.Number("0.1000000000000000055511151231257827"),
	}

	blob, err := execstate.Encode(state)
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}
	got, ok, err := execstate.Decode(blob)
	if err != nil || !ok {
		t.Fatalf("Decode = %v, %v", ok, err)
	}
	args := got.Messages[2].ToolCalls[0].Arguments
	if row, _ := args["row"].(json.Number); row.String() != snowflake {
		t.Errorf("row = %v (%T), want the exact %s", args["row"], args["row"], snowflake)
	}
	if ratio, _ := args["ratio"].(json.Number); ratio.String() != "0.1000000000000000055511151231257827" {
		t.Errorf("ratio = %v (%T), want its digits unchanged", args["ratio"], args["ratio"])
	}

	// And a second suspension of the resumed conversation writes the same
	// bytes: what a resume reads is exactly what the next suspend writes,
	// with no drift a chain of suspensions could compound.
	again, err := execstate.Encode(got)
	if err != nil {
		t.Fatalf("re-Encode: %v", err)
	}
	if string(again) != string(blob) {
		t.Errorf("a decoded state re-encodes differently:\n first %s\nsecond %s", blob, again)
	}
}

// A rolling upgrade means the node that resumes is routinely not the build
// that suspended. A half-understood conversation must not be acted on.
func TestAnUnknownVersionIsRefusedLoudlyRatherThanReadBestEffort(t *testing.T) {
	blob, err := execstate.Encode(suspended())
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}
	blob = withVersion(t, blob, execstate.Version+7)

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
	var stamped struct {
		Version int `json:"version"`
	}
	if err := json.Unmarshal(blob, &stamped); err != nil {
		t.Fatalf("the blob is not JSON: %v", err)
	}
	if stamped.Version != execstate.Version {
		t.Fatalf("version = %d, want %d", stamped.Version, execstate.Version)
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
	// A row written with no state at all carries a JSON null, which is what
	// a launching run's row holds.
	for _, empty := range []string{"null", "{}", " { } "} {
		if got, ok, err := execstate.Decode(json.RawMessage(empty)); err != nil || ok {
			t.Fatalf("Decode(%s) = %+v, %v, %v; want a clean miss", empty, got, ok, err)
		}
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

// A version this build does not know — a successor's reshape — is a
// REFUSAL, not a best-effort read: a reader that decoded a reshaped blob into
// its own fields would find the ones that moved empty and resume believing
// every round before the suspend had called nothing — so it would re-fire
// whatever they delivered.
//
// The retired numbers are among them: 1 and 2 named earlier shapes, neither in
// a release, and a blob carrying either is refused like any other version this
// build does not read rather than decoded as this one — a reader for a version
// no release wrote would be code for a build nobody runs.
func TestAVersionThisBuildDoesNotKnowIsRefused(t *testing.T) {
	t.Parallel()
	for _, version := range []int{execstate.Version + 1, 1, 2} {
		blob, err := execstate.Encode(suspended())
		if err != nil {
			t.Fatalf("Encode: %v", err)
		}
		blob = withVersion(t, blob, version)
		got, ok, err := execstate.Decode(blob)
		if !errors.Is(err, execstate.ErrUnknownVersion) {
			t.Fatalf("Decode(version %d) = %+v, %v, %v; want ErrUnknownVersion",
				version, got, ok, err)
		}
	}
}

// besideASibling is a parked phase whose last response made a call BESIDE its
// detached run: the sibling ran first — a detached call runs alone, after the
// others its response made — and was answered; the run is the one call left.
func besideASibling() execstate.State {
	state := suspended()
	state.Messages[2].ToolCalls = []llm.ToolCall{
		{ID: "call-0", Name: "read_file", Arguments: map[string]any{"path": "/a"}},
		{ID: "call-1", Name: "run_sandbox", Arguments: map[string]any{"brief": "fix it"}},
	}
	state.Messages = append(state.Messages,
		llm.Message{Role: llm.RoleTool, ToolCallID: "call-0", Name: "read_file", Content: "contents"})
	return state
}

// A ROUND THAT CALLED SOMETHING BESIDE ITS DETACHED RUN PARKS. Its sibling was
// answered before the run started, so the one call left unanswered is the run
// — and the resume's answer follows the sibling's, closing the response.
//
// Mutation: count every call of the last response as dangling, the shape a
// loop that parked before the round's other calls ran left, and this is
// refused as two unanswered calls.
func TestAParkedCallBesideAnAnsweredSiblingIsResumable(t *testing.T) {
	t.Parallel()
	state := besideASibling()
	blob, err := execstate.Encode(state)
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}
	got, ok, err := execstate.Decode(blob)
	if err != nil || !ok {
		t.Fatalf("Decode = %v, %v", ok, err)
	}
	answered := got
	answered.Messages = got.Answer("the run succeeded")
	last := answered.Messages[len(answered.Messages)-1]
	if last.ToolCallID != "call-1" || last.Name != "run_sandbox" {
		t.Fatalf("the resume's answer is %+v, want the parked run's", last)
	}
	if err := answered.Validate(); !errors.Is(err, execstate.ErrNoPendingCall) {
		t.Fatalf("Validate after Answer = %v; every call should read as answered", err)
	}

	// The counterfactual: the sibling NOT answered is two calls stranded,
	// one of them a box nothing would collect.
	stranded := besideASibling()
	stranded.Messages = stranded.Messages[:len(stranded.Messages)-1]
	if _, err := execstate.Encode(stranded); !errors.Is(err, execstate.ErrDanglingCalls) {
		t.Fatalf("Encode = %v, want ErrDanglingCalls for the unanswered sibling", err)
	}
}

// TWO CALLS SHARING AN ID make "answered" mean nothing: one answer reads as
// both, and a parked call answered by its namesake's result reads as closed.
func TestTwoCallsSharingAnIDAreRefused(t *testing.T) {
	t.Parallel()
	state := besideASibling()
	state.Messages[2].ToolCalls[0].ID = "call-1"
	state.Messages[3].ToolCallID = "call-1"
	if _, err := execstate.Encode(state); !errors.Is(err, execstate.ErrDuplicateCallID) {
		t.Fatalf("Encode = %v, want ErrDuplicateCallID", err)
	}
}

// THE PARKED CALL IS THE CONVERSATION'S LAST WORD. The resume appends its
// answer at the end, so a turn the model took after the parked response would
// put the answer after a reply that never saw it.
func TestAConversationThatGoesOnPastItsParkedCallIsRefused(t *testing.T) {
	t.Parallel()
	state := besideASibling()
	state.Messages = append(state.Messages,
		llm.Message{Role: llm.RoleAssistant, Content: "while that runs, a word"})
	if _, err := execstate.Encode(state); !errors.Is(err, execstate.ErrPastTheParkedCall) {
		t.Fatalf("Encode = %v, want ErrPastTheParkedCall", err)
	}
}

// THE PENDING CALL IS NAMED AS IT WAS MADE: an id that matches with a name
// that does not is a state written about some other call.
func TestThePendingNameMustBeTheOpenCalls(t *testing.T) {
	t.Parallel()
	state := suspended()
	state.PendingCallName = "read_file"
	if _, err := execstate.Encode(state); !errors.Is(err, execstate.ErrNoPendingCall) {
		t.Fatalf("Encode = %v, want ErrNoPendingCall", err)
	}
}
