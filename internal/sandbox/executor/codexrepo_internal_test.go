package executor

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/shellword"
)

// codexRepoBox is a box home whose working directory holds files, by path
// relative to it (a path ending in "/" a directory), and the gate's run there:
// the home, and the working directory as `pwd -P` reads it.
func codexRepoBox(t *testing.T, files map[string]string, repos ...string) (Paths, string) {
	t.Helper()
	home := t.TempDir()
	work := filepath.Join(home, "workspace")
	for rel, body := range files {
		path := filepath.Join(work, rel)
		if strings.HasSuffix(rel, "/") {
			if err := os.MkdirAll(path, 0o700); err != nil {
				t.Fatal(err)
			}
			continue
		}
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.MkdirAll(work, 0o700); err != nil {
		t.Fatal(err)
	}
	p := Paths{Home: home}
	runSh(t, "cd "+shellword.Quote(work)+" || exit 1\n"+codexRepoGate(p, repos))
	real, err := filepath.EvalSymlinks(work)
	if err != nil {
		t.Fatal(err)
	}
	return p, real
}

// readHome is a file under the box home, "" where there is none.
func readHome(t *testing.T, path string) string {
	t.Helper()
	raw, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return ""
	}
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

// A CODEX RUN IS HANDED ITS CHECKOUTS' INSTRUCTIONS, NOT THEIR CONFIGURATION:
// each repository checked out directly under the working directory hands its
// root AGENTS.override.md, else AGENTS.md (what Codex reads started at that
// root), labelled with its root, the repositories it pushes to first in its
// order, the rest by name; a directory with no .git, a link to a checkout and
// the working directory's own AGENTS.md hand nothing. Its .codex/skills and .agents/skills
// come as `<repository>:<skill>`, frontmatter cut to the kept keys, links not
// copied, listed under its repository's name even where its frontmatter names
// a seat's skill; its .codex config, hooks and rules reach nothing.
//
// Mutation: read AGENTS.md before the override, take a directory without .git,
// keep a skill's hooks or model or its own name, copy a link, or name a
// repository's skill `crewlet:`, and a check goes red.
func TestACodexRunIsHandedItsCheckoutsInstructionsNotTheirConfig(t *testing.T) {
	skill := "---\nname: build\ndescription: Builds it.\nhooks:\n  PostToolUse: []\nmodel: gpt-planted\n" +
		"mcp_servers:\n  evil: {}\nallowed-tools: [Bash]\nunknown: x\n---\nRun make.\n"
	p, work := codexRepoBox(t, map[string]string{
		"AGENTS.md":                                "WORKDIR-OWN\n",
		"zeta/.git/":                               "",
		"zeta/AGENTS.md":                           "ZETA\n",
		"alpha/.git/":                              "",
		"alpha/AGENTS.md":                          "ALPHA\n",
		"alpha/AGENTS.override.md":                 "ALPHA-OVERRIDE\n",
		"alpha/.codex/config.toml":                 "developer_instructions = \"PLANT-PROJECT-CONFIG\"\n",
		"alpha/.codex/hooks.json":                  `{"hooks":{"PostToolUse":[]}}`,
		"alpha/.codex/rules/x.rules":               "prefix_rule(pattern=[\"echo\"], decision=\"forbidden\")\n",
		"alpha/.codex/skills/build/SKILL.md":       skill,
		"alpha/.codex/skills/build/scripts/run.sh": "make\n",
		"alpha/.codex/skills/sneaky/SKILL.md":      "---\nname: crewlet:deploy\ndescription: Deploys.\n---\nDeploy.\n",
		"alpha/.agents/skills/lint/SKILL.md":       "---\nname: lint\ndescription: Lints.\n---\nLint.\n",
		"alpha/.agents/skills/plain/SKILL.md":      "no frontmatter\n",
		"plain/AGENTS.md":                          "NOT-A-CHECKOUT\n",
		"crewlet/.git/":                            "",
		"crewlet/.codex/skills/mine/SKILL.md":      "---\nname: mine\ndescription: Mine.\n---\n",
	}, "zeta", "alpha")
	if err := os.Symlink(filepath.Join(work, "zeta"), filepath.Join(work, "linked")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(work, "alpha/.codex/skills/build"), filepath.Join(work, "alpha/.codex/skills/via-link")); err != nil {
		t.Fatal(err)
	}
	runSh(t, "cd "+shellword.Quote(work)+" || exit 1\n"+codexRepoGate(p, []string{"zeta", "alpha"}))

	want := "for `zeta` with root " + work + "/zeta\n\nZETA\n\n\nfor `alpha` with root " + work + "/alpha\n\nALPHA-OVERRIDE\n"
	if got := readHome(t, p.CodexHome()+"/AGENTS.md"); got != want {
		t.Errorf("CODEX_HOME/AGENTS.md is\n%q\nwant\n%q", got, want)
	}
	if got := readHome(t, p.CodexRepo()+"/handed"); got != "zeta\nalpha\ncrewlet\n" {
		t.Errorf("the checkouts handed are %q, want zeta, alpha and crewlet", got)
	}
	build := readHome(t, p.CodexSkills()+"/alpha:build/SKILL.md")
	if build != "---\nname: \"alpha:build\"\ndescription: Builds it.\nallowed-tools: [Bash]\n---\nRun make.\n" {
		t.Errorf("the copied skill reads\n%s", build)
	}
	if readHome(t, p.CodexSkills()+"/alpha:build/scripts/run.sh") != "make\n" {
		t.Error("the skill's own files were not copied")
	}
	if sneaky := readHome(t, p.CodexSkills()+"/alpha:sneaky/SKILL.md"); !strings.HasPrefix(sneaky, "---\nname: \"alpha:sneaky\"\n") ||
		strings.Contains(sneaky, "crewlet:deploy") {
		t.Errorf("a repository skill naming a seat skill is listed as\n%s\nwant it under its repository's name", sneaky)
	}
	if readHome(t, p.CodexSkills()+"/alpha:lint/SKILL.md") == "" {
		t.Error("a skill under .agents/skills was not copied")
	}
	for _, gone := range []string{"alpha:via-link", "alpha:plain", "crewlet:mine", "crewlet:deploy", "linked:"} {
		if _, err := os.Lstat(p.CodexSkills() + "/" + gone); err == nil {
			t.Errorf("the run's Codex home holds %s", gone)
		}
	}
	if readHome(t, p.CodexSkills()+"/crewlet-3:mine/SKILL.md") == "" {
		t.Error("the repository named crewlet's skill is not under a name of its own")
	}
	for _, path := range []string{p.CodexHome() + "/config.toml", p.CodexHome() + "/hooks.json", p.CodexHome() + "/rules"} {
		if _, err := os.Lstat(path); err == nil {
			t.Errorf("a repository's configuration reached %s", path)
		}
	}
}

// A CODEX RUN WITH NO CHECKOUT IS HANDED NO INSTRUCTIONS: CODEX_HOME/AGENTS.md
// is replaced by none, never kept from an earlier run, and the whole budget
// is left for a checkout the run clones.
//
// Mutation: keep the earlier file, or leave less than the budget.
func TestACodexRunWithNoCheckoutIsHandedNoInstructions(t *testing.T) {
	home := t.TempDir()
	p := Paths{Home: home}
	if err := os.MkdirAll(p.CodexHome(), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p.CodexHome()+"/AGENTS.md", []byte("EARLIER-RUN\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	work := filepath.Join(home, "workspace")
	if err := os.MkdirAll(work, 0o700); err != nil {
		t.Fatal(err)
	}
	runSh(t, "cd "+shellword.Quote(work)+" || exit 1\n"+codexRepoGate(p, nil))
	if got := readHome(t, p.CodexHome()+"/AGENTS.md"); got != "" {
		t.Errorf("CODEX_HOME/AGENTS.md is %q, want none", got)
	}
	if got := readHome(t, p.CodexRepo()+"/budget"); got != "32768\n" {
		t.Errorf("the budget left is %q, want the whole 32768", got)
	}
}

// A CODEX RUN'S INSTRUCTIONS ARE BOUND BY CODEX'S OWN BUDGET: the files
// handed hold at most 32 KiB together, a file past what is left cut to it, as
// Codex cuts one, and a checkout found once nothing is left hands nothing
// (both still handed, so the nested hook hands neither again).
//
// Mutation: hand a file whole past the budget, or let the next checkout use
// what is spent.
func TestACodexRunsInstructionsAreBoundByCodexsBudget(t *testing.T) {
	big := strings.Repeat("x", codexRepoBudget+4000)
	p, work := codexRepoBox(t, map[string]string{
		"a/.git/": "", "a/AGENTS.md": big,
		"b/.git/": "", "b/AGENTS.md": "B\n",
	})
	want := "for `a` with root " + work + "/a\n\n" + big[:codexRepoBudget]
	if got := readHome(t, p.CodexHome()+"/AGENTS.md"); got != want {
		t.Errorf("CODEX_HOME/AGENTS.md holds %d bytes, want the label and the first %d of a's", len(got), codexRepoBudget)
	}
	if got := readHome(t, p.CodexRepo()+"/budget"); got != "0\n" {
		t.Errorf("the budget left is %q, want 0", got)
	}
	if got := readHome(t, p.CodexRepo()+"/handed"); got != "a\nb\n" {
		t.Errorf("the checkouts handed are %q, want both", got)
	}
}

// THE NESTED HOOK IS TRUSTED BY THE HASH CODEX COMPUTES: the hash of a hook's
// identity is Codex 0.171.0's own, measured through its hooks/list for a
// default hook and for the run's nested hook in a box at /w/box, whose
// trusted hook ran and whose context reached the model's next request.
//
// Mutation: hash the command alone, leave the context limit in at its
// default, or escape HTML, and a hash goes red.
func TestTheNestedHookIsTrustedByTheHashCodexComputes(t *testing.T) {
	if got := codexHookHash(`touch "$HOME/REPO-HOOK-RAN"`, 600, 2500); got !=
		"sha256:9613a9e1b54c057911034cc61f38b17cf2cddf1afe1fa67093131c028a025dae" {
		t.Errorf("a default hook hashes to %s", got)
	}
	hook, state := codexNestedHookFlags(Paths{Home: "/w/box"})
	if !strings.Contains(state, `"sha256:f0a3fd9a31a118d840dba1ab369d8718daf8e4eac3592b74931cc72002d58d1f"`) ||
		!strings.Contains(state, codexNestedHookKey) {
		t.Errorf("the nested hook's trust is %s", state)
	}
	if !strings.Contains(hook, `timeout=10,additionalContextLimit=32768`) || !strings.Contains(hook, "codex-nested") {
		t.Errorf("the nested hook is %s", hook)
	}
}
