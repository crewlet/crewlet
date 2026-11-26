package main

import (
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
// its own name rewritten where free, else is not listed. Each kept is given
// the name it lists under (listed), which its copy is named after.
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
	as := make([]string, len(dirs))
	for i, e := range entries {
		if e.owns && !taken[e.listed] {
			taken[e.listed], as[i] = true, e.listed
		}
	}
	for i, e := range entries {
		if !e.ok || e.owns {
			continue
		}
		for _, name := range []string{e.listed, e.own} {
			if name != "" && len(name) <= skillname.GrokMax && !taken[name] {
				taken[name], as[i] = true, name
				break
			}
		}
	}
	var out []skillDir
	for i, d := range dirs {
		if as[i] != "" {
			d.listed = as[i]
			out = append(out, d)
		}
	}
	return out
}

// grokListing is Grok Build 1.0.92's listing rule ([listingRule]): a copy
// is listed and keyed by its name rewritten ([skillname.Grok]); a home skill
// or command takes its listed name and its own name rewritten. Grok drops the
// copy behind such a home skill, whichever was written first, or keeps the
// name and lists the person's skill by another (measured, section 11's rule),
// so the copy is left out. A home walked as Grok walks a skills directory.
var grokListing = listingRule{harness: "Grok Build", walk: grokHomeWalk, key: skillname.Grok, shown: skillname.Grok,
	takes: func(d skillDir) (string, []string) {
		n, named := frontName(entryFile(d))
		listed, _ := skillname.GrokListed(n, named, d.name)
		return listed, []string{listed, skillname.Grok(d.name)}
	}}

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
