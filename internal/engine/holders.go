package engine

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/estate/partmap"
	"github.com/crewlet/crewlet/internal/statelog"
)

// partitionHolders is who the fleet says HOLDS each partition: the half of a
// log's counted set that is not the positions register ([statelog.CountedSet]).
//
// # Why the counted set asks who holds the log's partition
//
// The trim may not remove a record a node that applies the log has not applied,
// and the nodes that apply a log are its partition's holders — not the fleet's
// data nodes, once a partition is held by some of them. So a log's counted set
// is the register's rows naming it, UNION every holder of its partition,
// whether or not that holder has reported a position — a holder that has not
// is one about to replay, and counts at zero — MINUS the tombstones past their
// window. Asking the fleet for its data nodes instead would count every node on
// every log, so one offline node would pin every partition's log rather than
// its own.
//
// # One answer per tick, every partition at once
//
// Every log a tick evaluates is judged against one reading ([retention.read]),
// because two logs judged against two listings could disagree about who is
// there; so the question is asked for the partitions a caller needs, together.
type partitionHolders interface {
	Holders(ctx context.Context, partitions []statelog.PartitionID) (map[statelog.PartitionID][]statelog.Presence, error)
}

// holdersOf is who holds the partitions of layout, as every counted set on this
// node reads it: the live data nodes under layout 0 ([presenceHolders]), and
// the estate map's holder table under any other ([mapHolders]).
//
// ONE CHOICE, made here, for the trim and the embedding duty alike: the two read
// one log's counted set for two questions — what may be removed, and who must
// read a record before it is published — and answered from two sources they
// could disagree about who the log's readers are.
func (e *Engine) holdersOf(layout statelog.Layout) partitionHolders {
	if layout.Number == 0 {
		return presenceHolders{leases: e.backends.Coord}
	}
	return mapHolders{layout: layout, view: e.estateMapView}
}

// estateMapView is this node's estate view, looked up at every read — the
// watch may start after the loops that read it — and nil where it runs none.
func (e *Engine) estateMapView() estateHolderView {
	if w := e.estateWatch.Load(); w != nil {
		return w.view
	}
	return nil
}

// presenceHolders answers who holds a partition from the live data nodes: every
// live data node holds layout 0's one partition, and nothing else.
//
// TRUE OF LAYOUT 0 BY CONSTRUCTION: the estate is one partition, which every data
// node holds whole from boot ([heldIn]), so its holders are exactly the presence
// half the trim has always counted. It is the answer layout 0's routing reads
// too (partmap.View's roster) — which is why it stays the answer there rather
// than an estate map layout 0 never writes.
type presenceHolders struct{ leases liveLeases }

// Holders is every live data node, for each partition asked about.
func (h presenceHolders) Holders(ctx context.Context,
	partitions []statelog.PartitionID) (map[statelog.PartitionID][]statelog.Presence, error) {

	live, err := livePresences(ctx, h.leases)
	if err != nil {
		return nil, err
	}
	out := make(map[statelog.PartitionID][]statelog.Presence, len(partitions))
	for _, p := range partitions {
		out[p] = live
	}
	return out, nil
}

// estateHolderView is the estate view as a counted set reads it: the map it
// holds, and whether it is fresh enough to DECIDE from.
type estateHolderView interface {
	Map() (partmap.Map, uint64, bool, error)
	Fresh() bool
}

// mapHolders answers who holds a partition from the ESTATE MAP'S HOLDER TABLE:
// every holder of it, in EVERY state (§F2).
//
// # Every state, and whether or not its lease is live
//
// A JOINING holder is the node whose tail must not be trimmed: it took a hold
// when it began, and the hold goes stale after [statelog.TrimHoldStale] while an
// adoption can take longer — so it is counted from the moment the map names it,
// at zero until it reports. A LEAVING holder still decides writes until it
// drains, so it is counted until its release takes it out
// ([statelog.CountedSet]). And a holder whose lease flickered is counted all the
// same: the cost is bounded, because membership drops a holder that stays gone
// past its grace, after which only its register row can pin the log — which an
// operator's eviction lifts.
//
// # A stale map is UNKNOWN
//
// The counted set DECIDES — it licenses removing records — so it takes the view
// only while the view is fresh ([partmap.View.Fresh]): a map older than that may
// name last hour's holders and miss the joiner that arrived since, which is the
// one node the trim must not pass. A stale view, one that has never read the
// map, a fleet with no map, and a map of another layout are each an error, and
// the tick blocks on them rather than guessing.
type mapHolders struct {
	// layout is the layout this node runs, whose partitions it asks about.
	layout statelog.Layout

	// view is this node's estate view, looked up at every read, and nil
	// where it runs none.
	view func() estateHolderView
}

// Holders is every holder the map names for each partition asked about, or why
// that is not known.
func (h mapHolders) Holders(_ context.Context,
	partitions []statelog.PartitionID) (map[statelog.PartitionID][]statelog.Presence, error) {

	v := h.view()
	if v == nil {
		return nil, fmt.Errorf("%w: this node runs no estate view, so who holds the "+
			"partitions of layout %d is unknown", coord.ErrUnavailable, h.layout.Number)
	}
	if !v.Fresh() {
		return nil, fmt.Errorf("%w: this node's estate view is past its freshness bound, "+
			"so who holds each partition is unknown until it is confirmed again",
			coord.ErrUnavailable)
	}
	m, _, found, err := v.Map()
	switch {
	case err != nil:
		return nil, fmt.Errorf("read who holds each partition: %w", err)
	case !found:
		return nil, fmt.Errorf("%w: %w: %s", coord.ErrUnavailable, partmap.ErrNoMap,
			partmap.Unplaced(h.layout.Number))
	case m.Layout.Number != h.layout.Number:
		return nil, fmt.Errorf("%w: the estate map places layout %d and this node runs "+
			"layout %d, so it names no holder of this node's partitions",
			coord.ErrUnavailable, m.Layout.Number, h.layout.Number)
	}
	out := make(map[statelog.PartitionID][]statelog.Presence, len(partitions))
	for _, p := range partitions {
		// ASKED OF THE LAYOUT, never read off an empty answer: a partition
		// the map names and nobody holds yet is a table with no holders,
		// which is an answer — its log counts only the register's rows.
		if len(m.Layout.Logs(p)) == 0 {
			return nil, fmt.Errorf("%w: %q in layout %d", partmap.ErrUnknownPartition,
				p.String(), m.Layout.Number)
		}
		holders := m.HoldersOf(p)
		presences := make([]statelog.Presence, 0, len(holders))
		for _, holder := range holders {
			presences = append(presences, statelog.Presence{NodeID: holder.Node})
		}
		out[p] = presences
	}
	return out, nil
}

// everyHolder is every node holding any of the partitions answered, once each,
// in node order.
func everyHolder(held map[statelog.PartitionID][]statelog.Presence) []statelog.Presence {
	seen := map[string]bool{}
	var out []statelog.Presence
	for _, holders := range held {
		for _, h := range holders {
			if !seen[h.NodeID] {
				seen[h.NodeID] = true
				out = append(out, h)
			}
		}
	}
	slices.SortFunc(out, func(a, b statelog.Presence) int { return strings.Compare(a.NodeID, b.NodeID) })
	return out
}
