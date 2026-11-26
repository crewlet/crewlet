package main

import (
	"errors"
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
	for _, listedAs := range []string{"", listedGrok, listedKimi, listedOpenCode} {
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
	for _, listedAs := range []string{"", listedGrok, listedKimi, listedOpenCode} {
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
	occupied := t.TempDir()
	if err := renameCopy(t.TempDir(), occupied); !errors.Is(err, os.ErrExist) {
		t.Errorf("the copy's move onto an empty directory returned %v, want EEXIST", err)
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
