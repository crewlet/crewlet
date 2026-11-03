package engine

import (
	"context"
	"errors"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/agent/toolloop"
	"github.com/crewlet/crewlet/internal/agent/turn"
	"github.com/crewlet/crewlet/internal/coord"
	coordmem "github.com/crewlet/crewlet/internal/coord/memory"
	"github.com/crewlet/crewlet/internal/events"
	"github.com/crewlet/crewlet/internal/period"
	"github.com/crewlet/crewlet/internal/queue"
	"github.com/crewlet/crewlet/internal/queue/memory"
	"github.com/crewlet/crewlet/internal/queue/topics"
)

// The budget park, end to end over the in-memory queue: a seat whose capped
// window is refusing is not handed work it cannot run, and its mail comes back
// when the window turns over or a revision raises the ceiling.

// parkedAt is the instant the park cases run at: 23:00 in Berlin on a
// Wednesday, an hour before the company's day turns over.
var parkedAt = time.Date(2026, time.September, 23, 21, 0, 0, 0, time.UTC)

// berlinMidnight is when the Berlin day of parkedAt ends.
var berlinMidnight = time.Date(2026, time.September, 23, 22, 0, 0, 0, time.UTC)

// fakeAlarms is the park's alarm, fired by the test rather than by time.
type fakeAlarms struct {
	mu    sync.Mutex
	armed []*fakeAlarm
}

// fakeAlarm is one armed alarm. A FIRED ALARM IS SPENT, as a timer that
// fired is: live() stops counting it, so an alarm the code under test fails
// to arm again after one fires is seen missing rather than counted twice.
type fakeAlarm struct {
	mu      *sync.Mutex
	wait    time.Duration
	fn      func()
	stopped bool
}

// fire runs the alarm, spent first, as a timer's goroutine runs after the
// timer has fired.
func (a *fakeAlarm) fire() {
	a.mu.Lock()
	a.stopped = true
	a.mu.Unlock()
	a.fn()
}

func (a *fakeAlarm) Stop() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	was := !a.stopped
	a.stopped = true
	return was
}

func (f *fakeAlarms) after(d time.Duration, fn func()) alarm {
	f.mu.Lock()
	defer f.mu.Unlock()
	a := &fakeAlarm{mu: &f.mu, wait: d, fn: fn}
	f.armed = append(f.armed, a)
	return a
}

// live is every alarm armed and not stopped.
func (f *fakeAlarms) live() []*fakeAlarm {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []*fakeAlarm
	for _, a := range f.armed {
		if !a.stopped {
			out = append(out, a)
		}
	}
	return out
}

// parkRig is an engine over the in-memory queue and fleet, a dispatcher wired
// with the budget stage, and a turn that records that it ran.
type parkRig struct {
	e      *Engine
	q      *memory.Queue
	fleet  *coordmem.Fleet
	alarms *fakeAlarms

	mu    sync.Mutex
	now   time.Time
	ran   int
	noted int
	// handed counts deliveries the dispatcher was handed.
	handed int
	// worked is every trigger a turn was run on, in the order the turns
	// ran, and left the deliveries each had left as its turn ran.
	worked []uuid.UUID
	left   []int
	turnFn func() (turn.Result, error)
}

func (r *parkRig) clock() time.Time {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.now
}

func (r *parkRig) deferralsNoted() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.noted
}

func (r *parkRig) runs() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.ran
}

// leadDoc is the park cases' company: one Lead whose day is capped at %d, and
// %f the executor's floor (turn_engine.executor_min_budget_tokens) — 0 for the
// cases about a FULL window, so "room" means a token of it.
const leadDoc = `
name: Acme
timezone: Europe/Berlin
turn_engine:
  executor_min_budget_tokens: %f
roles:
  - name: Lead
    handle: lead
    token_budget:
      day: %d
`

// leadCompany is leadDoc with the day's ceiling and the executor's floor.
func leadCompany(t *testing.T, dayCeiling, floor string) *Company {
	t.Helper()
	return companyFor(t, strings.NewReplacer("%d", dayCeiling, "%f", floor).Replace(leadDoc))
}

func newParkRig(t *testing.T, dayCeiling string) *parkRig {
	t.Helper()
	return newFlooredParkRig(t, dayCeiling, "0")
}

