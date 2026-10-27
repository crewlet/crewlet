package steer_test

import (
	"slices"
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

// THE DASHBOARD KNOWS WHEN EVERY NOTE IS READ, read out of its own source.
//
// steer_turn answers when the turn will read a note it took (`reads`), and the
// Steer dialog says so in words chosen per value. A value the engine answers
// that the dashboard does not list is told as the generic sentence — a note to
// a run parked on its question announced as read at its next step — and one it
// lists that the engine never answers is copy nobody can reach.
func TestTheDashboardKnowsWhenANoteIsRead(t *testing.T) {
	t.Parallel()
	body, err := clientsource.Literal(clientsource.Tree(t), "STEER_READS")
	if err != nil {
		t.Fatal(err)
	}
	client := clientsource.Strings(body)
	engine := make([]string, 0, len(steer.ReadsAll))
	for _, r := range steer.ReadsAll {
		engine = append(engine, string(r))
	}
	if !slices.Equal(client, engine) {
		t.Errorf("the dashboard lists %v as when a note is read and the engine answers %v — "+
			"change STEER_READS in contract/steer.ts, and the dialog's words for each", client, engine)
	}
}
