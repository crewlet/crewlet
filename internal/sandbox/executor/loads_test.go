package executor_test

import (
	"encoding/json"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/api/mcpbridge"
	"github.com/crewlet/crewlet/internal/sandbox"
	"github.com/crewlet/crewlet/internal/sandbox/executor"
)

// homeCase is what a real box's home holds before a run of one harness, and
// what the run must leave there: each file left there gone, each kept file
// whole. Paths are the home's; `workspace/` is the box's working directory,
// which the run starts in.
type homeCase struct {
	left []string
	kept []string
}

// observedRun records, from a stand-in harness, its argv, its environment and
// its stdin in the box home, then answers as a finished run.
const observedRun = `printf '%s\n' "$@" > "$HOME/observed.argv"
env > "$HOME/observed.env"
cat > "$HOME/observed.brief"
echo '{"type":"result","status":"success"}'
`

// fillHome writes every left and kept file of c under box's home, each
// holding its own path, and an `.env` in the directory above the home, which
// no run may touch.
func fillHome(t *testing.T, box sandbox.Sandbox, c homeCase) (above string) {
	t.Helper()
	for _, rel := range slices.Concat(c.left, c.kept) {
		path := filepath.Join(box.Home(), rel)
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(plantedContent(rel)), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	above = filepath.Join(filepath.Dir(box.Home()), ".env")
	if err := os.WriteFile(above, []byte("ABOVE=1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return above
}

// plantedContent is what [fillHome] writes at rel: its path, so a file
// rewritten by the run reads as another.
func plantedContent(rel string) string { return "written before the run: " + rel + "\n" }

// runObserved starts req in box and waits for its end.
func runObserved(t *testing.T, runner *executor.Runner, box sandbox.Sandbox, req sandbox.RunRequest) {
	t.Helper()
	handle, err := runner.Start(t.Context(), box, req)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	awaitOver(t, runner, box, handle, time.Minute, "a stand-in that records and answers takes a second")
}

// checkHome holds box's home to c after a run: each left file gone, each kept
// file as [fillHome] wrote it, and the directory above the home untouched.
func checkHome(t *testing.T, box sandbox.Sandbox, c homeCase, above string) {
	t.Helper()
	for _, rel := range c.left {
		if _, err := os.Stat(filepath.Join(box.Home(), rel)); !os.IsNotExist(err) {
			t.Errorf("%s is still there after the run (%v)", rel, err)
		}
	}
	for _, rel := range c.kept {
		if got, err := os.ReadFile(filepath.Join(box.Home(), rel)); err != nil || string(got) != plantedContent(rel) {
			t.Errorf("%s, the home's or the working directory's own, reads %q after the run (%v), "+
				"want it as it was", rel, got, err)
		}
	}
	if _, err := os.Stat(above); err != nil {
		t.Errorf("the run removed %s, above the box's home: %v", above, err)
	}
}

// observedEnv is the stand-in harness's environment, by name.
func observedEnv(t *testing.T, box sandbox.Sandbox) map[string]string {
	t.Helper()
	env := map[string]string{}
	for _, line := range strings.Split(boxFile(t, box, box.Home()+"/observed.env"), "\n") {
		if name, value, ok := strings.Cut(line, "="); ok {
			env[name] = value
		}
	}
	return env
}

// loadsBridge is the run's own MCP server in every row, the bridge.
var loadsBridge = map[string]sandbox.MCPServer{
	"crewlet": {Transport: sandbox.TransportHTTP, URL: codexBridgeURL, AlwaysLoaded: true}}

// loadsSkills is the run's own skill in every row, with its load reported.
var (
	loadsSkills     = []sandbox.Skill{{Name: "deploy", Description: "How we deploy.", Body: "Run make deploy."}}
	loadsSkillLoads = sandbox.SkillWiring{Server: "crewlet", Tool: mcpbridge.SkillTool, URL: codexBridgeURL + "/skills"}
	loadsSteer      = sandbox.SteerWiring{Server: "crewlet", Tool: mcpbridge.SteerTool, URL: codexBridgeURL + "/steer"}
)

// A RUN LOADS WHAT A PERSON'S SESSION OF ITS HARNESS LOADS. A real box per
// runner, its harness a recording stand-in, the run going to its end: the
// start directory and the box home hold every kind the harness reads
// (instructions, settings, hooks, skills, MCP servers, environment files);
// each is where the harness reads it as it starts, unchanged after the run,
// and nothing the launch passes keeps it out; Crewlet's own MCP server and
// hook are still there, in a layer the harness reads after the home's; and the
// home's own entry of each kind survives beside Crewlet's.
func TestARunLoadsWhatAPersonsSessionLoads(t *testing.T) {
	t.Parallel()
	for _, name := range slices.Sorted(maps.Keys(loadsRows)) {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			loadsRows[name](t)
		})
	}
}

// loadsRows is each runner's row of [TestARunLoadsWhatAPersonsSessionLoads],
// by name.
var loadsRows = map[string]func(t *testing.T){
	executor.GeminiCLIName: geminiLoads,
	executor.QwenCodeName:  qwenLoads,
	executor.CodexName:     codexLoads,
	executor.OpenCodeName:  openCodeLoads,
	executor.CopilotName: copilotLoads,
}

// systemTier reads the settings file the run named in variable for its
// harness's system tier, holding it to the run's own path.
func systemTier(t *testing.T, box sandbox.Sandbox, variable, want string) map[string]any {
	t.Helper()
	if got := observedEnv(t, box)[variable]; got != want {
		t.Fatalf("the harness started with %s=%q, want the run's settings %s", variable, got, want)
	}
	var settings map[string]any
	if err := json.Unmarshal([]byte(boxFile(t, box, want)), &settings); err != nil {
		t.Fatalf("the run's settings do not decode: %v", err)
	}
	return settings
}

// geminiLoads is Gemini CLI's row: the home's and the working directory's
// GEMINI.md, settings, skills, policies, commands, extensions and `.env`
// stay, the run's settings are the system tier above them (Gemini CLI 0.70.0
// merges mcpServers key by key and concatenates hooks across tiers), no
// flag names the only extensions or servers to load, no `.env` of the run's
// ends the walk, and only an earlier run's `crewlet-` skill goes.
//
// Mutations, each red: restore `--extensions none` or
// `--allowed-mcp-server-names`; write the settings to the home's own file;
// clear the home's skills or seed the working directory's `.env`.
func geminiLoads(t *testing.T) {
	c := homeCase{
		left: []string{".gemini/skills/crewlet-old/SKILL.md"},
		kept: []string{".gemini/GEMINI.md", ".gemini/settings.json", ".gemini/skills/home/SKILL.md",
			".gemini/policies/home.toml", ".gemini/commands/home.toml", ".gemini/.env",
			".gemini/extensions/home/gemini-extension.json", "GEMINI.md", ".env",
			"workspace/GEMINI.md", "workspace/.env", "workspace/.gemini/settings.json",
			"workspace/.gemini/skills/repo/SKILL.md"},
	}
	runner, box := geminiBox(t, observedRun)
	above := fillHome(t, box, c)
	runObserved(t, runner, box, sandbox.RunRequest{Brief: "go", Model: "gemini-2.5-pro", MCPServers: loadsBridge,
		Skills: loadsSkills, SkillLoads: loadsSkillLoads, Steer: loadsSteer})
	checkHome(t, box, c, above)
	p := executor.PathsFor(box)
	if _, err := os.Stat(filepath.Join(box.Home(), "workspace", ".gemini", ".env")); !os.IsNotExist(err) {
		t.Errorf("the working directory holds a .gemini/.env of the run's (%v), which hides the walk's own", err)
	}
	if skill := boxFile(t, box, p.GeminiSkills()+"/crewlet-deploy/SKILL.md"); !strings.Contains(skill, "make deploy") {
		t.Errorf("the run's skill is %q, want it beside the home's", skill)
	}
	argv := boxFile(t, box, box.Home()+"/observed.argv")
	for _, limit := range []string{"--extensions", "--allowed-mcp-server-names"} {
		if strings.Contains(argv, limit) {
			t.Errorf("gemini started with %s, which keeps out what a person's session loads:\n%s", limit, argv)
		}
	}
	settings := systemTier(t, box, "GEMINI_CLI_SYSTEM_SETTINGS_PATH", p.GeminiSettings())
	if servers, _ := settings["mcpServers"].(map[string]any); servers["crewlet"] == nil {
		t.Errorf("the run's settings hold no crewlet server: %v", settings["mcpServers"])
	}
	if hooks, _ := settings["hooks"].(map[string]any); hooks["AfterAgent"] == nil || hooks["AfterTool"] == nil {
		t.Errorf("the run's settings hold no note or skill hook: %v", settings["hooks"])
	}
}

// qwenLoads is Qwen Code's row: the home's and the working directory's
// QWEN.md, AGENTS.md, settings, skills, rules, commands, extensions and
// `.env` stay, the run's settings are the system tier above them (Qwen Code
// 0.28.4 merges mcpServers key by key, concatenates hooks and unions skill
// directories across tiers) and disable no skill level, the run's skills sit
// in its own directory, and the sign-in reaches the shell.
//
// Mutations, each red: write the settings to the home's own file; disable a
// skill level; clear the home's skills or seed the working directory's `.env`.
func qwenLoads(t *testing.T) {
	c := homeCase{kept: []string{".qwen/QWEN.md", ".qwen/AGENTS.md", ".qwen/settings.json",
		".qwen/skills/home/SKILL.md", ".qwen/rules/home.md", ".qwen/commands/home.md", ".qwen/.env",
		".qwen/extensions/home/qwen-extension.json", "QWEN.md", "AGENTS.md", ".env",
		"workspace/QWEN.md", "workspace/AGENTS.md", "workspace/.env", "workspace/.qwen/settings.json",
		"workspace/.qwen/skills/repo/SKILL.md"}}
	runner, box := qwenBox(t, observedRun)
	above := fillHome(t, box, c)
	runObserved(t, runner, box, sandbox.RunRequest{Brief: "go", Model: "qwen3-coder-plus", MCPServers: loadsBridge,
		Skills: loadsSkills, SkillLoads: loadsSkillLoads, Env: map[string]string{"OPENAI_API_KEY": "key-placeholder"}})
	checkHome(t, box, c, above)
	p := executor.PathsFor(box)
	if _, err := os.Stat(filepath.Join(box.Home(), "workspace", ".qwen", ".env")); !os.IsNotExist(err) {
		t.Errorf("the working directory holds a .qwen/.env of the run's (%v), which hides the walk's own", err)
	}
	if got := observedEnv(t, box)["OPENAI_API_KEY"]; got != "key-placeholder" {
		t.Errorf("the run's shell has OPENAI_API_KEY=%q, want the sign-in", got)
	}
	settings := systemTier(t, box, "QWEN_CODE_SYSTEM_SETTINGS_PATH", p.QwenSettings())
	if servers, _ := settings["mcpServers"].(map[string]any); servers["crewlet"] == nil {
		t.Errorf("the run's settings hold no crewlet server: %v", settings["mcpServers"])
	}
	if hooks, _ := settings["hooks"].(map[string]any); hooks["PostToolUse"] == nil {
		t.Errorf("the run's settings hold no skill hook: %v", settings["hooks"])
	}
	skills, _ := settings["skills"].(map[string]any)
	if dirs, _ := skills["directories"].([]any); !slices.Equal(dirs, []any{p.QwenSkills()}) || skills["disabledLevels"] != nil {
		t.Errorf("the run's skills settings are %v, want its own directory and no level disabled", skills)
	}
	if skill := boxFile(t, box, p.QwenSkills()+"/crewlet-deploy/SKILL.md"); !strings.Contains(skill, "make deploy") {
		t.Errorf("the run's skill is %q", skill)
	}
}

// codexLoads is Codex's row: CODEX_HOME's AGENTS.md, rules, hooks, skills
// and `.env`, the home's `.agents/skills` and the working directory's
// AGENTS.md and `.codex/` stay; Codex starts on the home's config.toml with
// the run's over it, the home's MCP server beside the run's, and the home's
// file is back after; the run's skills sit beside the home's, an earlier
// run's gone; and no posture switches off plugins or skills.
//
// Mutations, each red: install the run's configuration alone; clear every
// skill but `.system`; restore features.plugins=false or
// skills.bundled.enabled=false.
func codexLoads(t *testing.T) {
	c := homeCase{
		left: []string{".codex/skills/crewlet:old/SKILL.md"},
		kept: []string{".codex/AGENTS.md", ".codex/AGENTS.override.md", ".codex/rules/home.rules",
			".codex/hooks.json", ".codex/.env", ".codex/skills/home/SKILL.md", ".codex/skills/.system/kept/SKILL.md",
			".agents/skills/home/SKILL.md", "workspace/AGENTS.md", "workspace/.codex/config.toml",
			"workspace/.agents/skills/repo/SKILL.md"},
	}
	runner, box, record := codexBox(t, "0.171.0")
	above := fillHome(t, box, c)
	p := executor.PathsFor(box)
	const own = "[mcp_servers.home]\ncommand = \"home-mcp-server\"\n"
	plantFile(t, filepath.Join(box.Home(), ".codex", "config.toml"), own)
	_, res := runToEnd(t, runner, box, sandbox.RunRequest{Brief: "go", MCPServers: loadsBridge,
		Skills: loadsSkills, SkillLoads: loadsSkillLoads})
	if !res.Success {
		t.Fatalf("the run failed: %s", res.Error)
	}
	checkHome(t, box, c, above)
	if got := boxFile(t, box, p.CodexLiveConfig()); got != own {
		t.Errorf("after the run the home's config.toml is\n%s\nwant its own back", got)
	}
	cfg := codexConfigSeen(t, record)
	for _, want := range []string{"home-mcp-server", codexBridgeURL, "required = true"} {
		if !strings.Contains(cfg, want) {
			t.Errorf("Codex started on a configuration without %q:\n%s", want, cfg)
		}
	}
	if _, err := os.Stat(filepath.Join(p.CodexSkills(), "crewlet:deploy", "SKILL.md")); err != nil {
		t.Errorf("the run's skill is not beside the home's: %v", err)
	}
	argv := strings.Join(recorded(t, record, "server-argv"), "\n")
	for _, limit := range []string{"features.plugins", "features.remote_plugin", "skills.bundled"} {
		if strings.Contains(argv, limit) {
			t.Errorf("the app-server started with %s set, which a person's session leaves on:\n%s", limit, argv)
		}
	}
	if !strings.Contains(argv, "analytics.enabled=false") {
		t.Errorf("the app-server started without analytics off:\n%s", argv)
	}
}
