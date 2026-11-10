package runner_test

import (
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/agent/runner"
	"github.com/crewlet/crewlet/internal/providers/llm"
	"github.com/crewlet/crewlet/internal/sandbox"
)

// A CALL NOBODY WAITED FOR: one whose harness had given up on it, or whose
// job's session had ended, by the time it returned (runner.BridgedCall.Abandoned).
// It ran and its effect happened, while the session of the job that made it
// shows it with no outcome or a failed one — and a harness ends its own run once
// every call it made is answered or given up on, so such a call can land before
// the read a capture of that session is placed by, and before the read a
// relaunch takes the job's own copy in its box to hold.

// A CALL NOBODY WAITED FOR IS TOLD PAST EVERY SAVE POINT, UNDER THE LATE CALLS'
// WORDS. A kept session saved past its number holds every other call at or
// below the point, and the job's own copy in its box whose harness ended its
// own run every call of the round: each is told the call all the same, apart,
// under the words of calls that finished after the run that made them stopped
// waiting for them, and nothing they hold. The copy cut off, told every call of
// the round, tells it there too rather than among the round's other calls. Held
// at the point, no continuation was told it, and a run shown the call as timed
// out could make it again.
//
// Mutation: hold a call nobody waited for at a save point, or tell it among the
// calls after the save, and a case goes red.
func TestACallNobodyWaitedForIsToldPastEverySavePoint(t *testing.T) {
	t.Parallel()
	const lateHeading = "## Calls that finished after the run that made them stopped waiting for them"
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

// NO GUARD COUNTS A LOAD NOBODY WAITED FOR. A skill a call loaded whose harness
// had given up on it never reached the session that made it, whose harness was
// told the call timed out: so neither the guard of a relaunch continuing a kept
// session saved past the call, nor the one of a run continuing its job's own
// copy in its box after its harness ended its own run, holds the skill, and the
// tool it covers is refused until the skill is loaded again.
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
			basis:   runner.SessionBasis{Why: "the newest capture of the job was not kept: refused"},
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
