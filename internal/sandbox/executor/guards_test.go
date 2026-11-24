package executor_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/sandbox"
	"github.com/crewlet/crewlet/internal/sandbox/executor"
	"github.com/crewlet/crewlet/internal/shellword"
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
// connected without a tool the engine says it lists (a deny, or an `agent`
// setting whose agent leaves them out: measured), is ended as it starts, its
// failure naming the file and the entry; one whose bridge connected with its
// tools, or is still connecting, runs to its end, a deny of another server's
// tools or an agent keeping the bridge's included. Mutations, each red: launch
// without the guard; read `disabled` as connected; check no tool by name; name
// no file, entry or agent.
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
		"the repository runs the main thread as an agent with the bridge's tools": {tools: bridgeTools,
			servers: entry("connected"), switchOffCase: switchOffCase{plant: map[string]string{
				"workspace/.claude/settings.json":   `{"agent":"coder"}`,
				"workspace/.claude/agents/coder.md": "---\nname: coder\ndescription: d\n---\nwork\n"}}},
		"the repository runs the main thread as an agent without the bridge's tools": {tools: `["Read"]`,
			servers: entry("connected"), switchOffCase: switchOffCase{plant: map[string]string{
				"workspace/.claude/settings.json":   `{"agent": "shadow"}`,
				"workspace/.claude/agents/probe.md": "---\nname: shadow\ndescription: d\ntools: Read\n---\nx\n"},
				fails: []string{"connected, and none of its tools", "an agent setting running the main thread",
					"/workspace/.claude/settings.json (agent: shadow, ", "/workspace/.claude/agents/probe.md)"}}},
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