// newFlooredParkRig is [newParkRig] under an executor floor.
func newFlooredParkRig(t *testing.T, dayCeiling, floor string) *parkRig {
	t.Helper()
	q := memory.New()
	if err := q.Start(t.Context()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { _ = q.Stop(context.Background()) })
	r := &parkRig{q: q, fleet: coordmem.NewFleet(), alarms: &fakeAlarms{}, now: parkedAt}
	r.e = &Engine{backends: &Backends{Queue: q, Fleet: r.fleet}}
	r.e.epoch.current.Store(leadCompany(t, dayCeiling, floor))
	// THE ENGINE'S CLOCK, which the park's question, its alarm and the
	// meter the question is asked through all read.
	r.e.clock = r.clock
	r.e.budgetParks.after = r.alarms.after
	r.e.providerParks.after = r.alarms.after

	d := &Dispatcher{
		Budget:   r.e.budgetPark,
		Provider: r.e.providerPark,
		NoteDeferred: func(string) {
			r.mu.Lock()
			r.noted++
			r.mu.Unlock()
		},
		Turn: func(ctx context.Context, req Request) (turn.Result, error) {
			r.mu.Lock()
			r.ran++
			for _, ev := range req.Events {
				r.worked = append(r.worked, ev.ID)
				left, _ := queue.DeliveriesLeftFor(ctx, ev.ID)
				r.left = append(r.left, left)
			}
			fn := r.turnFn
			r.mu.Unlock()
			if fn != nil {
				return fn()
			}
			return turn.Result{}, nil
		},
	}
	inbox, group := topics.AgentInbox(seatOf("lead")), topics.AgentInboxGroup(seatOf("lead"))
	if err := q.SubscribeBatch(t.Context(), inbox, group,
		func(ctx context.Context, evs []*events.Event) queue.Result {
			r.mu.Lock()
			r.handed++
			r.mu.Unlock()
			return d.Dispatch(ctx, "lead", evs)
		},
		func(ev *events.Event) string { return ev.ID.String() },
		queue.DefaultBatchOptions()); err != nil {
		t.Fatalf("SubscribeBatch: %v", err)
	}
	return r
}

// spend puts tokens on the Lead's counters in the windows of the rig's clock.
func (r *parkRig) spend(t *testing.T, tokens int) {
	t.Helper()
	c := r.e.Company()
	id, ok := c.Org.AgentIDFor(c.Org.AgentSeatByHandle("lead"))
	if !ok {
		t.Fatal("the Lead has no agent id")
	}
	berlin := c.Config.Location()
	if _, err := r.fleet.PostCharge(t.Context(), coord.AgentScope(id.String()), tokens,
		coord.WindowsAt(r.clock(), berlin)); err != nil {
		t.Fatalf("PostCharge: %v", err)
	}
}

// leadDay is the Lead's day on its counter, in the windows of the rig's clock.
func (r *parkRig) leadDay(t *testing.T) coord.WindowUsage {
	t.Helper()
	c := r.e.Company()
	id, ok := c.Org.AgentIDFor(c.Org.AgentSeatByHandle("lead"))
	if !ok {
		t.Fatal("the Lead has no agent id")
	}
	u, err := r.fleet.Used(t.Context(), coord.AgentScope(id.String()),
		coord.WindowsAt(r.clock(), c.Config.Location()))
	if err != nil {
		t.Fatalf("Used: %v", err)
	}
	return u.In(period.Day)
}

// deliver publishes one trigger to the Lead's inbox, which the in-memory queue
// hands to the dispatcher before Publish returns.
func (r *parkRig) deliver(t *testing.T) {
	t.Helper()
	ev := &events.Event{ID: uuid.New(), Type: "task_assigned", Timestamp: r.clock()}
	if err := r.q.Publish(t.Context(), topics.AgentInbox(seatOf("lead")), ev); err != nil {
		t.Fatalf("Publish: %v", err)
	}
}

func (r *parkRig) holds() []string {
	return r.q.PauseHolds(topics.AgentInbox(seatOf("lead")), topics.AgentInboxGroup(seatOf("lead")))
}

