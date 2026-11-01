package structured_test

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/crewlet/crewlet/internal/agent/structured"
)

// answer is a stand-in for a phase's payload. Deliberately not one of the real
// ones: what is under test here is the SHAPE and its three rules, and a test
// built on a phase's own schema would fail for a reason belonging to that
// phase.
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

// SUBMISSIONS MADE AT ONCE LAND WHOLE. An executor run's bridge hands a
// response's calls to the surface side by side, with nothing that runs a
// submission alone, so two submissions — and the read of one — meet on the
// tool. Whichever lands last is the answer, and it is one submission: never
// one call's decision beside another's note.
//
// Mutation: drop the lock, and the race detector reports the writes (this
// package's tests run under -race), or a read returns a torn pair.
func TestSubmissionsMadeAtOnceLandWhole(t *testing.T) {
	t.Parallel()
	tl := tool()
	ctx := context.Background()
	const calls = 32
	submissions := make([]map[string]any, calls)
	for i := range submissions {
		decision := "yes"
		if i%2 == 1 {
			decision = "no"
		}
		submissions[i] = args(t, fmt.Sprintf(`{"decision":%q,"note":"%s-%d"}`, decision, decision, i))
	}
	whole := func(a answer) bool { return strings.HasPrefix(a.Note, a.Decision+"-") }

	var wg sync.WaitGroup
	torn := make(chan answer, calls)
	for i := range calls {
		wg.Add(2)
		go func() {
			defer wg.Done()
			if res, err := tl.Call(ctx, submissions[i]); err != nil || res.Failed {
				t.Errorf("submission %d: %v %+v", i, err, res)
			}
		}()
		go func() {
			defer wg.Done()
			if got, called := tl.Value(); called && !whole(got) {
				torn <- got
			}
		}()
	}
	wg.Wait()
	close(torn)
	for got := range torn {
		t.Errorf("a read mid-submission returned %+v: one call's decision beside another's note", got)
	}
	got, called := tl.Value()
	if !called || !whole(got) {
		t.Errorf("captured %+v (called=%v), want one whole submission", got, called)
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

// A FIELD THE PAYLOAD DOES NOT HAVE IS REFUSED BY NAME, at any depth, and
// the submission is not recorded.
//
// A plain unmarshal DROPS it, so a model that misspelt an optional field had
// its submission accepted as though it had said less — and was never told
// which field it got wrong. Refused, it goes back to the model, which is the
// one failure a model reliably fixes.
func TestAFieldThePayloadDoesNotHaveIsRefusedByName(t *testing.T) {
	t.Parallel()
	tl := tool()
	res, err := tl.Call(context.Background(), map[string]any{"decision": "yes", "not": "x"})
	if err != nil {
		t.Fatalf("Call returned a Go error: %v", err)
	}
	if !res.Failed || !strings.Contains(res.Output, "not is not a key") ||
		!strings.Contains(res.Output, "takes decision, note, id") {
		t.Fatalf("output = %q, want the misspelt field refused by name", res.Output)
	}
	if _, ok := tl.Value(); ok {
		t.Error("a refused submission was recorded as the answer")
	}

	// NESTED TOO: a field inside a list of objects is the same drop one
	// level down, and the one a schema built of task lists hides best.
	var plan struct {
		Tasks []struct {
			ID    string   `json:"id"`
			After []string `json:"after"`
		} `json:"tasks"`
	}
	err = structured.Remarshal(map[string]any{"tasks": []any{
		map[string]any{"id": "b", "afer": []any{"a"}},
	}}, &plan)
	if err == nil || !strings.Contains(err.Error(), "tasks[0].afer is not a key") {
		t.Errorf("a nested misspelt field answered %v, want it refused by name", err)
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

// EVERY FAULT OF A SUBMISSION IN ONE REFUSAL (ADR-0059): a misspelt field and
// a value of the wrong type come back together, each by its path, where
// encoding/json named only the first, so a model learned one per attempt.
//
// Mutation: answer encoding/json's own error and only the first is named.
func TestEveryFaultOfASubmissionIsNamedAtOnce(t *testing.T) {
	t.Parallel()
	var plan struct {
		ID    string   `json:"id"`
		After []string `json:"after"`
		Count int      `json:"count"`
	}
	err := structured.Remarshal(map[string]any{"id": 7.0, "afer": []any{"a"}, "count": "x"}, &plan)
	if err == nil {
		t.Fatal("a submission with three faults was accepted")
	}
	for _, want := range []string{"id is a number, and the format reads a string there",
		"afer is not a key", "count is a string, and the format reads a whole number there"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal %q does not say %q", err, want)
		}
	}
	// AND ACCEPTANCE IS UNCHANGED: a key in another case is what encoding/json
	// read, and still is.
	if err := structured.Remarshal(map[string]any{"ID": "x", "after": []any{"a"}}, &plan); err != nil ||
		plan.ID != "x" {
		t.Errorf("a key in another case is refused now: %v", err)
	}
}
