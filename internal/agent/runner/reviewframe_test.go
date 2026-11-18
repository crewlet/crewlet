package runner_test

import (
	"context"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/agent/phase"
	"github.com/crewlet/crewlet/internal/agent/runner"
	"github.com/crewlet/crewlet/internal/agent/turn"
	"github.com/crewlet/crewlet/internal/providers/llm/llmtest"
)

// The FRAME REACHES THE MODEL: reviewTask's own test proves the string is
// built, this that the review phase sends it. A bare trigger beside an
// `## Earlier rounds` block reads to a reviewer as the sender asking again.
func TestTheReviewerReceivesTheTriggerFramedAsTheTurnsOwn(t *testing.T) {
	t.Parallel()
	r, prov, _ := fixture(t, &scriptedProvider{
		review: []llmtest.Response{submitCall(t, runner.SubmitReviewTool, `{"decision":"done"}`)},
	})
	w := turn.Work{Outcome: turn.OutcomeDelivered, Summary: "posted it", Text: "posted"}
	if _, err := r.Review(context.Background(), 1, w, nil); err != nil {
		t.Fatalf("Review: %v", err)
	}
	user := prov.requestsFor("review")[0].Messages[1].Content
	if !strings.Contains(user, "ALREADY working on") {
		t.Errorf("the reviewer was handed the trigger bare:\n%s", user)
	}
	// The trigger still reaches it verbatim: the frame precedes the
	// evidence, never rewrites it.
	if !strings.Contains(user, "post the weekly summary") {
		t.Errorf("the trigger was lost in the framing:\n%s", user)
	}
}

// The EXECUTOR is still handed it bare: to it the trigger is the thing to do,
// and "you were already working on this" would be false on round 1 and cost
// the prefix cache every round after.
func TestTheExecutorIsStillHandedTheTriggerBare(t *testing.T) {
	t.Parallel()
	_, req := launch(t, []phase.Entry{{Key: "default", Provider: &scriptedProvider{}}},
		buildOpts{}, 1, "", nil)
	user := req.Brief
	if strings.Contains(user, "ALREADY working on") {
		t.Errorf("the executor was handed the reviewer's frame:\n%s", user)
	}
	if !strings.Contains(user, "post the weekly summary") {
		t.Errorf("the executor lost the trigger:\n%s", user)
	}
}

// The frame does not move between rounds: the provider's prefix cache is keyed
// on the user message the review re-sends each round, so it is a plain
// function of the task, not [Runner.taskFor]'s per-round correction. Driven
// through two real rounds, since the property is what the PHASE sends.
func TestTheReviewFrameDoesNotMoveBetweenRounds(t *testing.T) {
	t.Parallel()
	r, prov, _ := fixture(t, &scriptedProvider{
		review: []llmtest.Response{
			submitCall(t, runner.SubmitReviewTool, `{"decision":"self_iterate","notes":"try again"}`),
			submitCall(t, runner.SubmitReviewTool, `{"decision":"done"}`),
		},
	})
	w := turn.Work{Outcome: turn.OutcomeDelivered, Summary: "posted it", Text: "posted"}
	for round := 1; round <= 2; round++ {
		if _, err := r.Review(context.Background(), round, w, nil); err != nil {
			t.Fatalf("Review round %d: %v", round, err)
		}
	}
	reqs := prov.requestsFor("review")
	if len(reqs) != 2 {
		t.Fatalf("review requests = %d, want 2", len(reqs))
	}
	if first, second := reqs[0].Messages[1].Content, reqs[1].Messages[1].Content; first != second {
		t.Errorf("the frame moved between rounds:\nround 1: %q\nround 2: %q", first, second)
	}
}

// A TASK'S CHARGE COUNTS THE REVIEWS THAT SENT ITS WORK BACK, from the
// decisions the review phases published, on the same tally as rounds and the
// phases that ran, which the turn's ending record states for the usage domain.
func TestTheSpendCountsTheReviewsThatSentWorkBack(t *testing.T) {
	t.Parallel()
	r, _, _ := fixture(t, &scriptedProvider{
		review: []llmtest.Response{
			submitCall(t, runner.SubmitReviewTool, `{"decision":"self_iterate","notes":"try again"}`),
			submitCall(t, runner.SubmitReviewTool, `{"decision":"self_iterate","notes":"closer"}`),
			submitCall(t, runner.SubmitReviewTool, `{"decision":"done"}`),
		},
	})
	w := turn.Work{Outcome: turn.OutcomeDelivered, Summary: "posted it", Text: "posted"}
	for round := 1; round <= 3; round++ {
		if _, err := r.Review(context.Background(), round, w, nil); err != nil {
			t.Fatalf("Review round %d: %v", round, err)
		}
	}
	spend := r.Spend()
	if spend.SentBack != 2 {
		t.Errorf("sent back = %d over a turn whose reviewer returned the work "+
			"twice and accepted it once, want 2", spend.SentBack)
	}
	// The reviews, stated whole by the turn's ending record
	// (types.TurnTally): three ran and two did not accept.
	if spend.Reviews != 3 || spend.Unaccepted != 2 {
		t.Errorf("reviews = %d, unaccepted = %d over three reviews of which two sent the "+
			"work back, want 3 and 2", spend.Reviews, spend.Unaccepted)
	}
	if spend.Rounds < 3 {
		t.Errorf("rounds = %d over three review phases, want at least one each", spend.Rounds)
	}
	if len(spend.Phases) != 1 || spend.Phases[0] != "review" {
		t.Errorf("phases = %v, want the one phase that ran, once", spend.Phases)
	}
	// The last send-back's request, which the following pass answered, is
	// what the turn card quotes: not the first, nor the acceptance's empty
	// notes.
	if spend.Review != "closer" {
		t.Errorf("review = %q, want the newest send-back's notes %q",
			spend.Review, "closer")
	}
}
