package engine

import (
	"context"
	"errors"
	"maps"
	"sync"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/agent/toolloop"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/coord"
	coordmem "github.com/crewlet/crewlet/internal/coord/memory"
	"github.com/crewlet/crewlet/internal/org"
	"github.com/crewlet/crewlet/internal/period"
)

// budgetNow is the instant every case that charges through an engine pins
// [Engine.clock] to, and cuts the windows it reads back in at. PINNED: a charge
// and the read that checks it are two clock reads, and a midnight between them
// on the wall clock reads the charge's day as unspent.
var budgetNow = time.Date(2026, time.June, 14, 12, 0, 0, 0, time.UTC)

// fixedClock is a clock that always reads at.
func fixedClock(at time.Time) func() time.Time { return func() time.Time { return at } }

// spentIn is what scope has spent in the p window of windows, failing the case
// when the counter is on a different window of that period.
//
// THE WINDOW AS WELL AS THE FIGURE: a counter never rolls back, so a read of an
// EARLIER window answers the later one's spend (coord.Tally.Usage), and a charge
// cut on the wall clock would still read back as the right number.
func spentIn(t *testing.T, fleet *coordmem.Fleet, scope string, windows coord.Windows, p period.Period) int {
	t.Helper()
	u, err := fleet.Used(t.Context(), scope, windows)
	if err != nil {
		t.Fatalf("Used(%s): %v", scope, err)
	}
	slot, want := u.In(p), coord.Unspent(scope, windows).In(p).Window
	if slot.Window.Label != want.Label {
		t.Errorf("%s's %s counter is on window %q, want %q: the charge was cut on "+
			"another clock than the one the case pinned", scope, p, slot.Window.Label, want.Label)
	}
	return slot.Used
}

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

func (c counters) PostCharge(context.Context, string, int, coord.Windows) (coord.Spend, error) {
	return coord.Spend{}, errors.New("not used by these cases")
}

func (c counters) Refuse(context.Context, string, coord.Caps, coord.Windows) (coord.Usage, error) {
	return coord.Usage{}, errors.New("not used by these cases")
}

// THE HEADROOM IS THE TIGHTEST WINDOW OF EITHER SCOPE.
//
// A charge is admitted only while every capped window of both scopes has room;
// reporting any looser window lets a fan-out size itself against an allowance
// it cannot spend.
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
		// An exhausted window HAS zero headroom: read as "not set yet",
		// another window's room would overwrite it and an exhausted company
		// would read as uncapped.
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
				basis: budgetBasis{org: tc.org, seat: tc.seat, zone: time.UTC}, now: fixedClock(budgetNow),
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
// unreadable store to 0 would fail OPEN, handing a fan-out no ceiling.
func TestAnUnreachableCounterRefusesRatherThanReportingZero(t *testing.T) {
	t.Parallel()
	m := &meter{
		budgets:    counters{err: errors.New("the coordination store is unreachable")},
		agentScope: "agent:x", basis: budgetBasis{org: coord.Caps{period.Day: 1000}, zone: time.UTC},
		now: fixedClock(budgetNow),
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
		agentScope: "agent:x", basis: budgetBasis{zone: time.UTC}, now: fixedClock(budgetNow),
	}
	got, err := m.Remaining(t.Context())
	if err != nil || got != 0 {
		t.Errorf("Remaining = (%d, %v), want (0, nil)", got, err)
	}
}

