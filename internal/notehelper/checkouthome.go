package main

import (
	"bufio"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/crewlet/crewlet/internal/skillname"
)

// THE HOME'S SKILL NAMES (checkoutSpec.HomeSkills, ListedAs): a copy is left
// out, said, where a skill its harness lists beside it takes the name the
// copy lists under ([homeHolds]). The home's skill directories are walked as
// a checkout's are (a plain one: the skills directly in it, dot ones too),
// read once a firing, each SKILL.md only to its frontmatter's close, the
// firing's copies added as each lands; a directory that cannot be read, or a
// name that cannot be read plainly ([museFrontName]), leaves out every copy,
// as it may hold the name. Muse Code 1.4.25 lists a skill under its
// frontmatter's name, else its directory's, and no two of one name
// (measured, rows fn1-fn4, so1-so6), so even where the copy would win (over
// ~/.claude/skills and ~/.codex/skills) the person's keeps its name.

// skillListing is a harness's rule for the name it lists a skill under
// ([checkoutSpec.ListedAs]): "" checks the run's skills directory alone.
type skillListing string

const (
	// listedGrok lists a skill as Grok Build 1.0.92 does ([grokSessionSkills]).
	listedGrok skillListing = "grok"
	// listedMuse lists a skill as Muse Code 1.4.25 does: by its
	// frontmatter's name, else its directory's ([museFrontName]).
	listedMuse skillListing = "muse"
)

// Valid reports whether l is a rule the helper knows.
func (l skillListing) Valid() bool { return l == "" || l == listedGrok || l == listedMuse }

// key is the name a copy named name is listed and compared under.
func (l skillListing) key(name string) string {
	if l == listedGrok {
		return skillname.Grok(name)
	}
	return name
}

// homeTake is a name a home skill takes, and why a copy may not take it; any,
// every name, for a skill whose name cannot be read plainly.
type homeTake struct {
	key, why string
	any      bool
}

// takes is the names the home's skill d takes under l. Grok Build: its
// listed name and its own name rewritten. Muse Code: the name it lists under,
// as of two skills of one name it lists the higher place's alone, or, both in
// one directory, neither (measured, rows fn1-fn4, so1-so6), so the person's
// keeps its name and the copy is left out.
func (l skillListing) takes(d skillDir) []homeTake {
	if l == listedMuse {
		names, plain := museFrontName(entryFile(d))
		if !plain {
			return []homeTake{{why: "the home's " + d.src + " names its skill in a form the helper does not read " +
				"plainly, so Muse Code may list it under any copy's name", any: true}}
		}
		var out []homeTake
		for _, listed := range names {
			if listed == "" {
				listed = d.name
			}
			out = append(out, homeTake{key: listed, why: "the home's " + d.src + " is listed as " + listed +
				" too, and Muse Code would not list both"})
		}
		return out
	}
	n, named := frontName(entryFile(d))
	listed, _ := skillname.GrokListed(n, named, d.name)
	var out []homeTake
	for _, key := range []string{listed, skillname.Grok(d.name)} {
		out = append(out, homeTake{key: key, why: "the home's " + d.src + ", which Grok Build lists as " + listed +
			", takes the name " + key + " it would list the copy under"})
	}
	return out
}

// homeHolds is why a copy named name may not be made, "" where it may: the
// run's skills hold an entry of that name, which the copy would not replace
// (Grok's walk of them reads it), or, under spec's listing rule, a skill of
// the home's ([homeNames]) takes the name the copy lists under.
func homeHolds(spec checkoutSpec, name string) string {
	if spec.ListedAs != listedGrok {
		if _, err := os.Lstat(filepath.Join(spec.Skills, name)); err == nil {
			return "the home's skills hold " + name + " already"
		}
		if spec.ListedAs == "" {
			return ""
		}
	}
	held := spec.held
	if held == nil {
		held = &homeNames{}
	}
	return held.holds(spec, spec.ListedAs.key(name))
}

// homeNames is the names the home's skills take as their harness lists them,
// read once a firing ([homeNames.readHome]), each with what takes it, and the
// names of the copies the firing made since, added as each lands.
type homeNames struct {
	read   bool
	unread string
	taken  map[string]string
}

