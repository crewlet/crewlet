package executor_test

import (
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/crewlet/crewlet/internal/api/mcpbridge"
	"github.com/crewlet/crewlet/internal/sandbox"
)

// A CODEX RUN CLEARS ONLY AN EARLIER RUN'S SKILLS. A real box whose home and
// working directory hold everything Codex 0.171.0 reads beside a launch's own
// (CODEX_HOME's AGENTS.md, AGENTS.override.md, rules and .env; the home's
// .agents/skills; the working directory's AGENTS.md, AGENTS.override.md and
// .codex/; CODEX_HOME's skills): after its run each is in place, the working
// directory's AGENTS.md among them, and CODEX_HOME's skills are the home's
// beside the run's, an earlier run's `crewlet:` skill gone.
//
// Mutations, each red: clear every skill but `.system`; leave an earlier
// run's skill; remove the working directory's AGENTS.md.
func TestACodexRunClearsOnlyAnEarlierRunsSkills(t *testing.T) {
	t.Parallel()
	c := homeCase{
		left: []string{".codex/skills/crewlet:old/SKILL.md"},
		kept: []string{".codex/AGENTS.md", ".codex/AGENTS.override.md", ".codex/rules/kept.rules", ".codex/.env",
			".agents/skills/kept/SKILL.md", "AGENTS.md", "AGENTS.override.md", "workspace/AGENTS.md",
			"workspace/AGENTS.override.md", "workspace/.codex/config.toml", ".codex/skills/kept/SKILL.md",
			".codex/auth.json", ".codex/sessions/2026/11/01/rollout-2026-11-01T09-00-00-kept.jsonl",
			".codex/skills/.system/kept/SKILL.md", "workspace/checkout/AGENTS.md"},
	}
	runner, box, _ := codexBox(t, "0.171.0")
	above := fillHome(t, box, c)
	_, res := runToEnd(t, runner, box, sandbox.RunRequest{Brief: "go",
		Skills:     []sandbox.Skill{{Name: "deploy", Description: "How we deploy.", Body: "Run make deploy."}},
		SkillLoads: sandbox.SkillWiring{Server: "crewlet", Tool: mcpbridge.SkillTool, URL: codexBridgeURL + "/skills"}})
	if !res.Success {
		t.Fatalf("the run failed: %s", res.Error)
	}
	checkHome(t, box, c, above)
	entries, err := os.ReadDir(filepath.Join(box.Home(), ".codex", "skills"))
	if err != nil {
		t.Fatal(err)
	}
	var skills []string
	for _, e := range entries {
		skills = append(skills, e.Name())
	}
	if !slices.Equal(skills, []string{".system", "crewlet:deploy", "kept"}) {
		t.Errorf("the run's Codex home holds the skills %q, want the home's beside the run's own", skills)
	}
}