// renew does what the seat host's next successful renew does for a noted
// deferral: it resumes the attachment the deferral quiesced.
func (r *parkRig) renew(t *testing.T) {
	t.Helper()
	if _, err := r.q.Unquiesce(t.Context(), topics.AgentInbox(seatOf("lead")), topics.AgentInboxGroup(seatOf("lead"))); err != nil {
		t.Fatalf("Unquiesce: %v", err)
	}
}

// A SEAT WHOSE WINDOW IS SPENT IS PARKED UNTIL IT TURNS OVER, AND ITS MAIL IS
// DELIVERED THEN.
//
// Before the park, the delivery ran a turn that was refused on its first
// charge and NAKed, and every redelivery was refused the same way until the
// broker dead-lettered it: a seat that ran out at ten in the morning lost its
// mail for the rest of the day.
func TestASpentSeatIsParkedUntilItsWindowTurnsOver(t *testing.T) {
	t.Parallel()
	r := newParkRig(t, "100")
	r.spend(t, 100)

	r.deliver(t)
	if got := r.runs(); got != 0 {
		t.Fatalf("a seat with its day spent ran %d turns; it cannot run a round", got)
	}
	if !slices.Contains(r.holds(), pauseReasonBudget) {
		t.Fatalf("holds = %v, want the budget hold on the Lead's inbox", r.holds())
	}
	if n := r.deferralsNoted(); n != 1 {
		t.Errorf("the deferral was noted %d times, want once — unnoted, nothing "+
			"resumes the attachment it quiesced", n)
	}
	alarms := r.alarms.live()
	if len(alarms) != 1 || alarms[0].wait != berlinMidnight.Sub(parkedAt) {
		t.Fatalf("alarms = %+v, want one set for Berlin's midnight, an hour away", alarms)
	}

	// The renew resumes the attachment; the hold keeps it from being handed
	// the mail again.
	r.renew(t)
	if got := r.runs(); got != 0 {
		t.Fatalf("the renew handed the parked seat its mail: %d turns ran", got)
	}

	// Midnight in Berlin: the day turns over, the alarm fires, and the
	// held delivery runs in a window with room.
	r.mu.Lock()
	r.now = berlinMidnight.Add(time.Second)
	r.mu.Unlock()
	alarms[0].fire()
	if got := r.runs(); got != 1 {
		t.Fatalf("after the window turned over %d turns ran, want the held one", got)
	}
	if h := r.holds(); slices.Contains(h, pauseReasonBudget) {
		t.Errorf("the budget hold outlived the window it waited on: %v", h)
	}
}

// A PARK IS THE GATE'S REFUSAL, AND THE COUNTER RECORDS IT.
//
// The Lead's day was filled by a post-charge — a collected executor launch, a
// background pass — which refuses nothing and stamps nothing, so no turn ever
// had a charge refused there: every delivery was parked before one could run.
// Unrecorded, that window held every message its seat was sent while its
// refused_at, "last refused" on every screen and `crewlet budgets show` said
// nothing had ever been refused. The record spends nothing.
//
// Mutation: drop the park's record, and the day carries no stamp.
func TestAParkIsRecordedAsTheGatesRefusal(t *testing.T) {
	t.Parallel()
	r := newParkRig(t, "100")
	r.spend(t, 100)
	if day := r.leadDay(t); !day.RefusedAt.IsZero() {
		t.Fatalf("setup: the post-charge stamped the Lead's day at %v", day.RefusedAt)
	}

	from := time.Now()
	r.deliver(t)
	if r.runs() != 0 || !slices.Contains(r.holds(), pauseReasonBudget) {
		t.Fatalf("precondition: the seat is not parked (runs %d, holds %v)", r.runs(), r.holds())
	}
	day := r.leadDay(t)
	if day.RefusedAt.Before(from) || day.RefusedAt.After(time.Now()) {
		t.Fatalf("the Lead's day refused_at = %v, want the instant the park turned its "+
			"delivery away (in [%v, now])", day.RefusedAt, from)
	}
	if day.Used != 100 {
		t.Errorf("the Lead's day holds %d after the park, want the 100 spent and nothing more", day.Used)
	}
}

