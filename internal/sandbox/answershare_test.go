package sandbox

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/redact"
)

// A REPLY PAST ITS SHARE IS SEALED ONLY TO ITS SHARE: words whose redaction
// passes what an answer may hold ([MaxAnswerBytes]) are sealed as the first
// MaxAnswerBytes+1 bytes of that redaction, which alone weigh the record past
// its share, so the store refuses it as it refuses every answer past it; words
// within it are sealed as their whole redaction and fit.
//
// Mutation: seal the whole redaction, and the words past the share are
// carried whole.
func TestAReplyPastItsShareIsSealedOnlyToItsShare(t *testing.T) {
	t.Parallel()
	secret := "ghp_" + strings.Repeat("c", 36)
	piece := "pwd=" + strings.Repeat(`x\n`, 20) + "\n"
	for name, tc := range map[string]struct {
		text string
		fits bool
	}{
		"within the share":      {strings.Repeat("the build is green ", 5<<10) + secret, true},
		"past the share":        {strings.Repeat("a log line ", MaxAnswerBytes/11) + secret + " and the rest", false},
		"past it once redacted": {strings.Repeat(piece, MaxAnswerBytes/len(piece)/2), false},
	} {
		words, answer := Reply{Text: tc.text}.answerOf(time.Now().UTC())
		redacted := redact.Secrets(tc.text)
		want := redacted
		if !tc.fits {
			if len(redacted) <= MaxAnswerBytes {
				t.Fatalf("%s: the case's words redact to %d bytes, within the share", name, len(redacted))
			}
			want = redacted[:MaxAnswerBytes+1]
		}
		if words.Text != want {
			t.Errorf("%s: sealed %d bytes of words, want %d", name, len(words.Text), len(want))
		}
		sealed, err := rigAnswers().Seal(PendingRun{TurnID: "t1", LaunchID: "l1"}, words, answer)
		if err != nil {
			t.Fatalf("%s: sealing: %v", name, err)
		}
		if err := answerFits("t1", sealed); errors.Is(err, ErrAnswerTooLarge) == tc.fits {
			t.Errorf("%s: the store's weighing answered %v, want the record to fit: %v", name, err, tc.fits)
		}
	}
}
