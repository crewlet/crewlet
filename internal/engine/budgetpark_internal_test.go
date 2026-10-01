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

type fakeAlarm struct {
	wait    time.Duration
	fire    func()
	stopped bool
}

func (a *fakeAlarm) Stop() bool {
	was := !a.stopped
	a.stopped = true
	return was
}

func (f *fakeAlarms) after(d time.Duration, fn func()) alarm {
	f.mu.Lock()
	defer f.mu.Unlock()
	a := &fakeAlarm{wait: d, fire: fn}
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

const leadDoc = `
name: Acme
timezone: Europe/Berlin
roles:
  - name: Lead
    handle: lead
    token_budget:
      day: %d
`

func newParkRig(t *testing.T, dayCeiling string) *parkRig {
	t.Helper()
	q := memory.New()
	if err := q.Start(t.Context()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { _ = q.Stop(context.Background()) })
	r := &parkRig{q: q, fleet: coordmem.NewFleet(), alarms: &fakeAlarms{}, now: parkedAt}
	r.e = &Engine{backends: &Backends{Queue: q, Fleet: r.fleet}}
	r.e.epoch.current.Store(companyFor(t, strings.Replace(leadDoc, "%d", dayCeiling, 1)))
	r.e.budgetParks.now = r.clock
	r.e.budgetParks.after = r.alarms.after

	d := &Dispatcher{
		Budget: r.e.budgetPark,
		NoteDeferred: func(string) {
			r.mu.Lock()
			r.noted++
			r.mu.Unlock()
		},
		Turn: func(context.Context, Request) (turn.Result, error) {
			r.mu.Lock()
			r.ran++
			fn := r.turnFn
			r.mu.Unlock()
			if fn != nil {
				return fn()
			}
			return turn.Result{}, nil
		},
	}
	inbox, group := topics.AgentInbox("lead"), topics.AgentInboxGroup("lead")
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

// deliver publishes one trigger to the Lead's inbox, which the in-memory queue
// hands to the dispatcher before Publish returns.
func (r *parkRig) deliver(t *testing.T) {
	t.Helper()
	ev := &events.Event{ID: uuid.New(), Type: "task_assigned", Timestamp: r.clock()}
	if err := r.q.Publish(t.Context(), topics.AgentInbox("lead"), ev); err != nil {
		t.Fatalf("Publish: %v", err)
	}
}

func (r *parkRig) holds() []string {
	return r.q.PauseHolds(topics.AgentInbox("lead"), topics.AgentInboxGroup("lead"))
}

// renew does what the seat host's next successful renew does for a noted
// deferral: it resumes the attachment the deferral quiesced.
func (r *parkRig) renew(t *testing.T) {
	t.Helper()
	if _, err := r.q.Unquiesce(t.Context(), topics.AgentInbox("lead"), topics.AgentInboxGroup("lead")); err != nil {
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
	same := companyFor(t, strings.Replace(leadDoc, "%d", "100", 1))
	r.e.epoch.current.Store(same)
	r.e.reconcileBudgetParks(t.Context(), same)
	if r.runs() != 0 {
		t.Fatal("a revision that changed no ceiling released the park")
	}

	raised := companyFor(t, strings.Replace(leadDoc, "%d", "1000", 1))
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
		return turn.Result{}, &toolloop.BudgetError{Scope: "agent", Used: 100, Limit: 100}
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

	// A window the gate has STAMPED refuses although spend is below its
	// ceiling: the round that did not fit was larger than the room left.
	stamped := u(coord.OrgScope, map[period.Period]int{period.Day: 60})
	stamped.Windows[0].RefusedAt = parkedAt.Add(-time.Minute)
	m := &meter{
		budgets:    usageReader{coord.OrgScope: stamped},
		agentScope: "agent:x", basis: budgetBasis{org: coord.Caps{period.Day: 100}, zone: berlin},
		now: func() time.Time { return parkedAt },
	}
	if got, refusing, err := m.refusing(t.Context()); err != nil || !refusing || got.Window.Label != "2026-09-23" {
		t.Fatalf("a stamped day = (%+v, %v, %v), want it refusing", got, refusing, err)
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

// unreachableFleet is a fleet whose counters cannot be read. Embedded through
// an alias, because coord.Fleet has a method named Fleet that a field of that
// name would shadow.
type unreachableFleet struct{ fleetBase }

type fleetBase = coord.Fleet

func (unreachableFleet) Used(context.Context, string, coord.Windows) (coord.Usage, error) {
	return coord.Usage{}, errors.New("the coordination store is unreachable")
}

// A RELEASED SEAT TAKES ITS ALARM WITH IT, and a stopping node disarms every
// one: an alarm left armed would fire into a seat a peer now serves, or into a
// queue client that has closed.
func TestAParkEndsWithItsSeatAndWithTheNode(t *testing.T) {
	t.Parallel()
	r := newParkRig(t, "100")
	r.spend(t, 100)
	r.deliver(t)
	if len(r.alarms.live()) != 1 {
		t.Fatalf("precondition: %d alarms armed, want the park's one", len(r.alarms.live()))
	}
	r.e.forgetBudgetPark("lead")
	if n := len(r.alarms.live()); n != 0 {
		t.Fatalf("a released seat left %d alarms armed", n)
	}

	// The seat comes back to this node: the release's detach dropped the
	// hold, and the attachment is live again. The mail it kept is handed
	// over and parks again, on the counters it finds.
	if err := r.q.ResumeTopic(t.Context(), topics.AgentInbox("lead"),
		topics.AgentInboxGroup("lead"), pauseReasonBudget); err != nil {
		t.Fatalf("ResumeTopic: %v", err)
	}
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
