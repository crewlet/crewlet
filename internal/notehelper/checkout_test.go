package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"
	"unicode/utf8"
)

// repoRun is a Codex run's working directory, its checkout state, the
// instruction file its launch writes and its skills directory, as
// checkout-launch and codex-nested find them.
type repoRun struct {
	work, state, agents, skills string
}

// codexSpec is the spec a Codex run's launch gives the core
// (internal/sandbox/executor's codexCheckouts), its skills in skills.
func codexSpec(skills string) checkoutSpec {
	return checkoutSpec{Instructions: names([]string{"AGENTS.override.md", "AGENTS.md"}), Limit: limited(32 << 10),
		SkillDirs: []string{".codex/skills", ".agents/skills"}, Skills: skills,
		Copied: filepath.Join(filepath.Dir(skills), "copied"), Reserved: []string{"crewlet"}}
}

// newRepoRun is a repoRun whose working directory holds files, by path
// relative to it (a path ending in "/" a directory), and nothing else yet.
func newRepoRun(t *testing.T, files map[string]string) repoRun {
	t.Helper()
	root := t.TempDir()
	r := repoRun{work: filepath.Join(root, "workspace"), state: filepath.Join(root, "codex-repo"),
		agents: filepath.Join(root, "codex", "AGENTS.md"), skills: filepath.Join(root, "codex", "skills")}
	for _, dir := range []string{r.work, filepath.Dir(r.agents)} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	r.add(t, files)
	return r
}

