package worklet

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/statelog"
)

// A CREATE THE FRAMEWORK FOUND MADE NAMES NOTHING, carrying the framework's
// sentinel: its error names the subject it found made, and the translation
// carried that after the subject's id, which any caller printing the error
// would have shown a person. The sentence is the state log's, and every other
// refusal travels unchanged. Mutations: wrap the framework's error again and
// the text check goes red; drop the statelog arm from Sentence and the
// sentence checks do.
func TestACreateTheFrameworkFoundMadeNamesNothing(t *testing.T) {
	t.Parallel()
	const subject = "crewlet.tracker.task.0192f0aa-7b1c-7d2e-9f3a-4b5c6d7e8f90"
	err := publishRefusal(fmt.Errorf("%w: %s", statelog.ErrExists, subject))
	if !errors.Is(err, statelog.ErrExists) {
		t.Fatalf("a create of what is there is %v, want the framework's sentinel", err)
	}
	if strings.Contains(err.Error(), "0192f0aa") || strings.Contains(err.Error(), "crewlet.") {
		t.Errorf("a create of what is there is %q, which names the subject", err)
	}
	if said := Sentence(err); said != statelog.AlreadyThere {
		t.Errorf("a create of what is there is told %q, want %q", said, statelog.AlreadyThere)
	}
	lost := fmt.Errorf("%w: %s kept changing under this write", statelog.ErrConflict, subject)
	if got := publishRefusal(lost); !errors.Is(got, lost) || got.Error() != lost.Error() {
		t.Errorf("a lost race became %v, want it unchanged", got)
	}
	if said := Sentence(lost); said != statelog.LostRace {
		t.Errorf("a lost race is told %q, want %q", said, statelog.LostRace)
	}
}
