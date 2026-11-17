package engine

import (
	"context"
	"slices"

	"github.com/crewlet/crewlet/internal/notify"
	"github.com/crewlet/crewlet/internal/observe"
)

// The fleet-wide consumer groups this engine competes in, and whether each
// needs the node to hold data.
//
// A FLEET-WIDE GROUP's message goes to whichever member takes it first, unlike
// a seat's mailbox, so a node without `data` in it silently answers its share
// from a store deleted at its last boot, or no estate at all. So each group
// says ONCE, here, whether its handler needs data (reads or writes the
// replicated estate, or keeps a record that must outlive the process), and
// [Engine.joins] is the one door every attachment passes: a node without
// `data` joins no group that needs it, and nobody joins an undeclared group.
// groups_internal_test.go holds the table against the tree in BOTH directions.

// fleetGroup is one fleet-wide consumer group.
type fleetGroup struct {
	// Name is the group as the queue keys it.
	Name string
	// Data says the handler needs this node to hold data.
	Data bool
	// Why is the reason, written where the next reader looks.
	Why string
}

// fleetGroups are the groups the engine's own subsystems subscribe. A
// state-log domain's wake feed is a fleet-wide group too, declared by the
// domain itself ([statelog.Domain.FeedGroup]) — see [declaredFleetGroups].
var fleetGroups = []fleetGroup{
	{
		Name: notify.InboundGroup,
		Why: "routes an inbound delivery from its own envelope, the record's routing " +
			"snapshot and the chart, and reads no row — so a node without data routes " +
			"it exactly as a data node does",
	},
	{
		Name: observe.CustodyGroup,
		Data: true,
		Why: "writes a stateless node's events into this node's event log, which " +
			"a node without data deletes at every boot",
	},
}

// declaredFleetGroups is every fleet-wide group a node may join: the engine's
// own, and each registered domain's wake feed, which reads the domain's log —
// applied only where the estate is held.
func declaredFleetGroups() []fleetGroup {
	out := slices.Clone(fleetGroups)
	for _, d := range registeredDomains() {
		if group := d.FeedGroup(); group != "" {
			out = append(out, fleetGroup{Name: group, Data: true,
				Why: "the " + d.Name() + " domain's wake feed reads its log, which only " +
					"a node holding the estate applies"})
		}
	}
	return out
}

// fleetGroupNamed is the declaration of one group, false for one nobody
// declared.
func fleetGroupNamed(name string) (fleetGroup, bool) {
	for _, g := range declaredFleetGroups() {
		if g.Name == name {
			return g, true
		}
	}
	return fleetGroup{}, false
}

// joins reports whether this node attaches the fleet-wide group named — the
// one door every attachment of one passes.
//
// AN UNDECLARED GROUP IS REFUSED, and said: whether it needs data is the
// question this file exists to have answered, and a group nobody answered it
// for is attached on every node, data or not.
func (e *Engine) joins(ctx context.Context, name string) bool {
	group, ok := fleetGroupNamed(name)
	if !ok {
		log.ErrorContext(ctx, "fleet_group_undeclared", "group", name,
			"detail", "a fleet-wide consumer group that does not say whether it needs "+
				"data is attached nowhere; declare it in internal/engine/groups.go")
		return false
	}
	return !group.Data || e.local != nil
}
