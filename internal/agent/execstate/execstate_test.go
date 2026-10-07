package execstate_test

import (
	"bytes"
	"encoding/json"
	"errors"
	"strconv"
	"strings"
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
// the provider sees edited, and an id that names something else. The thinking
// rides along beside it, and comes back as it went in.
func TestNumbersSurviveASuspensionExactly(t *testing.T) {
	t.Parallel()
	const snowflake = "1234567890123456789"
	state := suspended()
	state.Messages[2].ThinkingBlocks = []llm.ThinkingBlock{{
		Type: "thinking", Thinking: "find the row <first> & then fix it",
	}}
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
	if blocks := got.Messages[2].ThinkingBlocks; len(blocks) != 1 || blocks[0] != state.Messages[2].ThinkingBlocks[0] {
		t.Errorf("thinking blocks = %+v, want them unchanged", blocks)
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

// THE VENDOR'S OWN BLOCKS COME BACK FROM A SUSPENSION AS THEY WENT IN, with
// the origin that says whose they are. A resumed executor replays them to the
// provider, and a model that preserves its thinking binds every block to the
// conversation before it — so a parked turn that came back with a block
// missing, reordered or re-spelled would be an edited conversation the vendor
// refuses on the first round after the resume.
//
// Held to the TOKENS: the row's encoder drops the whitespace between them and
// escapes <, > and & inside strings, exactly as the request encoder does on
// the way out, and neither changes a key, its order, a number's digits or a
// string. The tags are held too — they are a wire format the node that
// resumes the run reads — and so is the turn's binding, which says which tools
// its reasoning was written under.
func TestTheVendorsBlocksSurviveASuspension(t *testing.T) {
	t.Parallel()
	written := []string{
		`{"signature":"EqQBCgIYAhIM+/=","type":"thinking","thinking":"find <it> & fix it"}`,
		`{"type":"text","text":"  Starting.  ","citations":null}`,
		`{"type":"tool_use","id":"call-1","name":"run_sandbox","input":{"brief": "fix it", "row": 1234567890123456789, "z": 1.0}}`,
	}
	state := suspended()
	state.Messages[2].Origin = llm.Origin{Provider: "anthropic", Model: "claude-opus-5-5"}
	state.Messages[2].Binding = "9f2c"
	for _, b := range written {
		state.Messages[2].Raw = append(state.Messages[2].Raw, json.RawMessage(b))
	}

	blob, err := execstate.Encode(state)
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}
	for _, tag := range []string{
		`"Origin":{"Provider":"anthropic","Model":"claude-opus-5-5"}`, `"Raw":[`, `"Binding":"9f2c"`,
	} {
		if !strings.Contains(string(blob), tag) {
			t.Errorf("the row does not carry %s — a renamed tag is a parked turn the resume cannot read:\n%s", tag, blob)
		}
	}
	// A turn with no origin writes none: every message but the assistant's
	// is one, and an empty object on each is bytes for nothing.
	if strings.Count(string(blob), `"Origin"`) != 1 {
		t.Errorf("the row carries an origin on a message that has none:\n%s", blob)
	}

	got, ok, err := execstate.Decode(blob)
	if err != nil || !ok {
		t.Fatalf("Decode = %v, %v", ok, err)
	}
	turn := got.Messages[2]
	if turn.Origin != state.Messages[2].Origin {
		t.Errorf("origin = %+v, want %+v", turn.Origin, state.Messages[2].Origin)
	}
	// The binding comes back too: without it the resumed loop cannot tell
	// reasoning written under the tools it resumes with from reasoning a
	// re-rendered definition invalidated, and sheds all of it.
	if turn.Binding != state.Messages[2].Binding {
		t.Errorf("binding = %q, want %q", turn.Binding, state.Messages[2].Binding)
	}
	if strings.Count(string(blob), `"Binding"`) != 1 {
		t.Errorf("the row carries a binding on a message that has none:\n%s", blob)
	}
	if len(turn.Raw) != len(written) {
		t.Fatalf("raw = %d blocks, want the %d written", len(turn.Raw), len(written))
	}
	for i := range written {
		if got, want := tokens(t, turn.Raw[i]), tokens(t, []byte(written[i])); got != want {
			t.Errorf("block %d came back as\n  %s\nwant\n  %s", i, got, want)
		}
	}
	if again, err := execstate.Encode(got); err != nil || string(again) != string(blob) {
		t.Errorf("a decoded state re-encodes differently (%v):\n first %s\nsecond %s", err, blob, again)
	}
}

// tokens is a JSON value with the whitespace between its tokens dropped and
// <, > and & escaped — the form both the row's encoder and the request's give
// it, which changes no token.
func tokens(t *testing.T, raw []byte) string {
	t.Helper()
	var compact, escaped bytes.Buffer
	if err := json.Compact(&compact, raw); err != nil {
		t.Fatalf("%s is not JSON: %v", raw, err)
	}
	json.HTMLEscape(&escaped, compact.Bytes())
	return escaped.String()
}

// A V1 row's numbers are read the same exact way, since it is read for ever.
func TestAV1RowKeepsItsNumbersExact(t *testing.T) {
	t.Parallel()
	blob := strings.Replace(v1Blob, `{"brief": "fix it"}`, `{"brief": "fix it", "row": 1234567890123456789}`, 1)
	got, ok, err := execstate.Decode(json.RawMessage(blob))
	if err != nil || !ok {
		t.Fatalf("Decode of a v1 row = %v, %v", ok, err)
	}
	if row := got.Messages[2].ToolCalls[0].Arguments["row"]; row != json.Number("1234567890123456789") {
		t.Errorf("row = %v (%T), want the exact digits", row, row)
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
	// A row written with no state at all carries a JSON null, and every
	// build before this one wrote exactly that for a launching run.
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
	got, ok, err := execstate.Decode(json.RawMessage(v1Blob))
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
	blob = withVersion(t, blob, execstate.Version+1)
	got, ok, err := execstate.Decode(blob)
	if !errors.Is(err, execstate.ErrUnknownVersion) {
		t.Fatalf("Decode = %+v, %v, %v; want ErrUnknownVersion", got, ok, err)
	}
}
