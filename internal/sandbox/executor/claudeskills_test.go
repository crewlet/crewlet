package executor_test

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/sandbox"
	"github.com/crewlet/crewlet/internal/sandbox/executor"
)

// A CLAUDE CODE RUN TAKES ITS SKILLS AS A PLUGIN OF ITS OWN: a manifest naming
// it for the skills' namespace, each skill under `skills/<name>` with its
// frontmatter and its body as handed, and one PostToolUse hook on the Skill
// tool, which hands the run's bridge the name of the skill the tool loaded.
//
// Mutation: name the plugin anything but [sandbox.SkillNamespace], and every
// skill is listed under a name the engine never told the run's model; drop the
// hook, or fire it on another tool, and no load reaches the bridge.
func TestAClaudeCodeRunTakesItsSkillsAsAPluginOfItsOwn(t *testing.T) {
	t.Parallel()
	b := sandbox.NewFakeSandbox("box-1")
	p := executor.PathsFor(b)
	req := sandbox.RunRequest{Brief: "x", Skills: handedSkills(), SkillLoads: skillWiring()}
	if _, err := claude().WriteConfig(t.Context(), b, req, executor.Session{}, p); err != nil {
		t.Fatalf("WriteConfig: %v", err)
	}
	plugin := p.ClaudeSkillsPlugin()

	var manifest struct {
		Name string `json:"name"`
	}
	if err := json.Unmarshal([]byte(boxFile(t, b, plugin+"/.claude-plugin/plugin.json")), &manifest); err != nil {
		t.Fatalf("the manifest is not JSON: %v", err)
	}
	if manifest.Name != sandbox.SkillNamespace {
		t.Errorf("the plugin is named %q, want %q: its skills are listed under it", manifest.Name,
			sandbox.SkillNamespace)
	}
	// THE SKILL'S OWN NAME in its frontmatter: the plugin's prefixes it.
	if got, want := boxFile(t, b, plugin+"/skills/mcp-github/SKILL.md"), githubSkillFile("mcp-github"); got != want {
		t.Errorf("the skill's file is\n%s\nwant\n%s", got, want)
	}
	if got := boxFile(t, b, plugin+"/skills/getting-unstuck/SKILL.md"); !strings.HasSuffix(got, "Ask a colleague.\n") {
		t.Errorf("the second skill's file is\n%s", got)
	}

	var hooks struct {
		Hooks map[string][]struct {
			Matcher string `json:"matcher"`
			Hooks   []struct {
				Type, Server, Tool string
				Input              map[string]string
				Timeout            int
			} `json:"hooks"`
		} `json:"hooks"`
	}
	raw := boxFile(t, b, plugin+"/hooks/hooks.json")
	if err := json.Unmarshal([]byte(raw), &hooks); err != nil {
		t.Fatalf("the hooks are not JSON: %v", err)
	}
	used := hooks.Hooks["PostToolUse"]
	if len(hooks.Hooks) != 1 || len(used) != 1 || used[0].Matcher != "Skill" || len(used[0].Hooks) != 1 {
		t.Fatalf("the plugin's hooks are\n%s\nwant one PostToolUse hook on the Skill tool", raw)
	}
	h := used[0].Hooks[0]
	if h.Type != "mcp_tool" || h.Server != "crewlet" || h.Tool != "skill_loaded" ||
		h.Input["skill"] != "${tool_input.skill}" || h.Timeout <= 0 {
		t.Errorf("the hook is %+v, want an mcp_tool call of the wiring's tool naming the loaded skill", h)
	}
}

// A CLAUDE CODE RUN'S SKILLS ARE ADDED BY THE BOX'S RELEASE: at or after
// claudeSkillsFloor the run starts with its plugin, its hook marked one of the
// engine's (hk). On an older release a run with a required skill is refused
// before its harness starts (its tools would be refused for good), and one with
// advisory skills starts without them.
//
// Mutation: add the plugin on any release, and an older one lists skills whose
// loads it never reports; refuse an advisory run, and a box short of one release
// can run nothing.
func TestAClaudeCodeRunsSkillsAreAddedByItsRelease(t *testing.T) {
	t.Parallel()
	p := executor.Paths{Home: "/home/box"}
	gate := `if at 2 1 118; then set -- "$@" --plugin-dir '` + p.ClaudeSkillsPlugin() + `'; hk=1; else `
	tail := func(script string) string {
		_, after, ok := strings.Cut(script, gate)
		if !ok {
			t.Fatalf("the script adds no plugin by the release:\n%s", script)
		}
		older, _, _ := strings.Cut(after, "; fi\n")
		return older
	}

	required := scriptOf(t, commandOf(claude(),
		sandbox.RunRequest{Brief: "x", Skills: handedSkills(), SkillLoads: skillWiring()}, p, ""))
	if older := tail(required); !strings.HasSuffix(older, "; exit 1") {
		t.Errorf("an older release runs a run with a required skill: %s", older)
	}
	advisory := handedSkills()[1:]
	script := scriptOf(t, commandOf(claude(),
		sandbox.RunRequest{Brief: "x", Skills: advisory, SkillLoads: skillWiring()}, p, ""))
	if older := tail(script); strings.Contains(older, "exit") || !strings.Contains(older, "starts without") {
		t.Errorf("an older release refuses a run whose skills are advisory, or says nothing: %s", older)
	}
	if plain := scriptOf(t, commandOf(claude(), sandbox.RunRequest{Brief: "x"}, p, "")); strings.Contains(plain, "--plugin-dir") {
		t.Errorf("a run handed no skills is started with a plugin:\n%s", plain)
	}
}
