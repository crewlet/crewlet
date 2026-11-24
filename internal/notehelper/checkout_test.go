package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
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
	return checkoutSpec{Instructions: [][]string{{"AGENTS.override.md", "AGENTS.md"}}, Limit: limited(32 << 10),
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

// add writes files under the working directory, as newRepoRun does.
func (r repoRun) add(t *testing.T, files map[string]string) {
	t.Helper()
	for rel, body := range files {
		path := filepath.Join(r.work, rel)
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

// limited is a limit of n bytes.
func limited(n int) *int { return &n }

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
	if err := os.MkdirAll(filepath.Join(other, ".git"), 0o700); err != nil {
		t.Fatal(err)
	}
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
	text, n := rootInstructions(root, 100, [][]string{{"ONE.md"}, {"TWO.md", "TWO-ALT.md"}, {"THREE.md", "THREE-ALT.md"}, {"NONE.md"}})
	if text != "ONE\n\n\nTHREE\n" || n != 10 {
		t.Errorf("the slots hand %q for %d bytes, want ONE and THREE for 10", text, n)
	}
	if text, n = rootInstructions(root, 7, [][]string{{"ONE.md"}, {"THREE-ALT.md"}}); text != "ONE\n\n\nTHR" || n != 7 {
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

// A SKILL THAT LINKS BACK INTO ITSELF IS LEFT OUT and said, never copied
// without end, and the checkout's other skills are handed.
//
// Mutation: drop the check on the directories above, and the launch never
// ends; skip the rest of the checkout, and a check goes red.
func TestASkillThatLinksBackIntoItselfIsLeftOut(t *testing.T) {
	t.Parallel()
	r := newRepoRun(t, map[string]string{"repo/.git/": "", "repo/.codex/skills/loop/SKILL.md": "---\nname: loop\n---\n",
		"repo/.codex/skills/ok/SKILL.md": "---\nname: ok\n---\n"})
	if err := os.Symlink("..", filepath.Join(r.work, "repo/.codex/skills/loop/up")); err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(launchConfig{State: r.state, Agents: r.agents, checkoutSpec: codexSpec(r.skills)})
	config := filepath.Join(t.TempDir(), "launch.json")
	if err := os.WriteFile(config, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	var errs bytes.Buffer
	if err := checkoutLaunch(config, r.work, &errs); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(errs.String(), "links back into a directory above it") {
		t.Errorf("the launch said %q, want the looping skill named", errs.String())
	}
	if _, err := os.Stat(r.skills + "/repo:loop"); !os.IsNotExist(err) {
		t.Errorf("the looping skill was copied (%v)", err)
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
