package partmap

import (
	"fmt"
	"slices"

	"github.com/crewlet/crewlet/internal/membership"
	"github.com/crewlet/crewlet/internal/statelog"
)

// WHAT THE MAP SAYS ABOUT EACH PARTITION'S COPIES, NOW — for every surface that
// shows the map and for the alarms that watch it, which must agree about which
// partitions are short and which are nobody's.

// WholeEstate is what every surface says of a fleet at layout 0, where there is
// no estate map: the GET /estate answer, the command line, the dashboard and
// every gesture's refusal give this one sentence, so an operator reads the same
// words wherever they ask.
const WholeEstate = "layout 0: every data node holds the whole estate, so there " +
	"is no partition to place, move or hold a node for"

// Unplaced is what every surface says of a fleet at a partitioned layout whose
// first estate map has not been written: nothing is placed yet, which — unlike
// [WholeEstate] — is a wait rather than a fact about the fleet.
func Unplaced(layout int) string {
	return fmt.Sprintf("layout %d: no estate map has been written yet, so no partition "+
		"is placed — the estate-map duty writes the first once the layout's logs exist "+
		"and a company and a data node are there to place them on", layout)
}

// Able is every node whose live estate lease counts as PRESENT AND HEALTHY at a
// layout — the reading the map's maintainer takes of each lease
// ([Presence.membership]): a lease that says its store is healthy and that it
// runs the layout. A node outside it is, to the map, a node that is not there.
//
// A node listed twice is taken at its first, and a lease offering no share is
// none, as membership takes them ([membership.ByNode]).
func Able(live []Presence, layout int) map[string]bool {
	byNode := membership.ByNode(live, func(p Presence) membership.Presence {
		return membership.Presence{Node: p.Node, Weight: p.Meta.Weight}
	})
	out := make(map[string]bool, len(byNode))
	for node, p := range byNode {
		if !p.membership(layout).Unhealthy {
			out[node] = true
		}
	}
	return out
}

// Coverage is one partition's copies as the map and the live estate leases say
// them now.
type Coverage struct {
	// Partition is the partition.
	Partition statelog.PartitionID

	// Serving is its holders the map lists serving whose node is able
	// ([Able]) — the copies that can answer for it now, in node order.
	//
	// NOT THE MAP'S OWN COUNT, which keeps a serving holder serving while
	// its node is gone until membership removes it, ten minutes on: routers
	// route to it and nothing answers, which is exactly what a surface
	// showing this partition must not call a copy.
	Serving []string

	// Wanted is how many copies its target has: the company's copies,
	// bounded by the members the map can place on ([Map.Size]).
	Wanted int

	// Joining is its holders the map lists joining, in node order.
	Joining []Holder
}

// Unserved reports whether no copy can answer for the partition now.
func (c Coverage) Unserved() bool { return len(c.Serving) == 0 }

// Short reports whether fewer copies can answer for the partition than its
// target has — an unserved partition included.
func (c Coverage) Short() bool { return len(c.Serving) < c.Wanted }

// Coverage is every partition's [Coverage], in the layout's order, as the live
// estate leases say it now.
func (m Map) Coverage(live []Presence) []Coverage {
	able := Able(live, m.Layout.Number)
	wanted := m.Size()
	parts := m.Layout.Partitions()
	out := make([]Coverage, len(m.Partitions))
	for g, p := range m.Partitions {
		c := Coverage{Partition: parts[g], Wanted: wanted}
		for _, h := range p.Holders {
			switch {
			case h.State == Serving && able[h.Node]:
				c.Serving = append(c.Serving, h.Node)
			case h.State == Joining:
				c.Joining = append(c.Joining, h)
			}
		}
		slices.Sort(c.Serving)
		out[g] = c
	}
	return out
}

// Targets is every partition's [Map.Target], in the layout's order, from one
// layout of the draw — for a surface rendering every partition, which would
// otherwise hash every member once per partition. The slices are the caller's
// own: the layout they come from is computed for this call and kept by nobody.
func (m Map) Targets() [][]string { return m.targets() }
