package assistant_test

import (
	"fmt"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/assistant/assistanttest"
	"github.com/crewlet/crewlet/internal/assistant/evals"
)

// A conversation past its window is summarized after a turn, and the next
// turn's prompt carries that summary in place of the messages it covers.
func TestALongConversationIsSummarizedForItsNextTurn(t *testing.T) {
	t.Parallel()
	const summary = "They chose a Worker route for the deck library."
	f := assistanttest.NewRig(t, evals.RigOptions{Rounds: []evals.Round{
		evals.Say("Answer."), evals.Say(summary), evals.Say(summary), evals.Say("Next answer.")}})
	f.Store.Seed(f.Person.ID, f.Conversation, "Routing")
	var earlier []string
	for i := range 20 {
		earlier = append(earlier, fmt.Sprintf("earlier message %d.", i))
	}
	f.Store.SeedHistory(f.Person.ID, f.Conversation, nil, earlier...)

	summarized := make(chan struct{})
	f.Store.OnSummary(func() { close(summarized) })
	first, err := f.Ask("k-first", "And now?", nil)
	if err != nil {
		t.Fatal(err)
	}
	f.Follow(first.TurnID, 0)
	// The summary call runs in the background, after the answer.
	select {
	case <-summarized:
	case <-t.Context().Done():
		t.Fatal("no summary was recorded")
	}
	tr, err := f.Store.Transcript(t.Context(), f.Person.ID, f.Conversation)
	if err != nil {
		t.Fatal(err)
	}
	// 22 messages, the last 12 kept as they are: the first 10 are summarized.
	if tr.SummaryThrough != 10 || !strings.Contains(tr.Summary, summary) {
		t.Fatalf("summary = %q through %d, want it through 10", tr.Summary, tr.SummaryThrough)
	}

	next, err := f.Ask("k-next", "What did we decide?", nil)
	if err != nil {
		t.Fatal(err)
	}
	f.Follow(next.TurnID, 0)
	requests := f.Model.Requests()
	var prompt string
	for _, m := range requests[len(requests)-1].Messages {
		prompt += m.Content + "\n"
	}
	if !strings.Contains(prompt, "first 10 messages") || !strings.Contains(prompt, summary) {
		t.Errorf("the next turn's prompt does not carry the summary:\n%s", prompt)
	}
	if strings.Contains(prompt, "earlier message 0.") {
		t.Error("a message the summary covers is in the prompt beside it")
	}
}
