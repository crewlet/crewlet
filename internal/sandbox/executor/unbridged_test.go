package executor_test

import (
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/queue"
	"github.com/crewlet/crewlet/internal/sandbox"
	"github.com/crewlet/crewlet/internal/sandbox/executor"
)

// A RUN WHOSE HARNESS NEVER CONNECTED ITS BRIDGE IS NEVER A SUCCESS, for every
// runner: one its harness ended with a pure text answer, beside a report or not,
// collects as a failure naming the bridge where its record holds no connect
// (sandbox.RunHandle.BridgeUnconnected), and as its own result where it does.
// Every runner, since every harness was measured listing the bridge's tools at
// startup before its first model request (internal/api/mcpbridge's package doc).
// A run ended on request says that, never the bridge.
//
// Mutation: drop the check, apply it to one runner only, or leave the failure
// unsaid, and that runner's rows fail; a runner with no stream lines (shared with
// noresult_test.go) fails the table.
func TestARunWhoseHarnessNeverConnectedItsBridgeIsNeverASuccess(t *testing.T) {
	t.Parallel()
	const report = "Outcome: succeeded"
	for _, h := range executor.Registered() {
		lines, ok := streamLines[h.Name()]
		if !ok {
			t.Errorf("%s has no stream lines: give it one that ends nothing and those that end its run", h.Name())
			continue
		}
		for _, c := range []struct {
			name, findings, ended string
			unconnected           bool
			success               bool
		}{
			{name: "connected, a text answer beside a report", findings: report, success: true},
			{name: "never connected, a text answer beside a report", findings: report, unconnected: true},
			{name: "never connected, a text answer alone", unconnected: true},
			{name: "never connected, ended on request", findings: report, ended: string(sandbox.JobEndAsked), unconnected: true},
		} {
			t.Run(h.Name()+"/"+c.name, func(t *testing.T) {
				t.Parallel()
				runner := executor.New(h)
				b := box(t, runner)
				p := paths(b)
				out := h.Output(p)
				b.Put(out.Stdout, lines.unended+"\n"+lines.ended)
				if out.Result != "" && out.Result != out.Stdout {
					// The wrapper's copy of the stream's last line.
					b.Put(out.Result, lines.ended[strings.LastIndex(lines.ended, "\n")+1:])
				}
				if c.findings != "" {
					b.Put(p.Findings(), c.findings)
				}
				if c.ended != "" {
					b.Put(p.JobEnded(), c.ended)
				}
				b.Put(p.ExitCode(), "0")

				res, err := runner.Collect(t.Context(), b,
					sandbox.RunHandle{BridgeUnconnected: c.unconnected}, queue.NewAnswerBudget(nil))
				if err != nil {
					t.Fatalf("Collect: %v", err)
				}
				names := strings.Contains(res.Error, "never connected its Crewlet bridge")
				switch {
				case c.success && !res.Success:
					t.Fatalf("a run whose harness connected its bridge failed: %q", res.Error)
				case !c.success && res.Success:
					t.Fatalf("a run whose harness never connected its bridge collected as a success (text %q)", res.Text)
				case c.unconnected && c.ended == "" && !names:
					t.Fatalf("a run whose harness never connected its bridge failed without saying so: %q", res.Error)
				case (!c.unconnected || c.ended != "") && names:
					t.Fatalf("the failure blames the bridge where it says nothing of it: %q", res.Error)
				}
			})
		}
	}
}
