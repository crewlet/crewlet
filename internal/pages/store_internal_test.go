package pages

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/statelog"
)

// EVERY REFUSAL A SEAT IS TOLD TO ACT ON HAS A PRODUCER.
//
// The tool surface branches on [ErrTitleTaken], [ErrStaleVersion] and
// [ErrConflict] and tells the model something different for each — "that page
// exists, edit it", "re-base and save again", "somebody else is editing this,
// read it again". The conflict branch matched nothing for as long as the retry
// lived in the framework and this translation did not know about it: a write
// that lost sixteen races came back as the generic "did not land", which tells
// a model to report a failure rather than to re-read and retry.
//
// A test over values, because the producing condition is sixteen lost races.
func TestEveryRefusalACallerActsOnIsTranslated(t *testing.T) {
	t.Parallel()

	for name, tc := range map[string]struct {
		in     error
		want   error
		framed bool // the framework's own sentinel survives the translation
	}{
		"a title somebody already holds": {
			in: fmt.Errorf("append: %w", statelog.ErrExists), want: ErrTitleTaken,
		},
		"a write that ran out of rounds": {
			in: fmt.Errorf("append: %w", statelog.ErrConflict), want: ErrConflict,
			framed: true,
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			got := refusal(tc.in, "ENG.deploy-runbook")
			if !errors.Is(got, tc.want) {
				t.Fatalf("refusal(%v) = %v, which is not %v — the tool surface "+
					"branches on that sentinel and would fall through to the "+
					"answer that tells a model the change is simply lost",
					tc.in, got, tc.want)
			}
			if !strings.Contains(got.Error(), "ENG.deploy-runbook") {
				t.Errorf("the refusal does not name the subject: %v — an error "+
					"a person reads names what they have to go and look at", got)
			}
			if framed := errors.Is(got, tc.in); framed != tc.framed {
				t.Errorf("the framework's own error survives = %v, want %v",
					framed, tc.framed)
			}
		})
	}
}

// AND NOTHING ELSE IS RESHAPED. An unavailable broker is not a refusal a
// person can act on, and translating it into one of the two above would tell a
// seat to re-read a page over a store it could not reach.
func TestARefusalThisPackageHasNoWordForTravelsUnchanged(t *testing.T) {
	t.Parallel()

	if got := refusal(nil, "ENG.runbook"); got != nil {
		t.Errorf("a write that landed came back as %v", got)
	}
	unreachable := fmt.Errorf("dial: %w", statelog.ErrUnavailable)
	got := refusal(unreachable, "ENG.runbook")
	if !errors.Is(got, statelog.ErrUnavailable) {
		t.Fatalf("an unavailable log came back as %v", got)
	}
	for _, mistaken := range []error{ErrTitleTaken, ErrConflict, ErrStaleVersion} {
		if errors.Is(got, mistaken) {
			t.Errorf("an unavailable log reads as %v, so a caller acts on a "+
				"refusal the broker never made", mistaken)
		}
	}
}
