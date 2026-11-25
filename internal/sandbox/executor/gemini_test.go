package executor_test

import (
	"encoding/json"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/sandbox"
	"github.com/crewlet/crewlet/internal/sandbox/executor"
)

// geminiWired is a run handed the bridge, its notes and a skill, signing in
// with a key: everything a Gemini CLI run's configuration can carry.
func geminiWired() sandbox.RunRequest {
	const bridge = "http://127.0.0.1:7/mcp/run-token-placeholder"
	return sandbox.RunRequest{System: "SYSTEM", Brief: "x", Model: "gemini-2.5-pro",
		Env: map[string]string{"GEMINI_API_KEY": "key-placeholder"},
		MCPServers: map[string]sandbox.MCPServer{"crewlet": {Name: "crewlet", Transport: sandbox.TransportHTTP,
			URL: bridge, CallTimeout: 5 * time.Minute}},
		Steer:      sandbox.SteerWiring{Server: "crewlet", Tool: "steer_take", URL: bridge + "/steer"},
		SkillLoads: sandbox.SkillWiring{Server: "crewlet", Tool: "skill_loaded", URL: bridge + "/skills"},
		Skills:     []sandbox.Skill{{Name: "probe", Description: "A probe.", Body: "BODY"}},
	}
}

// writtenGemini is what WriteConfig wrote for req: the settings decoded, and
// every file by path.
func writtenGemini(t *testing.T, req sandbox.RunRequest) (map[string]any, *sandbox.FakeSandbox, executor.Paths) {
	t.Helper()
	b := sandbox.NewFakeSandbox("box-1")
	p := executor.PathsFor(b)
	if _, err := (executor.GeminiCLI{}).WriteConfig(t.Context(), b, req, executor.Session{}, p); err != nil {
		t.Fatalf("WriteConfig: %v", err)
	}
	var settings map[string]any
	if err := json.Unmarshal([]byte(boxFile(t, b, p.GeminiSettings())), &settings); err != nil {
		t.Fatalf("the settings do not decode: %v", err)
	}
	return settings, b, p
}

// A GEMINI RUN'S SETTINGS ARE THE MEASURED SHAPES, AND ITS HOOKS FOLLOW ITS
// WIRING: both hooks for notes and a skill, AfterAgent alone for notes, each
// the take script under ten seconds; the bridge as `httpUrl` held to the
// question hold in ms; the key sign-in for a key; subagents, statistics and
// updates off; invoke_agent and enter_plan_mode denied; the skill dashed; the
// note instruction last. A run wired to nothing has no hook, take script or
// instruction, and a login names no sign-in.
//
// Mutations, each red: write the hooks whatever the wiring, or AfterTool for
// notes; drop the server's timeout; add the instruction to a run its notes
// cannot reach; name the key sign-in for a login.
func TestAGeminiRunsSettingsFollowItsWiring(t *testing.T) {
	t.Parallel()
	settings, b, p := writtenGemini(t, geminiWired())
	hooks, _ := settings["hooks"].(map[string]any)
	for _, event := range []string{"AfterTool", "AfterAgent"} {
		raw, _ := json.Marshal(hooks[event])
		if !strings.Contains(string(raw), `"timeout":10000`) ||
			!strings.Contains(string(raw), p.GeminiTake()) || !strings.Contains(string(raw), p.GeminiTakeConfig()) {
			t.Errorf("the %s hook is %s, want the take script under 10,000 ms", event, raw)
		}
	}
	raw, _ := json.Marshal(settings)
	for _, want := range []string{`"httpUrl":"http://127.0.0.1:7/mcp/run-token-placeholder"`, `"timeout":300000`,
		`"selectedType":"gemini-api-key"`, `"enableAgents":false`, `"usageStatisticsEnabled":false`,
		`"enableAutoUpdate":false`} {
		if !strings.Contains(string(raw), want) {
			t.Errorf("the settings lack %s:\n%s", want, raw)
		}
	}
	if policy := boxFile(t, b, p.GeminiPolicy()); !strings.Contains(policy, `"invoke_agent", "enter_plan_mode"`) ||
		!strings.Contains(policy, `decision = "deny"`) {
		t.Errorf("the policy is %q, want invoke_agent and enter_plan_mode denied", policy)
	}
	if skill := boxFile(t, b, p.GeminiSkills()+"/crewlet-probe/SKILL.md"); !strings.Contains(skill, `name: "crewlet-probe"`) {
		t.Errorf("the skill is %q, want it named crewlet-probe", skill)
	}
	if system := boxFile(t, b, p.System()); !strings.HasSuffix(system, executor.GeminiCLISteerInstruction) {
		t.Errorf("the system text does not end with the note instruction:\n%s", system)
	}

	notesOnly := geminiWired()
	notesOnly.Skills, notesOnly.SkillLoads = nil, sandbox.SkillWiring{}
	settings, _, _ = writtenGemini(t, notesOnly)
	if hooks, _ := settings["hooks"].(map[string]any); hooks["AfterAgent"] == nil || hooks["AfterTool"] != nil {
		t.Errorf("a run wired to notes alone has hooks %v, want AfterAgent alone: no note is taken mid-run", hooks)
	}

	login := sandbox.RunRequest{System: "SYSTEM", Brief: "x", Model: "gemini-2.5-pro"}
	settings, b, p = writtenGemini(t, login)
	if _, ok := settings["hooks"]; ok {
		t.Errorf("a run wired to nothing has hooks: %v", settings["hooks"])
	}
	if _, ok := settings["security"]; ok {
		t.Errorf("a login run names a sign-in: %v", settings["security"])
	}
	if take := boxFile(t, b, p.GeminiTake()); take != "" {
		t.Error("a run wired to nothing was handed the take script")
	}
	if system := boxFile(t, b, p.System()); system != "SYSTEM" {
		t.Errorf("a run its notes cannot reach has system text %q, want its own", system)
	}
}

