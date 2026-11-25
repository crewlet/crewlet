package executor

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/sandbox"
)

// guardHookInput is a PreToolUse hook input as Claude Code 2.1.330 writes it
// (measured): JSON.stringify's compact form, its keys in this order, with
// `mcp_server` last and absent where the release states none ("").
func guardHookInput(agent, toolInput, server string) string {
	in := `{"session_id":"s","transcript_path":"/h/t.jsonl","cwd":"/w","prompt_id":"p","permission_mode":"bypassPermissions",`
	if agent != "" {
		in += `"agent_id":"a1","agent_type":"` + agent + `",`
	}
	in += `"effort":{"level":"medium"},"hook_event_name":"PreToolUse","tool_name":"mcp__crewlet__submit_work",` +
		`"tool_input":` + toolInput + `,"tool_use_id":"toolu_3"`
	if server != "" {
		in += `,"mcp_server":` + server
	}
	return in + "}"
}

// runGuardSh runs script under sh with stdin, in dir, answering its stdout.
func runGuardSh(t *testing.T, script, stdin string) (string, error) {
	t.Helper()
	cmd := exec.CommandContext(t.Context(), "sh", "-c", script)
	cmd.Stdin = strings.NewReader(stdin)
	out, err := cmd.Output()
	return string(out), err
}

// ONLY THE BRIDGE ITSELF SERVES A CALL NAMED INSIDE ITS NAMES. Claude Code
// 2.1.330 connects a server an agent's definition declares (`mcpServers`) for
// that agent, and one named `crewlet` serves every bridge name the agent's
// `tools:` leaves out (measured: the call reached the agent's server). The
// guard reads the server off the hook input's END, where `mcp_server` follows
// the model's arguments, and lets through only the run's own `--mcp-config`
// server of the bridge's name (source `dynamic`); anything else is refused, the
// loop stopped, and the source and agent recorded once.
//
// Mutations, each red: match the server's name alone; match the suffix
// anywhere in the input (the forged arguments); let an input with no
// `mcp_server` through; record nothing.
func TestOnlyTheBridgeItselfServesACallNamedInsideItsNames(t *testing.T) {
	t.Parallel()
	const bridge = `{"name":"crewlet","source":"dynamic"}`
	forged := `{"x":"y","tool_use_id":"z","mcp_server":` + bridge + `}`
	for _, tc := range []struct {
		name, input, marked string
	}{
		{"the bridge's own call", guardHookInput("", `{}`, bridge), ""},
		{"the bridge's call from a subagent", guardHookInput("shadow", `{}`, bridge), ""},
		{"an agent's own crewlet", guardHookInput("shadow", `{}`, `{"name":"crewlet","source":"agent"}`), "agent shadow"},
		{"a managed crewlet", guardHookInput("", `{}`, `{"name":"crewlet","source":"enterprise"}`), "enterprise "},
		{"arguments forging the bridge's end", guardHookInput("shadow", forged,
			`{"name":"crewlet","source":"agent"}`), "agent shadow"},
		{"a release that names no server", guardHookInput("", `{}`, ""), "unstated "},
		{"an agent's name a repository wrote", guardHookInput(`sh\"adow; rm -rf`, `{}`,
			`{"name":"crewlet","source":"agent"}`), "agent sh"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			marker := filepath.Join(dir, "claude-unbridged")
			script, err := claudeGuardScript([]string{"crewlet"}, marker)
			if err != nil {
				t.Fatal(err)
			}
			out, err := runGuardSh(t, script, tc.input)
			if err != nil {
				t.Fatalf("the guard: %v", err)
			}
			wrote, _ := os.ReadFile(marker)
			if tc.marked == "" {
				if out != "" || len(wrote) != 0 {
					t.Errorf("the guard answered %q and marked %q, want the call let through", out, wrote)
				}
				return
			}
			var answer struct {
				Continue *bool `json:"continue"`
				Specific struct {
					Decision string `json:"permissionDecision"`
				} `json:"hookSpecificOutput"`
			}
			if err := json.Unmarshal([]byte(out), &answer); err != nil || answer.Continue == nil || *answer.Continue ||
				answer.Specific.Decision != "deny" {
				t.Errorf("the guard answered %q, want the call denied and the loop stopped", out)
			}
			if got := strings.TrimSuffix(string(wrote), "\n"); got != tc.marked {
				t.Errorf("the guard marked %q, want %q", got, tc.marked)
			}
		})
	}
}

