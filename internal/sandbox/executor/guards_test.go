package executor_test

import (
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