// A HOME OR A REPOSITORY CANNOT SWITCH QWEN CODE'S BRIDGE OFF UNSEEN. Qwen
// Code 0.28.4 adds `mcp.excluded`, `mcp.allowed`, `permissions.deny`,
// `tools.exclude` and `tools.disabled` up across its settings files, so a run
// whose home's or working directory's settings take the bridge or its tools
// away that way is refused before Qwen Code starts, naming the file, the key
// and the entry; an entry naming something else, a case or a specifier Qwen
// Code does not match, an `mcp.allowed` another file's list completes, or
// `disableAllHooks: true` (which the run's settings overrule) starts it.
// Mutations, each red: drop the guard from the script; match a server's case
// folded; skip the working directory's file; read each `mcp.allowed` alone;
// drop `disableAllHooks: false`.
func TestQwenCodesBridgeCannotBeSwitchedOffUnseen(t *testing.T) {
	t.Parallel()
	const reason = "Qwen Code would start without what the run's tools go through"
	for name, tc := range map[string]switchOffCase{
		"nothing switches it off": {},
		"entries naming other things, comments, and hooks switched off whole": {plant: map[string]string{
			".qwen/settings.json": "// the home's own\n{\"mcp\": {\"excluded\": [\"other\", \"Crewlet\"], " +
				"\"allowed\": [\"other\"]}, /* kept */ \"permissions\": {\"deny\": [\"mcp__other\", " +
				"\"mcp__crewlet__x(y)\"]}, \"tools\": {\"exclude\": [\"submit_work\"], \"disabled\": " +
				"[\"mcp__crewlet__*\"]}, \"disableAllHooks\": true}",
			"workspace/.qwen/settings.json": `{"mcp": {"allowed": ["crew*"]}}`}},
		"the home excludes the bridge by a glob": {plant: map[string]string{".qwen/settings.json": `{"mcp":{"excluded":["crew*"]}}`},
			fails: []string{reason, `/.qwen/settings.json: its mcp.excluded entry "crew*" takes the server crewlet away`}},
		"the repository excludes it in the older shape": {
			plant: map[string]string{"workspace/.qwen/settings.json": `{"excludeMCPServers":["crewlet"]}`},
			fails: []string{reason, `/workspace/.qwen/settings.json: its mcp.excluded entry "crewlet"`}},
		"the home allows only another server": {plant: map[string]string{".qwen/settings.json": `{"mcp":{"allowed":["other"]}}`},
			fails: []string{reason, `/.qwen/settings.json: its mcp.allowed ["other"] leaves the server crewlet out`}},
		"the home denies the bridge's tools": {
			plant: map[string]string{".qwen/settings.json": `{"permissions":{"deny":["mcp__crewlet"]}}`},
			fails: []string{reason, `its permissions.deny entry "mcp__crewlet" denies the tools of the server crewlet`}},
		"the repository excludes every server's tools": {
			plant: map[string]string{"workspace/.qwen/settings.json": `{"tools":{"exclude":["mcp__*"]}}`},
			fails: []string{reason, `/workspace/.qwen/settings.json: its tools.exclude entry "mcp__*"`}},
		"the repository hides a tool of the bridge's": {
			plant: map[string]string{"workspace/.qwen/settings.json": `{"tools":{"disabled":["mcp__crewlet__submit_work"]}}`},
			fails: []string{reason, `its tools.disabled entry "mcp__crewlet__submit_work" hides a tool of the server crewlet`}},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			finished := observedRun + `echo '{"type":"crewlet_qwen_end","stop_reason":"end_turn"}'` + "\n"
			runner, box := qwenBox(t, finished)
			// THE REAL HELPER FOR ITS GUARD; the stand-in plays the rest.
			writeBoxStandIn(t, box, executor.PathsFor(box).BinDir()+"/node", "#!/bin/sh\nif [ \"$2\" = guard ]; then exec "+
				shellword.Quote(nodeForQwen(t))+" \"$@\"; fi\n"+finished)
			plant(t, box, tc.plant)
			_, res := runToEnd(t, runner, box, sandbox.RunRequest{Brief: "go", Model: "qwen3-coder-plus",
				MCPServers: loadsBridge, Skills: loadsSkills, SkillLoads: loadsSkillLoads,
				Env: map[string]string{"OPENAI_API_KEY": "key-placeholder"}})
			checkSwitchOff(t, box, res, tc)
			argv, err := os.ReadFile(filepath.Join(box.Home(), "observed.argv"))
			if len(tc.fails) > 0 {
				if err == nil {
					t.Errorf("the helper ran Qwen Code although the run was refused")
				}
				return
			}
			if !strings.Contains(string(argv), "\nrun\n") {
				t.Errorf("the helper did not run Qwen Code:\n%s", argv)
			}
			settings := systemTier(t, box, "QWEN_CODE_SYSTEM_SETTINGS_PATH", executor.PathsFor(box).QwenSettings())
			if settings["disableAllHooks"] != false {
				t.Errorf("the run's settings do not keep hooks on: disableAllHooks %v", settings["disableAllHooks"])
			}
		})
	}
}

// A REPOSITORY'S CONFIGURATION MAY NOT CHANGE CODEX'S BRIDGE. A launch whose
// app-server's `config/read` (layers shaped as 0.171.0 gave them) has a loaded
// layer other than the run's own file and flags holding the bridge's entry at
// all, a parent's or the system's included, is refused before Codex starts,
// naming the file, the key and the entry. Another server's entry, an untrusted
// project's file and the run's own start it; an answer without layers refuses
// it. The guard asks for the start directory's layers under a home of its own.
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

// grokInspectStandIn is a stand-in `grok` that prints its release, answers
// `inspect` with the box home's inspect.json (NAME the run's own server's
// name off its settings, HOMEDIR the home), failing where there is none, and
// otherwise records its argv and ends its run as finished.
const grokInspectStandIn = "#!/bin/sh\ncase \"$1\" in --version) echo 'grok 1.0.92 (b06756fd6256)'; exit 0;;\n" +
	"inspect) [ -f \"$HOME/inspect.json\" ] || exit 1\n" +
	"n=$(sed -n 's/^ *\\[mcp_servers\\.\\(crewlet_[0-9a-f]*\\)\\]$/\\1/p' \"$HOME/.grok/config.toml\" | head -n 1)\n" +
	"sed -e \"s/NAME/$n/g\" -e \"s|HOMEDIR|$HOME|g\" \"$HOME/inspect.json\"; exit 0;; esac\n" +
	"printf '%s\\n' \"$@\" > \"$HOME/observed.argv\"\n" +
	"echo '{\"type\":\"text\",\"data\":\"done\"}'; echo '{\"type\":\"end\",\"stopReason\":\"end_turn\"}'\n"

