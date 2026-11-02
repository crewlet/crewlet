package executor_test

import (
	"encoding/json"
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/sandbox"
	"github.com/crewlet/crewlet/internal/sandbox/executor"
)

// handedSkills are two skills a run is handed as its own: one required, one
// advisory, the first described in prose that holds what a YAML scalar would
// read as structure.
func handedSkills() []sandbox.Skill {
	return []sandbox.Skill{
		{Name: "mcp-github", Description: `Read / search code: issues, "PRs" & <reviews>.`,
			Body: "# GitHub tools\n\nAuthor code in your own checkout.\n", Required: true},
		{Name: "getting-unstuck", Description: "When you are stuck: ask, then wait.",
			Body: "# Getting unstuck\n\nAsk a colleague.\n"},
	}
}

// skillWiring is where a run handed skills reports loading one.
func skillWiring() sandbox.SkillWiring {
	return sandbox.SkillWiring{Server: "crewlet", Tool: "skill_loaded",
		URL: "http://127.0.0.1:7/mcp/run-token-placeholder/skill"}
}

// githubSkillFile is the SKILL.md of handedSkills' first, named name.
func githubSkillFile(name string) string {
	return "---\nname: \"" + name + "\"\ndescription: \"Read / search code: issues, \\\"PRs\\\" & <reviews>.\"\n---\n\n" +
		"# GitHub tools\n\nAuthor code in your own checkout.\n"
}

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
	if _, err := claude().WriteConfig(t.Context(), b, req, p); err != nil {
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
// 2.1.118, the first whose hooks call an MCP tool, the run starts with its
// plugin. On an older release a run with a required skill is refused before
// its harness starts — every call to a tool the skill covers would be refused
// for good — and a run whose skills are advisory starts without them.
//
// Mutation: add the plugin on any release, and an older one lists skills whose
// loads it never reports; refuse an advisory run, and a box short of one release
// can run nothing.
func TestAClaudeCodeRunsSkillsAreAddedByItsRelease(t *testing.T) {
	t.Parallel()
	p := executor.Paths{Home: "/home/box"}
	gate := `if at 2 1 118; then set -- "$@" --plugin-dir '` + p.ClaudeSkillsPlugin() + `'; else `
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

// AN OPENCODE RUN TAKES ITS SKILLS AS A DIRECTORY ITS CONFIGURATION NAMES: a
// directory per skill named as the skill is listed — the id OpenCode loads a
// skill by — and, in `plugins`, the plugin that reports each load, its one
// option the skill route, read from a file as the notes plugin's is.
//
// Mutation: name a skill's directory by its own name, and OpenCode lists it as
// a name the engine never told the run's model; leave the plugin out, and no
// load reaches the bridge.
func TestAnOpenCodeRunTakesItsSkillsAsADirectoryItsConfigurationNames(t *testing.T) {
	t.Parallel()
	req := sandbox.RunRequest{Brief: "x", Skills: handedSkills(), SkillLoads: skillWiring()}
	_, raw, b := writeOpenCodeConfigIn(t, req)
	p := executor.PathsFor(b)
	var cfg struct {
		Skills  []string         `json:"skills"`
		Plugins []openCodePlugin `json:"plugins"`
	}
	if err := json.Unmarshal([]byte(raw), &cfg); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if !slices.Equal(cfg.Skills, []string{p.OpenCodeSkills()}) {
		t.Errorf("skills = %v, want the run's own directory", cfg.Skills)
	}
	if len(cfg.Plugins) != 1 || cfg.Plugins[0].Package != p.OpenCodeSkillsPlugin() {
		t.Fatalf("plugins = %+v, want the skill-load plugin alone", cfg.Plugins)
	}
	if got := openCodeValue(t, b, cfg.Plugins[0].Options.URL); got != skillWiring().URL {
		t.Errorf("the plugin's route is %q, want %q", got, skillWiring().URL)
	}
	if got, want := boxFile(t, b, p.OpenCodeSkills()+"/crewlet:mcp-github/SKILL.md"),
		githubSkillFile("crewlet:mcp-github"); got != want {
		t.Errorf("the skill's file is\n%s\nwant\n%s", got, want)
	}
	embedded, err := os.ReadFile("opencode_skills.js")
	if err != nil {
		t.Fatalf("read the plugin's source: %v", err)
	}
	if got := boxFile(t, b, p.OpenCodeSkillsPlugin()+"/index.js"); got != string(embedded) {
		t.Error("the box holds a skill-load plugin that is not this build's")
	}
}

// A RUN WIRED TO ITS NOTES AND HANDED SKILLS NAMES BOTH PLUGINS: the notes one
// is not replaced by the other, nor the other way round.
func TestAnOpenCodeRunWiredToItsNotesNamesBothPlugins(t *testing.T) {
	t.Parallel()
	req := sandbox.RunRequest{Brief: "x", Skills: handedSkills(), SkillLoads: skillWiring(),
		Steer: sandbox.SteerWiring{Server: "crewlet", Tool: "steer_take",
			URL: "http://127.0.0.1:7/mcp/run-token-placeholder/steer"}}
	_, raw, b := writeOpenCodeConfigIn(t, req)
	p := executor.PathsFor(b)
	var cfg struct {
		Plugins []openCodePlugin `json:"plugins"`
	}
	if err := json.Unmarshal([]byte(raw), &cfg); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	var packages []string
	for _, plugin := range cfg.Plugins {
		packages = append(packages, plugin.Package)
	}
	if !slices.Equal(packages, []string{p.OpenCodeSteerPlugin(), p.OpenCodeSkillsPlugin()}) {
		t.Errorf("plugins = %v, want the notes plugin and the skill-load plugin", packages)
	}
}

// noSkills is Claude Code with no way to be handed skills of its own.
type noSkills struct{ executor.ClaudeCode }

func (noSkills) HandsSkills() bool { return false }

// A RUN IS REFUSED SKILLS IT COULD NOT BE HELD TO, before anything reaches its
// box: handed to a harness that takes none, with nowhere to report a load,
// under a name no harness reads a skill by, or two under one name. Each would
// start a run without the skills the engine holds its tools to — or name a
// directory outside the run's own.
func TestARunIsRefusedSkillsItCouldNotBeHeldTo(t *testing.T) {
	t.Parallel()
	for name, c := range map[string]struct {
		runner *executor.Runner
		req    sandbox.RunRequest
	}{
		"a harness that takes none": {executor.New(noSkills{}),
			sandbox.RunRequest{Brief: "x", Skills: handedSkills(), SkillLoads: skillWiring()}},
		"nowhere to report a load": {executor.NewClaudeCode(),
			sandbox.RunRequest{Brief: "x", Skills: handedSkills()}},
		"a name that is a path": {executor.NewOpenCode(), sandbox.RunRequest{Brief: "x",
			Skills: []sandbox.Skill{{Name: "../escape"}}, SkillLoads: skillWiring()}},
		"a name a harness reads no skill by": {executor.NewClaudeCode(), sandbox.RunRequest{Brief: "x",
			Skills: []sandbox.Skill{{Name: "GitHub"}}, SkillLoads: skillWiring()}},
		"two under one name": {executor.NewClaudeCode(), sandbox.RunRequest{Brief: "x",
			Skills: []sandbox.Skill{{Name: "github"}, {Name: "github"}}, SkillLoads: skillWiring()}},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			b := box(t, c.runner)
			if _, err := c.runner.Start(t.Context(), b, c.req); err == nil {
				t.Fatal("the run started")
			}
			if len(b.Background()) != 0 {
				t.Error("the refused run's harness was started anyway")
			}
		})
	}
}

