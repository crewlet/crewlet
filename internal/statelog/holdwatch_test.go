package statelog_test

import (
	"context"
	"slices"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/statelog"
)

// listedHolds is a register whose listing is scripted, on a clock the case
// steps: each listing takes during on that clock.
type listedHolds struct {
	clock  *stepClock
	during time.Duration
	holds  []coord.TrimHold
}

func (l *listedHolds) Holds(context.Context) ([]coord.TrimHold, error) {
	l.clock.advance(l.during)
	return slices.Clone(l.holds), nil
}

// watchedHold is one owner's hold at a revision, pinning heldStream at 4000.
func watchedHold(owner string, revision uint64) coord.TrimHold {
	return coord.TrimHold{Owner: owner, Revision: revision,
		Streams: map[string]coord.Position{heldStream: {Stream: heldStream, Generation: 1, Seq: 4_000}}}
}

// watchCase is a watch over a scripted register, its clock starting at t0.
type watchCase struct {
	t      *testing.T
	t0     time.Time
	clock  *stepClock
	lister *listedHolds
	watch  *statelog.HoldWatch
}

func newWatchCase(t *testing.T) *watchCase {
	t0 := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	clock := &stepClock{now: t0}
	return &watchCase{t: t, t0: t0, clock: clock, lister: &listedHolds{clock: clock},
		watch: &statelog.HoldWatch{Now: clock.Now}}
}

// listAt lists holds with the watch at t0+at, and answers the owner's watched
// silence and the trim's min_hold term over that listing.
func (c *watchCase) listAt(at time.Duration, owner string, holds ...coord.TrimHold) (time.Duration, statelog.Term) {
	c.t.Helper()
	c.clock.set(c.t0.Add(at))
	c.lister.holds = holds
	listed, silent, err := c.watch.Read(c.t.Context(), c.lister)
	if err != nil {
		c.t.Fatalf("Read: %v", err)
	}
	return silent[owner], minHoldOver(listed, silent)
}

// minHoldOver is the trim's min_hold term over a listing and its silences.
func minHoldOver(holds []coord.TrimHold, silent map[string]time.Duration) statelog.Term {
	in := baseInputs()
	for _, h := range holds {
		pos := h.Streams[heldStream]
		in.Holds = append(in.Holds, statelog.Hold{Owner: h.Owner, Generation: pos.Generation,
			Seq: pos.Seq, Silent: silent[h.Owner]})
	}
	for _, term := range in.Terms() {
		if term.Name == statelog.TermMinHold {
			return term
		}
	}
	return statelog.Term{}
}

// pinsTheLog reports whether the term holds the log at the scripted hold's 4000.
func pinsTheLog(term statelog.Term) bool { return term.Known && term.Seq == 4_000 }

// A RENEWED HOLD IS NEVER IGNORED, however long the trim watches it: each
// listing meets a new write, so no write was ever watched standing.
func TestARenewedHoldIsNeverIgnored(t *testing.T) {
	t.Parallel()
	c := newWatchCase(t)
	for i := range 5 {
		silent, term := c.listAt(time.Duration(i)*statelog.TrimInterval, "n0/join",
			watchedHold("n0/join", uint64(10+i)))
		if silent != 0 || !pinsTheLog(term) {
			t.Fatalf("listing %d, %v into the watch, reads the renewed hold "+
				"silent for %v (%s): a holder renewing between listings is alive",
				i+1, time.Duration(i)*statelog.TrimInterval, silent, term.Detail)
		}
	}
}

// A HOLD IS IGNORED ONLY ONCE ONE WRITE WAS WATCHED PAST THE BOUND, counted
// from that write's first listing, not the owner's, and exactly the bound
// still pins.
func TestAHoldIsIgnoredOnlyOnceOneWriteWasWatchedPastTheBound(t *testing.T) {
	t.Parallel()
	c := newWatchCase(t)
	c.listAt(0, "n0/join", watchedHold("n0/join", 1))
	if silent, term := c.listAt(50*time.Second, "n0/join", watchedHold("n0/join", 2)); silent != 0 || !pinsTheLog(term) {
		t.Fatalf("a renewal listed for the first time reads silent for %v (%s)", silent, term.Detail)
	}
	if silent, term := c.listAt(100*time.Second, "n0/join", watchedHold("n0/join", 2)); silent != 50*time.Second || !pinsTheLog(term) {
		t.Fatalf("the renewal watched 50s reads silent for %v (%s), want 50s and "+
			"pinned: the owner's first write is not the one standing", silent, term.Detail)
	}
	edge := 50*time.Second + statelog.TrimHoldStale
	if silent, term := c.listAt(edge, "n0/join", watchedHold("n0/join", 2)); silent != statelog.TrimHoldStale || !pinsTheLog(term) {
		t.Fatalf("a write watched exactly the bound reads silent for %v (%s), "+
			"want %v and pinned", silent, term.Detail, statelog.TrimHoldStale)
	}
	if silent, term := c.listAt(edge+time.Nanosecond, "n0/join", watchedHold("n0/join", 2)); pinsTheLog(term) {
		t.Fatalf("a write watched %v unrenewed, past the %v bound, still pins "+
			"(%s): a crashed holder would pin the log for ever", silent,
			statelog.TrimHoldStale, term.Detail)
	}
}

// A LISTING'S OWN TIME IS NEVER COUNTED AS SILENCE: a write is first seen
// once its listing returns, and seen again as the next listing begins, so the
// silence is a lower bound on the holder's.
func TestAListingsOwnTimeIsNeverCountedAsSilence(t *testing.T) {
	t.Parallel()
	c := newWatchCase(t)
	c.lister.during = 10 * time.Second
	c.listAt(0, "n0/join", watchedHold("n0/join", 1))
	c.lister.during = 5 * time.Second
	silent, term := c.listAt(70*time.Second, "n0/join", watchedHold("n0/join", 1))
	if silent != statelog.TrimHoldStale || !pinsTheLog(term) {
		t.Fatalf("a write first listed by 10s and listed again from 70s reads "+
			"silent for %v (%s), want %v and pinned: a listing's own duration "+
			"is time the holder may have renewed in", silent, term.Detail,
			statelog.TrimHoldStale)
	}
}

// A HOLD READ WITH NO REVISION IS NEVER IGNORED: nothing names its write, so
// nothing says it was the same one.
func TestAHoldReadWithNoRevisionIsNeverIgnored(t *testing.T) {
	t.Parallel()
	c := newWatchCase(t)
	c.listAt(0, "n0/join", watchedHold("n0/join", 0))
	if silent, term := c.listAt(10*time.Minute, "n0/join", watchedHold("n0/join", 0)); silent != 0 || !pinsTheLog(term) {
		t.Fatalf("a hold with no revision reads silent for %v (%s), want pinned", silent, term.Detail)
	}
}