// grokInspect is `grok inspect --json` as Grok Build 1.0.92 prints it, cut to
// the fields the guard reads, in its layout (measured): perms inside
// `permissions`, then each of servers, a name and the reason it is disabled
// where there is one.
func grokInspect(perms string, servers ...[2]string) string {
	var items []string
	for _, s := range servers {
		item := "    {\n      \"name\": \"" + s[0] + "\",\n      \"transport\": \"http\",\n      \"target\": \"x\",\n" +
			"      \"source\": {\n        \"type\": \"configToml\",\n        \"path\": \"HOMEDIR/.grok/config.toml\"\n      }"
		if s[1] != "" {
			item += ",\n      \"disabledReason\": \"" + s[1] + "\""
		}
		items = append(items, item+"\n    }")
	}
	return "{\n  \"grokVersion\": \"1.0.92\",\n  \"permissions\": {\n    \"sources\": [],\n" + perms +
		"    \"claudeBypassLockAdvisory\": false\n  },\n  \"hooks\": [],\n  \"mcpServers\": [\n" +
		strings.Join(items, ",\n") + "\n  ],\n  \"lspServers\": []\n}\n"
}

// A HOME CANNOT SWITCH GROK BUILD'S BRIDGE OR HOOKS OFF UNSEEN. Grok Build
// 1.0.92's policy (`~/.grok/requirements.toml`, `/etc/grok`) blocks a server
// its `denied_mcp_servers` matches or its `allowed_mcp_servers` leaves out,
// and its `allow_managed_hooks_only` skips the run's hooks; no layer releases
// either (measured with `grok inspect --json`). So a run whose inspection
// reports its own server disabled, or, hooked, that pin engaged, is refused
// before Grok starts, naming the file, the key and the policy's entries, and
// the box's settings are put back; a policy blocking another server, a pin on
// a run with no hooks, or an inspection that fails, start it.
// Mutations, each red: drop the guard from the script; read another server's
// reason as the run's; check the pin on a run with no hooks; name no entries.
func TestGrokBuildsBridgeAndHooksCannotBeSwitchedOffUnseen(t *testing.T) {
	t.Parallel()
	const policy = "HOMEDIR/.grok/requirements.toml"
	allowlist := func(entries ...string) string {
		return "    \"mcpServerAllowlist\": [\n      \"" + strings.Join(entries, "\",\n      \"") + "\"\n    ],\n"
	}
	const pin = "    \"enforced\": [\n      {\n        \"setting\": \"nonManagedHooks\",\n        \"enabled\": false,\n" +
		"        \"source\": \"" + policy + "\"\n      }\n    ],\n"
	for name, tc := range map[string]struct {
		inspect string
		hooked  bool
		switchOffCase
	}{
		"nothing switches it off": {inspect: grokInspect("", [2]string{"NAME", ""}), hooked: true},
		"the inspection fails":    {hooked: true},
		"the policy denies another server": {hooked: true, inspect: grokInspect(allowlist("deny:https://other.example.com/*"),
			[2]string{"NAME", ""}, [2]string{"other", "matches deniedMcpServers (" + policy + ")"})},
		"a hooks pin on a run with no hooks": {inspect: grokInspect(pin, [2]string{"NAME", ""})},
		"the home's policy denies the bridge's URL": {hooked: true,
			inspect: grokInspect(allowlist("deny:http://127.0.0.1:9/*"), [2]string{"NAME", "matches deniedMcpServers (" + policy + ")"}),
			switchOffCase: switchOffCase{fails: []string{"Grok Build would start without Crewlet's MCP server crewlet_",
				"matches deniedMcpServers (", "/.grok/requirements.toml)", "deny:http://127.0.0.1:9/*",
				"denied_mcp_servers", "refused before Grok Build started"}}},
		"the home's policy allows only another server": {
			inspect: grokInspect(allowlist("name:home_srv"), [2]string{"NAME", "not in allowedMcpServers (" + policy + ")"}),
			switchOffCase: switchOffCase{fails: []string{"Crewlet's MCP server crewlet_", "not in allowedMcpServers (",
				"name:home_srv", "allowed_mcp_servers"}}},
		"the home's policy runs managed hooks only": {hooked: true, inspect: grokInspect(pin, [2]string{"NAME", ""}),
			switchOffCase: switchOffCase{fails: []string{"Grok Build would run none of the run's hooks",
				"/.grok/requirements.toml sets allow_managed_hooks_only"}}},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			runner, box := standInBox(t, executor.NewGrok(), map[string]string{"grok": grokInspectStandIn})
			if tc.inspect != "" {
				plant(t, box, map[string]string{"inspect.json": tc.inspect})
			}
			req := sandbox.RunRequest{Brief: "go", MCPServers: loadsBridge}
			if tc.hooked {
				req.Steer = loadsSteer
			}
			_, res := runToEnd(t, runner, box, req)
			checkSwitchOff(t, box, res, tc.switchOffCase)
			_, started := os.Stat(filepath.Join(box.Home(), "observed.argv"))
			if (started == nil) != (len(tc.fails) == 0) {
				t.Errorf("grok started %t, want %t", started == nil, len(tc.fails) == 0)
			}
			p := executor.PathsFor(box)
			for _, left := range []string{p.GrokConfig(), p.GrokConfigBox(), p.GrokConfigOurs()} {
				if got := boxFile(t, box, left); got != "" {
					t.Errorf("%s is left after the run: %q", left, got)
				}
			}
		})
	}
}

