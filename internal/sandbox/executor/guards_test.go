package executor_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/sandbox"
	"github.com/crewlet/crewlet/internal/sandbox/executor"
)

// switchOffCase is one row of a guard's table: the files planted by their
// path under the box home (`workspace/` is the start directory), and either
// nothing wanted in the failure (the run passes) or every piece of it.
type switchOffCase struct {
	plant map[string]string
	fails []string
}

// claudeInitThenWait is a stand-in `claude` that prints the init event Claude
// Code 2.1.330 prints for a run whose tools and servers it reports as tools
// and servers (measured per case), and then answers as a finished run unless
// switchedOff: one whose bridge is switched off waits for good, so only the
// guard ends it.
func claudeInitThenWait(tools, servers string, switchedOff bool) string {
	wait := ""
	if switchedOff {
		wait = "while :; do sleep 1; done\n"
	}
	return `#!/bin/sh
case "$1" in --version) echo '2.1.330 (Claude Code)'; exit 0;; esac
cat > /dev/null
printf '%s\n' '{"type":"system","subtype":"init","cwd":"/w","session_id":"s","tools":` + tools +
		`,"mcp_servers":` + servers + `,"model":"m"}'
` + wait + `echo '{"type":"result","subtype":"success","is_error":false,"result":"done"}'
`
}

// A HOME OR A REPOSITORY CANNOT SWITCH CLAUDE CODE'S BRIDGE OFF UNSEEN. Claude
// Code 2.1.330 has no layer above a `deniedMcpServers` entry or a
// `permissions.deny` one in any settings file, or a `disabledMcpServers` entry
// for the start directory in the home's `.claude.json` (measured), so a run
// whose init event reports the bridge missing, disabled or failed, or
// connected with none of its tools, is ended as it starts, its failure naming
// the file; one whose bridge connected with its tools, or is still connecting,
// runs to its end, a deny of another server's tools included.
//
// Mutations, each red: launch without the guard (the switched-off runs never
// end); read `disabled` as connected; read a connected server as whole
// without its tools; name no settings file.
func TestClaudeCodesBridgeCannotBeSwitchedOffUnseen(t *testing.T) {
	t.Parallel()
	const denied = `{"deniedMcpServers":[{"serverName":"crewlet"}]}`
	const bridgeTools, otherTools = `["Bash","mcp__crewlet__report"]`, `["Bash","mcp__other__x"]`
	entry := func(status string) string {
		return `[{"name":"crewlet","status":"` + status + `","source":"dynamic"}]`
	}
	for name, tc := range map[string]struct {
		tools, servers string
		switchOffCase
	}{
		"nothing switches it off": {tools: bridgeTools, servers: entry("connected")},
		"it is still connecting":  {tools: `["Bash"]`, servers: entry("pending")},
		"the home denies another server's tools": {tools: bridgeTools, servers: entry("connected"),
			switchOffCase: switchOffCase{plant: map[string]string{
				".claude/settings.json": `{"permissions":{"deny":["mcp__other"],"allow":["mcp__crewlet"]}}`}}},
		"the repository denies the bridge's tools": {tools: otherTools, servers: entry("connected"),
			switchOffCase: switchOffCase{
				plant: map[string]string{"workspace/.claude/settings.json": `{"permissions":{"deny":["mcp__crewlet"]}}`},
				fails: []string{"started with Crewlet's MCP server crewlet", "connected, and none of its tools",
					"permissions.deny", "(mcp__crewlet)", "/workspace/.claude/settings.json"}}},
		"the home's settings deny it": {tools: `["Bash"]`, servers: `[]`, switchOffCase: switchOffCase{
			plant: map[string]string{".claude/settings.json": denied},
			fails: []string{"started without Crewlet's MCP server crewlet", "/.claude/settings.json"}}},
		"the repository's local settings deny it": {tools: `["Bash"]`, servers: `[]`, switchOffCase: switchOffCase{
			plant: map[string]string{"workspace/.claude/settings.local.json": denied},
			fails: []string{"started without Crewlet's MCP server crewlet", "/workspace/.claude/settings.local.json"}}},
		"the home's state disables it for the start directory": {tools: `["Bash"]`, servers: entry("disabled"),
			switchOffCase: switchOffCase{plant: map[string]string{".claude.json": `{"projects":{}}`},
				fails: []string{"crewlet", "disabled: ", "/.claude.json disables it", "disabledMcpServers"}}},
		"it did not connect": {tools: `["Bash"]`, servers: entry("failed"), switchOffCase: switchOffCase{
			fails: []string{"without Crewlet's MCP server crewlet", "connected: its init event reports it failed"}}},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			runner, box := realBox(t)
			writeBoxStandIn(t, box, executor.PathsFor(box).BinDir()+"/claude",
				claudeInitThenWait(tc.tools, tc.servers, len(tc.fails) > 0))
			plant(t, box, tc.plant)
			_, res := runToEnd(t, runner, box, sandbox.RunRequest{Brief: "go", MCPServers: loadsBridge})
			checkSwitchOff(t, box, res, tc.switchOffCase)
		})
	}
}

