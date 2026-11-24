package executor_test

import (
	"fmt"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/queue"
	"github.com/crewlet/crewlet/internal/sandbox"
	"github.com/crewlet/crewlet/internal/sandbox/executor"
)

// requestStreams is, for each runner, the whole stream of a run its harness
// ended having made n model requests, each shown where its runner's count
// reads it ([executor.Decoder.Requests]): its own events, or the records its
// script appends once the harness exits.
var requestStreams = map[string]func(n int) string{
	executor.ClaudeCodeName: func(n int) string {
		return streamOf(`{"type":"system","subtype":"init","session_id":"0b5e0000-0000-4000-8000-000000000001"}`,
			each(n, `{"type":"assistant","message":{"id":"msg_%d","role":"assistant","content":[{"type":"text","text":"step"}],"usage":{"input_tokens":1,"output_tokens":1}}}`),
			`{"type":"result","subtype":"success","is_error":false,"result":"done"}`)
	},
	executor.CodexName: func(n int) string {
		return streamOf(`{"type":"thread.started","thread_id":"01a20286-faf2-7490-a21d-7f1215273287"}`,
			`{"type":"turn.completed","usage":{"input_tokens":2,"cached_input_tokens":0,"output_tokens":1}}`,
			each(n, `{"type":"crewlet.rollout","line":{"type":"event_msg","payload":{"type":"token_count","info":{"last_token_usage":{"input_tokens":%d,"output_tokens":1,"total_tokens":3}}}}}`))
	},
	executor.CursorAgentName: func(n int) string {
		// THE ANSWER IS A RESPONSE TOO, one that calls no tool.
		return streamOf(`{"type":"system","subtype":"init","session_id":"0b5e0000-0000-4000-8000-000000000001","model":"gpt-5"}`,
			each(n-1, `{"type":"tool_call","subtype":"started","call_id":"call_%[1]d","model_call_id":"model-%[1]d","tool_call":{}}`),
			`{"type":"result","subtype":"success","is_error":false,"result":"done","session_id":"0b5e0000-0000-4000-8000-000000000001"}`)
	},
	executor.CopilotName: func(n int) string {
		return streamOf(`{"type":"session.start","data":{"sessionId":"0b5e0000-0000-4000-8000-000000000001"}}`,
			`{"type":"result","sessionId":"0b5e0000-0000-4000-8000-000000000001","exitCode":0}`,
			`{"type":"crewlet.usage"}`,
			each(n, `{"type":"session.usage_record","data":{"usage":{"inputTokens":%d,"outputTokens":1}}}`))
	},
	executor.GeminiCLIName: func(n int) string {
		return streamOf(`{"type":"init","session_id":"0b5e0000-0000-4000-8000-000000000001","model":"gemini-2.5-pro"}`,
			`{"type":"result","status":"success"}`,
			fmt.Sprintf(`{"type":"crewlet_gemini_usage","tokens":{"input":1,"output":1},"requests":%d}`, n))
	},
	executor.GrokName: func(n int) string {
		return streamOf(each(n, `{"type":"usage","usage":{"input_tokens":%d,"output_tokens":1}}`),
			`{"type":"text","data":"done"}`,
			`{"type":"end","stopReason":"end_turn","sessionId":"0b5e0000-0000-4000-8000-000000000001"}`)
	},
	executor.KimiName: func(n int) string {
		return streamOf(`{"type":"turn.started","turn":{"turn_id":"t_1","status":"running","steps":[]}}`,
			each(n, `{"type":"turn.updated","turn_id":"t_1","step":{"index":%d}}`),
			`{"type":"turn.ended","turn":{"turn_id":"t_1","status":"ended","outcome":"completed","steps":[]}}`,
			`{"type":"crewlet.end","asked":false,"code":0}`)
	},
	executor.MuseName: func(n int) string {
		return streamOf(
			fmt.Sprintf(`{"stream":{"kind":"exec.usage","id":"0b5e0000-0000-4000-8000-000000000002"},"payload_type":"run.usage","payload":{"calls":%d,"models":[]}}`, n),
			`{"stream":{"kind":"session","id":"0b5e0000-0000-4000-8000-000000000001"},"payload_type":"run.terminal.completed","payload":{"text":"done"}}`)
	},
	executor.OpenCodeName: func(n int) string {
		return streamOf(
			each(n-1, `{"type":"step_start","part":{"messageID":"msg_%[1]d"}}`+"\n"+
				`{"type":"step_finish","part":{"messageID":"msg_%[1]d","reason":"tool-calls"}}`),
			fmt.Sprintf(`{"type":"step_start","part":{"messageID":"msg_%d"}}`, n),
			fmt.Sprintf(`{"type":"text","part":{"messageID":"msg_%d","text":"done"}}`, n),
			fmt.Sprintf(`{"type":"step_finish","part":{"messageID":"msg_%d","reason":"stop"}}`, n))
	},
	executor.HermesName: func(n int) string {
		return streamOf(`{"type": "system", "subtype": "init", "model": "", "session_id": ""}`,
			`{"type": "result", "session_id": "local-1", "exit_code": 0, "text": "done"}`,
			each(n, `{"type":"crewlet.hermes_request","line":{"turn_id":"t_%d","finish_reason":"stop"}}`))
	},
	executor.PiName: func(n int) string {
		return streamOf(`{"type":"session","version":3,"id":"33333333-3333-4333-8333-333333333333","cwd":"/home/user/workspace"}`,
			each(n-1, `{"type":"message_end","message":{"role":"assistant","content":[],"stopReason":"toolUse","timestamp":%d}}`),
			`{"type":"message_end","message":{"role":"assistant","content":[{"type":"text","text":"done"}],"stopReason":"stop"}}`,
			`{"type":"agent_settled","aborted":false}`)
	},
	executor.QwenCodeName: func(n int) string {
		return streamOf(each(n, `{"jsonrpc":"2.0","method":"session/update","params":{"update":{"sessionUpdate":"agent_message_chunk","content":{"type":"text","text":"step %d"},"_meta":{"usage":{"inputTokens":1,"outputTokens":1}}}}}`),
			`{"type":"crewlet_qwen_end","stop_reason":"end_turn"}`)
	},
}