// museRequestThenWait is a stand-in `muse` that answers its release and, for
// `exec`, writes the session's record as Muse Code 1.4.25 does up to its first
// model request, `model_request_configured` offering the model the tools of
// the run's own server as the script added it (`mcp__<name>__submit_work`,
// `-` written `_`) where $HOME/loads is there, an earlier run's server's
// where $HOME/old is, and no server's otherwise; and where $HOME/earlier is,
// its `mcp add-json` first appends to the session the run continues an
// earlier run's tool catalog and request offering the server it adds, as a
// session holds when a server keeps its name across runs. It then answers as
// a finished run where it offered the server, and otherwise waits for good,
// so only the guard ends it.
func museRequestThenWait(added string) string {
	return `#!/bin/sh
request() { printf '{"payload_type":"runtime.session","payload":{"event":{"kind":"model_request_configured","tools":["read_skill"%s]}}}\n' "$1" >> "$f"; }
offers() { printf ',"mcp__%s__submit_work"' "$(printf '%s' "$1" | tr - _)"; }
case "$1 $2" in
"--version "*) echo 'Muse Code 1.4.25 (1.4.25-R9361.1)'; exit 0;;
"mcp add-json") if [ -f "$HOME/earlier" ]; then for f in "$HOME"/.local/share/muse/sessions/*/*/*/*/session.jsonl; do
  printf '{"payload_type":"runtime.mcp_tool_identity_catalog","payload":{"entries":[{"server_name":"%s"}]}}\n' "$3" >> "$f"
  request "$(offers "$3")"; done; fi; exit 0;;
exec*) ;; *) exit 0;; esac
s=; while [ $# -gt 0 ]; do [ "$1" = --session-id ] && s=$2; shift; done
d=$(ls -d "$HOME"/.local/share/muse/sessions/*/*/*/"$s" 2> /dev/null | head -n 1)
[ -n "$d" ] || d="$HOME/.local/share/muse/sessions/2026/11/24/$s"; mkdir -p "$d"; f="$d/session.jsonl"
printf '%s\n' '{"payload_type":"runtime.session.metadata"}' >> "$f"
if [ -f "$HOME/loads" ]; then request "$(offers "$(grep -e '^crewlet-' ` + shellword.Quote(added) + ` | head -n 1)")"
elif [ -f "$HOME/old" ]; then request "$(offers crewlet-0ld000)"
else request ""; fi
[ -f "$HOME/loads" ] || while :; do sleep 1; done
cat "$HOME/plain.stream.jsonl"
`
}

