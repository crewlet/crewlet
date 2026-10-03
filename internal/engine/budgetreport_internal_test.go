package engine

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/coord"
	coordmem "github.com/crewlet/crewlet/internal/coord/memory"
)

// meteringReporter is a meter loop over a company capped at 1 000 tokens a
// day, reading the given shared counters — everything a frame reads, and
// nothing a publish adds.
func meteringReporter(t *testing.T, counters coord.Fleet) *budgetReporter {
	t.Helper()
	e := &Engine{backends: &Backends{Fleet: counters}}
	e.epoch.current.Store(meteredCompany(config.TokenBudget{Day: ceiling(1000)}))
	return &budgetReporter{engine: e}
}

// spentCounters is the shared counters with 250 tokens charged in now's day.
func spentCounters(t *testing.T, now time.Time) coord.Fleet {
	t.Helper()
	fleet := coordmem.NewFleet()
	if _, err := fleet.PostCharge(t.Context(), coord.AgentScope("x"), 250,
		coord.WindowsAt(now, time.UTC)); err != nil {
		t.Fatalf("PostCharge: %v", err)
	}
	return fleet
}

// A CAPPED COMPANY'S FRAME IS ONE READ OF THE SHARED COUNTERS, stated against
// its own ceiling.
//
// Asked of [budgetReporter.frame], the decision [budgetReporter.publish]
// sends or does not send, rather than of [budgetSnapshot] alone: a snapshot
// nothing reads the counters into passes every test of the snapshot.
func TestACappedCompanysFrameIsTheSharedCounter(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, time.September, 23, 12, 0, 0, 0, time.UTC)
	r := meteringReporter(t, spentCounters(t, now))
	frame, sent := r.frame(t.Context(), now)
	if !sent {
		t.Fatal("a capped company whose counters could be read published nothing")
	}
	if w := frame.Org.Windows; len(w) != 1 || w[0].Used != 250 || w[0].Limit == nil || *w[0].Limit != 1000 {
		t.Fatalf("frame = %+v, want the company's day at 250 of its 1000", w)
	}
}

// A COUNTER THAT CANNOT BE READ PUBLISHES NO FRAME. The consumer replaces what
// it holds on every report, so a frame of zeroes would render a company that
// is spending as one that has spent nothing; a frame skipped costs one
// interval of a meter that keeps its last reading.
func TestAnUnreadableCounterPublishesNoFrame(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, time.September, 23, 12, 0, 0, 0, time.UTC)
	r := meteringReporter(t, unreadableCounters{spentCounters(t, now)})
	if frame, sent := r.frame(t.Context(), now); sent {
		t.Fatalf("the meter published %+v from counters nobody could read", frame)
	}
}

// unreadableCounters is a coordination store whose token counters cannot be
// listed.
//
// The store is embedded under an alias because [coord.Fleet] has a method of
// that name (the config plane's per-node apply status), which a field named
// Fleet would shadow.
type unreadableCounters struct{ sharedState }

type sharedState = coord.Fleet

func (unreadableCounters) Usage(context.Context, coord.Windows) ([]coord.Usage, error) {
	return nil, errors.New("the coordination store is unreachable")
}

// A COMPANY THAT CAPS NOTHING IS PUBLISHED, AND READS NOTHING TO SAY SO.
//
// Its frame is "there is no ceiling", which the dashboard must be able to tell
// from "no node has reported yet" — so it is sent, where it used to be
// withheld and the two collapsed into one empty meter that told the operator
// of a CAPPED company, for the first interval after every start, that it had
// no budget. And it is sent whatever the counters hold, because an empty list
// of windows holds no figure a reading could change: the counters here cannot
// be read at all.
func TestAnUncappedCompanyPublishesItsNoCeilingWithoutAReading(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, time.September, 23, 12, 0, 0, 0, time.UTC)
	r := meteringReporter(t, unreadableCounters{coordmem.NewFleet()})
	r.engine.epoch.current.Store(meteredCompany(config.TokenBudget{}))
	frame, sent := r.frame(t.Context(), now)
	if !sent {
		t.Fatal("an uncapped company published nothing, which a reader cannot tell from no report")
	}
	if frame.Metered() || frame.Org.Windows == nil {
		t.Fatalf("frame = %+v, want the org as an empty list and no seat", frame)
	}
}
