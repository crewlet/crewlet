package engine

import (
	"slices"
	"testing"

	"github.com/crewlet/crewlet/internal/chat"
	"github.com/crewlet/crewlet/internal/org"
)

// WHO BELONGS IN A UNIT'S ROOM, as arithmetic over the chart.
//
// Separated from the reconcile that writes it, for the reason internal/tracker
// and internal/textindex separate theirs: a rule exercised only through a
// database and a broker is a rule nobody re-reads, and this one decides two
// things that are easy to get quietly wrong — who is IN the room, and which of
// them every message wakes.
func chartFor(t *testing.T) *org.Organization {
	t.Helper()
	eng := &org.Unit{
		Name: "engineering", Lead: "dana", Channel: "eng",
		Roles: []*org.Role{
			{Name: "dana", Kind: org.KindAgent},
			{Name: "erin", Kind: org.KindHuman},
		},
		Children: []*org.Unit{{
			Name: "platform", Channel: "platform",
			Roles: []*org.Role{{Name: "frank", Kind: org.KindAgent}},
		}},
	}
	o := &org.Organization{Name: "acme", Units: []*org.Unit{eng}}
	o.Normalize()
	return o
}

func handlesOf(members []chat.Member) []string {
	out := make([]string, 0, len(members))
	for _, m := range members {
		out = append(out, m.Handle)
	}
	return out
}

// THE SUBTREE IS IN THE ROOM, not just the direct members.
//
// A division's room is where its teams are reachable. The other reading leaves
// a lead talking to an empty room while the people doing the work sit one
// level down.
func TestAUnitsRoomHoldsItsWholeSubtree(t *testing.T) {
	t.Parallel()
	chart := chartFor(t)
	got := handlesOf(unitRoomMembers(chart, chart.Unit("engineering")))
	want := []string{"dana", "erin", "frank"}
	if !slices.Equal(got, want) {
		t.Errorf("engineering's room holds %v, want %v — a descendant's seat "+
			"is unreachable in the room its division talks in", got, want)
	}
}

// AND EVERY MESSAGE WAKES THIS UNIT'S OWN AGENTS AND NOBODY ELSE.
//
// THE THREE CASES ARE ONE RULE AND HAVE TO BE ASSERTED TOGETHER:
//
//   - this unit's own AGENT seat follows everything, because the room is where
//     it is addressed;
//   - a DESCENDANT's agent does not, because it has its own room and a message
//     in a division's room would otherwise be a turn for every agent beneath
//     it, against the node's whole concurrency budget;
//   - a HUMAN never does, because a person is addressable and runs no turn.
//
// Asserting only the first would pass against a build that woke the entire
// subtree, which is the expensive failure.
func TestOnlyTheUnitsOwnAgentsFollowEverything(t *testing.T) {
	t.Parallel()
	chart := chartFor(t)
	follow := map[string]bool{}
	for _, m := range unitRoomMembers(chart, chart.Unit("engineering")) {
		follow[m.Handle] = m.FollowAll
	}
	for handle, want := range map[string]bool{
		"dana":  true,  // this unit's own agent, and its lead
		"erin":  false, // a person: addressable, never woken
		"frank": false, // a descendant's agent: its own room wakes it
	} {
		if got := follow[handle]; got != want {
			t.Errorf("%s follows every message = %v, want %v", handle, got, want)
		}
	}
}

// A LEAD WHO IS NOT IN THE SUBTREE IS STILL IN THE ROOM.
//
// The lead answers for the unit — the chat routing's own fallback resolves
// through it — so a room it could not read would route a person's question to
// somebody who never sees it.
func TestTheLeadIsInTheRoomEvenFromOutsideTheSubtree(t *testing.T) {
	t.Parallel()
	o := &org.Organization{
		Name:  "acme",
		Roles: []*org.Role{{Name: "gita", Kind: org.KindAgent}},
		Units: []*org.Unit{{
			Name: "support", Lead: "gita", Channel: "support",
			Roles: []*org.Role{{Name: "hugo", Kind: org.KindAgent}},
		}},
	}
	o.Normalize()
	got := handlesOf(unitRoomMembers(o, o.Unit("support")))
	if !slices.Contains(got, "gita") {
		t.Errorf("support's room holds %v, without the lead that answers for "+
			"it — the routing's unit fallback resolves to somebody who cannot "+
			"read the room", got)
	}
}

// A UNIT WITH NOBODY IN IT GETS NO MEMBERSHIP, and therefore no room.
//
// This kind refuses a join, so an empty unit room is one nothing could ever
// reach — and creating it would claim the name against the day the unit is
// actually staffed.
func TestAnEmptyUnitYieldsNoMembers(t *testing.T) {
	t.Parallel()
	o := &org.Organization{
		Name:  "acme",
		Units: []*org.Unit{{Name: "ghost", Channel: "ghost"}},
	}
	o.Normalize()
	if got := unitRoomMembers(o, o.Unit("ghost")); len(got) != 0 {
		t.Errorf("an unstaffed unit yielded %v", handlesOf(got))
	}
}

// A ROOM'S STORED UNIT IS MATCHED BY EITHER SPELLING.
//
// [org.Unit.Key] is the id when a unit has one and the name otherwise, so a
// company that ADDS ids to units it already had would otherwise have every
// room read as somebody else's on the next apply — and be refused as a
// takeover it must not perform.
func TestAUnitIsRecognisedByIDOrName(t *testing.T) {
	t.Parallel()
	unit := &org.Unit{Name: "engineering", ID: "u-7"}
	if !sameUnit("u-7", unit) {
		t.Error("a room stamped with the unit's id was not recognised")
	}
	if !sameUnit("engineering", unit) {
		t.Error("a room stamped before the unit had an id was not recognised — " +
			"adding ids would orphan every existing unit room")
	}
	if sameUnit("marketing", unit) {
		t.Error("another unit's room was claimed")
	}
}
