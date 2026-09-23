package engine

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/coord"
	coordmem "github.com/crewlet/crewlet/internal/coord/memory"
	"github.com/crewlet/crewlet/internal/org"
	"github.com/crewlet/crewlet/internal/period"
)

// counters is a token counter whose reads the test controls: one figure per
// scope and period, read against whatever windows the meter asks about.
type counters struct {
	used map[string]map[period.Period]int
	err  error
}

func (c counters) Used(_ context.Context, scope string, w coord.Windows) (coord.Usage, error) {
	if c.err != nil {
		return coord.Usage{}, c.err
	}
	u := coord.Unspent(scope, w)
	for i, p := range period.Periods {
		u.Windows[i].Used = c.used[scope][p]
	}
	return u, nil
}

func (c counters) Charge(context.Context, coord.ChargeRequest) (coord.Spend, error) {
	return coord.Spend{}, errors.New("not used by these cases")
}

// THE HEADROOM IS THE TIGHTEST WINDOW OF EITHER SCOPE.
//
// A charge is admitted only while every capped window of both scopes has room,
// so a seat with a week to spare and nothing left in the company's day has no
// room. Reporting any looser window would let a fan-out size itself against
// an allowance it cannot spend.
func TestTheHeadroomIsTheTightestCappedWindow(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name      string
		org, seat coord.Caps
		used      map[string]map[period.Period]int
		want      int
	}{
		{"the org's day is tightest",
			coord.Caps{period.Day: 1000}, coord.Caps{period.Day: 1000},
			map[string]map[period.Period]int{coord.OrgScope: {period.Day: 900}, "agent:x": {period.Day: 100}}, 100},
		{"the seat's week is tightest",
			coord.Caps{period.Day: 1000}, coord.Caps{period.Week: 200},
			map[string]map[period.Period]int{coord.OrgScope: {period.Day: 100}, "agent:x": {period.Week: 150}}, 50},
		{"a month nearly spent beats a fresh day",
			coord.Caps{period.Day: 1000, period.Month: 5000}, nil,
			map[string]map[period.Period]int{coord.OrgScope: {period.Day: 0, period.Month: 4980}}, 20},
		{"only the org is capped",
			coord.Caps{period.Week: 1000}, nil,
			map[string]map[period.Period]int{coord.OrgScope: {period.Week: 400}}, 600},
		{"only the seat is capped",
			nil, coord.Caps{period.Day: 300},
			map[string]map[period.Period]int{"agent:x": {period.Day: 100}}, 200},
		// A window that has spent its whole allowance HAS zero headroom.
		// Reading that as "not set yet" would let another window's room
		// overwrite it — an exhausted company reading as an uncapped one
		// at exactly the moment the cap matters.
		{"an exhausted window is zero, not unset",
			coord.Caps{period.Day: 500}, coord.Caps{period.Month: 10_000},
			map[string]map[period.Period]int{coord.OrgScope: {period.Day: 500}}, 0},
		{"an overspent window is zero, never negative",
			coord.Caps{period.Day: 500}, nil,
			map[string]map[period.Period]int{coord.OrgScope: {period.Day: 900}}, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			m := &meter{
				budgets: counters{used: tc.used}, agentScope: "agent:x",
				basis: budgetBasis{org: tc.org, seat: tc.seat, zone: time.UTC}, now: time.Now,
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
		agentScope: "agent:x", basis: budgetBasis{org: coord.Caps{period.Day: 1000}, zone: time.UTC},
		now: time.Now,
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
		agentScope: "agent:x", basis: budgetBasis{zone: time.UTC}, now: time.Now,
	}
	got, err := m.Remaining(t.Context())
	if err != nil || got != 0 {
		t.Errorf("Remaining = (%d, %v), want (0, nil)", got, err)
	}
}

// A ROUND IS CHARGED TO THE DAY ON THE COMPANY'S CLOCK, the one the turn's
// epoch names, at the moment it is charged.
//
// 23:30 in Los Angeles is 06:30 the next day in UTC. A meter that cut its day
// on UTC would charge a company's late evening to tomorrow — and hand back
// today's allowance seven hours early every day.
func TestARoundIsChargedToTheCompanysDay(t *testing.T) {
	t.Parallel()
	la, err := time.LoadLocation("America/Los_Angeles")
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	fleet := coordmem.NewFleet()
	evening := time.Date(2026, time.September, 22, 23, 30, 0, 0, la)
	m := &meter{
		budgets: fleet, agentScope: "agent:x",
		basis: budgetBasis{org: coord.Caps{period.Day: 100}, zone: la},
		now:   func() time.Time { return evening },
	}
	if got, err := m.Spend(t.Context(), 100); err != nil || !got.OK {
		t.Fatalf("Spend = (%+v, %v)", got, err)
	}
	onLA := coord.WindowsAt(evening, la)
	u, err := fleet.Used(t.Context(), coord.OrgScope, onLA)
	if err != nil {
		t.Fatalf("Used: %v", err)
	}
	if u.In(period.Day).Used != 100 || u.In(period.Day).Window.Label != "2026-09-22" {
		t.Fatalf("the Los Angeles day reads %+v, want the 100 charged at 23:30 on the 22nd",
			u.In(period.Day))
	}
	// And the day refuses at its own midnight, not at UTC's.
	if got, err := m.Spend(t.Context(), 1); err != nil || got.OK {
		t.Fatalf("a charge past the day's ceiling = (%+v, %v), want refused", got, err)
	}
}

// THE BASIS IS THE EPOCH'S: its caps, the seat's own and its clock.
func TestTheBasisIsReadOffTheEpoch(t *testing.T) {
	t.Parallel()
	seat := &org.Role{Name: "Lead", TokenBudget: org.TokenCeilings{period.Week: 700}}
	c := &Company{
		Config: &config.Company{Timezone: "Europe/Berlin"},
		Org:    &org.Organization{Name: "Acme", Roles: []*org.Role{seat}, TokenBudget: org.TokenCeilings{period.Day: 50}},
	}
	b := basisOf(c, seat)
	if b.org[period.Day] != 50 || b.seat[period.Week] != 700 || b.zone.String() != "Europe/Berlin" {
		t.Fatalf("basis = %+v, want the company's day, the seat's week and Berlin's clock", b)
	}
	if basisOf(nil, nil).zone != time.UTC {
		t.Fatal("a missing epoch did not read as UTC")
	}
}
