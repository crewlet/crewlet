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
// own with a call; and the wire log it keeps in the seat's home under a
// directory named after the working directory: its profile.bind naming that
// directory, then per step its usage (the prompt without its cached part, and
// a model named only by alias), its prose and calls, and the step's end. The
// assistant's messages are on stdout, and a failed run's class is in the log
// alone (measured on 2.7.3; see the profile).
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

// kimiHomeServers is every server 2.7.3 starts from the seat's home: the ones
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

// kimiSessionDir is where 2.7.3 keeps a run's session: under the home, in a
// directory named after the working directory — its base name and a digest
// of its path, which no glob the engine writes can spell.
func kimiSessionDir(cwd, session string) string {
	sum := sha256.Sum256([]byte(cwd))
	return filepath.Join(os.Getenv("KIMI_CODE_HOME"), "sessions",
		"wd_"+filepath.Base(cwd)+"_"+hex.EncodeToString(sum[:])[:12], "session_"+session)
}

type kimiRecorder struct {
	wire  *json.Encoder
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

func (k *kimiRecorder) begin(_ string, tools []string) {
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
	_ = k.wire.Encode(map[string]any{"type": "usage.record", "agentId": "main", "usageScope": "turn",
		"model": "__kimi_env_model__", "usage": kimiUsage(r.Usage)})
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
	if script.RecordEnd != "" {
		var line map[string]any
		if err := json.Unmarshal([]byte(script.RecordEnd), &line); err == nil {
			_ = k.wire.Encode(line)
		}
	}
	return script.Exit
}

// KIMI CODE'S RUN IS READ FROM THE WIRE LOG IT KEEPS FOR IT IN THE SEAT'S HOME,
// AND THE HOME STARTS NO SERVER OF ITS OWN. The crewlet server is the call's
// own .kimi-code/mcp.json (read only in a trusted workspace); the home's
// mcp.json and plugin list are the profile's own and empty. Each step's usage
// is the prompt less its cached part. The log sits in a directory named after
// the call's working directory, so the run's own is the one profile.bind
// names. Calls carry no id (joined by name and arguments); the host is told
// it waits 600 seconds, re-reads no list, calls one by one.
// Mutation: drop KIMI_CODE_TRUST_WORKSPACE or mcp__crewlet__* from the agent
// file, the seat's tools are unreachable; drop the mcp.json or installed.json
// seed, the planted server starts; drop the owner rule's match, another call's
// log is taken; read inputOther as the whole prompt, 100 go unbilled.
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

// A FAILED KIMI CODE RUN SAYS WHY ONLY IN ITS RECORD: stderr says the turn
// failed and no more, and the wire log the run is read from ends on turn.ended
// with the harness's class, which judges it.
//
// Mutation: stop feeding the record's lines to its reports in serveTools, and
// the spent plan is a server failure.
func TestAFailedKimiCodeRunSaysWhyOnlyInItsRecord(t *testing.T) {
	t.Parallel()
	p := fakeAs(t, "kimi-code", fakeScript{Exit: 1, Stderr: kimi27Stderr, RecordEnd: kimi27Spent,
		Responses: []fakeResponse{{Calls: []fakeCall{{Name: "search"}}, Usage: fakeUsage{Input: 5, Output: 5}}}},
		func(c *Config) { c.Auth = Auth{Mode: AuthSubscription} })
	host := newTestHost(t, "search")
	host.handle("search", func(context.Context, map[string]any) (string, bool) { return "3 results", false })
	_, err := p.Complete(t.Context(), phase(host))
	if kind := llm.KindOf(err); kind != llm.KindRateLimit || !strings.Contains(err.Error(), "5-hour usage limit") {
		t.Fatalf("err = %v (%v), want the spent plan the harness's record ended on", err, kind)
	}
}
