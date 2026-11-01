package cliagent

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/providers/llm"
)

// Grok Build, as the fake plays it: the server from the agent file --agent
// names, every MCP server its home names started beside it unless the home's
// allowlist keeps it out, no id of its own with a call, and the record its
// streaming-json stdout — a response's prose, its counts without the cached
// part, then a tool_call per call (use_tool naming the server's tool, or one of
// the CLI's own), and a tool_call_update per result — ending in an `end` event
// (measured on 1.0.70; see the profile).
func init() {
	fakeCLIs["grok"] = fakeCLI{
		server: func() (string, []string, error) {
			if started := grokHomeServers(); len(started) > 0 {
				fmt.Fprintln(os.Stderr, "fake grok: started the home's own MCP servers", started)
				os.Exit(77)
			}
			raw, err := os.ReadFile(argAfter("--agent")) //nolint:gosec // the fake's own configuration
			if err != nil {
				return "", nil, err
			}
			return grokAgentServer(string(raw))
		},
		recorder: func(fakeScript, []byte) (fakeRecorder, error) {
			return &grokRecorder{out: json.NewEncoder(os.Stdout)}, nil
		},
	}
}

// grokHomeServers is every server other than crewlet that 1.0.70 starts from
// the home: the ones config.toml names, and ~/.claude.json's while the Claude
// source is scanned — each unless a home allowlist leaves it out.
func grokHomeServers() []string {
	home := os.Getenv("HOME")
	config, _ := os.ReadFile(filepath.Join(home, ".grok", "config.toml"))       //nolint:gosec // the fake's home
	policy, _ := os.ReadFile(filepath.Join(home, ".grok", "requirements.toml")) //nolint:gosec // the fake's home
	allowed := func(name string) bool {
		if !strings.Contains(string(policy), "[[allowed_mcp_servers]]") {
			return true
		}
		return strings.Contains(string(policy), `server_name = "`+name+`"`)
	}
	var names []string
	for _, m := range regexp.MustCompile(`\[mcp_servers\.([A-Za-z0-9_-]+)\]`).FindAllStringSubmatch(string(config), -1) {
		names = append(names, m[1])
	}
	if !strings.Contains(string(config), "[compat.claude]\nmcps = false") {
		raw, _ := os.ReadFile(filepath.Join(home, ".claude.json")) //nolint:gosec // the fake's home
		var claude struct {
			MCPServers map[string]any `json:"mcpServers"`
		}
		if json.Unmarshal(raw, &claude) == nil {
			for name := range claude.MCPServers {
				names = append(names, name)
			}
		}
	}
	var started []string
	for _, name := range names {
		if name != "crewlet" && allowed(name) {
			started = append(started, name)
		}
	}
	return started
}

// grokAgentServer reads the crewlet server out of an agent file's frontmatter,
// as the profile writes it: `command:` and `args:` lines carrying JSON values.
func grokAgentServer(agent string) (string, []string, error) {
	var command string
	var args []string
	for line := range strings.SplitSeq(agent, "\n") {
		line = strings.TrimSpace(line)
		if v, ok := strings.CutPrefix(line, "command: "); ok {
			if err := json.Unmarshal([]byte(v), &command); err != nil {
				return "", nil, err
			}
		}
		if v, ok := strings.CutPrefix(line, "args: "); ok {
			if err := json.Unmarshal([]byte(v), &args); err != nil {
				return "", nil, err
			}
		}
	}
	if command == "" {
		return "", nil, fmt.Errorf("no crewlet server in the agent file")
	}
	return command, args, nil
}

type grokRecorder struct {
	out *json.Encoder
}

func (g *grokRecorder) begin(_ string, tools []string) {
	offered := []string{"search_tool", "use_tool", "web_search", "web_fetch"}
	for _, tool := range tools {
		offered = append(offered, "crewlet__"+tool)
	}
	_ = g.out.Encode(map[string]any{"type": "available_commands", "tools": offered})
}

func (g *grokRecorder) asked(_ int, r fakeResponse, ids []string) {
	if r.Text != "" {
		_ = g.out.Encode(map[string]any{"type": "text", "data": r.Text})
	}
	if r.StopEarly {
		return
	}
	// The prompt WITHOUT its cached part, which is counted apart.
	_ = g.out.Encode(map[string]any{"type": "usage", "usage": map[string]any{
		"input_tokens": r.Usage.Input, "output_tokens": r.Usage.Output,
		"cache_read_input_tokens": r.Usage.CacheRead, "cache_creation_input_tokens": r.Usage.CacheWrite,
		"reasoning_tokens": 0}})
	for j, c := range r.Calls {
		event := map[string]any{"type": "tool_call", "toolCallId": ids[j], "status": "pending"}
		if c.Own {
			event["kind"], event["toolName"], event["rawInput"] = c.Name, c.Name, orEmpty(c.Args)
		} else {
			event["kind"], event["toolName"] = "use_tool", "use_tool"
			event["rawInput"] = map[string]any{"tool_name": "crewlet__" + c.Name, "tool_input": orEmpty(c.Args)}
		}
		_ = g.out.Encode(event)
	}
}

