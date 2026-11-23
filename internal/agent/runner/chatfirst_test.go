package runner_test

import (
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/agent/prompts"
	"github.com/crewlet/crewlet/internal/agent/runner"
	"github.com/crewlet/crewlet/internal/sandbox"
	"github.com/crewlet/crewlet/internal/sandbox/executor"
)

// A GEMINI CLI OR QWEN CODE CONTINUATION IS JUDGED AT ITS FIRST REQUEST, as
// their runners state it (executor.SessionTraits): both compress there, before
// the brief, at a point the box sets, so neither is held whatever its brief.
// A kept capture with a bounded count is handed both tellings, the held one
// picked and the summarized one beside it for the launch to judge in its box;
// a copy its box holds, whose record counts nothing, is told as a summary
// alone.
//
// Mutation: give either harness no traits (held whatever its brief), and a
// case goes red.
func TestAGeminiOrQwenContinuationIsJudgedAtItsFirstRequest(t *testing.T) {
	t.Parallel()
	for _, harness := range []string{executor.GeminiCLIName, executor.QwenCodeName} {
		copied := func(count *sandbox.ResumedCount) func(*runner.Resume) {
			return func(r *runner.Resume) {
				copyOf(sandbox.HarnessEndOwnRun, count)(r)
				r.Run.Harness = harness
			}
		}
		basis := keptClaude(far, false)
		basis.Harness = harness
		kept := guarded{basis: basis, bridged: []runner.BridgedCall{earlierCall}, prep: copied(far)}.relaunch(t)
		switch s := kept.Session; {
		case s == nil:
			t.Fatalf("%s: the relaunch continues no kept session", harness)
		case strings.Contains(s.Replaced, prompts.MayCompactSession):
			t.Errorf("%s: the kept path's picked telling is the summarized one", harness)
		case s.Summarized == nil || !strings.Contains(s.Summarized.Replaced, prompts.MayCompactSession):
			t.Errorf("%s: the kept path hands no summarized telling beside the held one: %+v", harness,
				s.Summarized)
		case s.Count == nil || s.Count.Tokens != far.Tokens:
			t.Errorf("%s: the kept path is judged by %+v, want the capture's count", harness, s.Count)
		}
		inPlace := guarded{basis: inBoxBasis, bridged: []runner.BridgedCall{earlierCall},
			carry: func(r *runner.Resume) { answered("use main")(r); copied(nil)(r) }}.relaunch(t)
		if c := inPlace.Continue; c == nil || !strings.Contains(c.Brief, prompts.MayCompactSession) ||
			c.Summarized != "" {
			t.Errorf("%s: the copy is continued as %+v, want the summarized telling alone", harness, c)
		}
	}
}
