package harness

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
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

// Kimi Code, as the fake plays it: the crewlet server from the call's own
// .kimi-code/mcp.json, read only in a trusted workspace; every server the
// home's mcp.json or one of its installed plugins names started beside it; the
// server's tools offered only where the agent file allows them; no id of its
// own with a call; and the record the wire log it keeps in the seat's home
// under a directory named after the working directory — its profile.bind
// naming that directory, then per step its request, its usage (the prompt
// without its cached part), its prose and calls, and the step's end — with the
// assistant's messages on stdout and a failure on stderr (measured on 2.3.2;
// see the profile).
func init() {
	fakeHarnesses["kimi-code"] = fakeHarness{
		server: func() (string, []string, error) {
			if started := kimiHomeServers(); len(started) > 0 {
				fmt.Fprintln(os.Stderr, "fake kimi: started the home's own MCP servers", started)
				os.Exit(77)
			}
			agent, err := os.ReadFile(argAfter("--agent-file")) //nolint:gosec // the fake's own agent file
			if err != nil {
				return "", nil, err
			}
			if !slices.Contains(strings.Fields(string(agent)), "mcp__crewlet__*") {
				return "", nil, fmt.Errorf("the agent file does not allow the crewlet server's tools")
			}
			if trusted := os.Getenv("KIMI_CODE_TRUST_WORKSPACE"); trusted != "1" && trusted != "true" {
				return "", nil, fmt.Errorf("the workspace is not trusted, so its .kimi-code/mcp.json is skipped")
			}
			raw, err := os.ReadFile(filepath.Join(".kimi-code", "mcp.json"))
			if err != nil {
				return "", nil, err
			}
			return serverIn(raw)
		},
		recorder: newKimiRecorder,
	}
}

// kimiHomeServers is every server 2.3.2 starts from the seat's home: the ones
// its mcp.json names and, for each plugin installed.json lists, the plugin's.
func kimiHomeServers() []string {
	home := os.Getenv("KIMI_CODE_HOME")
	var names []string
	raw, _ := os.ReadFile(filepath.Join(home, "mcp.json")) //nolint:gosec // the fake's home
	var config struct {
		MCPServers map[string]any `json:"mcpServers"`
	}
	if json.Unmarshal(raw, &config) == nil {
		for name := range config.MCPServers {
			names = append(names, name)
		}
	}
	raw, _ = os.ReadFile(filepath.Join(home, "plugins", "installed.json")) //nolint:gosec // the fake's home
	var installed struct {
		Plugins []struct {
			ID string `json:"id"`
		} `json:"plugins"`
	}
	if json.Unmarshal(raw, &installed) == nil {
		for _, p := range installed.Plugins {
			names = append(names, "plugin "+p.ID)
		}
	}
	return names
}

// kimiSessionDir is where 2.3.2 keeps a run's session: under the home, in a
// directory named after the working directory — its base name and a digest
// of its path, which no glob the engine writes can spell.
func kimiSessionDir(cwd, session string) string {
	sum := sha256.Sum256([]byte(cwd))
	return filepath.Join(os.Getenv("KIMI_CODE_HOME"), "sessions",
		"wd_"+filepath.Base(cwd)+"_"+hex.EncodeToString(sum[:])[:12], "session_"+session)
}

type kimiRecorder struct {
	wire  *json.Encoder
	model string
	steps []string
}

func newKimiRecorder(fakeScript, []byte) (fakeRecorder, error) {
	cwd, _ := os.Getwd()
	// ANOTHER CALL OF THE SAME SEAT, under way in the shared home: its wire
	// log matches the profile's glob and names another directory.
	other := filepath.Join(filepath.Dir(cwd), "another-call")
	foreign, err := recordFile(filepath.Join(kimiSessionDir(other, "0000"), "agents", "main", "wire.jsonl"))
	if err != nil {
		return nil, err
	}
	for _, line := range []map[string]any{
		{"type": "metadata", "protocol_version": "1.5"},
		{"type": "profile.bind", "agentId": "main", "environmentDisclosure": map[string]any{"cwd": other}},
		{"type": "context.append_loop_event", "event": map[string]any{"type": "step.begin", "uuid": "foreign"}},
		{"type": "context.append_loop_event", "event": map[string]any{"type": "content.part", "stepUuid": "foreign",
			"part": map[string]any{"type": "text", "text": "another call's prose"}}},
	} {
		_ = foreign.Encode(line)
	}
	wire, err := recordFile(filepath.Join(kimiSessionDir(cwd, "0001"), "agents", "main", "wire.jsonl"))
	if err != nil {
		return nil, err
	}
	_ = wire.Encode(map[string]any{"type": "metadata", "protocol_version": "1.5"})
	return &kimiRecorder{wire: wire}, nil
}

func (k *kimiRecorder) loop(event map[string]any) {
	_ = k.wire.Encode(map[string]any{"type": "context.append_loop_event", "agentId": "main", "event": event})
}

func (k *kimiRecorder) begin(model string, tools []string) {
	k.model = model
	cwd, _ := os.Getwd()
	offered := []any{map[string]any{"name": "FetchURL"}}
	for _, name := range tools {
		offered = append(offered, map[string]any{"name": "mcp__crewlet__" + name})
	}
	_ = k.wire.Encode(map[string]any{"type": "profile.bind", "agentId": "main", "profileName": "crewlet-seat",
		"environmentDisclosure": map[string]any{"cwd": cwd}})
	_ = k.wire.Encode(map[string]any{"type": "llm.tools_snapshot", "agentId": "main", "tools": offered})
}

