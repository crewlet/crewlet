package executor_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/sandbox"
	"github.com/crewlet/crewlet/internal/sandbox/executor"
)

// A HOME SKILL MAY NOT HIDE A GROK RUN'S SEAT SKILL UNSEEN. Grok Build 1.0.92
// lists `crewlet:mcp-github` as `crewlet-mcp-github`, and drops it with no
// word where a home skill's name and directory both rewrite to that, its own
// directory winning the name (measured), so the run is refused before Grok
// starts, naming the home skill and the seat skill; a home skill whose
// directory only rewrites to it is refused too, its winner unmeasured. A home
// skill only named alike (its directory rewriting elsewhere, which then lists
// it: measured), another name, an earlier run's own copy and a run without
// skills start; the frontmatter is read as YAML. Mutations, each red: drop
// the check; skip the directory's condition; read the name line by hand.
func TestAGrokHomeSkillMayNotHideASeatSkillUnseen(t *testing.T) {
	t.Parallel()
	skill := func(name string) string { return "---\nname: " + name + "\ndescription: d\n---\nbody\n" }
	const hidden = ".grok/skills/crewlet-mcp-github/SKILL.md"
	for name, tc := range map[string]struct {
		plant   map[string]string
		skills  bool
		refused []string
	}{
		"a home skill of the seat skill's rewritten name and directory": {skills: true,
			plant:   map[string]string{hidden: skill(`"crewlet-mcp-github"`)},
			refused: []string{"/.grok/skills/crewlet-mcp-github/SKILL.md, listed as crewlet-mcp-github, hides the seat skill crewlet:mcp-github"}},
		"a name YAML reads past its comment": {skills: true,
			plant:   map[string]string{hidden: "---\nname: 'crewlet-mcp-github' # the person's own\ndescription: d\n---\nbody\n"},
			refused: []string{"/.grok/skills/crewlet-mcp-github/SKILL.md, listed as crewlet-mcp-github, hides the seat skill crewlet:mcp-github"}},
		"frontmatter YAML cannot read": {skills: true,
			plant: map[string]string{".grok/skills/other/SKILL.md": "---\nname: [unclosed\n---\nbody\n"}},
		"a home skill whose directory rewrites alike": {skills: true,
			plant:   map[string]string{".grok/skills/Crewlet_MCP_GitHub/SKILL.md": skill("Crewlet MCP GitHub")},
			refused: []string{"/.grok/skills/Crewlet_MCP_GitHub/SKILL.md, listed as crewlet-mcp-github"}},
		"a home skill named alike in a directory of its own": {skills: true,
			plant: map[string]string{".grok/skills/crewlet-mcp-github-x/SKILL.md": skill("crewlet-mcp-github")}},
		"a home skill of another name": {skills: true,
			plant: map[string]string{".grok/skills/github/SKILL.md": skill("crewlet-github")}},
		"an earlier run's own copy": {skills: true,
			plant: map[string]string{".grok/skills/crewlet:mcp-github/SKILL.md": skill("crewlet:mcp-github")}},
		"a run without skills": {plant: map[string]string{hidden: skill("crewlet-mcp-github")}},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			runner, box := standInBox(t, executor.NewGrok(), map[string]string{"grok": grokInspectStandIn})
			plant(t, box, map[string]string{"inspect.json": grokInspect("", [2]string{"NAME", ""})})
			plant(t, box, tc.plant)
			req := sandbox.RunRequest{Brief: "go", MCPServers: loadsBridge}
			if tc.skills {
				req.Skills, req.SkillLoads = handedSkills(), skillWiring()
			}
			if len(tc.refused) == 0 {
				if _, res := runToEnd(t, runner, box, req); !res.Success {
					t.Fatalf("a run no home skill hid a seat skill of failed: %s", res.Error)
				}
				return
			}
			_, err := runner.Start(t.Context(), box, req)
			if err == nil {
				t.Fatal("a run whose seat skill a home skill hides started")
			}
			for _, want := range tc.refused {
				if want = box.Home() + want; !strings.Contains(err.Error(), want) {
					t.Errorf("the refusal does not say %q:\n%v", want, err)
				}
			}
			if _, err := os.Stat(filepath.Join(box.Home(), "observed.argv")); !os.IsNotExist(err) {
				t.Errorf("grok started for a refused run (%v)", err)
			}
		})
	}
}

// A HOME'S GROK SETTINGS CANNOT SWITCH A SEAT SKILL OFF UNSEEN. Grok Build
// 1.0.92 hides a skill whose listed name `[skills] disabled` names (the raw
// `crewlet:<name>` matching nothing) or whose directory `[skills] ignore`
// names, `~/` for the home, with no word (measured), so a run handed that
// skill is refused before it starts, naming the file and the entry; another
// skill's entries and a run without skills start. Mutations, each red: drop
// the check; match the raw name; leave `~/` unexpanded.
func TestAGrokHomesSettingsCannotSwitchASeatSkillOffUnseen(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		config  string
		skills  bool
		refused []string
	}{
		"nothing switched off": {skills: true, config: "[skills]\ndisabled = [\"crewlet:mcp-github\", \"other\"]\n" +
			"ignore = [\"~/.grok/skills/other\"]\n"},
		"disabled by its listed name": {skills: true, config: "[skills]\ndisabled = [\"crewlet-mcp-github\"]\n",
			refused: []string{`its [skills] disabled names "crewlet-mcp-github", the seat skill crewlet:mcp-github`}},
		"ignored by its directory": {skills: true, config: "[skills]\nignore = [\"~/.grok/skills/crewlet:getting-unstuck\"]\n",
			refused: []string{`its [skills] ignore names "~/.grok/skills/crewlet:getting-unstuck"`}},
		"a run without skills": {config: "[skills]\ndisabled = [\"crewlet-mcp-github\"]\n"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			runner, box := standInBox(t, executor.NewGrok(), map[string]string{"grok": grokInspectStandIn})
			plant(t, box, map[string]string{"inspect.json": grokInspect("", [2]string{"NAME", ""}),
				".grok/config.toml": tc.config})
			req := sandbox.RunRequest{Brief: "go", MCPServers: loadsBridge}
			if tc.skills {
				req.Skills, req.SkillLoads = handedSkills(), skillWiring()
			}
			if len(tc.refused) == 0 {
				if _, res := runToEnd(t, runner, box, req); !res.Success {
					t.Fatalf("a run nothing switched a seat skill off of failed: %s", res.Error)
				}
				return
			}
			_, err := runner.Start(t.Context(), box, req)
			if err == nil {
				t.Fatal("a run whose seat skill the home switched off started")
			}
			for _, want := range append(tc.refused, "/.grok/config.toml") {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("the refusal does not say %q:\n%v", want, err)
				}
			}
		})
	}
}
