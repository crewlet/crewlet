package engine

import (
	"context"
	"sync"
	"time"
)

// HOW LONG THIS NODE HAS BEEN UNABLE TO READ THE TRIM FLOOR, for
// `floor_unknown`, which fires on an age. A health read has no memory and fails
// whole on an unreadable floor, so the age is this node's own observation on
// the alarm heartbeat (first failing beat to latest), in memory because it is a
// fact about this node alone.
//
// A read is the read path's own two steps: each domain's floor by its own key
// ([readFloorsOf]), then [floorFor] at its applier's generation, as the write
// fence and the readiness gate read it, so the alarm names which failed. Never a listing of the register: that creates a consumer
// through a clustered broker's metadata group, which times out under load with
// every key readable. The keys are read at once: one round trip per beat.

// floorSubject is one domain the watch asks about, at the generation its
// applier is on.
type floorSubject struct {
	domain     string
	generation uint32
}

// floorWatch is how long each domain's floor has been unreadable on this node.
type floorWatch struct {
	// floors reads the published floor of each domain named, each by its
	// key ([readFloorsOf]). Nil observes nothing, which is a node with no
	// fleet store — and one with nothing to read either.
	floors func(ctx context.Context, domains []string) publishedFloors

	mu sync.Mutex
	// first is when each domain's floor was first found unreadable, and
	// cause what the latest read said; both are dropped the moment a read
	// succeeds, so an outage that ends and a later one start two clocks.
	first map[string]time.Time
	cause map[string]string
	// at is the latest observation.
	at time.Time
}

// newFloorWatch is a watch over the published floors as floors reads them.
func newFloorWatch(floors func(ctx context.Context, domains []string) publishedFloors) *floorWatch {
	return &floorWatch{floors: floors, first: map[string]time.Time{},
		cause: map[string]string{}}
}

// observe takes one look at every domain's floor at now. A read that failed
// because the beat's own ctx ended starts no clock and clears none: it says
// nothing about coordination.
func (w *floorWatch) observe(ctx context.Context, now time.Time, subjects []floorSubject) {
	if w == nil || w.floors == nil {
		return
	}
	domains := make([]string, 0, len(subjects))
	for _, s := range subjects {
		domains = append(domains, s.domain)
	}
	read := w.floors(ctx, domains)
	if read.unread() != nil && ctx.Err() != nil {
		return
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	seen := make(map[string]bool, len(subjects))
	for _, s := range subjects {
		seen[s.domain] = true
		// The read path's own error ([publishedFloors.floor]), so the cause
		// names the key or the generation rather than a bare transport error.
		_, err := read.floor(s.domain, s.generation)
		if err == nil {
			delete(w.first, s.domain)
			delete(w.cause, s.domain)
			continue
		}
		if _, known := w.first[s.domain]; !known {
			w.first[s.domain] = now
		}
		w.cause[s.domain] = err.Error()
	}
	// A DOMAIN THIS NODE NO LONGER RUNS has no floor to be unable to read.
	for domain := range w.first {
		if !seen[domain] {
			delete(w.first, domain)
			delete(w.cause, domain)
		}
	}
	w.at = now
}

// of is the latest observation of one domain's floor: how long it has been
// unreadable and what its latest read said, both zero while it is readable.
//
// Per domain, because a read refuses on its own log's floor: the report lists
// every unreadable log with its own cause under the one `floor_unknown` alarm
// ([statelog.Alarm.Domains]), so an operator who repairs one is not met by a
// second under an alarm that never cleared; the [statelog.Tracker] raises and
// clears each.
//
// Measured to the latest observation, never to now: a report between beats
// would otherwise show an alarm the gauge set at the beat does not.
func (w *floorWatch) of(domain string) (time.Duration, string) {
	if w == nil {
		return 0, ""
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	first, unreadable := w.first[domain]
	if !unreadable {
		return 0, ""
	}
	return w.at.Sub(first), w.cause[domain]
}
