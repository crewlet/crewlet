package executor_test

import (
	"encoding/json"
	"strings"
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