// checkSwitchOff holds a run's result to c: a success where c wants no
// failure, else a failure saying each of c.fails, the box home's path in
// place of a leading `/` of a planted file's.
func checkSwitchOff(t *testing.T, box sandbox.Sandbox, res sandbox.Result, c switchOffCase) {
	t.Helper()
	if len(c.fails) == 0 {
		if !res.Success {
			t.Fatalf("a run nothing switched off failed: %s", res.Error)
		}
		return
	}
	if res.Success {
		t.Fatalf("a run whose own server or hook was switched off succeeded: %+v", res)
	}
	for _, want := range c.fails {
		if strings.HasPrefix(want, "/") && !strings.Contains(want, "disables") {
			want = box.Home() + want
		}
		if !strings.Contains(res.Error, want) {
			t.Errorf("the run's failure does not say %q:\n%s", want, res.Error)
		}
	}
}

// A HOME OR A REPOSITORY CANNOT SWITCH GEMINI CLI'S BRIDGE OR HOOK OFF UNSEEN.
// Gemini CLI 0.70.0 adds `mcp.excluded`, `tools.exclude` and
// `hooksConfig.disabled` up across its settings files and intersects
// `mcp.allowed`, so a run whose home's or working directory's settings list
// the bridge, a tool of its own or its hook that way is refused before Gemini
// CLI starts, naming the file; a list naming something else, or comments, or
// `hooksConfig.enabled: false` (which the run's settings overrule), start it.
// The run's settings switch hooks on, and its admin-tier policy allows the
// bridge's tools over a home's or a repository's rule.
// Mutations, each red: drop the guard from the script; match a server by
// case; skip the working directory's file; drop `hooksConfig.enabled` or the
// admin policy.
func TestGeminiCLIsBridgeAndHookCannotBeSwitchedOffUnseen(t *testing.T) {
	t.Parallel()
	const reason = "Gemini CLI would start without what the run's notes and tools go through"
	for name, tc := range map[string]switchOffCase{
		"nothing switches it off": {},
		"lists of other things, comments, and hooks switched off whole": {plant: map[string]string{
			".gemini/settings.json": "// the home's own\n{\"mcp\": {\"excluded\": [\"other\"]}, /* kept */ " +
				"\"tools\": {\"exclude\": [\"mcp_other_x\"]}, \"hooksConfig\": {\"enabled\": false, \"disabled\": [\"other\"]}}",
			"workspace/.gemini/settings.json": `{"mcp": {"allowed": ["crewlet", "other"]}}`}},
		"the home excludes the bridge": {plant: map[string]string{".gemini/settings.json": `{"mcp":{"excluded":[" Crewlet"]}}`},
			fails: []string{reason, "/.gemini/settings.json: its mcp.excluded names the server crewlet"}},
		"the repository allows only another server": {
			plant: map[string]string{"workspace/.gemini/settings.json": `{"mcp":{"allowed":["other"]}}`},
			fails: []string{reason, "/workspace/.gemini/settings.json: its mcp.allowed leaves the server crewlet out"}},
		"the home excludes a tool of the bridge's": {
			plant: map[string]string{".gemini/settings.json": `{"tools":{"exclude":["mcp_crewlet_submit_work"]}}`},
			fails: []string{reason, "its tools.exclude names the tool mcp_crewlet_submit_work"}},
		"the repository excludes the notes' tool by its own name": {
			plant: map[string]string{"workspace/.gemini/settings.json": `{"tools":{"exclude":["steer_take"]}}`},
			fails: []string{reason, "its tools.exclude names the tool steer_take"}},
		"the repository disables the run's hook": {
			plant: map[string]string{"workspace/.gemini/settings.json": `{"hooksConfig":{"disabled":["crewlet"]}}`},
			fails: []string{reason, "/workspace/.gemini/settings.json: its hooksConfig.disabled names the hook crewlet"}},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			runner, box := geminiBox(t, observedRun)
			plant(t, box, tc.plant)
			_, res := runToEnd(t, runner, box, sandbox.RunRequest{Brief: "go", Model: "gemini-2.5-pro",
				MCPServers: loadsBridge, Steer: loadsSteer})
			checkSwitchOff(t, box, res, tc)
			_, err := os.Stat(filepath.Join(box.Home(), "observed.argv"))
			if len(tc.fails) > 0 {
				if err == nil {
					t.Errorf("Gemini CLI started although the run was refused")
				}
				return
			}
			p := executor.PathsFor(box)
			if argv := boxFile(t, box, box.Home()+"/observed.argv"); !strings.Contains(argv,
				"--admin-policy\n"+p.GeminiAdminPolicy()+"\n") {
				t.Errorf("gemini started without the run's admin policy:\n%s", argv)
			}
			if policy := boxFile(t, box, p.GeminiAdminPolicy()+"/crewlet.toml"); !strings.Contains(policy,
				"mcpName = \"crewlet\"\ndecision = \"allow\"") {
				t.Errorf("the run's admin policy does not allow the bridge's tools:\n%s", policy)
			}
			settings := systemTier(t, box, "GEMINI_CLI_SYSTEM_SETTINGS_PATH", p.GeminiSettings())
			if hooks, _ := settings["hooksConfig"].(map[string]any); hooks["enabled"] != true {
				t.Errorf("the run's settings do not switch hooks on: %v", settings["hooksConfig"])
			}
		})
	}
}

