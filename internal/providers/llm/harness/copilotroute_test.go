package harness

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/crewlet/crewlet/internal/providers/llm"
)

// Copilot, as the fake plays it: every server the home's mcp-config.json names
// started beside the one --additional-mcp-config adds, each call's toolCallId
// in `_meta` as `com.github.copilot/toolCallId`, and the record in the session's
// events file under the home — a turn per request, its usage_record and its
// assistant.message written before its calls run — with the answer alone on
// stdout and the run's counts in its usage report (measured on 1.0.114; see
// the profile).
func init() {
	fakeHarnesses["copilot"] = fakeHarness{
		server: func() (string, []string, error) {
			// THE HOME'S OWN SERVERS START TOO, as Copilot's do: a home
			// that names one fails the run here, so a profile that lets
			// one through fails its test.
			if raw, err := os.ReadFile(filepath.Join(os.Getenv("HOME"), ".copilot", "mcp-config.json")); err == nil {
				var home struct {
					MCPServers map[string]any `json:"mcpServers"`
				}
				if json.Unmarshal(raw, &home) == nil && len(home.MCPServers) > 0 {
					fmt.Fprintln(os.Stderr, "fake copilot: started the home's own MCP servers")
					os.Exit(77)
				}
			}
			raw, err := os.ReadFile(strings.TrimPrefix(argAfter("--additional-mcp-config"), "@")) //nolint:gosec // the fake's own configuration
			if err != nil {
				return "", nil, err
			}
			return serverIn(raw)
		},
		meta:     func(id string) sdk.Meta { return sdk.Meta{"com.github.copilot/toolCallId": id} },
		recorder: newCopilotRecorder,
	}
}

type copilotRecorder struct {
	events *json.Encoder
	model  string
}

func copilotEvent(kind string, data map[string]any) map[string]any {
	return map[string]any{"type": kind, "data": data}
}

// copilotUsage is a turn's usage_record: its input count the whole prompt, the
// cached tokens, read and written, inside it.
func copilotUsage(model, call string, u fakeUsage) map[string]any {
	return copilotEvent("session.usage_record", map[string]any{"usage": map[string]any{"model": model,
		"inputTokens": u.Input + u.CacheRead + u.CacheWrite, "outputTokens": u.Output,
		"cacheReadTokens": u.CacheRead, "cacheWriteTokens": u.CacheWrite, "apiCallId": call}})
}

func newCopilotRecorder(script fakeScript, _ []byte) (fakeRecorder, error) {
	state := filepath.Join(os.Getenv("HOME"), ".copilot", "session-state")
	if script.ForeignRecord {
		other, err := recordFile(filepath.Join(state, "00000000-another-session", "events.jsonl"))
		if err != nil {
			return nil, err
		}
		_ = other.Encode(copilotEvent("session.start", map[string]any{"sessionId": "00000000-another-session",
			"context": map[string]any{"cwd": "/somewhere/else"}}))
		_ = other.Encode(copilotEvent("assistant.turn_start", map[string]any{"turnId": "0"}))
		_ = other.Encode(copilotUsage("fake-model-1", "other", fakeUsage{Input: 999, Output: 999}))
		_ = other.Encode(copilotEvent("assistant.message", map[string]any{"turnId": "0", "content": "FOREIGN"}))
	}
	session := fmt.Sprintf("11111111-2222-3333-4444-%012d", os.Getpid())
	events, err := recordFile(filepath.Join(state, session, "events.jsonl"))
	if err != nil {
		return nil, err
	}
	cwd, _ := os.Getwd()
	_ = events.Encode(copilotEvent("session.start", map[string]any{"sessionId": session,
		"context": map[string]any{"cwd": cwd}}))
	return &copilotRecorder{events: events}, nil
}

func (c *copilotRecorder) begin(model string, _ []string) { c.model = model }

