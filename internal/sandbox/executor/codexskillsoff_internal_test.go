package executor

import (
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/sandbox"
)

// A HOME'S CODEX CONFIGURATION CANNOT SWITCH A SEAT SKILL OFF UNSEEN. Codex
// 0.171.0 leaves unlisted, with no word, a skill a `[[skills.config]]` entry of
// the home's config.toml sets `enabled = false` by its listed name or its
// SKILL.md's path (measured), so such a run is refused, naming the entry; the
// entry enabled, naming the skill's directory, another case or another skill,
// and a run without skills start (measured as no effect). Mutations, each
// red: drop the check; skip the path; match blind to case; let the first
// entry decide.
func TestACodexHomeCannotSwitchASeatSkillOffUnseen(t *testing.T) {
	t.Parallel()
	paths := Paths{Home: "/home/box"}
	skills := []codexSkill{{name: "crewlet:mcp-github"}}
	file := paths.CodexSkills() + "/crewlet:mcp-github/SKILL.md"
	for name, tc := range map[string]struct {
		home    string
		skills  []codexSkill
		refused string
	}{
		"by its name": {"[[skills.config]]\nname = \"crewlet:mcp-github\"\nenabled = false\n", skills, `name "crewlet:mcp-github"`},
		"by its file": {"[[skills.config]]\npath = \"" + file + "\"\nenabled = false\n", skills, `path "` + file + `"`},
		"enabled":     {"[[skills.config]]\nname = \"crewlet:mcp-github\"\nenabled = true\n", skills, ""},
		"by its directory": {"[[skills.config]]\npath = \"" + strings.TrimSuffix(file, "/SKILL.md") + "\"\nenabled = false\n",
			skills, ""},
		"another case":  {"[[skills.config]]\nname = \"Crewlet:MCP-GitHub\"\nenabled = false\n", skills, ""},
		"another skill": {"[[skills.config]]\nname = \"home-skill\"\nenabled = false\n", skills, ""},
		"a later entry lists it again": {"[[skills.config]]\nname = \"crewlet:mcp-github\"\nenabled = false\n\n" +
			"[[skills.config]]\npath = \"" + file + "\"\nenabled = true\n", skills, ""},
		"a later entry hides it": {"[[skills.config]]\nname = \"crewlet:mcp-github\"\nenabled = true\n\n" +
			"[[skills.config]]\nname = \"crewlet:mcp-github\"\nenabled = false\n", skills, `name "crewlet:mcp-github"`},
		"a run without skills": {"[[skills.config]]\nname = \"crewlet:mcp-github\"\nenabled = false\n", nil, ""},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			err := codexSkillsOff(tc.home, tc.skills, paths)
			if (err != nil) != (tc.refused != "") {
				t.Fatalf("refused %v, want %q", err, tc.refused)
			}
			if err != nil && !strings.Contains(err.Error(), tc.refused) {
				t.Errorf("the refusal does not say %q: %v", tc.refused, err)
			}
		})
	}
}

// THE MACHINE'S OR A MANAGED LAYER CANNOT SWITCH A SEAT SKILL OFF UNSEEN
// EITHER. Codex's own `config/read` answer lists every layer it loads, the
// highest first; their `[[skills.config]]` entries, lowest first and a
// project's left out, settle a skill as the home's do, so /etc's entry
// switching one off refuses the run, naming that file, unless the home's later
// entry lists it again. Mutations, each red: skip the layers; count a
// project's; read the layers highest first.
func TestCodexsMachineLayersCannotSwitchASeatSkillOffUnseen(t *testing.T) {
	t.Parallel()
	paths := Paths{Home: "/home/box"}
	skills := []sandbox.Skill{{Name: "mcp-github"}}
	layer := func(kind, file, entries string) string {
		return `{"name":{"type":"` + kind + `","file":"` + file + `"},"config":{"skills":{"config":[` + entries + `]}}}`
	}
	off := `{"name":"crewlet:mcp-github","enabled":false}`
	on := `{"name":"crewlet:mcp-github","enabled":true}`
	answer := func(layers ...string) string {
		return `{"id":2,"result":{"layers":[` + strings.Join(layers, ",") + `],"config":{}}}`
	}
	for name, tc := range map[string]struct {
		out     string
		refused string
	}{
		"the machine's layer": {answer(layer("user", "/g/config.toml", ""), layer("system", "/etc/codex/config.toml", off)),
			`/etc/codex/config.toml holds a [[skills.config]] entry (name "crewlet:mcp-github"`},
		"the home lists it again": {answer(layer("user", "/g/config.toml", on), layer("system", "/etc/codex/config.toml", off)), ""},
		"the machine's on under the home's off": {answer(layer("user", "/g/config.toml", off), layer("system", "/etc/codex/config.toml", on)),
			"/home/box/.codex/config.toml holds"},
		"a project's entry": {answer(layer("project", "/w/.codex/config.toml", off), layer("user", "/g/config.toml", "")), ""},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			err := codexSkillsVerdict(tc.out, skills, paths)
			if (err != nil) != (tc.refused != "") {
				t.Fatalf("refused %v, want %q", err, tc.refused)
			}
			if err != nil && !strings.Contains(err.Error(), tc.refused) {
				t.Errorf("the refusal does not say %q: %v", tc.refused, err)
			}
		})
	}
}