// A GUARDED RUN IS HANDED THE GUARD ON EVERY RELEASE. The run's script swaps
// whichever settings file the gates chose for its guarded copy, whatever
// release the box runs (a run is started on the newest Claude Code alone, and
// no release is let start without the guard); a run with no bridge never
// names a guarded copy.
//
// Mutations, each red: gate the guarded copy on a release; leave the notes'
// file out of the guarded arms.
func TestAGuardedRunIsHandedTheGuardOnEveryRelease(t *testing.T) {
	t.Parallel()
	p := Paths{Home: "/home/box"}
	gate := func(guarded bool) string {
		return claudePinGate(p, true, guarded)
	}
	for _, tc := range []struct {
		release, chosen, want string
	}{
		{"2.1.330", p.ClaudeSettings(), p.ClaudeSettingsGuarded()},
		{"2.1.300", p.ClaudeSettings(), p.ClaudeSettingsGuarded()},
		{"2.1.330", p.ClaudeNotesSettings(), p.ClaudeNotesSettingsGuarded()},
		{"2.1.300", p.ClaudeNotesSettings(), p.ClaudeNotesSettingsGuarded()},
	} {
		script := `claude() { echo '` + tc.release + ` (Claude Code)'; }` + "\n" + claudeRelease + "cs='" + tc.chosen + "'\n" +
			gate(true) + `echo "$cs"`
		out, err := exec.CommandContext(t.Context(), "sh", "-c", script).Output()
		if err != nil {
			t.Fatalf("%s: the gate: %v\n%s", tc.release, err, script)
		}
		if got := strings.TrimSpace(string(out)); got != tc.want {
			t.Errorf("on %s %s became %s, want %s", tc.release, tc.chosen, got, tc.want)
		}
	}
	if strings.Contains(gate(false), "guarded") {
		t.Errorf("a run with no bridge names a guarded copy:\n%s", gate(false))
	}
}