func (c *copilotRecorder) asked(i int, r fakeResponse, ids []string) {
	turn := fmt.Sprint(i)
	_ = c.events.Encode(copilotEvent("assistant.turn_start", map[string]any{"turnId": turn}))
	if !r.StopEarly {
		_ = c.events.Encode(copilotUsage(c.model, "chatcmpl-"+turn, r.Usage))
	}
	requests := []any{}
	for j, call := range r.Calls {
		name := "crewlet-" + call.Name
		if call.Own {
			name = call.Name
		}
		requests = append(requests, map[string]any{"toolCallId": ids[j], "name": name, "arguments": orEmpty(call.Args)})
	}
	_ = c.events.Encode(copilotEvent("assistant.message", map[string]any{"turnId": turn, "model": c.model,
		"content": r.Text, "toolRequests": requests, "apiCallId": "chatcmpl-" + turn}))
}

func (c *copilotRecorder) answered(i int, _ fakeResponse, ids []string, results []string) {
	for j, id := range ids {
		_ = c.events.Encode(copilotEvent("tool.execution_complete", map[string]any{"toolCallId": id,
			"success": true, "result": map[string]any{"content": results[j]}}))
	}
	_ = c.events.Encode(copilotEvent("assistant.turn_end", map[string]any{"turnId": fmt.Sprint(i)}))
}

func (c *copilotRecorder) end(script fakeScript, total fakeUsage, last string) int {
	if path := argAfter("--usage-output-file"); path != "" {
		raw, _ := json.Marshal(map[string]any{"providerModelMetrics": []any{map[string]any{"metrics": map[string]any{
			"usage": map[string]any{"inputTokens": total.Input + total.CacheRead + total.CacheWrite,
				"outputTokens": total.Output, "cacheReadTokens": total.CacheRead,
				"cacheWriteTokens": total.CacheWrite}}}}})
		_ = os.WriteFile(path, raw, 0o600)
	}
	if script.Exit == 0 {
		fmt.Println(last)
	}
	return script.Exit
}

// COPILOT'S RUN IS READ FROM ITS OWN SESSION'S EVENTS. With -s its stdout is
// the answer alone, so each request — a turn — is read from the events file
// the harness keeps under the home, its counts and its calls written before the
// calls run, each call joined to its request by the toolCallId it carries in
// `_meta`. The session's directory is named by an id the harness mints, so the
// run's file is told from a concurrent run's by the working directory its
// first event names, and its home's mcp-config.json — whose servers the harness
// starts on every run — is the profile's own, empty, whatever was there.
//
// Mutation: drop the profile's owner, and the other run's FOREIGN turn is
// this run's; drop the seed, and the planted server starts; drop
// call_id_meta, and the route's options differ.
func TestCopilotsRunIsReadFromItsOwnSessionEvents(t *testing.T) {
	t.Parallel()
	script := routeCase
	script.ForeignRecord = true
	p := fakeAs(t, "copilot", script, nil)
	planted := filepath.Join(p.Workspace().KeySeatDir(llm.SeatOf(t.Context()), p.Key()), "home", ".copilot", "mcp-config.json")
	if err := os.MkdirAll(filepath.Dir(planted), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(planted, []byte(`{"mcpServers":{"planted":{"command":"sh"}}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	host := holdsTheRoute(t, p, llm.ServeOptions{
		Relists: true, ToolTimeout: 10 * time.Minute, ObservesRequests: true, Concurrency: llm.CallsAtOnce,
		CallIDMeta: "com.github.copilot/toolCallId",
	}, llm.Usage{Input: 340, Output: 29, CacheRead: 100})
	_, calls, _, _ := host.snapshot()
	turns := host.latestTurns()
	if id, _ := calls[0].Meta["com.github.copilot/toolCallId"].(string); id == "" || id != turns[0].Calls[0].ID {
		t.Errorf("the call carried toolCallId %q and the events name %q: they must join",
			calls[0].Meta["com.github.copilot/toolCallId"], turns[0].Calls[0].ID)
	}
	for _, turn := range turns {
		if strings.Contains(turn.Text, "FOREIGN") {
			t.Errorf("another run's session events were read as this one's: %+v", turn)
		}
	}
}
