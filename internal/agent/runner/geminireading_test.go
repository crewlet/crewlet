package runner_test

import (
	"strconv"
	"testing"

	"github.com/crewlet/crewlet/internal/agent/runner"
	"github.com/crewlet/crewlet/internal/sandbox"
	"github.com/crewlet/crewlet/internal/sandbox/executor"
)

// A GEMINI CLI COUNT THAT MAY MASK ASKS NO READING: a cut-off Gemini CLI
// capture whose count is marked masked (as masked.run0's is, whose resume
// masked 15 of the 20 parts a reading would count) has no bound, so its first
// request compacts whatever the brief and no reading is asked; the same
// capture under a bounded count is asked once, each call keyed by its place.
//
// Mutation: ask a reading under compactsWhatever, or key Gemini CLI's calls
// by anything but their place, and a case goes red.
func TestAGeminiCountThatMayMaskAsksNoReading(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		count *sandbox.ResumedCount
		asks  int
	}{
		"a count that may mask": {&sandbox.ResumedCount{Mark: sandbox.CountMasked}, 0},
		"a bounded count":       {far, 1},
	} {
		basis := openCodeKept
		basis.Harness, basis.Release, basis.Count, basis.CountProvider = executor.GeminiCLIName, "0.70.0",
			tc.count, ""
		reads := &reading{byContent: true}
		lostRelaunchWith(t, buildOpts{reads: reads}, basis, nil, runner.Resume{BasisRestart: new(""),
			Bridged: []runner.BridgedCall{
				withArgsText(numbered("slack_post", map[string]any{"channel": "C1", "text": "at the point"}, 1, 1)),
				withArgsText(numbered("slack_post", map[string]any{"channel": "C1", "text": "after it"}, 2, 2)),
			}})
		asks := reads.asked()
		if len(asks) != tc.asks {
			t.Fatalf("%s: a Gemini CLI relaunch was read %d times; want %d", name, len(asks), tc.asks)
		}
		for _, ask := range asks {
			for _, c := range ask.Calls {
				if c.Asked && c.ID != strconv.Itoa(c.Appended) || c.Seq == 0 || !c.Args.Whole {
					t.Errorf("%s: a Gemini CLI call is asked as %+v; want it keyed by its place, with its "+
						"admission place and its arguments' key", name, c)
				}
			}
		}
	}
}
