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
	"github.com/crewlet/crewlet/internal/sandbox"
	"github.com/crewlet/crewlet/internal/shellword"
)

// codexRepoTools is every program the repository gate runs but the note
// helper, which it runs by path.
var codexRepoTools = []string{"rm", "mkdir", "chmod", "cp", "cat", "mv"}

// codexRepoBox is a box home whose working directory holds files, by path
// relative to it (a path ending in "/" a directory), and the Codex home the
// run's script makes before the gate: the home, and the working directory as
// `pwd -P` reads it.
func codexRepoBox(t *testing.T, files map[string]string) (Paths, string) {
	t.Helper()
	home := t.TempDir()
	work := filepath.Join(home, "workspace")
	for _, dir := range []string{work, Paths{Home: home}.CodexHome()} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	for rel, body := range files {
		path := filepath.Join(work, rel)
		if strings.HasSuffix(rel, "/.git/") {
			finishedClone(t, path)
			continue
		}
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

// finishedClone makes dir the git directory a finished clone leaves (measured
// on git 2.40.4 and 2.56.0): HEAD naming a branch that exists, an index, and
// no index.lock, which is what the note helper claims a checkout on.
func finishedClone(t *testing.T, dir string) {
	t.Helper()
	for rel, body := range map[string]string{"HEAD": "ref: refs/heads/main\n",
		"refs/heads/main": strings.Repeat("0", 40) + "\n", "index": "DIRC"} {
		if err := os.MkdirAll(filepath.Dir(filepath.Join(dir, rel)), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, rel), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
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
	if err := os.MkdirAll(filepath.Dir(p.CodexCheckoutLaunch()), 0o700); err != nil {
		t.Fatal(err)
	}
	launch := codexCheckoutHome(p).launchConfig(codexCheckouts(codexProjectDocMaxBytes), repos)
	if err := os.WriteFile(p.CodexCheckoutLaunch(), launch, 0o600); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(sh, "-c", "cd "+shellword.Quote(work)+" || exit 1\n"+codexCheckoutHome(p).gate()+`echo "hk=$hk"`)
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

// A CODEX RUN IS HANDED ITS CHECKOUTS' INSTRUCTIONS AND SKILLS, from the note
// helper as its launch writes it (0600): the gate runs it, which writes each
// checkout's instructions labelled with its root, the repositories the run
// pushes to first, and sets the nested hook, saying nothing. Each checkout's
// .codex/skills and .agents/skills come whole as `<repository>:<skill>`, links
// followed as Codex follows them, only the name rewritten, so a skill whose
// frontmatter names a seat's skill and a repository named for the seats'
// namespace each list under a name of their own.
//
// Mutation: test the helper with -x, drop a frontmatter key, skip a linked
// skill or file, or keep a skill's own name, and a check goes red.
func TestACodexRunIsHandedItsCheckoutsInstructionsAndSkills(t *testing.T) {
	t.Parallel()
	skill := "---\nname: build\ndescription: Builds it.\nhooks:\n  PostToolUse: []\nmodel: gpt-other\n" +
		"allowed-tools: [Bash]\n---\nRun make.\n"
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
		"alpha/shared/notes.md":                    "SHARED\n",
	})
	for link, target := range map[string]string{
		"alpha/.codex/skills/via-link":       filepath.Join(work, "alpha/.codex/skills/build"),
		"alpha/.codex/skills/build/notes.md": filepath.Join(work, "alpha/shared/notes.md"),
	} {
		if err := os.Symlink(target, filepath.Join(work, link)); err != nil {
			t.Fatal(err)
		}
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
	if got, want := skillsIn(t, p), []string{"alpha:build", "alpha:lint", "alpha:sneaky", "alpha:via-link", "crewlet-2:mine"}; !slices.Equal(got, want) {
		t.Errorf("the run's Codex skills are %q, want %q", got, want)
	}
	renamed := "---\nname: \"alpha:build\"\ndescription: Builds it.\nhooks:\n  PostToolUse: []\nmodel: gpt-other\n" +
		"allowed-tools: [Bash]\n---\nRun make.\n"
	if got := readHome(t, p.CodexSkills()+"/alpha:build/SKILL.md"); got != renamed {
		t.Errorf("the copied skill reads\n%s\nwant\n%s", got, renamed)
	}
	for path, body := range map[string]string{"alpha:build/scripts/run.sh": "make\n", "alpha:build/notes.md": "SHARED\n",
		"alpha:via-link/scripts/run.sh": "make\n"} {
		if got := readHome(t, p.CodexSkills()+"/"+path); got != body {
			t.Errorf("the skill's file %s reads %q, want %q", path, got, body)
		}
	}
	if sneaky := readHome(t, p.CodexSkills()+"/alpha:sneaky/SKILL.md"); !strings.HasPrefix(sneaky, "---\nname: \"alpha:sneaky\"\n") ||
		strings.Contains(sneaky, "crewlet:deploy") {
		t.Errorf("a repository skill naming a seat skill is listed as\n%s\nwant it under its repository's name", sneaky)
	}
}

// A REPOSITORY'S LINKED SKILLS ARE COPIED WHILE THEY STAY IN IT: a .codex or
// .agents that is a link, and a skills directory that is one, are followed
// where they lead inside the checkout, as Codex follows them; one that leads
// out of the checkout is left out, said, since a checkout's skill is the
// checkout's own content.
//
// Mutation: refuse a link on a skill's path, or follow one out of the
// checkout, and a check goes red.
func TestARepositorysLinkedSkillsAreCopiedWhileTheyStayInIt(t *testing.T) {
	t.Parallel()
	p, work := codexRepoBox(t, map[string]string{
		"beta/.git/":                           "",
		"gamma/.git/":                          "",
		"delta/.git/":                          "",
		"eps/.git/":                            "",
		"beta/kept/.codex/skills/x/SKILL.md":   "---\nname: x\ndescription: X.\n---\n",
		"gamma/kept/.agents/skills/y/SKILL.md": "---\nname: y\ndescription: Y.\n---\n",
		"delta/kept/skills/x/SKILL.md":         "---\nname: x\ndescription: X.\n---\n",
		"outside/.codex/skills/away/SKILL.md":  "---\nname: away\ndescription: Away.\n---\n",
	})
	withNoteHelper(t, p)
	for link, target := range map[string]string{
		"beta/.codex":         filepath.Join(work, "beta/kept/.codex"),
		"gamma/.agents":       "kept/.agents",
		"delta/.codex/skills": filepath.Join(work, "delta/kept/skills"),
		"eps/.codex":          filepath.Join(work, "outside/.codex"),
	} {
		if err := os.MkdirAll(filepath.Dir(filepath.Join(work, link)), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(target, filepath.Join(work, link)); err != nil {
			t.Fatal(err)
		}
	}
	stderr, _ := runCodexRepoGate(t, p, work, "")
	if got, want := skillsIn(t, p), []string{"beta:x", "delta:x", "gamma:y"}; !slices.Equal(got, want) {
		t.Errorf("the run's Codex skills are %q, want %q", got, want)
	}
	if !strings.Contains(stderr, "away is left out: it leads out of its checkout") {
		t.Errorf("the gate said %q, want the skill outside its checkout named", stderr)
	}
}

// EVERY REPOSITORY'S SKILLS ARE COPIED UNDER A NAME OF ITS OWN, whatever the
// repository is named: a name another repository, or the seats' namespace,
// already took moves on until one is free.
//
// Mutation: try one suffix only, or reserve no namespace, and a check goes
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
// no instructions (an earlier run's file, which ended before it put the
// home's none back, removed), no skills, no hook.
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
	if err := os.MkdirAll(filepath.Dir(p.CodexHomeInstructions()), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p.CodexHomeInstructions()+".none", nil, 0o600); err != nil {
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

// THE HOME'S OWN INSTRUCTIONS AND SKILLS STAY BESIDE ITS REPOSITORIES': the
// home's CODEX_HOME/AGENTS.md comes first in the one Codex reads, its
// repositories' after it, and is put back as it was once Codex stops
// ([codexRestoreHome]); a repository's skill never replaces a home's of its
// name, and the next run removes the repositories' skills an earlier one
// copied, the home's kept.
//
// Mutation: write the repositories' instructions alone, drop the restore,
// copy over a home's skill, or leave an earlier run's repository skill.
func TestTheHomesOwnInstructionsAndSkillsStayBesideItsRepositories(t *testing.T) {
	t.Parallel()
	p, work := codexRepoBox(t, map[string]string{
		"repo/.git/": "", "repo/AGENTS.md": "REPO\n",
		"repo/.codex/skills/s/SKILL.md":    "---\nname: s\ndescription: S.\n---\n",
		"repo/.codex/skills/home/SKILL.md": "---\nname: home\ndescription: H.\n---\n",
	})
	home := map[string]string{p.CodexHome() + "/AGENTS.md": "HOME\n", p.CodexSkills() + "/repo:home/SKILL.md": "HOME-SKILL\n"}
	for path, body := range home {
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	withNoteHelper(t, p)
	runCodexRepoGate(t, p, work, "")
	want := "HOME\n\n\nfor `repo` with root " + work + "/repo\n\nREPO\n"
	if got := readHome(t, p.CodexHome()+"/AGENTS.md"); got != want {
		t.Errorf("CODEX_HOME/AGENTS.md is\n%q\nwant\n%q", got, want)
	}
	if got := skillsIn(t, p); !slices.Equal(got, []string{"repo:home", "repo:s"}) {
		t.Errorf("the run's Codex skills are %q, want the home's and the repository's own", got)
	}
	sh := prereqtest.OnPath(t, "sh", "the case runs the box's script", "install a POSIX shell")
	if out, err := exec.Command(sh, "-c", codexRestoreHome(p)).CombinedOutput(); err != nil {
		t.Fatalf("the restore: %v\n%s", err, out)
	}
	if err := os.RemoveAll(filepath.Join(work, "repo")); err != nil {
		t.Fatal(err)
	}
	runCodexRepoGate(t, p, work, "")
	for path, body := range home {
		if got := readHome(t, path); got != body {
			t.Errorf("%s is %q after the next run's gate, want the home's %q", path, got, body)
		}
	}
	if got := skillsIn(t, p); !slices.Equal(got, []string{"repo:home"}) {
		t.Errorf("the next run's Codex skills are %q, want the home's alone", got)
	}
}

// THE NESTED HOOK IS TRUSTED BY THE HASH CODEX COMPUTES: the hash of a hook's
// identity is Codex 0.171.0's own, measured through its hooks/list for a
// default hook and for the run's nested hook in a box at /w/box, whose
// trusted hook ran and whose context reached the model's next request.
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
	if !strings.Contains(state, `"sha256:f0a3fd9a31a118d840dba1ab369d8718daf8e4eac3592b74931cc72002d58d1f"`) ||
		!strings.Contains(state, codexNestedHookKey) {
		t.Errorf("the nested hook's trust is %s", state)
	}
	if !strings.Contains(hook, `timeout=10,additionalContextLimit=32768`) || !strings.Contains(hook, "-adapter codex-nested") {
		t.Errorf("the nested hook is %s", hook)
	}
}

// A RUN'S PROJECT-DOC LIMIT IS THE ONE CODEX READS FOR ITS START DIRECTORY:
// the effective configuration's project_doc_max_bytes in Codex 0.171.0's
// `config/read` answer, as measured with a trusted start directory's 4096 over
// the home's 65536, the home's alone, and neither (32768); 0 is a limit.
// Codex's default stands where the answer holds none: a box with no codex, an
// answer without a value or with no whole number of bytes (a fraction, a
// string), which never costs the verdict its layers.
//
// Mutation: read a layer's value, take a missing value as 0, or decode the
// limit with the layers, and a case goes red.
func TestARunsProjectDocLimitIsTheOneCodexReadsForItsStartDirectory(t *testing.T) {
	t.Parallel()
	answer := func(config string) string {
		return `{"id":1,"result":{}}` + "\n" + `{"id":2,"result":{"config":` + config + `,"layers":[` +
			`{"name":{"type":"project","dotCodexFolder":"/w/.codex"},"config":{"project_doc_max_bytes":4096}},` +
			`{"name":{"type":"user","file":"/h/config.toml"},"config":{"project_doc_max_bytes":65536}}]}}`
	}
	for _, c := range []struct {
		out  string
		want int
	}{
		{answer(`{"project_doc_max_bytes":4096}`), 4096},
		{answer(`{"project_doc_max_bytes":65536}`), 65536},
		{answer(`{"project_doc_max_bytes":32768}`), 32768},
		{answer(`{"project_doc_max_bytes":0}`), 0},
		{answer(`{}`), codexProjectDocMaxBytes},
		{answer(`{"project_doc_max_bytes":-1}`), codexProjectDocMaxBytes},
		{answer(`{"project_doc_max_bytes":65536.5}`), codexProjectDocMaxBytes},
		{answer(`{"project_doc_max_bytes":"big"}`), codexProjectDocMaxBytes},
		{codexGuardNoCodex + "\n", codexProjectDocMaxBytes},
	} {
		if got := codexDocLimit(c.out); got != c.want {
			t.Errorf("%.120s gives the limit %d, want %d", c.out, got, c.want)
		}
	}
	odd := `{"id":2,"result":{"config":{"project_doc_max_bytes":"big"},"layers":[` +
		`{"name":{"type":"project","dotCodexFolder":"/w/.codex"},"config":{"mcp_servers":{"crewlet":{"url":"http://x"}}}}]}}`
	if err := codexGuardVerdict(odd, []string{"crewlet"}); err == nil || !strings.Contains(err.Error(), "mcp_servers.crewlet.url") {
		t.Errorf("an odd limit cost the verdict its layers: %v", err)
	}
}

// A FIRING KILLED MID-COPY LEAVES NOTHING THE NEXT LAUNCH KEEPS: whatever a
// killed firing left (a name listed with nothing built, a copy half built in
// the staging directory, one whole there but not yet moved into place), the
// next run's gate removes it, and the run's skills are its checkouts' copies
// alone, each whole, with no staging directory among them; the run's restore
// removes what a firing left in staging too.
//
// Mutation: leave the state, remove only names that are built, or leave the
// staging at the restore, and a check goes red.
func TestAFiringKilledMidCopyLeavesNothingTheNextLaunchKeeps(t *testing.T) {
	t.Parallel()
	p, work := codexRepoBox(t, map[string]string{
		"repo/.git/": "", "repo/.codex/skills/s/SKILL.md": "---\nname: s\ndescription: S.\n---\n",
	})
	withNoteHelper(t, p)
	left := map[string]string{
		codexCheckoutHome(p).staging() + "/late:half/partial":   "HALF",
		codexCheckoutHome(p).staging() + "/late:whole/SKILL.md": "---\nname: \"late:whole\"\n---\n",
		p.CodexRepoSkills(): "late:listed\nlate:half\nlate:whole\n",
	}
	for path, body := range left {
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if stderr, hooked := runCodexRepoGate(t, p, work, ""); !hooked || stderr != "" {
		t.Errorf("the gate set the hook %v and said %q", hooked, stderr)
	}
	if got := skillsIn(t, p); !slices.Equal(got, []string{"repo:s"}) {
		t.Errorf("the run's Codex skills are %q, want the checkout's copy alone", got)
	}
	if got := readHome(t, p.CodexRepoSkills()); got != "repo:s\n" {
		t.Errorf("the copies listed for the next run are %q, want this run's alone", got)
	}
	if _, err := os.Lstat(codexCheckoutHome(p).staging()); !os.IsNotExist(err) {
		t.Errorf("the gate left the staging a killed firing built in (%v)", err)
	}
	half := codexCheckoutHome(p).staging() + "/late:half/partial"
	if err := os.MkdirAll(filepath.Dir(half), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(half, []byte("HALF"), 0o600); err != nil {
		t.Fatal(err)
	}
	sh := prereqtest.OnPath(t, "sh", "the case runs the box's script", "install a POSIX shell")
	if out, err := exec.Command(sh, "-c", codexRestoreHome(p)).CombinedOutput(); err != nil {
		t.Fatalf("the restore: %v\n%s", err, out)
	}
	if _, err := os.Lstat(codexCheckoutHome(p).staging()); !os.IsNotExist(err) {
		t.Errorf("the restore left the staging a killed firing built in (%v)", err)
	}
}

// A LINKED OR EMPTY HOME FILE COMES BACK AS IT WAS. A person's
// CODEX_HOME/AGENTS.md linked from their dotfiles stays a link through the
// gate and the put-back, the dotfile holding their text again, and an empty
// config.toml held for a run comes back empty, never removed.
//
// Mutation: install or put back by moving a file over the link; hold an empty
// file as none.
func TestALinkedOrEmptyCodexHomeFileComesBackAsItWas(t *testing.T) {
	t.Parallel()
	p, work := codexRepoBox(t, map[string]string{"repo/.git/": "", "repo/AGENTS.md": "REPO\n"})
	agents, dotfile := p.CodexHome()+"/AGENTS.md", p.Home+"/dotfiles/AGENTS.md"
	for _, dir := range []string{p.CodexHome(), filepath.Dir(dotfile)} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(dotfile, []byte("HOME\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(dotfile, agents); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p.CodexLiveConfig(), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	withNoteHelper(t, p)
	sh := prereqtest.OnPath(t, "sh", "the case runs the box's script", "install a POSIX shell")
	if out, err := exec.Command(sh, "-c", holdHomeShell(p.CodexLiveConfig(), p.CodexHomeConfig())+
		" && printf 'run = 1\\n' > "+shellword.Quote(p.CodexLiveConfig())).CombinedOutput(); err != nil {
		t.Fatalf("holding the config: %v\n%s", err, out)
	}
	runCodexRepoGate(t, p, work, "")
	if fi, err := os.Lstat(agents); err != nil || fi.Mode()&os.ModeSymlink == 0 {
		t.Errorf("CODEX_HOME/AGENTS.md is no link once the gate wrote it (%v)", err)
	}
	if got := readHome(t, dotfile); !strings.HasPrefix(got, "HOME\n\n\nfor `repo`") {
		t.Errorf("the run's instructions through the link read %q, want the home's then the repository's", got)
	}
	if out, err := exec.Command(sh, "-c", codexRestoreHome(p)).CombinedOutput(); err != nil {
		t.Fatalf("the restore: %v\n%s", err, out)
	}
	if fi, err := os.Lstat(agents); err != nil || fi.Mode()&os.ModeSymlink == 0 || readHome(t, dotfile) != "HOME\n" {
		t.Errorf("after the put-back AGENTS.md is no link or the dotfile reads %q (%v)", readHome(t, dotfile), err)
	}
	if fi, err := os.Stat(p.CodexLiveConfig()); err != nil || fi.Size() != 0 {
		t.Errorf("the home's empty config.toml did not come back empty (%v)", err)
	}
}

// A NESTED FIRING'S SKIP REACHES THE RUN'S ERROR STREAM ONCE: a checkout
// cloned during a Codex run whose skill takes a name the home's skills hold
// is handed by the nested hook, whose stderr Codex keeps nowhere (measured on
// 0.172.0); the run's stop prints that line on the run's stderr once the
// harness has stopped, and a second stop prints nothing.
//
// Mutation: print nothing, print the kept lines on every stop, or leave the
// piece out of the script's stop, and a check goes red.
func TestANestedFiringsSkipReachesTheRunsErrorStreamOnce(t *testing.T) {
	t.Parallel()
	p, work := codexRepoBox(t, nil)
	withNoteHelper(t, p)
	runCodexRepoGate(t, p, work, "")
	finishedClone(t, filepath.Join(work, "late", ".git"))
	for path, body := range map[string]string{
		filepath.Join(work, "late/.codex/skills/s/SKILL.md"): "---\nname: s\n---\nREPO\n",
		p.CodexSkills() + "/late:s/SKILL.md":                 "---\nname: \"late:s\"\n---\nHOME\n",
	} {
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	sh := prereqtest.OnPath(t, "sh", "the case runs the box's script", "install a POSIX shell")
	if out, err := exec.Command(sh, "-c", "cd "+shellword.Quote(work)+" && "+codexNestedCommand(p)+" < /dev/null 2> /dev/null").
		CombinedOutput(); err != nil {
		t.Fatalf("the nested firing: %v\n%s", err, out)
	}
	piece := codexCheckoutHome(p).saySkipped()
	want := "notehelper: the skill " + filepath.Join(work, "late/.codex/skills/s") +
		" is left out: the home's skills hold late:s already\n"
	for _, says := range []string{want, ""} {
		var stdout, stderr strings.Builder
		cmd := exec.Command(sh, "-c", piece)
		cmd.Stdout, cmd.Stderr = &stdout, &stderr
		if err := cmd.Run(); err != nil || stdout.Len() > 0 || stderr.String() != says {
			t.Errorf("the piece said %q on stderr and %q on stdout (%v), want %q on stderr", stderr.String(),
				stdout.String(), err, says)
		}
	}
	stops := 0
	for line := range strings.Lines(codexScript(sandbox.RunRequest{}, Session{}, p, codexRunTimes())) {
		if strings.HasPrefix(line, "stop() {") && strings.Contains(line, piece) {
			stops++
		}
	}
	if stops != 1 {
		t.Errorf("the script's stop says what the nested hook left out %d times, want once", stops)
	}
}

// A HOME FILE THAT IS A LINK LEADING NOWHERE COMES BACK AS IT WAS: a
// CODEX_HOME/AGENTS.md or config.toml linked to a file that does not exist is
// set aside whole for the run, the run's own file standing in its place, and
// put back as the same link once Codex stops, the file it names never created;
// a run killed before its put-back (a second gate) leaves the next run's
// put-back the home's own link.
//
// Mutation: write through the link, hold it as none, or put back a link the
// run made, and a check goes red.
func TestADanglingHomeLinkComesBackAsItWas(t *testing.T) {
	t.Parallel()
	p, work := codexRepoBox(t, map[string]string{"repo/.git/": "", "repo/AGENTS.md": "REPO\n"})
	agents, config := p.CodexHome()+"/AGENTS.md", p.CodexLiveConfig()
	links := map[string]string{agents: "../dotfiles/AGENTS.md", config: p.Home + "/dotfiles/config.toml"}
	if err := os.MkdirAll(p.Home+"/dotfiles", 0o700); err != nil {
		t.Fatal(err)
	}
	for live, target := range links {
		if err := os.Symlink(target, live); err != nil {
			t.Fatal(err)
		}
	}
	withNoteHelper(t, p)
	sh := prereqtest.OnPath(t, "sh", "the case runs the box's script", "install a POSIX shell")
	if out, err := exec.Command(sh, "-c", holdHomeShell(config, p.CodexHomeConfig())+
		" && printf 'run = 1\\n' > "+shellword.Quote(config)).CombinedOutput(); err != nil {
		t.Fatalf("holding the config: %v\n%s", err, out)
	}
	for range 2 {
		runCodexRepoGate(t, p, work, "")
	}
	if fi, err := os.Lstat(agents); err != nil || !fi.Mode().IsRegular() || !strings.Contains(readHome(t, agents), "REPO") {
		t.Errorf("CODEX_HOME/AGENTS.md is no file of the run's while it runs (%v): %q", err, readHome(t, agents))
	}
	if out, err := exec.Command(sh, "-c", codexRestoreHome(p)).CombinedOutput(); err != nil {
		t.Fatalf("the restore: %v\n%s", err, out)
	}
	for live, target := range links {
		if got, err := os.Readlink(live); err != nil || got != target {
			t.Errorf("%s is %q after the put-back (%v), want the link to %q", live, got, err, target)
		}
	}
	if entries, err := os.ReadDir(p.Home + "/dotfiles"); err != nil || len(entries) > 0 {
		t.Errorf("the run created what the links name: %v (%v)", entries, err)
	}
}
