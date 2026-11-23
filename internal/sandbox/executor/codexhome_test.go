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

// WHAT A CHECKOUT BELOW THE START DIRECTORY BRINGS A CODEX RUN: a real box
// whose working directory holds its own AGENTS.md and a checkout at ./repo,
// as code_checkout places one, with every layer a repository brings. Its
// instructions are handed under its root, the working directory's not (Codex
// reads them itself); its skills come whole as `repo:<skill>`; its .codex
// config, MCP servers, rules and hooks reach neither the Codex home nor the
// app-server's command line, with no trust for it, since Codex loads them only
// for the directory a thread starts in. The one hook trusted is the run's own.
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
	if got, _ := os.ReadFile(filepath.Join(record, "AGENTS.md")); string(got) != "for `repo` with root "+work+"/repo\n\nREPO-INSTRUCTIONS\n" {
		t.Errorf("instructions: Codex started on a CODEX_HOME/AGENTS.md of %q, want the checkout's alone", got)
	}
	if _, err := os.Stat(p.CodexHome() + "/AGENTS.md"); !os.IsNotExist(err) {
		t.Errorf("instructions: the run left a CODEX_HOME/AGENTS.md in a home that had none (%v)", err)
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
