package sandbox_test

import (
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/sandbox"
)

// A SKILL'S NAME IS ONE A HARNESS READS A SKILL BY, AND A SINGLE DIRECTORY: the
// Agent Skills grammar — lowercase letters and digits in runs joined by single
// hyphens, at most 64 — which OpenCode names a skill by its directory under and
// Claude Code prefixes with its plugin's name. A name outside it is one a
// harness may refuse, or one that names a path outside the run's own skills.
func TestASkillsNameIsOneAHarnessReadsASkillBy(t *testing.T) {
	t.Parallel()
	for name, valid := range map[string]bool{
		"mcp-github":              true,
		"skill-platform-mentions": true,
		"a":                       true,
		"7":                       true,
		strings.Repeat("a", 64):   true,
		strings.Repeat("a", 65):   false,
		"":                        false,
		"-github":                 false,
		"github-":                 false,
		"git--hub":                false,
		"GitHub":                  false,
		"mcp:github":              false,
		"mcp_github":              false,
		"../escape":               false,
		"a/b":                     false,
		"crewlet:mcp-github":      false,
		"skill platform mentions": false,
		"gïthub":                  false,
	} {
		if got := sandbox.ValidSkillName(name); got != valid {
			t.Errorf("ValidSkillName(%q) = %v, want %v", name, got, valid)
		}
	}
}

// A SKILL IS LISTED UNDER THE ENGINE'S NAMESPACE on every harness, which is the
// name its model loads it by and the one a reported load is read back by.
func TestASkillIsListedUnderTheEnginesNamespace(t *testing.T) {
	t.Parallel()
	if got := (sandbox.Skill{Name: "mcp-github"}).Listed(); got != "crewlet:mcp-github" {
		t.Errorf("Listed() = %q, want crewlet:mcp-github", got)
	}
}
