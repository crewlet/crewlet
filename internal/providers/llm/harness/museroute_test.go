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
)

// Muse Code, as the fake plays it: the crewlet server from the run's own `-c
// mcpServers.crewlet=` setting and its tools only where `-c run.toolset=`
// admits them; every server the home's settings file names started beside it,
// and every plugin server it approves; the call's id and its key sent in
// `_meta`; and the record its exec stream — a model task per request, the
// request's prose in fragments, a tool task's side effect per call — ending
// in the run's counts per model (the prompt with its cached part in it) and
// a terminal event (measured on 1.4.13; see the profile).
func init() {
	fakeHarnesses["muse-code"] = fakeHarness{
		server: func() (string, []string, error) {
			if started := museHomeServers(); len(started) > 0 {
				fmt.Fprintln(os.Stderr, "fake muse: started the home's own MCP servers", started)
				os.Exit(77)
			}
			toolset, ok := argAfterPrefix("run.toolset=")
			var tools []string
			if !ok || json.Unmarshal([]byte(toolset), &tools) != nil || !slices.Contains(tools, "mcp__crewlet__*") {
				return "", nil, fmt.Errorf("run.toolset does not admit the crewlet server's tools: %q", toolset)
			}
			raw, ok := argAfterPrefix("mcpServers.crewlet=")
			if !ok {
				return "", nil, fmt.Errorf("no -c mcpServers.crewlet")
			}
			var server struct {
				Transport string   `json:"transport"`
				Command   string   `json:"command"`
				Args      []string `json:"args"`
			}
			if err := json.Unmarshal([]byte(raw), &server); err != nil || server.Transport != "stdio" {
				return "", nil, fmt.Errorf("-c mcpServers.crewlet is not a stdio server: %q", raw)
			}
			return server.Command, server.Args, nil
		},
		meta: func(id string) sdk.Meta {
			return sdk.Meta{"callId": id, "idempotencyKey": "tool:" + id}
		},
		recorder: func(fakeScript, []byte) (fakeRecorder, error) {
			return &museRecorder{out: json.NewEncoder(os.Stdout)}, nil
		},
	}
}

// museHomeServers is every server 1.4.13 starts from the seat's home: the ones
// its settings file names, and each plugin server it approves.
func museHomeServers() []string {
	raw, _ := os.ReadFile(filepath.Join(os.Getenv("XDG_CONFIG_HOME"), "muse", "settings.json")) //nolint:gosec // the fake's home
	var settings struct {
		MCPServers          map[string]any `json:"mcpServers"`
		RuntimeCapabilities map[string]any `json:"runtime_capabilities"`
	}
	_ = json.Unmarshal(raw, &settings)
	var names []string
	for name := range settings.MCPServers {
		names = append(names, name)
	}
	for id := range settings.RuntimeCapabilities {
		names = append(names, id)
	}
	return names
}

type museRecorder struct {
	out   *json.Encoder
	texts []string
}

func (m *museRecorder) event(payloadType string, payload map[string]any) {
	_ = m.out.Encode(map[string]any{"schema_version": 1, "record_type": "event",
		"payload_type": payloadType, "payload": payload})
}

func (m *museRecorder) task(kind, id, operation, key string) {
	m.event("task.lifecycle.proposed", map[string]any{"task_id": id,
		"event": map[string]any{"kind": "proposed", "task_id": id, "task_kind": kind}})
	m.event("task.lifecycle.side_effect_intent", map[string]any{"task_id": id,
		"event": map[string]any{"kind": "side_effect_intent", "task_id": id, "operation": operation,
			"idempotency_key": key}})
}

func (m *museRecorder) begin(string, []string) {
	m.event("run.lifecycle.started", map[string]any{"kind": "run_started"})
}

func (m *museRecorder) asked(i int, r fakeResponse, ids []string) {
	model := fmt.Sprintf("model-task-%d", i+1)
	m.task("model.meta.response", model, "model.meta.response", "model:"+model)
	// The prose in fragments, as a model streams it.
	for len(r.Text) > 0 {
		n := min(len(r.Text), 5)
		m.event("run.output.delta", map[string]any{"kind": "run_output_delta", "text": r.Text[:n]})
		r.Text = r.Text[n:]
	}
	for j, c := range r.Calls {
		name := "mcp__crewlet__" + c.Name
		if c.Own {
			name = c.Name
		}
		m.task("tool."+name, fmt.Sprintf("tool-task-%d-%d", i+1, j), "tool:"+name, "tool:"+ids[j])
	}
}

func (m *museRecorder) answered(i int, r fakeResponse, ids []string, results []string) {
	m.texts = append(m.texts, r.Text)
	for j, id := range ids {
		m.event("tool.result", map[string]any{"call_id": id, "text": results[j]})
	}
}

func (m *museRecorder) end(script fakeScript, total fakeUsage, _ string) int {
	// The prompt WITH its cached part in it.
	m.event("run.usage", map[string]any{"kind": "run_usage", "models": []any{map[string]any{
		"model": script.Model, "input_tokens": total.Input + total.CacheRead, "cached_tokens": total.CacheRead,
		"output_tokens": total.Output, "reasoning_tokens": 0}}})
	if script.Exit != 0 {
		m.event("run.terminal.failed", map[string]any{"terminal": "failed", "reason": script.Result})
		fmt.Fprintln(os.Stderr, "run ended with Failed")
		return script.Exit
	}
	m.event("run.terminal.completed", map[string]any{"terminal": "completed", "text": strings.Join(m.texts, "")})
	return 0
}

