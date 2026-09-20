package runner

import (
	"strings"
	"testing"
)

// The reviewer must not read the turn's own trigger as a request that just
// arrived.
//
// Both phases are handed the SAME bytes, and only the executor is being
// handed them as the thing to do. Bare, next to an `## Earlier rounds` block
// showing a question the turn had already posted, the only account that fits
// both is that the sender asked again — which is what reviewers wrote, twice
// running, before instructing the next round to act on a message nobody had
// sent.
func TestTheReviewerIsToldTheTriggerIsNotANewRequest(t *testing.T) {
	t.Parallel()
	const trigger = "Can you open a task for test purpose?"
	framed := reviewTask(trigger)

	if framed == trigger {
		t.Fatal("the reviewer is handed the trigger bare, so it reads as a request that has just arrived")
	}
	// VERBATIM AND LAST. The frame is context in front of the evidence, not
	// a rewrite of it: a reviewer judging a paraphrase of the trigger is
	// judging against something the sender never wrote.
	if !strings.HasSuffix(framed, trigger) {
		t.Errorf("the trigger must survive the framing verbatim and last, got %q", framed)
	}
	// The three facts the frame exists to carry. Each is a separate reading
	// a reviewer actually reached: that the message is new, that it is a
	// re-post, and that it is an instruction to act on now rather than the
	// thing the rounds below were already answering.
	for _, want := range []string{"ALREADY working on", "not a new", "not a repeat of it"} {
		if !strings.Contains(framed, want) {
			t.Errorf("the frame never says %q:\n%s", want, framed)
		}
	}
}
