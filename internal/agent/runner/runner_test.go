package runner

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/agent/turn"
)

func planTool() *submitted[planPayload] {
	return &submitted[planPayload]{
		name: SubmitPlanTool, schema: planSchema, decode: decodePlan,
	}
}

func reviewTool() *submitted[reviewPayload] {
	return &submitted[reviewPayload]{
		name: SubmitReviewTool, schema: reviewSchema, decode: decodeReview,
	}
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
	tool := planTool()
	if _, called := tool.Value(); called {
		t.Fatal("a fresh tool reports a submission")
	}
	res, err := tool.Call(context.Background(), args(t, `{
		"decision": "plan",
		"reasoning": "post the summary",
		"tools_needed": ["slack_post"],
		"steps": [{"intent": "post it", "approach": "say hello"}]
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
	if got.Decision != "plan" || len(got.ToolsNeeded) != 1 {
		t.Errorf("captured %+v", got)
	}
}

func TestAnInvalidSubmissionGoesBackToTheModel(t *testing.T) {
	t.Parallel()
	// It is the one tool failure a model can reliably fix, and refusing the
	// turn over a malformed submission throws away everything the phase
	// already did.
	tool := planTool()
	res, err := tool.Call(context.Background(), args(t, `{"decision": "wat"}`))
	if err != nil {
		t.Fatalf("Call returned a Go error: %v", err)
	}
	if !res.Failed {
		t.Fatal("an invalid decision was accepted")
	}
	if !strings.Contains(res.Output, "plan, direct or skip") {
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
	tool := planTool()
	ctx := context.Background()
	if _, err := tool.Call(ctx, args(t, `{"decision":"plan","tools_needed":["a"]}`)); err != nil {
		t.Fatalf("Call: %v", err)
	}
	res, err := tool.Call(ctx, args(t, `{"decision":"plan","tools_needed":["b"]}`))
	if err != nil {
		t.Fatalf("Call: %v", err)
	}
	if res.Failed {
		t.Fatalf("the second submission was refused: %s", res.Output)
	}
	got, _ := tool.Value()
	if len(got.ToolsNeeded) != 1 || got.ToolsNeeded[0] != "b" {
		t.Errorf("captured %v, want the correction", got.ToolsNeeded)
	}
}

func TestAnAbsentDecisionTakesTheCommonCase(t *testing.T) {
	t.Parallel()
	// `plan` is what every other field is written for, and `done` is the
	// unremarkable review. Failing on the most predictable omission would
	// reject an otherwise complete answer.
	p, err := decodePlan(args(t, `{"tools_needed":["slack_post"]}`))
	if err != nil {
		t.Fatalf("decodePlan: %v", err)
	}
	if turn.PlanDecision(p.Decision) != turn.PlanRun {
		t.Errorf("decision = %q, want plan", p.Decision)
	}
	r, err := decodeReview(args(t, `{"final_artifact":"done here"}`))
	if err != nil {
		t.Fatalf("decodeReview: %v", err)
	}
	if r.Decision != "done" {
		t.Errorf("decision = %q, want done", r.Decision)
	}
}

func TestAPlanThatDecidedNothingIsRefused(t *testing.T) {
	t.Parallel()
	// Saying so is what makes the model try again INSIDE the phase. Letting
	// it through means Execute receives an empty plan and improvises
	// against the full surface — which is the `direct` path, arrived at by
	// accident.
	if _, err := decodePlan(args(t, `{"decision":"plan"}`)); err == nil {
		t.Error("a plan with no steps and no tools was accepted")
	}
	// A skip legitimately has neither.
	if _, err := decodePlan(args(t, `{"decision":"skip","reasoning":"not for me"}`)); err != nil {
		t.Errorf("a skip was refused: %v", err)
	}
	// And either half alone is enough.
	for _, blob := range []string{
		`{"decision":"plan","tools_needed":["a"]}`,
		`{"decision":"plan","steps":[{"intent":"think"}]}`,
	} {
		if _, err := decodePlan(args(t, blob)); err != nil {
			t.Errorf("%s was refused: %v", blob, err)
		}
	}
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

func TestThePlanSummaryCarriesEachStepsApproach(t *testing.T) {
	t.Parallel()
	// The planner may have pre-composed the exact content Execute should
	// produce, and Execute cannot see what Plan saw. Dropping the approach
	// makes Execute re-derive data the planner already gathered, or invent
	// it.
	p, err := decodePlan(args(t, `{
		"decision":"plan","tools_needed":["slack_post"],
		"steps":[
			{"intent":"post the summary","approach":"Weekly update: three PRs merged."},
			{"intent":"link the doc"}
		]}`))
	if err != nil {
		t.Fatalf("decodePlan: %v", err)
	}
	got := p.Summary()
	for _, want := range []string{"1. post the summary", "Weekly update: three PRs merged.", "2. link the doc"} {
		if !strings.Contains(got, want) {
			t.Errorf("summary is missing %q:\n%s", want, got)
		}
	}
}

func TestASkipSummaryCarriesItsReasoningForTheRecord(t *testing.T) {
	t.Parallel()
	// A skip short-circuits before Execute and Review, so this string is
	// the only diagnostic the turn leaves behind. Without the reasoning the
	// record is a tombstone.
	p, _ := decodePlan(args(t, `{"decision":"skip","reasoning":"addressed to the CTO"}`))
	got := p.Summary()
	if !strings.Contains(got, "addressed to the CTO") {
		t.Errorf("summary = %q, want the reasoning", got)
	}
	if !strings.Contains(got, "skip") {
		t.Errorf("summary = %q, want it marked as a skip", got)
	}
	// And a skip with no reasoning still says what happened.
	bare, _ := decodePlan(args(t, `{"decision":"skip"}`))
	if bare.Summary() == "" {
		t.Error("a bare skip summarised to nothing")
	}
}

func TestADirectPlanWithNoStepsStillSummarisesToSomething(t *testing.T) {
	t.Parallel()
	// Execute reads this. An empty summary tells it nothing at all, which
	// is worse than telling it the planner left it to improvise.
	p, _ := decodePlan(args(t, `{"decision":"direct","tools_needed":["slack_post"]}`))
	if p.Summary() == "" {
		t.Error("a direct plan with no steps summarised to nothing")
	}
	withReasoning, _ := decodePlan(args(t, `{"decision":"direct","tools_needed":["a"],"reasoning":"just post it"}`))
	if !strings.Contains(withReasoning.Summary(), "just post it") {
		t.Errorf("summary = %q, want the planner's thinking", withReasoning.Summary())
	}
}

func TestASchemaShapedSubmissionDecodesIntoItsStruct(t *testing.T) {
	t.Parallel()
	// The schemas are hand-written so their DESCRIPTIONS can be real prose,
	// which means the tags and the schema can drift. This is what says they
	// have not: every property the schema publishes is a field the struct
	// accepts.
	for name, pair := range map[string]struct {
		schema map[string]any
		decode func(map[string]any) error
	}{
		"plan":   {planSchema, func(m map[string]any) error { _, err := decodePlan(m); return err }},
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
		// The FIRST enum member, which for both decisions is the one with
		// no extra requirements — self_iterate needs notes, and picking it
		// here would fail for the right reason and hide a real drift.
		return enum[0]
	}
	switch obj["type"] {
	case "string":
		return "x"
	case "array":
		items, _ := obj["items"].(map[string]any)
		if items != nil && items["type"] == "object" {
			return []any{map[string]any{"intent": "x"}}
		}
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
	tool := planTool()
	res, err := tool.Call(context.Background(), map[string]any{"decision": make(chan int)})
	if err != nil {
		t.Fatalf("Call returned a Go error: %v", err)
	}
	if !res.Failed {
		t.Fatal("an unencodable argument was accepted")
	}
	// The REASON matters. Swallowing the encode error leaves the payload
	// zero-valued, which then trips the empty-plan rule — so the
	// submission still fails, for a reason that has nothing to do with
	// what went wrong, and the model is told to add steps it already
	// added. Found by mutation: asserting only that it failed passed
	// either way.
	if !strings.Contains(res.Output, "re-encoded") {
		t.Errorf("the failure blames the wrong thing: %s", res.Output)
	}
}

func TestATypeMismatchIsReportedAsOne(t *testing.T) {
	t.Parallel()
	// A model that sends a bare string where the schema says array. The
	// reason has to be the mismatch: swallowing it leaves the payload part
	// filled, which trips the empty-plan rule instead, and the model is
	// told to add tools it already named.
	//
	// Found by mutation — asserting only that it failed passed either way.
	tool := planTool()
	res, err := tool.Call(context.Background(), args(t, `{"decision":"plan","tools_needed":"slack_post"}`))
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

func TestALargeIDInAPlanSurvivesTheStructRoundTrip(t *testing.T) {
	t.Parallel()
	// The plan's own arguments go through a marshal/unmarshal to reach the
	// struct. json.Number has to survive it, or the precision the provider
	// layer just protected is lost one layer higher.
	var m map[string]any
	dec := json.NewDecoder(strings.NewReader(
		`{"decision":"plan","tools_needed":["a"],"steps":[{"intent":"issue 1234567890123456789"}]}`))
	dec.UseNumber()
	if err := dec.Decode(&m); err != nil {
		t.Fatalf("decode: %v", err)
	}
	p, err := decodePlan(m)
	if err != nil {
		t.Fatalf("decodePlan: %v", err)
	}
	if !strings.Contains(p.Summary(), "1234567890123456789") {
		t.Errorf("summary = %q", p.Summary())
	}
}