// holds is why the name want is taken, "" where it is free.
func (h *homeNames) holds(spec checkoutSpec, want string) string {
	if !h.read {
		h.read, h.taken = true, map[string]string{}
		h.unread = h.readHome(spec)
	}
	if h.unread != "" { // unread, the home may hold the name: the copy waits on no guess
		return h.unread
	}
	return h.taken[want]
}

// add records a copy the firing made, at path, listed as listed.
func (h *homeNames) add(listed, path string) {
	if h.taken != nil {
		h.taken[listed] = "the run's copy " + path + " takes the name " + listed + " already"
	}
}

// readHome walks the home's skill directories its harness lists beside the
// run's (spec.HomeSkills; for Grok, the run's skills first, which it walks
// below a skill), a link followed wherever it leads, as a harness follows a
// home's, and records the names each skill takes ([skillListing.takes]); or
// is why it could not.
func (h *homeNames) readHome(spec checkoutSpec) string {
	dirs := spec.HomeSkills
	if spec.ListedAs == listedGrok {
		dirs = append([]skillDirSlot{{Dir: spec.Skills, Recursive: true, skillWalkOptions: grokHomeWalk}}, dirs...)
	}
	for _, slot := range dirs {
		found, err := homeSlotSkills(slot)
		if err != nil {
			return "the home's skills under " + slot.Dir + " could not be read for the names they take: " + err.Error()
		}
		for _, d := range found {
			for _, t := range spec.ListedAs.takes(d) {
				if t.any {
					return t.why
				}
				if _, ok := h.taken[t.key]; t.key != "" && !ok {
					h.taken[t.key] = t.why
				}
			}
		}
	}
	return ""
}

// homeSlotSkills is the skills of a home skill directory: a recursive one's
// walk, a commands one's files, and a plain one's skill directories directly
// in it, dot ones too (Muse Code's; its home's dot directories unmeasured,
// so none is assumed unlisted), each within the walk's bounds.
func homeSlotSkills(slot skillDirSlot) ([]skillDir, error) {
	switch {
	case slot.Commands:
		return commandSkills("", slot.Dir)
	case slot.Recursive:
		return walkSkills("", slot.Dir, map[string]bool{}, slot.skillWalkOptions)
	}
	return walkSkills("", slot.Dir, map[string]bool{}, skillWalkOptions{Every: true, Depth: 1})
}

// entryFile is the file a skill's name is read from: a command itself, a
// skill directory's SKILL.md.
func entryFile(d skillDir) string {
	if d.command {
		return d.src
	}
	return filepath.Join(d.src, "SKILL.md")
}

// maxFrontmatter bounds the bytes of a SKILL.md read for its name alone: a
// real frontmatter is a few lines. One longer reads as naming nothing, so the
// skill is named by its directory.
const maxFrontmatter = 64 << 10

// scanFrontmatter reads the file at path a line at a time ([readFrontmatter]).
func scanFrontmatter(path string, fence, line func(string) bool) bool {
	file, err := os.Open(path) //nolint:gosec // a skill's file the harness itself would read
	if err != nil {
		return false
	}
	defer func() { _ = file.Close() }()
	return readFrontmatter(file, fence, line)
}

// readFrontmatter reads r a line at a time, a trailing `\r` trimmed, no
// further than its frontmatter's closing line nor past [maxFrontmatter]: a
// first line fence accepts opens it, the next closes it, and each line
// between is handed to line, which ends the read by returning true. It
// reports whether line did, false where r has no frontmatter or fails.
func readFrontmatter(r io.Reader, fence, line func(string) bool) bool {
	br := bufio.NewReader(io.LimitReader(r, maxFrontmatter))
	for first := true; ; first = false {
		raw, err := br.ReadString('\n')
		if err != nil && !errors.Is(err, io.EOF) {
			return false
		}
		l := strings.TrimSuffix(strings.TrimSuffix(raw, "\n"), "\r")
		switch {
		case first && !fence(l), !first && fence(l):
			return false
		case !first && line(l):
			return true
		case err != nil:
			return false
		}
	}
}
