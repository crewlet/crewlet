package executor_test

import (
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/queue"
	"github.com/crewlet/crewlet/internal/sandbox"
	"github.com/crewlet/crewlet/internal/sandbox/executor"
)

// skipBound is the line bound the oversized cases run at, and skipPayload a
// payload past it: the cases measure where each marker sits, not the figure.
const (
	skipBound   = 64 << 10
	skipPayload = 200 << 10
)

// oversized is, for the runners whose request marker follows a large payload
// (Pi's stopReason, Qwen Code's usage, Cursor's model_call_id) and one whose
// marker leads it (Claude Code's message id), the stream of a run whose one
// request sits in a line past the bound, beside the lines that end the run.
var oversized = map[string]string{
	executor.PiName: streamOf(`{"type":"session","version":3,"id":"33333333-3333-4333-8333-333333333333","cwd":"/w"}`,
		`{"type":"message_end","message":{"role":"assistant","content":[{"type":"text","text":"`+
			strings.Repeat("x", skipPayload)+`"}],"stopReason":"stop","timestamp":1}}`,
		`{"type":"agent_settled","aborted":false}`),
	executor.QwenCodeName: streamOf(`{"jsonrpc":"2.0","method":"session/update","params":{"update":{"sessionUpdate":`+
		`"agent_message_chunk","content":{"type":"text","text":"`+strings.Repeat("x", skipPayload)+`"},"_meta":`+
		`{"usage":{"inputTokens":10,"outputTokens":2},"durationMs":3}}}}`,
		`{"type":"crewlet_qwen_end","stop_reason":"end_turn"}`),
	executor.CursorAgentName: streamOf(`{"type":"system","subtype":"init","session_id":"0b5e0000-0000-4000-8000-000000000001"}`,
		`{"type":"tool_call","subtype":"started","call_id":"c1","tool_call":{"shellToolCall":{"args":{"command":"`+
			strings.Repeat("x", skipPayload)+`"}}},"model_call_id":"m1","session_id":"s","timestamp_ms":1}`,
		`{"type":"system","subtype":"init"}`),
	executor.ClaudeCodeName: streamOf(`{"type":"system","subtype":"init","session_id":"0b5e0000-0000-4000-8000-000000000001"}`,
		`{"type":"assistant","message":{"id":"msg_1","role":"assistant","content":[{"type":"text","text":"`+
			strings.Repeat("x", skipPayload)+`"}]}}`,
		`{"type":"result","subtype":"success","is_error":false,"result":"done"}`),
}

// A REQUEST IN A LINE PAST THE BOUND IS COUNTED: the line reaches its decoder
// as its head and tail, and the decoder reads its request marker at whichever
// end its runner writes it, a marker after a large payload as much as one
// before it.
//
// Mutation: hand the decoder no tail, or no head, and a row counts none.
func TestARequestInALinePastTheBoundIsCounted(t *testing.T) {
	t.Parallel()
	for _, h := range executor.Registered() {
		stream, ok := oversized[h.Name()]
		if !ok {
			continue
		}
		t.Run(h.Name(), func(t *testing.T) {
			t.Parallel()
			dec := h.Events()
			if err := executor.EachLine(strings.NewReader(stream), dec, skipBound); err != nil {
				t.Fatal(err)
			}
			if dec.Requests() != 1 || dec.Undecided() != 0 {
				t.Fatalf("a request past the bound counts %d, %d undecided; want 1 and none", dec.Requests(),
					dec.Undecided())
			}
		})
	}
}

// A CAPPED RUN WHOSE SKIPPED LINE MAY HIDE A REQUEST IS NEVER A SUCCESS: an
// assistant message past the bound whose stop is in neither end of its line
// is undecided, and the run's cap cannot be judged on its count, whatever its
// report said. The same run uncapped is the success it reported.
//
// Mutation: treat an undecided line as no request, and the capped run
// succeeds.
func TestACappedRunWhoseSkippedLineMayHideARequestIsNeverASuccess(t *testing.T) {
	t.Parallel()
	pad := strings.Repeat("x", skipPayload)
	stream := streamOf(`{"type":"session","version":3,"id":"33333333-3333-4333-8333-333333333333","cwd":"/w"}`,
		`{"type":"message_end","message":{"role":"assistant","content":[{"type":"text","text":"`+pad+
			`"}],"stopReason":"stop","note":"`+pad+`"}}`,
		`{"type":"message_end","message":{"role":"assistant","content":[{"type":"text","text":"done"}],"stopReason":"stop"}}`,
		`{"type":"agent_settled","aborted":false}`)
	collect := func(turnCap int) sandbox.Result {
		runner := executor.Bounded(executor.New(executor.Pi{}), sandbox.MaxFailureBytes, skipBound)
		b := box(t, runner)
		p := paths(b)
		b.Put(executor.Pi{}.Output(p).Stdout, stream)
		b.Put(p.Findings(), "Outcome: succeeded")
		b.Put(p.ExitCode(), "0")
		res, err := runner.Collect(t.Context(), b, sandbox.RunHandle{TurnCap: turnCap}, queue.NewAnswerBudget(nil))
		if err != nil {
			t.Fatalf("Collect: %v", err)
		}
		return res
	}
	if res := collect(5); res.Success || !strings.Contains(res.Error, "could not be judged") {
		t.Fatalf("a capped run with an undecided line: success %v, error %q", res.Success, res.Error)
	}
	if res := collect(0); !res.Success {
		t.Fatalf("the same run uncapped failed: %q", res.Error)
	}
}
