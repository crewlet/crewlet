package executor_test

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/sandbox"
	"github.com/crewlet/crewlet/internal/sandbox/executor"
)

// qwenWired is a run handed the bridge, its notes and a skill, signing in
// with a key: everything a Qwen Code run's configuration can carry.
func qwenWired() sandbox.RunRequest {
	const bridge = "http://127.0.0.1:7/mcp/run-token-placeholder"
	return sandbox.RunRequest{System: "SYSTEM", Brief: "x", Model: "qwen3-coder-plus",
		Env: map[string]string{"OPENAI_API_KEY": "key-placeholder", "OPENAI_BASE_URL": "https://example.com/v1"},
		MCPServers: map[string]sandbox.MCPServer{"crewlet": {Name: "crewlet", Transport: sandbox.TransportHTTP,
			URL: bridge, AlwaysLoaded: true, CallTimeout: 5 * time.Minute}},
		Steer:      sandbox.SteerWiring{Server: "crewlet", Tool: "steer_take", URL: bridge + "/steer"},
		SkillLoads: sandbox.SkillWiring{Server: "crewlet", Tool: "skill_loaded", URL: bridge + "/skills"},
		Skills:     []sandbox.Skill{{Name: "probe", Description: "A probe.", Body: "BODY"}},
	}
}

// writtenQwen is what WriteConfig wrote for req: the settings and the
// helper's configuration decoded, and the box.
func writtenQwen(t *testing.T, req sandbox.RunRequest) (settings, helper map[string]any, b *sandbox.FakeSandbox,
	p executor.Paths) {
	t.Helper()
	b = sandbox.NewFakeSandbox("box-1")
	p = executor.PathsFor(b)
	if _, err := (executor.QwenCode{}).WriteConfig(t.Context(), b, req, executor.Session{}, p); err != nil {
		t.Fatalf("WriteConfig: %v", err)
	}
	for path, into := range map[string]*map[string]any{p.QwenSettings(): &settings, p.QwenHelperConfig(): &helper} {
		if err := json.Unmarshal([]byte(boxFile(t, b, path)), into); err != nil {
			t.Fatalf("%s does not decode: %v", path, err)
		}
	}
	return settings, helper, b, p
}

// A QWEN RUN'S SETTINGS ARE THE MEASURED SHAPES, AND ITS HOOK AND SKILLS
// FOLLOW ITS WIRING: the key in the settings and the shell's environment;
// the bridge as `httpUrl` held to the question hold, its tools always
// offered; co-author trailer, memory, statistics and updates off, the chat
// file kept; agent, ask and plan tools denied; the run's skills named, a hook
// reporting a load; the note instruction last; the helper told the routes and
// the bridge, nothing secret. A run wired to nothing has no hook, skills or
// instruction; one handed no key signs in with the paid login.
//
// Mutations, each red: key to OPENAI_API_KEY (or unset for the shell); drop
// alwaysLoadTools; gitCoAuthor on; drop a denied tool; instruction or hook
// unwired; the helper handed the key; a keyless run signed in as openai.
func TestAQwenRunsSettingsAreTheMeasuredShapesAndFollowItsWiring(t *testing.T) {
	t.Parallel()
	settings, helper, b, p := writtenQwen(t, qwenWired())
	raw, _ := json.Marshal(settings)
	for _, want := range []string{
		`"apiKey":"key-placeholder"`,
		`"httpUrl":"http://127.0.0.1:7/mcp/run-token-placeholder"`, `"timeout":300000`, `"alwaysLoadTools":true`,
		`"gitCoAuthor":{"commit":false,"pr":false}`, `"enableManagedAutoMemory":false`,
		`"enableManagedAutoDream":false`, `"usageStatisticsEnabled":false`, `"enableAutoUpdate":false`,
		`"chatRecording":true`, `"enableFollowupSuggestions":false`,
		`"directories":["` + p.QwenSkills() + `"]`, `"matcher":"skill"`, `"timeout":10,"type":"command"`,
	} {
		if !strings.Contains(string(raw), want) {
			t.Errorf("the settings lack %s:\n%s", want, raw)
		}
	}
	deny, _ := settings["permissions"].(map[string]any)["deny"].([]any)
	var denied []string
	for _, d := range deny {
		denied = append(denied, d.(string))
	}
	if want := []string{"agent", "ask_user_question", "enter_plan_mode", "exit_plan_mode"}; !slices.Equal(denied, want) {
		t.Errorf("the settings deny %v, want %v", denied, want)
	}
	if skill := boxFile(t, b, p.QwenSkills()+"/crewlet-probe/SKILL.md"); !strings.Contains(skill, `name: "crewlet-probe"`) {
		t.Errorf("the skill is %q, want it named crewlet-probe", skill)
	}
	if system := boxFile(t, b, p.System()); !strings.HasSuffix(system, executor.QwenCodeSteerInstruction) {
		t.Errorf("the system text does not end with the note instruction:\n%s", system)
	}
	if out := (executor.QwenCode{}).Output(p); slices.Contains(out.Unset, "OPENAI_API_KEY") {
		t.Errorf("the wrapper keeps OPENAI_API_KEY from the run's shell: %v", out.Unset)
	}
	want := map[string]any{"authType": "openai", "qwenHome": p.QwenDir(), "servers": []any{"crewlet"},
		"bridge": "crewlet", "mcpWaitMs": float64(60000), "notes": qwenWired().Steer.URL,
		"skills": qwenWired().SkillLoads.URL, "namespace": "crewlet"}
	for key, value := range want {
		got, _ := json.Marshal(helper[key])
		if exp, _ := json.Marshal(value); string(got) != string(exp) {
			t.Errorf("the helper's %s is %s, want %s", key, got, exp)
		}
	}
	if config, _ := json.Marshal(helper); strings.Contains(string(config), "key-placeholder") {
		t.Errorf("the helper's configuration holds the key: %s", config)
	}

	bare := sandbox.RunRequest{System: "SYSTEM", Brief: "x"}
	settings, helper, b, p = writtenQwen(t, bare)
	if _, ok := settings["hooks"]; ok {
		t.Errorf("a run wired to nothing has hooks: %v", settings["hooks"])
	}
	if skills, _ := settings["skills"].(map[string]any); skills["directories"] != nil {
		t.Errorf("a run handed no skills names a skill directory: %v", skills)
	}
	if helper["notes"] != nil || helper["bridge"] != nil {
		t.Errorf("a run wired to nothing hands its helper routes: %v", helper)
	}
	if helper["authType"] != "qwen-subscription" {
		t.Errorf("a run handed no key signs in as %v, want the login the box holds", helper["authType"])
	}
	if system := boxFile(t, b, p.System()); system != "SYSTEM" {
		t.Errorf("a run its notes cannot reach has system text %q, want its own", system)
	}
}

