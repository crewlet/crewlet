package executor_test

import (
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/crewlet/crewlet/internal/api/mcpbridge"
	"github.com/crewlet/crewlet/internal/sandbox"
)

// A CODEX RUN LOADS ONLY WHAT ITS LAUNCH WRITES. A real box whose home holds
// everything Codex 0.171.0 reads that a launch does not write (CODEX_HOME's
// AGENTS.md, AGENTS.override.md, rules and .env; the home's .agents/skills;
// the working directory's AGENTS.md, AGENTS.override.md and .codex/; skills
// under CODEX_HOME/skills, an earlier run's included): as its run starts each
// is gone; the login, the sessions, Codex's own `.system` skills and a
// checkout's AGENTS.md are whole; nothing above the home is touched; the
// working directory is left no `.codex/.env`, Codex walking for none; and the
// skills are the run's own beside `.system`.
//
// Mutations, each red: drop an entry from codexRunHome; prune only the
// engine's skills; seed an .env for Codex.
func TestACodexRunLoadsOnlyWhatItsLaunchWrites(t *testing.T) {
	t.Parallel()
	c := homeCase{
		left: []string{".codex/AGENTS.md", ".codex/AGENTS.override.md", ".codex/rules/kept.rules", ".codex/.env",
			".agents/skills/kept/SKILL.md", "AGENTS.md", "AGENTS.override.md", "workspace/AGENTS.md",
			"workspace/AGENTS.override.md", "workspace/.codex/config.toml", ".codex/skills/kept/SKILL.md",
			".codex/skills/crewlet:old/SKILL.md"},
		kept: []string{".codex/auth.json", ".codex/sessions/2026/11/01/rollout-2026-11-01T09-00-00-kept.jsonl",
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
	if _, err := os.Stat(filepath.Join(box.Home(), "workspace", ".codex")); !os.IsNotExist(err) {
		t.Errorf("the working directory holds a .codex/ (%v), which Codex walks for no .env of", err)
	}
	entries, err := os.ReadDir(filepath.Join(box.Home(), ".codex", "skills"))
	if err != nil {
		t.Fatal(err)
	}
	var skills []string
	for _, e := range entries {
		skills = append(skills, e.Name())
	}
	if !slices.Equal(skills, []string{".system", "crewlet:deploy"}) {
		t.Errorf("the run's Codex home holds the skills %q, want the run's own beside .system", skills)
	}
}
