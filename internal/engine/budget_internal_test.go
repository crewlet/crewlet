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

// AN UNCAPPED METER READS ZERO WITH NO ERROR AND NO STORE ROUND TRIP. Nothing
// is handed one to ask ([Engine.remainingFor] is nil for such a seat); this is
// the answer if something ever is.
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

// EVERY SEAT IS COUNTED, A COMPANY THAT CAPS NOTHING INCLUDED.
//
// The meter used to be nil for such a company, so nothing counted its spend: a
// ceiling added mid-window started from zero, handing the window back the
// whole allowance the morning had already spent, and every reader of the
// counters showed an uncapped company as having spent nothing.
func TestAnUncappedCompanyIsCounted(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	fleet := coordmem.NewFleet()
	free := &org.Role{Name: "Free"}
	c := meteredCompany(config.TokenBudget{}, free)
	e := &Engine{backends: &Backends{Fleet: fleet}}

	m := e.meterFor(c, free.Handle())
	if m == nil {
		t.Fatal("a seat with no ceiling got no meter, so nothing counts its spend")
	}
	if got, err := m.Spend(ctx, 250); err != nil || !got.OK {
		t.Fatalf("Spend = (%+v, %v), want admitted: nothing caps it", got, err)
	}
	windows := coord.WindowsAt(time.Now(), time.UTC)
	for _, scope := range []string{coord.OrgScope, scopeOf(t, c, free)} {
		u, err := fleet.Used(ctx, scope, windows)
		if err != nil || u.In(period.Day).Used != 250 || u.In(period.Month).Used != 250 {
			t.Errorf("%s = (%+v, %v), want the 250 in the day and the month", scope, u, err)
		}
	}
	// And a fan-out is told the seat is uncapped, not that it has 0 left.
	if e.remainingFor(c, free.Handle()) != nil {
		t.Error("an uncapped seat was handed a headroom reader; its zero reads as exhausted")
	}
}

// THE CAPS A TURN IS JUDGED BY ARE THE ONES IT WAS PINNED TO, and the clock is
// its epoch's: a revision that raises a ceiling mid-turn takes effect on the
// NEXT turn, like every other epoch read.
func TestAMeterPinsItsTurnsCapsAndClock(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	fleet := coordmem.NewFleet()
	lead := &org.Role{Name: "Lead", TokenBudget: org.TokenCeilings{period.Day: 100}}
	pinned := meteredCompany(config.TokenBudget{}, lead)
	pinned.Config.Timezone = "Asia/Tokyo"
	e := &Engine{backends: &Backends{Fleet: fleet}}
	m := e.meterFor(pinned, lead.Handle())

	// The revision that lands mid-turn, current from here on.
	raisedLead := &org.Role{Name: "Lead", TokenBudget: org.TokenCeilings{period.Day: 1_000_000}}
	raised := meteredCompany(config.TokenBudget{}, raisedLead)
	e.epoch.current.Store(raised)

	if got, err := m.Spend(ctx, 100); err != nil || !got.OK {
		t.Fatalf("Spend(100) = (%+v, %v)", got, err)
	}
	got, err := m.Spend(ctx, 1)
	if err != nil || got.OK {
		t.Fatalf("a round past the pinned ceiling = (%+v, %v), want refused", got, err)
	}
	tokyo, err := time.LoadLocation("Asia/Tokyo")
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	day := period.At(period.Day, time.Now(), tokyo)
	if got.Period != period.Day || got.Window != day.Label || !got.ResetsAt.Equal(day.End) {
		t.Errorf("refusal = %s %q resets %v, want Tokyo's day %q resetting %v",
			got.Period, got.Window, got.ResetsAt, day.Label, day.End)
	}
	// The NEXT turn is built on the revision, and has room.
	if next, err := e.meterFor(raised, raisedLead.Handle()).Spend(ctx, 1); err != nil || !next.OK {
		t.Errorf("a turn on the raised ceiling = (%+v, %v), want admitted", next, err)
	}
}

