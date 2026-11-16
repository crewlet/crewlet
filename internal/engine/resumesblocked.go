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
// A finished job whose resume fails on a condition only an operator clears is
// counted by the executor coordinator of the node holding its seat, from that
// node's own memory of its attempts ([sandbox.Coordinator.ResumesBlocked]). That
// node may evaluate no alarm table at all: a satellite (`roles: [seats]`) runs
// no state log, and read from each node's own memory alone, the alarm was never
// raised for a seat a satellite held, the one topology where nobody reads the
// node's log by default. So the node says it in its status record
// ([coord.NodeStatusRecord.ResumesBlocked]) and every data node raises it from
// the live records, as the model alarms are raised, this node's own runs read
// from its memory rather than from its last record.
//
// A LIVE NODE WHOSE RECORD IS NOT COUNTED KEEPS ITS LAST WORD while its
// presence stands at the epoch it said it under: a record not yet rewritten, or
// one whose write failed and aged out while the node lives, is a node not
// saying, never one whose runs stopped waiting, and read as the second the
// alarm cleared on that pass and was raised again on the next. A node whose
// presence is gone, or moved to a new epoch, says nothing it said before: its
// seats move to holders that say their own, and a restarted node's memory of
// its attempts went with its process. And a listing that fails is the last one
// that answered, as the model alarms' is, so a blip neither clears the alarm
// nor raises it again on the next tick.

// blockedSaid is what [Engine.executorReading] last read of each live node: its
// runs waiting for an operator, nil where it named none, under the presence
// epoch it said them at.
type blockedSaid struct {
	mu    sync.Mutex
	nodes map[string]blockedWord
}

// blockedWord is one live node's last word on its runs waiting for an
// operator, and the presence epoch its record was written under.
type blockedWord struct {
	epoch   int64
	blocked *coord.ResumesBlocked
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
// status records, joined by epoch ([coord.LiveNodeStatuses]): a live node whose
// record is not counted keeps its last word at the same epoch, and the last
// listings that answered stand for a pair that failed.
func (e *Engine) blockedByNode(ctx context.Context) map[string]*coord.ResumesBlocked {
	in := &e.blockedSaid
	leases, records, err := e.statusListings(ctx)
	in.mu.Lock()
	defer in.mu.Unlock()
	if err != nil {
		log.WarnContext(ctx, "executor_alarm_records_unread", "error", err.Error(),
			"detail", "the runs of seats waiting for an operator are judged on the status records "+
				"this node last read")
	} else {
		counted := coord.LiveNodeStatuses(records, leases)
		next := make(map[string]blockedWord, len(leases))
		for _, lease := range leases {
			node, ok := coord.NodeID(lease.Resource)
			if !ok {
				continue
			}
			if r, ok := counted[node]; ok {
				next[node] = blockedWord{epoch: r.Epoch, blocked: r.ResumesBlocked}
			} else if last, ok := in.nodes[node]; ok && last.epoch == lease.Epoch {
				next[node] = last
			}
		}
		in.nodes = next
	}
	out := make(map[string]*coord.ResumesBlocked, len(in.nodes)+1)
	for node, word := range in.nodes {
		out[node] = word.blocked
	}
	return out
}

// statusListings is one listing of the presence leases and one of the status
// records; neither where this engine has no store to list them from.
func (e *Engine) statusListings(ctx context.Context) ([]coord.Lease, []coord.NodeStatusRecord, error) {
	if e.backends == nil || e.backends.Coord == nil || e.backends.Fleet == nil {
		return nil, nil, nil
	}
	leases, err := e.backends.Coord.ListLive(ctx, coord.ClassNode)
	if err != nil {
		return nil, nil, err
	}
	records, err := e.backends.Fleet.NodeStatusRecords(ctx)
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
