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

// AN OUTCOME OFF THE WIRE IS A VALUE, never a decode error, and this build
// knows four: a newer peer may report a note's end under a name this build has
// none for, and the record must still arrive as one a reader can say nothing
// of rather than misread.
func TestSteerOutcomeValid(t *testing.T) {
	t.Parallel()
	for _, o := range SteerOutcomes {
		if !o.Valid() {
			t.Errorf("%q is not valid", o)
		}
	}
	if SteerOutcome("forgotten").Valid() || SteerOutcome("").Valid() {
		t.Error("an outcome this build does not record is valid")
	}
	want := []SteerOutcome{"delivered", "expired", "missed", "unconfirmed"}
	if !slices.Equal(SteerOutcomes, want) {
		t.Errorf("the outcomes are %v, want %v", SteerOutcomes, want)
	}
}

// EACH OUTCOME IS SAID AS WHAT BECAME OF THE NOTE, in the line an activity feed
// shows: read, finished before reading, missed by a step that took it and never
// handed it over, or handed over and never confirmed; and one this build does
// not know as no more than a note having been sent, never as read or as expired.
//
// Mutation: say a missed or unconfirmed note as read, or an unknown outcome as
// read, and the case goes red.
func TestEachSteerOutcomeIsSaidAsWhatBecameOfTheNote(t *testing.T) {
	t.Parallel()
	for outcome, want := range map[SteerOutcome]string{
		SteerDelivered:   "swe read a note from founder",
		SteerExpired:     "swe finished before reading a note from founder",
		SteerMissed:      "swe missed a note from founder, taken by a step that never handed it over",
		SteerUnconfirmed: "swe was handed a note from founder and never confirmed reading it",
		"forgotten":      "swe was sent a note from founder",
	} {
		if got := (AgentTurnSteered{Outcome: outcome, SteeredBy: "founder"}).SummaryFor("swe"); got != want {
			t.Errorf("%s says %q, want %q", outcome, got, want)
		}
	}
}

// AN EXPIRED NOTE NAMES NO STEP, and a phase's read always names one. Nothing
// read an expired note, so its record carries no step, job, phase or round
// (absent rather than zero, as the dashboard reads them), and says nothing of a
// re-telling, which only a delivered note's record may. Every read a phase
// records names the kind of step that read it, so a reader never has to guess
// one.
func TestBoundaryAndLaunchIDAreOmittedWhenEmpty(t *testing.T) {
	t.Parallel()
	expired := AgentTurnSteered{TurnID: "run-1", NoteID: "n-1", Outcome: SteerExpired, Note: "use staging"}
	raw, err := json.Marshal(expired)
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{`"boundary"`, `"launch_id"`, `"phase"`, `"round"`, `"retold"`} {
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

// THE DASHBOARD NAMES EVERY OUTCOME A NOTE CAN HAVE, read out of its own
// source. The turn trace says each in words and places every one that names a
// step on that step; an outcome the engine records that the dashboard does not
// list is drawn as one it cannot name, and one it lists that the engine never
// records is copy nobody can reach.
//
// Mutation: add an outcome to SteerOutcomes alone, and this goes red.
func TestTheDashboardNamesEverySteerOutcome(t *testing.T) {
	t.Parallel()
	body, err := clientsource.Literal(clientsource.Tree(t), "STEER_OUTCOMES")
	if err != nil {
		t.Fatal(err)
	}
	client := clientsource.Strings(body)
	engine := make([]string, 0, len(SteerOutcomes))
	for _, o := range SteerOutcomes {
		engine = append(engine, string(o))
	}
	if !slices.Equal(client, engine) {
		t.Errorf("the dashboard names %v as what can become of a note and the engine records %v: "+
			"change STEER_OUTCOMES in contract/steer.ts, and the trace's words for each", client, engine)
	}
}

// THE DASHBOARD NAMES EVERY STEP A NOTE IS READ AT, read out of its own source.
//
// The turn trace says each boundary in words and places a read inside an executor
// on the run's bar rather than on a round. A boundary the engine records
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
