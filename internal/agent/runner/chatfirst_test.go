package runner_test

import (
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/agent/prompts"
	"github.com/crewlet/crewlet/internal/agent/runner"
	"github.com/crewlet/crewlet/internal/sandbox"
	"github.com/crewlet/crewlet/internal/sandbox/executor"
)

// A GEMINI CLI OR QWEN CODE CONTINUATION IS JUDGED AT ITS FIRST REQUEST
// (executor.SessionTraits), in the box, never held whatever its brief. A kept
// capture taken whole in its box is judged by the worse of its count and its
// copy's, which its collection counts as the take does: both bounded, it gets
// both tellings, the held one picked; a copy counted nothing (nil) or unread
// (its harness cut off) gets the summarized telling alone. A copy continued in
// its box is judged by its own count the same way.
//
// Mutations, each red: give either harness no traits (held whatever its
// brief); judge a kept capture by its own count alone; hand the held telling
// for a copy with no bound.
func TestAGeminiOrQwenContinuationIsJudgedAtItsFirstRequest(t *testing.T) {
	t.Parallel()
	unread := &sandbox.ResumedCount{Mark: sandbox.CountUnread}
	for _, harness := range []string{executor.GeminiCLIName, executor.QwenCodeName} {
		copied := func(count *sandbox.ResumedCount) func(*runner.Resume) {
			return func(r *runner.Resume) {
				copyOf(sandbox.HarnessEndOwnRun, count)(r)
				r.Run.Harness = harness
			}
		}
		basis := keptClaude(far, false)
		basis.Harness = harness
		for _, tc := range []struct {
			name         string
			copy, judged *sandbox.ResumedCount
		}{
			{"its copy counted as the capture", far, far},
			{"its copy counted above the capture", near, near},
			{"its copy counted nothing", nil, nil},
			{"its copy unread", unread, nil},
		} {
			kept := guarded{basis: basis, bridged: []runner.BridgedCall{earlierCall}, prep: copied(tc.copy)}.relaunch(t)
			s := kept.Session
			switch {
			case s == nil:
				t.Fatalf("%s, %s: the relaunch continues no kept session", harness, tc.name)
			case tc.judged == nil:
				if !strings.Contains(s.Replaced, prompts.MayCompactSession) || s.Summarized != nil {
					t.Errorf("%s, %s: the kept path is handed %q beside %+v, want the summarized telling "+
						"alone", harness, tc.name, s.Replaced, s.Summarized)
				}
			case strings.Contains(s.Replaced, prompts.MayCompactSession):
				t.Errorf("%s, %s: the kept path's picked telling is the summarized one", harness, tc.name)
			case s.Summarized == nil || !strings.Contains(s.Summarized.Replaced, prompts.MayCompactSession):
				t.Errorf("%s, %s: the kept path hands no summarized telling beside the held one: %+v", harness,
					tc.name, s.Summarized)
			case s.Count == nil || s.Count.Tokens != tc.judged.Tokens:
				t.Errorf("%s, %s: the kept path is judged by %+v, want %d", harness, tc.name, s.Count,
					tc.judged.Tokens)
			}
		}
		for _, tc := range []struct {
			name  string
			count *sandbox.ResumedCount
			held  bool
		}{
			{"counted", far, true},
			{"counted nothing", nil, false},
			{"unread", unread, false},
		} {
			inPlace := guarded{basis: inBoxBasis, bridged: []runner.BridgedCall{earlierCall},
				carry: func(r *runner.Resume) { answered("use main")(r); copied(tc.count)(r) }}.relaunch(t)
			c := inPlace.Continue
			switch {
			case c == nil:
				t.Fatalf("%s, a copy %s: the relaunch continues no copy", harness, tc.name)
			case !tc.held && (!strings.Contains(c.Brief, prompts.MayCompactSession) || c.Summarized != ""):
				t.Errorf("%s, a copy %s: continued as %+v, want the summarized telling alone", harness, tc.name, c)
			case tc.held && (strings.Contains(c.Brief, prompts.MayCompactSession) ||
				!strings.Contains(c.Summarized, prompts.MayCompactSession) || c.Count == nil ||
				c.Count.Tokens != tc.count.Tokens):
				t.Errorf("%s, a copy %s: continued as %+v, want the held telling, the summarized one beside "+
					"it, judged by its count", harness, tc.name, c)
			}
		}
	}
}