// A GEMINI RUN STARTS ITS HARNESS WITH THE RUN'S ENVIRONMENT AND NO BRIDGE
// VARIABLE: no variable carries the bridge's address, its notes route or its
// token, which its settings and its take script's configuration hold; the key
// it signs in with, the Gitlet names and an operator's own exporter settings
// stay. The stand-in records the environment Gemini CLI starts with, in a
// real box.
//
// Mutations, each red: hand the bridge's address through a variable; unset
// any OTEL_ variable; drop the key or the Gitlet names.
func TestAGeminiRunStartsItsHarnessWithTheRunsEnvironmentAndNoBridgeVariable(t *testing.T) {
	t.Parallel()
	const bridge = "http://127.0.0.1:7/mcp/bridge-token-placeholder"
	runner, b := geminiBox(t, `env > "$HOME/harness-env"
echo '{"type":"result","status":"success"}'`)
	_, res := runToEnd(t, runner, b, sandbox.RunRequest{Brief: "begin",
		MCPServers: map[string]sandbox.MCPServer{"crewlet": {Name: "crewlet", Transport: sandbox.TransportHTTP,
			URL: bridge}},
		Steer: sandbox.SteerWiring{Server: "crewlet", Tool: "steer_take", URL: bridge + "/steer"},
		Env: map[string]string{"GEMINI_API_KEY": "key-placeholder",
			"OTEL_EXPORTER_OTLP_ENDPOINT": "https://collector.example.com",
			"OTEL_SERVICE_NAME":           "seat-own",
			sandbox.GitURLVar:             "https://git.example.com/box/git",
			sandbox.GitAccessFileVar:      "/home/box/.crewlet/git-access"}})
	if !res.Success {
		t.Fatalf("the run failed: %q / %q", res.Error, res.Transcript)
	}
	env := "\n" + boxFile(t, b, b.Home()+"/harness-env")
	for _, line := range strings.Split(env, "\n") {
		if strings.Contains(line, "bridge-token-placeholder") {
			t.Errorf("Gemini CLI starts with %q, want it absent", line)
		}
	}
	for _, kept := range []string{"GEMINI_API_KEY=", sandbox.GitURLVar + "=",
		sandbox.GitAccessFileVar + "=", "OTEL_EXPORTER_OTLP_ENDPOINT=", "OTEL_SERVICE_NAME="} {
		if !strings.Contains(env, "\n"+kept) {
			t.Errorf("Gemini CLI starts without %s, which the run was handed", strings.TrimSuffix(kept, "="))
		}
	}
}