// A REFUSED ROUND IS ON BOTH COUNTERS, AND LEAVES NO ROOM FOR A SMALLER ONE.
//
// The meter is handed a round once its reply is in, so the round it refuses
// has been billed. A counter that dropped it read short of the invoice, and
// since the gate decides on room, the next round smaller than the room left
// was admitted on top of tokens the refused one had already spent: at 98 of a
// 100-token day, a 3-token round refused and then a 1-token round let through.
func TestARefusedRoundIsCountedAndLeavesNoRoomForASmallerOne(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	fleet := coordmem.NewFleet()
	lead := &org.Role{Name: "Lead", TokenBudget: org.TokenCeilings{period.Day: 100}}
	c := meteredCompany(config.TokenBudget{}, lead)
	e := &Engine{backends: &Backends{Fleet: fleet}}
	m := e.meterFor(c, lead.Handle())

	if got, err := m.Spend(ctx, 98); err != nil || !got.OK {
		t.Fatalf("Spend(98) = (%+v, %v), want admitted", got, err)
	}
	got, err := m.Spend(ctx, 3)
	if err != nil || got.OK || got.Used != 101 || got.Limit != 100 {
		t.Fatalf("Spend(3) = (%+v, %v), want refused at 101 of 100: the refusal states "+
			"the counter as the round left it", got, err)
	}
	if got, err := m.Spend(ctx, 1); err != nil || got.OK {
		t.Fatalf("a smaller round after the refusal = (%+v, %v), want refused: the "+
			"refused round was paid for, and left no room", got, err)
	}
	windows := coord.WindowsAt(time.Now(), time.UTC)
	for _, scope := range []string{coord.OrgScope, scopeOf(t, c, lead)} {
		u, err := fleet.Used(ctx, scope, windows)
		if err != nil || u.In(period.Day).Used != 102 {
			t.Errorf("%s = (%+v, %v), want all 102 tokens the three rounds spent", scope,
				u.In(period.Day), err)
		}
	}
}

// A ROUND CHARGED AFTER ITS TURN ENDED IS STILL RECORDED, AND THE TURN IS TOLD.
//
// The round was billed when its reply arrived. A turn cancelled between that
// reply and the charge — a node draining, the turn's own deadline — used to
// lose the round from the counter with the charge, because the write ran on
// the turn's dead context. The record now outlives it, and the caller is still
// answered with its own ending, so it stops where it always did and runs none
// of the round's tools.
func TestARoundChargedAfterItsTurnEndedIsStillRecorded(t *testing.T) {
	t.Parallel()
	fleet := coordmem.NewFleet()
	lead := &org.Role{Name: "Lead", TokenBudget: org.TokenCeilings{period.Day: 1000}}
	c := meteredCompany(config.TokenBudget{}, lead)
	m := &meter{
		budgets: deadContextRefused{fleet}, agentScope: scopeOf(t, c, lead),
		basis: basisOf(c, lead), now: time.Now,
	}
	ended, cancel := context.WithCancel(t.Context())
	cancel()

	got, err := m.Spend(ended, 250)
	if !errors.Is(err, context.Canceled) || got.OK {
		t.Fatalf("Spend on an ended turn = (%+v, %v), want the turn's own ending", got, err)
	}
	windows := coord.WindowsAt(time.Now(), time.UTC)
	for _, scope := range []string{coord.OrgScope, scopeOf(t, c, lead)} {
		u, err := fleet.Used(t.Context(), scope, windows)
		if err != nil || u.In(period.Day).Used != 250 {
			t.Errorf("%s = (%+v, %v), want the 250 the round spent", scope, u.In(period.Day), err)
		}
	}
}

// clockedMeter is a seat's meter over fleet, on a clock the case moves.
func clockedMeter(t *testing.T, fleet *coordmem.Fleet, c *Company, seat *org.Role, clock *time.Time) *meter {
	t.Helper()
	return &meter{
		budgets: fleet, agentScope: scopeOf(t, c, seat),
		basis: basisOf(c, seat), now: func() time.Time { return *clock },
	}
}

// A METER HOLDS THE REFUSAL IT ANSWERED UNTIL THE WINDOW TURNS OVER.
//
// The refused round is counted, so the window reads past its ceiling, and the
// turn's ceilings are pinned: every later charge in that window is refused,
// whatever its size. The meter is what every call of the turn charges, so it
// is the one place that can say so before the next call is made — and only
// for as long as that is true, which ends when the window does.
func TestAMeterHoldsTheRefusalItAnsweredUntilTheWindowTurnsOver(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	fleet := coordmem.NewFleet()
	lead := &org.Role{Name: "Lead", TokenBudget: org.TokenCeilings{period.Day: 100}}
	c := meteredCompany(config.TokenBudget{}, lead)
	clock := time.Date(2026, time.March, 14, 15, 0, 0, 0, time.UTC)
	m := clockedMeter(t, fleet, c, lead, &clock)

	if got, held := m.Refused(); held {
		t.Fatalf("a fresh meter holds %+v", got)
	}
	if got, err := m.Spend(ctx, 98); err != nil || !got.OK {
		t.Fatalf("Spend(98) = (%+v, %v), want admitted", got, err)
	}
	if got, held := m.Refused(); held {
		t.Fatalf("a window with room left holds %+v", got)
	}
	if got, err := m.Spend(ctx, 3); err != nil || got.OK {
		t.Fatalf("Spend(3) = (%+v, %v), want refused", got, err)
	}
	got, held := m.Refused()
	midnight := time.Date(2026, time.March, 15, 0, 0, 0, 0, time.UTC)
	if !held || got.OK || got.Scope != "agent" || got.Used != 101 || got.Limit != 100 ||
		got.Period != period.Day || got.Window != "2026-03-14" || !got.ResetsAt.Equal(midnight) {
		t.Fatalf("Refused = (%+v, %v), want the seat's day as the counter refused it", got, held)
	}

	clock = midnight
	if got, held := m.Refused(); held {
		t.Fatalf("Refused after the window turned over = %+v, want nothing: the next "+
			"charge is judged against a new day", got)
	}
}

