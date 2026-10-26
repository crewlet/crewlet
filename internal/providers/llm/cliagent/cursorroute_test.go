package cliagent

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

	"github.com/crewlet/crewlet/internal/providers/llm"
)

// Cursor Agent, as the fake plays it — from its documentation and the
// 2026.10.22 bundle, since a run's model streams from Cursor's own service
// and no stand-in answers it (see the profile): every server the home's
// mcp.json names started with no approval asked; the project's — the call's
// own .cursor/mcp.json — only with --approve-mcps; a call to the server's
// tools run only where the project's cli.json allows `Mcp(crewlet:*)`; and
// the stream-json record: a model call's prose flushed under its
// model_call_id when its first tool call starts, a tool_call started and
// completed per call, the last response's prose with no id, and the run's
// counts on the result event, the input less its cached parts.
func init() {
	fakeCLIs["cursor-agent"] = fakeCLI{
		server: func() (string, []string, error) {
			raw, _ := os.ReadFile(filepath.Join(os.Getenv("HOME"), ".cursor", "mcp.json")) //nolint:gosec // the fake's home
			var home struct {
				MCPServers map[string]any `json:"mcpServers"`
			}
			if json.Unmarshal(raw, &home) == nil && len(home.MCPServers) > 0 {
				fmt.Fprintln(os.Stderr, "fake cursor-agent: started the home's own MCP servers")
				os.Exit(77)
			}
			if !slices.Contains(os.Args, "--approve-mcps") {
				return "", nil, fmt.Errorf("crewlet: not loaded (needs approval)")
			}
			policy, _ := os.ReadFile(filepath.Join(".cursor", "cli.json"))
			if !strings.Contains(string(policy), `"Mcp(crewlet:*)"`) {
				return "", nil, fmt.Errorf("the project's cli.json does not allow the crewlet server's tools")
			}
			project, err := os.ReadFile(filepath.Join(".cursor", "mcp.json"))
			if err != nil {
				return "", nil, err
			}
			return serverIn(project)
		},
		recorder: func(fakeScript, []byte) (fakeRecorder, error) {
			return &cursorRecorder{out: json.NewEncoder(os.Stdout)}, nil
		},
	}
}

type cursorRecorder struct {
	out  *json.Encoder
	text []string
}

func (c *cursorRecorder) begin(model string, _ []string) {
	_ = c.out.Encode(map[string]any{"type": "system", "subtype": "init", "model": model, "session_id": "s"})
}

func (c *cursorRecorder) assistant(text, modelCall string) {
	event := map[string]any{"type": "assistant", "session_id": "s",
		"message": map[string]any{"role": "assistant", "content": []any{map[string]any{"type": "text", "text": text}}}}
	if modelCall != "" {
		event["model_call_id"] = modelCall
	}
	_ = c.out.Encode(event)
}

func (c *cursorRecorder) toolCall(subtype, id, modelCall string, call fakeCall) {
	tool := map[string]any{"webFetchToolCall": map[string]any{"args": orEmpty(call.Args)}}
	if !call.Own {
		tool = map[string]any{"mcpToolCall": map[string]any{"args": map[string]any{
			"name": "crewlet-" + call.Name, "toolCallId": id, "providerIdentifier": "crewlet",
			"toolName": call.Name, "args": orEmpty(call.Args)}}}
	}
	_ = c.out.Encode(map[string]any{"type": "tool_call", "subtype": subtype, "call_id": id,
		"model_call_id": modelCall, "tool_call": tool, "session_id": "s"})
}

func (c *cursorRecorder) asked(i int, r fakeResponse, ids []string) {
	c.text = append(c.text, r.Text)
	if len(r.Calls) == 0 {
		// The last response's prose, flushed before the result with no id.
		if r.Text != "" {
			c.assistant(r.Text, "")
		}
		return
	}
	modelCall := fmt.Sprintf("model-call-%d", i+1)
	if r.Text != "" {
		c.assistant(r.Text, modelCall)
	}
	for j, call := range r.Calls {
		c.toolCall("started", ids[j], modelCall, call)
	}
}

func (c *cursorRecorder) answered(i int, r fakeResponse, ids []string, _ []string) {
	for j, call := range r.Calls {
		c.toolCall("completed", ids[j], fmt.Sprintf("model-call-%d", i+1), call)
	}
}