// MUSE CODE'S RUN IS READ FROM ITS EXEC STREAM, AND ITS HOME STARTS NO SERVER OF
// ITS OWN. The crewlet server is declared for the run on argv (`-c`), and the
// run's toolset admits its tools; the home's settings file, whose servers —
// and approved plugins' — the CLI starts on every call, is the profile's own,
// whatever was there. Each model task is a request, its prose the fragments
// streamed after it, run together, and each call a tool task's side effect,
// joined to the call by the key the CLI sends in `_meta`. The stream carries
// no counts per request: each is estimated as it is read, and the run's own
// count — the prompt with its cached part in it — settles the rest. The host is
// told the CLI waits 300 seconds for a call, does not re-read the list, and
// makes a response's calls one after another.
//
// Prompts of a real size here, because an estimate is never refunded: it is a
// floor under what the request read, and the run's count settles what it left.
//
// Mutation: drop the run.toolset setting, and the CLI never reaches the seat's
// tools; drop the settings seed, and the planted server starts (the fake fails
// the run); join by callId, and no call is joined; read the run's prompt as
// less its cached part, and it is charged 1,000 tokens more than it spent.
func TestMuseCodesRunIsReadFromItsExecStream(t *testing.T) {
	t.Parallel()
	script := routeCase
	script.Responses = slices.Clone(routeCase.Responses)
	script.Responses[0].Usage = fakeUsage{Input: 1000, Output: 20, CacheRead: 400}
	script.Responses[1].Usage = fakeUsage{Input: 1400, Output: 9, CacheRead: 600}
	p := fakeAs(t, "muse-code", script, nil)
	planted := filepath.Join(p.Workspace().KeySeatDir(llm.SeatOf(t.Context())), "home", ".config", "muse", "settings.json")
	writeFile(t, planted, `{"schema_version":1,"mcpServers":{"planted":{"type":"stdio","command":"sh"}}}`)
	host := newTestHost(t, "search")
	host.handle("search", func(context.Context, map[string]any) (string, bool) { return "3 results", false })
	comp, err := p.Complete(t.Context(), phase(host))
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if comp.Content != "Done: the notes are in." || comp.StopReason != llm.StopEnd {
		t.Errorf("completion = %q (%q), want the last response's prose, run together, ended",
			comp.Content, comp.StopReason)
	}
	_, calls, spent, spends := host.snapshot()
	if len(calls) != 1 || calls[0].Name != "search" {
		t.Fatalf("the host was called %+v, want the one search", calls)
	}
	latest := host.latestTurns()
	if len(latest) != 2 || latest[0].Text != "Looking it up." || latest[1].Text != "Done: the notes are in." {
		t.Fatalf("the host was told %+v, want each request's prose, run together", latest)
	}
	if c := latest[0].Calls; len(c) != 1 || c[0].Name != "search" || !c[0].Host || c[0].ID != "tool:"+calls[0].Meta["callId"].(string) {
		t.Errorf("first request's calls = %+v, want the host's search under the key it was sent with", c)
	}
	if want := (llm.Usage{Input: 3400, Output: 29, CacheRead: 1000}); spent.Input != want.Input ||
		spent.Output != want.Output || spent.CacheRead != want.CacheRead {
		t.Errorf("the run was charged %+v in all (%+v), want the CLI's own count %+v", spent, spends, want)
	}
	if len(spends) < 2 || !spends[0].Estimated {
		t.Errorf("charged %+v, want each request estimated as it was read", spends)
	}
	host.mu.Lock()
	opts := host.opts[0]
	host.mu.Unlock()
	if opts.Relists || opts.ToolTimeout != 300*time.Second || !opts.ObservesRequests ||
		opts.Concurrency != llm.CallsInTurn || opts.CallIDMeta != "idempotencyKey" {
		t.Errorf("the host was told %+v, want no relisting, 300 s, observed requests, calls in turn, "+
			"joined by idempotencyKey", opts)
	}
}

// A FAILED MUSE CODE RUN SAYS WHY IN ITS TERMINAL EVENT, and only there: its
// stderr says "run ended with Failed" and nothing else, so the reason a spent
// plan is told by is the run.terminal.failed event's — read by field, as the
// CLI's own failure event, and never the run's other lines.
//
// Mutation: drop the profile's failure event, and the spent plan is a server
// failure.
func TestAFailedMuseCodeRunSaysWhyInItsTerminalEvent(t *testing.T) {
	t.Parallel()
	p := fakeAs(t, "muse-code", fakeScript{Exit: 1,
		Result:    "API error 429: slow down (rate_limit_error) (after 4 provider attempts)",
		Responses: []fakeResponse{{Calls: []fakeCall{{Name: "search"}}, Usage: fakeUsage{Input: 5, Output: 5}}}},
		nil)
	host := newTestHost(t, "search")
	host.handle("search", func(context.Context, map[string]any) (string, bool) { return "3 results", false })
	_, err := p.Complete(t.Context(), phase(host))
	if kind := llm.KindOf(err); kind != llm.KindRateLimit {
		t.Fatalf("err = %v (%v), want the spent plan the run's terminal event reported", err, kind)
	}
}
