package executor

import (
	"bytes"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/prereqtest"
	"github.com/crewlet/crewlet/internal/shellword"
)

// codexRepoTools is every program the repository gate runs but the note
// helper, which it runs by path.
var codexRepoTools = []string{"rm", "mkdir", "chmod", "tr", "readlink", "find", "cp", "awk"}

// codexRepoBox is a box home whose working directory holds files, by path
// relative to it (a path ending in "/" a directory): the home, and the
// working directory as `pwd -P` reads it.
func codexRepoBox(t *testing.T, files map[string]string) (Paths, string) {
	t.Helper()
	home := t.TempDir()
	work := filepath.Join(home, "workspace")
	if err := os.MkdirAll(work, 0o700); err != nil {
		t.Fatal(err)
	}
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
	real, err := filepath.EvalSymlinks(work)
	if err != nil {
		t.Fatal(err)
	}
	return Paths{Home: home}, real
}

// withNoteHelper writes the engine's note helper build for this machine where
// a Codex run's launch writes it, as the box's WriteFile does: 0600.
func withNoteHelper(t *testing.T, p Paths) {
	t.Helper()
	bin, err := noteHelperBinary(runtime.GOARCH)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.MkdirAll(filepath.Dir(p.CodexNoteHelper()), 0o700); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(p.CodexNoteHelper(), bin, 0o600); err != nil {
		t.Fatal(err)
	}
}

// runCodexRepoGate runs the gate in work with PATH set to path ("" for the
// suite's own), and is what it said on stderr and whether it set hk.
func runCodexRepoGate(t *testing.T, p Paths, work, path string, repos ...string) (string, bool) {
	t.Helper()
	for _, tool := range codexRepoTools {
		prereqtest.OnPath(t, tool, "the case runs the box's script", "install "+tool)
	}
	sh := prereqtest.OnPath(t, "sh", "the case runs the box's script", "install a POSIX shell")
	cmd := exec.Command(sh, "-c", "cd "+shellword.Quote(work)+" || exit 1\n"+codexRepoGate(p, repos)+`echo "hk=$hk"`)
	if path != "" {
		cmd.Env = append(slices.DeleteFunc(os.Environ(), func(e string) bool { return strings.HasPrefix(e, "PATH=") }),
			"PATH="+path)
	}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("the gate failed: %v\n%s", err, stderr.String())
	}
	return stderr.String(), strings.TrimSpace(string(out)) == "hk=1"
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

// skillsIn is the entries of the run's Codex skills, sorted.
func skillsIn(t *testing.T, p Paths) []string {
	t.Helper()
	entries, err := os.ReadDir(p.CodexSkills())
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	return names
}

