package runner_test

import (
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/agent/ledger"
	"github.com/crewlet/crewlet/internal/agent/runner"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/sandbox"
)

// A CALL THE RUN PARKED ON THAT IS STILL IN FLIGHT IS TOLD AS ONE WHOSE OUTCOME
// IS NOT KNOWN. The collection can stop waiting for a parked call past its
// bound, and the round's record then holds it among the calls in flight
// (runner.Resume.Unfinished), not the log's. The relaunch finds it there: the
// run is told its outcome is not known and to check its effect, never that it
// returned or ran once, nor that its result is off the record.
//
// Mutation: withReturned reading the log's calls alone, and the run is told
// the call's result is no longer on the record, under a heading saying it
// returned.
func TestAParkedCallStillInFlightIsToldAsOneWhoseOutcomeIsNotKnown(t *testing.T) {
	t.Parallel()
	inFlight := runner.BridgedCall{Call: ledger.Call{Name: "read_page", Result: runner.UnfinishedNote, Failed: true,
		Unfinished: true}, Seq: 2}
	asked := func(boxKept bool) *runner.Asked {
		return &runner.Asked{Question: sandbox.CallWaitQuestion("read_page"),
			Answer: "Your call to read_page returned.", Called: []types.CallReturned{{Tool: "read_page", Seq: 2}},
			CallsReturned: true, SessionID: "sess-1", BoxKept: boxKept}
	}
	carry := inBoxCarry{name: "a run parked on its call", lead: "## The call you waited for",
		set: func(r *runner.Resume, boxKept bool) { r.Asked = asked(boxKept) }}
	for path, told := range map[string]func(t *testing.T) string{
		"its job's copy": func(t *testing.T) string {
			return inPlaceBrief(t, carry, true, 0, nil, func(r *runner.Resume) {
				r.Unfinished, r.HarnessEnd = []runner.BridgedCall{inFlight}, sandbox.HarnessEndOwnRun
			})
		},
		"a fresh run": func(t *testing.T) string {
			return freshRelaunch(t, runner.Resume{Unfinished: []runner.BridgedCall{inFlight}, Asked: asked(false)},
				buildOpts{}).Brief
		},
	} {
		t.Run(path, func(t *testing.T) {
			t.Parallel()
			brief := told(t)
			if !strings.Contains(brief, "Its outcome is not known") {
				t.Errorf("the run is not told the parked call's outcome is not known:\n%s", brief)
			}
			for _, not := range []string{"no longer on the run's record", "It ran once", "which returned",
				"## Your call returned", runner.UnfinishedNote} {
				if strings.Contains(brief, not) {
					t.Errorf("the parked call still in flight is told %q:\n%s", not, brief)
				}
			}
		})
	}
}
