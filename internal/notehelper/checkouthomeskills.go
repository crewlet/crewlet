package main

import (
	"bufio"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// A PERSON'S OWN SKILL IS NEVER HIDDEN BY A COPY: where a harness lists
// skills by a rule of its own (checkoutSpec.ListedAs), the home's skills are
// read once a firing, in the run's skills directory and the other user roots
// the harness lists beside it (checkoutSpec.HomeSkills), and a copy whose key
// one of them takes is left out, said; each SKILL.md is read only to its
// frontmatter's close, and a directory that cannot be read, or a name the
// rule cannot read plainly, leaves out every copy, as it may hold the name.
// With no rule, a copy is left out only where the run's skills directory
// holds an entry of its name.

// listingRule is how a harness lists a skill, for the home check: harness,
// its name in what is said; walk, how the run's skills directory is walked
// for the names it holds, and loose where its loose `.md` files are skills
// too; key, the key a copy named name lists under; shown, the name a copy
// is listed by; takes, the name a home skill or command d lists as and the
// keys it takes, none where the harness lists it not, and whether its name
// reads plainly (where not, it may take any).
type listingRule struct {
	harness string
	walk    skillWalkOptions
	loose   bool
	key     func(name string) string
	shown   func(name string) string
	takes   func(d skillDir) (listed string, keys []string, plain bool)
}

// listingRules is each listing rule by its checkoutSpec.ListedAs.
var listingRules = map[string]listingRule{listedGrok: grokListing, listedKimi: kimiListing, listedMuse: museListing}

// homeHolds is why a copy named name may not be made, "" where it may: by
// the spec's listing rule, the home's skills or a copy this firing made
// take its key ([homeNames]); with none, the run's skills hold an entry of
// that name.
func homeHolds(spec checkoutSpec, name string) string {
	rule, ruled := listingRules[spec.ListedAs]
	if !ruled {
		if _, err := os.Lstat(filepath.Join(spec.Skills, name)); err == nil {
			return "the home's skills hold " + name + " already"
		}
		return ""
	}
	held := spec.held
	if held == nil {
		held = &homeNames{}
	}
	return held.holds(spec, rule, rule.key(name))
}

// homeNames is the keys the home's skills take by a listing rule, read once
// a firing ([homeNames.readHome]), each with what takes it, and those of the
// copies the firing made since, added as each lands.
type homeNames struct {
	read   bool
	unread string
	taken  map[string]string
}

// holds is why the key want is taken, "" where it is free.
func (h *homeNames) holds(spec checkoutSpec, rule listingRule, want string) string {
	if !h.read {
		h.read, h.taken = true, map[string]string{}
		h.unread = h.readHome(spec, rule)
	}
	if h.unread != "" { // unread, the home may hold the name: the copy waits on no guess
		return h.unread
	}
	return h.taken[want]
}

// add records a copy the firing made, at path, under key.
func (h *homeNames) add(key, path string) {
	if h.taken != nil {
		h.taken[key] = "the run's copy " + path + " takes the name " + key + " already"
	}
}

// readHome walks the run's skills and the home's other skill directories its
// harness lists beside them (spec.HomeSkills), a link followed wherever it
// leads, as a harness follows a home's, and records the keys each one takes
// by rule; or is why it could not.
func (h *homeNames) readHome(spec checkoutSpec, rule listingRule) string {
	dirs := []skillDirSlot{{Dir: spec.Skills, Recursive: true, skillWalkOptions: rule.walk}}
	if rule.loose {
		dirs = append(dirs, skillDirSlot{Dir: spec.Skills, skillWalkOptions: skillWalkOptions{Commands: true}})
	}
	for _, slot := range append(dirs, spec.HomeSkills...) {
		var found []skillDir
		var err error
		if slot.Commands {
			found, err = commandSkills("", slot.Dir)
		} else {
			found, err = walkSkills("", slot.Dir, map[string]bool{}, slot.skillWalkOptions)
		}
		if err != nil {
			return "the home's skills under " + slot.Dir + " could not be read for the names they take: " + err.Error()
		}
		for _, d := range found {
			listed, keys, plain := rule.takes(d)
			if !plain {
				return "the home's " + d.src + " names its skill in a form the helper does not read plainly, so " +
					rule.harness + " may list it under any copy's name"
			}
			for _, key := range keys {
				if _, ok := h.taken[key]; key != "" && !ok {
					h.taken[key] = "the home's " + d.src + ", which " + rule.harness + " lists as " + listed +
						", takes the name " + key + " it would list the copy under"
				}
			}
		}
	}
	return ""
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
