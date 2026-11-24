package executor_test

import (
	"encoding/json"
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
// whose working directory holds its own AGENTS.md and a checkout at ./repo
// with every layer a repository brings. Its instructions are handed under its
// root, the working directory's not; its skills come whole as `repo:<skill>`
// for the turn alone, none left once the run ends; its .codex config, MCP
// servers, rules and hooks reach neither the Codex home nor the app-server's
// command line. The run's own trusted hook hands a checkout cloned during the
// turn its instructions and skills, copied and listed by path, once.
//
// Mutation: drop the gate, hand the working directory's own file, filter a
// skill, copy the checkout's config, skip a firing's copy or listing, or
// leave the copies past the run.
func TestWhatACheckoutBelowTheStartDirectoryBringsACodexRun(t *testing.T) {
	t.Parallel()
	runner, box, record := codexBox(t, "0.171.0")
	p := executor.PathsFor(box)
	for rel, body := range map[string]string{
		"workspace/AGENTS.md":                         "WORKDIR-INSTRUCTIONS\n",
		"workspace/repo/.git/HEAD":                    "ref: refs/heads/main\n",
		"workspace/repo/.git/refs/heads/main":         strings.Repeat("0", 40) + "\n",
		"workspace/repo/.git/index":                   "DIRC",
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
	late := filepath.Join(t.TempDir(), "late")
	for rel, body := range map[string]string{".git/HEAD": "ref: refs/heads/main\n", ".git/index": "DIRC",
		".git/refs/heads/main": strings.Repeat("0", 40) + "\n", "AGENTS.md": "LATE-INSTRUCTIONS\n",
		".agents/skills/fix/SKILL.md": "---\nname: fix\ndescription: Fixes.\n---\nFix.\n",
		".codex/config.toml":          "developer_instructions = \"LATE-CONFIG\"\n"} {
		if err := os.MkdirAll(filepath.Dir(filepath.Join(late, rel)), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(late, rel), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	for name, body := range map[string]string{"clone-src": late, "clone-dst": filepath.Join(box.Home(), "workspace", "late")} {
		if err := os.WriteFile(filepath.Join(record, name), []byte(body), 0o600); err != nil {
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
		if got := recordedFile(t, record, "skills/"+listed+"/SKILL.md"); got != want {
			t.Errorf("skills: %s reads %q during the turn, want %q", listed, got, want)
		}
	}
	// A checkout cloned during the turn.
	wantLate := "# AGENTS.md instructions for " + work + "/late\n\n<INSTRUCTIONS>\nLATE-INSTRUCTIONS\n\n</INSTRUCTIONS>\n\n" +
		"# Skills of " + work + "/late\n\nEach is a set of local instructions to follow, stored in its `SKILL.md` file; " +
		"to use one, read that file.\n- late:fix: Fixes. (file: " + p.CodexSkills() + "/late:fix/SKILL.md)"
	var nested struct {
		HookSpecificOutput struct {
			AdditionalContext string `json:"additionalContext"`
		} `json:"hookSpecificOutput"`
	}
	if err := json.Unmarshal([]byte(recordedFile(t, record, "nested-1")), &nested); err != nil ||
		nested.HookSpecificOutput.AdditionalContext != wantLate {
		t.Errorf("clone: the first firing after it answered %q (%v), want\n%q", recordedFile(t, record, "nested-1"), err, wantLate)
	}
	if got := recordedFile(t, record, "nested-2") + recordedFile(t, record, "nested-1.err") + recordedFile(t, record, "nested-2.err"); got != "" {
		t.Errorf("clone: the later firing, or a firing's errors, said %q, want nothing", got)
	}
	if got := recordedFile(t, record, "skills/late:fix/SKILL.md"); got != "---\nname: \"late:fix\"\ndescription: Fixes.\n---\nFix.\n" {
		t.Errorf("clone: its skill reads %q during the turn", got)
	}
	for _, copied := range []string{"repo:check", "repo:lint", "late:fix"} {
		if _, err := os.Lstat(filepath.Join(p.CodexSkills(), copied)); !os.IsNotExist(err) {
			t.Errorf("skills: %s is left in the home after the run (%v)", copied, err)
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
		for kind, mark := range map[string]string{"config": "REPO-CONFIG", "clone's config": "LATE-CONFIG", "MCP servers": "repo-mcp",
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

// recordedFile is a file the stand-in recorded, whole, "" where there is none.
func recordedFile(t *testing.T, record, name string) string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(record, name)) //nolint:gosec // the case's own file
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	return string(raw)
}

// A CODEX RUN HANDS A CHECKOUT'S INSTRUCTIONS WITHIN THE PROJECT-DOC LIMIT
// CODEX READS FOR ITS START DIRECTORY: where the home's or a trusted start
// directory's configuration sets project_doc_max_bytes (here as Codex
// 0.171.0's `config/read` reported a start directory's 4096 over the home's
// 65536, scaled down), the checkout's file is cut to it, as a session would
// cut it, and not to Codex's default.
//
// Mutation: hand the launch Codex's default whatever the answer says, and
// this goes red.
func TestACodexRunHandsInstructionsWithinTheLimitCodexReads(t *testing.T) {
	t.Parallel()
	runner, box, record := codexBox(t, "0.171.0")
	plantFile(t, filepath.Join(record, "config-read"), `{"id":2,"result":{"config":{"project_doc_max_bytes":8},"layers":[`+
		`{"name":{"type":"project","dotCodexFolder":"/w/.codex"},"config":{"project_doc_max_bytes":8}},`+
		`{"name":{"type":"user","file":"/h/config.toml"},"config":{"project_doc_max_bytes":16}}]}}`)
	for rel, body := range map[string]string{"workspace/repo/.git/HEAD": "ref: refs/heads/main\n",
		"workspace/repo/.git/index": "DIRC", "workspace/repo/.git/refs/heads/main": strings.Repeat("0", 40) + "\n",
		"workspace/repo/AGENTS.md": "0123456789ABCDEF\n"} {
		plantFile(t, filepath.Join(box.Home(), rel), body)
	}
	if _, res := runToEnd(t, runner, box, sandbox.RunRequest{Brief: "go"}); !res.Success {
		t.Fatalf("the run failed: %s", res.Error)
	}
	work, err := filepath.EvalSymlinks(filepath.Join(box.Home(), "workspace"))
	if err != nil {
		t.Fatal(err)
	}
	if got, want := recordedFile(t, record, "AGENTS.md"), "for `repo` with root "+work+"/repo\n\n01234567"; got != want {
		t.Errorf("Codex started on a CODEX_HOME/AGENTS.md of %q, want the checkout's first 8 bytes: %q", got, want)
	}
}

// ONLY CREWLET'S SERVERS REFUSE A RUN WHOSE CODEX CANNOT BE ASKED: where the
// launch's ask of the box's Codex fails (a box whose mkfifo fails), a run with
// none of Crewlet's servers starts, hands each checkout's instructions within
// Codex's default 32 KiB and says so on its error stream; a run with them is
// refused, as its bridge cannot be held to its own configuration.
//
// Mutation: refuse every run on a failed ask, or start one with Crewlet's
// servers, or say nothing, and a check goes red.
func TestOnlyCrewletsServersRefuseARunWhoseCodexCannotBeAsked(t *testing.T) {
	t.Parallel()
	runner, box, record := codexBox(t, "0.171.0")
	p := executor.PathsFor(box)
	writeBoxStandIn(t, box, p.BinDir()+"/mkfifo", "#!/bin/sh\nexit 1\n")
	plant(t, box, map[string]string{"workspace/repo/.git/HEAD": "ref: refs/heads/main\n", "workspace/repo/.git/index": "DIRC",
		"workspace/repo/.git/refs/heads/main": strings.Repeat("0", 40) + "\n",
		"workspace/repo/AGENTS.md":            strings.Repeat("a", 32<<10) + "PAST-THE-DEFAULT\n"})
	if _, res := runToEnd(t, runner, box, sandbox.RunRequest{Brief: "go"}); !res.Success {
		t.Fatalf("a run with none of Crewlet's servers failed: %s", res.Error)
	}
	if got := recordedFile(t, record, "AGENTS.md"); strings.Contains(got, "PAST-THE-DEFAULT") || !strings.HasSuffix(got, "aaaa") {
		t.Errorf("Codex started on %d bytes of instructions ending %q, want the checkout's cut at 32 KiB", len(got), got[max(0, len(got)-20):])
	}
	if errs := boxFile(t, box, p.Err()); !strings.Contains(errs, "could not be asked which project_doc_max_bytes") {
		t.Errorf("the run's error stream did not say its Codex could not be asked:\n%s", errs)
	}
	guarded, other, _ := codexBox(t, "0.171.0")
	writeBoxStandIn(t, other, executor.PathsFor(other).BinDir()+"/mkfifo", "#!/bin/sh\nexit 1\n")
	if _, err := guarded.Start(t.Context(), other, sandbox.RunRequest{Brief: "go", MCPServers: loadsBridge}); err == nil ||
		!strings.Contains(err.Error(), "asking the box's Codex which configuration it loads") {
		t.Errorf("a run with Crewlet's servers whose Codex could not be asked was not refused for it: %v", err)
	}
}
