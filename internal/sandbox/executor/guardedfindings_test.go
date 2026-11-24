package executor_test

import (
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/queue"
	"github.com/crewlet/crewlet/internal/sandbox"
	"github.com/crewlet/crewlet/internal/sandbox/executor"
)

// A RUN ITS BRIDGE GUARD STOPPED STAYS FAILED WHATEVER IT REPORTED. A findings
// file the run left beside an ended stream and exit 0 reads as its success,
// but a run whose guard stopped it (another server served, or could have
// served, a call under a name of Crewlet's own server's) worked without its
// seat's tools, so its report is no result: Gemini CLI's hook line, Claude
// Code's guard line wrapping the harness's result, and Copilot's extension
// that never ran beside the run's guard mark. The same streams without the
// guard's line succeed on the report.
//
// Mutation: let the findings file decide whatever the decoder said, or set
// nothing on any one runner's guard line, and that runner's row is red.
func TestARunItsBridgeGuardStoppedStaysFailedWhateverItReported(t *testing.T) {
	t.Parallel()
	claudeResult := streamLines[executor.ClaudeCodeName].ended
	for _, tc := range []struct {
		harness       executor.Harness
		stream, guard string
	}{
		{executor.GeminiCLI{}, streamLines[executor.GeminiCLIName].unended + "\n" + streamLines[executor.GeminiCLIName].ended,
			`{"type":"crewlet_gemini_unbridged","message":"Gemini CLI would run \"mcp_crewlet_x\" on the server \"mcp\""}`},
		{executor.ClaudeCode{}, streamLines[executor.ClaudeCodeName].unended + "\n" + claudeResult,
			`{"type":"crewlet_claude_unbridged","source":"agent","agent":"shadow","result":` + claudeResult + `}`},
		{executor.Copilot{}, streamLines[executor.CopilotName].unended + "\n" + streamLines[executor.CopilotName].ended,
			`{"type":"crewlet.guard"}`},
	} {
		for _, guarded := range []bool{false, true} {
			t.Run(tc.harness.Name()+map[bool]string{false: "/unguarded", true: "/guarded"}[guarded], func(t *testing.T) {
				t.Parallel()
				runner := executor.New(tc.harness)
				b := box(t, runner)
				p := paths(b)
				out := tc.harness.Output(p)
				stream, last := tc.stream, tc.stream[strings.LastIndex(tc.stream, "\n")+1:]
				if guarded {
					if tc.harness.Name() == executor.CopilotName {
						stream = tc.guard + "\n" + stream
					} else {
						stream, last = stream+"\n"+tc.guard, tc.guard
					}
				}
				b.Put(out.Stdout, stream)
				if out.Result != "" && out.Result != out.Stdout {
					b.Put(out.Result, last)
				}
				b.Put(p.Findings(), "Outcome: succeeded")
				b.Put(p.ExitCode(), "0")
				res, err := runner.Collect(t.Context(), b, sandbox.RunHandle{}, queue.NewAnswerBudget(nil))
				if err != nil {
					t.Fatalf("Collect: %v", err)
				}
				if res.Success == guarded {
					t.Errorf("guarded %v: success %v (%q), want %v", guarded, res.Success, res.Error, !guarded)
				}
			})
		}
	}
}
