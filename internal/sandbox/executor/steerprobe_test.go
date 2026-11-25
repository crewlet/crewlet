package executor_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/crewlet/crewlet/internal/agent/turnctx"
	"github.com/crewlet/crewlet/internal/api/mcpbridge"
	"github.com/crewlet/crewlet/internal/org"
	"github.com/crewlet/crewlet/internal/runtoken"
	"github.com/crewlet/crewlet/internal/sandbox"
	"github.com/crewlet/crewlet/internal/sandbox/executor"
	"github.com/crewlet/crewlet/internal/tools"
)

// probeInbox answers one note from ada at every boundary it is asked at, and
// records each event.
type probeInbox struct {
	mu    sync.Mutex
	takes []string
}

const probeNote = "## A note from ada, sent while this turn was running\n\nTarget the release branch."

// probeTake is the take every answer of the probe's inbox names.
const probeTake = "5e2d1c0b-9a87-4f65-b432-10fedcba9876"

func (i *probeInbox) Cancelled(context.Context, sandbox.NoteCall) {}

func (i *probeInbox) Again(context.Context, []string) (map[string]string, []string, error) {
	return nil, nil, nil
}

func (i *probeInbox) Folded(context.Context, []string) {}

func (i *probeInbox) Take(_ context.Context, event string, _ *sandbox.NoteCall) string {
	i.mu.Lock()
	defer i.mu.Unlock()
	i.takes = append(i.takes, event)
	return executor.HookAnswer(executor.ClaudeCodeName, event, probeTake, []executor.HookNote{{By: "ada", Message: probeNote}})
}

func (i *probeInbox) Wait(context.Context, int) ([]mcpbridge.SteerNote, <-chan struct{}) {
	return nil, make(chan struct{})
}

func (i *probeInbox) Read(context.Context, []string) error { return nil }

func (i *probeInbox) Close() {}

func (i *probeInbox) asked() []string {
	i.mu.Lock()
	defer i.mu.Unlock()
	return slices.Clone(i.takes)
}

// probeLedger records nothing a case reads: steer_take is never recorded.
type probeLedger struct {
	mu    sync.Mutex
	calls []mcpbridge.Call
}

// Begin takes every call's entry, and Withdraw every withdrawal.
func (l *probeLedger) Begin(context.Context, string, mcpbridge.Begun) error    { return nil }
func (l *probeLedger) Withdraw(context.Context, string, mcpbridge.Begun) error { return nil }

func (l *probeLedger) Record(_ context.Context, _ string, c mcpbridge.Call) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.calls = append(l.calls, c)
	return nil
}

// probeBridge serves a real tool bridge over HTTP with one run's session open
// on inbox, and answers the run's endpoint.
func probeBridge(t *testing.T, inbox mcpbridge.SteerInbox, ledger mcpbridge.Ledger) string {
	t.Helper()
	server := httptest.NewServer(http.NotFoundHandler())
	t.Cleanup(server.Close)
	bridge, err := mcpbridge.New(mcpbridge.Options{
		Material: runtoken.OneKey("k", "probe-key"), BaseURL: server.URL,
	})
	if err != nil {
		t.Fatalf("mcpbridge.New: %v", err)
	}
	mux := http.NewServeMux()
	mux.Handle(mcpbridge.PathPrefix+"{token}", bridge.Handler())
	mux.Handle(mcpbridge.SteerPattern, bridge.SteerHandler())
	server.Config.Handler = mux
	seat := &org.Role{Name: "Engineer", DeclaredHandle: "dev"}
	turn := &turnctx.Turn{RunID: "run-1", Seat: seat,
		Org: &org.Organization{Name: "Nimbus", Roles: []*org.Role{seat}}}
	endpoint := bridge.Open(t.Context(), &mcpbridge.Session{
		RunID: "run-1", Launch: "job-1", Handle: "dev", Role: "Engineer",
		Surface: tools.NewSurface("execute", tools.NewRegistry().Snapshot(), nil).ForTurn(turn),
		Ledger:  ledger, Steer: inbox,
	})
	if endpoint == "" {
		t.Fatal("the bridge minted no endpoint for the run")
	}
	return endpoint
}

