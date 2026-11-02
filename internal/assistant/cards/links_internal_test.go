package cards

import (
	"strings"
	"testing"
)

// A RECORD LINK NAMED BY NOTHING IS NAMED BY WHAT IT OPENS, never by the raw
// value its address carries: a seat's handle ("pm", which a two-letter handle
// made a near-empty button), a room's or a page's UUID is how the address
// spells the record, and "Open seat" is what a person reads. A task, a project
// or a space keeps its key, which people know.
//
// DERIVED FROM THE TABLE, every screen of it: one whose address takes an
// opaque value ({id}, {turn_id}, {handle} or {unit}) carries a noun and is
// named by it, unless its {id} is a key a person reads as well (a task's), and
// one that takes a key ({KEY}, {CONTAINER}) is named by the key. A screen the
// table gains is held the day it lands, never only once somebody lists it
// here. Mutation: drop the chat screen's noun, or fall back to the first path
// value again, and this fails.
func TestARecordLinkIsNamedByWhatItOpens(t *testing.T) {
	t.Parallel()
	const uuid = "5f0c5c8e-1a2b-4c3d-8e9f-0a1b2c3d4e5f"
	opaque := map[string]string{"{id}": uuid, "{turn_id}": uuid, "{handle}": "pm",
		"{unit}": "eng"}
	keys := map[string]string{"{KEY}": "FAIR", "{CONTAINER}": "HOME"}
	walked := 0
	for name, s := range links {
		params, key, takesOpaque := map[string]string{}, "", false
		for _, segment := range strings.Split(s.address, "/") {
			p, ok := placeholders[segment]
			if !ok {
				continue
			}
			switch v, keyed := keys[segment]; {
			case segment == "{id}" && s.id != nil:
				// A KEY AS WELL AS AN ID, as a task's {id} is: named by the
				// key a person reads.
				params[p.param], key = "FAIR-10", "FAIR-10"
			case keyed:
				params[p.param], key = v, v
			default:
				params[p.param], takesOpaque = opaque[segment], true
			}
		}
		if len(params) == 0 {
			continue
		}
		walked++
		link, err := Link(name, params, "")
		switch {
		case err != nil:
			t.Errorf("%s: %v", name, err)
		case takesOpaque && (s.noun == "" || link.Label != "Open "+s.noun):
			t.Errorf("a %s link with no name reads %q: its address takes a value nobody "+
				"reads, so the screen names its record by a noun", name, link.Label)
		case !takesOpaque && link.Label != key:
			t.Errorf("a %s link with no name reads %q, want its key %q", name, link.Label, key)
		}
	}
	if walked < 8 {
		t.Fatalf("walked %d record screens; the link table is not being read", walked)
	}
}