// A ROUND IS CHARGED TO THE DAY ON THE COMPANY'S CLOCK, the one the turn's
// epoch names, at the moment it is charged.
//
// 23:30 in Los Angeles is 06:30 the next day in UTC: a day cut on UTC charges
// the evening to tomorrow and hands back the allowance seven hours early.
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
// With no meter, a ceiling added mid-window would start from zero, handing back
// the allowance the morning already spent, and every reader of the counters
// would show an uncapped company as having spent nothing.
func TestAnUncappedCompanyIsCounted(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	fleet := coordmem.NewFleet()
	free := &org.Role{Name: "Free"}
	c := meteredCompany(config.TokenBudget{}, free)
	e := &Engine{backends: &Backends{Fleet: fleet}, clock: fixedClock(budgetNow)}

	m := e.meterFor(c, free.Handle())
	if m == nil {
		t.Fatal("a seat with no ceiling got no meter, so nothing counts its spend")
	}
	if got, err := m.Spend(ctx, 250); err != nil || !got.OK {
		t.Fatalf("Spend = (%+v, %v), want admitted: nothing caps it", got, err)
	}
	windows := coord.WindowsAt(budgetNow, time.UTC)
	for _, scope := range []string{coord.OrgScope, scopeOf(t, c, free)} {
		day := spentIn(t, fleet, scope, windows, period.Day)
		month := spentIn(t, fleet, scope, windows, period.Month)
		if day != 250 || month != 250 {
			t.Errorf("%s spent %d in the day and %d in the month, want the 250 in each",
				scope, day, month)
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
	// AN INSTANT WHOSE TOKYO DATE IS NOT ITS UTC DATE (20:00 UTC on the 22nd
	// is 05:00 on the 23rd in Tokyo), so a day cut on the wrong zone names the
	// wrong date. Pinned on the engine: the two charges and the expectation
	// are three clock reads, which Tokyo's midnight could split.
	at := time.Date(2026, time.September, 22, 20, 0, 0, 0, time.UTC)
	e := &Engine{backends: &Backends{Fleet: fleet}, clock: fixedClock(at)}
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
	day := period.At(period.Day, at, tokyo)
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
// A round reaches the meter once its reply is in, so a refused round was
// billed. Dropping it reads the counter short, and a smaller next round is
// admitted on top: at 98 of 100, a 3-token round refused, then a 1-token one
// let through.
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
// The round was billed when its reply arrived, so a turn cancelled before the
// charge (a node draining, the turn's deadline) must not lose it: the record
// outlives the turn's context, while the caller is still answered with its
// own ending and runs none of the round's tools.
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
// The refused round is counted and the turn's ceilings are pinned, so every
// later charge in that window is refused. The meter every call charges through
// is the one place that can say so before the next call, until the window ends.
func TestAMeterHoldsTheRefusalItAnsweredUntilTheWindowTurnsOver(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	fleet := coordmem.NewFleet()
	lead := &org.Role{Name: "Lead", TokenBudget: org.TokenCeilings{period.Day: 100}}
	c := meteredCompany(config.TokenBudget{}, lead)
	clock := time.Date(2026, time.March, 14, 15, 0, 0, 0, time.UTC)
	m := clockedMeter(t, fleet, c, lead, &clock)

	if got, held := m.Refused(t.Context()); held {
		t.Fatalf("a fresh meter holds %+v", got)
	}
	if got, err := m.Spend(ctx, 98); err != nil || !got.OK {
		t.Fatalf("Spend(98) = (%+v, %v), want admitted", got, err)
	}
	if got, held := m.Refused(t.Context()); held {
		t.Fatalf("a window with room left holds %+v", got)
	}
	if got, err := m.Spend(ctx, 3); err != nil || got.OK {
		t.Fatalf("Spend(3) = (%+v, %v), want refused", got, err)
	}
	got, held := m.Refused(t.Context())
	midnight := time.Date(2026, time.March, 15, 0, 0, 0, 0, time.UTC)
	if !held || got.OK || got.Scope != "agent" || got.Used != 101 || got.Limit != 100 ||
		got.Period != period.Day || got.Window != "2026-03-14" || !got.ResetsAt.Equal(midnight) {
		t.Fatalf("Refused = (%+v, %v), want the seat's day as the counter refused it", got, held)
	}

	clock = midnight
	if got, held := m.Refused(t.Context()); held {
		t.Fatalf("Refused after the window turned over = %+v, want nothing: the next "+
			"charge is judged against a new day", got)
	}
}

// AN ADMITTED ROUND THAT FILLS A WINDOW LEAVES NOTHING TO ADMIT.
//
// It fitted exactly, but the window has no room for a token: the "refusing"
// the budget park parks a seat on (windowRefuses), so the next call would be
// billed and refused.
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
	got, held := m.Refused(t.Context())
	if !held || got.Scope != "org" || got.Period != period.Week || got.Used != 100 || got.Limit != 100 {
		t.Fatalf("Refused = (%+v, %v), want the company's full week", got, held)
	}
}

// AN AUXILIARY CALL'S POST-CHARGE THAT FILLS A WINDOW IS HELD, as a round's
// answer is.
//
// Auxiliary calls reach the same counters by a post-charge that refuses
// nothing. Charged through the meter, its answer is kept, so the next round and
// auxiliary call are held rather than sent, billed and refused. The spend is
// counted whole on both scopes, past the ceiling included.
func TestAnAuxiliaryPostChargeThatFillsAWindowIsHeld(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	fleet := coordmem.NewFleet()
	lead := &org.Role{Name: "Lead", TokenBudget: org.TokenCeilings{period.Day: 100}}
	c := meteredCompany(config.TokenBudget{Month: ceiling(10_000)}, lead)
	clock := time.Date(2026, time.March, 14, 15, 0, 0, 0, time.UTC)
	m := clockedMeter(t, fleet, c, lead, &clock)

	if err := m.Record(ctx, 40, clock); err != nil {
		t.Fatalf("Record(40): %v", err)
	}
	if got, held := m.Refused(t.Context()); held {
		t.Fatalf("a post-charge that left room is held as %+v", got)
	}
	if err := m.Held(t.Context()); err != nil {
		t.Fatalf("Held after a post-charge that left room = %v", err)
	}

	if err := m.Record(ctx, 90, clock); err != nil {
		t.Fatalf("Record(90): %v", err)
	}
	got, held := m.Refused(t.Context())
	if !held || got.Scope != "agent" || got.Period != period.Day || got.Used != 130 || got.Limit != 100 {
		t.Fatalf("Refused = (%+v, %v), want the seat's day the post-charge took to 130 of 100", got, held)
	}
	if err := m.Held(t.Context()); !errors.Is(err, toolloop.ErrBudgetExhausted) {
		t.Errorf("Held = %v, want the refusal the turn's next call is certain to meet", err)
	}
	windows := coord.WindowsAt(clock, time.UTC)
	for _, scope := range []string{coord.OrgScope, scopeOf(t, c, lead)} {
		u, err := fleet.Used(ctx, scope, windows)
		if err != nil || u.In(period.Day).Used != 130 {
			t.Errorf("%s day = (%+v, %v), want the 130 both calls spent, whole", scope,
				u.In(period.Day), err)
		}
	}
	// The counter agrees: the hold saved a call, not invented a refusal.
	if next, err := m.Spend(ctx, 1); err != nil || next.OK {
		t.Errorf("Spend(1) after the hold = (%+v, %v), want it refused", next, err)
	}
}

// AN AUXILIARY CALL IS COUNTED IN THE WINDOWS OF THE INSTANT IT RETURNED, read
// once for the counter and the record alike, not in whichever window the
// meter's clock reads when the charge is written.
func TestAnAuxiliaryPostChargeIsCountedWhenItsCallReturned(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	fleet := coordmem.NewFleet()
	lead := &org.Role{Name: "Lead", TokenBudget: org.TokenCeilings{period.Day: 100}}
	c := meteredCompany(config.TokenBudget{}, lead)
	returned := time.Date(2026, time.March, 14, 23, 59, 59, 0, time.UTC)
	clock := returned.Add(time.Minute)
	m := clockedMeter(t, fleet, c, lead, &clock)

	if err := m.Record(ctx, 150, returned); err != nil {
		t.Fatalf("Record: %v", err)
	}
	u, err := fleet.Used(ctx, scopeOf(t, c, lead), coord.WindowsAt(returned, time.UTC))
	if err != nil || u.In(period.Day).Used != 150 || u.In(period.Day).Window.Label != "2026-03-14" {
		t.Fatalf("the 14th = (%+v, %v), want the 150 spent before its midnight", u.In(period.Day), err)
	}
	// On the meter's clock the next call is judged on the 15th, with room.
	if got, held := m.Refused(t.Context()); held {
		t.Errorf("Refused = %+v, want nothing held on a day that has turned over", got)
	}
}

// refusalLog is a meter's counter that remembers every refusal handed to it,
// and can be made to fail them.
type refusalLog struct {
	budgetCounter

	mu     sync.Mutex
	scopes []string
	err    error
}

func (r *refusalLog) Refuse(ctx context.Context, scope string, caps coord.Caps, w coord.Windows) (coord.Usage, error) {
	r.mu.Lock()
	r.scopes = append(r.scopes, scope)
	err := r.err
	r.mu.Unlock()
	if err != nil {
		return coord.Usage{}, err
	}
	return r.budgetCounter.Refuse(ctx, scope, caps, w)
}

func (r *refusalLog) refused() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.scopes...)
}

