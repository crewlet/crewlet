package main

import (
	"io"
	"os"
	"path/filepath"
	"strings"
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

// A COPY'S TAKEN PLACE IS NEVER LISTED AS THE RUN'S: the run's end removes
// every name its copied list holds, so an entry already at the copy's place,
// of any kind and under any listing rule, refuses the copy before it is
// listed, said as the home's skills holding the name, and the entry is as it
// was.
//
// Mutation: list the copy before checking its place (drop copyPlanned's
// check), and the name is listed.
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
			if copied, _ := os.ReadFile(spec.Copied); strings.Contains(string(copied), "late:s") {
				t.Errorf("listedAs %q, %s: the person's entry was listed for removal: %q", listedAs, kind, copied)
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
