package executor_test

import (
	"encoding/json"
	"testing"

	"github.com/crewlet/crewlet/internal/sandbox"
	"github.com/crewlet/crewlet/internal/sandbox/executor"
)

// handedSkills are a required and an advisory skill, the first described in
// prose a plain YAML scalar would read as structure.
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

// noSkills is Claude Code with no way to be handed skills of its own.
type noSkills struct{ executor.ClaudeCode }

func (noSkills) HandsSkills() bool { return false }

// A RUN IS REFUSED SKILLS IT COULD NOT BE HELD TO, before anything reaches its
// box: to a harness that takes none, with nowhere to report a load, under a
// name no harness reads a skill by, or two under one name (refuseSkills).
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

// A RUN HANDED SKILLS DIALS ITS SKILL ROUTE AS THE BOX SEES THE ENGINE: a
// loopback route reaches nothing from inside a container, so the runner moves
// it as it moves the bridge's own address.
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
	if plugins := runPlugins(cfg.Plugins); len(plugins) != 1 || openCodeValue(t, b, plugins[0].Options.URL) != want {
		t.Errorf("plugins = %+v, want the skill route as the box dials it (%s)", cfg.Plugins, want)
	}
}
