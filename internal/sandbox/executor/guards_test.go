package executor_test

import (
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
// connected without a tool the engine says it lists, is ended as it starts,
// its failure naming the file and the entry; one whose bridge connected with
// its tools, or is still connecting, runs to its end, a deny of another
// server's tools included.
//
// Mutations, each red: launch without the guard; read `disabled` as
// connected; check no tool by name; name no file or entry.
func TestClaudeCodesBridgeCannotBeSwitchedOffUnseen(t *testing.T) {
	t.Parallel()
	const denied = `{"deniedMcpServers":[{"serverName":"crewlet"}]}`
	const bridgeTools = `["Bash","mcp__crewlet__report","mcp__crewlet__submit_work","mcp__other__submit_work"]`
	const otherTools = `["Bash","mcp__other__x"]`
	bridge := map[string]sandbox.MCPServer{"crewlet": {Transport: sandbox.TransportHTTP, URL: codexBridgeURL,
		AlwaysLoaded: true, Tools: []string{"submit_work", "report"}}}
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
		"the repository denies another server's tool of a bridge tool's name": {
			tools: `["Bash","mcp__crewlet__report","mcp__crewlet__submit_work"]`, servers: entry("connected"),
			switchOffCase: switchOffCase{plant: map[string]string{
				"workspace/.claude/settings.json": `{"permissions":{"deny":["mcp__other__submit_work"]}}`}}},
		"the home denies one of the bridge's tools": {tools: `["Bash","mcp__crewlet__report"]`,
			servers: entry("connected"), switchOffCase: switchOffCase{
				plant: map[string]string{".claude/settings.json": `{"permissions":{"deny":["mcp__crewlet__submit_work"]}}`},
				fails: []string{"started with Crewlet's MCP server crewlet", "connected, and without its tools mcp__crewlet__submit_work:",
					"permissions.deny", "/.claude/settings.json (mcp__crewlet__submit_work)"}}},
		"the repository's local settings deny every tool by a wildcard": {tools: otherTools, servers: entry("connected"),
			switchOffCase: switchOffCase{
				plant: map[string]string{"workspace/.claude/settings.local.json": `{"permissions":{"deny":["mcp__*"]}}`},
				fails: []string{"connected, and none of its tools", "permissions.deny",
					"/workspace/.claude/settings.local.json (mcp__*)"}}},
		"the repository denies the bridge's tools": {tools: otherTools, servers: entry("connected"),
			switchOffCase: switchOffCase{
				plant: map[string]string{"workspace/.claude/settings.json": `{"permissions":{"deny":["mcp__crewlet"]}}`},
				fails: []string{"started with Crewlet's MCP server crewlet", "connected, and none of its tools",
					"permissions.deny", "/workspace/.claude/settings.json (mcp__crewlet)"}}},
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
			_, res := runToEnd(t, runner, box, sandbox.RunRequest{Brief: "go", MCPServers: bridge})
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

// A REPOSITORY'S CONFIGURATION MAY NOT CHANGE CODEX'S BRIDGE. Codex 0.171.0
// merges every trusted project's `.codex/config.toml` from the project root
// down to the start directory key by key above the run's own, so a launch
// whose app-server's `config/read` (the layers below, of the shape 0.171.0
// gave when measured) has a loaded layer other than the run's own file and
// flags holding the bridge's entry at all, a parent's or the system's
// included, is refused before Codex starts, naming the file, the key and the
// entry. Another server's entry, an untrusted project's file and the run's
// own start it; an answer without layers refuses it. The guard asks for the
// start directory's layers, under a home of its own holding the home's
// configuration.
//
// Mutations, each red: skip the guard; read an untrusted layer as loaded; let
// a system layer pass; pass an empty entry; ask without the start directory
// or its layers; pass a missing answer.
func TestARepositorysConfigurationMayNotChangeCodexsBridge(t *testing.T) {
	t.Parallel()
	answer := func(layers ...string) string {
		return `{"id":1,"result":{}}` + "\n" + `{"id":2,"result":{"config":{},"layers":[` +
			strings.Join(layers, ",") + `]}}`
	}
	layer := func(name, crewlet, disabled string) string {
		l := `{"name":` + name + `,"version":"v","config":{"mcp_servers":{"crewlet":` + crewlet + `}}`
		if disabled != "" {
			l += `,"disabledReason":"` + disabled + `"`
		}
		return l + "}"
	}
	const (
		project = `{"type":"project","dotCodexFolder":"WORK/.codex"}`
		parent  = `{"type":"project","dotCodexFolder":"PARENT/.codex"}`
		system  = `{"type":"system","file":"/etc/codex/config.toml"}`
	)
	own := layer(`{"type":"sessionFlags"}`, `{"enabled":true,"disabled_tools":[]}`, "") + "," +
		layer(`{"type":"user","file":"/tmp/x/config.toml","profile":null}`, `{"url":"http://crewlet.invalid/"}`, "") +
		"," + layer(system, "null", "")
	const refusal = "a repository's configuration may not change Crewlet's own server"
	for name, tc := range map[string]struct {
		answer string
		fails  []string
	}{
		"the run's own file and flags": {answer: answer(own, layer(project, "null", ""))},
		"a repository's server of its own": {answer: answer(own,
			`{"name":`+project+`,"config":{"mcp_servers":{"other":{"url":"http://x"}}}}`)},
		"an untrusted repository's entry": {answer: answer(own, layer(project,
			`{"url":"http://elsewhere.example.com/mcp"}`, "To load project-local config, add it as a trusted project."))},
		"a repository switches it off": {answer: answer(own, layer(project, `{"enabled":false,"disabled_tools":["report"]}`, "")),
			fails: []string{refusal, `WORK/.codex/config.toml sets mcp_servers.crewlet.disabled_tools = ["report"] in [mcp_servers.crewlet]`,
				`WORK/.codex/config.toml sets mcp_servers.crewlet.enabled = false in [mcp_servers.crewlet]`,
				"remove [mcp_servers.crewlet] from that configuration"}},
		"a repository redirects it": {answer: answer(own, layer(project, `{"url":"http://elsewhere.example.com/mcp"}`, "")),
			fails: []string{refusal, `WORK/.codex/config.toml sets mcp_servers.crewlet.url = "http://elsewhere.example.com/mcp"`}},
		"a parent directory's project narrows its tools": {answer: answer(own, layer(project, "null", ""),
			layer(parent, `{"enabled_tools":["report"]}`, "")),
			fails: []string{refusal, `PARENT/.codex/config.toml sets mcp_servers.crewlet.enabled_tools = ["report"]`}},
		"a repository holds an empty entry": {answer: answer(own, layer(project, `{}`, "")),
			fails: []string{refusal, `WORK/.codex/config.toml holds [mcp_servers.crewlet] as {}`}},
		"the system's configuration sets its timeout": {answer: answer(layer(system, `{"tool_timeout_sec":5}`, "")),
			fails: []string{"/etc/codex/config.toml sets mcp_servers.crewlet.tool_timeout_sec = 5"}},
		"Codex gives no layers": {answer: `{"id":1,"result":{}}`,
			fails: []string{"gave no layers of its configuration for the start directory"}},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			runner, box, record := codexBox(t, "0.171.0")
			work, err := filepath.EvalSymlinks(filepath.Join(box.Home(), "workspace"))
			if err != nil {
				t.Fatal(err)
			}
			const home = "[projects.\"/w\"]\ntrust_level = \"trusted\"\n"
			plant(t, box, map[string]string{".codex/config.toml": home, "workspace/.codex/config.toml": "[mcp_servers.crewlet]\n"})
			at := strings.NewReplacer("WORK", work, "PARENT", filepath.Dir(work))
			plantFile(t, filepath.Join(record, "config-read"), at.Replace(tc.answer))
			req := sandbox.RunRequest{Brief: "go", MCPServers: loadsBridge}
			p := executor.PathsFor(box)
			if len(tc.fails) == 0 {
				if _, res := runToEnd(t, runner, box, req); !res.Success {
					t.Fatalf("a run nothing changed the bridge of failed: %s", res.Error)
				}
			} else {
				_, err := runner.Start(t.Context(), box, req)
				if err == nil {
					t.Fatal("a run whose bridge a layer above its own changed started")
				}
				for _, want := range tc.fails {
					if want = at.Replace(want); !strings.Contains(err.Error(), want) {
						t.Errorf("the refusal does not say %q:\n%v", want, err)
					}
				}
				if _, err := os.Stat(filepath.Join(record, "server-argv")); !os.IsNotExist(err) {
					t.Errorf("Codex started for a refused run (%v)", err)
				}
				if got := boxFile(t, box, p.CodexLiveConfig()); got != home {
					t.Errorf("a refused run changed the home's config.toml:\n%s", got)
				}
			}
			stdin := strings.Join(recorded(t, record, "guard-stdin"), "\n")
			if !strings.Contains(stdin, `"method":"config/read"`) || !strings.Contains(stdin, `"cwd":"`+work+`"`) ||
				!strings.Contains(stdin, `"includeLayers":true`) {
				t.Errorf("the guard did not ask for the start directory's layers:\n%s", stdin)
			}
			guardConfig, _ := os.ReadFile(filepath.Join(record, "guard-config.toml"))
			if g := string(guardConfig); !strings.Contains(g, `trust_level = "trusted"`) ||
				!strings.Contains(g, `url = "http://crewlet.invalid/"`) || strings.Contains(g, codexBridgeURL) {
				t.Errorf("the guard's Codex read other than the home's own configuration with the run's "+
					"servers at a URL holding no token:\n%s", g)
			}
			if _, err := os.Stat(p.CodexGuardConfig()); !os.IsNotExist(err) {
				t.Errorf("the guard left its configuration in the box (%v)", err)
			}
			guardHome := strings.Join(recorded(t, record, "guard-home"), "")
			if guardHome == p.CodexHome() || guardHome == "" {
				t.Errorf("the guard's Codex ran under %q, not a home of its own", guardHome)
			} else if _, err := os.Stat(guardHome); !os.IsNotExist(err) {
				t.Errorf("the guard left its home behind (%v)", err)
			}
			if argv := strings.Join(recorded(t, record, "guard-argv"), "\n"); !strings.Contains(argv, "mcp_servers.crewlet.enabled=true") {
				t.Errorf("the guard's Codex ran without the run's own flags:\n%s", argv)
			}
		})
	}
}