// A RUN WITH A BRIDGE HAS GUARDED SETTINGS: the pin and a PreToolUse command
// hook on every name inside the bridge's running the guard's script, hooks
// left as the box sets them (a box that switches them off refuses the run,
// [claudeHooksOff]); the notes' copy keeps the notes' hooks beside it. A run
// with no bridge has neither file nor script.
//
// Mutations, each red: switch hooks on; match every tool; drop the notes'
// hooks from their guarded copy.
func TestARunWithABridgeHasGuardedSettings(t *testing.T) {
	t.Parallel()
	box := sandbox.NewFakeSandbox("box-1")
	p := PathsFor(box)
	req := sandbox.RunRequest{Brief: "x", Steer: sandbox.SteerWiring{Server: "crewlet", Tool: "steer_take",
		URL: "http://127.0.0.1:7/steer/tok"},
		MCPServers: map[string]sandbox.MCPServer{
			"crewlet": {Transport: sandbox.TransportHTTP, URL: "http://127.0.0.1:7/mcp/tok", AlwaysLoaded: true}}}
	if _, err := (ClaudeCode{}).WriteConfig(t.Context(), box, req, Session{}, p); err != nil {
		t.Fatalf("WriteConfig: %v", err)
	}
	read := func(file string) map[string]any {
		raw, err := box.ReadFile(t.Context(), file)
		if err != nil {
			t.Fatalf("read %s: %v", file, err)
		}
		var doc map[string]any
		if err = json.Unmarshal(raw, &doc); err != nil {
			t.Fatalf("%s: %v", file, err)
		}
		return doc
	}
	want := `[{"hooks":[{"command":"sh '` + p.ClaudeBridgeGuard() + `'","timeout":10,"type":"command"}],"matcher":"^mcp__crewlet__"}]`
	for _, file := range []string{p.ClaudeSettingsGuarded(), p.ClaudeNotesSettingsGuarded()} {
		doc := read(file)
		hooks, _ := doc["hooks"].(map[string]any)
		guard, _ := json.Marshal(hooks["PreToolUse"])
		if string(guard) != want {
			t.Errorf("%s's PreToolUse is %s, want %s", file, guard, want)
		}
		if off, set := doc["disableAllHooks"]; set {
			t.Errorf("%s sets disableAllHooks to %v, which would run the home's and the repository's hooks "+
				"where a person's session runs none", file, off)
		}
		if doc["env"] == nil {
			t.Errorf("%s holds no pin", file)
		}
	}
	notes, _ := read(p.ClaudeNotesSettingsGuarded())["hooks"].(map[string]any)
	if notes["PostToolBatch"] == nil || notes["Stop"] == nil {
		t.Errorf("the notes' guarded copy lost the notes' hooks: %v", notes)
	}
	if got, err := box.ReadFile(t.Context(), p.ClaudeBridgeGuard()); got == nil {
		t.Errorf("no guard script: %v", err)
	}

	plain := sandbox.NewFakeSandbox("box-2")
	q := PathsFor(plain)
	if _, err := (ClaudeCode{}).WriteConfig(t.Context(), plain, sandbox.RunRequest{Brief: "x",
		MCPServers: map[string]sandbox.MCPServer{"tools": {Transport: sandbox.TransportHTTP,
			URL: "http://127.0.0.1:7/x"}}}, Session{}, q); err != nil {
		t.Fatalf("WriteConfig: %v", err)
	}
	for _, file := range []string{q.ClaudeSettingsGuarded(), q.ClaudeBridgeGuard()} {
		if got, _ := plain.ReadFile(t.Context(), file); got != nil {
			t.Errorf("a run with no bridge has %s", file)
		}
	}
}

