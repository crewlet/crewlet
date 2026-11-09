package runner_test

import (
	"slices"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/agent/execstate"
	"github.com/crewlet/crewlet/internal/agent/ledger"
	"github.com/crewlet/crewlet/internal/agent/phase"
	"github.com/crewlet/crewlet/internal/agent/runner"
	"github.com/crewlet/crewlet/internal/agent/turn"
	"github.com/crewlet/crewlet/internal/providers/llm/llmtest"
)

// A COLLECTED ROUND CARRIES ITS REFUSED REPLIES TO ITS REVIEW (ADR-0114). A reply
// too large for the run to take is refused by the engine, which resumes the run
// told so; the round's calls hold the run's questions and none of the reply, so
// the refusals ride the collected run's record (sandbox's RoundRefusals, through
// the engine's RunRecord) into the round's Work, which is what the reviewer is
// handed.
//
// Mutation: leave RefusedReplies off the Work finishWork builds, and the round's
// reviewer is told of no refusal.
func TestACollectedRoundCarriesItsRefusedRepliesToItsReview(t *testing.T) {
	t.Parallel()
	refused := []turn.RefusedReply{{Messages: 3, Bytes: 600 << 10, MaxBytes: 512 << 10}}
	r, _ := buildWith(t, []phase.Entry{{Key: "default", Provider: &scriptedProvider{}}}, buildOpts{
		resume: &runner.Resume{
			State:  execstate.State{Version: execstate.Version, Round: 1},
			Answer: "asked again for the last hundred lines of the log, and fixed the test",
			Bridged: []runner.BridgedCall{
				{Call: ledger.Call{Name: "slack_post", Result: "posted"}},
				{Call: ledger.Call{Name: runner.SubmitWorkTool, Args: map[string]any{
					"outcome": "delivered", "summary": "fixed the failing test",
					"deliveries": []any{"slack_post"},
				}}},
			},
			Run: runner.RunRecord{Harness: "claude-code", SandboxID: "box-7", RefusedReplies: refused},
		},
	})
	work, _, err := r.Resume(t.Context(), nil)
	if err != nil {
		t.Fatalf("Resume: %v", err)
	}
	if !slices.Equal(work.RefusedReplies, refused) {
		t.Errorf("the round's work carries %+v, want the refusals its record names", work.RefusedReplies)
	}
}

// A REVIEWER IS TOLD WHAT REPLY THE RUN NEVER GOT (ADR-0114). The round's
// refused replies reach the review prompt beside the log, in the engine's
// words: each one's message count and figures, and what the engine told the
// run to do — so a question asked again after one reads as the follow-up it
// is, never the same question sent twice.
//
// Mutation: leave RefusedReplies off the ReviewInput Review builds, and the
// reviewer reads the round's two questions with nothing between them.
func TestAReviewerIsHandedTheRoundsRefusedReplies(t *testing.T) {
	t.Parallel()
	r, prov, _ := fixture(t, &scriptedProvider{
		review: []llmtest.Response{submitCall(t, runner.SubmitReviewTool, `{"decision":"done"}`)},
	})
	w := turn.Work{
		Outcome: turn.OutcomeDelivered, Summary: "asked for the log, then for its tail", Text: "fixed",
		Calls: []ledger.Call{
			{Name: "ask_and_wait", Args: map[string]any{"question": "paste the failing log"}},
			{Name: "ask_and_wait", Args: map[string]any{"question": "the log was too large; its last 100 lines?"}},
		},
		RefusedReplies: []turn.RefusedReply{{Messages: 2, Bytes: 3 << 20, MaxBytes: 512 << 10}},
	}
	if _, err := r.Review(t.Context(), 1, w, nil); err != nil {
		t.Fatalf("Review: %v", err)
	}
	system := prov.requestsFor("review")[0].Messages[0].Content
	for _, want := range []string{"## Replies the agent did not get",
		"- a reply of 2 messages, 3.0 MiB as the engine records an answer, past the 512 KiB",
		"the agent was given none of it"} {
		if !strings.Contains(system, want) {
			t.Errorf("the reviewer is not told %q:\n%s", want, system)
		}
	}
}