// A SEAT WITH ROOM LEAVES NO REFUSAL: its delivery runs, and nothing is
// recorded on its counter — a stamp there would tell every screen the gate is
// refusing a seat it let through.
func TestASeatWithRoomIsNotRecordedAsRefused(t *testing.T) {
	t.Parallel()
	r := newParkRig(t, "100")
	fleet, refusals := counted(r.fleet, nil)
	r.e.backends.Fleet = fleet
	r.spend(t, 99)
	r.deliver(t)
	if r.runs() != 1 {
		t.Fatalf("a seat with a token of room left ran %d turns", r.runs())
	}
	if got := refusals.refused(); len(got) != 0 {
		t.Errorf("refusals recorded = %v for a seat the park let through", got)
	}
}

// A REVISION THAT RAISES THE CEILING RELEASES THE PARK AT ONCE, without waiting
// for the window: the room it waited for exists now.
func TestARaisedCeilingReleasesTheParkAtOnce(t *testing.T) {
	t.Parallel()
	r := newParkRig(t, "100")
	r.spend(t, 100)
	r.deliver(t)
	if r.runs() != 0 || !slices.Contains(r.holds(), pauseReasonBudget) {
		t.Fatalf("precondition: the seat is not parked (runs %d, holds %v)", r.runs(), r.holds())
	}
	r.renew(t)

	// An unrelated revision — the same ceilings — leaves the park alone.
	same := leadCompany(t, "100", "0")
	r.e.epoch.current.Store(same)
	r.e.reconcileBudgetParks(t.Context(), same)
	if r.runs() != 0 {
		t.Fatal("a revision that changed no ceiling released the park")
	}

	raised := leadCompany(t, "1000", "0")
	r.e.epoch.current.Store(raised)
	r.e.reconcileBudgetParks(t.Context(), raised)
	if got := r.runs(); got != 1 {
		t.Fatalf("after the ceiling was raised %d turns ran, want the held one at once", got)
	}
	if len(r.alarms.live()) != 0 {
		t.Error("the released park left its alarm armed")
	}
}

// A SEAT WITH ROOM IS NOT PARKED, and a seat nothing caps is never asked about.
func TestASeatWithRoomRunsAndIsNotParked(t *testing.T) {
	t.Parallel()
	r := newParkRig(t, "100")
	r.spend(t, 99)
	r.deliver(t)
	if r.runs() != 1 || len(r.holds()) != 0 {
		t.Fatalf("a seat with a token of room left: runs %d, holds %v", r.runs(), r.holds())
	}
}

// A TURN REFUSED MID-FLIGHT PARKS THE SEAT rather than NAKing into the same
// refusal: the window had room when the delivery was claimed and none by the
// turn's round.
func TestATurnRefusedMidFlightParksTheSeat(t *testing.T) {
	t.Parallel()
	r := newParkRig(t, "100")
	r.mu.Lock()
	r.turnFn = func() (turn.Result, error) {
		// The round that did not fit: the counter stamps the window.
		r.spend(t, 100)
		return turn.Result{}, &toolloop.BudgetError{Scope: "agent", Used: 100, Limit: 100,
			Period: period.Day, Window: "2026-09-23", ResetsAt: time.Date(2026, 9, 24, 0, 0, 0, 0, time.UTC)}
	}
	r.mu.Unlock()
	r.deliver(t)
	if r.runs() != 1 || !slices.Contains(r.holds(), pauseReasonBudget) {
		t.Fatalf("a turn refused mid-flight: runs %d, holds %v; want the seat parked",
			r.runs(), r.holds())
	}
	// PARKED BY THE TURN'S OWN DISPOSITION, not by a NAK whose redelivery
	// the budget stage then parks: that path spends a second delivery of
	// the message on a refusal already known.
	r.mu.Lock()
	handed := r.handed
	r.mu.Unlock()
	if handed != 1 {
		t.Errorf("the delivery was handed over %d times, want once: the refused turn "+
			"was NAKed into a redelivery rather than parked", handed)
	}
}

