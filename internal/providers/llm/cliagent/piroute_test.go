package cliagent

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/providers/llm"
)

// Pi, as the fake plays it: the server from the extension the run loads by its
// path, every server the agent directory's mcp.json names started beside it, no
// id of its own with a call, and the record the session file --session-dir
// names — an assistant message per response, its prose and calls as content
// blocks and its counts without the cached part, then a tool result per call —
// with the plain answer on stdout and a failure on stderr (measured on 1.4.0;
// see the profile).
func init() {
	fakeCLIs["pi"] = fakeCLI{
		server: func() (string, []string, error) {
			raw, _ := os.ReadFile(filepath.Join(os.Getenv("PI_CODING_AGENT_DIR"), "mcp.json")) //nolint:gosec // the fake's home
			var home struct {
				MCPServers map[string]any `json:"mcpServers"`
			}
			if json.Unmarshal(raw, &home) == nil && len(home.MCPServers) > 0 {
				fmt.Fprintln(os.Stderr, "fake pi: started the home's own MCP servers")
				os.Exit(77)
			}
			for i, arg := range os.Args {
				if arg == "-e" && i+1 < len(os.Args) && strings.HasSuffix(os.Args[i+1], ".mjs") {
					ext, err := os.ReadFile(os.Args[i+1]) //nolint:gosec // the fake's own extension
					if err != nil {
						return "", nil, err
					}
					return piExtensionServer(string(ext))
				}
			}
			return "", nil, fmt.Errorf("no extension loaded by its path")
		},
		recorder: newPiRecorder,
	}
}

// piExtensionServer reads the crewlet server out of the extension as the
// profile writes it: `command:` and `args:` lines carrying JSON values.
func piExtensionServer(ext string) (string, []string, error) {
	var command string
	var args []string
	for line := range strings.SplitSeq(ext, "\n") {
		line = strings.TrimSuffix(strings.TrimSpace(line), ",")
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
		return "", nil, fmt.Errorf("no crewlet server registered")
	}
	return command, args, nil
}

type piRecorder struct {
	session *json.Encoder
	model   string
	seq     int
}

func newPiRecorder(_ fakeScript, _ []byte) (fakeRecorder, error) {
	dir := argAfter("--session-dir")
	if dir == "" {
		return nil, fmt.Errorf("no --session-dir")
	}
	session, err := recordFile(filepath.Join(dir, "2026-10-25T00-00-00-000Z_0001.jsonl"))
	if err != nil {
		return nil, err
	}
	cwd, _ := os.Getwd()
	_ = session.Encode(map[string]any{"type": "session", "version": 3, "id": "0001", "cwd": cwd})
	return &piRecorder{session: session}, nil
}

func (p *piRecorder) entry(message map[string]any) {
	p.seq++
	_ = p.session.Encode(map[string]any{"type": "message", "id": fmt.Sprintf("%08x", p.seq), "message": message})
}

func (p *piRecorder) begin(model string, _ []string) {
	p.model = model
	p.entry(map[string]any{"role": "user", "content": "do the task"})
}

func (p *piRecorder) asked(_ int, r fakeResponse, ids []string) {
	content := []any{}
	if r.Text != "" {
		content = append(content, map[string]any{"type": "text", "text": r.Text})
	}
	for j, c := range r.Calls {
		name := "mcp__crewlet__" + c.Name
		if c.Own {
			name = c.Name
		}
		content = append(content, map[string]any{"type": "toolCall", "id": ids[j], "name": name, "arguments": orEmpty(c.Args)})
	}
	stop := "stop"
	if len(r.Calls) > 0 {
		stop = "toolUse"
	}
	// The prompt WITHOUT its cached part, which is counted apart.
	p.entry(map[string]any{"role": "assistant", "content": content, "model": p.model, "stopReason": stop,
		"usage": map[string]any{"input": r.Usage.Input, "output": r.Usage.Output,
			"cacheRead": r.Usage.CacheRead, "cacheWrite": r.Usage.CacheWrite}})
}

func (p *piRecorder) answered(_ int, _ fakeResponse, ids []string, results []string) {
	for j, id := range ids {
		p.entry(map[string]any{"role": "toolResult", "toolCallId": id,
			"content": []any{map[string]any{"type": "text", "text": results[j]}}})
	}
}

func (p *piRecorder) end(script fakeScript, _ fakeUsage, last string) int {
	if script.Exit == 0 {
		fmt.Println(last)
	}
	return script.Exit
}

// PI'S RUN IS READ FROM ITS OWN SESSION FILE, AND ITS HOME STARTS NO SERVER OF ITS
// OWN. A run with tools keeps its session in its own directory, where the CLI
// appends each message as it completes — each response's prose, calls and
// counts, the prompt less its cached part — and prints the plain answer as a
// text function does. The server is registered by an extension the run loads
// by its path; the agent directory's mcp.json, whose servers the CLI starts
// whenever its MCP support is on, is the profile's own, empty, whatever was
// there. The CLI sends no id with a call, so a call is joined by name and
// arguments; the host is told the CLI waits the profile's 600 seconds for a
// call, re-reads the list when told it changed, and makes a response's calls
// at once.
//
// Mutation: drop the route's --session-dir, and no record is read; drop the
// mcp.json seed, and the planted server starts (the fake fails the run); read
// the record's input as the whole prompt, and the run is charged 100 tokens
// less than it spent.
func TestPisRunIsReadFromItsOwnSessionFile(t *testing.T) {
	t.Parallel()
	p := fakeAs(t, "pi", routeCase, func(c *Config) { c.Auth = Auth{Mode: AuthSubscription} })
	planted := filepath.Join(p.Workspace().SeatDir(llm.SeatOf(t.Context())), "home", ".pi", "agent", "mcp.json")
	writeFile(t, planted, `{"mcpServers":{"planted":{"command":"sh"}}}`)
	holdsTheRoute(t, p, llm.ServeOptions{
		Relists: true, ToolTimeout: 10 * time.Minute, ObservesRequests: true, Concurrency: llm.CallsAtOnce,
	}, llm.Usage{Input: 340, Output: 29, CacheRead: 100})
}

// A FAILED PI RUN SAYS WHY ON STDERR, and is judged by it: its session records
// the failed request with nothing in it, and the CLI prints the provider's
// error and exits 1 — where a spent plan's words are read, as a text
// function's are. (Its --mode json stream, the other record it could have
// been read from, exits 0 with nothing on stderr on a failed request.)
//
// Mutation: drop rate_limit_error from the profile's limit markers, and the
// spent plan is a server failure.
func TestAFailedPiRunSaysWhyOnStderr(t *testing.T) {
	t.Parallel()
	p := fakeAs(t, "pi", fakeScript{Exit: 1,
		Stderr:    `429: {"message":"slow down","type":"rate_limit_error","code":"rate_limit_exceeded"}` + "\n",
		Responses: []fakeResponse{{Calls: []fakeCall{{Name: "search"}}, Usage: fakeUsage{Input: 5, Output: 5}}}},
		func(c *Config) { c.Auth = Auth{Mode: AuthSubscription} })
	host := newTestHost(t, "search")
	host.handle("search", func(context.Context, map[string]any) (string, bool) { return "3 results", false })
	_, err := p.Complete(t.Context(), phase(host))
	if kind := llm.KindOf(err); kind != llm.KindRateLimit {
		t.Fatalf("err = %v (%v), want the spent plan the CLI reported on stderr", err, kind)
	}
}
