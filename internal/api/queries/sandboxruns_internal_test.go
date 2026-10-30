package queries

import (
	"maps"
	"slices"
	"testing"

	"github.com/crewlet/crewlet/internal/api/livestate"
	"github.com/crewlet/crewlet/internal/sandbox"
)

// EVERY KEY A CODING RUN'S ROW CARRIES IS CLASSIFIED, in both directions, and
// what a run was asked, asked back and answered is its seat's BODY (ADR-0033).
//
// The row is the board everybody reads, so a key added to [runRow] reaches
// everybody's overview the moment it is added — unless the table says what it
// is. A key the row writes that [runKeys] does not name fails here, as does a
// name the table keeps for a key the row no longer writes (a classification of
// nothing, which reads as a decision somebody made). And the four the owner's
// decision lists as a run's body are held to it by name, because the
// classification itself is what a careless edit would move.
//
// Mutation: add a key to runRow, or drop a row of runKeys, and this goes red;
// classify `task_description` as status and it goes red too.
func TestEverySandboxRunKeyIsClassified(t *testing.T) {
	t.Parallel()
	row := runRow(sandbox.PendingRun{}, nil)
	for key := range row {
		shown, ok := runKeys[key]
		if !ok {
			t.Errorf("the run row carries %q and runKeys does not classify it — "+
				"say whether it is status, body, the question or the tracker's", key)
			continue
		}
		if shown < runStatus || shown > runTracker {
			t.Errorf("%q is classified %d, which is no class", key, shown)
		}
	}
	for key := range runKeys {
		if _, ok := row[key]; !ok {
			t.Errorf("runKeys classifies %q and the run row does not carry it", key)
		}
	}
	for key, want := range map[string]runShown{
		"task_description": runBody,
		"reply":            runBody,
		"bridge_calls":     runBody,
		"question":         runQuestion,
		"work_item":        runTracker,
	} {
		if got := runKeys[key]; got != want {
			t.Errorf("%q is classified %d, want %d", key, got, want)
		}
	}
}

// A READER WITH NO REACH IS SHOWN STATUS AND NOTHING ELSE, and a reader with
// every reach is shown the whole row: the mask withholds by the table and by
// nothing else.
//
// Mutation: drop the default arm's delete in serialiseRun and add an
// unclassified key, and the first half goes red.
func TestAMaskedRunCarriesOnlyItsStatus(t *testing.T) {
	t.Parallel()
	run := sandbox.PendingRun{TurnID: "t1", AgentHandle: "swe",
		AudienceHandles: []string{"ana"}}
	status := serialiseRun(run, nil, livestate.Reach{})
	for key := range status {
		if runKeys[key] != runStatus {
			t.Errorf("a reader with no reach was shown %q, which is not status", key)
		}
	}
	if status["body_withheld"] != true {
		t.Errorf("body_withheld = %v for a reader shown no body", status["body_withheld"])
	}
	whole := serialiseRun(run, nil, livestate.Reach{Body: true, Tracker: true})
	if got, want := slices.Sorted(maps.Keys(whole)), slices.Sorted(maps.Keys(runKeys)); !slices.Equal(got, want) {
		t.Errorf("a reader with every reach was shown %v, want every key %v", got, want)
	}
	if whole["body_withheld"] != false {
		t.Errorf("body_withheld = %v for a reader shown the body", whole["body_withheld"])
	}
}