// streamOf joins a stream's lines, dropping empty ones.
func streamOf(parts ...string) string {
	var kept []string
	for _, p := range parts {
		if p != "" {
			kept = append(kept, p)
		}
	}
	return strings.Join(kept, "\n")
}

// each is format, given each of 1..n, a line apiece.
func each(n int, format string) string {
	out := make([]string, 0, n)
	for i := 1; i <= n; i++ {
		out = append(out, fmt.Sprintf(format, i))
	}
	return strings.Join(out, "\n")
}

// collectCapped is what runner h collects of stream, beside a report and a
// clean exit, from a job its launch handed turnCap.
func collectCapped(t *testing.T, h executor.Harness, stream string, turnCap int) sandbox.Result {
	t.Helper()
	runner := executor.New(h)
	b := box(t, runner)
	p := paths(b)
	out := h.Output(p)
	b.Put(out.Stdout, stream)
	if out.Result != "" && out.Result != out.Stdout {
		b.Put(out.Result, stream[strings.LastIndex(stream, "\n")+1:])
	}
	b.Put(p.Findings(), "Outcome: succeeded")
	b.Put(p.ExitCode(), "0")
	res, err := runner.Collect(t.Context(), b, sandbox.RunHandle{TurnCap: turnCap}, queue.NewAnswerBudget(nil))
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	return res
}

// A RUN PAST ITS CAP NEVER COLLECTS AS A SUCCESS, for every runner: the
// engine's cap over the requests its stream shows, decided once in the
// collection, whatever the run reported and whatever held the cap in its box.
// A run at its cap, and an uncapped one, are the success they reported. A
// runner with no stream fails the sweep, so none ships without its count.
//
// Mutation: drop the verdict, compare with >= instead of >, count one
// runner's requests as none, or let a report's success win after the verdict,
// and that runner's rows fail; a runner with no row fails the table.
func TestARunPastItsTurnCapNeverCollectsAsASuccess(t *testing.T) {
	t.Parallel()
	const made = 3
	for _, h := range executor.Registered() {
		stream, ok := requestStreams[h.Name()]
		if !ok {
			t.Errorf("%s has no request stream: give it one that shows each model request where its count reads it",
				h.Name())
			continue
		}
		t.Run(h.Name(), func(t *testing.T) {
			t.Parallel()
			if dec := decoded(h, stream(made)); dec.Requests() != made {
				t.Fatalf("the stream of %d requests counts %d", made, dec.Requests())
			}
			for _, within := range []int{0, made, made + 1} {
				if res := collectCapped(t, h, stream(made), within); !res.Success || res.TurnCapExceeded {
					t.Fatalf("%d requests under a cap of %d: success %v, exceeded %v, error %q", made, within,
						res.Success, res.TurnCapExceeded, res.Error)
				}
			}
			res := collectCapped(t, h, stream(made), made-1)
			want := fmt.Sprintf("the turn cap of %d was exceeded: the run made %d model requests", made-1, made)
			if res.Success || !res.TurnCapExceeded || res.RoundCapped || !strings.HasPrefix(res.Error, want) {
				t.Fatalf("%d requests past a cap of %d: success %v, exceeded %v, capped %v, error %q; want %q first",
					made, made-1, res.Success, res.TurnCapExceeded, res.RoundCapped, res.Error, want)
			}
		})
	}
}

// A STREAM CUT SHORT NEVER COLLECTS AS A SUCCESS, for every runner, whether
// it was cut at a line or inside one: a harness that never said how its run
// ended did not finish it, however few requests the stream still shows and
// whatever report it left.
//
// Mutation: read a report as a success without the harness's end, or have one
// runner's Ended hold on a cut stream, and that runner's rows fail.
func TestACappedRunWhoseStreamWasCutShortNeverCollectsAsASuccess(t *testing.T) {
	t.Parallel()
	const made = 3
	for _, h := range executor.Registered() {
		stream, ok := requestStreams[h.Name()]
		if !ok {
			continue // TestARunPastItsTurnCapNeverCollectsAsASuccess names it
		}
		whole := stream(made)
		first := whole[:strings.Index(whole, "\n")]
		for name, cut := range map[string]string{
			"at its first line":     first,
			"inside its first line": first[:len(first)/2],
		} {
			t.Run(h.Name()+"/"+name, func(t *testing.T) {
				t.Parallel()
				if res := collectCapped(t, h, cut, made); res.Success {
					t.Fatalf("a stream cut %s collected as a success: %q", name, res.Text)
				}
			})
		}
	}
}

// decoded is stream read whole through a fresh decoder of h's.
func decoded(h executor.Harness, stream string) executor.Decoder {
	dec := h.Events()
	for line := range strings.Lines(stream) {
		dec.Line([]byte(strings.TrimRight(line, "\n")))
	}
	return dec
}
