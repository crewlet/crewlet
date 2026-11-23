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

// A HOME OR A REPOSITORY CANNOT SWITCH GEMINI CLI'S BRIDGE, HOOK, SIGN-IN OR
// ROUND CAP OFF UNSEEN. The run's settings stand in the home's file; Gemini
// CLI 0.70.0 ranks the working directory's above it and adds every file's
// `mcp.excluded`, `tools.exclude` and `hooksConfig.disabled` up. So a run is
// refused, naming the file, key and entry, where a list names the bridge, a
// tool of its own or its hook, or the working directory replaces the bridge,
// switches hooks off, or names another sign-in or round cap; it starts on
// entries naming anything else, the home's own values, and machine files root
// does not keep (Gemini CLI skips them).
// Mutations, each red: drop the guard; match a server by case; skip the
// working directory's file; drop any override check; drop `hooksConfig.enabled`
// or the admin policy; read the home's file whole; read an unkept system file.
func TestGeminiCLIsBridgeAndHookCannotBeSwitchedOffUnseen(t *testing.T) {
	t.Parallel()
	const reason = "Gemini CLI would start without what the run's notes, tools, sign-in or round cap rest on"
	const repo = "/workspace/.gemini/settings.json: "
	for name, tc := range map[string]switchOffCase{
		"nothing switches it off": {},
		"lists of other things, comments, and the home's own values": {plant: map[string]string{
			".gemini/settings.json": "// the home's own\n{\"mcp\": {\"excluded\": [\"other\"]}, /* kept */ " +
				"\"tools\": {\"exclude\": [\"mcp_other_x\"]}, \"hooksConfig\": {\"enabled\": false, \"disabled\": [\"other\"]}, " +
				"\"mcpServers\": {\"crewlet\": {\"httpUrl\": \"http://127.0.0.1:9/home\"}}, " +
				"\"security\": {\"auth\": {\"selectedType\": \"oauth-personal\"}}, \"model\": {\"maxSessionTurns\": -1}}",
			"workspace/.gemini/settings.json": `{"mcp": {"allowed": ["crewlet", "other"]}, "hooksConfig": {"enabled": true},
				"mcpServers": {"other": {"httpUrl": "http://127.0.0.1:9/repo"}},
				"security": {"auth": {"selectedType": "gemini-api-key"}}, "model": {"maxSessionTurns": 30}}`}},
		"the home excludes the bridge": {plant: map[string]string{".gemini/settings.json": `{"mcp":{"excluded":[" Crewlet"]}}`},
			fails: []string{reason, "/.gemini/settings.json: its mcp.excluded names the server crewlet"}},
		"the repository allows only another server": {
			plant: map[string]string{"workspace/.gemini/settings.json": `{"mcp":{"allowed":["other"]}}`},
			fails: []string{reason, repo + "its mcp.allowed leaves the server crewlet out"}},
		"the home excludes a tool of the bridge's": {
			plant: map[string]string{".gemini/settings.json": `{"tools":{"exclude":["mcp_crewlet_submit_work"]}}`},
			fails: []string{reason, "its tools.exclude names the tool mcp_crewlet_submit_work"}},
		"the repository excludes the notes' tool by its own name": {
			plant: map[string]string{"workspace/.gemini/settings.json": `{"tools":{"exclude":["steer_take"]}}`},
			fails: []string{reason, "its tools.exclude names the tool steer_take"}},
		"the repository disables the run's hook": {
			plant: map[string]string{"workspace/.gemini/settings.json": `{"hooksConfig":{"disabled":["crewlet"]}}`},
			fails: []string{reason, repo + "its hooksConfig.disabled names the hook crewlet"}},
		"the repository replaces the bridge": {
			plant: map[string]string{"workspace/.gemini/settings.json": `{"mcpServers":{"crewlet":{"httpUrl":"http://127.0.0.1:9/x"}}}`},
			fails: []string{reason, repo + "its mcpServers.crewlet replaces the run's server crewlet"}},
		"the repository switches hooks off": {
			plant: map[string]string{"workspace/.gemini/settings.json": `{"hooksConfig":{"enabled":false}}`},
			fails: []string{reason, repo + "its hooksConfig.enabled is false, which switches the hook crewlet off"}},
		"the repository names another sign-in": {
			plant: map[string]string{"workspace/.gemini/settings.json": `{"security":{"auth":{"selectedType":"oauth-personal"}}}`},
			fails: []string{reason, repo + `its security.auth.selectedType is "oauth-personal", not the run's gemini-api-key`}},
		"the repository lifts the round cap": {
			plant: map[string]string{"workspace/.gemini/settings.json": `{"model":{"maxSessionTurns":-1}}`},
			fails: []string{reason, repo + "its model.maxSessionTurns is -1, not the run's round cap 30"}},
		"the machine's files, which root does not keep, so Gemini CLI skips": {plant: map[string]string{
			"system/settings.json": `{"mcpServers":{"crewlet":{"httpUrl":"http://127.0.0.1:9/x"}},"hooksConfig":{"enabled":false}}`,
			"system/defaults.json": `{"mcp":{"excluded":["crewlet"]}}`}},
		"the home's settings do not read": {plant: map[string]string{".gemini/settings.json": `["not an object"]`},
			fails: []string{"/.gemini/settings.json does not read as a JSON object", "The run was not started"}},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			runner, box := geminiBox(t, `cp "$HOME/.gemini/settings.json" "$HOME/observed.settings.json"
`+observedRun)
			plant(t, box, tc.plant)
			_, res := runToEnd(t, runner, box, sandbox.RunRequest{Brief: "go", Model: "gemini-2.5-pro",
				MCPServers: loadsBridge, Steer: loadsSteer, Env: map[string]string{"GEMINI_API_KEY": "key-placeholder",
					"GEMINI_CLI_SYSTEM_SETTINGS_PATH": box.Home() + "/system/settings.json",
					"GEMINI_CLI_SYSTEM_DEFAULTS_PATH": box.Home() + "/system/defaults.json"},
				Limits: sandbox.Limits{MaxTurns: 30}})
			checkSwitchOff(t, box, res, tc)
			for rel, body := range tc.plant {
				if got := boxFile(t, box, filepath.Join(box.Home(), rel)); got != body {
					t.Errorf("%s reads %q after the run, want it as it was", rel, got)
				}
			}
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
			var settings map[string]any
			if err := json.Unmarshal([]byte(boxFile(t, box, box.Home()+"/observed.settings.json")), &settings); err != nil {
				t.Fatalf("the home's settings as Gemini CLI started do not decode: %v", err)
			}
			if hooks, _ := settings["hooksConfig"].(map[string]any); hooks["enabled"] != true {
				t.Errorf("the run's settings do not switch hooks on: %v", settings["hooksConfig"])
			}
			raw, _ := json.Marshal(settings)
			for _, want := range []string{`"httpUrl":"` + codexBridgeURL + `"`, `"selectedType":"gemini-api-key"`,
				`"maxSessionTurns":30`} {
				if !strings.Contains(string(raw), want) {
					t.Errorf("Gemini CLI started on home settings without the run's %s:\n%s", want, raw)
				}
			}
		})
	}
}