// A HOME OR A REPOSITORY CANNOT SWITCH MUSE CODE'S BRIDGE OFF UNSEEN. Muse Code
// 1.4.25 leaves out every MCP server, the bridge included, and runs on to exit
// 0, where one entry of a repository's `.mcp.json` or of the home's settings
// is one it does not take (64 configurations measured, among them an http
// server without a url), and records no such fault; its first model request
// (`model_request_configured`) then offers none of the servers' tools. So a
// run whose own first request offers no tool of its own server, as this run
// added it, is ended there, its failure naming the files, a session an
// earlier run offered it in included; one whose request offers it runs to its
// end, a repository's own server beside it and a resumed session included.
// Mutations, each red: launch without the guard; read the session's first
// request in place of this run's; accept any server's tools; name no file.
func TestMuseCodesBridgeCannotBeSwitchedOffUnseen(t *testing.T) {
	t.Parallel()
	plain, err := os.ReadFile(filepath.Join("testdata", "muse", "1.4.25", "plain.stream.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	bridge := map[string]sandbox.MCPServer{"crewlet": {Transport: sandbox.TransportHTTP, URL: codexBridgeURL,
		AlwaysLoaded: true, Tools: []string{"submit_work"}}}
	const dropped = `{"mcpServers":{"x":{"type":"http"}}}`
	const resumed = "11111111-2222-4333-8444-0000000000a7"
	earlier := ".local/share/muse/sessions/2026/11/23/" + resumed + "/session.jsonl"
	for name, c := range map[string]struct {
		resume bool
		switchOffCase
	}{
		"it loaded its servers": {switchOffCase: switchOffCase{plant: map[string]string{"loads": ""}}},
		"a repository's ordinary server loads beside it": {switchOffCase: switchOffCase{plant: map[string]string{
			"loads": "", "workspace/.mcp.json": `{"mcpServers":{"docs":{"command":"docs-server"}}}`}}},
		"a resumed session's run loads it again": {resume: true, switchOffCase: switchOffCase{
			plant: map[string]string{"loads": "", "earlier": "", earlier: "{}\n"}}},
		"a repository's .mcp.json holds an entry it does not take": {switchOffCase: switchOffCase{
			plant: map[string]string{"workspace/.mcp.json": dropped},
			fails: []string{"started without Crewlet's MCP server", "/workspace/.mcp.json",
				"The run was ended as it started"}}},
		"the home's settings hold mcp_servers beside mcpServers": {switchOffCase: switchOffCase{
			plant: map[string]string{".config/muse/settings.json": `{"schema_version":1,"mcp_servers":{}}`},
			fails: []string{"started without Crewlet's MCP server", "/.config/muse/settings.json"}}},
		"only another run's server is offered": {switchOffCase: switchOffCase{
			plant: map[string]string{"old": "", "workspace/.mcp.json": dropped},
			fails: []string{"started without Crewlet's MCP server", "/workspace/.mcp.json"}}},
		"an earlier run of the session offered it and this run offers none": {resume: true,
			switchOffCase: switchOffCase{plant: map[string]string{"earlier": "", earlier: "{}\n",
				"workspace/.mcp.json": dropped},
				fails: []string{"started without Crewlet's MCP server", "/workspace/.mcp.json"}}},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			runner, box := standInBox(t, executor.NewMuse(), nil)
			writeBoxStandIn(t, box, executor.PathsFor(box).BinDir()+"/muse",
				museRequestThenWait(executor.PathsFor(box).MuseServersAdded()))
			c.plant["plain.stream.jsonl"] = string(plain)
			plant(t, box, c.plant)
			req := sandbox.RunRequest{Brief: "go", MCPServers: bridge}
			if c.resume {
				req.Resume = resumed
			}
			_, res := runToEnd(t, runner, box, req)
			checkSwitchOff(t, box, res, c.switchOffCase)
		})
	}
}
