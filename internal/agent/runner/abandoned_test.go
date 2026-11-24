package runner_test

import (
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/agent/runner"
	"github.com/crewlet/crewlet/internal/providers/llm"
	"github.com/crewlet/crewlet/internal/sandbox"
)

// A CALL NOBODY WAITED FOR (runner.BridgedCall.Abandoned): its harness had
// given up on it, or its job's session had ended, by the time it returned. Its
// effect happened, yet the session shows it unanswered or failed; and since a
// harness ends its run once every call is answered or given up on, the log can
// take such a call before the read a capture is placed by, or the read a
// relaunch holds the job's in-box copy to.

// A CALL NOBODY WAITED FOR IS TOLD PAST EVERY SAVE POINT, UNDER THE LATE CALLS'
// WORDS. A kept session saved past its number holds every other call at or
// below the point, and the in-box copy whose harness ended its own run every
// call of the round: each is told this one all the same, apart, under the late
// calls' heading. The copy cut off, told every call of the round, tells it
// there too rather than among the others. Held at the point, no continuation
// is told it, and a run shown it as timed out could make it again.
//
// Mutation: hold a call nobody waited for at a save point, or tell it among the
// calls after the save, and a case goes red.
func TestACallNobodyWaitedForIsToldPastEverySavePoint(t *testing.T) {
	t.Parallel()
	const lateHeading = "## Calls the run that made them stopped waiting for, or whose outcome is unknown"
	gaveUp := numbered("delegate", map[string]any{"task": "the worker it gave up on"}, 2, 2)
	gaveUp.Abandoned = true
	bridged := []runner.BridgedCall{
		numbered("slack_post", map[string]any{"text": "before it"}, 1, 1),
		gaveUp,
		numbered("slack_post", map[string]any{"text": "after it"}, 3, 3),
	}
	// tells fails t unless brief tells the call nobody waited for once, under
	// the late calls' heading, and the round's other calls where others says.
	tells := func(t *testing.T, brief string, others bool) {
		t.Helper()
		at := strings.Index(brief, lateHeading)
		if at < 0 || strings.Count(brief, "the worker it gave up on") != 1 ||
			!strings.Contains(brief[at:], "the worker it gave up on") {
			t.Errorf("the call nobody waited for is not told once, under the late calls' words:\n%s", brief)
		}
		if told := strings.Contains(brief, "slack_post"); told != others {
			t.Errorf("the round's other calls are told: %v, want %v:\n%s", told, others, brief)
		}
	}
	t.Run("a kept session saved past it", func(t *testing.T) {
		t.Parallel()
		tells(t, afterSaveBrief(t, 3, bridged, nil), false)
	})
	for _, carry := range inBoxCarries {
		for end, others := range map[sandbox.HarnessEnd]bool{
			sandbox.HarnessEndOwnRun: false,
			sandbox.HarnessEndCutOff: true,
		} {
			t.Run(carry.name+" in its box, its harness's end "+string(end), func(t *testing.T) {
				t.Parallel()
				tells(t, inPlaceBrief(t, carry, true, 0, nil, func(r *runner.Resume) {
					r.Bridged, r.HarnessEnd = bridged, end
				}), others)
			})
		}
	}
}

// NO GUARD COUNTS A LOAD NOBODY WAITED FOR. A skill loaded by a call its
// harness had given up on never reached the session, which was told the call
// timed out: neither a relaunch continuing a kept session saved past it nor a
// run continuing the in-box copy after its harness ended its own run holds the
// skill, so the tool it covers is refused until it is loaded again.
//
// Mutation: count the load of a call nobody waited for as any other's, and the
// covered tool is let through.
func TestNoGuardCountsALoadNobodyWaitedFor(t *testing.T) {
	t.Parallel()
	gaveUp := loadSkill(1)
	gaveUp.Abandoned = true
	for name, g := range map[string]guarded{
		"a relaunch continuing a kept session": {basis: keptBasis, bridged: []runner.BridgedCall{gaveUp}},
		"a relaunch continuing its job's copy in its box, its harness having ended its own run": {
			basis:   runner.SessionBasis{},
			bridged: []runner.BridgedCall{gaveUp}, carry: func(r *runner.Resume) {
				r.Rotated = &runner.Rotated{SessionID: "sess-1", BoxKept: true}
				r.HarnessEnd = sandbox.HarnessEndOwnRun
			}},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if g.carry != nil {
				g.state.ActiveTools = guarded{basis: g.basis}.relaunch(t).Surface.Active()
			}
			req := g.relaunch(t)
			bridged := play(t, req, run{calls: []llm.ToolCall{call(t, "slack_post", `{"text":"hi"}`)}})
			if !bridged[0].Failed {
				t.Fatalf("slack_post ran (%q) on a skill a call nobody waited for loaded", bridged[0].Result)
			}
		})
	}
}