func kimiUsage(u fakeUsage) map[string]any {
	// The prompt WITHOUT its cached part, which is counted apart.
	return map[string]any{"inputOther": u.Input, "output": u.Output,
		"inputCacheRead": u.CacheRead, "inputCacheCreation": u.CacheWrite}
}

func (k *kimiRecorder) asked(i int, r fakeResponse, ids []string) {
	step := fmt.Sprintf("step-%d", i+1)
	k.steps = append(k.steps, step)
	k.loop(map[string]any{"type": "step.begin", "uuid": step, "step": i + 1})
	_ = k.wire.Encode(map[string]any{"type": "llm.request", "agentId": "main", "kind": "loop", "model": k.model})
	_ = k.wire.Encode(map[string]any{"type": "usage.record", "agentId": "main", "usageScope": "turn",
		"usage": kimiUsage(r.Usage)})
	if r.Text != "" {
		k.loop(map[string]any{"type": "content.part", "stepUuid": step, "part": map[string]any{"type": "text", "text": r.Text}})
		raw, _ := json.Marshal(map[string]any{"role": "assistant", "content": r.Text})
		fmt.Println(string(raw))
	}
	for j, c := range r.Calls {
		name := "mcp__crewlet__" + c.Name
		if c.Own {
			name = c.Name
		}
		k.loop(map[string]any{"type": "tool.call", "stepUuid": step, "toolCallId": ids[j], "name": name,
			"args": orEmpty(c.Args)})
	}
}

func (k *kimiRecorder) answered(i int, r fakeResponse, ids []string, results []string) {
	for j, id := range ids {
		k.loop(map[string]any{"type": "tool.result", "toolCallId": id, "result": map[string]any{"output": results[j]}})
	}
	finish := "end_turn"
	if len(r.Calls) > 0 {
		finish = "tool_use"
	}
	k.loop(map[string]any{"type": "step.end", "uuid": k.steps[i], "finishReason": finish, "usage": kimiUsage(r.Usage)})
}

func (k *kimiRecorder) end(script fakeScript, _ fakeUsage, _ string) int {
	return script.Exit
}

// KIMI CODE'S RUN IS READ FROM THE WIRE LOG IT KEEPS FOR IT IN THE SEAT'S HOME,
// AND THE HOME STARTS NO SERVER OF ITS OWN. The crewlet server is the call's
// own .kimi-code/mcp.json, which the CLI reads only in a trusted workspace; the
// home's mcp.json and its plugin list, whose servers the CLI starts on every
// call, are the profile's own, empty, whatever was there; and the agent file
// allows the server's tools. Each step of the run is a request: its usage —
// the prompt less its cached part — read as soon as the CLI records it, its
// prose and calls, its end. The log lives in a directory named after the call's
// working directory, beside every other call's of the seat, so the run's own
// is the one whose profile.bind names its directory. The CLI sends no id with
// a call, so a call is joined by name and arguments; the host is told the CLI
// waits the profile's 600 seconds for a call, does not re-read the list, and
// makes a response's calls one after another.
//
// Mutation: drop KIMI_CODE_TRUST_WORKSPACE, or mcp__crewlet__* from the agent
// file, and the CLI never reaches the seat's tools; drop the mcp.json or the
// installed.json seed, and the planted server starts (the fake fails the run);
// drop the owner rule's match, and another call's log is taken for this one;
// read inputOther as the whole prompt, and the run is charged 100 tokens less
// than it spent.
func TestKimiCodesRunIsReadFromTheWireLogItKeepsForIt(t *testing.T) {
	t.Parallel()
	p := fakeAs(t, "kimi-code", routeCase, func(c *Config) { c.Auth = Auth{Mode: AuthSubscription} })
	home := filepath.Join(p.Workspace().SeatDir(llm.SeatOf(t.Context())), "home", ".kimi-code")
	writeFile(t, filepath.Join(home, "mcp.json"), `{"mcpServers":{"planted":{"command":"sh"}}}`)
	writeFile(t, filepath.Join(home, "plugins", "installed.json"),
		`{"version":1,"plugins":[{"id":"planted","root":"/nowhere","source":"local-path","enabled":true}]}`)
	holdsTheRoute(t, p, llm.ServeOptions{
		ToolTimeout: 10 * time.Minute, ObservesRequests: true, Concurrency: llm.CallsInTurn,
	}, llm.Usage{Input: 340, Output: 29, CacheRead: 100})
}

// A FAILED KIMI CODE RUN SAYS WHY ON STDERR, and is judged by it: the CLI
// prints its own classification of the provider's answer — `provider.<kind>:`
// and the upstream message — and exits 1, where a spent plan's words are read
// as a text function's are.
//
// Mutation: drop provider.rate_limit from the profile's limit markers, and the
// spent plan is a server failure.
func TestAFailedKimiCodeRunSaysWhyOnStderr(t *testing.T) {
	t.Parallel()
	p := fakeAs(t, "kimi-code", fakeScript{Exit: 1,
		Stderr:    "error: failed to run prompt: provider.rate_limit: 429 slow down\n",
		Responses: []fakeResponse{{Calls: []fakeCall{{Name: "search"}}, Usage: fakeUsage{Input: 5, Output: 5}}}},
		func(c *Config) { c.Auth = Auth{Mode: AuthSubscription} })
	host := newTestHost(t, "search")
	host.handle("search", func(context.Context, map[string]any) (string, bool) { return "3 results", false })
	_, err := p.Complete(t.Context(), phase(host))
	if kind := llm.KindOf(err); kind != llm.KindRateLimit {
		t.Fatalf("err = %v (%v), want the spent plan the CLI reported on stderr", err, kind)
	}
}