func (g *grokRecorder) answered(_ int, _ fakeResponse, ids []string, results []string) {
	for j, id := range ids {
		_ = g.out.Encode(map[string]any{"type": "tool_call_update", "toolCallId": id, "status": "completed",
			"rawOutput": map[string]any{"type": "MCP", "output": map[string]any{"OkayOutput": results[j]}}})
	}
}

func (g *grokRecorder) end(script fakeScript, total fakeUsage, _ string) int {
	stop := "end_turn"
	switch script.Subtype {
	case "max_turns":
		_ = g.out.Encode(map[string]any{"type": "max_turns_reached"})
		stop = "max_turn_requests"
		fmt.Fprintln(os.Stderr, "Error: max turns reached")
	case "cancelled":
		stop = "cancelled"
	}
	_ = g.out.Encode(map[string]any{"type": "end", "stopReason": stop, "usage": map[string]any{
		"input_tokens": total.Input, "output_tokens": total.Output, "cache_read_input_tokens": total.CacheRead}})
	return script.Exit
}

// GROK'S RUN IS READ FROM ITS STREAM, AND ITS HOME STARTS NO SERVER OF ITS OWN. A
// run with tools prints its streaming-json record where a text function prints
// the plain answer: each response's prose, its counts — the prompt less its
// cached part — and its calls, through use_tool, by the server's qualified
// name; the results of its calls end it. The CLI sends no id with a call, so a
// call is joined by name and arguments, and the host is told the CLI waits the
// CLI's own 6000 seconds for a call, re-reads no list, and makes a response's
// calls at once. A server planted in the home's config.toml and in
// ~/.claude.json does not start: the home's config.toml is the profile's own,
// with the Claude source off, and its allowlist names the seat's server alone.
//
// Mutation: drop the profile's mcp.output, and the run's stdout is read as the
// plain answer; drop both home seeds, and the planted servers start (the fake
// fails the run); read the record's input as the whole prompt, and the run is
// charged 100 tokens less than it spent.
func TestGrokRunIsReadFromItsStream(t *testing.T) {
	t.Parallel()
	p := fakeAs(t, "grok", routeCase, nil)
	home := filepath.Join(p.Workspace().KeySeatDir(llm.SeatOf(t.Context())), "home")
	for path, content := range map[string]string{
		".grok/config.toml": "[mcp_servers.planted]\ncommand = \"sh\"\n",
		".claude.json":      `{"mcpServers":{"planted_claude":{"command":"sh"}}}`,
	} {
		writeFile(t, filepath.Join(home, path), content)
	}
	holdsTheRoute(t, p, llm.ServeOptions{
		ToolTimeout: 6000 * time.Second, ObservesRequests: true, Concurrency: llm.CallsAtOnce,
	}, llm.Usage{Input: 340, Output: 29, CacheRead: 100})
}

// A GROK RUN ITS OWN TURN LIMIT ENDED IS AN ANSWER STOPPED AT THE CAP: a
// max_turns_reached event, then exit 1.
//
// Mutation: drop the profile's cap, and this is a server failure.
func TestAGrokRunItsTurnLimitEndedStopsAtTheCap(t *testing.T) {
	t.Parallel()
	p := fakeAs(t, "grok", fakeScript{Exit: 1, Subtype: "max_turns",
		Responses: []fakeResponse{{Calls: []fakeCall{{Name: "search"}}, Usage: fakeUsage{Input: 5, Output: 5}}}}, nil)
	comp, err := p.Complete(t.Context(), phase(newTestHost(t, "search")))
	if err != nil {
		t.Fatalf("a capped run came back as a failure: %v", err)
	}
	if comp.StopReason != llm.StopCap {
		t.Errorf("stop = %q, want the cap", comp.StopReason)
	}
}