// countedFleet is a fleet whose refusals go through a [refusalLog], for a gate
// the engine builds over its whole fleet rather than over a meter's counter.
type countedFleet struct {
	fleetBase
	log *refusalLog
}

func (f countedFleet) Refuse(ctx context.Context, scope string, caps coord.Caps, w coord.Windows) (coord.Usage, error) {
	return f.log.Refuse(ctx, scope, caps, w)
}

// counted wraps fleet so every refusal handed to it is remembered, and failed
// with err where err is not nil.
func counted(fleet *coordmem.Fleet, err error) (countedFleet, *refusalLog) {
	log := &refusalLog{budgetCounter: fleet, err: err}
	return countedFleet{fleetBase: fleet, log: log}, log
}

// stampOf is one scope's refusal stamp in one period, read off the counter.
func stampOf(t *testing.T, fleet *coordmem.Fleet, scope string, p period.Period, w coord.Windows) time.Time {
	t.Helper()
	u, err := fleet.Used(t.Context(), scope, w)
	if err != nil {
		t.Fatalf("Used(%s): %v", scope, err)
	}
	return u.In(p).RefusedAt
}

// A CALL THE METER HOLDS IS A REFUSAL THE COUNTER RECORDS.
//
// Post-charges refuse and stamp nothing, so once they take the seat past its
// day the call the meter holds is the only refusal. Unrecorded, the window
// refuses every call while refused_at, "last refused" and `crewlet budgets
// show` say nothing was refused. The stamp is the counter's and spends nothing.
func TestAHeldCallIsRecordedAsTheGatesRefusal(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	fleet := coordmem.NewFleet()
	lead := &org.Role{Name: "Lead", TokenBudget: org.TokenCeilings{period.Day: 100}}
	c := meteredCompany(config.TokenBudget{}, lead)
	clock := time.Date(2026, time.March, 14, 15, 0, 0, 0, time.UTC)
	m := clockedMeter(t, fleet, c, lead, &clock)
	scope, windows := scopeOf(t, c, lead), coord.WindowsAt(clock, time.UTC)

	for _, tokens := range []int{60, 70} {
		if err := m.Record(ctx, tokens, clock); err != nil {
			t.Fatalf("Record(%d): %v", tokens, err)
		}
	}
	if stamp := stampOf(t, fleet, scope, period.Day, windows); !stamp.IsZero() {
		t.Fatalf("setup: the post-charges stamped the seat's day at %v", stamp)
	}

	from := time.Now()
	if err := m.Held(ctx); !errors.Is(err, toolloop.ErrBudgetExhausted) {
		t.Fatalf("Held = %v, want the seat's full day", err)
	}
	stamp := stampOf(t, fleet, scope, period.Day, windows)
	if stamp.Before(from) || stamp.After(time.Now()) {
		t.Fatalf("the seat's day refused_at = %v, want the instant the meter held the call "+
			"(in [%v, now])", stamp, from)
	}
	if org := stampOf(t, fleet, coord.OrgScope, period.Day, windows); !org.IsZero() {
		t.Errorf("the company was stamped at %v; it caps nothing and refused nothing", org)
	}
	u, err := fleet.Used(ctx, scope, windows)
	if err != nil || u.In(period.Day).Used != 130 {
		t.Errorf("the seat's day = (%+v, %v), want the 130 spent and nothing more", u.In(period.Day), err)
	}
}