// A CALL THE GUARD REFUSED FAILS THE RUN, ITS SPEND KEPT. A subagent's refused
// call leaves the run to finish as a success (measured on 2.1.330), so the
// script wraps the harness's last line, its result, in the guard's line: the
// run fails in the guard's words, naming the source and agent, and is charged
// what the result says. A last line that is no object is wrapped as null.
//
// Mutations, each red: leave the marker unread; read the guard's line as the
// result's; drop the inner result's cost.
func TestACallTheGuardRefusedFailsTheRunItsSpendKept(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	p := Paths{Home: dir}
	if err := os.MkdirAll(p.WorkDir(), 0o755); err != nil {
		t.Fatal(err)
	}
	before, after := claudeGuardLines(p)
	result := `{"type":"result","subtype":"success","is_error":false,"result":"done","session_id":"s1","total_cost_usd":0.5,"num_turns":2}`
	for _, tc := range []struct {
		name, last string
		cost       float64
	}{
		{"a result", result, 0.5},
		{"no object", "Killed", 0},
	} {
		if err := os.WriteFile(p.Stream(), []byte(`{"type":"system"}`+"\n"+tc.last+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p.ClaudeUnbridged(), []byte("agent shadow\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		out, err := runGuardSh(t, after, "")
		if err != nil {
			t.Fatalf("%s: the script's line: %v", tc.name, err)
		}
		res := (ClaudeCode{}).Parse([]byte(strings.TrimSpace(out)))
		if res.Success || !strings.HasPrefix(res.Error, claudeShadowed) || !strings.Contains(res.Error, "the agent shadow") ||
			!strings.Contains(res.Error, `"agent"`) {
			t.Errorf("%s: the run is %+v, want a failure in the guard's words", tc.name, res)
		}
		if res.CostUSD != tc.cost {
			t.Errorf("%s: the run cost %v, want the result's %v", tc.name, res.CostUSD, tc.cost)
		}
		d := &claudeEvents{}
		d.Line([]byte(out))
		if got := d.Result().Transcript; !strings.Contains(got, claudeShadowed) {
			t.Errorf("%s: the transcript %q does not say why the run ended", tc.name, got)
		}
		if _, err := runGuardSh(t, before, ""); err != nil {
			t.Fatal(err)
		}
		if _, err := os.Stat(p.ClaudeUnbridged()); err == nil {
			t.Errorf("the marker survives the next run's start")
		}
	}
	if out, _ := runGuardSh(t, after, ""); out != "" {
		t.Errorf("a run the guard refused nothing of prints %q", out)
	}
}

// A RUN HANDED SERVERS IS NOT STARTED BESIDE A MANAGED MCP CONFIGURATION.
// Claude Code 2.1.330 refuses `--mcp-config` beside a readable
// managed-mcp.json, and beside one it cannot read or parse, or one the run's
// account can write, drops the run's servers and starts without them, exit 0
// (measured). The gate refuses any such file, a dangling link included, naming
// it, before the harness; a run with no servers is not held to it.
//
// Mutations, each red: test the file for being readable; drop the gate; drop
// the guard's lines around the harness.
func TestARunHandedServersIsNotStartedBesideAManagedMCPConfiguration(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	file := filepath.Join(dir, "managed-mcp.json")
	gate := strings.ReplaceAll(claudeManagedGate(), claudeManagedMCP, file)
	for _, tc := range []struct {
		name    string
		make    func() error
		refused bool
	}{
		{"none", func() error { return nil }, false},
		{"an unreadable one", func() error { return os.WriteFile(file, []byte("{"), 0o000) }, true},
		{"a dangling link", func() error { _ = os.Remove(file); return os.Symlink(filepath.Join(dir, "gone"), file) }, true},
	} {
		if err := tc.make(); err != nil {
			t.Fatal(err)
		}
		cmd := exec.CommandContext(t.Context(), "sh", "-c", gate+"echo started")
		var stderr strings.Builder
		cmd.Stderr = &stderr
		out, _ := cmd.Output()
		if refused := !strings.Contains(string(out), "started"); refused != tc.refused ||
			(tc.refused && !strings.Contains(stderr.String(), file)) {
			t.Errorf("%s: started %q, said %q", tc.name, out, stderr.String())
		}
	}
	p := Paths{Home: "/home/box"}
	req := sandbox.RunRequest{Brief: "x", MCPServers: map[string]sandbox.MCPServer{
		"crewlet": {Transport: sandbox.TransportHTTP, URL: "http://127.0.0.1:7/mcp/tok", AlwaysLoaded: true}}}
	script := claudeScript(req, Session{ID: "s"}, p, p.MCPConfig(), sandbox.JobStopGrace)
	at, launch := strings.Index(script, claudeManagedMCP), strings.Index(script, "claude -p")
	if at < 0 || at > launch {
		t.Errorf("the script does not refuse a managed MCP configuration before the harness:\n%s", script)
	}
	cleared, wrapped := strings.Index(script, "rm -f '"+p.ClaudeUnbridged()+"'"), strings.Index(script, claudeUnbridgedEvent)
	if cleared < 0 || cleared > launch || wrapped < launch {
		t.Errorf("the script does not clear the guard's marker before the harness and read it after:\n%s", script)
	}
	if strings.Contains(claudeScript(sandbox.RunRequest{Brief: "x"}, Session{ID: "s"}, p, "", sandbox.JobStopGrace), claudeManagedMCP) {
		t.Errorf("a run handed no servers is held to the managed configuration")
	}
}
