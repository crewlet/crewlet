package setup

import (
	"fmt"
	"strings"
)

// Set is a list a form builds one entry at a time, at least Min entries:
// App Store Connect's API keys, one per Apple role, each a key ID and a .p8.
//
// A KIND OF REQUIREMENT, NOT Required ON EACH FIELD: "at least one of these
// eight keys" is true of the list and false of every key in it, so a static
// flag demands all eight or calls each optional, and the form folded sixteen
// "(optional)" boxes over a block config refused without one. Every member
// carries the set ([Requirement.Set]); [Requirement.Needed], [Outstanding] and
// [CheckSets] read it. An entry is the members whose Entry.Value agree, and an
// entry with any field answered is started and must be finished.
type Set struct {
	// ID names the set within its requirement list.
	ID string `json:"id"`
	// Label heads the list ("API keys").
	Label string `json:"label"`
	// Item is what one entry is called in a sentence ("key").
	Item string `json:"item"`
	// Hint is the line under the label, on the form and in what a person has
	// ready before connecting: what an entry holds, how many, and which first.
	Hint string `json:"hint"`
	// Add is the control that adds an entry ("Add a key").
	Add string `json:"add"`
	// Pick labels the choice of which entry to add ("Role").
	Pick string `json:"pick"`
	// Missing is the refusal of fewer than Min entries, in the operator's words
	// ("Add at least one API key."), for a caller with no form: a form says its
	// Add control beside the Hint instead, so the listing does not send it.
	Missing string `json:"-"`
	// Min is how many entries the integration cannot work with fewer than.
	Min int `json:"min"`
	// Entry is the entry this requirement is a field of: its value, the
	// label it is picked by, and what it unlocks.
	Entry Choice `json:"entry"`
}

// entryOf is r's set and entry, "" outside a set.
func (r Requirement) entryOf() string {
	if r.Set == nil {
		return ""
	}
	return r.Set.ID + "\x00" + r.Set.Entry.Value
}

// started reports whether any field of r's entry is answered, by answer.
func started(r Requirement, among []Requirement, answer func(Requirement) string) bool {
	for _, other := range among {
		if other.entryOf() == r.entryOf() && answer(other) != "" {
			return true
		}
	}
	return false
}

// stored is a requirement's answer as the document holds it.
func stored(r Requirement) string { return strings.TrimSpace(r.Stored) }

// SetProblem is a refusal of a set the answers leave unfinished: fewer than Min
// entries, or an entry with a field left empty. Field and ConfigPath locate it
// at the first field it is about, Set at the set.
type SetProblem struct {
	Set        string
	Field      string
	ConfigPath string
	Problem    string
}

func (e *SetProblem) Error() string { return "setup: " + e.ConfigPath + ": " + e.Problem }

// CheckSets refuses answers that leave a set unfinished, before anything is
// written: values are what a submission sends, by field, and a field it does
// not send keeps what the document holds. Nil when every set has at least its
// Min entries and each started entry is whole.
func CheckSets(reqs []Requirement, values map[string]string) *SetProblem {
	answer := func(r Requirement) string {
		if v, sent := values[r.Field]; sent && strings.TrimSpace(v) != "" {
			return strings.TrimSpace(v)
		}
		return stored(r)
	}
	type tally struct {
		set     *Set
		first   Requirement
		entries map[string]bool
	}
	var order []string
	sets := map[string]*tally{}
	for _, r := range reqs {
		if r.Set == nil {
			continue
		}
		t := sets[r.Set.ID]
		if t == nil {
			t = &tally{set: r.Set, first: r, entries: map[string]bool{}}
			sets[r.Set.ID] = t
			order = append(order, r.Set.ID)
		}
		if !started(r, reqs, answer) {
			continue
		}
		t.entries[r.Set.Entry.Value] = true
		if answer(r) == "" {
			return &SetProblem{Set: r.Set.ID, Field: r.Field, ConfigPath: r.ConfigPath,
				Problem: fmt.Sprintf("The %s %s needs its %s too.", r.Set.Entry.Label, r.Set.Item, r.Noun())}
		}
	}
	for _, id := range order {
		if t := sets[id]; len(t.entries) < t.set.Min {
			return &SetProblem{Set: id, Field: t.first.Field, ConfigPath: t.first.ConfigPath, Problem: t.set.Missing}
		}
	}
	return nil
}
