package structured_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/agent/structured"
)

// answer is a stand-in for a phase's payload. Deliberately not one of the real
// ones: what is under test here is the SHAPE and its three rules, and a test
// built on the plan's schema would fail for a reason belonging to the planner.
type answer struct {
	Decision string `json:"decision"`
	Note     string `json:"note"`
	ID       int64  `json:"id"`
}

func decode(args map[string]any) (answer, error) {
	var a answer
	if err := structured.Remarshal(args, &a); err != nil {
		return a, err
	}
	if a.Decision != "yes" && a.Decision != "no" {
		return a, errDecision
	}
	return a, nil
}

var errDecision = errDecisionType{}

type errDecisionType struct{}

func (errDecisionType) Error() string { return "decision must be yes or no" }

func tool() *structured.Tool[answer] {
	return structured.New("submit_answer", "Submit the answer.", map[string]any{
		"type": "object",
		"properties": map[string]any{
			"decision": map[string]any{"type": "string"},
			"note":     map[string]any{"type": "string"},
			"id":       map[string]any{"type": "integer"},
		},
	}, decode)
}

func args(t *testing.T, blob string) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal([]byte(blob), &m); err != nil {
		t.Fatalf("test fixture is not JSON: %v", err)
	}
	return m
}

// The phase's answer IS the arguments it was called with, so the loop's
// ordinary tool machinery carries it out and nothing needs a side channel.
func TestASubmissionCapturesTheAnswer(t *testing.T) {
	t.Parallel()
	tl := tool()
	if _, called := tl.Value(); called {
		t.Fatal("a fresh tool reports a submission")
	}
	res, err := tl.Call(context.Background(), args(t, `{"decision":"yes","note":"shipped"}`))
	if err != nil {
		t.Fatalf("Call: %v", err)
	}
	if res.Failed {
		t.Fatalf("a valid submission failed: %s", res.Output)
	}
	got, called := tl.Value()
	if !called {
		t.Fatal("a valid submission was not captured")
	}
	if got.Decision != "yes" || got.Note != "shipped" {
		t.Errorf("captured %+v", got)
	}
}

// An absent submission is not a value. Every rescue path in the turn engine
// turns on telling "the phase decided this" from "nothing decided anything".
func TestAnAbsentSubmissionIsNotAValue(t *testing.T) {
	t.Parallel()
	got, called := tool().Value()
	if called {
		t.Fatalf("an untouched tool reports a submission: %+v", got)
	}
}

// A rejected submission goes back to the model as a FAILED TOOL RESULT, not
// as a Go error: it is the one tool failure a model reliably fixes, and
// ending the phase over one throws away everything it already did.
func TestAnInvalidSubmissionGoesBackToTheModel(t *testing.T) {
	t.Parallel()
	tl := tool()
	res, err := tl.Call(context.Background(), args(t, `{"decision":"maybe"}`))
	if err != nil {
		t.Fatalf("Call returned a Go error, which would end the phase: %v", err)
	}
	if !res.Failed {
		t.Fatal("an invalid submission was accepted")
	}
	// The decoder's own words reach the model, because the model is who has
	// to act on them.
	if !strings.Contains(res.Output, "yes or no") {
		t.Errorf("the failure does not carry the decoder's message: %s", res.Output)
	}
	if _, called := tl.Value(); called {
		t.Error("an invalid submission was captured anyway")
	}
}

// A model that submits twice has corrected itself. Rejecting the second leaves
// the engine acting on the draft the model just replaced.
func TestTheLastSubmissionWins(t *testing.T) {
	t.Parallel()
	tl := tool()
	ctx := context.Background()
	if _, err := tl.Call(ctx, args(t, `{"decision":"yes","note":"first"}`)); err != nil {
		t.Fatalf("Call: %v", err)
	}
	res, err := tl.Call(ctx, args(t, `{"decision":"no","note":"second"}`))
	if err != nil {
		t.Fatalf("Call: %v", err)
	}
	if res.Failed {
		t.Fatalf("the second submission was refused: %s", res.Output)
	}
	got, _ := tl.Value()
	if got.Note != "second" || got.Decision != "no" {
		t.Errorf("captured %+v, want the correction", got)
	}
}

// An invalid submission that arrives AFTER a valid one leaves the valid one
// standing: the model corrected itself badly, and the engine still has the
// answer it was given rather than nothing at all.
func TestARejectedCorrectionLeavesTheAnswerStanding(t *testing.T) {
	t.Parallel()
	tl := tool()
	ctx := context.Background()
	if _, err := tl.Call(ctx, args(t, `{"decision":"yes","note":"good"}`)); err != nil {
		t.Fatalf("Call: %v", err)
	}
	if _, err := tl.Call(ctx, args(t, `{"decision":"nonsense"}`)); err != nil {
		t.Fatalf("Call: %v", err)
	}
	got, called := tl.Value()
	if !called || got.Note != "good" {
		t.Errorf("captured %+v (called=%v), want the last VALID submission", got, called)
	}
}

// The tool reports itself to the model the way any other does.
func TestTheToolPresentsItsOwnSchema(t *testing.T) {
	t.Parallel()
	tl := tool()
	if tl.Name() != "submit_answer" {
		t.Errorf("Name = %q", tl.Name())
	}
	if tl.Description() == "" {
		t.Error("a tool with no description tells the model nothing about when to call it")
	}
	if tl.Parameters()["type"] != "object" {
		t.Errorf("Parameters = %v", tl.Parameters())
	}
}

// json.Number survives the round trip, so a large id in a submission's own
// arguments stays exact rather than arriving as a float that lost its tail.
func TestALargeIDSurvivesTheStructRoundTrip(t *testing.T) {
	t.Parallel()
	var got answer
	if err := structured.Remarshal(map[string]any{
		"decision": "yes",
		"id":       json.Number("9007199254740993"),
	}, &got); err != nil {
		t.Fatalf("Remarshal: %v", err)
	}
	if got.ID != 9007199254740993 {
		t.Errorf("id = %d, want the exact value", got.ID)
	}
}

// A type mismatch is reported as one, so the model is told what to change
// rather than being handed a zero value it cannot see is wrong.
func TestATypeMismatchIsReportedAsOne(t *testing.T) {
	t.Parallel()
	var got answer
	err := structured.Remarshal(map[string]any{"decision": 42}, &got)
	if err == nil {
		t.Fatal("a string field given a number was accepted")
	}
	if !strings.Contains(err.Error(), "do not match the schema") {
		t.Errorf("error = %v", err)
	}
}

// Arguments that cannot be encoded at all fail the SUBMISSION, not the turn.
func TestArgumentsThatCannotBeReEncodedFailTheSubmission(t *testing.T) {
	t.Parallel()
	res, err := tool().Call(context.Background(), map[string]any{"decision": make(chan int)})
	if err != nil {
		t.Fatalf("Call returned a Go error: %v", err)
	}
	if !res.Failed {
		t.Fatal("unencodable arguments were accepted")
	}
	if !strings.Contains(res.Output, "could not be re-encoded") {
		t.Errorf("output = %s", res.Output)
	}
}