// A RUN HANDED SKILLS DIALS ITS SKILL ROUTE AS THE BOX SEES THE ENGINE: the
// route is written in the engine host's network, and a loopback one reaches
// nothing from inside a container — so the runner moves it as it moves the
// bridge's own address.
func TestARunsSkillRouteIsDialledAsTheBoxSeesTheEngine(t *testing.T) {
	t.Parallel()
	runner := executor.NewOpenCode()
	b := containerLike{sandbox.NewFakeSandbox("box-1").CapReads(boxReadCap)}
	if err := runner.Install(t.Context(), b); err != nil {
		t.Fatalf("Install: %v", err)
	}
	req := sandbox.RunRequest{Brief: "x", Skills: handedSkills(), SkillLoads: skillWiring()}
	if _, err := runner.Start(t.Context(), b, req); err != nil {
		t.Fatalf("Start: %v", err)
	}
	var cfg struct {
		Plugins []openCodePlugin `json:"plugins"`
	}
	if err := json.Unmarshal([]byte(boxFile(t, b, paths(b).OpenCodeConfig())), &cfg); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	want := sandbox.EngineAddress(b, skillWiring().URL)
	if want == skillWiring().URL {
		t.Fatal("the fixture's box dials the engine as the engine host does; the case proves nothing")
	}
	if len(cfg.Plugins) != 1 || openCodeValue(t, b, cfg.Plugins[0].Options.URL) != want {
		t.Errorf("plugins = %+v, want the skill route as the box dials it (%s)", cfg.Plugins, want)
	}
}
