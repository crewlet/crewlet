package partmap

import (
	"slices"
	"time"

	"github.com/google/uuid"
)

// WHAT THE ESTATE ALARMS READ OF THE MAP OVER TIME: which partitions no copy
// can answer for, which are short of copies and since when, and which joins
// have been joining since when.
//
// # Why this node's own observation, and not a time in the record
//
// Two of the alarms fire on a DURATION — a partition short of copies for
// longer than membership's grace, a join older than the rejoin window — and
// the record holds no time a node could compare its clock with: the holder
// table counts EPOCHS, which say what happened in which order and nothing
// about how long ago, and the map is written only when something changes, so
// a tick count per partition would rewrite it every tick. So each node that
// evaluates the alarms watches the map itself, on the maintainer's own
// cadence, and measures how long IT has seen a condition hold without a
// break, on its own monotonic clock.
//
// That is a LOWER BOUND, and deliberately so: a node that restarted, or whose
// view went stale in between, starts counting again — an alarm that fires late
// by one sighting, never one that fires on a condition nobody saw hold. A
// break in what the node could see is the same as a break in the condition:
// [Watch.Forget].

// Finding is one condition the watch has seen hold.
type Finding struct {
	// Partition is the partition, and Node the holder a join finding is
	// about — empty for a partition's own.
	Partition string
	Node      string

	// Serving and Wanted are the partition's copies that can answer and
	// the copies its target has ([Coverage]).
	Serving, Wanted int

	// For is how long this node has seen the condition hold without a
	// break.
	For time.Duration
}

// Findings is one observation of the map: every partition nobody serves,
// every partition short of copies, and every join in flight — each list with
// the condition held longest first.
type Findings struct {
	Unserved []Finding
	Short    []Finding
	Joining  []Finding
}

// Watch measures how long each condition the estate alarms read has held, as
// this node has seen it. Its zero value is ready; it is not safe for
// concurrent use — the caller that samples it on a cadence owns it.
type Watch struct {
	// gen is the map lineage the memory below is of: a map written again
	// from nothing is another map, and nothing seen of the old one holds.
	gen uuid.UUID

	// short is when each partition was first seen short, in this run.
	short map[string]time.Time

	// joining is when each join was first seen, keyed by the partition, the
	// node and the epoch it was named at — so a join withdrawn and named
	// again is a new join, counted from its own start.
	joining map[joinKey]time.Time
}

// joinKey is one join: a node named to join a partition at an epoch.
type joinKey struct {
	partition, node string
	since           uint64
}

// Observe takes one sighting of the map and the live estate leases at now, and
// answers what holds and for how long. A condition that does not hold in this
// sighting is forgotten, so one that holds again is counted from again.
func (w *Watch) Observe(m Map, live []Presence, now time.Time) Findings {
	if w.gen != m.Generation {
		w.Forget()
		w.gen = m.Generation
	}
	short := make(map[string]time.Time, len(w.short))
	joining := make(map[joinKey]time.Time, len(w.joining))
	var out Findings
	for _, c := range m.Coverage(live) {
		id := c.Partition.String()
		if c.Short() {
			since, seen := w.short[id]
			if !seen {
				since = now
			}
			short[id] = since
			f := Finding{Partition: id, Serving: len(c.Serving), Wanted: c.Wanted, For: now.Sub(since)}
			out.Short = append(out.Short, f)
			if c.Unserved() {
				out.Unserved = append(out.Unserved, f)
			}
		}
		for _, h := range c.Joining {
			key := joinKey{partition: id, node: h.Node, since: h.Since}
			since, seen := w.joining[key]
			if !seen {
				since = now
			}
			joining[key] = since
			out.Joining = append(out.Joining, Finding{Partition: id, Node: h.Node,
				Serving: len(c.Serving), Wanted: c.Wanted, For: now.Sub(since)})
		}
	}
	w.short, w.joining = short, joining
	for _, list := range [][]Finding{out.Unserved, out.Short, out.Joining} {
		slices.SortStableFunc(list, func(a, b Finding) int {
			switch {
			case a.For > b.For:
				return -1
			case a.For < b.For:
				return 1
			}
			return 0
		})
	}
	return out
}

// Forget drops everything the watch has seen: the node could not see the map
// for a while, so nothing it saw before is known to have held since.
func (w *Watch) Forget() {
	w.gen = uuid.Nil
	w.short = nil
	w.joining = nil
}
