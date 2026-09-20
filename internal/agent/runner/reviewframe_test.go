package runner_test

import (
	"context"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/agent/runner"
	"github.com/crewlet/crewlet/internal/agent/turn"
	"github.com/crewlet/crewlet/internal/providers/llm"
)

// The FRAME REACHES THE MODEL, which is the half a unit test of the helper
// cannot see.
//
// reviewTask's own test proves the string is built; this proves the review
// phase actually sends it. Wiring the bare trigger back into the user message
// would leave that one green, and the bare trigger is precisely the bug: next
// to an `## Earlier rounds` block showing a question the turn had already
// posted, a reviewer reads it as the sender asking a second time.
func TestTheReviewerReceivesTheTriggerFramedAsTheTurnsOwn(t *testing.T) {
	t.Parallel()
	r, prov, _ := fixture(t, &scriptedProvider{
		review: []llm.Completion{submitCall(t, runner.SubmitReviewTool, `{"decision":"done"}`)},
	})
	w := turn.Work{Outcome: turn.OutcomeDelivered, Summary: "posted it", Text: "posted"}
	if _, err := r.Review(context.Background(), 1, w, nil); err != nil {
		t.Fatalf("Review: %v", err)
	}
	user := prov.requestsFor("review")[0].Messages[1].Content
	if !strings.Contains(user, "ALREADY working on") {
		t.Errorf("the reviewer was handed the trigger bare:\n%s", user)
	}
	// And the trigger itself still reaches it verbatim — the frame is
	// context in front of the evidence, never a rewrite of it.
	if !strings.Contains(user, "post the weekly summary") {
		t.Errorf("the trigger was lost in the framing:\n%s", user)
	}
}

// The EXECUTOR is still handed it bare, which is the other half of the same
// fact: the executor is being given the trigger as the thing to do, and a
// frame saying "you were already working on this" would be false on round 1
// and would cost the prompt-prefix cache on every round after.
func TestTheExecutorIsStillHandedTheTriggerBare(t *testing.T) {
	t.Parallel()
	r, prov, _ := fixture(t, &scriptedProvider{execute: []llm.Completion{submitWork(t)}})
	if _, _, err := r.Execute(context.Background(), 1, "", nil); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	user := prov.requestsFor("execute")[0].Messages[1].Content
	if strings.Contains(user, "ALREADY working on") {
		t.Errorf("the executor was handed the reviewer's frame:\n%s", user)
	}
	if !strings.Contains(user, "post the weekly summary") {
		t.Errorf("the executor lost the trigger:\n%s", user)
	}
}