// A HOME OR A REPOSITORY CANNOT TAKE OPENCODE'S BRIDGE TOOLS OR AGENT AWAY.
// OpenCode 2.3.2 reads an agent's rules after every document's own, and a
// home's or a repository's agent rules (`agents.build.permissions`,
// `.opencode/agents/build.md`) before the run's document's, the last match
// deciding; a server's or the agent's `disabled` is the last document's
// (measured with `opencode api agent.list` and `mcp.list`). So the run's agent
// states `disabled: false` and ends its rules allowing every tool of the run's
// own servers, whatever the turn cap, and allows no seat server's beside them.
// Mutations, each red: state the agent only with a turn cap; drop the allow;
// allow every server's tools.
func TestOpenCodesBridgeToolsAndAgentCannotBeSwitchedOff(t *testing.T) {
	t.Parallel()
	for name, web := range map[string]bool{"a run that keeps the web": false, "a web-denied run": true} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			b := sandbox.NewFakeSandbox("box-1")
			servers := map[string]sandbox.MCPServer{
				"crewlet": {Transport: sandbox.TransportHTTP, URL: "http://127.0.0.1:7/mcp/t", AlwaysLoaded: true},
				"linear":  {Transport: sandbox.TransportHTTP, URL: "https://example.com/mcp"}}
			path, err := executor.OpenCode{}.WriteConfig(t.Context(), b,
				sandbox.RunRequest{MCPServers: servers, WebDenied: web}, executor.Session{}, executor.PathsFor(b))
			if err != nil {
				t.Fatalf("WriteConfig: %v", err)
			}
			raw, _ := b.ReadFile(t.Context(), path)
			var cfg struct {
				DefaultAgent string `json:"default_agent"`
				Agents       map[string]struct {
					Disabled *bool                                       `json:"disabled"`
					Rules    []struct{ Action, Resource, Effect string } `json:"permissions"`
				} `json:"agents"`
			}
			if err := json.Unmarshal(raw, &cfg); err != nil {
				t.Fatalf("unmarshal: %v\n%s", err, raw)
			}
			agent, ok := cfg.Agents[cfg.DefaultAgent]
			if !ok || agent.Disabled == nil || *agent.Disabled {
				t.Errorf("the run's agent %q does not state disabled: false:\n%s", cfg.DefaultAgent, raw)
			}
			n := len(agent.Rules)
			if n == 0 || agent.Rules[n-1].Action != "crewlet_*" || agent.Rules[n-1].Resource != "*" ||
				agent.Rules[n-1].Effect != "allow" {
				t.Errorf("the run's agent's rules do not end allowing the bridge's tools:\n%s", raw)
			}
			for _, r := range agent.Rules {
				if r.Effect == "allow" && r.Action != "crewlet_*" {
					t.Errorf("the run's agent allows %q beside its own servers' tools:\n%s", r.Action, raw)
				}
			}
		})
	}
}
