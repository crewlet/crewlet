package harness

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/providers/llm"
)

// Qwen Code, as the fake plays it: the server from its --mcp-config file, no id
// of its own with a call, and the record its stream-json stdout — a response's
// prose and its calls as `assistant` events of their own, each under an id of
// its own, the calls' with the request's counts and its stop reason, then a
// `user` event per result, and a `result` at the end (measured on 0.26.1; see
// the profile).
func init() {
	fakeHarnesses["qwen-code"] = fakeHarness{
		server: func() (string, []string, error) {
			raw, err := os.ReadFile(argAfter("--mcp-config")) //nolint:gosec // the fake's own configuration
			if err != nil {
				return "", nil, err
			}
			return serverIn(raw)
		},
		recorder: func(fakeScript, []byte) (fakeRecorder, error) {
			return &qwenRecorder{out: json.NewEncoder(os.Stdout)}, nil
		},
	}
}

type qwenRecorder struct {
	out   *json.Encoder
	model string
	seq   int
}

// event is one `assistant` event: a message of its own, with its own id.
func (q *qwenRecorder) event(content []any, stop any, u fakeUsage) {
	q.seq++
	id := fmt.Sprintf("m%d", q.seq)
	_ = q.out.Encode(map[string]any{"type": "assistant", "uuid": id, "message": map[string]any{
		"id": id, "type": "message", "role": "assistant", "model": q.model, "content": content,
		"stop_reason": stop,
		// The prompt whole, as the OpenAI-compatible count it passes on holds it.
		"usage": map[string]any{"input_tokens": u.Input + u.CacheRead, "output_tokens": u.Output}}})
}

func (q *qwenRecorder) begin(model string, tools []string) {
	q.model = model
	offered := []string{"web_fetch"}
	for _, tool := range tools {
		offered = append(offered, "mcp__crewlet__"+tool)
	}
	_ = q.out.Encode(map[string]any{"type": "system", "subtype": "init", "tools": offered, "model": model})
}

func (q *qwenRecorder) asked(_ int, r fakeResponse, ids []string) {
	text := []any{map[string]any{"type": "text", "text": r.Text}}
	if len(r.Calls) == 0 {
		q.event(text, nil, r.Usage)
		return
	}
	if r.Text != "" {
		// The prose first, an event of its own counting nothing.
		q.event(text, nil, fakeUsage{})
	}
	var calls []any
	for j, c := range r.Calls {
		name := "mcp__crewlet__" + c.Name
		if c.Own {
			name = c.Name
		}
		calls = append(calls, map[string]any{"type": "tool_use", "id": ids[j], "name": name, "input": orEmpty(c.Args)})
	}
	q.event(calls, "tool_use", r.Usage)
}

func (q *qwenRecorder) answered(_ int, _ fakeResponse, ids []string, results []string) {
	for j, id := range ids {
		_ = q.out.Encode(map[string]any{"type": "user", "message": map[string]any{"role": "user",
			"content": []any{map[string]any{"type": "tool_result", "tool_use_id": id, "content": results[j]}}}})
	}
}

func (q *qwenRecorder) end(script fakeScript, total fakeUsage, last string) int {
	if script.NoResult {
		return script.Exit
	}
	subtype := script.Subtype
	if subtype == "" {
		subtype = "success"
	}
	result := map[string]any{"type": "result", "subtype": subtype, "is_error": script.IsError, "result": last,
		"usage": map[string]any{"input_tokens": total.Input + total.CacheRead, "output_tokens": total.Output}}
	if script.IsError {
		result["error"] = map[string]any{"message": script.Result}
	}
	_ = q.out.Encode(result)
	return script.Exit
}

// QWEN CODE'S RUN IS READ FROM ITS STREAM, ONE REQUEST BETWEEN ONE SET OF
// RESULTS AND THE NEXT. Its stream gives a response's prose and its calls
// events of their own, under ids of their own, so no id tells one request from
// the next: the results of a response's calls end it, and the event after them
// begins the next. The host is told the CLI makes only read-only calls beside
// each other, and re-reads no list mid-run.
//
// Mutation: drop closes from the profile's user rule, and both responses are
// one request; read a request by its events' ids, and the first response's
// prose and its calls are two.
func TestQwenCodesRunIsReadFromItsStream(t *testing.T) {
	t.Parallel()
	p := fakeAs(t, "qwen-code", routeCase, nil)
	holdsTheRoute(t, p, llm.ServeOptions{
		ToolTimeout: 10 * time.Minute, ObservesRequests: true, Concurrency: llm.CallsReadOnlyAtOnce,
	}, llm.Usage{Input: 340, Output: 29})
}

// A QWEN CODE RUN ITS OWN TURN LIMIT ENDED IS AN ANSWER STOPPED AT THE CAP: exit
// 53 with no result event, the CLI's code for its session-turn limit.
//
// Mutation: drop the profile's cap, and this is a server failure.
func TestAQwenCodeRunItsTurnLimitEndedStopsAtTheCap(t *testing.T) {
	t.Parallel()
	p := fakeAs(t, "qwen-code", fakeScript{Exit: 53, NoResult: true, Stderr: "Reached max session turns for this session.\n",
		Responses: []fakeResponse{{Calls: []fakeCall{{Name: "search"}}, Usage: fakeUsage{Input: 5, Output: 5}}}}, nil)
	comp, err := p.Complete(t.Context(), phase(newTestHost(t, "search")))
	if err != nil {
		t.Fatalf("a capped run came back as a failure: %v", err)
	}
	if comp.StopReason != llm.StopCap {
		t.Errorf("stop = %q, want the cap", comp.StopReason)
	}
}

// A FAILED QWEN CODE RUN SAYS WHY BY ITS RESULT EVENT, by field, and quotes
// nothing else it printed.
//
// Mutation: drop the profile's failure events, and the error says only that
// the CLI exited 1.
func TestAFailedQwenCodeRunSaysWhyByItsResult(t *testing.T) {
	t.Parallel()
	p := fakeAs(t, "qwen-code", fakeScript{
		Responses: []fakeResponse{{Text: "posting", Calls: []fakeCall{
			{Name: "post", Args: map[string]any{"body": "ARGUMENT-MARKER"}},
		}, Usage: fakeUsage{Input: 5, Output: 5}}},
		Exit: 1, IsError: true, Subtype: "error_during_execution", Result: "[API Error: 500 the vendor is overloaded]",
	}, nil)
	host := newTestHost(t, "post")
	host.handle("post", func(context.Context, map[string]any) (string, bool) { return "RESULT-MARKER", false })
	_, err := p.Complete(t.Context(), phase(host))
	if llm.KindOf(err) != llm.KindServer {
		t.Fatalf("err = %v, want a server failure", err)
	}
	msg := err.Error()
	for _, want := range []string{"subtype=error_during_execution", "error.message=[API Error: 500 the vendor is overloaded]"} {
		if !strings.Contains(msg, want) {
			t.Errorf("the error does not say %q: %s", want, msg)
		}
	}
	for _, leaked := range []string{"ARGUMENT-MARKER", "RESULT-MARKER"} {
		if strings.Contains(msg, leaked) {
			t.Errorf("the error quotes the run's record (%q): %s", leaked, msg)
		}
	}
}
