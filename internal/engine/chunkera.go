package engine

import (
	"context"
	"errors"

	"github.com/crewlet/crewlet/internal/tracker"
)

// chunkEra is whether any node that keeps files in content-addressed chunks is
// left in the fleet: the gate on deleting what that build left behind — the
// chunks themselves (the collector's retirement) and the bucket it locked them
// in (the maintenance duty's).
//
// # The tracker log's census, not the lease protocol
//
// The era is OVER once every node the TRACKER log counts reads at least
// [tracker.FileObjectVersion] there, and at least one node is counted. A chunk
// is named only by a file row on a data node of the older build — the only
// rows that ever named one — and that node is counted on the tracker log until
// it upgrades, leaves, or an operator evicts it, which is exactly the census
// [countedReaders] takes for the vector log's index and the usage log's
// person days. A node that does not run
// the log is not counted, and an empty census is NOT over: a store that sees
// nobody at all cannot see an older node either, while a deletion cannot be
// taken back.
//
// Not the lease protocol floor the lifetime counters retire on: one object per
// upload changed what a tracker record says and nothing about what holding a
// lease means, and bumping the protocol would have kept new nodes from taking
// seats until the last old one drained — pushing more writes into chunks the
// new build cannot read.
type chunkEra struct {
	e *Engine
	n *native
}

// chunkEra is the era as this node's native runtime counts it.
func (e *Engine) chunkEra(n *native) chunkEra { return chunkEra{e: e, n: n} }

// errNoTrackerLog is a census asked of a node running no tracker log.
var errNoTrackerLog = errors.New("engine: this node runs no tracker log to count the chunk era on")

// Over implements the collector's and the maintenance duty's ChunkEra.
func (c chunkEra) Over(ctx context.Context) (bool, error) {
	if c.n == nil || c.n.log == nil || c.e.backends == nil {
		return false, errNoTrackerLog
	}
	s := c.n.log
	name := tracker.Domain{}.Name()
	if s.Domain(name) == nil {
		return false, errNoTrackerLog
	}
	readers, err := c.e.countedReadersOf(s).readers(ctx, name)
	if err != nil {
		return false, err
	}
	return chunkEraOver(readers), nil
}

// chunkEraOver is the era's rule over a census: true when it counts at least
// one node and every one reads [tracker.FileObjectVersion] or later.
func chunkEraOver(readers map[string]int) bool {
	if len(readers) == 0 {
		return false
	}
	for _, version := range readers {
		if version < tracker.FileObjectVersion {
			return false
		}
	}
	return true
}
