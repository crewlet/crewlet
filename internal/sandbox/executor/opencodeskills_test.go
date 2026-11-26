package executor_test

import (
	"encoding/json"
	"os"
	"slices"
	"testing"

	"github.com/crewlet/crewlet/internal/sandbox"
	"github.com/crewlet/crewlet/internal/sandbox/executor"
)

// AN OPENCODE RUN TAKES ITS SKILLS AS A DIRECTORY ITS CONFIGURATION NAMES: one
// per skill, named as the skill is listed (OpenCode's id for it), and in
// `plugins` the load-reporting plugin, its one option the skill route, read
// from a file as the notes plugin's is.
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
	if !slices.Equal(cfg.Skills, []string{p.OpenCodeSkills(), p.OpenCodeCheckoutSkills()}) {
		t.Errorf("skills = %v, want the run's own directory, then its checkouts'", cfg.Skills)
	}
	plugins := runPlugins(cfg.Plugins)
	if len(plugins) != 1 || plugins[0].Package != p.OpenCodeSkillsPlugin() {
		t.Fatalf("plugins = %+v, want the skill-load plugin alone", cfg.Plugins)
	}
	if got := openCodeValue(t, b, plugins[0].Options.URL); got != skillWiring().URL {
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
	for _, plugin := range runPlugins(cfg.Plugins) {
		packages = append(packages, plugin.Package)
	}
	if !slices.Equal(packages, []string{p.OpenCodeSteerPlugin(), p.OpenCodeSkillsPlugin()}) {
		t.Errorf("plugins = %v, want the notes plugin and the skill-load plugin", packages)
	}
}
