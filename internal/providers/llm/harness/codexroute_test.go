package harness

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/crewlet/crewlet/internal/providers/llm"
	"github.com/crewlet/crewlet/internal/providers/llm/harness/harnessprofile"
)

// Codex, as the fake plays it: the server from its `-c mcp_servers.crewlet.*`
// arguments, each call's id in `_meta` as `callId`, the record in the rollout
// file under CODEX_HOME — response items, then a token_usage_record ending
// each response — and `exec --json` events on stdout (measured on 0.164.0; see
// the profile).
func init() {
	fakeHarnesses["codex"] = fakeHarness{
		server: func() (string, []string, error) {
			rawCommand, ok := argAfterPrefix("mcp_servers.crewlet.command=")
			rawArgs, okArgs := argAfterPrefix("mcp_servers.crewlet.args=")
			if !ok || !okArgs {
				return "", nil, fmt.Errorf("no -c mcp_servers.crewlet.command and .args")
			}
			var command string
			var args []string
			if err := json.Unmarshal([]byte(rawCommand), &command); err != nil {
				return "", nil, err
			}
			if err := json.Unmarshal([]byte(rawArgs), &args); err != nil {
				return "", nil, err
			}
			return command, args, nil
		},
		meta:     func(id string) sdk.Meta { return sdk.Meta{"callId": id} },
		recorder: newCodexRecorder,
	}
}

type codexRecorder struct {
	out, rollout *json.Encoder
	thread, cwd  string
}

func newCodexRecorder(script fakeScript, _ []byte) (fakeRecorder, error) {
	dir := filepath.Join(os.Getenv("CODEX_HOME"), "sessions", "2026", "10", "25")
	cwd, _ := os.Getwd()
	if script.ForeignRecord {
		// Another run of the seat's, begun a moment earlier: sorted first.
		other, err := recordFile(filepath.Join(dir, "rollout-2026-10-25T00-00-00-another-run.jsonl"))
		if err != nil {
			return nil, err
		}
		_ = other.Encode(map[string]any{"type": "session_meta", "payload": map[string]any{
			"id": "another-run", "cwd": "/somewhere/else"}})
		_ = other.Encode(codexItem(map[string]any{"type": "message", "role": "assistant",
			"content": []any{map[string]any{"type": "output_text", "text": "FOREIGN"}}}))
		_ = other.Encode(codexUsage(fakeUsage{Input: 999, Output: 999}))
	}
	thread := fmt.Sprintf("thread-%d", os.Getpid())
	rollout, err := recordFile(filepath.Join(dir, "rollout-2026-10-25T00-00-01-"+thread+".jsonl"))
	if err != nil {
		return nil, err
	}
	return &codexRecorder{out: json.NewEncoder(os.Stdout), rollout: rollout, thread: thread, cwd: cwd}, nil
}

func codexItem(payload map[string]any) map[string]any {
	return map[string]any{"type": "response_item", "payload": payload}
}

func codexUsage(u fakeUsage) map[string]any {
	return map[string]any{"type": "token_usage_record", "payload": map[string]any{"usage": map[string]any{
		"input_tokens": u.Input + u.CacheRead, "cached_input_tokens": u.CacheRead,
		"cache_write_input_tokens": u.CacheWrite, "output_tokens": u.Output}}}
}

func (c *codexRecorder) begin(string, []string) {
	_ = c.out.Encode(map[string]any{"type": "thread.started", "thread_id": c.thread})
	_ = c.out.Encode(map[string]any{"type": "turn.started"})
	_ = c.rollout.Encode(map[string]any{"type": "session_meta", "payload": map[string]any{
		"id": c.thread, "cwd": c.cwd}})
}

func (c *codexRecorder) asked(_ int, r fakeResponse, ids []string) {
	if r.Text != "" {
		_ = c.rollout.Encode(codexItem(map[string]any{"type": "message", "role": "assistant",
			"content": []any{map[string]any{"type": "output_text", "text": r.Text}}}))
		_ = c.out.Encode(map[string]any{"type": "item.completed",
			"item": map[string]any{"type": "agent_message", "text": r.Text}})
	}
	for j, call := range r.Calls {
		args, _ := json.Marshal(orEmpty(call.Args))
		item := map[string]any{"type": "function_call", "name": call.Name, "arguments": string(args), "call_id": ids[j]}
		if !call.Own {
			item["namespace"] = "mcp__crewlet"
		}
		_ = c.rollout.Encode(codexItem(item))
	}
	if !r.StopEarly {
		_ = c.rollout.Encode(codexUsage(r.Usage))
	}
}

func (c *codexRecorder) answered(_ int, _ fakeResponse, ids []string, results []string) {
	for j, id := range ids {
		_ = c.rollout.Encode(codexItem(map[string]any{"type": "function_call_output", "call_id": id,
			"output": results[j]}))
	}
}

