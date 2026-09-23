package engine

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/crewlet/crewlet/internal/coord"
)

// HOW LONG THIS NODE HAS BEEN UNABLE TO READ THE TRIM FLOOR, for
// `floor_unknown`.
//
// # Why it is observed rather than read off a health
//
// A health read has no memory: it reads the published floor there and then,
// and a floor that cannot be read makes the whole read fail — so the alarm's
// input could only ever say "unknown now", never "unknown for four
// heartbeats", which is what the rule fires on. The report used to stand the
// threshold itself in for the age on the one branch that could see an unknown
// floor, and that branch was unreachable besides: a health read that failed on
// its floor was skipped before the branch was reached. The alarm could not
// fire.
//
// So the age is this node's own observation, taken on the alarm heartbeat
// exactly as the dangling bindings' is (see bindings.go): first failing beat
// to latest, never a persistence nobody saw, and in memory because "can this
// node read the floor" is a fact about this node alone.
//
// # What a read is
//
// THE READ PATH'S OWN TWO STEPS — the published floors, then [floorFor] per
// domain at the generation its applier is on — which is what the write fence,
// the readiness gate and every read's health take. A floor this node cannot
// use is therefore one of two things, and the alarm names which: coordination
// could not be listed, or the fleet published at a generation this node has
// not reached. One listing serves every domain, so a beat costs one
// coordination read however many logs the node runs.

// floorSubject is one domain the watch asks about, at the generation its
// applier is on.
type floorSubject struct {
	domain     string
	generation uint32
}

// floorWatch is how long each domain's floor has been unreadable on this node.
type floorWatch struct {
	// floors lists the published floors. Nil observes nothing, which is a
	// node with no fleet store — and one with nothing to read either.
	floors func(context.Context) ([]coord.TrimFloor, error)

	mu sync.Mutex
	// first is when each domain's floor was first found unreadable, and
	// cause what the latest read said; both are dropped the moment a read
	// succeeds, so an outage that ends and a later one start two clocks.
	first map[string]time.Time
	cause map[string]string
	// at is the latest observation.
	at time.Time
}

// newFloorWatch is a watch over one listing of the published floors.
func newFloorWatch(floors func(context.Context) ([]coord.TrimFloor, error)) *floorWatch {
	return &floorWatch{floors: floors, first: map[string]time.Time{},
		cause: map[string]string{}}
}

// observe takes one look at every domain's floor at now.
//
// A STOP THIS PROCESS ASKED FOR IS NOT AN UNREADABLE FLOOR: a read that failed
// because the beat's own context ended starts no clock and clears none, since
// it says nothing about coordination.
func (w *floorWatch) observe(ctx context.Context, now time.Time, subjects []floorSubject) {
	if w == nil || w.floors == nil {
		return
	}
	floors, listErr := w.floors(ctx)
	if listErr != nil && ctx.Err() != nil {
		return
	}
	if listErr != nil {
		// IN THE READ PATH'S OWN WORDS ([stateLog.trimFloor]), so the
		// cause names what could not be read: the transport's error alone
		// — a timeout, a closed connection — reached the alarm as the
		// whole of its cause, with nothing in it saying it was the fleet's
		// floors that were asked for, while the refusal a read met on the
		// same node named them.
		listErr = fmt.Errorf("engine: read the fleet's published trim floors: %w", listErr)
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	seen := make(map[string]bool, len(subjects))
	for _, s := range subjects {
		seen[s.domain] = true
		err := listErr
		if err == nil {
			_, err = floorFor(floors, s.domain, s.generation)
		}
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
// PER DOMAIN, because the refusal is: a read of one log refuses on that log's
// floor, and a floor published ahead of this node is one log's re-anchor. The
// form this replaced reduced the watch to the domain unreadable longest, so a
// node two logs behind a re-anchor named one of them — and an operator who
// repaired it met the second only after the fix, under an alarm that had never
// cleared. The report evaluates each domain's answer on its own and names the
// log; a coordination outage that fails every domain at once raises one per
// log, each saying so.
//
// FIRST SIGHTING TO LATEST OBSERVATION, never to now, for the binding watch's
// reason: a report assembled between two beats knows only what the last one
// found, and measuring to now would fire on a screen an alarm the gauge beside
// it, set at the beat, does not.
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
