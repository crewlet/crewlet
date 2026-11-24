package runner_test

import (
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/agent/runner"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/sandbox"
)

// A CALL THE RUN PARKED ON IS TOLD ONCE, BY ITS LEAD, ON EVERY PATH. The park is
// answered once the call returned, which nobody waited for by then
// (runner.BridgedCall.Abandoned), so the round's log lists it among the late
// calls; the lead that carries the run on tells it, with what it returned. In
// the job's own copy, a kept session or a fresh run's brief, the call is in no
// list too: listed, it is told twice, its failure twice, the second time as a
// call the session "may show as still running".
//
// Mutation: leave the parked calls out of leadsHeld (no parkedOn), and the call
// is listed beside its lead on every path.
func TestACallTheRunParkedOnIsToldOnceByItsLead(t *testing.T) {
	t.Parallel()
	asked := func(boxKept bool) *runner.Asked {
		return &runner.Asked{Question: sandbox.CallWaitQuestion("read_page"),
			Answer: "Your call to read_page returned.", Called: []types.CallReturned{{Tool: "read_page", Seq: 2}},
			CallsReturned: true, SessionID: "sess-1", BoxKept: boxKept}
	}
	carry := inBoxCarry{name: "a run parked on its call", lead: "## Your call returned",
		set: func(r *runner.Resume, boxKept bool) { r.Asked = asked(boxKept) }}
	for name, failed := range map[string]bool{"it returned": false, "it failed": true} {
		parked := numbered("read_page", map[string]any{"url": "the parked page"}, 2, 2)
		parked.Abandoned, parked.Failed, parked.Result = true, failed, "what the parked call returned"
		bridged := []runner.BridgedCall{numbered("slack_post", map[string]any{"text": "a post"}, 1, 1), parked}
		briefs := map[string]func(t *testing.T) string{
			"its job's copy, its harness's own run ended": func(t *testing.T) string {
				return inPlaceBrief(t, carry, true, 0, nil, func(r *runner.Resume) {
					r.Bridged, r.HarnessEnd = bridged, sandbox.HarnessEndOwnRun
				})
			},
			"its job's copy, cut off": func(t *testing.T) string {
				return inPlaceBrief(t, carry, true, 0, nil, func(r *runner.Resume) {
					r.Bridged, r.HarnessEnd = bridged, sandbox.HarnessEndCutOff
				})
			},
			"a kept session": func(t *testing.T) string {
				again := relaunchOf(t, runner.Resume{Bridged: bridged, Asked: asked(true)},
					&probingLauncher{basis: keptBasis}, buildOpts{})
				if again.Session == nil {
					t.Fatal("the relaunch continues no kept session")
				}
				return again.Session.Brief
			},
			"a fresh run": func(t *testing.T) string {
				brief := freshRelaunch(t, runner.Resume{Bridged: bridged, Asked: asked(false)}, buildOpts{}).Brief
				if !strings.Contains(brief, `"text":"a post"`) {
					t.Errorf("the fresh run is not told the round's other call:\n%s", brief)
				}
				return brief
			},
		}
		for path, brief := range briefs {
			t.Run(name+", "+path, func(t *testing.T) {
				t.Parallel()
				told := brief(t)
				if strings.Count(told, "what the parked call returned") != 1 ||
					strings.Contains(told, "the parked page") {
					t.Errorf("the parked call is not told once, by its lead alone:\n%s", told)
				}
			})
		}
	}
}
