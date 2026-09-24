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

// The frame does not move between rounds.
//
// The review phase re-sends its user message every round and the provider's
// prefix cache is keyed on those bytes, so a frame that varied per round — a
// round number in the label, a correction folded in — would cost a cache miss
// on every round of every turn. That is why this is a plain function of the
// task and not [Runner.taskFor]'s per-round correction.
//
// Driven through two real rounds rather than comparing one call to itself:
// the property is about what the PHASE sends, and a pure function compared
// with a pure function is a tautology that cannot fail.
func TestTheReviewFrameDoesNotMoveBetweenRounds(t *testing.T) {
	t.Parallel()
	r, prov, _ := fixture(t, &scriptedProvider{
		review: []llm.Completion{
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

// A TASK'S CHARGE COUNTS THE REVIEWS THAT SENT ITS WORK BACK.
//
// The count is taken from the review phases' own records — the decision each
// one published — so it is the same number the usage domain's per-seat
// `sent_back` is. Rounds and the phases that ran ride the same tally, because
// a task's charge reads all three from it.
func TestTheSpendCountsTheReviewsThatSentWorkBack(t *testing.T) {
	t.Parallel()
	r, _, _ := fixture(t, &scriptedProvider{
		review: []llm.Completion{
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
	if spend.Rounds < 3 {
		t.Errorf("rounds = %d over three review phases, want at least one each", spend.Rounds)
	}
	if len(spend.Phases) != 1 || spend.Phases[0] != "review" {
		t.Errorf("phases = %v, want the one phase that ran, once", spend.Phases)
	}
}