func (c *codexRecorder) end(script fakeScript, total fakeUsage, _ string) int {
	if script.Exit != 0 {
		_ = c.out.Encode(map[string]any{"type": "error", "message": script.Result})
		_ = c.out.Encode(map[string]any{"type": "turn.failed", "error": map[string]any{"message": script.Result}})
		return script.Exit
	}
	_ = c.out.Encode(map[string]any{"type": "turn.completed", "usage": map[string]any{
		"input_tokens": total.Input + total.CacheRead, "cached_input_tokens": total.CacheRead,
		"cache_write_input_tokens": total.CacheWrite, "output_tokens": total.Output}})
	return 0
}

// CODEX'S RUN IS READ FROM ITS ROLLOUT, AND ONLY ITS OWN. `exec --json` prints
// no line that tells one model request from the next, so each request is read
// from the rollout file codex keeps under the seat's home — its prose and calls
// as they arrive, its counts where it ends — and each call is joined to its
// request by the id codex hands the server with it. That home is every
// concurrent call's of the seat, and codex names the file after a thread id
// it mints itself: the run's own is the one whose first line names the call's
// directory, and a concurrent run's rollout beside it is never read.
//
// Mutation: drop the profile's owner, and the other run's FOREIGN response and
// its 999 tokens are this run's; drop closes from the usage rule, and the two
// responses are read as one request.
func TestCodexsRunIsReadFromItsOwnRollout(t *testing.T) {
	t.Parallel()
	script := routeCase
	script.ForeignRecord = true
	p := fakeAs(t, "codex", script, nil)
	host := holdsTheRoute(t, p, llm.ServeOptions{
		ToolTimeout: 10 * time.Minute, ObservesRequests: true, Concurrency: llm.CallsAtOnce, CallIDMeta: "callId",
	}, llm.Usage{Input: 340, Output: 29, CacheRead: 100})
	_, calls, _, _ := host.snapshot()
	turns := host.latestTurns()
	if id, _ := calls[0].Meta["callId"].(string); id == "" || id != turns[0].Calls[0].ID {
		t.Errorf("the call carried callId %q and the rollout names %q — they must join",
			calls[0].Meta["callId"], turns[0].Calls[0].ID)
	}
	for _, turn := range turns {
		if strings.Contains(turn.Text, "FOREIGN") {
			t.Errorf("another run's rollout was read as this one's: %+v", turn)
		}
	}
}

// A RUN WITH TOOLS KEEPS EVERY FLAG THAT CONFINES A TEXT FUNCTION, and both
// keep the home's config.toml out. The route's argv replaces the profile's — it
// drops --ephemeral, whose absence writes the rollout the record is read from —
// so a confinement flag added to one and not the other would hold for half the
// calls. And a config.toml in the seat's home declares MCP servers of its own:
// read, it handed a text function codex's MCP-resource tools reaching them,
// and started them beside the seat's in a run with tools (measured on 0.164.0).
//
// Mutation: drop a -c from the route's argv, or --ignore-user-config from
// either, and this names it.
func TestCodexsToolRouteKeepsEveryConfinementFlag(t *testing.T) {
	t.Parallel()
	p, _ := harnessprofile.Builtin("codex")
	want := slices.DeleteFunc(slices.Clone(p.CompleteArgs), func(a string) bool { return a == "--ephemeral" })
	if !slices.Equal(p.MCP.CompleteArgs, want) {
		t.Errorf("the route's argv is %q, want the profile's own less --ephemeral: %q", p.MCP.CompleteArgs, want)
	}
	for name, argv := range map[string][]string{"complete_args": p.CompleteArgs, "mcp.complete_args": p.MCP.CompleteArgs} {
		if !slices.Contains(argv, "--ignore-user-config") {
			t.Errorf("%s %q reads the home's config.toml and the servers it declares", name, argv)
		}
	}
}

// A FAILED CODEX RUN SAYS WHY BY ITS OWN FAILURE EVENTS, and quotes nothing
// else it printed — its items carry every call's arguments and result.
//
// Mutation: drop the profile's failure events, and the error says only that
// codex exited 1.
func TestAFailedCodexRunSaysWhyByItsFailureEvents(t *testing.T) {
	t.Parallel()
	p := fakeAs(t, "codex", fakeScript{
		Responses: []fakeResponse{{Text: "posting", Calls: []fakeCall{
			{Name: "post", Args: map[string]any{"body": "ARGUMENT-MARKER"}},
		}, Usage: fakeUsage{Input: 5, Output: 5}}},
		Exit: 1, Result: "unexpected status 500: the vendor is overloaded",
	}, nil)
	host := newTestHost(t, "post")
	host.handle("post", func(context.Context, map[string]any) (string, bool) { return "RESULT-MARKER", false })
	_, err := p.Complete(t.Context(), phase(host))
	if llm.KindOf(err) != llm.KindServer {
		t.Fatalf("err = %v, want a server failure", err)
	}
	msg := err.Error()
	if !strings.Contains(msg, "error.message=unexpected status 500: the vendor is overloaded") {
		t.Errorf("the error does not say why by turn.failed's message: %s", msg)
	}
	for _, leaked := range []string{"ARGUMENT-MARKER", "RESULT-MARKER"} {
		if strings.Contains(msg, leaked) {
			t.Errorf("the error quotes the run's record (%q): %s", leaked, msg)
		}
	}
}
