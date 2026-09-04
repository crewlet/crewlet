package runner

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/agent/ledger"
	"github.com/crewlet/crewlet/internal/agent/structured"
	"github.com/crewlet/crewlet/internal/agent/turn"
)

// deliverySurface is a plain one-write-tool surface, which the citation check
// judges against.
func deliverySurface() turn.Surface {
	return turn.Surface{
		Catalogue:    []string{"slack_post", "slack_history", "lookup_colleague"},
		Deliverables: []string{"slack_post"},
		KnownReads:   []string{"slack_history"},
	}
}

// workTool builds a submit_work tool over a scripted record of the turn: what
// was called, and who is waiting. Both are facts the model does not control,
// which is the whole point — the account a phase gives of itself is exactly
// what a phase can get wrong.
func workTool(reply turn.Reply, calls ...ledger.Call) *structured.Tool[workPayload] {
	return structured.New(SubmitWorkTool, submitWorkDescription, workSchema,
		decodeWork(reply,
			func() []ledger.Call { return calls },
			func() turn.Surface { return deliverySurface() }))
}

func args(t *testing.T, blob string) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal([]byte(blob), &m); err != nil {
		t.Fatalf("test fixture is not JSON: %v", err)
	}
	return m
}

func TestASubmissionCapturesThePhasesAnswer(t *testing.T) {
	t.Parallel()
	// The tool does not DO anything: the phase's answer IS the arguments it
	// was called with, so the loop's ordinary tool machinery carries it out
	// and nothing needs a side channel.
	tool := workTool(turn.ReplyTool, ledger.Call{Name: "slack_post"})
	if _, called := tool.Value(); called {
		t.Fatal("a fresh tool reports a submission")
	}
	res, err := tool.Call(context.Background(), args(t, `{
		"outcome": "delivered",
		"summary": "posted the weekly summary",
		"deliveries": ["slack_post"]
	}`))
	if err != nil {
		t.Fatalf("Call: %v", err)
	}
	if res.Failed {
		t.Fatalf("a valid submission failed: %s", res.Output)
	}
	got, called := tool.Value()
	if !called {
		t.Fatal("a valid submission was not captured")
	}
	if got.Outcome != "delivered" || len(got.Deliveries) != 1 {
		t.Errorf("captured %+v", got)
	}
}

func TestAnInvalidSubmissionGoesBackToTheModel(t *testing.T) {
	t.Parallel()
	// It is the one tool failure a model can reliably fix, and refusing the
	// turn over a malformed submission throws away everything the phase
	// already did.
	tool := workTool(turn.ReplyNone)
	res, err := tool.Call(context.Background(), args(t, `{"outcome":"wat","summary":"s"}`))
	if err != nil {
		t.Fatalf("Call returned a Go error: %v", err)
	}
	if !res.Failed {
		t.Fatal("an invalid outcome was accepted")
	}
	if !strings.Contains(res.Output, "delivered, no_action or blocked") {
		t.Errorf("the failure does not say what is allowed: %s", res.Output)
	}
	if _, called := tool.Value(); called {
		t.Error("an invalid submission was captured anyway")
	}
}

func TestTheLastSubmissionWins(t *testing.T) {
	t.Parallel()
	// A model that submits twice has corrected itself. Rejecting the second
	// leaves the engine acting on the draft the model just replaced.
	tool := workTool(turn.ReplyNone)
	ctx := context.Background()
	if _, err := tool.Call(ctx, args(t, `{"outcome":"blocked","summary":"a","evidence":"e"}`)); err != nil {
		t.Fatalf("Call: %v", err)
	}
	res, err := tool.Call(ctx, args(t, `{"outcome":"blocked","summary":"b","evidence":"e"}`))
	if err != nil {
		t.Fatalf("Call: %v", err)
	}
	if res.Failed {
		t.Fatalf("the second submission was refused: %s", res.Output)
	}
	got, _ := tool.Value()
	if got.Summary != "b" {
		t.Errorf("captured %q, want the correction", got.Summary)
	}
}

