package executor

import (
	"strings"
	"testing"
)

// A HOME'S CODEX CONFIGURATION CANNOT SWITCH A SEAT SKILL OFF UNSEEN. Codex
// 0.171.0 leaves unlisted, with no word, a skill a `[[skills.config]]` entry of
// the home's config.toml sets `enabled = false` by its listed name or its
// SKILL.md's path (measured), so such a run is refused, naming the entry; the
// entry enabled, naming the skill's directory, another case or another skill,
// and a run without skills start (measured as no effect). Mutations, each
// red: drop the check; skip the path; match blind to case.
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
		"another case":         {"[[skills.config]]\nname = \"Crewlet:MCP-GitHub\"\nenabled = false\n", skills, ""},
		"another skill":        {"[[skills.config]]\nname = \"home-skill\"\nenabled = false\n", skills, ""},
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