// A REFUSAL IS ONE EVENT, NOT ONE WRITE PER QUESTION.
//
// Every caller asks before every call (the loop, the judge, each fan-out
// worker at once), so the meter writes once per window however many ask, and
// again only for a window it has not recorded: here the next day's.
func TestAHeldWindowIsRecordedOnce(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	fleet := coordmem.NewFleet()
	lead := &org.Role{Name: "Lead", TokenBudget: org.TokenCeilings{period.Day: 100}}
	c := meteredCompany(config.TokenBudget{}, lead)
	clock := time.Date(2026, time.March, 14, 15, 0, 0, 0, time.UTC)
	refusals := &refusalLog{budgetCounter: fleet}
	m := &meter{
		budgets: refusals, agentScope: scopeOf(t, c, lead),
		basis: basisOf(c, lead), now: func() time.Time { return clock },
	}
	if err := m.Record(ctx, 150, clock); err != nil {
		t.Fatalf("Record: %v", err)
	}

	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			if _, held := m.Refused(ctx); !held {
				t.Error("a full day was not held")
			}
		})
	}
	wg.Wait()
	for range 3 {
		if err := m.Held(ctx); err == nil {
			t.Fatal("a full day was not held")
		}
	}
	if got := refusals.refused(); len(got) != 1 || got[0] != m.agentScope {
		t.Fatalf("refusals recorded = %v, want the seat's one", got)
	}

	clock = time.Date(2026, time.March, 15, 9, 0, 0, 0, time.UTC)
	if err := m.Record(ctx, 120, clock); err != nil {
		t.Fatalf("Record on the next day: %v", err)
	}
	if _, held := m.Refused(ctx); !held {
		t.Fatal("the next day's full window was not held")
	}
	if got := refusals.refused(); len(got) != 2 {
		t.Fatalf("refusals recorded = %v, want a second for the next day's window", got)
	}
}

// A HELD REFUSAL STAMPS THE SCOPE IT NAMES, THE COMPANY BEFORE THE SEAT.
//
// Both scopes' days are full. The counter judges the company first, so a
// charge would stamp the company alone; the meter's record follows the same
// rule, or the seat's screens would show refusals the company made.
func TestAHeldRefusalStampsTheScopeItNames(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	fleet := coordmem.NewFleet()
	lead := &org.Role{Name: "Lead", TokenBudget: org.TokenCeilings{period.Day: 100}}
	c := meteredCompany(config.TokenBudget{Day: ceiling(100)}, lead)
	clock := time.Date(2026, time.March, 14, 15, 0, 0, 0, time.UTC)
	m := clockedMeter(t, fleet, c, lead, &clock)
	scope, windows := scopeOf(t, c, lead), coord.WindowsAt(clock, time.UTC)

	if err := m.Record(ctx, 120, clock); err != nil {
		t.Fatalf("Record: %v", err)
	}
	if got, held := m.Refused(ctx); !held || got.Scope != "org" {
		t.Fatalf("Refused = (%+v, %v), want the company's day", got, held)
	}
	if stamp := stampOf(t, fleet, coord.OrgScope, period.Day, windows); stamp.IsZero() {
		t.Error("the company's day carries no refusal stamp")
	}
	if stamp := stampOf(t, fleet, scope, period.Day, windows); !stamp.IsZero() {
		t.Errorf("the seat's day was stamped at %v for a refusal the company makes", stamp)
	}
}