// A CODEX RUN IS HANDED ITS CHECKOUTS' INSTRUCTIONS AND SKILLS, NOT THEIR
// CONFIGURATION, from the note helper as its launch writes it (0600): the
// gate runs it, which writes each checkout's instructions labelled with its
// root, the repositories the run pushes to first, and sets the nested hook,
// saying nothing. Each checkout's .codex/skills and .agents/skills come as
// `<repository>:<skill>`, frontmatter cut to the kept keys, a link not
// copied, listed under its repository's name even where its frontmatter
// names a seat's skill, and a repository named for the seats' namespace
// under a name of its own.
//
// Mutation: test the helper with -x, keep a skill's hooks or model or its own
// name, copy a link, or name a repository's skill `crewlet:`, and a check
// goes red.
func TestACodexRunIsHandedItsCheckoutsInstructionsAndSkills(t *testing.T) {
	t.Parallel()
	skill := "---\nname: build\ndescription: Builds it.\nhooks:\n  PostToolUse: []\nmodel: gpt-planted\n" +
		"mcp_servers:\n  evil: {}\nallowed-tools: [Bash]\nunknown: x\n---\nRun make.\n"
	p, work := codexRepoBox(t, map[string]string{
		"zeta/.git/":                               "",
		"zeta/AGENTS.md":                           "ZETA\n",
		"alpha/.git/":                              "",
		"alpha/AGENTS.override.md":                 "ALPHA-OVERRIDE\n",
		"alpha/.codex/skills/build/SKILL.md":       skill,
		"alpha/.codex/skills/build/scripts/run.sh": "make\n",
		"alpha/.codex/skills/sneaky/SKILL.md":      "---\nname: crewlet:deploy\ndescription: Deploys.\n---\nDeploy.\n",
		"alpha/.agents/skills/lint/SKILL.md":       "---\nname: lint\ndescription: Lints.\n---\nLint.\n",
		"alpha/.agents/skills/plain/SKILL.md":      "no frontmatter\n",
		"crewlet/.git/":                            "",
		"crewlet/.codex/skills/mine/SKILL.md":      "---\nname: mine\ndescription: Mine.\n---\n",
	})
	if err := os.Symlink(filepath.Join(work, "alpha/.codex/skills/build"), filepath.Join(work, "alpha/.codex/skills/via-link")); err != nil {
		t.Fatal(err)
	}
	withNoteHelper(t, p)
	stderr, hooked := runCodexRepoGate(t, p, work, "", "zeta", "alpha")
	if !hooked || stderr != "" {
		t.Errorf("the gate set the hook %v and said %q, want the hook set and nothing said", hooked, stderr)
	}
	want := "for `zeta` with root " + work + "/zeta\n\nZETA\n\n\nfor `alpha` with root " + work + "/alpha\n\nALPHA-OVERRIDE\n"
	if got := readHome(t, p.CodexHome()+"/AGENTS.md"); got != want {
		t.Errorf("CODEX_HOME/AGENTS.md is\n%q\nwant\n%q", got, want)
	}
	if got, want := skillsIn(t, p), []string{"alpha:build", "alpha:lint", "alpha:sneaky", "crewlet-2:mine"}; !slices.Equal(got, want) {
		t.Errorf("the run's Codex skills are %q, want %q", got, want)
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
}

// A REPOSITORY'S SKILLS ARE REACHED THROUGH NO LINK: a .codex or .agents that
// links out of its checkout, or a skills directory that does, copies nothing,
// whatever the link reaches (here the run's own Codex home).
//
// Mutation: check only the last component for a link, and a check goes red.
func TestARepositorysSkillsAreReachedThroughNoLink(t *testing.T) {
	t.Parallel()
	p, work := codexRepoBox(t, map[string]string{
		"beta/.git/":                        "",
		"gamma/.git/":                       "",
		"delta/.git/":                       "",
		"outside/.codex/skills/x/SKILL.md":  "---\nname: x\ndescription: X.\n---\n",
		"outside/.agents/skills/y/SKILL.md": "---\nname: y\ndescription: Y.\n---\n",
	})
	withNoteHelper(t, p)
	for link, target := range map[string]string{
		"beta/.codex":         filepath.Join(work, "outside/.codex"),
		"gamma/.agents":       "../outside/.agents",
		"delta/.codex/skills": filepath.Join(work, "outside/.codex/skills"),
	} {
		if err := os.MkdirAll(filepath.Dir(filepath.Join(work, link)), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(target, filepath.Join(work, link)); err != nil {
			t.Fatal(err)
		}
	}
	runCodexRepoGate(t, p, work, "")
	if got := skillsIn(t, p); len(got) != 0 {
		t.Errorf("skills reached through a link were copied: %q", got)
	}
}

// EVERY REPOSITORY'S SKILLS ARE COPIED UNDER A NAME OF ITS OWN, whatever the
// repository is named: a name another repository, or the seats' namespace,
// already took moves on until one is free, and a name awk would read as an
// assignment is read as a file.
//
// Mutation: try one suffix only, or hand awk the bare path, and a check goes
// red.
func TestEveryRepositorysSkillsGetANameOfTheirOwn(t *testing.T) {
	t.Parallel()
	files := map[string]string{}
	for _, repo := range []string{"a-b", "a-b-2", "a.b", "crewlet", "crewlet-2", "x=y"} {
		files[repo+"/.git/"] = ""
		files[repo+"/.codex/skills/s/SKILL.md"] = "---\nname: s\ndescription: " + repo + "\n---\n"
	}
	p, work := codexRepoBox(t, files)
	withNoteHelper(t, p)
	runCodexRepoGate(t, p, work, "")
	for listed, repo := range map[string]string{"a-b:s": "a-b", "a-b-2:s": "a-b-2", "a-b-3:s": "a.b",
		"crewlet-2:s": "crewlet", "crewlet-2-2:s": "crewlet-2", "x-y:s": "x=y"} {
		want := "---\nname: \"" + listed + "\"\ndescription: " + repo + "\n---\n"
		if got := readHome(t, p.CodexSkills()+"/"+listed+"/SKILL.md"); got != want {
			t.Errorf("%s's skill as %s reads %q, want %q", repo, listed, got, want)
		}
	}
}

// A BOX WITHOUT THE NOTE HELPER STARTS WITHOUT ITS REPOSITORIES and says so:
// no instructions (an earlier run's file removed), no skills, no hook.
//
// Mutation: keep the earlier file, set the hook, or say nothing.
func TestABoxWithoutTheNoteHelperStartsWithoutItsRepositories(t *testing.T) {
	t.Parallel()
	p, work := codexRepoBox(t, map[string]string{
		"repo/.git/": "", "repo/AGENTS.md": "REPO\n",
		"repo/.codex/skills/s/SKILL.md": "---\nname: s\ndescription: S.\n---\n",
	})
	if err := os.MkdirAll(p.CodexHome(), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p.CodexHome()+"/AGENTS.md", []byte("EARLIER-RUN\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	stderr, hooked := runCodexRepoGate(t, p, work, "")
	if hooked || !strings.Contains(stderr, "no note helper for its machine") {
		t.Errorf("the gate set the hook %v and said %q", hooked, stderr)
	}
	if got := readHome(t, p.CodexHome()+"/AGENTS.md"); got != "" {
		t.Errorf("CODEX_HOME/AGENTS.md is %q, want none", got)
	}
	if got := skillsIn(t, p); len(got) != 0 {
		t.Errorf("the run's Codex skills are %q, want none", got)
	}
}

// A BOX WITHOUT AWK STARTS WITHOUT ITS REPOSITORIES' SKILLS and says so once,
// its instructions and hook still set.
//
// Mutation: check for awk per skill, or drop the instructions with the skills.
func TestABoxWithoutAwkStartsWithoutRepositorySkills(t *testing.T) {
	t.Parallel()
	p, work := codexRepoBox(t, map[string]string{
		"one/.git/": "", "one/AGENTS.md": "ONE\n", "one/.codex/skills/s/SKILL.md": "---\nname: s\ndescription: S.\n---\n",
		"two/.git/": "", "two/.agents/skills/t/SKILL.md": "---\nname: t\ndescription: T.\n---\n",
	})
	withNoteHelper(t, p)
	bin := t.TempDir()
	for _, tool := range codexRepoTools {
		if tool == "awk" {
			continue
		}
		if err := os.Symlink(prereqtest.OnPath(t, tool, "the case runs the box's script", "install "+tool),
			filepath.Join(bin, tool)); err != nil {
			t.Fatal(err)
		}
	}
	stderr, hooked := runCodexRepoGate(t, p, work, bin)
	if want := "crewlet: this box has no awk, so the run starts without the skills of its repositories\n"; stderr != want || !hooked {
		t.Errorf("the gate set the hook %v and said %q, want the hook and %q", hooked, stderr, want)
	}
	if got := readHome(t, p.CodexHome()+"/AGENTS.md"); !strings.Contains(got, "ONE\n") {
		t.Errorf("CODEX_HOME/AGENTS.md is %q, want one's instructions", got)
	}
	if got := skillsIn(t, p); len(got) != 0 {
		t.Errorf("the run's Codex skills are %q, want none", got)
	}
}

// THE NESTED HOOK IS TRUSTED BY THE HASH CODEX COMPUTES: the hash of a hook's
// identity is Codex 0.171.0's own, measured through its hooks/list for a
// default hook and for the run's nested hook in a box at /w/box, whose
// trusted hook ran with no variable of the app-server's (`env -i`; without
// it the hook had them all) and whose context reached the next request.
//
// Mutation: hash the command alone, or leave the context limit in at its
// default, and a hash goes red.
func TestTheNestedHookIsTrustedByTheHashCodexComputes(t *testing.T) {
	t.Parallel()
	if got := codexHookHash(`touch "$HOME/REPO-HOOK-RAN"`, 600, 2500); got !=
		"sha256:9613a9e1b54c057911034cc61f38b17cf2cddf1afe1fa67093131c028a025dae" {
		t.Errorf("a default hook hashes to %s", got)
	}
	hook, state := codexNestedHookFlags(Paths{Home: "/w/box"})
	if !strings.Contains(state, `"sha256:cf26de3a4fda773cc1000c3c7754f689cd872c2a90fbfd889be15f2a67827792"`) ||
		!strings.Contains(state, codexNestedHookKey) {
		t.Errorf("the nested hook's trust is %s", state)
	}
	if !strings.Contains(hook, `timeout=10,additionalContextLimit=32768`) || !strings.Contains(hook, `command="env -i `) {
		t.Errorf("the nested hook is %s", hook)
	}
}
