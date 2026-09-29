package engine

import (
	"context"

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

// presenceHolders answers who holds a partition from the live data nodes: every
// live data node holds every partition of the layout this build runs.
//
// TRUE OF THIS BUILD BY CONSTRUCTION, because no partition is joined or left
// while it runs — a data node opens every partition of its layout and runs every
// log of it ([heldIn]) — so the holders of each are exactly the presence half
// the trim has always counted, and nothing is counted differently for having
// asked per partition. A layout whose partitions are placed on some nodes
// answers from where they are placed instead, including a holder whose lease
// has lapsed: one that is still joining is the node whose tail must not be
// trimmed.
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
