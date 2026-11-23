package executor_test

import (
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/queue"
	"github.com/crewlet/crewlet/internal/sandbox"
	"github.com/crewlet/crewlet/internal/sandbox/executor"
)

// streamLines is, for each runner, a line of its stream that says nothing of
// how its run ended, and the lines that end it.
var streamLines = map[string]struct{ unended, ended string }{
	executor.ClaudeCodeName: {
		`{"type":"system","subtype":"init","session_id":"0b5e0000-0000-4000-8000-000000000001"}`,
		`{"type":"result","subtype":"success","is_error":false,"result":"done"}`,
	},
	executor.CodexName: {
		`{"type":"thread.started","thread_id":"01a20286-faf2-7490-a21d-7f1215273287"}`,
		`{"type":"turn.completed","usage":{"input_tokens":2,"cached_input_tokens":0,"output_tokens":1}}`,
	},
	executor.CopilotName: {
		`{"type":"session.start","data":{"sessionId":"0b5e0000-0000-4000-8000-000000000001"}}`,
		`{"type":"result","sessionId":"0b5e0000-0000-4000-8000-000000000001","exitCode":0}`,
	},
	executor.GeminiCLIName: {
		`{"type":"init","session_id":"0b5e0000-0000-4000-8000-000000000001","model":"gemini-2.5-pro"}`,
		`{"type":"result","status":"success"}`,
	},
	executor.KimiName: {
		`{"type":"turn.started","turn":{"turn_id":"t_1","status":"running","steps":[]}}`,
		`{"type":"turn.ended","turn":{"turn_id":"t_1","status":"ended","outcome":"completed","steps":[]}}` + "\n" +
			`{"type":"crewlet.end","asked":false,"code":0}`,
	},
	executor.MuseName: {
		`{"stream":{"kind":"session","id":"0b5e0000-0000-4000-8000-000000000001"},"payload_type":"run.output.delta","payload":{"text":"done"}}`,
		`{"stream":{"kind":"session","id":"0b5e0000-0000-4000-8000-000000000001"},"payload_type":"run.terminal.completed","payload":{"text":"done"}}`,
	},
	executor.OpenCodeName: {
		`{"type":"text","part":{"messageID":"msg_1","text":"done"}}`,
		`{"type":"step_finish","part":{"messageID":"msg_1","reason":"stop"}}`,
	},
	executor.HermesName: {
		`{"type": "system", "subtype": "init", "model": "", "session_id": ""}`,
		`{"type": "result", "session_id": "local-1", "exit_code": 0, "text": "done"}`,
	},
	executor.PiName: {
		`{"type":"session","version":3,"id":"33333333-3333-4333-8333-333333333333","cwd":"/home/user/workspace"}`,
		`{"type":"agent_settled","aborted":false}`,
	},
	executor.QwenCodeName: {
		`{"jsonrpc":"2.0","method":"session/update","params":{"update":{"sessionUpdate":"agent_message_chunk","content":{"type":"text","text":"done"}}}}`,
		`{"type":"crewlet_qwen_end","stop_reason":"end_turn"}`,
	},
}

// A RUN WHOSE HARNESS NEVER SAID HOW ITS RUN ENDED IS NEVER A SUCCESS, for
// every runner: a stand-in that exits 0 having printed nothing, or only a line
// that ends nothing, collects as a failure that says why, even beside a report
// in the findings file, which after the harness's own end makes a clean exit a
// success. An earlier run in the same box could leave a launcher that starts
// no harness and exits 0.
//
// Mutation: take the report as a success without the harness's end, or have a
// runner's Decoder.Ended answer always the same, and that runner's rows fail; a
// runner with no lines fails the table.
func TestARunWhoseHarnessNeverEndedItIsNeverASuccess(t *testing.T) {
	t.Parallel()
	const report = "Outcome: succeeded"
	for _, h := range executor.Registered() {
		lines, ok := streamLines[h.Name()]
		if !ok {
			t.Errorf("%s has no stream lines: give it one that ends nothing and those that end its run", h.Name())
			continue
		}
		for _, c := range []struct {
			name, stream, findings string
			success                bool
		}{
			{"printed nothing", "", "", false},
			{"printed nothing beside a report", "", report, false},
			{"printed a line that ends nothing", lines.unended, "", false},
			{"printed a line that ends nothing beside a report", lines.unended, report, false},
			{"ended its run beside a report", lines.unended + "\n" + lines.ended, report, true},
		} {
			t.Run(h.Name()+"/"+c.name, func(t *testing.T) {
				t.Parallel()
				runner := executor.New(h)
				b := box(t, runner)
				p := paths(b)
				out := h.Output(p)
				b.Put(out.Stdout, c.stream)
				if out.Result != "" && out.Result != out.Stdout {
					// The wrapper's copy of the stream's last line.
					b.Put(out.Result, c.stream[strings.LastIndex(c.stream, "\n")+1:])
				}
				if c.findings != "" {
					b.Put(p.Findings(), c.findings)
				}
				b.Put(p.ExitCode(), "0")

				res, err := runner.Collect(t.Context(), b, sandbox.RunHandle{}, queue.NewAnswerBudget(nil))
				if err != nil {
					t.Fatalf("Collect: %v", err)
				}
				switch {
				case c.success && !res.Success:
					t.Fatalf("a run its harness ended, beside a report, failed: %q", res.Error)
				case !c.success && res.Success:
					t.Fatalf("a run whose harness never said how it ended collected as a success (text %q)", res.Text)
				case !c.success && strings.TrimSpace(res.Error) == "":
					t.Fatal("a run whose harness never said how it ended failed without saying why")
				}
			})
		}
	}
}