// A GEMINI RUN WHOSE NOTES NAME NO SERVER IT IS HANDED IS REFUSED: every take
// would have no endpoint, and its notes would be served to nothing.
//
// Mutations, each red: drop the check in geminiTakeConfig.
func TestAGeminiRunWhoseNotesNameNoServerIsRefused(t *testing.T) {
	t.Parallel()
	req := geminiWired()
	req.Steer.Server = "elsewhere"
	b := sandbox.NewFakeSandbox("box-1")
	if _, err := (executor.GeminiCLI{}).WriteConfig(t.Context(), b, req, executor.Session{}, executor.PathsFor(b)); err == nil ||
		!strings.Contains(err.Error(), "elsewhere") {
		t.Fatalf("WriteConfig answered %v, want the notes' server refused", err)
	}
}

// A GEMINI HOOK'S ANSWER IS A BLOCK AT THE FINISH ALONE, WITHIN ITS BOUND. Its
// notes' messages are the block's reason, a blank line apart, its take's token
// in the field the take script records and drops; nothing for no note, and
// nothing at any boundary but the finish, where no Gemini CLI hook takes. One
// answer carries what fits in 9,000 UTF-16 units, at least one message, and
// leaves beside the widest note the room the engine's largest item needs.
//
// Mutations, each red: answer after a batch; join without the blank line;
// leave the token out; count bytes rather than UTF-16 units.
func TestAGeminiHooksAnswerIsABlockAtTheFinishAlone(t *testing.T) {
	t.Parallel()
	notes := []executor.HookNote{{By: "ada", Message: "## A note from ada"}, {Call: true, Message: "## A call"}}
	var got map[string]string
	if err := json.Unmarshal([]byte(executor.HookAnswer(executor.GeminiCLIName, "Stop", "take-1", notes)), &got); err != nil {
		t.Fatalf("the answer does not decode: %v", err)
	}
	if got["decision"] != "block" || got["reason"] != "## A note from ada\n\n## A call" ||
		got["crewlet_take"] != "take-1" || len(got) != 3 {
		t.Errorf("the answer is %v, want a block with the notes as its reason and the take's token", got)
	}
	for event, n := range map[string][]executor.HookNote{"PostToolBatch": notes, "Stop": nil} {
		if answer := executor.HookAnswer(executor.GeminiCLIName, event, "take-1", n); answer != "" {
			t.Errorf("at %s with %d notes the hook answered %q, want nothing", event, len(n), answer)
		}
	}
	half := strings.Repeat("ü", 4499) // two bytes a character, one UTF-16 unit
	if got := executor.HookNotesThatFit(executor.GeminiCLIName, []string{half, half, "x"}); got != 2 {
		t.Errorf("%d of two 4,499-unit notes and one more fit, want 2", got)
	}
	if got := executor.HookNotesThatFit(executor.GeminiCLIName, []string{strings.Repeat("x", 9001), "x"}); got != 1 {
		t.Errorf("%d notes fit after an oversized one, want it alone", got)
	}
	if room, hooks := executor.HookRoomBeside(executor.GeminiCLIName, "abc"); !hooks || room != 9000-3-2 {
		t.Errorf("the room beside a 3-unit note is %d (%v), want 8,995", room, hooks)
	}
}

// A GEMINI RUN PUTS THE HOME'S OWN SETTINGS BACK: Gemini CLI starts on the
// run's settings over the home's own, and once the run ends the home's file
// is byte for byte as it was, or absent where the home had none, and nothing
// is held for it.
//
// Mutations, each red: restore nothing at the script's end; leave the file of
// a home that had none.
func TestAGeminiRunPutsTheHomesOwnSettingsBack(t *testing.T) {
	t.Parallel()
	for name, home := range map[string]string{"a home with settings": "// mine\n{\"ui\": {\"theme\": \"mine\"}}\n",
		"a home with none": ""} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			runner, box := geminiBox(t, `cp "$HOME/.gemini/settings.json" "$HOME/observed.settings.json"
`+observedRun)
			if home != "" {
				plant(t, box, map[string]string{".gemini/settings.json": home})
			}
			runObserved(t, runner, box, sandbox.RunRequest{Brief: "go", Model: "gemini-2.5-pro", MCPServers: loadsBridge})
			seen := boxFile(t, box, box.Home()+"/observed.settings.json")
			if !strings.Contains(seen, codexBridgeURL) || strings.Contains(seen, "mine") != (home != "") {
				t.Errorf("Gemini CLI started on home settings %s, want the run's over the home's own", seen)
			}
			after, err := os.ReadFile(filepath.Join(box.Home(), ".gemini", "settings.json"))
			switch {
			case home == "" && !os.IsNotExist(err):
				t.Errorf("a home that had no settings holds %q after the run (%v)", after, err)
			case home != "" && string(after) != home:
				t.Errorf("the home's settings read %q after the run (%v), want %q", after, err, home)
			}
			if _, err := os.Stat(executor.PathsFor(box).GeminiHeldSettings()); !os.IsNotExist(err) {
				t.Errorf("the home's settings are still held after the run (%v)", err)
			}
		})
	}
}

