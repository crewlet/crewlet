package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// piSkillDirs is Pi 1.6.0's skill directories, both walked.
var piSkillDirs = []skillDirSlot{{Dir: ".pi/skills", Recursive: true}, {Dir: ".agents/skills", Recursive: true}}

// skillNames is each skill directory's name and path below root.
func skillNames(t *testing.T, root string, dirs []skillDir) []string {
	t.Helper()
	var out []string
	for _, d := range dirs {
		rel, err := filepath.Rel(root, d.src)
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, d.name+"="+rel)
	}
	return out
}

// A RECURSIVE SKILL DIRECTORY IS WALKED AS PI WALKS IT (row walk, Pi 1.6.0):
// a directory holding SKILL.md is a skill and ends the descent (x/y is
// none), dot directories and node_modules are skipped, any other name walked
// (vendor/v), 9 levels deep reached; a link is followed inside the checkout
// and a skill reached twice is one, the first reached, across both
// directories; a link out of the checkout is not followed (Pi follows it:
// the documented gap).
//
// Mutation: descend past a SKILL.md, walk dot directories or node_modules,
// drop the dedupe, or follow a link out of the checkout, and the list goes
// red.
func TestARecursiveSkillDirectoryIsWalkedAsPiWalksIt(t *testing.T) {
	t.Parallel()
	root, outside := t.TempDir(), t.TempDir()
	skill := "---\nname: s\ndescription: S.\n---\n"
	writeTree(t, root, map[string]string{
		".pi/skills/x/SKILL.md":                        skill,
		".pi/skills/x/y/SKILL.md":                      skill,
		".pi/skills/.hidden/z/SKILL.md":                skill,
		".pi/skills/node_modules/n/SKILL.md":           skill,
		".pi/skills/vendor/v/SKILL.md":                 skill,
		".pi/skills/a/b/c/d/e/f/g/h/deep/SKILL.md":     skill,
		".pi/skills/empty/notes.md":                    "not a skill\n",
		".agents/skills/ag/SKILL.md":                   skill,
		".agents/skills/node_modules/m/SKILL.md":       skill,
		".agents/skills/.dot/SKILL.md":                 skill,
		".agents/skills/group/nested/inner/SKILL.md":   skill,
		".agents/skills/group/nested/inner/x/SKILL.md": skill,
	})
	writeTree(t, outside, map[string]string{"o/SKILL.md": skill})
	for link, target := range map[string]string{".pi/skills/lin": filepath.Join(root, ".pi/skills/x"),
		".pi/skills/lout": filepath.Join(outside, "o"), ".agents/skills/al": filepath.Join(root, ".pi/skills/vendor/v")} {
		if err := os.Symlink(target, filepath.Join(root, link)); err != nil {
			t.Fatal(err)
		}
	}
	dirs, over := checkoutSkillDirs(root, checkoutSpec{SkillDirs: piSkillDirs})
	want := []string{"deep=.pi/skills/a/b/c/d/e/f/g/h/deep", "lin=.pi/skills/lin", "v=.pi/skills/vendor/v",
		"ag=.agents/skills/ag", "inner=.agents/skills/group/nested/inner"}
	if got := skillNames(t, root, dirs); !slices.Equal(got, want) || len(over) > 0 {
		t.Errorf("the walk found %q (over %q), want %q", got, over, want)
	}
}

// A RECURSIVE SKILL DIRECTORY PAST A BOUND IS LEFT OUT WHOLE, said with the
// directory and the bound (a directory without a skill 9 below it, entered
// at 9 deep), and the directories beside it are walked.
//
// Mutation: drop the depth bound, or keep the skills found before it, and a
// check goes red.
func TestARecursiveSkillDirectoryPastABoundIsLeftOutWhole(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	skill := "---\nname: s\ndescription: S.\n---\n"
	writeTree(t, root, map[string]string{
		".pi/skills/a/SKILL.md":                   skill,
		".pi/skills/b/c/d/e/f/g/h/i/j/k/SKILL.md": skill,
		".agents/skills/kept/SKILL.md":            skill,
	})
	dirs, over := checkoutSkillDirs(root, checkoutSpec{SkillDirs: piSkillDirs})
	if got := skillNames(t, root, dirs); !slices.Equal(got, []string{"kept=.agents/skills/kept"}) {
		t.Errorf("the walk found %q, want .agents/skills/kept alone", got)
	}
	if len(over) != 1 || !strings.HasPrefix(over[0], "the skills of "+filepath.Join(root, ".pi/skills")+" are left out: ") ||
		!strings.Contains(over[0], "runs past the bound of 8 directories deep") {
		t.Errorf("the walk said %q, want .pi/skills left out past 8 directories deep", over)
	}
}

// A SKILL DIRECTORY KEEPS ITS SHAPE ON THE WIRE: a plain one is the string it
// always was, a recursive one the object the runner wrote.
//
// Mutation: write a plain directory as an object, or drop recursive, and the
// round trip goes red.
func TestASkillDirectoryKeepsItsShapeOnTheWire(t *testing.T) {
	t.Parallel()
	wire := `[".codex/skills",{"dir":".pi/skills","recursive":true}]`
	var dirs []skillDirSlot
	if err := json.Unmarshal([]byte(wire), &dirs); err != nil {
		t.Fatal(err)
	}
	if !dirs[1].Recursive || dirs[0].Recursive {
		t.Errorf("the directories read as %+v", dirs)
	}
	if again, err := json.Marshal(dirs); err != nil || string(again) != wire {
		t.Errorf("the directories read back as %s (%v), want %s", again, err, wire)
	}
}
