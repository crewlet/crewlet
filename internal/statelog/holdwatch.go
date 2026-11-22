package statelog

import (
	"context"
	"time"

	"github.com/crewlet/crewlet/internal/coord"
)

// HoldLister is the slice of the fleet's trim-hold register the trim reads
// ([coord.HoldRegister]).
type HoldLister interface {
	Holds(ctx context.Context) ([]coord.TrimHold, error)
}

// HoldWatch is what the trim has watched of each hold: the write of it last
// listed, by the store's revision, and when that write was first listed. It
// answers each hold's watched silence ([Hold.Silent]), how long this process
// has seen that one write stand unrenewed, on its own monotonic clock and on
// no stored instant (package doc, "A trim hold is ignored only once the trim
// watched it go unrenewed").
//
// The zero value is ready. One goroutine owns it.
type HoldWatch struct {
	// Now is the watch's clock. Nil is [time.Now], never in UTC, which
	// would drop the monotonic reading the silences are counted on.
	Now func() time.Time

	seen map[string]sighting
}

// sighting is one hold's write as the watch first listed it.
type sighting struct {
	revision uint64
	first    time.Time
}

// clock is [HoldWatch.Now], or the wall clock with its monotonic reading.
func (w *HoldWatch) clock() time.Time {
	if w.Now != nil {
		return w.Now()
	}
	return time.Now()
}

// Read lists the holds and answers each owner's watched silence: zero for a
// write listed for the first time, and from the instant a write was first
// listed to the moment this listing began for one listed again, so neither
// listing's own duration counts. A hold with no revision is never watched. A
// failed listing changes nothing.
func (w *HoldWatch) Read(ctx context.Context, from HoldLister) ([]coord.TrimHold, map[string]time.Duration, error) {
	begun := w.clock()
	holds, err := from.Holds(ctx)
	if err != nil {
		return nil, nil, err
	}
	listed := w.clock()
	seen := make(map[string]sighting, len(holds))
	silent := make(map[string]time.Duration, len(holds))
	for _, hold := range holds {
		if hold.Revision == 0 {
			continue
		}
		was, ok := w.seen[hold.Owner]
		if !ok || was.revision != hold.Revision {
			seen[hold.Owner] = sighting{revision: hold.Revision, first: listed}
			continue
		}
		seen[hold.Owner] = was
		if quiet := begun.Sub(was.first); quiet > 0 {
			silent[hold.Owner] = quiet
		}
	}
	w.seen = seen
	return holds, silent, nil
}
