package executor_test

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/api/mcpbridge"
	"github.com/crewlet/crewlet/internal/sandbox"
	"github.com/crewlet/crewlet/internal/sandbox/executor"
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

// A CODEX RUN IS HANDED ITS CHECKOUT'S INSTRUCTIONS AND SKILLS, AND THE HOOK
// FOR A LATER ONE. A real box whose working directory holds a checkout with
// an AGENTS.md, a skill and a planted .codex/config.toml: as its run ends,
// CODEX_HOME/AGENTS.md holds the checkout's file under its root, its skill
// is the run's as `repo:check`, its config reached neither the Codex home nor
// the app-server's command line, and that command line declares the nested
// hook and its trust, the note helper it runs executable beside it, the run
// saying nothing of a missing helper.
//
// Mutation: drop the gate from the script, test the helper with -x, or drop
// the hook's flags from the app-server, and a check goes red.
func TestACodexRunIsHandedItsCheckoutAndTheHookForALaterOne(t *testing.T) {
	t.Parallel()
	runner, box, record := codexBox(t, "0.171.0")
	p := executor.PathsFor(box)
	for rel, body := range map[string]string{
		"workspace/repo/.git/HEAD":                    "ref: refs/heads/main\n",
		"workspace/repo/AGENTS.md":                    "REPO-INSTRUCTIONS\n",
		"workspace/repo/.codex/config.toml":           "developer_instructions = \"PLANT-PROJECT-CONFIG\"\n",
		"workspace/repo/.codex/skills/check/SKILL.md": "---\nname: check\ndescription: Checks.\nmodel: gpt-planted\n---\nCheck.\n",
	} {
		path := filepath.Join(box.Home(), rel)
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if _, res := runToEnd(t, runner, box, sandbox.RunRequest{Brief: "go"}); !res.Success {
		t.Fatalf("the run failed: %s", res.Error)
	}
	work, err := filepath.EvalSymlinks(filepath.Join(box.Home(), "workspace"))
	if err != nil {
		t.Fatal(err)
	}
	if got := boxFile(t, box, p.CodexHome()+"/AGENTS.md"); got != "for `repo` with root "+work+"/repo\n\nREPO-INSTRUCTIONS\n" {
		t.Errorf("CODEX_HOME/AGENTS.md is %q", got)
	}
	skill := boxFile(t, box, p.CodexSkills()+"/repo:check/SKILL.md")
	if !strings.Contains(skill, `name: "repo:check"`) || strings.Contains(skill, "gpt-planted") {
		t.Errorf("the repository's skill reads\n%s", skill)
	}
	server := strings.Join(recorded(t, record, "server-argv"), "\n")
	if !strings.Contains(server, "hooks.PostToolUse=") || !strings.Contains(server, "-adapter codex-nested") ||
		!strings.Contains(server, "/<session-flags>/config.toml:post_tool_use:0:0") {
		t.Errorf("the app-server declares no nested hook:\n%s", server)
	}
	for _, where := range []string{server, boxFile(t, box, p.CodexLiveConfig())} {
		if strings.Contains(where, "PLANT-PROJECT-CONFIG") {
			t.Error("the repository's .codex/config.toml reached the run's configuration")
		}
	}
	if info, err := os.Stat(p.CodexNoteHelper()); err != nil || info.Mode().Perm()&0o100 == 0 {
		t.Errorf("the note helper the hook runs is not executable in the box (%v, %v)", info, err)
	}
	if errs := boxFile(t, box, p.Err()); strings.Contains(errs, "no note helper") {
		t.Errorf("the run said its box has no note helper:\n%s", errs)
	}
}