// A REAL BOX'S HOOKS REACH THE BRIDGE'S TOOL: the settings a run in a real
// `direct` box was named are played as the harness plays them, over a real MCP
// client against a real bridge, so the file and the bridge are held to each
// other rather than to copies of each other's names.
//
// It holds: the brief is on stdin, never argv; --settings names the hooks'
// file; the variables that switch hooks off never reach the harness; each main
// agent boundary is answered with the note; a subagent's takes nothing.
//
// Mutation: rename the hook input's `event` key and no boundary takes the note;
// drop Output.Unset and the variables reach the harness.
func realBoxsHooksReachTheBridgeTool(t *testing.T) {
	t.Parallel()
	inbox := &probeInbox{}
	ledger := &probeLedger{}
	endpoint := probeBridge(t, inbox, ledger)

	runner, box := realBox(t)
	p := executor.PathsFor(box)
	record := t.TempDir()
	installClaude(t, box, `d='`+record+`'
for a in "$@"; do printf '%s\n' "$a"; done > "$d/argv"
env > "$d/env"
cat > "$d/stdin"
prev=
for a in "$@"; do
  if [ "$prev" = --settings ]; then cp "$a" "$d/settings.json"; fi
  if [ "$prev" = --mcp-config ]; then cp "$a" "$d/mcp.json"; fi
  prev=$a
done
echo '{"type":"result","subtype":"success","is_error":false,"result":"done","terminal_reason":"completed"}'`)

	const brief = "fix the flake: \"quoted\", $dollar and `ticks`"
	waitRun(t, runner, box, sandbox.RunRequest{
		Brief: brief, System: "You are the seat.",
		Env: map[string]string{"CLAUDE_CODE_SIMPLE": "1", "CLAUDE_CODE_SAFE_MODE": "1"},
		MCPServers: map[string]sandbox.MCPServer{
			"crewlet": {Transport: sandbox.TransportHTTP, URL: endpoint},
		},
		Steer: sandbox.SteerWiring{Server: "crewlet", Tool: mcpbridge.SteerTool,
			URL: mcpbridge.SteerURL(endpoint)},
	})

	argv := lines(t, filepath.Join(record, "argv"))
	if argv[0] != "-p" || slices.Contains(argv, brief) {
		t.Errorf("the brief is on the harness's argv: %q", argv)
	}
	// The pinned copy, which the stand-in's 2.1.310 takes ([claudePinGate]).
	if got := after(argv, "--settings"); got != p.ClaudeNotesSettingsPinned() {
		t.Errorf("--settings names %q, want %q", got, p.ClaudeNotesSettingsPinned())
	}
	if stdin := strings.Join(lines(t, filepath.Join(record, "stdin")), "\n"); stdin != brief {
		t.Errorf("the harness read %q on stdin, want the brief", stdin)
	}
	for _, kv := range lines(t, filepath.Join(record, "env")) {
		if strings.HasPrefix(kv, "CLAUDE_CODE_SIMPLE=") || strings.HasPrefix(kv, "CLAUDE_CODE_SAFE_MODE=") {
			t.Errorf("the harness started with %s, which switches its hooks off", kv)
		}
	}

	var settings struct {
		DisableAllHooks *bool `json:"disableAllHooks"`
		Hooks           map[string][]struct {
			Hooks []struct {
				Type, Server, Tool string
				Input              map[string]string
				Timeout            int
			} `json:"hooks"`
		} `json:"hooks"`
	}
	readJSON(t, filepath.Join(record, "settings.json"), &settings)
	if settings.DisableAllHooks != nil {
		t.Errorf("the settings switch hooks on over the box's own: disableAllHooks %v", *settings.DisableAllHooks)
	}
	var servers struct {
		Servers map[string]struct{ URL string } `json:"mcpServers"`
	}
	readJSON(t, filepath.Join(record, "mcp.json"), &servers)

	// EACH HOOK, PLAYED AS THE HARNESS PLAYS IT.
	play := func(event, agent string) []string {
		var answers []string
		for _, matcher := range settings.Hooks[event] {
			for _, hook := range matcher.Hooks {
				if hook.Type != "mcp_tool" {
					t.Fatalf("a %s hook is %q, not a call of the bridge's tool", event, hook.Type)
				}
				server, ok := servers.Servers[hook.Server]
				if !ok {
					t.Fatalf("the %s hook calls server %q, which the run's MCP config does not name",
						event, hook.Server)
				}
				args := map[string]any{}
				for k, v := range hook.Input {
					v = strings.ReplaceAll(v, "${hook_event_name}", event)
					args[k] = strings.ReplaceAll(v, "${agent_id}", agent)
				}
				answers = append(answers, callHookTool(t, server.URL, hook.Tool, args))
			}
		}
		return answers
	}
	for _, event := range []string{"PostToolBatch", "Stop"} {
		answers := play(event, "")
		if len(answers) != 1 {
			t.Fatalf("%s has %d hooks, want one", event, len(answers))
		}
		var answer struct {
			HookSpecificOutput struct {
				HookEventName     string `json:"hookEventName"`
				AdditionalContext string `json:"additionalContext"`
			} `json:"hookSpecificOutput"`
			SystemMessage string `json:"systemMessage"`
		}
		if err := json.Unmarshal([]byte(answers[0]), &answer); err != nil {
			t.Fatalf("the %s hook's answer is not a hook's JSON: %v\n%s", event, err, answers[0])
		}
		if answer.HookSpecificOutput.HookEventName != event ||
			answer.HookSpecificOutput.AdditionalContext != probeNote {
			t.Errorf("the %s hook was answered %+v, want the note as that event's additional context",
				event, answer.HookSpecificOutput)
		}
	}
	// A SUBAGENT'S BOUNDARY takes nothing: the note waits for the main agent.
	if answers := play("PostToolBatch", "agent-7"); len(answers) != 1 || answers[0] != "" {
		t.Errorf("a subagent's batch was answered %q", answers)
	}
	if got := inbox.asked(); !slices.Equal(got, []string{"PostToolBatch", "Stop"}) {
		t.Errorf("the inbox was asked at %v, want the main agent's two boundaries", got)
	}
}

