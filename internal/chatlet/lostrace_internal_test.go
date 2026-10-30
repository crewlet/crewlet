package chatlet

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/statelog"
)

// A LOST RACE ON A ROOM CARRIES NEITHER THE SUBJECT NOR THE FRAMEWORK'S WORDS.
//
// The store's translation wrapped the framework's own error after its mark,
// "%w: <subject>: %w", so the error's text named the room's broker subject —
// its id, or for an address the digest of its name — and "statelog: …kept
// changing under this write"; Sentence cut the mark and handed the rest to
// whoever asked it alone. Now the chain carries both sentinels, the text
// neither the subject nor the framework's error, and Sentence says a lost race
// in the state log's words for a person. Mutations: wrap the framework's error
// again and the subject row goes red; drop the statelog arm from Sentence and
// the sentence rows do.
func TestALostRaceOnARoomNamesNoSubject(t *testing.T) {
	t.Parallel()
	const subject = "0192f0aa-7b1c-7d2e-9f3a-4b5c6d7e8f90"
	framework := fmt.Errorf("%w: %s kept changing under this write",
		statelog.ErrConflict, "crewlet.chat.channel."+subject)
	for _, kind := range []ObjectKind{KindChannel, KindChannelName} {
		err := publishRefusal(kind, framework)
		if !errors.Is(err, ErrConflict) || !errors.Is(err, statelog.ErrConflict) {
			t.Fatalf("%s: a lost race is %v, want both sentinels in its chain", kind, err)
		}
		if strings.Contains(err.Error(), subject) || strings.Contains(err.Error(), "crewlet.") {
			t.Errorf("%s: a lost race's error is %q, which names the subject", kind, err)
		}
		if said := Sentence(err); said != statelog.LostRace {
			t.Errorf("%s: a lost race is told %q, want %q", kind, said, statelog.LostRace)
		}
	}
	// THE FRAMEWORK'S OTHER REFUSALS ARE ITS WORDS TOO, wherever a caller
	// meets one this store passed on unchanged.
	if said := Sentence(fmt.Errorf("%w: crewlet.chat.channel.%s", statelog.ErrExists,
		subject)); said != statelog.AlreadyThere {
		t.Errorf("a create of what is there is told %q, want %q", said, statelog.AlreadyThere)
	}
	// A CONFLICT THIS PACKAGE WORDED ITSELF keeps its own sentence: a
	// decision about a room that moved is not a lost race.
	own := fmt.Errorf("%w: #launch moved to another team after this was decided", ErrConflict)
	if said := Sentence(own); said != "#launch moved to another team after this was decided" {
		t.Errorf("the store's own conflict is told %q", said)
	}
}
