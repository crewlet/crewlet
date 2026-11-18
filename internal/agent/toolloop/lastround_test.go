package toolloop_test

import (
	"testing"

	"github.com/crewlet/crewlet/internal/agent/toolloop"
)

// THE LAST ROUND IS ANNOUNCED: a run that would go on into it ends once the round before is
// answered, and the next run, the last, reads the note first. Mutation: never end the run for the
// note, and the one run spends all three rounds without reading it.
func TestTheLastRoundNoteIsReadBeforeTheLastRound(t *testing.T) {
	t.Parallel()
	const note = "This is your last round."
	prov := script(
		runOf(calling("", "search"), calling("", "search"), calling("", "search")),
		runOf(says("here is what I have")),
	)
	res, err := run(t, toolloop.Config{Provider: prov, Surface: newSurface("search"),
		MaxRounds: 3, LastRoundNote: note})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	seen := prov.Seen()
	if len(seen) != 2 {
		t.Fatalf("%d runs, want the one ended before the last round and the last", len(seen))
	}
	if indexOf(seen[0].Messages, note) != -1 {
		t.Error("the first run read the note before its last round")
	}
	if indexOf(seen[1].Messages, note) == -1 {
		t.Errorf("the last run did not read the note: %+v", seen[1].Messages)
	}
	if res.RoundsUsed != 3 || res.Text != "here is what I have" {
		t.Errorf("rounds %d, text %q, want 3 rounds ending in the answer", res.RoundsUsed, res.Text)
	}
}

// A ONE-ROUND LOOP READS THE NOTE BEFORE ITS ONLY ROUND, and once: there is no
// earlier run to end.
func TestAOneRoundLoopReadsTheLastRoundNoteFirst(t *testing.T) {
	t.Parallel()
	const note = "This is your last round."
	prov := script(runOf(says("done")))
	if _, err := run(t, toolloop.Config{Provider: prov, Surface: newSurface("search"),
		MaxRounds: 1, LastRoundNote: note}); err != nil {
		t.Fatalf("Run: %v", err)
	}
	seen := prov.Seen()
	if len(seen) != 1 || indexOf(seen[0].Messages, note) == -1 {
		t.Fatalf("runs = %+v, want one run reading the note", seen)
	}
}
