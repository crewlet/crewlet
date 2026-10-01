package steer_test

import (
	"strconv"
	"testing"

	"github.com/crewlet/crewlet/internal/agent/steer"
	"github.com/crewlet/crewlet/internal/clientsource"
)

// THE DASHBOARD HOLDS A NOTE AT THE ENGINE'S CAP, read out of its own source.
//
// The Steer dialog stops a note at `STEER_NOTE_MAX_RUNES` before it is sent,
// so a person is told the bound while typing. A figure there below this one
// holds back a note the turn would take; one above it sends a note the tool
// refuses, one press too late.
func TestTheDashboardBoundsANoteAtTheEnginesCap(t *testing.T) {
	t.Parallel()
	raw, err := clientsource.Scalar(clientsource.Tree(t), "STEER_NOTE_MAX_RUNES")
	if err != nil {
		t.Fatal(err)
	}
	client, err := strconv.Atoi(raw)
	if err != nil {
		t.Fatalf("STEER_NOTE_MAX_RUNES is %q, which is not an integer: %v", raw, err)
	}
	if client != steer.MaxNoteRunes {
		t.Errorf("the dashboard bounds a note at %d characters and the engine at %d — "+
			"change it in contract/steer.ts to the engine's figure", client, steer.MaxNoteRunes)
	}
}