func TestAnAbsentOutcomeTakesTheCommonCase(t *testing.T) {
	t.Parallel()
	// `delivered` is what every other field is written for, and `done` is
	// the unremarkable review. Refusing the most predictable omission would
	// throw away a round of real work — and the engine checks the delivery
	// claim against the record either way.
	decode := decodeWork(turn.ReplyTool,
		func() []ledger.Call { return []ledger.Call{{Name: "slack_post"}} },
		deliverySurface)
	w, err := decode(args(t, `{"summary":"posted it","deliveries":["slack_post"]}`))
	if err != nil {
		t.Fatalf("decodeWork: %v", err)
	}
	if turn.Outcome(w.Outcome) != turn.OutcomeDelivered {
		t.Errorf("outcome = %q, want delivered", w.Outcome)
	}
	r, err := decodeReview(args(t, `{"final_artifact":"done here"}`))
	if err != nil {
		t.Fatalf("decodeReview: %v", err)
	}
	if r.Decision != "done" {
		t.Errorf("decision = %q, want done", r.Decision)
	}
}

// A submission with no summary tells the reviewer and the next turn nothing at
// all, and it is the one field every outcome needs.
func TestASubmissionWithNoSummaryIsRefused(t *testing.T) {
	t.Parallel()
	tool := workTool(turn.ReplyNone)
	for _, blob := range []string{`{"outcome":"no_action"}`, `{"outcome":"no_action","summary":"  "}`} {
		res, _ := tool.Call(context.Background(), args(t, blob))
		if !res.Failed {
			t.Errorf("%s was accepted", blob)
		}
	}
}

// SILENCE IS NOT A DECLINE. The requester cannot tell it from a message that
// was lost, and the engine knows one arrived — so this is refused where the
// model can still act on it rather than corrected a phase later.
func TestNoActionIsRefusedOnAnAwaitedTurn(t *testing.T) {
	t.Parallel()
	for _, reply := range []turn.Reply{turn.ReplyTool, turn.ReplyEngine} {
		tool := workTool(reply)
		res, _ := tool.Call(context.Background(),
			args(t, `{"outcome":"no_action","summary":"not for me"}`))
		if !res.Failed {
			t.Errorf("%s: no_action was accepted on a turn somebody asked for", reply)
		}
		if !strings.Contains(res.Output, "even to decline") {
			t.Errorf("%s: the refusal does not say what to do instead: %s", reply, res.Output)
		}
	}
	// The counterfactual: nobody asking makes it the right answer, and
	// without this the assertion above passes for a decoder that refuses
	// no_action outright.
	tool := workTool(turn.ReplyNone)
	if res, _ := tool.Call(context.Background(),
		args(t, `{"outcome":"no_action","summary":"a broadcast"}`)); res.Failed {
		t.Errorf("no_action was refused on an unaddressed turn: %s", res.Output)
	}
}

// "Blocked" with no account of what was tried is a round the reviewer can only
// send back blind.
func TestBlockedNeedsEvidence(t *testing.T) {
	t.Parallel()
	tool := workTool(turn.ReplyNone)
	res, _ := tool.Call(context.Background(), args(t, `{"outcome":"blocked","summary":"stuck"}`))
	if !res.Failed {
		t.Fatal("blocked was accepted with no evidence")
	}
	if !strings.Contains(res.Output, "what did you try") {
		t.Errorf("the refusal does not say what is missing: %s", res.Output)
	}
	if res, _ := tool.Call(context.Background(),
		args(t, `{"outcome":"blocked","summary":"stuck","evidence":"the channel 404s"}`)); res.Failed {
		t.Errorf("blocked with evidence was refused: %s", res.Output)
	}
}

// CHECKED INSIDE THE LOOP, where a wrong claim costs one bounced tool call the
// model can fix — instead of a whole review round or a silently accepted
// no-op.
func TestADeliveryClaimMustNameACallThatHappened(t *testing.T) {
	t.Parallel()
	called := []ledger.Call{{Name: "slack_post"}, {Name: "slack_history"}}
	decode := decodeWork(turn.ReplyTool, func() []ledger.Call { return called }, deliverySurface)

	if _, err := decode(args(t, `{"outcome":"delivered","summary":"s","deliveries":["slack_post"]}`)); err != nil {
		t.Errorf("a real delivery was refused: %v", err)
	}
	// A name the model MEANT to call. The refusal lists what is citable,
	// because a bare "no" sends it round the same loop.
	err := mustErr(t, decode, `{"outcome":"delivered","summary":"s","deliveries":["slack_postMessage"]}`)
	if !strings.Contains(err.Error(), "slack_post") {
		t.Errorf("the refusal does not list what is citable: %v", err)
	}
	// A read is not a delivery, however successfully it ran.
	mustErr(t, decode, `{"outcome":"delivered","summary":"s","deliveries":["slack_history"]}`)
	// And a failed call did not deliver.
	failed := decodeWork(turn.ReplyTool,
		func() []ledger.Call { return []ledger.Call{{Name: "slack_post", Failed: true}} },
		deliverySurface)
	mustErr(t, failed, `{"outcome":"delivered","summary":"s","deliveries":["slack_post"]}`)
}

