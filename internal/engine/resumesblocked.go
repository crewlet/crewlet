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
// node's log by default. So the node says it on its heartbeat
// ([coord.NodeStatus.ResumesBlocked]) and every data node raises it from the
// live leases, as the model and MCP alarms are raised, this node's own runs read
// from its memory rather than from its last beat.
//
// A LIVE NODE THAT SAID NOTHING THIS BEAT KEEPS ITS LAST WORD: a heartbeat whose
// status hook overran its share publishes no status at all, which is a node not
// saying, never one whose runs stopped waiting, and read as the second the alarm
// cleared on that beat and was raised again on the next. A node whose lease is
// gone says nothing any more: its seats move to holders that say their own. And
// a listing that fails is the last one that answered, as the model alarms' is,
// so a blip neither clears the alarm nor raises it again on the next tick.

// blockedSaid is what [Engine.executorReading] last read of each live node: its
// runs waiting for an operator, nil where it named none.
type blockedSaid struct {
	mu    sync.Mutex
	nodes map[string]*coord.ResumesBlocked
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
// operator, by node id, off one listing of the presence leases: a node whose
// beat carried no status keeps its last word, and the last listing that
// answered stands for one that failed.
func (e *Engine) blockedByNode(ctx context.Context) map[string]*coord.ResumesBlocked {
	in := &e.blockedSaid
	var leases []coord.Lease
	var err error
	if e.backends != nil && e.backends.Coord != nil {
		leases, err = e.backends.Coord.ListLive(ctx, coord.ClassNode)
	}
	in.mu.Lock()
	defer in.mu.Unlock()
	if err != nil {
		log.WarnContext(ctx, "executor_alarm_leases_unread", "error", err.Error(),
			"detail", "the runs of seats waiting for an operator are judged on the heartbeats "+
				"this node last read")
	} else {
		next := make(map[string]*coord.ResumesBlocked, len(leases))
		for _, lease := range leases {
			node, _ := coord.ClassNode.Name(lease.Resource)
			if status, ok := coord.StatusFromMeta(lease.Meta); ok {
				next[node] = status.ResumesBlocked
			} else {
				next[node] = in.nodes[node]
			}
		}
		in.nodes = next
	}
	out := make(map[string]*coord.ResumesBlocked, len(in.nodes)+1)
	maps.Copy(out, in.nodes)
	return out
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