// A HELD REFUSAL'S RECORD IS JUDGED BY THE COUNTER, NOT BY THE METER'S MEMORY.
//
// A peer whose clock leads moved the seat's slot onto the next day, which has
// room. The meter holds on through its own day
// (TestAHeldRefusalLastsTheTurnsOwnCalendar), but a stamp on the counter's day
// would tell every screen it is refusing a day nobody has refused.
func TestAHeldRefusalStampsNoWindowTheCounterHasRoomIn(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	fleet := coordmem.NewFleet()
	lead := &org.Role{Name: "Lead", TokenBudget: org.TokenCeilings{period.Day: 100}}
	c := meteredCompany(config.TokenBudget{}, lead)
	clock := time.Date(2026, time.March, 14, 23, 59, 50, 0, time.UTC)
	m := clockedMeter(t, fleet, c, lead, &clock)

	if err := m.Record(ctx, 120, clock); err != nil {
		t.Fatalf("Record: %v", err)
	}
	ahead := coord.WindowsAt(clock.Add(20*time.Second), time.UTC)
	if _, err := fleet.PostCharge(ctx, m.agentScope, 5, ahead); err != nil {
		t.Fatalf("the peer's post-charge: %v", err)
	}
	if _, held := m.Refused(ctx); !held {
		t.Fatal("the meter dropped the refusal its own day still makes")
	}
	for _, w := range []coord.Windows{m.windows(), ahead} {
		if stamp := stampOf(t, fleet, m.agentScope, period.Day, w); !stamp.IsZero() {
			t.Errorf("the seat's day read at %s is stamped at %v; the counter's day has room",
				w[0].Label, stamp)
		}
	}
}

// A HELD REFUSAL IS RECORDED HOWEVER ITS CALLER ENDS, AND STANDS IF IT CANNOT BE.
//
// The record outlives a caller that hung up. It reports a refusal, never makes
// one: a counter that cannot take it leaves the call refused, and is not asked
// again on every question.
func TestAHeldRefusalIsRecordedOnAContextThatOutlivesItsCaller(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	fleet := coordmem.NewFleet()
	lead := &org.Role{Name: "Lead", TokenBudget: org.TokenCeilings{period.Day: 100}}
	c := meteredCompany(config.TokenBudget{}, lead)
	clock := time.Date(2026, time.March, 14, 15, 0, 0, 0, time.UTC)
	windows := coord.WindowsAt(clock, time.UTC)

	m := &meter{
		budgets: deadContextRefused{fleet}, agentScope: scopeOf(t, c, lead),
		basis: basisOf(c, lead), now: func() time.Time { return clock },
	}
	if err := m.Record(ctx, 150, clock); err != nil {
		t.Fatalf("Record: %v", err)
	}
	ended, cancel := context.WithCancel(ctx)
	cancel()
	if _, held := m.Refused(ended); !held {
		t.Fatal("a full day was not held for a caller that hung up")
	}
	if stamp := stampOf(t, fleet, m.agentScope, period.Day, windows); stamp.IsZero() {
		t.Error("the refusal of a caller that hung up was never recorded")
	}

	failing := &refusalLog{budgetCounter: fleet, err: errors.New("the counter is unreachable")}
	other := &org.Role{Name: "Other", TokenBudget: org.TokenCeilings{period.Day: 100}}
	c = meteredCompany(config.TokenBudget{}, other)
	unrecorded := &meter{
		budgets: failing, agentScope: scopeOf(t, c, other),
		basis: basisOf(c, other), now: func() time.Time { return clock },
	}
	if err := unrecorded.Record(ctx, 150, clock); err != nil {
		t.Fatalf("Record: %v", err)
	}
	for range 3 {
		if err := unrecorded.Held(ctx); !errors.Is(err, toolloop.ErrBudgetExhausted) {
			t.Fatalf("Held with an unwritable record = %v, want the refusal all the same", err)
		}
	}
	if got := failing.refused(); len(got) != 1 {
		t.Errorf("record attempts = %v, want one: a refusal is reported once, not per question", got)
	}
}

// A HELD REFUSAL REPORTS THE COUNTER'S FIGURE AT THE REFUSAL.
//
// The record answers the counter as it stands, spend the meter never saw
// included (another node, a background pass), so budget_exhausted's
// used_tokens is the window's spend at the refusal, not at the meter's last
// answer.
func TestAHeldRefusalReportsTheCountersFigureAtTheRefusal(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	fleet := coordmem.NewFleet()
	lead := &org.Role{Name: "Lead", TokenBudget: org.TokenCeilings{period.Day: 100}}
	c := meteredCompany(config.TokenBudget{}, lead)
	clock := time.Date(2026, time.March, 14, 15, 0, 0, 0, time.UTC)
	m := clockedMeter(t, fleet, c, lead, &clock)

	if err := m.Record(ctx, 130, clock); err != nil {
		t.Fatalf("Record: %v", err)
	}
	if _, err := fleet.PostCharge(ctx, m.agentScope, 20, m.windows()); err != nil {
		t.Fatalf("the spend outside the turn: %v", err)
	}
	got, held := m.Refused(ctx)
	if !held || got.Scope != "agent" || got.Used != 150 || got.Limit != 100 {
		t.Fatalf("Refused = (%+v, %v), want the seat's day at the 150 the counter holds", got, held)
	}
}