// AN ADMITTED ROUND THAT FILLS A WINDOW LEAVES NOTHING TO ADMIT.
//
// Nothing refused it — it fitted exactly — but the window has no room left for
// a single token, which is the same "refusing" the budget park parks a seat
// on (windowRefuses), and the next call would be billed and refused.
func TestAnAdmittedRoundThatFillsAWindowIsHeld(t *testing.T) {
	t.Parallel()
	fleet := coordmem.NewFleet()
	lead := &org.Role{Name: "Lead"}
	c := meteredCompany(config.TokenBudget{Week: ceiling(100)}, lead)
	clock := time.Date(2026, time.March, 14, 15, 0, 0, 0, time.UTC)
	m := clockedMeter(t, fleet, c, lead, &clock)

	if got, err := m.Spend(t.Context(), 100); err != nil || !got.OK {
		t.Fatalf("Spend(100) = (%+v, %v), want admitted at the ceiling", got, err)
	}
	got, held := m.Refused()
	if !held || got.Scope != "org" || got.Period != period.Week || got.Used != 100 || got.Limit != 100 {
		t.Fatalf("Refused = (%+v, %v), want the company's full week", got, held)
	}
}

// A HELD REFUSAL IS NAMED AS THE COUNTER WOULD NAME IT NOW: the company before
// the seat, because the company is judged first; and once the company's window
// turns over, the seat's that still stands.
func TestAHeldRefusalNamesTheCompanyFirst(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	fleet := coordmem.NewFleet()
	lead := &org.Role{Name: "Lead", TokenBudget: org.TokenCeilings{period.Month: 50}}
	c := meteredCompany(config.TokenBudget{Day: ceiling(100)}, lead)
	clock := time.Date(2026, time.March, 14, 15, 0, 0, 0, time.UTC)
	m := clockedMeter(t, fleet, c, lead, &clock)

	if got, err := m.Spend(ctx, 60); err != nil || got.Scope != "agent" {
		t.Fatalf("Spend(60) = (%+v, %v), want the seat's month refusing", got, err)
	}
	if got, err := m.Spend(ctx, 50); err != nil || got.Scope != "org" {
		t.Fatalf("Spend(50) = (%+v, %v), want the company's day refusing", got, err)
	}
	if got, held := m.Refused(); !held || got.Scope != "org" || got.Period != period.Day {
		t.Fatalf("Refused = (%+v, %v), want the company's day named before the seat's month", got, held)
	}
	clock = time.Date(2026, time.March, 15, 9, 0, 0, 0, time.UTC)
	if got, held := m.Refused(); !held || got.Scope != "agent" || got.Period != period.Month {
		t.Fatalf("Refused the next day = (%+v, %v), want the seat's month, which still stands", got, held)
	}
}

// A REFUSAL ANSWERED TO A CALLER THAT HUNG UP IS HELD ALL THE SAME. The window
// is full whether or not that caller is listening, and the turn's next call is
// made by somebody else.
func TestARefusalAnsweredToACallerThatHungUpIsHeld(t *testing.T) {
	t.Parallel()
	fleet := coordmem.NewFleet()
	lead := &org.Role{Name: "Lead", TokenBudget: org.TokenCeilings{period.Day: 100}}
	c := meteredCompany(config.TokenBudget{}, lead)
	clock := time.Date(2026, time.March, 14, 15, 0, 0, 0, time.UTC)
	m := clockedMeter(t, fleet, c, lead, &clock)
	ended, cancel := context.WithCancel(t.Context())
	cancel()

	if _, err := m.Spend(ended, 250); !errors.Is(err, context.Canceled) {
		t.Fatalf("Spend on an ended turn = %v, want the turn's own ending", err)
	}
	if got, held := m.Refused(); !held || got.Scope != "agent" || got.Used != 250 {
		t.Fatalf("Refused = (%+v, %v), want the refusal the hung-up charge met", got, held)
	}
}

// deadContextRefused is the in-memory counter with the one property of the
// broker's it lacks: a request on a context that is done fails, as every
// compare-and-swap the KV backend makes does.
type deadContextRefused struct{ budgets budgetCounter }

func (d deadContextRefused) Charge(ctx context.Context, req coord.ChargeRequest) (coord.Spend, error) {
	if err := ctx.Err(); err != nil {
		return coord.Spend{}, err
	}
	return d.budgets.Charge(ctx, req)
}

func (d deadContextRefused) Used(ctx context.Context, scope string, w coord.Windows) (coord.Usage, error) {
	if err := ctx.Err(); err != nil {
		return coord.Usage{}, err
	}
	return d.budgets.Used(ctx, scope, w)
}