// Nothing citable at all is a different message: the model has not called an
// outward tool yet, and telling it to pick from an empty list is useless.
func TestNothingDeliveredYetSaysSoRatherThanListingNothing(t *testing.T) {
	t.Parallel()
	decode := decodeWork(turn.ReplyTool,
		func() []ledger.Call { return []ledger.Call{{Name: "slack_history"}} }, deliverySurface)
	err := mustErr(t, decode, `{"outcome":"delivered","summary":"s","deliveries":["slack_post"]}`)
	if !strings.Contains(err.Error(), "nothing has been delivered yet") {
		t.Errorf("err = %v", err)
	}
	if !strings.Contains(err.Error(), "list_mcp_server_tools") {
		t.Errorf("the refusal does not say how to find the tool: %v", err)
	}
}

// A colleague's ask is answered by the engine on the channel it opened, and an
// unaddressed turn owes nobody a posted answer — so demanding a citation in
// either case would loop a turn that did exactly the right thing.
func TestOnlyAToolAwaitedTurnMustCiteACall(t *testing.T) {
	t.Parallel()
	for _, reply := range []turn.Reply{turn.ReplyNone, turn.ReplyEngine} {
		decode := decodeWork(reply, func() []ledger.Call { return nil }, deliverySurface)
		if _, err := decode(args(t, `{"outcome":"delivered","summary":"answered in prose"}`)); err != nil {
			t.Errorf("%s: a delivery with no tool call was refused: %v", reply, err)
		}
	}
}

func mustErr(t *testing.T, decode func(map[string]any) (workPayload, error), blob string) error {
	t.Helper()
	_, err := decode(args(t, blob))
	if err == nil {
		t.Fatalf("%s was accepted", blob)
	}
	return err
}

func TestALoopBackNeedsSomethingToDoDifferently(t *testing.T) {
	t.Parallel()
	// A self_iterate with no correction sends the next round to do exactly
	// what the last one did. The stall guard catches it eventually — but
	// only after spending the rounds.
	if _, err := decodeReview(args(t, `{"decision":"self_iterate"}`)); err == nil {
		t.Error("a correction-free loop-back was accepted")
	}
	if _, err := decodeReview(args(t, `{"decision":"self_iterate","notes":"   "}`)); err == nil {
		t.Error("whitespace counted as a correction")
	}
	if _, err := decodeReview(args(t, `{"decision":"self_iterate","notes":"repost with the fixed link"}`)); err != nil {
		t.Errorf("a real correction was refused: %v", err)
	}
	// The counterfactual: done and failed need no notes.
	for _, d := range []string{"done", "failed"} {
		if _, err := decodeReview(args(t, `{"decision":"`+d+`"}`)); err != nil {
			t.Errorf("%s was refused: %v", d, err)
		}
	}
}

func TestASchemaShapedSubmissionDecodesIntoItsStruct(t *testing.T) {
	t.Parallel()
	// The schemas are hand-written so their DESCRIPTIONS can be real prose,
	// which means the tags and the schema can drift. This is what says they
	// have not: every property the schema publishes is a field the struct
	// accepts.
	decodeAWork := decodeWork(turn.ReplyNone, func() []ledger.Call { return nil }, deliverySurface)
	for name, pair := range map[string]struct {
		schema map[string]any
		decode func(map[string]any) error
	}{
		"work":   {workSchema, func(m map[string]any) error { _, err := decodeAWork(m); return err }},
		"review": {reviewSchema, func(m map[string]any) error { _, err := decodeReview(m); return err }},
	} {
		props, ok := pair.schema["properties"].(map[string]any)
		if !ok {
			t.Fatalf("%s: schema has no properties", name)
		}
		payload := map[string]any{}
		for key, spec := range props {
			payload[key] = sampleFor(t, name, key, spec)
		}
		if err := pair.decode(payload); err != nil {
			t.Errorf("%s: a submission shaped by the schema did not decode: %v", name, err)
		}
	}
}

