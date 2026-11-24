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
	executor.CursorAgentName: {
		`{"type":"system","subtype":"init","session_id":"0b5e0000-0000-4000-8000-000000000001","model":"gpt-5"}`,
		`{"type":"result","subtype":"success","is_error":false,"result":"done","session_id":"0b5e0000-0000-4000-8000-000000000001"}`,
	},
	executor.CopilotName: {
		`{"type":"session.start","data":{"sessionId":"0b5e0000-0000-4000-8000-000000000001"}}`,
		`{"type":"result","sessionId":"0b5e0000-0000-4000-8000-000000000001","exitCode":0}`,
	},
	executor.GeminiCLIName: {
		`{"type":"init","session_id":"0b5e0000-0000-4000-8000-000000000001","model":"gemini-2.5-pro"}`,
		`{"type":"result","status":"success"}`,
	},
	executor.GrokName: {
		`{"type":"text","data":"done"}`,
		`{"type":"end","stopReason":"end_turn","sessionId":"0b5e0000-0000-4000-8000-000000000001"}`,
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
// no harness and exits 0. A run its script stopped at its cap is never one.
//
// Mutation: take the report as a success without the harness's end, have a
// runner's Decoder.Ended answer always the same, or read a cap line as a
// success, and that runner's rows fail; a runner with no lines fails the
// table.
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
			code                   string
		}{
			{"printed nothing", "", "", false, "0"},
			{"printed nothing beside a report", "", report, false, "0"},
			{"printed a line that ends nothing", lines.unended, "", false, "0"},
			{"printed a line that ends nothing beside a report", lines.unended, report, false, "0"},
			{"ended its run beside a report", lines.unended + "\n" + lines.ended, report, true, "0"},
			capCase(lines.unended, capLines[h.Name()], report),
		} {
			if c.name == "" {
				continue
			}
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
				b.Put(p.ExitCode(), c.code)

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

// capLines is, for each runner whose script stops its harness at the turn cap,
// the line it adds to the stream then.
var capLines = map[string]string{
	executor.CursorAgentName: `{"type":"crewlet.cursor.cap","cap":3}`,
}

// capCase is the case of a run its script stopped at its turn cap, beside a
// report: never a success. The zero case for a runner whose script adds no
// cap line.
//
// ITS EXIT STATUS THE INTERRUPT'S: the script stops the harness with SIGINT,
// which Cursor exits 130 on (measured on 2026.11.22-e9042b2).
func capCase(unended, capped, report string) (c struct {
	name, stream, findings string
	success                bool
	code                   string
}) {
	if capped == "" {
		return c
	}
	c.name, c.stream, c.findings, c.code = "was stopped at its turn cap beside a report", unended+"\n"+capped,
		report, "130"
	return c
}
