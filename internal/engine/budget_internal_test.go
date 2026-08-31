package engine

import (
	"context"
	"errors"
	"testing"

	"github.com/crewlet/crewlet/internal/coord"
)

// counters is a token counter whose reads the test controls.
type counters struct {
	used map[string]int
	err  error
}

func (c counters) Used(_ context.Context, scope string) (int, error) {
	if c.err != nil {
		return 0, c.err
	}
	return c.used[scope], nil
}

func (c counters) Charge(context.Context, string, int, int, int) (coord.Spend, error) {
	return coord.Spend{}, errors.New("not used by these cases")
}

func (c counters) Usage(context.Context) ([]coord.Usage, error) { return nil, nil }
func (c counters) Reset(context.Context, string) (int, error)   { return 0, nil }

// THE HEADROOM IS THE TIGHTER OF THE TWO CAPS.
//
// A charge is checked against both, so a seat with room under its own cap and
// none under the company's has no room. Reporting the looser one would let a
// fan-out size itself against an allowance it cannot spend.
func TestTheHeadroomIsTheTighterCap(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name                 string
		orgLimit, agentLimit int
		orgUsed, agentUsed   int
		want                 int
	}{
		{"the org is tighter", 1000, 1000, 900, 100, 100},
		{"the seat is tighter", 1000, 200, 100, 150, 50},
		{"only the org is capped", 1000, 0, 400, 0, 600},
		{"only the seat is capped", 0, 300, 0, 100, 200},
		// A scope that has spent its whole allowance HAS zero headroom.
		// Reading that as "not set yet" would let the other scope's room
		// overwrite it — an exhausted company reading as an uncapped one
		// at exactly the moment the cap matters.
		{"an exhausted org is zero, not unset", 500, 10_000, 500, 0, 0},
		{"an overspent scope is zero, never negative", 500, 0, 900, 0, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			m := &meter{
				budgets:    counters{used: map[string]int{coord.OrgScope: tc.orgUsed, "agent:x": tc.agentUsed}},
				agentScope: "agent:x", orgLimit: tc.orgLimit, agentLimit: tc.agentLimit,
			}
			got, err := m.Remaining(t.Context())
			if err != nil {
				t.Fatalf("Remaining: %v", err)
			}
			if got != tc.want {
				t.Errorf("headroom = %d, want %d", got, tc.want)
			}
		})
	}
}

// AN UNREACHABLE COUNTER IS AN ERROR, NEVER A ZERO.
//
// subagent reads a ParentRemaining of zero as UNCAPPED, so collapsing an
// unreadable store to 0 would hand a fan-out no ceiling on exactly the failure
// a budget exists for — the fail-OPEN direction, on the one path where money
// leaves the building per token.
func TestAnUnreachableCounterRefusesRatherThanReportingZero(t *testing.T) {
	t.Parallel()
	m := &meter{
		budgets:    counters{err: errors.New("the coordination store is unreachable")},
		agentScope: "agent:x", orgLimit: 1000,
	}
	if _, err := m.Remaining(t.Context()); err == nil {
		t.Fatal("an unreadable counter reported a headroom")
	}
}

// AN UNCAPPED SEAT READS ZERO WITH NO ERROR AND NO STORE ROUND TRIP, which is
// the same "no ceiling" a company that set no budget already has.
func TestAnUncappedSeatNeedsNoCounterRead(t *testing.T) {
	t.Parallel()
	m := &meter{
		budgets:    counters{err: errors.New("this must not be called")},
		agentScope: "agent:x",
	}
	got, err := m.Remaining(t.Context())
	if err != nil || got != 0 {
		t.Errorf("Remaining = (%d, %v), want (0, nil)", got, err)
	}
}
