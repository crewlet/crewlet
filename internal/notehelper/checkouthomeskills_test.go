package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

// plantPlace puts a person's entry of kind at path: a directory without
// SKILL.md, a skill under another name, a plain file, or a link leading
// nowhere.
func plantPlace(t *testing.T, path, kind string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	var err error
	switch kind {
	case "a directory without SKILL.md":
		if err = os.Mkdir(path, 0o700); err == nil {
			err = os.WriteFile(filepath.Join(path, "x.md"), []byte("MINE"), 0o600)
		}
	case "a skill under another name":
		if err = os.Mkdir(path, 0o700); err == nil {
			err = os.WriteFile(filepath.Join(path, "SKILL.md"), []byte("---\nname: other\n---\n"), 0o600)
		}
	case "a plain file":
		err = os.WriteFile(path, []byte("MINE"), 0o600)
	default:
		err = os.Symlink("nowhere", path)
	}
	if err != nil {
		t.Fatal(err)
	}
}

// placeKinds is every kind of person's entry a copy's place may hold.
var placeKinds = []string{"a directory without SKILL.md", "a skill under another name", "a plain file",
	"a link leading nowhere"}

// A COPY'S TAKEN PLACE IS NEVER LISTED AS THE RUN'S, NOT FOR A MOMENT: the
// run's end and the next gate remove every name the copied list holds, so an
// entry already at the copy's place, of any kind and under any listing rule,
// refuses the copy before it is listed (a firing killed after listing it would
// leave the entry to the next gate), said as the home's skills holding the
// name, and the entry is as it was.
//
// Mutation: list the copy before checking its place (drop copyPlanned's
// check), and the list is written.
func TestACopysTakenPlaceIsNeverListedAsTheRuns(t *testing.T) {
	t.Parallel()
	for _, listedAs := range []string{"", listedGrok, listedKimi, listedOpenCode, listedMuse} {
		for _, kind := range placeKinds {
			r := newRepoRun(t, map[string]string{"late/.git/": "", "late/.agents/skills/s/SKILL.md": "---\nname: s\n---\n"})
			root := filepath.Join(r.work, "late")
			real, err := filepath.EvalSymlinks(root)
			if err != nil {
				t.Fatal(err)
			}
			plan, _, err := planSkill(filepath.Join(root, ".agents/skills/s"), real, "late:s")
			if err != nil {
				t.Fatal(err)
			}
			place := filepath.Join(r.skills, "late:s")
			plantPlace(t, place, kind)
			before, _ := os.Lstat(place)
			spec := codexSpec(r.skills)
			spec.ListedAs, spec.held = listedAs, &homeNames{read: true, taken: map[string]string{}}
			_, err = copyPlanned(plan, spec, filepath.Join(r.state, stagingDir), &skips{stderr: io.Discard})
			if err == nil || err.Error() != "the home's skills hold late:s already" {
				t.Errorf("listedAs %q, %s: the copy over it returned %v", listedAs, kind, err)
			}
			if _, err := os.Lstat(spec.Copied); !os.IsNotExist(err) {
				copied, _ := os.ReadFile(spec.Copied)
				t.Errorf("listedAs %q, %s: the copy over the person's entry was listed (%q, %v)", listedAs, kind, copied, err)
			}
			if after, err := os.Lstat(place); err != nil || after.Mode() != before.Mode() {
				t.Errorf("listedAs %q, %s: the person's entry is now %v (%v)", listedAs, kind, after, err)
			}
		}
	}
}

// WHATEVER THE LISTING RULE, A NAME THE RUN'S SKILLS HOLD IS HELD: an entry
// no rule keys as the copy (a directory without SKILL.md, a skill under
// another name, a file, a link leading nowhere) would be replaced by the copy
// or listed and removed, so planning says the name is held, after the rule's
// own reasons.
//
// Mutation: drop homeHolds' place check for a ruled harness, and the name is
// free.
func TestARunsSkillsEntryHoldsItsNameWhateverTheRule(t *testing.T) {
	t.Parallel()
	skills := t.TempDir()
	for i, kind := range placeKinds {
		plantPlace(t, filepath.Join(skills, "late:"+string(rune('a'+i))), kind)
	}
	for _, listedAs := range []string{"", listedGrok, listedKimi, listedOpenCode, listedMuse} {
		spec := checkoutSpec{Skills: skills, ListedAs: listedAs, held: &homeNames{read: true, taken: map[string]string{}}}
		for i, kind := range placeKinds {
			name := "late:" + string(rune('a'+i))
			if got, want := homeHolds(spec, name), "the home's skills hold "+name+" already"; got != want {
				t.Errorf("listedAs %q, %s: %s is held for %q, want %q", listedAs, kind, name, got, want)
			}
		}
		if got := homeHolds(spec, "late:free"); got != "" {
			t.Errorf("listedAs %q: a free name is held for %q", listedAs, got)
		}
	}
}

