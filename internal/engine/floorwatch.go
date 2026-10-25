package engine

import (
	"context"
	"sync"
	"time"
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
// So the age is this node's own observation, taken on the alarm heartbeat:
// first failing beat to latest, never a persistence nobody saw, and in memory
// because "can this node read the floor" is a fact about this node alone.
//
// # What a read is
//
// THE READ PATH'S OWN TWO STEPS — each domain's published floor read by its
// own key ([readFloorsOf]), then [floorFor] at the generation its applier is
// on — which is what the write fence, the readiness gate and every read's
// health take. A floor this node cannot use is therefore one of two things,
// and the alarm names which: that domain's key could not be read, or the fleet
// published at a generation this node has not reached. EACH DOMAIN BY ITS KEY,
// never a listing of the register, for the read path's reason: a listing is a
// consumer created and deleted through the metadata group on a clustered
// broker, and on a loaded fleet that create ran past the client's API timeout
// with every key perfectly readable — a watch that listed would age an outage
// no read was having. The keys are read at once, so a beat costs one round
// trip however many logs the node runs.

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

// observe takes one look at every domain's floor at now.
//
// A STOP THIS PROCESS ASKED FOR IS NOT AN UNREADABLE FLOOR: a read that failed
// because the beat's own context ended starts no clock and clears none, since
// it says nothing about coordination.
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
		// IN THE READ PATH'S OWN WORDS ([publishedFloors.floor]), so the
		// cause names what could not be read — the domain's key, or a floor
		// published at a generation this node has not reached — rather than
		// a transport's error alone, a timeout or a closed connection with
		// nothing in it saying it was a floor that was asked for.
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
// PER DOMAIN, because the refusal is: a read of one log refuses on that log's
// floor, and a floor published ahead of this node is one log's re-anchor. The
// form this replaced reduced the watch to the domain unreadable longest, so a
// node two logs behind a re-anchor named one of them — and an operator who
// repaired it met the second only after the fix, under an alarm that had never
// cleared. The report evaluates each domain's answer on its own and lists every
// unreadable log, with its own cause, under the one `floor_unknown` alarm
// ([statelog.Alarm.Domains]); a coordination outage that fails every domain at
// once is that one alarm naming every log, and the [statelog.Tracker] writes a
// raise and a clear line for each.
//
// FIRST SIGHTING TO LATEST OBSERVATION, never to now: a report assembled
// between two beats knows only what the last one found, and measuring to now
// would fire on a screen an alarm the gauge beside it, set at the beat, does
// not.
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