// A KILLED GEMINI RUN'S SETTINGS LEAVE THE HOME AT THE NEXT RUN'S START,
// whichever harness that run drives: a run killed outright while Gemini CLI
// works (SIGKILL, as a teardown past its grace or a lost host kills it; a
// SIGTERM runs the script's restore) never reaches its restore, so the
// home's file still holds its settings, its bridge's token among them; the
// next run's clear puts the home's own back, its entries and comments as they
// were, and afterwards no file of the home holds either run's token.
//
// Mutation, red: leave the restore out of the clear (the other harness's case;
// a Gemini CLI run still builds on the held copy and puts it back).
func TestAKilledGeminiRunsSettingsLeaveTheHomeAtTheNextRunsStart(t *testing.T) {
	t.Parallel()
	const home = "// the home's own\n{\"mcpServers\": {\"home\": {\"httpUrl\": \"http://127.0.0.1:9/home\"}}}\n"
	for _, next := range []string{"Gemini CLI's", "another harness's"} {
		t.Run("the next run is "+next, func(t *testing.T) {
			t.Parallel()
			runner, box := geminiBox(t, `cp "$HOME/.gemini/settings.json" "$HOME/observed.settings.json"
if [ -e "$HOME/block" ]; then rm -f "$HOME/block"; : > "$HOME/working"; exec sleep 600; fi
`+observedRun)
			plant(t, box, map[string]string{".gemini/settings.json": home, "block": ""})
			handle, err := runner.Start(t.Context(), box, killedRunRequest("GEMINI-RUN-ONE-TOKEN"))
			if err != nil {
				t.Fatalf("Start: %v", err)
			}
			boxFileAppears(t, box, box.Home()+"/observed.settings.json", "Gemini CLI to start")
			waitForFile(t, filepath.Join(box.Home(), "working"), "the stand-in to work")
			killOutright(t, handle.CommandID)
			awaitOver(t, runner, box, handle, 15*time.Second, "the job was killed")
			if left := homeFilesHolding(t, box.Home(), "GEMINI-RUN-ONE-TOKEN"); !slices.Equal(left, []string{".gemini/settings.json"}) {
				t.Fatalf("the killed run left its token in %v, want the home's settings alone: the kill let the "+
					"script put them back, so this case shows nothing", left)
			}
			if next == "Gemini CLI's" {
				runObserved(t, runner, box, killedRunRequest("GEMINI-RUN-TWO-TOKEN"))
				seen := boxFile(t, box, box.Home()+"/observed.settings.json")
				if strings.Contains(seen, "GEMINI-RUN-ONE-TOKEN") || !strings.Contains(seen, "GEMINI-RUN-TWO-TOKEN") ||
					!strings.Contains(seen, "/home") {
					t.Errorf("the next run started on home settings %s, want its own over the home's alone", seen)
				}
			} else if err := executor.NewClaudeCode().ClearArtifacts(t.Context(), box); err != nil {
				t.Fatalf("ClearArtifacts: %v", err)
			}
			if got := boxFile(t, box, filepath.Join(box.Home(), ".gemini", "settings.json")); got != home {
				t.Errorf("the home's settings read %q after the next run's start, want them as they were", got)
			}
			for _, token := range []string{"GEMINI-RUN-ONE-TOKEN", "GEMINI-RUN-TWO-TOKEN"} {
				if left := homeFilesHolding(t, box.Home(), token); len(left) > 0 {
					t.Errorf("%s is in %v afterwards, files of the home's", token, left)
				}
			}
		})
	}
}

