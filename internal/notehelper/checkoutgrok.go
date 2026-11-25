package main

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/crewlet/crewlet/internal/skillname"
)

// SKILLS AS GROK BUILD LISTS THEM (checkoutSpec.ListedAs "grok"): Grok Build
// 1.0.92 lists a skill under [skillname.GrokListed] of its frontmatter name,
// its directory's name (a command's file less `.md`) where that fails, and
// settles two that list alike by its own rule, so a copy is named, checked
// against the home and listed by the name Grok gives it, never the one
// written. The package doc's "Checkouts below the working directory" has the
// rule and the rows it rests on.

// listedGrok is the [checkoutSpec.ListedAs] of a harness that lists skills as
// Grok Build 1.0.92 does.
const listedGrok = "grok"

// maxFrontmatter bounds the bytes of a SKILL.md read for its name alone: a
// real frontmatter is a few lines. One longer reads as naming nothing, so the
// skill is named by its directory, as Grok names one whose name fails.
const maxFrontmatter = 64 << 10

// frontName is the `name` a SKILL.md's frontmatter gives, and whether it has
// one: the frontmatter's lines between a first line `---` and the next, the
// first `name:` key's value, its quotes trimmed. A file opening with no
// frontmatter, or one past [maxFrontmatter], names nothing.
func frontName(path string) (string, bool) {
	raw, err := readBounded(path, maxFrontmatter)
	if err != nil {
		return "", false
	}
	lines := strings.Split(string(raw), "\n")
	if strings.TrimSpace(lines[0]) != "---" {
		return "", false
	}
	for _, line := range lines[1:] {
		l := strings.TrimSuffix(line, "\r")
		if strings.TrimSpace(l) == "---" {
			return "", false
		}
		key, ok := frontKey(l)
		if !ok || key != "name" {
			continue
		}
		_, value, _ := strings.Cut(l, ":")
		value = strings.TrimSpace(value)
		if unq, err := strconv.Unquote(value); err == nil && strings.HasPrefix(value, `"`) {
			return unq, true
		}
		if len(value) >= 2 && value[0] == '\'' && value[len(value)-1] == '\'' {
			return strings.ReplaceAll(value[1:len(value)-1], "''", "'"), true
		}
		return value, true
	}
	return "", false
}

// entryFile is the file a skill's name is read from: a command itself, a
// skill directory's SKILL.md.
func entryFile(d skillDir) string {
	if d.command {
		return d.src
	}
	return filepath.Join(d.src, "SKILL.md")
}

// grokSessionSkills is those of dirs, in their order (the checkout's skill
// directories in Grok's order, each walked), that a Grok Build session started
// in the checkout lists, by Grok 1.0.92's rule (measured, rows x1-x7 and g4x):
// each listed as [skillname.GrokListed], none where that fails; a skill whose
// own name (its directory's, a command's file's) rewrites to that name, and a
// command (ranked by place, as the .grok command named like a .claude skill
// was), takes it first, in order; then each other takes it where free, else
// its own name rewritten where free, else is not listed.
func grokSessionSkills(dirs []skillDir) []skillDir {
	type entry struct {
		listed, own string
		owns, ok    bool
	}
	entries := make([]entry, len(dirs))
	for i, d := range dirs {
		name, named := frontName(entryFile(d))
		listed, ok := skillname.GrokListed(name, named, d.name)
		own := skillname.Grok(d.name)
		entries[i] = entry{listed: listed, own: own, owns: ok && (d.command || own == listed), ok: ok}
	}
	taken := map[string]bool{}
	keep := make([]bool, len(dirs))
	for i, e := range entries {
		if e.owns && !taken[e.listed] {
			taken[e.listed], keep[i] = true, true
		}
	}
	for i, e := range entries {
		if !e.ok || e.owns {
			continue
		}
		for _, name := range []string{e.listed, e.own} {
			if name != "" && len(name) <= skillname.GrokMax && !taken[name] {
				taken[name], keep[i] = true, true
				break
			}
		}
	}
	var out []skillDir
	for i, d := range dirs {
		if keep[i] {
			out = append(out, d)
		}
	}
	return out
}

// homeHolds is why a copy named name may not be made, "" where it may: the
// run's skills hold an entry of that name; or, listed as Grok Build lists,
// a skill or command of the run's skills or of the home's other skill
// directories its harness lists beside them (spec.HomeSkills) has a listed
// name or own name that rewrites as the copy's does, a link followed
// wherever it leads, as Grok follows a home's. Grok 1.0.92 drops the copy
// behind such a home skill, whichever was written first, or keeps the name
// and lists the person's skill by another (measured, section 11's rule), so
// the person's skill keeps its name and the copy is left out, said.
func homeHolds(spec checkoutSpec, name string) string {
	if spec.ListedAs != listedGrok {
		if _, err := os.Lstat(filepath.Join(spec.Skills, name)); err == nil {
			return "the home's skills hold " + name + " already"
		}
		return ""
	}
	want := skillname.Grok(name)
	dirs := append([]skillDirSlot{{Dir: spec.Skills, Recursive: true, skillWalkOptions: grokHomeWalk}}, spec.HomeSkills...)
	for _, slot := range dirs {
		var found []skillDir
		var err error
		if slot.Commands {
			found, err = commandSkills("", slot.Dir)
		} else {
			found, err = walkSkills("", slot.Dir, map[string]bool{}, slot.skillWalkOptions)
		}
		if err != nil { // unread, the home may hold the name: the copy waits on no guess
			return "the home's skills under " + slot.Dir + " could not be read for the names they take: " + err.Error()
		}
		for _, d := range found {
			n, named := frontName(entryFile(d))
			listed, _ := skillname.GrokListed(n, named, d.name)
			if listed == want || skillname.Grok(d.name) == want {
				return "the home's " + d.src + ", which Grok Build lists as " + listed + ", takes the name " + want +
					" it would list the copy under"
			}
		}
	}
	return ""
}

// grokHomeWalk is how the run's skills directory is walked for the names it
// holds, as Grok Build 1.0.92 walks a skills directory (measured in a
// checkout's, rows g4s and g4x): below a skill, dot directories and
// node_modules too, 6 directories deep.
var grokHomeWalk = skillWalkOptions{Through: true, Every: true, Depth: 6}

// bareSkill is a SKILL.md or command that opens with no frontmatter as its
// copy: a frontmatter naming it name, then the file whole. Grok Build 1.0.92
// describes a skill whose frontmatter has no description by its body's first
// paragraph, as it does a bare one (measured, row g4y), so the copy lists
// with the description the original would.
func bareSkill(raw []byte, name string) []byte {
	return []byte("---\nname: \"" + name + "\"\n---\n" + string(raw))
}
