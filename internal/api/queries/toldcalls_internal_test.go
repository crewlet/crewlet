package queries

import (
	"encoding/json"
	"slices"
	"testing"

	"github.com/crewlet/crewlet/internal/sandbox"
)

// THE RUNS ANSWER NAMES THE LATE CALLS ITS RUN'S SESSIONS TOOK: each by its
// place in the log and the job whose session took it, and only those of the
// log's own round, since a telling names a place in its round's log alone and
// one an older build's new-round reset left on the row would tag a call it
// never named. A log naming no round (a reader shown no calls) tags none, and
// the answer is an array either way.
//
// Mutation: answer every telling on the row, whatever its round
// (told-any-round), and the round before's telling tags this log's call 3.
func TestTheRunsAnswerNamesTheLateCallsItsRunsSessionsTook(t *testing.T) {
	t.Parallel()
	run := sandbox.PendingRun{TurnID: "t1", Steer: sandbox.RunSteer{Tellings: []sandbox.CallTelling{
		{Round: "job-0", Appended: 3, Launch: "job-0", Place: 1},
		{Round: "job-1", Appended: 2, Launch: "job-2", Place: 4},
	}}}
	told := runRow(run, sandbox.BridgeLog{Round: "job-1"}, runOpened{})["bridge_calls_told"]
	if got, _ := told.([]toldCall); !slices.Equal(got, []toldCall{{Appended: 2, Launch: "job-2"}}) {
		t.Errorf("bridge_calls_told = %+v, want the log's round's alone, call 2 by job-2", told)
	}
	wire, err := json.Marshal(told)
	if err != nil {
		t.Fatal(err)
	}
	if want := `[{"appended":2,"launch":"job-2"}]`; string(wire) != want {
		t.Errorf("bridge_calls_told is served as %s, want %s: a place and a job, never a word", wire, want)
	}
	none := runRow(run, sandbox.BridgeLog{}, runOpened{})["bridge_calls_told"]
	if got, ok := none.([]toldCall); !ok || got == nil || len(got) != 0 {
		t.Errorf("a log naming no round answers bridge_calls_told %#v, want an empty array", none)
	}
}