// plannedCopy is a run whose checkout late holds the skill s (SKILL.md and a
// data file), planned as late:s, with the Codex spec: the run, the plan and
// the spec.
func plannedCopy(t *testing.T) (repoRun, plannedSkill, checkoutSpec) {
	t.Helper()
	r := newRepoRun(t, map[string]string{"late/.git/": "", "late/.agents/skills/s/SKILL.md": "---\nname: s\n---\n",
		"late/.agents/skills/s/data": "DATA"})
	root := filepath.Join(r.work, "late")
	real, err := filepath.EvalSymlinks(root)
	if err != nil {
		t.Fatal(err)
	}
	plan, _, err := planSkill(filepath.Join(root, ".agents/skills/s"), real, "late:s")
	if err != nil {
		t.Fatal(err)
	}
	return r, plan, codexSpec(r.skills)
}

// unlisted fails t where the run's list of copies names late:s.
func unlisted(t *testing.T, spec checkoutSpec, why string) {
	t.Helper()
	if copied, _ := os.ReadFile(spec.Copied); strings.Contains(string(copied), "late:s") {
		t.Errorf("%s: the copy that never landed is still listed for removal: %q", why, copied)
	}
}

// A COPY THAT NEVER LANDS IS TAKEN OFF THE LIST: its name is listed before
// it is written (a firing killed mid-copy leaves nothing the next gate does
// not remove), so a copy that fails after that (here something put at its
// place while it was built, which the rename then refuses) comes off the
// list, or the run's end would remove that entry, and the entry is as it was.
//
// Mutation: list the name after the copy, or leave a failed copy listed, and
// this goes red. Not parallel: it stands in for renameCopy.
func TestACopyThatNeverLandsIsTakenOffTheList(t *testing.T) {
	r, plan, spec := plannedCopy(t)
	mine := filepath.Join(r.skills, "late:s", "x.md")
	renameCopy = func(from, to string) error {
		if copied, _ := os.ReadFile(spec.Copied); !strings.Contains(string(copied), "late:s") {
			t.Errorf("the copy is moved into place before it is listed for removal: %q", copied)
		}
		if err := os.MkdirAll(filepath.Dir(mine), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(mine, []byte("MINE"), 0o600); err != nil {
			t.Fatal(err)
		}
		return &os.LinkError{Op: "rename", Old: from, New: to, Err: syscall.ENOTEMPTY}
	}
	t.Cleanup(func() { renameCopy = renameNoReplace })
	if _, err := copyPlanned(plan, spec, filepath.Join(r.state, stagingDir), &skips{stderr: io.Discard}); err == nil {
		t.Fatal("the copy onto a place taken while it was built succeeded")
	}
	unlisted(t, spec, "a rename refused")
	if raw, _ := os.ReadFile(mine); string(raw) != "MINE" {
		t.Errorf("the entry put at the copy's place reads %q", raw)
	}
}

// A COPY NEVER REPLACES WHAT CAME TO ITS PLACE AFTER THE CHECK: the copy is
// moved by renameat2 with RENAME_NOREPLACE, so even an empty directory made
// at its place after the check of it (which a plain rename would replace) is
// left as it was, the copy refused as the home's skills holding its name, and
// its name off the list.
//
// Mutation: move it by a plain rename, and the copy replaces the entry. Not
// parallel: it stands in for renameCopy.
func TestACopyNeverReplacesWhatCameToItsPlace(t *testing.T) {
	r, plan, spec := plannedCopy(t)
	occupied := filepath.Join(t.TempDir(), "mine")
	if err := os.WriteFile(occupied, []byte("MINE"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := renameCopy(t.TempDir(), occupied); !errors.Is(err, syscall.EEXIST) {
		t.Errorf("the copy's move onto a file returned %v, want EEXIST, whatever the entry", err)
	}
	place := filepath.Join(r.skills, "late:s")
	renameCopy = func(from, to string) error {
		if err := os.MkdirAll(place, 0o700); err != nil {
			t.Fatal(err)
		}
		return renameNoReplace(from, to)
	}
	t.Cleanup(func() { renameCopy = renameNoReplace })
	_, err := copyPlanned(plan, spec, filepath.Join(r.state, stagingDir), &skips{stderr: io.Discard})
	if err == nil || err.Error() != "the home's skills hold late:s already" {
		t.Errorf("the copy onto a directory made after the check returned %v", err)
	}
	unlisted(t, spec, "a rename onto an entry")
	if entries, err := os.ReadDir(place); err != nil || len(entries) != 0 {
		t.Errorf("the directory made at the copy's place now holds %v (%v)", entries, err)
	}
}

// A COPY WHOSE FILE CHANGED SINCE ITS PLAN IS TAKEN OFF THE LIST: the build
// refuses a planned file no longer the one planned, and the name it listed
// before the build comes off, so nothing put at the place later in the run,
// the skill installed by hand among it, is removed at its end.
//
// Mutation: leave a failed copy listed, and this goes red.
func TestACopyWhoseFileChangedIsTakenOffTheList(t *testing.T) {
	t.Parallel()
	r, plan, spec := plannedCopy(t)
	data := filepath.Join(r.work, "late/.agents/skills/s/data")
	if err := os.WriteFile(data+".new", []byte("OTHER"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(data+".new", data); err != nil { // another file, as a checkout's pull would make
		t.Fatal(err)
	}
	if _, err := copyPlanned(plan, spec, filepath.Join(r.state, stagingDir), &skips{stderr: io.Discard}); !errors.Is(err,
		errChanged) {
		t.Errorf("the copy of a changed file returned %v, want errChanged", err)
	}
	unlisted(t, spec, "a changed file")
}

// countingReader serves an endless body of `a` lines after head, counting
// every byte it hands over.
type countingReader struct {
	head io.Reader
	read int
}

func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.head.Read(p)
	if errors.Is(err, io.EOF) {
		for i := range p {
			p[i] = "a\n"[i%2]
		}
		n, err = len(p), nil
	}
	c.read += n
	return n, err
}

// A SKILL'S NAME IS READ NO FURTHER THAN ITS FRONTMATTER'S CLOSE: a home of
// skills with long bodies costs a firing their frontmatters alone, not
// [maxFrontmatter] each, whether the frontmatter names the skill or not.
//
// Mutation: read the bound whole before parsing, or read on past the close,
// and the read passes one buffer.
func TestASkillsNameIsReadNoFurtherThanItsFrontmattersClose(t *testing.T) {
	t.Parallel()
	exact := func(l string) bool { return l == "---" }
	for _, front := range []string{"---\nname: x\n---\n", "---\ndescription: d\n---\n"} {
		r := &countingReader{head: strings.NewReader(front)}
		var name string
		named := readFrontmatter(r, exact, func(l string) bool {
			n, ok := strings.CutPrefix(l, "name: ")
			name = n
			return ok
		})
		if named != strings.Contains(front, "name:") || (named && name != "x") {
			t.Errorf("%q: named %v as %q", front, named, name)
		}
		if r.read > 4096 {
			t.Errorf("%q: %d bytes were read for its name, past one buffer of 4,096", front, r.read)
		}
	}
}

// A MUSE HOME IS READ AS MUSE CODE 1.4.25 LISTS IT (rows fn1-fn4): a skill
// directory directly in a home skill directory or the run's skills (Muse's rule
// walks them so), a dot one too (unmeasured, so not assumed unlisted), takes
// the name its frontmatter gives, read past a long body, or its directory's
// where that is empty; a directory holding no SKILL.md is no skill and takes no
// name, so the copies those hide are left out, said, and the rest are made.
//
// Mutation: skip dot directories in the rule's walk, take a directory
// without SKILL.md, or take an empty name, and this goes red.
func TestAMuseHomeIsReadAsMuseListsIt(t *testing.T) {
	t.Parallel()
	r := newRepoRun(t, nil)
	home := filepath.Join(t.TempDir(), "config-skills")
	long := strings.Repeat("body line\n", 8<<10)
	for dir, body := range map[string]string{
		".dot/SKILL.md":       "---\nname: late:dot\n---\n",
		"late:empty/SKILL.md": "---\nname: \"\"\n---\n",
		"long/SKILL.md":       "---\ndescription: d\nname: 'late:long'\n---\n" + long,
		"late:bare/x.md":      "not a skill\n",
	} {
		if err := os.MkdirAll(filepath.Join(home, filepath.Dir(dir)), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(home, dir), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	r.museLaunch(t, "", home)
	if err := os.MkdirAll(filepath.Join(r.skills, ".adot"), 0o700); err != nil { // ~/.agents/skills, the rule's walk
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(r.skills, ".adot", "SKILL.md"), []byte("---\nname: late:adot\n---\n"),
		0o600); err != nil {
		t.Fatal(err)
	}
	files := map[string]string{"late/.git/": ""}
	for _, s := range []string{"dot", "long", "bare", "empty", "adot"} {
		files["late/.agents/skills/"+s+"/SKILL.md"] = "---\nname: " + s + "\ndescription: D" + s + ".\n---\n"
	}
	r.add(t, files)
	got := museContext(t, r.museFire(t, "s", "s"))
	for _, s := range []string{"dot", "long", "empty", "adot"} {
		at := map[string]string{"dot": filepath.Join(home, ".dot"), "long": filepath.Join(home, "long"),
			"empty": filepath.Join(home, "late:empty"), "adot": filepath.Join(r.skills, ".adot")}[s]
		line := "the skill " + r.workdir(t) + "/late/.agents/skills/" + s + " is left out: the home's " + at +
			", which Muse Code lists as late:" + s + ", takes the name late:" + s + " it would list the copy under"
		if !strings.Contains(got, "- "+line) {
			t.Errorf("the answer is\n%s\nwant %q", got, line)
		}
	}
	if !strings.Contains(got, "- late:bare: Dbare.") {
		t.Errorf("the answer is\n%s\nwant late:bare listed: a directory without SKILL.md takes no name", got)
	}
}

// A MUSE HOME THE FIRING CANNOT READ LEAVES EVERY COPY OUT, SAID: a home skill
// directory past the walk's bound of 1,024 skills may hold any name, so no
// copy is made on a guess that it does not.
//
// Mutation: skip a home directory that cannot be read, and the copy is made.
func TestAMuseHomeTheFiringCannotReadLeavesEveryCopyOut(t *testing.T) {
	t.Parallel()
	r := newRepoRun(t, nil)
	home := filepath.Join(t.TempDir(), "config-skills")
	for i := range maxWalkSkills + 1 {
		dir := filepath.Join(home, fmt.Sprintf("s%04d", i))
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte("---\nname: n\n---\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	r.museLaunch(t, "", home)
	r.add(t, map[string]string{"late/.git/": "",
		"late/.agents/skills/deploy/SKILL.md": "---\nname: deploy\ndescription: Deploys.\n---\n"})
	got := museContext(t, r.museFire(t, "s", "s"))
	if want := "the home's skills under " + home + " could not be read for the names they take"; !strings.Contains(got, want) {
		t.Errorf("the answer is\n%s\nwant %q", got, want)
	}
	if _, err := os.Lstat(filepath.Join(r.skills, "late:deploy")); err == nil {
		t.Error("a copy was made beside a home the firing could not read")
	}
}

// A LAUNCH NAMING A LISTING RULE THE HELPER DOES NOT KNOW IS REFUSED, naming
// the field, rather than run as the bare rule, which checks no home skill.
//
// Mutation: accept any listedAs, and the launch runs.
func TestALaunchRefusesAListingRuleItDoesNotKnow(t *testing.T) {
	t.Parallel()
	r := newRepoRun(t, nil)
	spec := museSpec(r.skills)
	spec.ListedAs = "hermes"
	raw, _ := json.Marshal(launchConfig{State: r.state, Agents: r.agents, checkoutSpec: spec})
	config := filepath.Join(t.TempDir(), "launch.json")
	if err := os.WriteFile(config, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	err := checkoutLaunch(config, r.work, &bytes.Buffer{})
	if err == nil || !strings.Contains(err.Error(), "listedAs") {
		t.Fatalf("the launch with listedAs hermes returned %v, want it refused naming listedAs", err)
	}
}

// A MUSE HOME SKILL NAMED IN A FORM NO LINE SETTLES LEAVES EVERY COPY OUT,
// SAID: a block scalar's name may be any copy's, so none is made on a guess.
//
// Mutation: read the skill under its directory's name instead, and the copies
// are made.
func TestAMuseHomeSkillNamedUnplainlyLeavesEveryCopyOut(t *testing.T) {
	t.Parallel()
	r := newRepoRun(t, nil)
	home := filepath.Join(t.TempDir(), "config-skills")
	if err := os.MkdirAll(filepath.Join(home, "block"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, "block", "SKILL.md"), []byte("---\nname: |\n  late:deploy\n---\n"),
		0o600); err != nil {
		t.Fatal(err)
	}
	r.museLaunch(t, "", home)
	r.add(t, map[string]string{"late/.git/": "",
		"late/.agents/skills/deploy/SKILL.md": "---\nname: deploy\ndescription: Deploys.\n---\n",
		"late/.agents/skills/lint/SKILL.md":   "---\nname: lint\ndescription: Lints.\n---\n"})
	got := museContext(t, r.museFire(t, "s", "s"))
	want := "the home's " + filepath.Join(home, "block") + " names its skill in a form the helper does not read plainly"
	if strings.Count(got, want) != 2 {
		t.Errorf("the answer is\n%s\nwant both copies left out: %q", got, want)
	}
	for _, name := range []string{"late:deploy", "late:lint"} {
		if _, err := os.Lstat(filepath.Join(r.skills, name)); err == nil {
			t.Errorf("the copy %s was made beside a home skill whose name may be its", name)
		}
	}
}
