package runner

import (
	"strings"
	"testing"
)

// The reviewer must not read the turn's own trigger as a request that just
// arrived. Both phases get the same bytes, but only the executor as the thing
// to do; bare, beside an `## Earlier rounds` block holding a question already
// posted, it reads as the sender asking again.
func TestTheReviewerIsToldTheTriggerIsNotANewRequest(t *testing.T) {
	t.Parallel()
	const trigger = "Can you open a task for test purpose?"
	framed := reviewTask(trigger).Text

	if framed == trigger {
		t.Fatal("the reviewer is handed the trigger bare, so it reads as a request that has just arrived")
	}
	// VERBATIM AND LAST: a reviewer judging a paraphrase judges words the
	// sender never wrote.
	if !strings.HasSuffix(framed, trigger) {
		t.Errorf("the trigger must survive the framing verbatim and last, got %q", framed)
	}
	// The three misreadings the frame rules out: that the message is new, a
	// re-post, or an instruction to act on now rather than what the rounds
	// below were already answering.
	for _, want := range []string{"ALREADY working on", "not a new", "not a repeat of it"} {
		if !strings.Contains(framed, want) {
			t.Errorf("the frame never says %q:\n%s", want, framed)
		}
	}
}
