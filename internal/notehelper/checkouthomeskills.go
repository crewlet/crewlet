package main

import (
	"os"
	"path/filepath"
)

// A PERSON'S OWN SKILL IS NEVER HIDDEN BY A COPY: where a harness lists
// skills by a rule of its own (checkoutSpec.ListedAs), the home's skills are
// read once a firing, in the run's skills directory and the other user roots
// the harness lists beside it (checkoutSpec.HomeSkills), and a copy whose key
// one of them takes is left out, said. With no rule, a copy is left out only
// where the run's skills directory holds an entry of its name.

// listingRule is how a harness lists a skill, for the home check: harness,
// its name in what is said; walk, how the run's skills directory is walked
// for the names it holds, and loose where its loose `.md` files are skills
// too; key, the key a copy named name lists under; shown, the name a copy
// is listed by; takes, the name a home skill or command d lists as and the
// keys it takes, none where the harness lists it not.
type listingRule struct {
	harness string
	walk    skillWalkOptions
	loose   bool
	key     func(name string) string
	shown   func(name string) string
	takes   func(d skillDir) (listed string, keys []string)
}

// listingRules is each listing rule by its checkoutSpec.ListedAs.
var listingRules = map[string]listingRule{listedGrok: grokListing, listedKimi: kimiListing}

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
			listed, keys := rule.takes(d)
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