// sampleFor builds a value matching a schema property, so the test above
// exercises the real mapping rather than a hand-written fixture that could
// drift alongside the schema it is meant to check.
func sampleFor(t *testing.T, phase, key string, spec any) any {
	t.Helper()
	obj, ok := spec.(map[string]any)
	if !ok {
		t.Fatalf("%s.%s: property is not an object", phase, key)
	}
	if enum, ok := obj["enum"].([]any); ok && len(enum) > 0 {
		// The LAST enum member for the work schema and the first for the
		// review's: both are the value with no extra requirement attached.
		// `delivered` needs a citation and `self_iterate` needs notes, so
		// picking either would fail for the right reason and hide a real
		// drift.
		if phase == "work" {
			return enum[len(enum)-1]
		}
		return enum[0]
	}
	switch obj["type"] {
	case "string":
		return "x"
	case "array":
		return []any{"x"}
	case "object":
		return map[string]any{}
	default:
		t.Fatalf("%s.%s: unhandled schema type %v", phase, key, obj["type"])
		return nil
	}
}

func TestArgumentsThatCannotBeReEncodedFailTheSubmissionNotTheTurn(t *testing.T) {
	t.Parallel()
	// A model cannot produce this through the wire, but a builtin caller
	// could. It must come back as a failed tool result the phase can retry,
	// not as a Go error that ends the turn.
	tool := workTool(turn.ReplyNone)
	res, err := tool.Call(context.Background(), map[string]any{"summary": make(chan int)})
	if err != nil {
		t.Fatalf("Call returned a Go error: %v", err)
	}
	if !res.Failed {
		t.Fatal("an unencodable argument was accepted")
	}
	// The REASON matters. Swallowing the encode error leaves the payload
	// zero-valued, which then trips the missing-summary rule — so the
	// submission still fails, for a reason that has nothing to do with what
	// went wrong, and the model is told to add a summary it already wrote.
	// Found by mutation: asserting only that it failed passed either way.
	if !strings.Contains(res.Output, "re-encoded") {
		t.Errorf("the failure blames the wrong thing: %s", res.Output)
	}
}

func TestATypeMismatchIsReportedAsOne(t *testing.T) {
	t.Parallel()
	// A model that sends a bare string where the schema says array. The
	// reason has to be the mismatch: swallowing it leaves the payload part
	// filled, which trips a content rule instead, and the model is told to
	// fix something it got right.
	//
	// Found by mutation — asserting only that it failed passed either way.
	tool := workTool(turn.ReplyNone)
	res, err := tool.Call(context.Background(),
		args(t, `{"outcome":"delivered","summary":"s","deliveries":"slack_post"}`))
	if err != nil {
		t.Fatalf("Call returned a Go error: %v", err)
	}
	if !res.Failed {
		t.Fatal("a type mismatch was accepted")
	}
	if !strings.Contains(res.Output, "do not match the schema") {
		t.Errorf("the failure blames the wrong thing: %s", res.Output)
	}
}

func TestALargeIDInASubmissionSurvivesTheStructRoundTrip(t *testing.T) {
	t.Parallel()
	// The submission's own arguments go through a marshal/unmarshal to
	// reach the struct. json.Number has to survive it, or the precision the
	// provider layer just protected is lost one layer higher.
	var m map[string]any
	dec := json.NewDecoder(strings.NewReader(
		`{"outcome":"blocked","summary":"issue 1234567890123456789","evidence":"e"}`))
	dec.UseNumber()
	if err := dec.Decode(&m); err != nil {
		t.Fatalf("decode: %v", err)
	}
	decode := decodeWork(turn.ReplyNone, func() []ledger.Call { return nil }, deliverySurface)
	w, err := decode(m)
	if err != nil {
		t.Fatalf("decodeWork: %v", err)
	}
	if !strings.Contains(w.Summary, "1234567890123456789") {
		t.Errorf("summary = %q", w.Summary)
	}
}