// callHookTool calls tool on the MCP server at url as a hook does, and answers
// its text.
func callHookTool(t *testing.T, url, tool string, args map[string]any) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	session, err := mcp.NewClient(&mcp.Implementation{Name: "claude-code", Version: "probe"}, nil).
		Connect(ctx, &mcp.StreamableClientTransport{Endpoint: url}, nil)
	if err != nil {
		t.Fatalf("connect to the bridge: %v", err)
	}
	defer func() { _ = session.Close() }()
	res, err := session.CallTool(ctx, &mcp.CallToolParams{Name: tool, Arguments: args})
	if err != nil {
		t.Fatalf("call %s: %v", tool, err)
	}
	if res.IsError {
		t.Fatalf("%s answered an error, which makes the harness re-initialise its bridge", tool)
	}
	var out strings.Builder
	for _, c := range res.Content {
		if text, ok := c.(*mcp.TextContent); ok {
			out.WriteString(text.Text)
		}
	}
	return out.String()
}

// readJSON decodes the file at path into v.
func readJSON(t *testing.T, path string, v any) {
	t.Helper()
	raw := strings.Join(lines(t, path), "\n")
	if err := json.Unmarshal([]byte(raw), v); err != nil {
		t.Fatalf("decode %s: %v\n%s", path, err, raw)
	}
}