// add writes files under the working directory, as newRepoRun does: a path
// ending in "/.git/" a finished clone's git directory ([finishedGit]).
func (r repoRun) add(t *testing.T, files map[string]string) {
	t.Helper()
	for rel, body := range files {
		path := filepath.Join(r.work, rel)
		if strings.HasSuffix(rel, "/.git/") {
			finishedGit(t, path)
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
}

// launch runs checkout-launch in the working directory with first, under
// Codex's spec with limit (nil for none).
func (r repoRun) launch(t *testing.T, limit *int, first ...string) {
	t.Helper()
	spec := codexSpec(r.skills)
	spec.Limit = limit
	raw, _ := json.Marshal(launchConfig{State: r.state, Agents: r.agents, First: first, checkoutSpec: spec})
	config := filepath.Join(t.TempDir(), "launch.json")
	if err := os.WriteFile(config, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	var errs bytes.Buffer
	if err := checkoutLaunch(config, r.work, &errs); err != nil || errs.Len() > 0 {
		t.Fatalf("the launch failed: %v\n%s", err, errs.String())
	}
}

// fire runs one firing of the nested hook off the state the launch wrote, and
// is the context it hands, "" for none.
func (r repoRun) fire(t *testing.T) string {
	t.Helper()
	var out, errs bytes.Buffer
	code := runCodexNested(context.Background(), flags{config: filepath.Join(r.state, "nested.json")},
		strings.NewReader(`{"tool_name":"Bash","tool_input":{"command":"git clone x"}}`), &out, &errs, os.Getenv)
	if code != exitDone || errs.Len() > 0 {
		t.Fatalf("the firing exited %d: %s", code, errs.String())
	}
	if out.Len() == 0 {
		return ""
	}
	var answer struct {
		HookSpecificOutput struct {
			HookEventName     string `json:"hookEventName"`
			AdditionalContext string `json:"additionalContext"`
		} `json:"hookSpecificOutput"`
	}
	if err := json.Unmarshal(out.Bytes(), &answer); err != nil || answer.HookSpecificOutput.HookEventName != "PostToolUse" {
		t.Fatalf("the firing answered %s (%v)", out.String(), err)
	}
	return answer.HookSpecificOutput.AdditionalContext
}

// names is a slot of names for each of slots.
func names(slots ...[]string) []instructionSlot {
	out := make([]instructionSlot, len(slots))
	for i, slot := range slots {
		out[i] = instructionSlot{Names: slot}
	}
	return out
}

// limited is a limit of n bytes.
func limited(n int) *int { return &n }

// finishedGit makes dir the git directory a finished clone leaves (measured
// on git 2.40.4 and 2.56.0): HEAD naming a branch that exists, an index, and
// no index.lock.
func finishedGit(t *testing.T, dir string) {
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

// read is a file the run holds, "" where there is none.
func read(t *testing.T, path string) string {
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

// workdir is the working directory as the launch resolves it.
func (r repoRun) workdir(t *testing.T) string {
	t.Helper()
	real, err := filepath.EvalSymlinks(r.work)
	if err != nil {
		t.Fatal(err)
	}
	return real
}

// THE LAUNCH HANDS EACH CHECKOUT'S ROOT INSTRUCTIONS, THE REPOSITORIES THE RUN
// PUSHES TO FIRST: each repository checked out directly under the working
// directory hands its AGENTS.override.md, else AGENTS.md, labelled with its
// root, those first names in their order, the rest by name; a directory with
// no .git, a checkout under a dot-name and the working directory's own
// AGENTS.md (which Codex reads itself) hand nothing and are not handed. An
// earlier run's AGENTS.md is replaced.
//
// Mutation: read AGENTS.md before the override, ignore first, take a
// directory without .git or a dot-name, or keep the earlier file.
func TestTheLaunchHandsEachCheckoutsInstructionsTheReposItPushesToFirst(t *testing.T) {
	t.Parallel()
	r := newRepoRun(t, map[string]string{
		"AGENTS.md":                "WORKDIR-OWN\n",
		"zeta/.git/":               "",
		"zeta/AGENTS.md":           "ZETA\n",
		"alpha/.git/":              "",
		"alpha/AGENTS.md":          "ALPHA\n",
		"alpha/AGENTS.override.md": "ALPHA-OVERRIDE\n",
		"beta/.git/":               "",
		"beta/AGENTS.md":           "BETA\n",
		"bare/.git/":               "",
		"plain/AGENTS.md":          "NOT-A-CHECKOUT\n",
		".hidden/.git/":            "",
		".hidden/AGENTS.md":        "HIDDEN\n",
	})
	if err := os.WriteFile(r.agents, []byte("EARLIER-RUN\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	r.launch(t, limited(32<<10), "zeta", "missing", ".hidden", "alpha")
	work := r.workdir(t)
	want := "for `zeta` with root " + work + "/zeta\n\nZETA\n\n\nfor `alpha` with root " + work +
		"/alpha\n\nALPHA-OVERRIDE\n\n\nfor `beta` with root " + work + "/beta\n\nBETA\n"
	if got := read(t, r.agents); got != want {
		t.Errorf("CODEX_HOME/AGENTS.md is\n%q\nwant\n%q", got, want)
	}
	if got := read(t, filepath.Join(r.state, "handed")); got != "zeta\nalpha\nbare\nbeta\n" {
		t.Errorf("the checkouts handed are %q, want zeta, alpha, bare and beta in that order", got)
	}
	if got := read(t, filepath.Join(r.state, "nested.json")); !strings.Contains(got, `"workdir":"`+work+`"`) {
		t.Errorf("the nested hook's config is %s, want the working directory %s", got, work)
	}
}

// A RUN WITH NO CHECKOUT IS HANDED NO INSTRUCTIONS: CODEX_HOME/AGENTS.md is
// replaced by none, never kept from an earlier run.
//
// Mutation: keep the earlier file.
func TestARunWithNoCheckoutIsHandedNoInstructions(t *testing.T) {
	t.Parallel()
	r := newRepoRun(t, nil)
	if err := os.WriteFile(r.agents, []byte("EARLIER-RUN\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	r.launch(t, limited(32<<10))
	if _, err := os.Lstat(r.agents); !os.IsNotExist(err) {
		t.Errorf("CODEX_HOME/AGENTS.md is still there (%v), want none", err)
	}
}

// EACH CHECKOUT'S INSTRUCTIONS ARE BOUND BY THE HARNESS'S OWN LIMIT, as a
// session started in it reads them: a file past it is cut to it, at the
// launch and in a firing alike, whatever the run handed other checkouts.
//
// Mutation: hand a file whole past the limit, or charge one checkout's bytes
// to the next.
func TestEachCheckoutsInstructionsAreBoundByTheHarnessLimit(t *testing.T) {
	t.Parallel()
	r := newRepoRun(t, map[string]string{
		"a/.git/": "", "a/AGENTS.md": strings.Repeat("x", 6),
		"b/.git/": "", "b/AGENTS.md": strings.Repeat("w", 25),
	})
	r.launch(t, limited(10))
	if got := read(t, r.agents); !strings.Contains(got, "\n\nxxxxxx\n\nfor `b`") || !strings.HasSuffix(got, "\n\nwwwwwwwwww") {
		t.Errorf("the launch handed %q, want a's 6 bytes whole and b's first 10", got)
	}
	r.add(t, map[string]string{"c/.git/": "", "c/AGENTS.md": strings.Repeat("y", 25)})
	if got := r.fire(t); !strings.Contains(got, "\n<INSTRUCTIONS>\nyyyyyyyyyy\n</INSTRUCTIONS>") {
		t.Errorf("the clone's instructions are %q, want its first 10 bytes", got)
	}
}

// A HARNESS WITH NO LIMIT IS HANDED EACH FILE WHOLE, at the launch and in a
// firing, however long.
//
// Mutation: read a file to a fixed bound, or to none at all, and a check goes
// red.
func TestAHarnessWithNoLimitIsHandedEachFileWhole(t *testing.T) {
	t.Parallel()
	long := strings.Repeat("z", 1<<20) + "END\n"
	r := newRepoRun(t, map[string]string{"a/.git/": "", "a/AGENTS.md": long})
	r.launch(t, nil)
	if got := read(t, r.agents); !strings.HasSuffix(got, "\n\n"+long) {
		t.Errorf("the launch handed %d bytes, want a's %d whole", len(got), len(long))
	}
	r.add(t, map[string]string{"b/.git/": "", "b/AGENTS.md": long})
	if got := r.fire(t); !strings.Contains(got, "<INSTRUCTIONS>\n"+long+"\n</INSTRUCTIONS>") {
		t.Errorf("the firing handed %d bytes, want b's %d whole", len(got), len(long))
	}
}

// INSTRUCTIONS ARE HANDED AS TEXT CODEX KEEPS WHOLE: a cut falls on a rune
// boundary, so a character the limit would split is left out and charged
// for nothing, and a file that is not UTF-8 is handed with each invalid byte
// replaced, never raw bytes Codex may drop the whole file over.
//
// Mutation: cut mid-rune, or hand the bytes as they are.
func TestInstructionsAreHandedAsValidText(t *testing.T) {
	t.Parallel()
	r := newRepoRun(t, map[string]string{"a/.git/": "", "a/AGENTS.md": "abcé"})
	r.launch(t, limited(4))
	if got := read(t, r.agents); !strings.HasSuffix(got, "\n\nabc") {
		t.Errorf("the multibyte cut hands %q, want abc and no half of é", got)
	}
	r = newRepoRun(t, map[string]string{"b/.git/": "", "b/AGENTS.md": "caf\xe9\n"})
	r.launch(t, limited(32<<10))
	got := read(t, r.agents)
	if !utf8.ValidString(got) || !strings.HasSuffix(got, "\n\ncaf�\n") {
		t.Errorf("a Latin-1 file is handed as %q, want valid UTF-8 with the byte replaced", got)
	}
}

// A FILE OF WHITESPACE ALONE HANDS NO SECTION, as Codex reads one: the
// override that holds it is still the checkout's file, so its AGENTS.md is
// not read in its place.
//
// Mutation: hand an empty section, or fall back to AGENTS.md.
func TestAFileOfWhitespaceHandsNothing(t *testing.T) {
	t.Parallel()
	r := newRepoRun(t, map[string]string{
		"a/.git/": "", "a/AGENTS.override.md": "  \n\t\n", "a/AGENTS.md": "A\n",
	})
	r.launch(t, limited(100))
	if got := read(t, r.agents); got != "" {
		t.Errorf("CODEX_HOME/AGENTS.md is %q, want none", got)
	}
}

// A CHECKOUT CLONED DURING THE RUN IS HANDED ITS ROOT INSTRUCTIONS ONCE: the
// first firing after it appears hands its AGENTS.override.md, else AGENTS.md,
// in Codex's own "# AGENTS.md instructions for <dir>" shape; a later firing
// hands it nothing; a checkout the launch handed, a directory with no .git
// and a checkout under a dot-name are handed nothing.
//
// Mutation: hand every firing, read AGENTS.md before the override, or take a
// directory with no .git, a dot-name or one the launch handed.
func TestACheckoutClonedDuringTheRunIsHandedItsRootInstructionsOnce(t *testing.T) {
	t.Parallel()
	r := newRepoRun(t, map[string]string{"launched/.git/": "", "launched/AGENTS.md": "LAUNCHED\n"})
	r.launch(t, limited(32<<10))
	r.add(t, map[string]string{"notes/AGENTS.md": "NOT-A-CHECKOUT\n", ".cache/.git/": "", ".cache/AGENTS.md": "HIDDEN\n"})
	if got := r.fire(t); got != "" {
		t.Errorf("a firing with no new checkout handed %q", got)
	}
	r.add(t, map[string]string{"cloned/.git/": "", "cloned/AGENTS.md": "CLONED\n", "cloned/AGENTS.override.md": "CLONED-OVERRIDE\n"})
	want := "# AGENTS.md instructions for " + filepath.Join(r.workdir(t), "cloned") + "\n\n<INSTRUCTIONS>\nCLONED-OVERRIDE\n\n</INSTRUCTIONS>"
	if got := r.fire(t); got != want {
		t.Errorf("the firing after the clone handed\n%q\nwant\n%q", got, want)
	}
	if got := r.fire(t); got != "" {
		t.Errorf("a later firing handed %q again", got)
	}
	if got := read(t, filepath.Join(r.state, "handed")); got != "launched\ncloned\n" {
		t.Errorf("the checkouts handed are %q, want launched and cloned", got)
	}
}

// LINKS ARE FOLLOWED AS CODEX FOLLOWS THEM: an AGENTS.md that links out of
// its checkout hands the file it reaches, and a link to a checkout in the
// working directory is a checkout.
//
// Mutation: refuse a linked file or a linked checkout, and a check goes red.
func TestLinksAreFollowedAsCodexFollowsThem(t *testing.T) {
	t.Parallel()
	r := newRepoRun(t, nil)
	r.launch(t, limited(32<<10))
	outside := filepath.Join(t.TempDir(), "elsewhere.md")
	if err := os.WriteFile(outside, []byte("OUTSIDE\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	r.add(t, map[string]string{"linked-file/.git/": ""})
	if err := os.Symlink(outside, filepath.Join(r.work, "linked-file", "AGENTS.md")); err != nil {
		t.Fatal(err)
	}
	other := t.TempDir()
	finishedGit(t, filepath.Join(other, ".git"))
	if err := os.WriteFile(filepath.Join(other, "AGENTS.md"), []byte("LINKED-DIR\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(other, filepath.Join(r.work, "linked-dir")); err != nil {
		t.Fatal(err)
	}
	work := r.workdir(t)
	want := "# AGENTS.md instructions for " + work + "/linked-dir\n\n<INSTRUCTIONS>\nLINKED-DIR\n\n</INSTRUCTIONS>\n\n" +
		"# AGENTS.md instructions for " + work + "/linked-file\n\n<INSTRUCTIONS>\nOUTSIDE\n\n</INSTRUCTIONS>"
	if got := r.fire(t); got != want {
		t.Errorf("the firing handed\n%q\nwant\n%q", got, want)
	}
}

// A CHECKOUT CLONED DURING THE RUN IS HANDED ITS SKILLS ONCE, under a tag of
// its own: the firing after it appears copies its skills as the launch copies
// a checkout's (.codex/skills winning a name over .agents/skills, a SKILL.md
// with no frontmatter left out), lists each copy for the next launch to
// remove, and lists them in its context as Codex lists a skill, by the copy's
// path; a clone whose tag a launched checkout took moves on to `-2`, and a
// later firing hands nothing.
//
// Mutation: skip the copy in a firing, list the original's path, start the
// tags afresh per firing, or list a skill twice, and a check goes red.
func TestACheckoutClonedDuringTheRunIsHandedItsSkillsOnce(t *testing.T) {
	t.Parallel()
	r := newRepoRun(t, map[string]string{"launched/.git/": "", "launched/.codex/skills/s/SKILL.md": "---\nname: s\n---\n"})
	r.launch(t, limited(32<<10))
	r.add(t, map[string]string{
		"Launched/.git/":                         "",
		"Launched/AGENTS.md":                     "CLONED\n",
		"Launched/.codex/skills/check/SKILL.md":  "---\nname: check\ndescription: \"Checks.\"\n---\nCheck.\n",
		"Launched/.agents/skills/check/SKILL.md": "---\nname: check\ndescription: Not this one.\n---\n",
		"Launched/.agents/skills/lint/SKILL.md":  "---\nname: lint\ndescription: >\n  Lints\n  the tree.\nmodel: other\n---\nLint.\n",
		"Launched/.agents/skills/raw/SKILL.md":   "no frontmatter\n",
	})
	root := filepath.Join(r.workdir(t), "Launched")
	want := "# AGENTS.md instructions for " + root + "\n\n<INSTRUCTIONS>\nCLONED\n\n</INSTRUCTIONS>\n\n# Skills of " + root +
		"\n\nEach is a set of local instructions to follow, stored in its `SKILL.md` file; to use one, read that file.\n" +
		"- launched-2:check: Checks. (file: " + r.skills + "/launched-2:check/SKILL.md)\n" +
		"- launched-2:lint: Lints the tree. (file: " + r.skills + "/launched-2:lint/SKILL.md)"
	if got := r.fire(t); got != want {
		t.Errorf("the firing after the clone handed\n%q\nwant\n%q", got, want)
	}
	if got := read(t, r.skills+"/launched-2:check/SKILL.md"); got != "---\nname: \"launched-2:check\"\ndescription: \"Checks.\"\n---\nCheck.\n" {
		t.Errorf("the clone's check skill reads %q, want .codex/skills' own, renamed", got)
	}
	if got := read(t, filepath.Join(filepath.Dir(r.skills), "copied")); got != "launched:s\nlaunched-2:check\nlaunched-2:lint\n" {
		t.Errorf("the skills listed for removal are %q, want the launch's and the clone's", got)
	}
	if got := r.fire(t); got != "" {
		t.Errorf("a later firing handed %q again", got)
	}
}

// EACH INSTRUCTION SLOT HANDS ITS FIRST FILE, every slot in order within one
// limit: a later name in a slot is read only where no earlier one is a
// regular file, a slot whose file is whitespace alone hands nothing, and the
// slots' texts are apart by a blank line.
//
// Mutation: read every name of a slot, stop at the first slot, or charge a
// whitespace file, and a check goes red.
func TestEachInstructionSlotHandsItsFirstFile(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	for name, body := range map[string]string{"ONE.md": "ONE\n", "TWO.md": " \n", "TWO-ALT.md": "NOT-READ\n",
		"THREE-ALT.md": "THREE\n"} {
		if err := os.WriteFile(filepath.Join(root, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Mkdir(filepath.Join(root, "THREE.md"), 0o700); err != nil {
		t.Fatal(err)
	}
	text, n := rootInstructions(root, 100, names([]string{"ONE.md"}, []string{"TWO.md", "TWO-ALT.md"},
		[]string{"THREE.md", "THREE-ALT.md"}, []string{"NONE.md"}), &skips{stderr: io.Discard})
	if text != "ONE\n\n\nTHREE\n" || n != 10 {
		t.Errorf("the slots hand %q for %d bytes, want ONE and THREE for 10", text, n)
	}
	if text, n = rootInstructions(root, 7, names([]string{"ONE.md"}, []string{"THREE-ALT.md"}), &skips{stderr: io.Discard}); text != "ONE\n\n\nTHR" || n != 7 {
		t.Errorf("the slots within 7 bytes hand %q for %d, want THREE cut to what ONE left", text, n)
	}
}

// A COPIED SKILL IS RENAMED AND NOTHING ELSE: its frontmatter's name, and
// every line that continues it, gives way to the run's, a CRLF file keeps its
// line ends, every other line is as it was, and its description is read on
// one line (plain, quoted or a block); a file that does not open with `---`,
// and an empty one, is no skill.
//
// Mutation: keep the name's continuation, strip the CRs, or read a block's
// first line alone, and a case goes red.
func TestACopiedSkillIsRenamedAndNothingElse(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		in, out, description string
		ok                   bool
	}{
		{"---\nname: >\n  old\n  name\ndescription: 'Does it.'\n# note\n---\nBody\n",
			"---\nname: \"r:s\"\ndescription: 'Does it.'\n# note\n---\nBody\n", "Does it.", true},
		{"---\r\nname: old\r\ndescription: Plain words\r\n---\r\nBody\r\n",
			"---\r\nname: \"r:s\"\ndescription: Plain words\r\n---\r\nBody\r\n", "Plain words", true},
		{"---\ndescription: |\n  one\n\n  two\nkey: v\n---", "---\nname: \"r:s\"\ndescription: |\n  one\n\n  two\nkey: v\n---\n", "one two", true},
		{"---\n---\n", "---\nname: \"r:s\"\n---\n", "", true},
		{"no frontmatter\n---\n", "", "", false},
		{"", "", "", false},
	} {
		out, description, ok := renameSkill([]byte(c.in), "r:s")
		if string(out) != c.out || description != c.description || ok != c.ok {
			t.Errorf("%q renames to %q, %q, %v; want %q, %q, %v", c.in, out, description, ok, c.out, c.description, c.ok)
		}
	}
	long := strings.Repeat("é", maxSkillDescription+5)
	if _, description, _ := renameSkill([]byte("---\ndescription: "+long+"\n---\n"), "r:s"); description != long[:2*maxSkillDescription] {
		t.Errorf("a long description is listed with %d characters, want %d", utf8.RuneCountInString(description), maxSkillDescription)
	}
}

// launchSaying runs checkout-launch under Codex's spec, as launch does, and
// is what it said on stderr.
func (r repoRun) launchSaying(t *testing.T) string {
	t.Helper()
	raw, _ := json.Marshal(launchConfig{State: r.state, Agents: r.agents, checkoutSpec: codexSpec(r.skills)})
	config := filepath.Join(t.TempDir(), "launch.json")
	if err := os.WriteFile(config, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	var errs bytes.Buffer
	if err := checkoutLaunch(config, r.work, &errs); err != nil {
		t.Fatalf("the launch failed: %v\n%s", err, errs.String())
	}
	return errs.String()
}

// A SKILL IS COPIED WITHOUT WHAT LEADS OUT OF ITS CHECKOUT OR BACK INTO IT: a
// link inside the checkout is followed; a link out of it (to /, to the home)
// and one back into a directory above it are left out, each said, the rest
// of the skill copied; a skill directory that leads out of the checkout is
// left out whole, said; the checkout's other skills are handed.
//
// Mutation: follow a link out of the checkout, or drop the check on the
// directories above, and a check goes red (or the launch never ends).
func TestASkillIsCopiedWithoutWhatLeadsOutOfItsCheckout(t *testing.T) {
	t.Parallel()
	r := newRepoRun(t, map[string]string{"repo/.git/": "", "repo/.codex/skills/s/SKILL.md": "---\nname: s\n---\n",
		"repo/.codex/skills/ok/SKILL.md": "---\nname: ok\n---\n", "repo/shared/notes.md": "IN-CHECKOUT\n"})
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "SKILL.md"), []byte("---\nname: away\n---\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	for link, target := range map[string]string{
		"repo/.codex/skills/s/notes.md": filepath.Join(r.work, "repo/shared/notes.md"),
		"repo/.codex/skills/s/root":     "/",
		"repo/.codex/skills/s/home":     outside,
		"repo/.codex/skills/s/up":       "..",
		"repo/.codex/skills/away":       outside,
	} {
		if err := os.Symlink(target, filepath.Join(r.work, link)); err != nil {
			t.Fatal(err)
		}
	}
	said := r.launchSaying(t)
	for _, want := range []string{"s is copied without root leads out of the checkout", "s is copied without home leads out of the checkout",
		"s is copied without up/s leads back into a directory above it", "away is left out: it leads out of its checkout"} {
		if !strings.Contains(said, want) {
			t.Errorf("the launch said\n%s\nwant %q", said, want)
		}
	}
	if got := read(t, r.skills+"/repo:s/notes.md"); got != "IN-CHECKOUT\n" {
		t.Errorf("a link inside the checkout copied as %q", got)
	}
	for _, gone := range []string{"/repo:s/root", "/repo:s/home", "/repo:s/up/s", "/repo:away"} {
		if _, err := os.Lstat(r.skills + gone); !os.IsNotExist(err) {
			t.Errorf("%s was copied (%v)", gone, err)
		}
	}
	if got := read(t, r.skills+"/repo:ok/SKILL.md"); got != "---\nname: \"repo:ok\"\n---\n" {
		t.Errorf("the checkout's other skill reads %q", got)
	}
}

// A CLAUDE CODE FIRING HANDS A CLONE'S SKILLS' LISTING ALONE, AND ONLY THE MAIN
// AGENT'S: a subagent's firing (its input names an agent_id) and one whose
// input does not read claim nothing, so the main agent's next firing still
// hands the clone; that firing hands the listing in Claude Code's hook shape,
// never the clone's instructions, which Claude Code loads itself on a read.
//
// Mutation: claim in a subagent's firing, claim on unreadable input, or hand
// the instructions, and a check goes red.
func TestAClaudeCodeFiringHandsAClonesSkillsToTheMainAgentAlone(t *testing.T) {
	t.Parallel()
	r := newRepoRun(t, nil)
	r.launch(t, nil)
	r.add(t, map[string]string{"late/.git/": "", "late/CLAUDE.md": "LATE-INSTRUCTIONS\n",
		"late/.codex/skills/fix/SKILL.md": "---\nname: fix\ndescription: Fixes.\n---\n"})
	fire := func(input string) (string, string) {
		var out, errs bytes.Buffer
		if code := runClaudeNested(context.Background(), flags{config: filepath.Join(r.state, "nested.json")},
			strings.NewReader(input), &out, &errs, os.Getenv); code != exitDone {
			t.Fatalf("the firing exited %d", code)
		}
		return out.String(), errs.String()
	}
	if out, _ := fire(`{"hook_event_name":"PostToolUse","agent_id":"a1"}`); out != "" {
		t.Errorf("a subagent's firing handed %q", out)
	}
	if out, errs := fire(`not json`); out != "" || !strings.Contains(errs, "the hook's input") {
		t.Errorf("an unreadable input's firing handed %q and said %q", out, errs)
	}
	out, _ := fire(`{"hook_event_name":"PostToolUse","tool_name":"Bash"}`)
	var answer struct {
		HookSpecificOutput struct {
			HookEventName     string `json:"hookEventName"`
			AdditionalContext string `json:"additionalContext"`
		} `json:"hookSpecificOutput"`
	}
	want := "# Skills of " + filepath.Join(r.workdir(t), "late") + "\n\nEach is a set of local instructions to follow, " +
		"stored in its `SKILL.md` file; to use one, read that file.\n- late:fix: Fixes. (file: " + r.skills + "/late:fix/SKILL.md)"
	if err := json.Unmarshal([]byte(out), &answer); err != nil || answer.HookSpecificOutput.HookEventName != "PostToolUse" ||
		answer.HookSpecificOutput.AdditionalContext != want {
		t.Errorf("the main agent's firing handed %q (%v), want the listing\n%q", out, err, want)
	}
	if out, _ := fire(`{"hook_event_name":"PostToolUse"}`); out != "" {
		t.Errorf("a later firing handed %q again", out)
	}
}

// A CLAUDE CODE FIRING READS AT MOST ITS INPUT'S BOUND: an input past
// maxHookInput (a long tool_response) is not read whole, so it claims nothing
// and says why, as an unreadable one does, and the next firing hands the
// clone; an agent_id ahead of such a response ends the read at once, a
// subagent's firing claiming nothing.
//
// Mutation: read the input without the bound, and the long main firing claims.
func TestAClaudeCodeFiringReadsAtMostItsInputsBound(t *testing.T) {
	t.Parallel()
	r := newRepoRun(t, nil)
	r.launch(t, nil)
	r.add(t, map[string]string{"late/.git/": "", "late/.codex/skills/fix/SKILL.md": "---\nname: fix\ndescription: Fixes.\n---\n"})
	long := strings.Repeat("x", maxHookInput)
	fire := func(input string) (string, string) {
		var out, errs bytes.Buffer
		if code := runClaudeNested(context.Background(), flags{config: filepath.Join(r.state, "nested.json")},
			strings.NewReader(input), &out, &errs, os.Getenv); code != exitDone {
			t.Fatalf("the firing exited %d", code)
		}
		return out.String(), errs.String()
	}
	if out, errs := fire(`{"agent_id":"a1","tool_response":"` + long + `"}`); out != "" || errs != "" {
		t.Errorf("a subagent's firing past the bound handed %q and said %q", out, errs)
	}
	if out, errs := fire(`{"hook_event_name":"PostToolUse","tool_response":"` + long + `"}`); out != "" ||
		!strings.Contains(errs, "the hook's input") {
		t.Errorf("a main firing past the bound handed %q and said %q, want nothing claimed, said", out, errs)
	}
	if out, _ := fire(`{"hook_event_name":"PostToolUse","tool_name":"Bash"}`); !strings.Contains(out, "late:fix") {
		t.Errorf("the next main firing handed %q, want the clone's listing", out)
	}
}

// A NESTED ADAPTER THAT HANDS SKILLS ALONE IS GIVEN NO INSTRUCTION TO READ: a
// launch whose config says nestedSkillsOnly writes the nested config without
// instruction slots or limit, so a claude-nested firing reads no CLAUDE.md
// only to drop it; one that does not keeps them, as codex-nested needs.
//
// Mutation: carry the launch's slots into every nested config, and a check
// goes red.
func TestANestedAdapterThatHandsSkillsAloneIsGivenNoInstructionToRead(t *testing.T) {
	t.Parallel()
	for _, only := range []bool{true, false} {
		r := newRepoRun(t, nil)
		limit := 100
		spec := codexSpec(r.skills)
		spec.Limit = &limit
		raw, _ := json.Marshal(launchConfig{State: r.state, Agents: r.agents, NestedSkillsOnly: only, checkoutSpec: spec})
		config := filepath.Join(t.TempDir(), "launch.json")
		if err := os.WriteFile(config, raw, 0o600); err != nil {
			t.Fatal(err)
		}
		var errs bytes.Buffer
		if err := checkoutLaunch(config, r.work, &errs); err != nil {
			t.Fatalf("the launch: %v\n%s", err, errs.String())
		}
		c, err := readNested(filepath.Join(r.state, "nested.json"))
		if err != nil {
			t.Fatal(err)
		}
		if read := len(c.Instructions) > 0 || c.Limit != nil; read == only {
			t.Errorf("nestedSkillsOnly %v: the nested config names instructions %v and limit %v", only,
				c.Instructions, c.Limit)
		}
		if len(c.SkillDirs) == 0 || c.Skills == "" {
			t.Errorf("nestedSkillsOnly %v: the nested config lost its skills: %+v", only, c.checkoutSpec)
		}
	}
}

// ONE FIRING'S COPIES ARE BOUND: a skill of more entries than the bound, or
// deeper than it, is left out whole, said with the entry and the bound it
// passed; a checkout's skills are planned only until they pass a fresh room's
// capacity, the rest left out and said; a fresh room takes those planned; a
// skill within it is copied.
//
// Mutation: drop the entry count, the depth, the plan's stop or the firing's
// room, and a check goes red.
func TestOneFiringsCopiesAreBound(t *testing.T) {
	t.Parallel()
	files := map[string]string{"repo/.git/": ""}
	for _, s := range []string{"many", "y-half-a", "y-half-b", "deep", "small"} {
		files["repo/.codex/skills/"+s+"/SKILL.md"] = "---\nname: " + s + "\n---\n"
	}
	for i := range maxCopyFiles {
		files[fmt.Sprintf("repo/.codex/skills/many/f%d", i)] = "x"
	}
	for i := range maxCopyFiles/2 + 1 {
		files[fmt.Sprintf("repo/.codex/skills/y-half-a/f%d", i)] = "x"
		files[fmt.Sprintf("repo/.codex/skills/y-half-b/f%d", i)] = "x"
	}
	files["repo/.codex/skills/deep/"+strings.Repeat("d/", maxCopyDepth+1)+"f"] = "x"
	r := newRepoRun(t, files)
	said := r.launchSaying(t)
	for _, want := range []string{"many is left out: its directory . passes the bound of 1024 entries",
		"deep is left out: its entry d/d/d/d/d/d/d/d/d runs past the bound of 8 directories deep",
		"the skills of " + filepath.Join(r.workdir(t), "repo") + " past one firing's bound are left out"} {
		if !strings.Contains(said, want) {
			t.Errorf("the launch said\n%s\nwant %q", said, want)
		}
	}
	for skill, copied := range map[string]bool{"many": false, "deep": false, "small": true, "y-half-a": true, "y-half-b": false} {
		if _, err := os.Stat(r.skills + "/repo:" + skill + "/SKILL.md"); (err == nil) != copied {
			t.Errorf("repo:%s copied %v, want %v", skill, err == nil, copied)
		}
	}
}

// A COPY IS LISTED FOR REMOVAL BEFORE IT IS WRITTEN AND APPEARS WHOLE OR NOT
// AT ALL: copies that cannot be built (the staging directory blocked) are
// listed and leave no skill under their names; once they can, each is listed,
// under its name with its SKILL.md renamed, and no staging is left.
//
// Mutation: list a name after its copy, build it in place, or keep the
// staging, and a check goes red.
func TestACopyIsListedBeforeItIsWrittenAndAppearsWhole(t *testing.T) {
	t.Parallel()
	r := newRepoRun(t, map[string]string{"repo/.git/": "", "repo/.codex/skills/one/SKILL.md": "---\nname: one\n---\n",
		"repo/.codex/skills/one/a": "A", "repo/.codex/skills/two/SKILL.md": "---\nname: two\n---\n"})
	staging := filepath.Join(r.state, stagingDir)
	if err := os.MkdirAll(r.state, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(staging, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if said := r.launchSaying(t); !strings.Contains(said, "one is left out") || !strings.Contains(said, "two is left out") {
		t.Errorf("the launch said %q, want both skills named", said)
	}
	copied := filepath.Join(filepath.Dir(r.skills), "copied")
	if got := read(t, copied); got != "repo:one\nrepo:two\n" {
		t.Errorf("the copies listed are %q, want both, though neither was built", got)
	}
	for _, name := range []string{"repo:one", "repo:two"} {
		if _, err := os.Lstat(r.skills + "/" + name); !os.IsNotExist(err) {
			t.Errorf("a copy never built left %s (%v)", name, err)
		}
	}
	if err := os.Remove(staging); err != nil {
		t.Fatal(err)
	}
	r.launchSaying(t)
	if got := read(t, r.skills+"/repo:one/SKILL.md") + read(t, r.skills+"/repo:one/a"); got != "---\nname: \"repo:one\"\n---\nA" {
		t.Errorf("the copied skill reads %q", got)
	}
	if _, err := os.Lstat(staging); !os.IsNotExist(err) {
		t.Errorf("the staging directory is left after the launch (%v)", err)
	}
}

// A CLONE STILL RUNNING IS NOT CLAIMED: a checkout whose git directory shows a
// clone under way (HEAD naming a branch that does not exist yet, index.lock
// held, no index) is handed nothing and stays unclaimed, and the first firing
// after it finishes hands it; a worktree's .git file is read through to its
// git directory and common directory; a reftable clone counts once its
// tables.list names a table.
//
// Mutation: claim on .git alone, ignore index.lock, or read no .git file, and
// a check goes red.
func TestACloneStillRunningIsNotClaimed(t *testing.T) {
	t.Parallel()
	r := newRepoRun(t, nil)
	r.launch(t, nil)
	git := filepath.Join(r.work, "late", ".git")
	r.add(t, map[string]string{"late/AGENTS.md": "LATE\n", "late/.git/HEAD": "ref: refs/heads/.invalid\n"})
	if got := r.fire(t); got != "" {
		t.Errorf("a clone with no branch yet was handed %q", got)
	}
	finishedGit(t, git)
	if err := os.WriteFile(filepath.Join(git, "index.lock"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if got := r.fire(t); got != "" {
		t.Errorf("a clone still checking out was handed %q", got)
	}
	if got := read(t, filepath.Join(r.state, "handed")); got != "" {
		t.Errorf("a clone under way was claimed: %q", got)
	}
	if err := os.Remove(filepath.Join(git, "index.lock")); err != nil {
		t.Fatal(err)
	}
	if got := r.fire(t); !strings.Contains(got, "LATE") {
		t.Errorf("the finished clone was handed %q", got)
	}
	common := filepath.Join(t.TempDir(), "repo.git")
	finishedGit(t, common)
	wt := filepath.Join(common, "worktrees", "wt")
	for rel, body := range map[string]string{"HEAD": "ref: refs/heads/main\n", "index": "DIRC", "commondir": "../..\n"} {
		if err := os.MkdirAll(wt, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(wt, rel), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	r.add(t, map[string]string{"wt/AGENTS.md": "WORKTREE\n", "wt/.git": "gitdir: " + wt + "\n"})
	if got := r.fire(t); !strings.Contains(got, "WORKTREE") {
		t.Errorf("a finished worktree was handed %q", got)
	}
	rt := filepath.Join(r.work, "rt", ".git")
	r.add(t, map[string]string{"rt/AGENTS.md": "REFTABLE\n", "rt/.git/HEAD": "ref: refs/heads/.invalid\n",
		"rt/.git/config": "[extensions]\n\trefStorage = reftable\n", "rt/.git/reftable/tables.list": "",
		"rt/.git/index": "DIRC"})
	if got := r.fire(t); got != "" {
		t.Errorf("a reftable clone with no table listed was handed %q", got)
	}
	if err := os.WriteFile(filepath.Join(rt, "reftable", "tables.list"), []byte("0x01-0x04-ab.ref\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := r.fire(t); !strings.Contains(got, "REFTABLE") {
		t.Errorf("a finished reftable clone was handed %q", got)
	}
}

// A NAME HOLDING A CONTROL CHARACTER IS NO CHECKOUT: a directory named with a
// trailing CR, which the handed file would read back without it, is never
// claimed, so it cannot be handed at every firing.
//
// Mutation: refuse a line break alone, and a check goes red.
func TestANameHoldingAControlCharacterIsNoCheckout(t *testing.T) {
	t.Parallel()
	r := newRepoRun(t, map[string]string{"cr\r/.git/": "", "cr\r/AGENTS.md": "CR\n", "tab\tname/.git/": ""})
	r.launch(t, nil)
	if got := read(t, filepath.Join(r.state, "handed")); got != "" {
		t.Errorf("a name holding a control character was claimed: %q", got)
	}
	if got := r.fire(t); got != "" {
		t.Errorf("a firing handed %q", got)
	}
}

// A CUT SPENDS THE LIMIT: once a slot's file was cut, a later slot hands
// nothing, never a fragment in the bytes a split rune left.
//
// Mutation: charge only the bytes kept, and the second slot's byte is handed.
func TestACutSpendsTheLimit(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	for name, body := range map[string]string{"A.md": "abcé", "B.md": "Z"} {
		if err := os.WriteFile(filepath.Join(root, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if text, n := rootInstructions(root, 4, names([]string{"A.md"}, []string{"B.md"}), &skips{stderr: io.Discard}); text != "abc" || n != 4 {
		t.Errorf("the slots hand %q for %d bytes, want abc and the whole limit spent", text, n)
	}
}

// A SKILL'S NAME FITS WHAT CODEX LISTS AND KEEPS ITS SHAPE: `<tag>:<skill>`
// past 64 characters is cut to `<tag>:<skill>-<8 hex digits of its full
// name's SHA-256>`, the skill part cut first and the tag to half what is left
// only where the skill needs it, the same for the same skill and apart for
// another; a name within it is as it was.
//
// Mutation: cut without the digest, drop the colon, or cut the tag first, and
// a check goes red.
func TestASkillsNameFitsWhatCodexListsAndKeepsItsShape(t *testing.T) {
	t.Parallel()
	if got := skillName("repo", "Check"); got != "repo:check" {
		t.Errorf("a short name is %q", got)
	}
	tag, skill := strings.Repeat("t", 40), strings.Repeat("s", 40)
	long, other := skillName(tag, skill), skillName(tag, strings.Repeat("s", 39)+"x")
	if len(long) != maxSkillName || !strings.HasPrefix(long, strings.Repeat("t", 27)+":"+strings.Repeat("s", 27)+"-") ||
		long == other || len(other) != maxSkillName {
		t.Errorf("long names are %q and %q, want %d characters each, `<tag>:<skill>-<digest>`, apart", long, other, maxSkillName)
	}
	if again := skillName(tag, skill); again != long {
		t.Errorf("the same skill is %q, then %q", long, again)
	}
	if got := skillName(strings.Repeat("r", 60), "check"); len(got) != maxSkillName ||
		!strings.HasPrefix(got, strings.Repeat("r", 49)+":check-") {
		t.Errorf("a long tag's name is %q, want its tag cut and `:check-` kept", got)
	}
}

// A CHECKOUT THAT DOES NOT FIT THE ROOM WAITS FOR THE NEXT FIRING: checkouts
// are planned before they are claimed, so one whose skills do not fit what is
// left of the launch's bound is left unclaimed (a later one that fits is
// claimed past it) and the next firing, with a fresh room, hands it; once the
// room is spent, the rest with skills are not planned at all, said once, and
// one with no skills is still claimed at the launch, its instructions handed.
//
// Mutation: claim before planning, plan against a shared room for good, keep
// planning a spent room, or stop at it, and a check goes red.
func TestACheckoutThatDoesNotFitTheRoomWaitsForTheNextFiring(t *testing.T) {
	t.Parallel()
	files := map[string]string{"a/.git/": "", "b/.git/": "", "c/.git/": ""}
	for repo, n := range map[string]int{"a": 600, "b": 600, "c": 300} {
		files[repo+"/.codex/skills/s/SKILL.md"] = "---\nname: s\n---\n"
		for i := range n - 1 {
			files[fmt.Sprintf("%s/.codex/skills/s/f%d", repo, i)] = "x"
		}
	}
	r := newRepoRun(t, files)
	r.launch(t, nil)
	if got := read(t, filepath.Join(r.state, "handed")); got != "a\nc\n" {
		t.Errorf("the launch claimed %q, want a and c, b left for the next firing", got)
	}
	if got := r.fire(t); !strings.Contains(got, "- b:s (file: "+r.skills+"/b:s/SKILL.md)") {
		t.Errorf("the next firing handed %q, want b's skill", got)
	}
	full := map[string]string{"x/.git/": "", "y/.git/": "", "y/.codex/skills/t/SKILL.md": "---\nname: t\n---\n",
		"z/.git/": "", "z/AGENTS.md": "NO-SKILLS\n"}
	for _, s := range []string{"p", "q"} {
		full["x/.codex/skills/"+s+"/SKILL.md"] = "---\nname: " + s + "\n---\n"
		for i := range maxCopyFiles/2 - 1 {
			full[fmt.Sprintf("x/.codex/skills/%s/f%d", s, i)] = "x"
		}
	}
	spent := newRepoRun(t, full)
	if said := spent.launchSaying(t); !strings.Contains(said, "copy bound is spent; the checkouts with skills left wait") {
		t.Errorf("a spent room said %q", said)
	}
	if got := read(t, filepath.Join(spent.state, "handed")); got != "x\nz\n" {
		t.Errorf("a spent room's launch claimed %q, want x and z (no skills), y left for the next firing", got)
	}
	if got := read(t, spent.agents); !strings.Contains(got, "for `z` with root") || !strings.Contains(got, "NO-SKILLS") {
		t.Errorf("the launch handed %q, want z's instructions", got)
	}
}

// A DIRECTORY PAST THE BOUND IS NOT READ WHOLE: a skills directory holding
// more entries than the bound is left out and said, and so is a skill whose
// own directory does, never listed past the bound.
//
// Mutation: list a directory whole, and a check goes red.
func TestADirectoryPastTheBoundIsNotReadWhole(t *testing.T) {
	t.Parallel()
	files := map[string]string{"repo/.git/": "", "repo/.agents/skills/wide/SKILL.md": "---\nname: wide\n---\n"}
	for i := range maxCopyFiles + 1 {
		files[fmt.Sprintf("repo/.codex/skills/d%d/x", i)] = "x"
		files[fmt.Sprintf("repo/.agents/skills/wide/f%d", i)] = "x"
	}
	r := newRepoRun(t, files)
	said := r.launchSaying(t)
	for _, want := range []string{"the skills of " + filepath.Join(r.workdir(t), "repo/.codex/skills") + " are left out: it holds more than 1024 entries",
		"wide is left out: its directory . passes the bound of 1024 entries"} {
		if !strings.Contains(said, want) {
			t.Errorf("the launch said\n%s\nwant %q", said, want)
		}
	}
}

// A FIFO SWAPPED IN AFTER THE PLAN NEVER BLOCKS THE FIRING: a planned file
// replaced by a named pipe, which no writer opens, refuses the copy at once,
// and nothing is left under its name.
//
// Mutation: open the planned file waiting for a writer, and the copy blocks.
func TestAFIFOSwappedInAfterThePlanNeverBlocks(t *testing.T) {
	t.Parallel()
	r := newRepoRun(t, map[string]string{"repo/.git/": "", "repo/.codex/skills/s/SKILL.md": "---\nname: s\n---\n",
		"repo/.codex/skills/s/data": "DATA"})
	root := filepath.Join(r.work, "repo")
	real, err := filepath.EvalSymlinks(root)
	if err != nil {
		t.Fatal(err)
	}
	plan, _, err := planSkill(filepath.Join(root, ".codex/skills/s"), real, "repo:s")
	if err != nil {
		t.Fatal(err)
	}
	data := filepath.Join(root, ".codex/skills/s/data")
	if err := os.Remove(data); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Mkfifo(data, 0o600); err != nil {
		t.Fatal(err)
	}
	var errs bytes.Buffer
	if _, err := copyPlanned(plan, codexSpec(r.skills), filepath.Join(r.state, stagingDir), &skips{stderr: &errs}); err == nil {
		t.Errorf("a copy built over a FIFO succeeded")
	}
	if _, err := os.Lstat(filepath.Join(r.skills, "repo:s")); !os.IsNotExist(err) {
		t.Errorf("the refused copy left a skill under its name (%v)", err)
	}
}

// A FILE SWAPPED AFTER THE PLAN IS NEVER READ THROUGH: a planned file turned
// into a link out of the checkout, or replaced by another file, before the
// copy is built refuses the copy, and nothing is left under its name.
//
// Mutation: open by path following links, or skip the check against the
// planned file, and a check goes red.
func TestAFileSwappedAfterThePlanIsNeverReadThrough(t *testing.T) {
	t.Parallel()
	for name, swap := range map[string]func(t *testing.T, path string){
		"a link out": func(t *testing.T, path string) {
			outside := filepath.Join(t.TempDir(), "secret")
			if err := os.WriteFile(outside, []byte("OUTSIDE"), 0o600); err != nil {
				t.Fatal(err)
			}
			if err := os.Remove(path); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(outside, path); err != nil {
				t.Fatal(err)
			}
		},
		"another file": func(t *testing.T, path string) {
			if err := os.WriteFile(path+".new", []byte("OTHER"), 0o600); err != nil {
				t.Fatal(err)
			}
			if err := os.Rename(path+".new", path); err != nil {
				t.Fatal(err)
			}
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			r := newRepoRun(t, map[string]string{"repo/.git/": "", "repo/.codex/skills/s/SKILL.md": "---\nname: s\n---\n",
				"repo/.codex/skills/s/data": "DATA"})
			root := filepath.Join(r.work, "repo")
			real, err := filepath.EvalSymlinks(root)
			if err != nil {
				t.Fatal(err)
			}
			plan, _, err := planSkill(filepath.Join(root, ".codex/skills/s"), real, "repo:s")
			if err != nil {
				t.Fatal(err)
			}
			swap(t, filepath.Join(root, ".codex/skills/s/data"))
			spec := codexSpec(r.skills)
			var errs bytes.Buffer
			if _, err := copyPlanned(plan, spec, filepath.Join(r.state, stagingDir), &skips{stderr: &errs}); err == nil {
				t.Errorf("a copy built over a swapped file succeeded")
			}
			if _, err := os.Lstat(filepath.Join(r.skills, "repo:s")); !os.IsNotExist(err) {
				t.Errorf("the refused copy left a skill under its name (%v)", err)
			}
		})
	}
}

// A NESTED FIRING'S SKIPS ARE KEPT FOR THE RUN'S END AND HANDED TO THE MODEL:
// what a firing leaves out (here a skill leading out of its checkout) is said
// on its stderr, named in its answer, and appended to the state's skipped
// file, a later firing's lines after the earlier's; the launch's own skips go
// to its stderr alone, which is the run's.
//
// Mutation: keep the launch's lines, keep a firing's in place of the last,
// or leave them out of the answer, and a check goes red.
func TestANestedFiringsSkipsAreKeptForTheRunsEnd(t *testing.T) {
	t.Parallel()
	outside, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	writeOutside := func(t *testing.T, r repoRun, checkout string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Join(r.work, checkout, ".codex/skills"), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(filepath.Join(outside, "o"), filepath.Join(r.work, checkout, ".codex/skills/out")); err != nil {
			t.Fatal(err)
		}
	}
	r := newRepoRun(t, map[string]string{"early/.git/": ""})
	if err := os.MkdirAll(filepath.Join(outside, "o"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(outside, "o", "SKILL.md"), []byte("---\nname: o\n---\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	writeOutside(t, r, "early")
	if said := r.launchSaying(t); !strings.Contains(said, "early/.codex/skills/out is left out") {
		t.Fatalf("the launch said %q, want the early skill left out", said)
	}
	if got := read(t, filepath.Join(r.state, skippedLog)); got != "" {
		t.Errorf("the launch kept %q for the run's end, want its lines on its stderr alone", got)
	}
	var want string
	for _, late := range []string{"late", "later"} {
		r.add(t, map[string]string{late + "/.git/": ""})
		writeOutside(t, r, late)
		var out, errs bytes.Buffer
		runCodexNested(context.Background(), flags{config: filepath.Join(r.state, "nested.json")},
			strings.NewReader("{}"), &out, &errs, os.Getenv)
		line := "the skill " + filepath.Join(r.workdir(t), late, ".codex/skills/out") +
			" is left out: it leads out of its checkout, to " + filepath.Join(outside, "o")
		if errs.String() != "notehelper: "+line+"\n" {
			t.Errorf("the firing said %q, want %q", errs.String(), line)
		}
		if !strings.Contains(out.String(), `- `+line) {
			t.Errorf("the firing answered %s, want it to name %q", out.String(), line)
		}
		want += "notehelper: " + line + "\n"
		if got := read(t, filepath.Join(r.state, skippedLog)); got != want {
			t.Errorf("the run keeps %q for its end, want %q", got, want)
		}
	}
}

// A SKILL WHOSE NAME THE HOME'S SKILLS HOLD IS LEFT OUT, SAID: the home's own
// skill under the name a checkout's would take stays, and the checkout's is
// named on stderr, never copied over it.
//
// Mutation: leave it out silently, or copy over the home's, and a check goes
// red.
func TestASkillWhoseNameTheHomeHoldsIsLeftOutSaid(t *testing.T) {
	t.Parallel()
	r := newRepoRun(t, map[string]string{"repo/.git/": "", "repo/.codex/skills/s/SKILL.md": "---\nname: s\n---\nREPO\n"})
	if err := os.MkdirAll(r.skills+"/repo:s", 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(r.skills+"/repo:s/SKILL.md", []byte("HOME\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	said := r.launchSaying(t)
	if want := "the skill " + filepath.Join(r.workdir(t), "repo/.codex/skills/s") + " is left out: the home's skills " +
		"hold repo:s already"; !strings.Contains(said, want) {
		t.Errorf("the launch said %q, want %q", said, want)
	}
	if got := read(t, r.skills+"/repo:s/SKILL.md"); got != "HOME\n" {
		t.Errorf("the home's skill reads %q, want it kept", got)
	}
}

// THE SKIP LOG IS BOUNDED: lines past maxSkippedLog are dropped, the first of
// them replaced by the cut line once, and a log that ends in it takes no
// later line, however short.
//
// Mutation: drop the bound, write the cut on every firing, or take a short
// line after it, and a check goes red.
func TestTheSkipLogIsBounded(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), skippedLog)
	long := strings.Repeat("x", 1000)
	var lines []string
	for range maxSkippedLog / 1000 {
		lines = append(lines, long)
	}
	if err := appendSkipped(path, lines); err != nil {
		t.Fatal(err)
	}
	for _, more := range [][]string{{long}, {"short"}} {
		if err := appendSkipped(path, more); err != nil {
			t.Fatal(err)
		}
	}
	got := read(t, path)
	if len(got) > maxSkippedLog || !strings.HasSuffix(got, skippedCut) || strings.Count(got, skippedCut) != 1 {
		t.Errorf("the log holds %d bytes, ends in the cut %v and holds it %d times; want at most %d, the cut once, last",
			len(got), strings.HasSuffix(got, skippedCut), strings.Count(got, skippedCut), maxSkippedLog)
	}
	if kept := strings.Count(got, long); kept != (maxSkippedLog-len(skippedCut))/len("notehelper: "+long+"\n") {
		t.Errorf("the log keeps %d lines, want every one that fits before the cut", kept)
	}
}

// A REFUSED SKILL'S WALK IS CHARGED TO THE PLAN'S STOP: a skill refused past
// the entry bound after its walk looked at a fresh room's worth of entries
// stops the checkout's planning as a copied one would, the skills after it
// left out and said, so a checkout of refused skills costs a firing no more
// than one it could copy.
//
// Mutation: charge a refused plan nothing, and the skill after it is planned
// and copied.
func TestARefusedSkillsWalkIsChargedToThePlansStop(t *testing.T) {
	t.Parallel()
	files := map[string]string{"repo/.git/": "", "repo/.codex/skills/a-sprawl/SKILL.md": "---\nname: a\n---\n",
		"repo/.codex/skills/b-small/SKILL.md": "---\nname: b\n---\n"}
	for i := range maxCopyFiles/2 + 100 {
		files[fmt.Sprintf("repo/.codex/skills/a-sprawl/one/f%d", i)] = "x"
		files[fmt.Sprintf("repo/.codex/skills/a-sprawl/two/f%d", i)] = "x"
	}
	r := newRepoRun(t, files)
	said := r.launchSaying(t)
	for _, want := range []string{"a-sprawl is left out: its entry ", "passes one firing's bound of 1024 entries",
		"the skills of " + filepath.Join(r.workdir(t), "repo") + " past one firing's bound are left out"} {
		if !strings.Contains(said, want) {
			t.Errorf("the launch said\n%s\nwant %q", said, want)
		}
	}
	if _, err := os.Lstat(r.skills + "/repo:b-small"); !os.IsNotExist(err) {
		t.Errorf("the skill after a refused walk of a room's worth was copied (%v)", err)
	}
}

// NOTHING IS EVER BUILT INSIDE A SKILLS DIRECTORY: as each copy is moved into
// place, it comes from outside the run's skills directory, which holds the
// home's own skills and the copies moved before it, nothing half built, so no
// harness that walks the directory whole lists a copy being made.
//
// Mutation: build the copies under the skills directory, and a check goes
// red. Not parallel: it stands in for renameCopy.
func TestNothingIsEverBuiltInsideASkillsDirectory(t *testing.T) {
	r := newRepoRun(t, map[string]string{"repo/.git/": "", "repo/.codex/skills/one/SKILL.md": "---\nname: one\n---\n",
		"repo/.codex/skills/one/a": "A", "repo/.codex/skills/two/SKILL.md": "---\nname: two\n---\n"})
	if err := os.MkdirAll(r.skills+"/own", 0o700); err != nil {
		t.Fatal(err)
	}
	var landed []string
	renameCopy = func(from, to string) error {
		if strings.HasPrefix(from, r.skills+string(filepath.Separator)) {
			t.Errorf("the copy of %s was built inside the skills directory, at %s", filepath.Base(to), from)
		}
		entries, err := os.ReadDir(r.skills)
		if err != nil {
			t.Fatal(err)
		}
		names := make([]string, 0, len(entries))
		for _, e := range entries {
			names = append(names, e.Name())
		}
		landed = append(landed, filepath.Base(to)+" onto "+strings.Join(names, ","))
		return os.Rename(from, to)
	}
	t.Cleanup(func() { renameCopy = os.Rename })
	r.launchSaying(t)
	if got, want := strings.Join(landed, "; "), "repo:one onto own; repo:two onto own,repo:one"; got != want {
		t.Errorf("the copies landed as %q, want %q", got, want)
	}
}

// A COPY ACROSS FILESYSTEMS IS LEFT OUT, SAID: where no one rename can move a
// copy into the run's skills directory (EXDEV: one linked or mounted
// elsewhere), the skill is left out with the typed error and a line naming
// both directories, and nothing is copied there in parts or left built.
//
// Mutation: fall back to a copy, or lose the type, and a check goes red. Not
// parallel: it stands in for renameCopy.
func TestACopyAcrossFilesystemsIsLeftOutSaid(t *testing.T) {
	r := newRepoRun(t, map[string]string{"repo/.git/": "", "repo/.codex/skills/s/SKILL.md": "---\nname: s\n---\n"})
	renameCopy = func(from, to string) error {
		return &os.LinkError{Op: "rename", Old: from, New: to, Err: syscall.EXDEV}
	}
	t.Cleanup(func() { renameCopy = os.Rename })
	staging := filepath.Join(r.state, stagingDir)
	said := r.launchSaying(t)
	if want := "the skill " + filepath.Join(r.workdir(t), "repo/.codex/skills/s") + " is left out: the run's skills " +
		"directory " + r.skills + " is on another filesystem than " + staging; !strings.Contains(said, want) {
		t.Errorf("the launch said %q, want %q", said, want)
	}
	for _, dir := range []string{r.skills, staging} {
		if _, err := os.Lstat(filepath.Join(dir, "repo:s")); !os.IsNotExist(err) {
			t.Errorf("%s holds the skill not moved (%v)", dir, err)
		}
	}
	root := filepath.Join(r.work, "repo")
	real, err := filepath.EvalSymlinks(root)
	if err != nil {
		t.Fatal(err)
	}
	plan, _, err := planSkill(filepath.Join(root, ".codex/skills/s"), real, "repo:s")
	if err != nil {
		t.Fatal(err)
	}
	var other *otherFilesystemError
	if _, err = copyPlanned(plan, codexSpec(r.skills), staging, &skips{stderr: io.Discard}); !errors.As(err, &other) ||
		!errors.Is(err, syscall.EXDEV) {
		t.Errorf("the copy failed with %v, want an otherFilesystemError that is EXDEV", err)
	}
}

// A CHECKOUT THAT WAITS SAYS ITS PLAN ONCE, WHEN IT IS HANDED: a checkout
// whose skills do not fit what a firing has left is planned again at every
// firing until one has room; what its plan leaves out (here a skill whose name
// the home's skills hold) is said by the firing that hands it, once, never by
// the launch or a firing that defers it, nor by one after.
//
// Mutation: say a plan's lines before the checkout is claimed, and a check
// goes red.
func TestACheckoutThatWaitsSaysItsPlanOnceWhenItIsHanded(t *testing.T) {
	t.Parallel()
	files := map[string]string{"aaa/.git/": "", "aaa/.codex/skills/big/SKILL.md": "---\nname: big\n---\n",
		"bbb/.git/": "", "bbb/.codex/skills/big/SKILL.md": "---\nname: big\n---\n",
		"bbb/.codex/skills/dup/SKILL.md": "---\nname: dup\n---\n"}
	for i := range maxCopyFiles/2 + 10 {
		files[fmt.Sprintf("aaa/.codex/skills/big/f%d", i)] = "x"
		files[fmt.Sprintf("bbb/.codex/skills/big/f%d", i)] = "x"
	}
	r := newRepoRun(t, files)
	if err := os.MkdirAll(r.skills+"/bbb:dup", 0o700); err != nil {
		t.Fatal(err)
	}
	line := "the skill " + filepath.Join(r.workdir(t), "bbb/.codex/skills/dup") + " is left out: the home's skills hold bbb:dup already"
	if said := r.launchSaying(t); strings.Contains(said, line) {
		t.Errorf("the launch, which defers bbb, said %q", line)
	}
	var says []string
	for range 2 {
		var out, errs bytes.Buffer
		runCodexNested(context.Background(), flags{config: filepath.Join(r.state, "nested.json")},
			strings.NewReader("{}"), &out, &errs, os.Getenv)
		says = append(says, fmt.Sprintf("%d,%d", strings.Count(errs.String(), line), strings.Count(out.String(), line)))
	}
	if got := strings.Join(says, " "); got != "1,1 0,0" {
		t.Errorf("the firings said and answered the line %s times (stderr,answer per firing), want 1,1 0,0", got)
	}
	if got := strings.Count(read(t, filepath.Join(r.state, skippedLog)), line); got != 1 {
		t.Errorf("the run keeps the line %d times, want once", got)
	}
}

// A FIRING AFTER THE RUN ENDED CLAIMS NOTHING: once the runner's restore
// marked the state ended, a firing still running (orphaned by the harness's
// end) hands nothing and copies nothing, so nothing lands in the home after
// the restore removed the run's copies.
//
// Mutation: claim past the marker, and a check goes red.
func TestAFiringAfterTheRunEndedClaimsNothing(t *testing.T) {
	t.Parallel()
	r := newRepoRun(t, nil)
	r.launch(t, nil)
	if err := os.WriteFile(filepath.Join(r.state, endedMarker), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	r.add(t, map[string]string{"late/.git/": "", "late/AGENTS.md": "LATE\n", "late/.codex/skills/s/SKILL.md": "---\nname: s\n---\n"})
	if got := r.fire(t); got != "" {
		t.Errorf("a firing after the run ended answered %q, want nothing", got)
	}
	if _, err := os.Lstat(r.skills + "/late:s"); !os.IsNotExist(err) {
		t.Errorf("a firing after the run ended copied late:s (%v)", err)
	}
	if got := read(t, filepath.Join(r.state, "handed")); got != "" {
		t.Errorf("a firing after the run ended claimed %q", got)
	}
}

// ONE FIRING'S LISTING IS BOUND: the skills a firing copied and the lines of
// what it left out reach the model within maxListing, cut after a whole line
// with one line saying so and nothing after it; a checkout's instructions go
// whole beside them.
//
// Mutation: drop the bound, or cut mid-line, and a check goes red.
func TestOneFiringsListingIsBound(t *testing.T) {
	t.Parallel()
	long := strings.Repeat("d", maxSkillDescription)
	h := handedCheckout{name: "repo", root: "/w/repo", text: "INSTRUCTIONS"}
	for i := range 200 {
		h.skills = append(h.skills, handedSkill{name: fmt.Sprintf("repo:s%d", i), description: long,
			path: fmt.Sprintf("/h/skills/repo:s%d/SKILL.md", i)})
	}
	var skipped []string
	for i := range 100 {
		skipped = append(skipped, fmt.Sprintf("the skill /w/other/s%d is left out: %s", i, long))
	}
	answer := codexNestedContext([]handedCheckout{h}, skipped)
	instructions := "# AGENTS.md instructions for /w/repo\n\n<INSTRUCTIONS>\nINSTRUCTIONS\n</INSTRUCTIONS>\n\n"
	listing, ok := strings.CutPrefix(answer, instructions)
	if !ok {
		t.Fatalf("the answer does not open with the instructions whole: %.200q", answer)
	}
	if len(listing) > maxListing || !strings.HasSuffix(listing, "\n"+listingCut) || strings.Count(listing, listingCut) != 1 {
		t.Errorf("the listing holds %d bytes, ends in the cut %v (%d times); want at most %d, ending in one cut line",
			len(listing), strings.HasSuffix(listing, listingCut), strings.Count(listing, listingCut), maxListing)
	}
	before := strings.TrimSuffix(listing, "\n"+listingCut)
	if last := before[strings.LastIndexByte(before, '\n')+1:]; !strings.HasSuffix(last, "SKILL.md)") {
		t.Errorf("the listing is cut inside a line: it ends %q", last)
	}
	if strings.Contains(listing, "is left out") {
		t.Errorf("lines past the cut reached the model")
	}
}

// A COPY FOR SKILLS ON ANOTHER FILESYSTEM IS BUILT BESIDE THEM: where the
// run's state is on another filesystem than the skills directory, a copy is
// built in the staging beside the skills directory's real path, outside it,
// and moved into place by one rename, the staging removed once emptied.
//
// Mutation: build in the state whatever the filesystems, and a check goes
// red. Not parallel: it stands in for onOneFilesystem and renameCopy.
func TestACopyForSkillsOnAnotherFilesystemIsBuiltBesideThem(t *testing.T) {
	r := newRepoRun(t, map[string]string{"repo/.git/": "", "repo/.codex/skills/s/SKILL.md": "---\nname: s\n---\n"})
	oneFilesystem := onOneFilesystem
	onOneFilesystem = func(os.FileInfo, os.FileInfo) bool { return false }
	var from []string
	renameCopy = func(old, to string) error {
		from = append(from, old)
		return os.Rename(old, to)
	}
	t.Cleanup(func() { onOneFilesystem, renameCopy = oneFilesystem, os.Rename })
	if said := r.launchSaying(t); said != "" {
		t.Errorf("the launch said %q", said)
	}
	real, err := filepath.EvalSymlinks(r.skills)
	if err != nil {
		t.Fatal(err)
	}
	beside := filepath.Join(filepath.Dir(real), siblingStaging)
	if want := []string{filepath.Join(beside, "repo:s")}; !slices.Equal(from, want) {
		t.Errorf("the copies were built at %q, want %q", from, want)
	}
	if got := read(t, r.skills+"/repo:s/SKILL.md"); got != "---\nname: \"repo:s\"\n---\n" {
		t.Errorf("the copy reads %q", got)
	}
	if _, err := os.Lstat(beside); !os.IsNotExist(err) {
		t.Errorf("the staging beside the skills is left (%v)", err)
	}
}