// A GROK RUN THAT WOULD ASK IS A FAILURE, NEVER AN ANSWER. 1.0.70 cannot ask for
// an approval headless: it cancels the whole run, exits 0 with nothing of the
// answer, and says so on stderr. The profile allows or refuses everything a
// call is offered, so a run that asks is one its flags no longer cover — stopped
// and failed, naming the line, rather than passed off as the model's answer.
//
// Mutation: drop the profile's failure marker, and the cancelled run comes back
// as an answer.
func TestAGrokRunThatWouldAskFails(t *testing.T) {
	t.Parallel()
	p := fakeAs(t, "grok", fakeScript{Subtype: "cancelled",
		Stderr:    "permission required: something was not run, as a headless run cannot ask for approval\n",
		Responses: []fakeResponse{{Text: "half way", Usage: fakeUsage{Input: 5, Output: 5}}}}, nil)
	_, err := p.Complete(t.Context(), phase(newTestHost(t, "search")))
	if llm.KindOf(err) != llm.KindServer || err == nil ||
		!strings.Contains(err.Error(), "a headless run cannot ask for approval") {
		t.Fatalf("err = %v, want the run failed naming the approval it could not ask for", err)
	}
}

// A FAILED GROK RUN IS JUDGED BY WHAT GROK SAID, NEVER BY WHAT ITS TOOLS DID. A
// run with tools prints its whole record — every call's result among it — where
// a text function prints the answer alone, so the record is read as the route's
// own output: a tool that answered "Rate limited" is the tool's word, and the
// failed run is a server failure named by stderr, not a spent plan that benches
// the seat's credential.
//
// Mutation: read the run's stdout as the profile's plain answer
// (cliprofile.Profile.Routed dropped from both the reader and the extraction),
// and the tool's result is taken for the CLI's own limit report.
func TestAFailedGrokRunIsNotJudgedByWhatItsToolsSaid(t *testing.T) {
	t.Parallel()
	p := fakeAs(t, "grok", fakeScript{Exit: 1, Stderr: "Error: the connection was reset\n",
		Responses: []fakeResponse{{Calls: []fakeCall{{Name: "search"}}, Usage: fakeUsage{Input: 5, Output: 5}}}}, nil)
	host := newTestHost(t, "search")
	host.handle("search", func(context.Context, map[string]any) (string, bool) {
		return "Rate limited: the search index answered 429", false
	})
	_, err := p.Complete(t.Context(), phase(host))
	if kind := llm.KindOf(err); kind != llm.KindServer {
		t.Fatalf("err = %v (%v), want a server failure: the limit was the tool's, not the CLI's", err, kind)
	}
	if strings.Contains(err.Error(), "Rate limited") {
		t.Errorf("the error quotes the tool's result: %v", err)
	}
}

// A GROK CALL REACHES ONLY THE SEAT'S TOOLS, BY THE TWO META-TOOLS, AND READS NO
// FILE FOR THEM. The model is offered search_tool and use_tool rather than the
// server's tools by name, so the route drops both from the deny list a text
// function keeps; use_tool also takes its arguments from any file the model
// names, which a Read rule on every path refuses (a bare Read denial takes
// search_tool with it); every MCP tool runs unasked, so a name the model wrote
// without the server's prefix is refused for it rather than asked about, which
// cancels a headless run; and the home is the profile's own, with an allowlist
// naming the seat's server alone. All measured on 1.0.70.
func TestGrokCallsReachOnlyTheSeatsTools(t *testing.T) {
	t.Parallel()
	p := grok(t)
	m := p.MCP
	if m == nil || m.Output != "jsonl" {
		t.Fatalf("mcp = %+v, want a route whose run prints its streaming-json record", m)
	}
	for _, kept := range []string{"search_tool", "use_tool"} {
		denied, _ := flagValues(p.CompleteArgs, "--disallowed-tools")
		if !slices.Contains(denied, kept) {
			t.Errorf("a text function's deny list lacks %s, which it offers no server for", kept)
		}
		routed, _ := flagValues(m.CompleteArgs, "--disallowed-tools")
		if slices.Contains(routed, kept) {
			t.Errorf("the route denies %s, the only way its model reaches the seat's tools", kept)
		}
	}
	argv := slices.Concat(m.Args, m.AllowArgs)
	for flag, want := range map[string]string{"--deny": "Read(**)", "--allow": "MCPTool(*)"} {
		if got, _ := valueAfter(argv, flag); got != want {
			t.Errorf("%s %q, want %q", flag, got, want)
		}
	}
	seeds := map[string]string{}
	for _, f := range p.SeedFiles {
		seeds[f.Path] = f.Content
	}
	if config := seeds[".grok/config.toml"]; strings.Contains(config, "[mcp_servers") ||
		!strings.Contains(config, "[compat.claude]\nmcps = false") || !strings.Contains(config, "[compat.cursor]\nmcps = false") {
		t.Errorf("the home's config.toml seed is %q, want no server and the other vendors' servers unread", config)
	}
	if policy := seeds[".grok/requirements.toml"]; !strings.Contains(policy, "[[allowed_mcp_servers]]\nserver_name = \"crewlet\"") {
		t.Errorf("the home's requirements.toml seed is %q, want an allowlist naming the seat's server alone", policy)
	}
}