// A QWEN RUN IS ITS HELPER UNDER THE END PROTOCOL, ITS TEXT ADDED TO QWEN
// CODE'S PROMPT. The script publishes its pid and traps an end before it
// starts `node <helper> run <config> <session> <new|load>`, its session the
// one the gate named, so an end request reaches the helper as SIGTERM, which
// it answers with session/cancel; the run's
// text names QWEN_SYSTEM_IDENTITY_MD, the channel that keeps Qwen Code's own
// prompt; the models.dev fetch is off; the entry's model is OPENAI_MODEL.
//
// Mutations, each red: start the helper outside launchHarness (no end
// protocol); leave the session or its mode off its command line; name a
// session the gate did not; pin QWEN_SYSTEM_MD to a file, which replaces the
// prompt the identity file keeps.
func TestAQwenRunIsItsHelperUnderTheEndProtocol(t *testing.T) {
	t.Parallel()
	q := executor.QwenCode{}
	req := qwenWired()
	session := q.Session(req)
	p := executor.Paths{Home: "/home/box"}
	script := scriptOf(t, q.Command(req, session, p, ""))
	for _, want := range []string{"trap endask USR1", "s='" + session.ID + "' k=no w= cf=\n",
		`if [ "$k" = yes ]; then m=load; else m=new; fi`,
		"node '" + p.QwenHelper() + "' run '" + p.QwenHelperConfig() + `' "$s" "$m" <&3 3<&- &`} {
		if !strings.Contains(script, want) {
			t.Errorf("the script lacks %q:\n%s", want, script)
		}
	}
	pins := map[string]string{}
	for _, pin := range q.Pins(t.Context(), req, p) {
		pins[pin.Name] = pin.Value
	}
	if pins["QWEN_SYSTEM_IDENTITY_MD"] != p.System() || pins["QWEN_CODE_MODELS_DEV"] != "off" ||
		pins["OPENAI_MODEL"] != "qwen3-coder-plus" || pins["QWEN_SYSTEM_MD"] != "false" {
		t.Errorf("the run pins %v", pins)
	}
	if again := q.Session(req); again.ID == session.ID {
		t.Errorf("two runs share the session %s, which Qwen Code refuses to create twice", again.ID)
	}
}