// A SEAT WHOSE WINDOW LEAVES LESS THAN A LAUNCH NEEDS IS PARKED UNTIL IT TURNS
// OVER, exactly as a full one is — and a revision that lowers the floor
// releases it at once.
//
// Every turn launches its executor, and a launch below the floor is refused
// ([executorHeadroom]): a seat with room, but less than the floor, refused
// every delivery at its launch and took the redelivery into the same refusal
// until the broker dead-lettered it. The window has room, so the counter is
// stamped with nothing: no charge was refused.
//
// Mutation: drop the floor from the budget stage, and the starved seat runs.
func TestASeatBelowTheExecutorFloorIsParkedUntilItsWindowTurnsOver(t *testing.T) {
	t.Parallel()
	r := newFlooredParkRig(t, "100", "50")
	fleet, refusals := counted(r.fleet, nil)
	r.e.backends.Fleet = fleet
	r.spend(t, 60)

	r.deliver(t)
	if got := r.runs(); got != 0 || !slices.Contains(r.holds(), pauseReasonBudget) {
		t.Fatalf("a seat with 40 tokens left under a floor of 50: runs %d, holds %v; want it "+
			"parked", got, r.holds())
	}
	if got := refusals.refused(); len(got) != 0 {
		t.Errorf("refusals recorded = %v for a window with room: no charge was refused", got)
	}
	alarms := r.alarms.live()
	if len(alarms) != 1 || alarms[0].wait != berlinMidnight.Sub(parkedAt) {
		t.Fatalf("alarms = %+v, want one set for Berlin's midnight", alarms)
	}
	r.renew(t)

	// A revision that lowers the floor under what the window has left
	// releases the park at once.
	lowered := leadCompany(t, "100", "30")
	r.e.epoch.current.Store(lowered)
	r.e.reconcileBudgetParks(t.Context(), lowered)
	if got := r.runs(); got != 1 {
		t.Fatalf("after the floor was lowered %d turns ran, want the held one at once", got)
	}

	// And a starved seat parked again waits out its window as a full one does.
	r.spend(t, 20)
	r.renew(t)
	r.deliver(t)
	if got := r.runs(); got != 1 || !slices.Contains(r.holds(), pauseReasonBudget) {
		t.Fatalf("a seat with 20 tokens left under a floor of 30: runs %d, holds %v", got, r.holds())
	}
	alarms = r.alarms.live()
	if len(alarms) != 1 {
		t.Fatalf("alarms = %+v, want the second park's", alarms)
	}
	r.renew(t)
	r.mu.Lock()
	r.now = berlinMidnight.Add(time.Second)
	r.mu.Unlock()
	alarms[0].fire()
	if got := r.runs(); got != 2 {
		t.Fatalf("after the window turned over %d turns ran, want the held one", got)
	}
}

// A LAUNCH REFUSED BELOW THE FLOOR MID-FLIGHT PARKS THE SEAT too: the window
// had room for a launch when the delivery was claimed, and the turn's own
// context took it under the floor before the launch.
func TestALaunchRefusedBelowTheFloorParksTheSeat(t *testing.T) {
	t.Parallel()
	r := newFlooredParkRig(t, "100", "50")
	r.mu.Lock()
	r.turnFn = func() (turn.Result, error) {
		r.spend(t, 60)
		return turn.Result{}, executorHeadroom(t.Context(), staticRemaining{left: 40}, 50)
	}
	r.mu.Unlock()
	r.deliver(t)
	if r.runs() != 1 || !slices.Contains(r.holds(), pauseReasonBudget) {
		t.Fatalf("a launch refused below the floor: runs %d, holds %v; want the seat parked",
			r.runs(), r.holds())
	}
	r.mu.Lock()
	handed := r.handed
	r.mu.Unlock()
	if handed != 1 {
		t.Errorf("the delivery was handed over %d times, want once: the refused launch was "+
			"NAKed into a redelivery rather than parked", handed)
	}
}