func (c *cursorRecorder) end(script fakeScript, total fakeUsage, _ string) int {
	if script.Exit != 0 {
		fmt.Fprint(os.Stderr, script.Result)
		return script.Exit
	}
	// The input WITHOUT its cached parts.
	_ = c.out.Encode(map[string]any{"type": "result", "subtype": "success", "is_error": false,
		"result": strings.Join(c.text, ""), "session_id": "s", "usage": map[string]any{
			"inputTokens": total.Input, "outputTokens": total.Output,
			"cacheReadTokens": total.CacheRead, "cacheWriteTokens": total.CacheWrite}})
	return 0
}

// CURSOR AGENT'S RUN IS READ FROM ITS stream-json STDOUT, AS ITS BUNDLE WRITES
// IT, AND ITS HOME STARTS NO SERVER OF ITS OWN. The seat's server is the call's
// own .cursor/mcp.json — the project's, approved for the run by
// --approve-mcps — its tools allowed by the project's cli.json; the home's
// mcp.json, whose servers the CLI starts unasked, is the profile's own, empty,
// whatever was there. Each model call's prose and calls share its
// model_call_id, a call's completion ends its request, and the last response's
// prose — flushed with no id — begins the next. Calls are joined by name and
// arguments, under the server's name; the result's counts settle the run.
// None of this is measured against the real CLI (see the profile); the
// doctor's smoke test checks it on a real sign-in.
//
// Mutation: drop --approve-mcps, or Mcp(crewlet:*), and the CLI never reaches
// the seat's tools; drop the home's mcp.json seed, and the planted server
// starts (the fake fails the run); drop the completion's close, and the last
// response's prose is read into the request before it.
func TestCursorAgentsRunIsReadFromItsStream(t *testing.T) {
	t.Parallel()
	// Prompts of a real size: the record carries no counts per request, and
	// an estimate, a floor under what a request read, is never refunded.
	script := routeCase
	script.Responses = slices.Clone(routeCase.Responses)
	script.Responses[0].Usage = fakeUsage{Input: 1000, Output: 20, CacheRead: 400}
	script.Responses[1].Usage = fakeUsage{Input: 1400, Output: 9, CacheRead: 600}
	p := fakeAs(t, "cursor-agent", script, nil)
	planted := filepath.Join(p.Workspace().KeySeatDir(llm.SeatOf(t.Context())), "home", ".cursor", "mcp.json")
	writeFile(t, planted, `{"mcpServers":{"planted":{"command":"sh"}}}`)
	host := newTestHost(t, "search")
	host.handle("search", func(context.Context, map[string]any) (string, bool) { return "3 results", false })
	comp, err := p.Complete(t.Context(), phase(host))
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if comp.Content != "Done: the notes are in." {
		t.Errorf("answer = %q, want the last response's prose", comp.Content)
	}
	_, calls, spent, _ := host.snapshot()
	if len(calls) != 1 || calls[0].Name != "search" {
		t.Fatalf("the host was called %+v, want the one search", calls)
	}
	turns := host.latestTurns()
	if len(turns) != 2 {
		t.Fatalf("the host was told of %d requests, want 2: %+v", len(turns), turns)
	}
	if turns[0].Text != "Looking it up." || len(turns[0].Calls) != 1 || !turns[0].Calls[0].Host ||
		turns[0].Calls[0].Name != "search" || turns[0].Calls[0].Arguments["q"] != "release notes" {
		t.Errorf("first request = %+v, want the prose and the host's search", turns[0])
	}
	if turns[1].Text != "Done: the notes are in." || len(turns[1].Calls) != 0 {
		t.Errorf("second request = %+v, want the last response's prose alone", turns[1])
	}
	if want := (llm.Usage{Input: 3400, Output: 29, CacheRead: 1000}); spent.Input != want.Input ||
		spent.Output != want.Output || spent.CacheRead != want.CacheRead {
		t.Errorf("the run was charged %+v, want the result's %+v", spent, want)
	}
	host.mu.Lock()
	opts := host.opts[0]
	host.mu.Unlock()
	if opts.Relists || opts.ToolTimeout != 60*time.Second || !opts.ObservesRequests ||
		opts.Concurrency != llm.CallsAtOnce || opts.CallIDMeta != "" {
		t.Errorf("the host was told %+v, want the SDK's 60 s, no relisting, observed requests, "+
			"calls at once, joined by name and arguments", opts)
	}
}
