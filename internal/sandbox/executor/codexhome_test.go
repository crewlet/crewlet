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

// WHAT A CHECKOUT BELOW THE START DIRECTORY BRINGS A CODEX RUN, by kind, in a
// real box whose working directory holds its own AGENTS.md and a checkout at
// ./repo, as code_checkout places one, with every layer a repository brings:
//   - instructions: the checkout's are handed, under its root
//     (CODEX_HOME/AGENTS.md); the working directory's own are not, since
//     Codex reads them itself;
//   - skills: the checkout's .codex/skills and .agents/skills, whole, as
//     `repo:<skill>`;
//   - config, MCP servers, rules and hooks of the checkout's .codex: nowhere
//     in the Codex home nor on the app-server's command line, and no trust
//     for the checkout, since Codex loads them only for the directory a
//     thread starts in; the one hook declared is the run's own, which hands
//     a later checkout its instructions, its helper executable.
//
// Mutation: drop the gate, hand the working directory's own file, filter a
// skill, copy the checkout's config, or test the helper with -x.
func TestWhatACheckoutBelowTheStartDirectoryBringsACodexRun(t *testing.T) {
	t.Parallel()
	runner, box, record := codexBox(t, "0.171.0")
	p := executor.PathsFor(box)
	for rel, body := range map[string]string{
		"workspace/AGENTS.md":                         "WORKDIR-INSTRUCTIONS\n",
		"workspace/repo/.git/HEAD":                    "ref: refs/heads/main\n",
		"workspace/repo/AGENTS.md":                    "REPO-INSTRUCTIONS\n",
		"workspace/repo/.codex/config.toml":           "developer_instructions = \"REPO-CONFIG\"\n[mcp_servers.repo-mcp]\ncommand = \"repo-mcp-server\"\n",
		"workspace/repo/.codex/hooks.json":            `{"hooks":{"UserPromptSubmit":[{"hooks":[{"type":"command","command":"repo-hook"}]}]}}`,
		"workspace/repo/.codex/rules/repo.rules":      `prefix_rule(pattern=["echo"], decision="forbidden", justification="repo-rule")` + "\n",
		"workspace/repo/.codex/skills/check/SKILL.md": "---\nname: check\ndescription: Checks.\nmodel: gpt-other\n---\nCheck.\n",
		"workspace/repo/.agents/skills/lint/SKILL.md": "---\nname: lint\ndescription: Lints.\n---\nLint.\n",
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

	// Instructions.
	if got := boxFile(t, box, p.CodexHome()+"/AGENTS.md"); got != "for `repo` with root "+work+"/repo\n\nREPO-INSTRUCTIONS\n" {
		t.Errorf("instructions: CODEX_HOME/AGENTS.md is %q, want the checkout's alone", got)
	}
	// Skills.
	for listed, want := range map[string]string{
		"repo:check": "---\nname: \"repo:check\"\ndescription: Checks.\nmodel: gpt-other\n---\nCheck.\n",
		"repo:lint":  "---\nname: \"repo:lint\"\ndescription: Lints.\n---\nLint.\n",
	} {
		if got := boxFile(t, box, p.CodexSkills()+"/"+listed+"/SKILL.md"); got != want {
			t.Errorf("skills: %s reads %q, want %q", listed, got, want)
		}
	}
	// Config, MCP servers, rules and hooks.
	server := strings.Join(recorded(t, record, "server-argv"), "\n")
	home := []string{server}
	if err := filepath.WalkDir(p.CodexHome(), func(path string, d os.DirEntry, err error) error {
		if err == nil && d.Type().IsRegular() && !strings.HasPrefix(path, p.CodexSkills()+"/") {
			home = append(home, path+": "+boxFile(t, box, path))
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
	for _, where := range home {
		for kind, mark := range map[string]string{"config": "REPO-CONFIG", "MCP servers": "repo-mcp",
			"rules": "repo-rule", "hooks": "repo-hook", "trust": "trust_level"} {
			if strings.Contains(where, mark) {
				t.Errorf("%s: the checkout's .codex reached the run: %.300s", kind, where)
			}
		}
	}
	if !strings.Contains(server, "hooks.PostToolUse=") || !strings.Contains(server, "-adapter codex-nested") ||
		strings.Count(server, "trusted_hash") != 1 || !strings.Contains(server, "/<session-flags>/config.toml:post_tool_use:0:0") {
		t.Errorf("hooks: the app-server declares and trusts other than the run's own hook:\n%s", server)
	}
	if info, err := os.Stat(p.CodexNoteHelper()); err != nil || info.Mode().Perm()&0o100 == 0 {
		t.Errorf("hooks: the note helper the run's hook runs is not executable in the box (%v, %v)", info, err)
	}
	if errs := boxFile(t, box, p.Err()); strings.Contains(errs, "no note helper") {
		t.Errorf("hooks: the run said its box has no note helper:\n%s", errs)
	}
}