// THE REFUSING WINDOW IS THE ONE THAT ENDS LAST, across both scopes, and a
// spent window counts whether or not the gate has stamped it yet.
func TestTheParkWaitsOutTheWindowThatEndsLast(t *testing.T) {
	t.Parallel()
	berlin, err := time.LoadLocation("Europe/Berlin")
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	windows := coord.WindowsAt(parkedAt, berlin)
	u := func(scope string, used map[period.Period]int) coord.Usage {
		out := coord.Unspent(scope, windows)
		for i, p := range period.Periods {
			out.Windows[i].Used = used[p]
		}
		return out
	}
	for _, tc := range []struct {
		name      string
		org, seat coord.Caps
		used      map[string]coord.Usage
		want      period.Period
		refusing  bool
	}{
		{"the org's day and the seat's month: the month",
			coord.Caps{period.Day: 10}, coord.Caps{period.Month: 50},
			map[string]coord.Usage{
				coord.OrgScope: u(coord.OrgScope, map[period.Period]int{period.Day: 10}),
				"agent:x":      u("agent:x", map[period.Period]int{period.Month: 50}),
			}, period.Month, true},
		{"the org's week over the seat's day",
			coord.Caps{period.Week: 10}, coord.Caps{period.Day: 5},
			map[string]coord.Usage{
				coord.OrgScope: u(coord.OrgScope, map[period.Period]int{period.Week: 10}),
				"agent:x":      u("agent:x", map[period.Period]int{period.Day: 5}),
			}, period.Week, true},
		{"room everywhere",
			coord.Caps{period.Day: 10}, coord.Caps{period.Day: 5},
			map[string]coord.Usage{
				coord.OrgScope: u(coord.OrgScope, map[period.Period]int{period.Day: 9}),
				"agent:x":      u("agent:x", map[period.Period]int{period.Day: 4}),
			}, "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			m := &meter{
				budgets: usageReader(tc.used), agentScope: "agent:x",
				basis: budgetBasis{org: tc.org, seat: tc.seat, zone: berlin},
				now:   func() time.Time { return parkedAt },
			}
			got, refusing, err := m.refusing(t.Context())
			if err != nil {
				t.Fatalf("refusing: %v", err)
			}
			if refusing != tc.refusing || got.Window.Period != tc.want {
				t.Fatalf("refusing = (%+v, %v), want the %q window", got, refusing, tc.want)
			}
		})
	}

	// A STAMP BELOW THE CEILING IS A CEILING RAISED SINCE, and parks nothing.
	// The gate counts the round it refuses, so the window it refused in read
	// past the old ceiling; under one raised since, it has room again, and the
	// stamp stays until an admitted charge clears it. A park taken on the
	// stamp would hold the seat back from the only charge that could.
	stamped := u(coord.OrgScope, map[period.Period]int{period.Day: 60})
	stamped.Windows[0].RefusedAt = parkedAt.Add(-time.Minute)
	m := &meter{
		budgets:    usageReader{coord.OrgScope: stamped},
		agentScope: "agent:x", basis: budgetBasis{org: coord.Caps{period.Day: 100}, zone: berlin},
		now: func() time.Time { return parkedAt },
	}
	if got, refusing, err := m.refusing(t.Context()); err != nil || refusing {
		t.Fatalf("a stamped day with room = (%+v, %v, %v), want it not refusing", got, refusing, err)
	}
}

// A SEAT PARKED ON A REFUSED ROUND RUNS ONCE ITS CEILING IS RAISED.
//
// The ordinary way a seat is parked: its turn's round is refused, which stamps
// the window and counts the round past the ceiling. Raising the ceiling is the
// documented way out before the window turns over, and the release asks the
// counters again under the new ceiling. While the park read the stamp as
// "refusing", that second ask parked the seat again on the stamp alone — and a
// stamp is cleared only by an admitted charge, which a parked seat can never
// make — so the raise released nothing until midnight.
func TestARaisedCeilingReleasesASeatParkedOnARefusedRound(t *testing.T) {
	t.Parallel()
	r := newParkRig(t, "100")
	m := r.e.meterFor(r.e.Company(), "lead")
	if m == nil {
		t.Fatal("the Lead has no meter")
	}
	m.now = r.clock
	r.mu.Lock()
	r.turnFn = func() (turn.Result, error) {
		r.mu.Lock()
		r.turnFn = nil
		r.mu.Unlock()
		// The round that did not fit, through the real meter: refused,
		// stamped and counted.
		got, err := m.Spend(t.Context(), 150)
		if err != nil || got.OK {
			t.Errorf("the round = (%+v, %v), want it refused", got, err)
		}
		return turn.Result{}, &toolloop.BudgetError{Scope: got.Scope, Used: got.Used, Limit: got.Limit}
	}
	r.mu.Unlock()
	r.deliver(t)
	if r.runs() != 1 || !slices.Contains(r.holds(), pauseReasonBudget) {
		t.Fatalf("precondition: the refused turn did not park the seat (runs %d, holds %v)",
			r.runs(), r.holds())
	}
	r.renew(t)

	raised := leadCompany(t, "1000", "0")
	r.e.epoch.current.Store(raised)
	r.e.reconcileBudgetParks(t.Context(), raised)
	if got := r.runs(); got != 2 {
		t.Fatalf("after the ceiling was raised %d turns ran, want the held delivery run at "+
			"once (holds %v): the park held the seat on the refusal stamp", got, r.holds())
	}
	if slices.Contains(r.holds(), pauseReasonBudget) {
		t.Errorf("the budget hold outlived the raise: %v", r.holds())
	}
}