// killedRunRequest is a Gemini CLI run whose bridge carries token.
func killedRunRequest(token string) sandbox.RunRequest {
	return sandbox.RunRequest{Brief: "go", Model: "gemini-2.5-pro", MCPServers: map[string]sandbox.MCPServer{
		"crewlet": {Transport: sandbox.TransportHTTP, URL: codexBridgeURL, Headers: map[string]string{
			"Authorization": "Bearer " + token}}}}
}

// homeFilesHolding is the files under home, relative to it, that hold token:
// the run's work directory and the stand-in's records left out.
func homeFilesHolding(t *testing.T, home, token string) []string {
	t.Helper()
	var holding []string
	err := filepath.WalkDir(home, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(home, path)
		switch {
		case d.IsDir() && rel == ".crewlet":
			return filepath.SkipDir
		case !d.Type().IsRegular() || strings.HasPrefix(rel, "observed"):
			return nil
		}
		raw, err := os.ReadFile(path) //nolint:gosec // a file of the test's own box
		if err == nil && strings.Contains(string(raw), token) {
			holding = append(holding, rel)
		}
		return err
	})
	if err != nil {
		t.Fatalf("walking the box home: %v", err)
	}
	return holding
}

// GEMINI CLI'S SETTINGS BUILD ON THE HOME'S HELD COPY, NEVER A KILLED RUN'S:
// the helper's settings mode holds the home's file before it writes the run's
// over it, and where a held copy is already there (its restore failed), that
// copy is the home's, so the earlier run's server goes and the copy stays.
//
// Mutation, red: hold the live file although a held copy is there.
func TestGeminisSettingsBuildOnTheHomesHeldCopy(t *testing.T) {
	t.Parallel()
	node := geminiNode(t)
	const (
		homes   = `{"mcpServers": {"home": {"httpUrl": "http://127.0.0.1:9/home"}}}`
		earlier = `{"mcpServers": {"home": {"httpUrl": "http://127.0.0.1:9/home"}, "crewlet": {"httpUrl": "http://127.0.0.1:9/one"}}}`
	)
	for name, held := range map[string]string{"held by a killed run": homes, "none held": ""} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			own, live, hold := filepath.Join(dir, "run.json"), filepath.Join(dir, "settings.json"), filepath.Join(dir, "held.json")
			files := map[string]string{own: `{"mcpServers": {"crewlet": {"httpUrl": "http://127.0.0.1:9/two"}}}`, live: homes}
			if held != "" {
				files[live], files[hold] = earlier, held
			}
			for path, body := range files {
				if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			if out, err := exec.CommandContext(t.Context(), node, "gemini_helper.mjs", "settings", own, live, hold).CombinedOutput(); err != nil {
				t.Fatalf("settings: %v\n%s", err, out)
			}
			var got struct {
				MCPServers map[string]struct {
					HTTPURL string `json:"httpUrl"`
				} `json:"mcpServers"`
			}
			if err := json.Unmarshal(readFileBytes(t, live), &got); err != nil {
				t.Fatal(err)
			}
			if got.MCPServers["crewlet"].HTTPURL != "http://127.0.0.1:9/two" || got.MCPServers["home"].HTTPURL == "" {
				t.Errorf("the home's settings hold servers %v, want the home's and the run's own", got.MCPServers)
			}
			if kept := string(readFileBytes(t, hold)); kept != homes {
				t.Errorf("the held copy reads %q, want the home's own %q", kept, homes)
			}
		})
	}
}

// killOutright sends SIGKILL to the process group of the local job whose
// handle is id, as a teardown past its grace does: no trap of its script runs.
// A job in no group of its own fails the case, named, rather than killing the
// suite's.
func killOutright(t *testing.T, id string) {
	t.Helper()
	pid, err := strconv.Atoi(id)
	if err != nil || pid <= 1 {
		t.Fatalf("the job handle %q is no process id", id)
	}
	group, err := syscall.Getpgid(pid)
	if err != nil || group == syscall.Getpgrp() {
		t.Fatalf("the job %d has no group of its own (%d, %v)", pid, group, err)
	}
	if err := syscall.Kill(-group, syscall.SIGKILL); err != nil {
		t.Fatalf("killing group %d: %v", group, err)
	}
}
