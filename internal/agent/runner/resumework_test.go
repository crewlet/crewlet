package runner_test

import (
	"context"
	"errors"
	"testing"

	"github.com/crewlet/crewlet/internal/agent/execstate"
	"github.com/crewlet/crewlet/internal/agent/ledger"
	"github.com/crewlet/crewlet/internal/agent/phase"
	"github.com/crewlet/crewlet/internal/agent/runner"
)

// A RESUMED PHASE'S RECORD IS WHOLE, AND SAYS WHICH OF IT IS THIS ATTEMPT'S.
// The run's bridged calls, made before this segment, lead the record because
// the delivery check and ledger read the whole phase, and are marked an
// earlier attempt's ([turn.Work.Inherited]) because a retry, collecting or
// relaunching the same run, never makes them again. Covered: a collection, a
// continuation that parked again, and one whose relaunch was refused.
//
// Mutation: leave Inherited at zero on any of them, and the run's post reads as
// this attempt's.
func TestAResumedRecordMarksTheParkedRoundsCallsAsInherited(t *testing.T) {
	t.Parallel()
	bridged := []runner.BridgedCall{
		{Call: ledger.Call{Name: "slack_post", Result: "posted"}},
		{Call: ledger.Call{Name: runner.SubmitWorkTool, Args: map[string]any{
			"outcome": "delivered", "summary": "posted", "deliveries": []any{"slack_post"},
		}}},
	}
	asked := &runner.Asked{Question: "which channel?", Answer: "#eng", BoxKept: true,
		SessionID: "sess-ask"}
	for name, tc := range map[string]struct {
		asked     *runner.Asked
		suspended bool
		fails     bool
	}{
		"a collection":                          {},
		"a continuation that parked again":      {asked: asked, suspended: true},
		"a continuation whose relaunch refused": {asked: asked, fails: true},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			launcher := &playLauncher{}
			if tc.fails {
				launcher.err = errors.New("every executor member is sitting out")
			}
			r, _ := buildWith(t, []phase.Entry{{Key: "default", Provider: &scriptedProvider{}}},
				buildOpts{launcher: launcher, resume: &runner.Resume{
					State:   execstate.State{Version: execstate.Version, Round: 1},
					Answer:  "posted the summary",
					Bridged: bridged,
					Asked:   tc.asked,
				}})
			work, _, err := r.Resume(context.Background(), nil)
			if (err != nil) != tc.fails {
				t.Fatalf("Resume: %v, want it to fail %v", err, tc.fails)
			}
			if work.Suspended != tc.suspended {
				t.Fatalf("suspended = %v, want %v", work.Suspended, tc.suspended)
			}
			if len(work.Calls) != len(bridged) || work.Inherited != len(bridged) {
				t.Fatalf("resumed work holds %d calls, %d of them inherited; want the run's %d, "+
					"every one inherited", len(work.Calls), work.Inherited, len(bridged))
			}
		})
	}
}