// A GATE'S REFUSAL STAMPS THE SCOPE A CHARGE WOULD BE REFUSED BY, NOT THE ONE
// IT WAITS ON.
//
// The company's day and the seat's month are both full. A park waits on the
// month, which ends last, but a charge is judged against the company first, so
// the company refuses and is stamped alone; otherwise the screens would blame
// the seat. Once the company has room, the seat's month is what is stamped.
//
// Mutation: record the refusal on the scope the park waits on, and the
// company's day carries no stamp while the seat's month carries one.
func TestAGatesRefusalStampsTheScopeAChargeIsRefusedBy(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	fleet := coordmem.NewFleet()
	lead := &org.Role{Name: "Lead", TokenBudget: org.TokenCeilings{period.Month: 100}}
	c := meteredCompany(config.TokenBudget{Day: ceiling(100)}, lead)
	clock := time.Date(2026, time.March, 14, 15, 0, 0, 0, time.UTC)
	m := clockedMeter(t, fleet, c, lead, &clock)
	scope, windows := scopeOf(t, c, lead), coord.WindowsAt(clock, time.UTC)
	if _, err := fleet.PostCharge(ctx, scope, 100, windows); err != nil {
		t.Fatalf("PostCharge: %v", err)
	}

	r, refusing, err := m.refusing(ctx)
	if err != nil || !refusing || r.Scope != scope || r.Window.Period != period.Month || r.By != coord.OrgScope {
		t.Fatalf("refusing = (%+v, %v, %v), want the seat's month waited on and the "+
			"company refusing", r, refusing, err)
	}
	m.turnAway(ctx, r)
	if stamp := stampOf(t, fleet, coord.OrgScope, period.Day, windows); stamp.IsZero() {
		t.Error("the company's day, which refuses every charge, carries no stamp")
	}
	for _, p := range []period.Period{period.Day, period.Month} {
		if stamp := stampOf(t, fleet, scope, p, windows); !stamp.IsZero() {
			t.Errorf("the seat's %s was stamped at %v for a refusal the company makes", p, stamp)
		}
	}

	// The next day the company has room, and the seat's month refuses.
	clock = time.Date(2026, time.March, 15, 9, 0, 0, 0, time.UTC)
	windows = coord.WindowsAt(clock, time.UTC)
	r, refusing, err = m.refusing(ctx)
	if err != nil || !refusing || r.By != scope || r.Window.Period != period.Month {
		t.Fatalf("refusing the next day = (%+v, %v, %v), want the seat's month refusing",
			r, refusing, err)
	}
	m.turnAway(ctx, r)
	if stamp := stampOf(t, fleet, scope, period.Month, windows); stamp.IsZero() {
		t.Error("the seat's month, which refuses every charge now, carries no stamp")
	}
	if stamp := stampOf(t, fleet, coord.OrgScope, period.Day, windows); !stamp.IsZero() {
		t.Errorf("the company's new day was stamped at %v; it has room", stamp)
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
	if got, held := m.Refused(t.Context()); !held || got.Scope != "org" || got.Period != period.Day {
		t.Fatalf("Refused = (%+v, %v), want the company's day named before the seat's month", got, held)
	}
	clock = time.Date(2026, time.March, 15, 9, 0, 0, 0, time.UTC)
	if got, held := m.Refused(t.Context()); !held || got.Scope != "agent" || got.Period != period.Month {
		t.Fatalf("Refused the next day = (%+v, %v), want the seat's month, which still stands", got, held)
	}
}

// A SEAT'S REFUSAL THAT FILLS THE COMPANY IS HELD AS THE COMPANY'S.
//
// The seat refuses a round that leaves the company's day exactly at its
// ceiling. The counter judges the company first, so the next charge is the
// COMPANY's refusal; naming the seat would send an operator to raise a ceiling
// that changes nothing while the company's day is spent.
func TestASeatRefusalThatFillsTheCompanyIsHeldAsTheCompanys(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	fleet := coordmem.NewFleet()
	lead := &org.Role{Name: "Lead", TokenBudget: org.TokenCeilings{period.Day: 50}}
	c := meteredCompany(config.TokenBudget{Day: ceiling(100)}, lead)
	clock := time.Date(2026, time.March, 14, 15, 0, 0, 0, time.UTC)
	m := clockedMeter(t, fleet, c, lead, &clock)

	if got, err := m.Spend(ctx, 40); err != nil || !got.OK {
		t.Fatalf("Spend(40) = (%+v, %v), want admitted", got, err)
	}
	if got, err := m.Spend(ctx, 60); err != nil || got.OK || got.Scope != "agent" {
		t.Fatalf("Spend(60) = (%+v, %v), want the seat's day refusing", got, err)
	}
	held, ok := m.Refused(t.Context())
	if !ok || held.Scope != "org" || held.Period != period.Day || held.Used != 100 || held.Limit != 100 {
		t.Fatalf("Refused = (%+v, %v), want the company's day the refused round filled", held, ok)
	}
	// And that is what the counter answers the next charge.
	if got, err := m.Spend(ctx, 1); err != nil || got.OK || got.Scope != "org" || got.Period != period.Day {
		t.Fatalf("Spend(1) = (%+v, %v), want the company's day refusing, as the meter held", got, err)
	}
}

// A REFUSAL HOLDS EVERY WINDOW ITS ROUND FILLED, OF EITHER SCOPE.
//
// The company refuses a round that also takes the seat's week past its
// ceiling. While the company's day lasts its refusal is the one named; once the
// day turns over the seat's week still refuses, and a meter that kept only the
// refusal it was answered would pay a call to learn it.
func TestARefusalHoldsEveryWindowItsRoundFilled(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	fleet := coordmem.NewFleet()
	lead := &org.Role{Name: "Lead", TokenBudget: org.TokenCeilings{period.Week: 120}}
	c := meteredCompany(config.TokenBudget{Day: ceiling(100)}, lead)
	// A Saturday, so the next day is still in the same ISO week.
	clock := time.Date(2026, time.March, 14, 15, 0, 0, 0, time.UTC)
	m := clockedMeter(t, fleet, c, lead, &clock)

	if got, err := m.Spend(ctx, 90); err != nil || !got.OK {
		t.Fatalf("Spend(90) = (%+v, %v), want admitted", got, err)
	}
	if got, err := m.Spend(ctx, 40); err != nil || got.OK || got.Scope != "org" {
		t.Fatalf("Spend(40) = (%+v, %v), want the company's day refusing", got, err)
	}
	if held, ok := m.Refused(t.Context()); !ok || held.Scope != "org" || held.Period != period.Day {
		t.Fatalf("Refused = (%+v, %v), want the company's day", held, ok)
	}
	clock = time.Date(2026, time.March, 15, 9, 0, 0, 0, time.UTC)
	held, ok := m.Refused(t.Context())
	if !ok || held.Scope != "agent" || held.Period != period.Week || held.Used != 130 || held.Limit != 120 {
		t.Fatalf("Refused the next day = (%+v, %v), want the seat's week the company's "+
			"refused round took past its ceiling", held, ok)
	}
	if got, err := m.Spend(ctx, 1); err != nil || got.OK || got.Scope != "agent" || got.Period != period.Week {
		t.Fatalf("Spend(1) the next day = (%+v, %v), want the seat's week refusing, as the "+
			"meter held", got, err)
	}
}

// A HELD REFUSAL LASTS THE TURN'S OWN CALENDAR, NOT A PEER'S.
//
// The one edge of a held refusal's certainty, pinned as a decision. A peer
// whose clock leads (or on a newer epoch whose timezone moved east) rolls the
// shared slot onto the next day while this turn's clock is on the full one; the
// counter would admit the next charge, but the meter holds until its own day
// ends: that day is full on the turn's calendar, and the error is closed, the
// turn stopping with nothing billed (meter.Refused).
func TestAHeldRefusalLastsTheTurnsOwnCalendar(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	fleet := coordmem.NewFleet()
	lead := &org.Role{Name: "Lead", TokenBudget: org.TokenCeilings{period.Day: 100}}
	c := meteredCompany(config.TokenBudget{}, lead)
	clock := time.Date(2026, time.March, 14, 23, 59, 50, 0, time.UTC)
	m := clockedMeter(t, fleet, c, lead, &clock)

	if got, err := m.Spend(ctx, 120); err != nil || got.OK {
		t.Fatalf("Spend(120) = (%+v, %v), want the seat's day refusing", got, err)
	}
	// A peer twenty seconds ahead charges the seat in the next day.
	ahead := coord.WindowsAt(clock.Add(20*time.Second), time.UTC)
	if _, err := fleet.Charge(ctx, coord.ChargeRequest{
		Seat: m.agentScope, Tokens: 5, Windows: ahead, SeatCaps: m.basis.seat,
	}); err != nil {
		t.Fatalf("the peer's charge: %v", err)
	}
	// The counter would now judge this turn's next charge in the next day.
	next, err := fleet.Charge(ctx, coord.ChargeRequest{
		Seat: m.agentScope, Tokens: 1, Windows: m.windows(), SeatCaps: m.basis.seat,
	})
	if err != nil || !next.OK {
		t.Fatalf("this turn's next charge = (%+v, %v), want it admitted in the day the "+
			"peer rolled the slot onto: the edge is not the one meter.Refused documents",
			next, err)
	}
	// The meter holds the refusal its own day still makes.
	if held, ok := m.Refused(t.Context()); !ok || held.Scope != "agent" || held.Window != "2026-03-14" {
		t.Fatalf("Refused = (%+v, %v), want the seat's day on the turn's own calendar", held, ok)
	}
	clock = time.Date(2026, time.March, 15, 0, 0, 0, 0, time.UTC)
	if held, ok := m.Refused(t.Context()); ok {
		t.Fatalf("Refused once the turn's own day ended = %+v, want nothing", held)
	}
}

// A REFUSAL ANSWERED TO A CALLER THAT HUNG UP IS HELD ALL THE SAME: the window
// is full, and the turn's next call is made by somebody else.
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
	if got, held := m.Refused(t.Context()); !held || got.Scope != "agent" || got.Used != 250 {
		t.Fatalf("Refused = (%+v, %v), want the refusal the hung-up charge met", got, held)
	}
}

