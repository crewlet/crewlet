package tracker_test

import (
	"slices"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/tracker"
)

// renamedChart is a chart that resolves a unit by every spelling the org chart
// answers to — its key, the key it was created under, a former key and its
// name — which is what `org.Organization.UnitByRef` does and what the engine's
// seam hands the tracker.
type renamedChart []tracker.ChartUnit

func (c renamedChart) AllUnits() []tracker.ChartUnit { return c }

func (c renamedChart) ResolveUnit(ref string) (tracker.ChartUnit, bool) {
	ref = strings.ToLower(strings.TrimSpace(ref))
	for _, unit := range c {
		spellings := append([]string{unit.Key, unit.OriginKey, unit.Name}, unit.FormerKeys...)
		for _, s := range spellings {
			if s != "" && strings.ToLower(s) == ref {
				return unit, true
			}
		}
	}
	return tracker.ChartUnit{}, false
}

// platformRenamed is one team created as `plat`, renamed to `infra` and then to
// `platform` — so its rows hold three keys, none of which is its name — beside
// a team never renamed.
var platformRenamed = renamedChart{
	{Key: "platform", Name: "Platform", OriginKey: "plat",
		FormerKeys: []string{"infra", "plat"}},
	{Key: "product", Name: "Product"},
}

// A RENAMED TEAM'S WORK IS STILL ITS WORK, under every key it answered to.
//
// A unit's key is an ADDRESS in the org chart, and a rename moves it while the
// key the unit was created under and every key it has held since go on
// resolving to it. A task's filed unit is a record of what was true and nothing
// rewrites it — so everything the team filed before a rename holds a key it no
// longer has. A filter that matched only the unit's current key and its name
// answered with the work filed since the last rename and said nothing about the
// rest, which reads exactly like a team that has done less; and a board drew
// the team once per key it had ever held.
func TestARenamedUnitsWorkIsFoundUnderEveryKeyItHeld(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	unitTask(t, r, "first", "plat")      // filed under the key it was created with
	unitTask(t, r, "middle", "infra")    // filed after the first rename
	unitTask(t, r, "latest", "platform") // filed under the key it holds now
	unitTask(t, r, "other", "product")

	want := []string{"first", "latest", "middle"}
	for _, ref := range []string{"platform", "Platform", "infra", "plat", "PLAT"} {
		for _, filter := range []string{"unit", "routing_unit"} {
			got := ids(r.askWith(platformRenamed, map[string]any{filter: ref}))
			slices.Sort(got)
			if !slices.Equal(got, want) {
				t.Errorf("%s=%s answers %v, want every row the team filed under "+
					"any key it has held: %v", filter, ref, got, want)
			}
		}
	}
	// AND THE FILTER STILL NARROWS to the one team.
	if got := ids(r.askWith(platformRenamed, map[string]any{"unit": "product"})); len(got) != 1 ||
		got[0] != "other" {
		t.Errorf("unit=product answers %v, want only the row filed into it", got)
	}

	for _, axis := range []string{"unit", "routing_unit"} {
		answer := r.askWith(platformRenamed, map[string]any{
			"container": "project:ENG", "group_by": axis,
		})
		// ONE COLUMN, under the key the team holds now and headed with
		// its name, counting every row it ever filed.
		team := groupOf(t, answer, "platform")
		if team.Count != 3 || len(team.Rows) != 3 || team.Label != "Platform" {
			t.Errorf("group_by=%s draws the renamed team as %d counted, %d rows, "+
				"headed %q — want one column of all three rows under Platform",
				axis, team.Count, len(team.Rows), team.Label)
		}
		for _, group := range answer.Groups {
			if group.Key == "plat" || group.Key == "infra" {
				t.Errorf("group_by=%s still draws a column under the retired key %q",
					axis, group.Key)
			}
		}
		// AND A NARROWING WRITTEN WITH A RETIRED KEY LANDS ON THAT COLUMN.
		narrowed := r.askWith(platformRenamed, map[string]any{
			"container": "project:ENG", "group_by": axis, "group": "infra",
		})
		if len(narrowed.Groups) != 1 || groupOf(t, narrowed, "platform").Count != 3 {
			t.Errorf("group_by=%s group=infra narrows to %+v, want the team's one "+
				"column of three", axis, narrowed.Groups)
		}
	}
}
