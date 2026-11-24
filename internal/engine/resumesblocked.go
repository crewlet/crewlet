package engine

import (
	"context"
	"maps"
	"slices"
	"sync"
	"time"

	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/statelog"
)

// THE RUNS WAITING FOR AN OPERATOR, ACROSS THE FLEET: what the
// executor_resume_blocked alarm reads ([statelog.KindExecutorResumeBlocked]).
//
// Each node says its own in its status record
// ([coord.NodeStatusRecord.ResumesBlocked], from
// [sandbox.Coordinator.ResumesBlocked]) and every data node raises it from the
// live records, this node's own runs read from its memory: a satellite runs no
// state log, and read from each node's memory alone the alarm is never raised
// for a seat it held. A live node whose record is not counted keeps its last
// word at the same epoch ([coord.LiveNodeStatusesKept]), and a failed listing
// stands on the last that answered. The package doc's "The runs waiting for an
// operator" says why.

// blockedSaid is what [Engine.executorReading] last read of each live node's
// status record, a live node whose record is not counted keeping the one it
// last had.
type blockedSaid struct {
	mu      sync.Mutex
	records map[string]coord.NodeStatusRecord
}

// executorReading fills the alarm reading's runs of seats whose finished job's
// resume waits for an operator, across the fleet: how many, and the one waiting
// longest, by its seat, the node holding it, its condition and how long since
// its first such failure in a row, as of now. Fleet-wide, so every data node
// raises the same alarm whichever node holds the seat. A fleet with no such run
// leaves the fields zero, which the table reads as nothing wrong.
func (e *Engine) executorReading(ctx context.Context, now time.Time, out *statelog.Reading) {
	said := e.blockedByNode(ctx)
	said[e.selfNode()] = e.resumesBlocked()
	var oldest *coord.ResumesBlocked
	holder := ""
	for _, node := range slices.Sorted(maps.Keys(said)) {
		b := said[node]
		if b == nil || b.Runs <= 0 {
			continue
		}
		out.ExecutorResumesBlocked += b.Runs
		if oldest == nil || b.Since.Before(oldest.Since) {
			oldest, holder = b, node
		}
	}
	if oldest == nil {
		return
	}
	out.ExecutorResumeBlockedSeat = oldest.Seat
	out.ExecutorResumeBlockedNode = holder
	out.ExecutorResumeBlockedKind = oldest.Kind
	out.ExecutorResumeBlockedFor = max(now.Sub(oldest.Since), 0)
}

// blockedByNode is each live node's word on the runs of its seats waiting for an
// operator, by node id, off one listing of the presence leases and one of the
// status records, joined by epoch ([coord.LiveNodeStatusesKept]): a live node
// whose record is not counted keeps its last word at the same epoch, and the
// last listings that answered stand for a pair that failed.
func (e *Engine) blockedByNode(ctx context.Context) map[string]*coord.ResumesBlocked {
	in := &e.blockedSaid
	leases, records, err := e.statusListings(ctx, coord.NodeStatusRead{})
	in.mu.Lock()
	defer in.mu.Unlock()
	if err != nil {
		log.WarnContext(ctx, "executor_alarm_records_unread", "error", err.Error(),
			"detail", "the runs of seats waiting for an operator are judged on the status records "+
				"this node last read")
	} else {
		in.records = coord.LiveNodeStatusesKept(records, leases, in.records)
	}
	out := make(map[string]*coord.ResumesBlocked, len(in.records)+1)
	for node, r := range in.records {
		out[node] = r.ResumesBlocked
	}
	return out
}

// statusListings is one listing of the presence leases and one of the status
// records, each with the parts read names; neither where this engine has no
// store to list them from.
func (e *Engine) statusListings(ctx context.Context, read coord.NodeStatusRead) ([]coord.Lease,
	[]coord.NodeStatusRecord, error) {
	if e.backends == nil || e.backends.Coord == nil || e.backends.Fleet == nil {
		return nil, nil, nil
	}
	leases, err := e.backends.Coord.ListLive(ctx, coord.ClassNode)
	if err != nil {
		return nil, nil, err
	}
	records, err := e.backends.Fleet.NodeStatusRecords(ctx, read)
	if err != nil {
		return nil, nil, err
	}
	return leases, records, nil
}

// resumesBlocked is what this node says of the runs of seats it holds whose
// finished job's resume waits for an operator
// ([sandbox.Coordinator.ResumesBlocked]): read from the PUBLISHED runtime each
// time, never captured, since a runtime may arrive by an apply; nil where no
// coordinator runs here or none of its runs waits.
func (e *Engine) resumesBlocked() *coord.ResumesBlocked {
	rt := e.sandbox.Load()
	if rt == nil || rt.coordinator == nil {
		return nil
	}
	blocked := rt.coordinator.ResumesBlocked()
	if blocked.Runs == 0 {
		return nil
	}
	return &coord.ResumesBlocked{Runs: blocked.Runs, Seat: blocked.Seat, Kind: blocked.Kind,
		Since: blocked.Since}
}