// A RESUMED TURN'S METER KNOWS WHAT THE COUNTER ALREADY REFUSES.
//
// A launch's completion resumes its turn from the sandbox coordinator with no
// budget park asked, just after a post-charge that refuses nothing may have
// taken a window past its ceiling. So the meter reads the counter once and
// holds a full window before any call is sent, billed and refused; a counter
// it cannot read leaves it holding nothing, and the first charge is the gate.
func TestAResumedTurnsMeterHoldsWhatTheCounterAlreadyRefuses(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	fleet := coordmem.NewFleet()
	lead := &org.Role{Name: "Lead", TokenBudget: org.TokenCeilings{period.Day: 100}}
	c := meteredCompany(config.TokenBudget{}, lead)
	scope := scopeOf(t, c, lead)
	// The collected run, post-charged past the seat's day.
	if _, err := fleet.PostCharge(ctx, scope, 140, coord.WindowsAt(time.Now(), time.UTC)); err != nil {
		t.Fatalf("PostCharge: %v", err)
	}
	e := &Engine{backends: &Backends{Fleet: fleet}}

	got, held := e.resumeMeterFor(ctx, c, lead.Handle()).Refused(t.Context())
	if !held || got.Scope != "agent" || got.Period != period.Day || got.Used != 140 || got.Limit != 100 {
		t.Fatalf("Refused = (%+v, %v), want the seat's day the run took past its ceiling", got, held)
	}
	// A fresh turn's meter asks nothing: the park has already asked.
	if got, held := e.meterFor(c, lead.Handle()).Refused(t.Context()); held {
		t.Errorf("a fresh meter holds %+v without a charge or a read", got)
	}

	unreadable := &meter{
		budgets: counters{err: errors.New("the counter is unreachable")}, agentScope: scope,
		basis: basisOf(c, lead), now: time.Now,
	}
	unreadable.observe(ctx)
	if got, held := unreadable.Refused(t.Context()); held {
		t.Errorf("an unreadable counter left the meter holding %+v", got)
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

func (d deadContextRefused) PostCharge(ctx context.Context, seat string, tokens int, w coord.Windows) (coord.Spend, error) {
	if err := ctx.Err(); err != nil {
		return coord.Spend{}, err
	}
	return d.budgets.PostCharge(ctx, seat, tokens, w)
}

func (d deadContextRefused) Refuse(ctx context.Context, scope string, caps coord.Caps, w coord.Windows) (coord.Usage, error) {
	if err := ctx.Err(); err != nil {
		return coord.Usage{}, err
	}
	return d.budgets.Refuse(ctx, scope, caps, w)
}

func (d deadContextRefused) Used(ctx context.Context, scope string, w coord.Windows) (coord.Usage, error) {
	if err := ctx.Err(); err != nil {
		return coord.Usage{}, err
	}
	return d.budgets.Used(ctx, scope, w)
}

// A METER KNOWS WHAT ITS TURN SPENT, WINDOW BY WINDOW, for an executor launch's
// floor ([windowRoom]): every round, a refused one too since the counter counts
// it, and every auxiliary call, each in the window it was charged in, never the
// one current after a turnover.
//
// Mutation: tally no round, no refused round or no auxiliary call, or tally a
// charge by its period alone, and the case goes red.
func TestAMeterKnowsWhatItsTurnSpentInEachWindow(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	lead := &org.Role{Name: "Lead", TokenBudget: org.TokenCeilings{period.Day: 100}}
	c := meteredCompany(config.TokenBudget{}, lead)
	// THE LAST HOUR OF A WEDNESDAY: two hours on is the next day, in the same
	// week and the same month.
	var mu sync.Mutex
	at := time.Date(2026, time.September, 23, 23, 0, 0, 0, time.UTC)
	clock := func() time.Time {
		mu.Lock()
		defer mu.Unlock()
		return at
	}
	e := &Engine{backends: &Backends{Fleet: coordmem.NewFleet()}, clock: clock}
	m := e.meterFor(c, lead.Handle())

	if got, err := m.Spend(ctx, 30); err != nil || !got.OK {
		t.Fatalf("Spend(30) = (%+v, %v), want admitted", got, err)
	}
	if got, err := m.Spend(ctx, 80); err != nil || got.OK {
		t.Fatalf("Spend(80) at 30 of a 100-token day = (%+v, %v), want refused", got, err)
	}
	if err := m.Record(ctx, 20, clock()); err != nil {
		t.Fatalf("Record(20): %v", err)
	}
	all := map[period.Period]int{period.Day: 130, period.Week: 130, period.Month: 130}
	if got := m.chargedIn(coord.WindowsAt(clock(), c.Config.Location())); !maps.Equal(got, all) {
		t.Errorf("the turn charged %v in the windows it charged in, want %v: the admitted round, the refused "+
			"one and the auxiliary call", got, all)
	}
	mu.Lock()
	at = at.Add(2 * time.Hour)
	mu.Unlock()
	next := map[period.Period]int{period.Week: 130, period.Month: 130}
	if got := m.chargedIn(coord.WindowsAt(clock(), c.Config.Location())); !maps.Equal(got, next) {
		t.Errorf("the turn charged %v in the windows of the next day, want %v: the day before's spend is not "+
			"the new day's", got, next)
	}
}
