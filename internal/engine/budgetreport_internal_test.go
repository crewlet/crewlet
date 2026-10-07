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
// day that has spent 250 today, and the fleet store it reads — everything a frame reads, and nothing a
// publish adds — on an engine whose clock reads now, the instant the 250 were
// charged at.
func meteringReporter(t *testing.T, now time.Time) (*budgetReporter, *coordmem.Fleet) {
	t.Helper()
	fleet := coordmem.NewFleet()
	windows := coord.WindowsAt(now, time.UTC)
	if _, err := fleet.PostCharge(t.Context(), coord.AgentScope("x"), 250, windows); err != nil {
		t.Fatalf("PostCharge: %v", err)
	}
	e := &Engine{backends: &Backends{Fleet: fleet}, clock: fixedClock(now)}
	r := &budgetReporter{engine: e}
	e.epoch.current.Store(meteredCompany(config.TokenBudget{Day: ceiling(1000)}))
	return r, fleet
}

// A CAPPED COMPANY'S FRAME IS WHAT THE SHARED COUNTERS READ, from the first
// frame on: every node of a fleet charges the windowed counters, so there is
// no reading to wait for before this node's is the fleet's.
//
// Asked of [budgetReporter.frame], the decision [budgetReporter.publish]
// sends or does not send.
func TestACappedCompanyPublishesWhatItsCountersRead(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, time.September, 23, 12, 0, 0, 0, time.UTC)
	r, _ := meteringReporter(t, now)
	frame, sent := r.frame(t.Context())
	if !sent {
		t.Fatal("a capped company whose counters can be read published nothing")
	}
	if w := frame.Org.Windows; len(w) != 1 || w[0].Used != 250 || w[0].Limit == nil || *w[0].Limit != 1000 {
		t.Fatalf("frame = %+v, want the company's day at 250 of its 1000", w)
	}
}

// A COUNTER THAT CANNOT BE READ PUBLISHES NOTHING, rather than a frame of
// zeroes: the consumer replaces what it holds on every frame, so a zeroed one
// would draw a company that is spending as one that has spent nothing.
func TestAnUnreadableCounterPublishesNoFrame(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, time.September, 23, 12, 0, 0, 0, time.UTC)
	r, fleet := meteringReporter(t, now)
	r.engine.backends.Fleet = unreadableUsage{fleet}
	if frame, sent := r.frame(t.Context()); sent {
		t.Fatalf("the meter published %+v on a counter nobody could read", frame)
	}
}

// unreadableUsage is a fleet store whose counters cannot be read, embedded
// through [fleetStore] for the reason that alias gives.
type unreadableUsage struct{ *fleetStore }

func (unreadableUsage) Usage(context.Context, coord.Windows) ([]coord.Usage, error) {
	return nil, errors.New("the coordination store is unreachable")
}

// A COMPANY THAT CAPS NOTHING IS PUBLISHED, AND READS NOTHING TO SAY SO.
//
// Its frame is "there is no ceiling", which the dashboard must be able to tell
// from "no node has reported yet" — so it is sent, where it used to be
// withheld and the two collapsed into one empty meter that told the operator
// of a CAPPED company, for the first interval after every start, that it had
// no budget. And it is sent without reading the counters, because an empty
// list of windows holds no figure a reading could make wrong: the counters
// here cannot be read at all.
func TestAnUncappedCompanyPublishesItsNoCeilingWithoutAReading(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, time.September, 23, 12, 0, 0, 0, time.UTC)
	r, fleet := meteringReporter(t, now)
	r.engine.backends.Fleet = unreadableUsage{fleet}
	r.engine.epoch.current.Store(meteredCompany(config.TokenBudget{}))
	frame, sent := r.frame(t.Context())
	if !sent {
		t.Fatal("an uncapped company published nothing, which a reader cannot tell from no report")
	}
	if frame.Metered() || frame.Org.Windows == nil {
		t.Fatalf("frame = %+v, want the org as an empty list and no seat", frame)
	}
}