// AN UNREADABLE COUNTER DOES NOT PARK. The turn's own meter is the gate and
// fails closed, so a store blip must not hold a seat's mail.
func TestAnUnreadableCounterDoesNotPark(t *testing.T) {
	t.Parallel()
	r := newParkRig(t, "100")
	r.e.backends.Fleet = unreachableFleet{fleetBase: r.fleet}
	r.deliver(t)
	if r.runs() != 1 || len(r.holds()) != 0 {
		t.Fatalf("an unreadable counter: runs %d, holds %v; want the turn run unparked",
			r.runs(), r.holds())
	}
}

// usageReader answers Used from a fixed table.
type usageReader map[string]coord.Usage

func (u usageReader) Used(_ context.Context, scope string, w coord.Windows) (coord.Usage, error) {
	if got, ok := u[scope]; ok {
		return got, nil
	}
	return coord.Unspent(scope, w), nil
}

func (usageReader) Charge(context.Context, coord.ChargeRequest) (coord.Spend, error) {
	return coord.Spend{}, errors.New("not used by these cases")
}

func (usageReader) PostCharge(context.Context, string, int, coord.Windows) (coord.Spend, error) {
	return coord.Spend{}, errors.New("not used by these cases")
}

func (usageReader) Refuse(context.Context, string, coord.Caps, coord.Windows) (coord.Usage, error) {
	return coord.Usage{}, errors.New("not used by these cases")
}

// unreachableFleet is a fleet whose counters cannot be read. Embedded through
// an alias, because coord.Fleet has a method named Fleet that a field of that
// name would shadow.
type unreachableFleet struct{ fleetBase }

type fleetBase = coord.Fleet

func (unreachableFleet) Used(context.Context, string, coord.Windows) (coord.Usage, error) {
	return coord.Usage{}, errors.New("the coordination store is unreachable")
}

// A RELEASED SEAT TAKES ITS ALARM AND ITS HOLD WITH IT, and a stopping node
// disarms every alarm: an alarm left armed would fire into a seat a peer now
// serves, or into a queue client that has closed. The release's detach drops
// every hold the inbox had then, and the release forgets the park after it;
// but a park taken in between (a turn refused mid-flight, which the release
// does not wait for) holds an inbox the detach is done with, and forgotten
// without its hold, it kept the seat deaf on this node, with no park left to
// lift it.
//
// Mutation: drop the hold's lift from the forget (seatParks.forget), and the
// forgotten park's hold stays on the seat's inbox.
func TestAParkEndsWithItsSeatAndWithTheNode(t *testing.T) {
	t.Parallel()
	r := newParkRig(t, "100")
	r.spend(t, 100)
	r.deliver(t)
	if len(r.alarms.live()) != 1 || !slices.Contains(r.holds(), pauseReasonBudget) {
		t.Fatalf("precondition: %d alarms armed and holds %v, want the park's alarm and hold",
			len(r.alarms.live()), r.holds())
	}
	r.e.forgetBudgetPark(t.Context(), seatOf("lead"))
	if n := len(r.alarms.live()); n != 0 {
		t.Fatalf("a released seat left %d alarms armed", n)
	}
	if holds := r.holds(); slices.Contains(holds, pauseReasonBudget) {
		t.Fatalf("a released seat's inbox is still held %v: its forgotten park left its hold", holds)
	}

	// The seat comes back to this node, and the attachment is live again.
	// The mail it kept is handed over and parks again, on the counters it
	// finds.
	r.renew(t)
	if len(r.alarms.live()) != 1 || r.runs() != 0 {
		t.Fatalf("precondition: %d alarms armed and %d turns run after parking again",
			len(r.alarms.live()), r.runs())
	}
	r.e.stopBudgetParks()
	if n := len(r.alarms.live()); n != 0 {
		t.Fatalf("a stopping node left %d alarms armed", n)
	}
}
