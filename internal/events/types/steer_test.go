package types

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/clientsource"
)

// A BOUNDARY OFF THE WIRE IS A VALUE, never a decode error: a newer peer may
// read a note at a step this build has no name for, and the record must still
// arrive as one a reader can refuse to act on.
func TestSteerBoundaryValid(t *testing.T) {
	t.Parallel()
	for _, b := range SteerBoundaries {
		if !b.Valid() {
			t.Errorf("%q is not valid", b)
		}
	}
	if SteerBoundary("between_tools").Valid() || SteerBoundary("").Valid() {
		t.Error("a boundary this build does not record is valid")
	}
	want := []SteerBoundary{"round", "brief", "tool_batch", "finish", "step"}
	if !slices.Equal(SteerBoundaries, want) {
		t.Errorf("the boundaries are %v, want %v", SteerBoundaries, want)
	}
}

// AN EXPIRED NOTE NAMES NO STEP, and a phase's read always names one. Nothing
// read an expired note, so its record carries no step, job, phase or round —
// absent rather than zero, as the dashboard reads them. Every read a phase
// records names the kind of step that read it, so a reader never has to guess
// one.
func TestBoundaryAndLaunchIDAreOmittedWhenEmpty(t *testing.T) {
	t.Parallel()
	expired := AgentTurnSteered{TurnID: "run-1", NoteID: "n-1", Outcome: SteerExpired, Note: "use staging"}
	raw, err := json.Marshal(expired)
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{`"boundary"`, `"launch_id"`, `"phase"`, `"round"`} {
		if strings.Contains(string(raw), key) {
			t.Errorf("an expired note's record carries %s: %s", key, raw)
		}
	}
	steer, err := json.Marshal(PhaseSteer{Round: 2, NoteID: "n-1", Boundary: SteerBoundaryRound})
	if err != nil {
		t.Fatal(err)
	}
	if string(steer) != `{"round":2,"note_id":"n-1","boundary":"round"}` {
		t.Errorf("a round's read encodes as %s", steer)
	}
}

// THE DASHBOARD NAMES EVERY STEP A NOTE IS READ AT, read out of its own source.
//
// The turn trace says each boundary in words and places a read inside a coding
// run on the run's bar rather than on a round. A boundary the engine records
// that the dashboard does not list is drawn as a round that does not exist;
// one it lists that the engine never records is copy nobody can reach.
func TestTheDashboardNamesEverySteerBoundary(t *testing.T) {
	t.Parallel()
	body, err := clientsource.Literal(clientsource.Tree(t), "STEER_BOUNDARIES")
	if err != nil {
		t.Fatal(err)
	}
	client := clientsource.Strings(body)
	engine := make([]string, 0, len(SteerBoundaries))
	for _, b := range SteerBoundaries {
		engine = append(engine, string(b))
	}
	if !slices.Equal(client, engine) {
		t.Errorf("the dashboard names %v as the steps a note is read at and the engine records %v — "+
			"change STEER_BOUNDARIES in contract/steer.ts, and the trace's words for each", client, engine)
	}
}
