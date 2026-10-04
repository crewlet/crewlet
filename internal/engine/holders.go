package engine

import (
	"context"

	"github.com/crewlet/crewlet/internal/coord"
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

// holdersOf is who holds the estate, as every counted set on this node reads it:
// the live data nodes, listed afresh ([presenceHolders]).
//
// ONE CHOICE, made here, for the trim and the embedding duty alike: the two read
// one log's counted set for two questions — what may be removed, and who must
// read a record before it is published — and answered from two sources they
// could disagree about who the log's readers are.
func (e *Engine) holdersOf() partitionHolders {
	return presenceHolders{leases: e.backends.Coord}
}

// watchedHolders is who holds the estate as this node's WATCHED presence view
// answers it ([viewHolders]), for a question asked often that decides nothing
// a view's age could make unsafe: whether the estate has anybody to donate a
// snapshot to ([Engine.countedOn]).
//
// NOT THE TRIM'S ANSWER, which lists presence afresh on every tick
// ([Engine.holdersOf]): the trim licenses removing records, and a listing up to
// a heartbeat old may miss a node that has just booted — the one node the trim
// must not pass.
func (e *Engine) watchedHolders() partitionHolders {
	return viewHolders{view: e.dataView}
}

// viewHolders answers who holds a partition from the watched presence view:
// every live data node holds layout 0's one partition, as [presenceHolders]
// says, from memory rather than a listing.
type viewHolders struct{ view *coord.LeaseView }

// Holders is every live data node the view names, for each partition asked
// about — or why the view cannot say.
func (h viewHolders) Holders(ctx context.Context,
	partitions []statelog.PartitionID) (map[statelog.PartitionID][]statelog.Presence, error) {

	nodes, err := presenceRoster(h).LiveDataNodes()
	if err != nil {
		return nil, err
	}
	live := make([]statelog.Presence, 0, len(nodes))
	for _, id := range nodes {
		live = append(live, statelog.Presence{NodeID: id})
	}
	return heldByEvery(live, partitions), nil
}

// presenceHolders answers who holds a partition from the live data nodes: every
// live data node holds layout 0's one partition, and nothing else.
//
// TRUE OF LAYOUT 0 BY CONSTRUCTION: the estate is one partition, which every data
// node holds whole from boot ([heldIn]), so its holders are exactly the presence
// half the trim has always counted — and the same live data nodes the router
// asks.
type presenceHolders struct{ leases liveLeases }

// Holders is every live data node, for each partition asked about.
func (h presenceHolders) Holders(ctx context.Context,
	partitions []statelog.PartitionID) (map[statelog.PartitionID][]statelog.Presence, error) {

	live, err := livePresences(ctx, h.leases)
	if err != nil {
		return nil, err
	}
	return heldByEvery(live, partitions), nil
}

// heldByEvery is every one of partitions held by every node of live — layout
// 0's answer, where every live data node holds the one partition whole.
func heldByEvery(live []statelog.Presence,
	partitions []statelog.PartitionID) map[statelog.PartitionID][]statelog.Presence {

	out := make(map[statelog.PartitionID][]statelog.Presence, len(partitions))
	for _, p := range partitions {
		out[p] = live
	}
	return out
}
