package engine

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/crewlet/crewlet/internal/agent/runner"
	"github.com/crewlet/crewlet/internal/agent/toolloop"
	"github.com/crewlet/crewlet/internal/auxspend"
	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/org"
	"github.com/crewlet/crewlet/internal/period"
)

// Enforcing the token budget.
//
// The seam existed and nothing supplied it: runner.Config.Budget was nil on
// every turn, so a company with a `token_budget:` spent without limit and the
// number in its config was decoration. Money leaves the building for every
// token, which is why this fails CLOSED — a counter that cannot be reached
// stops the round rather than silently un-capping the company.
//
// CAPS AND THE CLOCK ARE READ OFF THE EPOCH, usage off the fleet's shared
// counters, and the split is the design: a revision that raises a ceiling or
// moves the company's clock takes effect on the next turn (both travel in on
// every call), while each window's counter has to be one number across the
// fleet or N nodes each spend the whole allowance — which is exactly what a
// counter on the node's own database was. The windows a round is charged in
// are cut at the moment it is charged, on the clock of the epoch the turn is
// pinned to, so a turn that runs across midnight charges its later rounds to
// the new day (ADR-0019).
//
// EVERY SEAT IS COUNTED, a seat nothing caps included ([Engine.meterFor]), so a
// ceiling set mid-window judges the spend the window already holds. And a seat
// whose capped window refuses is not handed work it cannot run: it is PARKED
// on its inbox until the window turns over (budgetpark.go).

// budgetCounter is the slice of the fleet's counters a meter calls.
//
// Declared here, by the consumer: a turn's meter charges its rounds, reads the
// counters, and post-charges the turn's AUXILIARY calls ([meter.Record]) —
// never a round, because Charge counts the round it refuses, so a refused
// round is on the counter without a second write. An auxiliary call has no
// verdict to wait on (its size is known only from its answer, and nothing is
// left to stop by then), and is post-charged through the meter rather than
// beside it so the window it fills is one the turn's next round is held on.
type budgetCounter interface {
	Charge(ctx context.Context, req coord.ChargeRequest) (coord.Spend, error)
	PostCharge(ctx context.Context, seat string, tokens int, windows coord.Windows) (coord.Spend, error)
	Used(ctx context.Context, scope string, windows coord.Windows) (coord.Usage, error)
}

// budgetBasis is what one epoch says a seat's spend is judged by: the
// company's ceilings, the seat's own, and the clock their windows are cut on.
type budgetBasis struct {
	org, seat coord.Caps
	zone      *time.Location
}

// basisOf reads a seat's budget basis off one epoch.
//
// The ROLE's ceilings, not the unit's: a unit budget would need a third
// counter scope and a rule for which of three caps a refusal names, and no
// config field declares one. Stated because the absence looks like an
// oversight otherwise.
func basisOf(c *Company, seat *org.Role) budgetBasis {
	b := budgetBasis{zone: time.UTC}
	if c == nil {
		return b
	}
	if c.Config != nil {
		b.zone = c.Config.Location()
	}
	if c.Org != nil {
		b.org = coord.Caps(c.Org.TokenBudget)
	}
	if seat != nil {
		b.seat = coord.Caps(seat.TokenBudget)
	}
	return b
}

// capped reports whether the basis caps any window of either scope.
func (b budgetBasis) capped() bool { return len(b.org) > 0 || len(b.seat) > 0 }

// meter charges one seat's rounds against the shared counters.
//
// Per turn, holding the caps and the clock the turn was PINNED to — so a
// mid-turn config change cannot move the ceiling a round is judged against or
// the day it is counted in, which is the same rule every other epoch read
// follows. What it does not pin is the instant: each round is charged to the
// windows current when it is charged.
//
// ONE METER SERVES THE WHOLE TURN — every phase, the round-cap judge and every
// worker the turn delegates to charge it, and every auxiliary call the turn
// makes is post-charged through it ([meter.Record]) — which is what lets it
// answer [toolloop.BudgetMeter.Refused] for all of them: a window it has seen
// full stays full for the rest of the turn, because the ceilings are pinned and
// nothing takes spend back off a counter, until the window turns over on the
// turn's pinned clock ([meter.Refused] names the one edge of that).
//
// THE TURN'S AUXILIARY CALLS ARE TWO OF ITS INPUTS. The context assembly, the
// conversation block's condensation, every rewrite the ledgers, the judge's
// evidence and the tools ask for: each reaches the same counters, by
// post-charge, and the meter used to learn only from its own charges' answers
// — so a window one of them filled was one the turn's next round was sent
// into, billed, and refused. Charged here, its answer is kept like a round's
// ([meter.keepFilled]), and every call after it, a round or another auxiliary
// call ([meter.Held]), is held instead of made.
type meter struct {
	budgets    budgetCounter
	agentScope string
	basis      budgetBasis
	now        func() time.Time

	// mu guards full.
	mu sync.Mutex
	// full is every capped window this meter has seen with no room left
	// for a single token, at most one per scope and period, each as the
	// refusal it makes certain. See [meter.Refused].
	full []toolloop.SpendOutcome
}

var (
	_ toolloop.BudgetMeter = (*meter)(nil)
	_ auxspend.Budget      = (*meter)(nil)
)

// windows is the day, week and month this instant falls in on the pinned
// clock.
func (m *meter) windows() coord.Windows {
	return coord.WindowsAt(m.now(), m.basis.zone)
}

// Spend records a round that has already been spent and judges whether it
// fitted, in ONE operation. See coord.Budgets.Charge, which counts a round it
// refuses as it counts one it admits.
//
// THE WRITE OUTLIVES THE CALLER'S CONTEXT, because the round was billed when
// its reply arrived: a turn cancelled between that reply and this write — a
// node draining, a turn's own deadline — is spend the counter would
// otherwise never hear of, which is the leak a refusal that
// counted nothing used to be. The CALLER is still told it hung up, exactly as
// a charge on its own dead context told it, so it stops where it stopped
// before and never runs the round's tools; only the record is new. It cannot
// hang in the caller's place: the counter's own retries are bounded, and the
// client bounds every request made on a context with no deadline.
func (m *meter) Spend(ctx context.Context, tokens int) (toolloop.SpendOutcome, error) {
	got, err := m.budgets.Charge(context.WithoutCancel(ctx), coord.ChargeRequest{
		Seat: m.agentScope, Tokens: tokens, Windows: m.windows(),
		OrgCaps: m.basis.org, SeatCaps: m.basis.seat,
	})
	if err != nil {
		// NOT a refusal. The caller must tell "the company is out of
		// tokens" from "the counter is unreachable": the first is a
		// budget event an operator acts on, the second is an outage.
		return toolloop.SpendOutcome{}, fmt.Errorf("engine: budget: %w", err)
	}
	// WHAT THE ANSWER MAKES CERTAIN is kept before the caller is told
	// anything, a caller that hung up included: the window is full whether
	// or not this caller is still listening, and the turn's next call is
	// made by somebody else.
	//
	// The refused window AND every other capped window the round left
	// full, of either scope: a refusal names one window, and the round it
	// recorded can have filled another — a round the seat refuses that
	// leaves the company's day at its ceiling, whose next charge the
	// company refuses, since it is judged first. Kept from the refusal
	// alone, the turn reported the seat for a refusal the company makes.
	outcome := toolloop.SpendOutcome{OK: true}
	if !got.OK {
		outcome = toolloop.SpendOutcome{
			Scope: got.RefusedScope, Used: got.RefusedUsed, Limit: got.RefusedLimit,
			Period: got.RefusedPeriod, Window: got.RefusedWindow.Label,
			ResetsAt: got.RefusedWindow.End,
		}
		m.keepFull(outcome)
	}
	m.keepFilled(got)
	if cause := context.Cause(ctx); cause != nil {
		return toolloop.SpendOutcome{}, fmt.Errorf("engine: budget: the round is "+
			"recorded and the turn has ended: %w", cause)
	}
	return outcome, nil
}

// Record post-charges an AUXILIARY call of the turn — tokens it has already
// spent, in the windows current at the instant it returned, on the turn's
// pinned clock — refusing nothing, and keeps every capped window the answer
// shows with no room left, exactly as a round's answer is kept. See
// [auxspend.Budget.Record].
//
// RECORDED WHOLE, past a ceiling included: the call happened, and a refusal
// here would leave the counter under its ceiling with the money gone, which
// is the leak the auxiliary seam exists to close. What the call changes is the
// turn's NEXT call, which [meter.Refused] and [meter.Held] now hold.
//
// A partial write ([coord.SeatUncountedError]: the company counted it and the
// seat did not) answers no counters, so nothing is kept from it, and the next
// round's charge is the gate as it is after any answer the meter cannot read.
func (m *meter) Record(ctx context.Context, tokens int, at time.Time) error {
	got, err := m.budgets.PostCharge(ctx, m.agentScope, tokens, coord.WindowsAt(at, m.basis.zone))
	if err != nil {
		return fmt.Errorf("engine: record auxiliary spend: %w", err)
	}
	m.keepFilled(got)
	return nil
}

// Held is [meter.Refused] as the turn's auxiliary calls ask it: the error the
// turn's loop would stop on ([toolloop.Refusal]), or nil. See
// [auxspend.Budget.Held].
func (m *meter) Held(ctx context.Context) error { return toolloop.Refusal(ctx, m) }

// Refused reports the refusal every further charge of this turn is certain to
// meet, if this meter has seen one. See [toolloop.BudgetMeter.Refused].
//
// From answers the counter already gave, and never a read: every capped
// window a charge's answer showed with no room left for a single token — the
// same "refusing" [windowRefuses] parks a seat on. That is the window a
// REFUSAL named, whose refused round is counted so it reads past its ceiling,
// and every other window the same round filled, of either scope, since a
// refusal answers both counters ([coord.Spend.Org]); and every window an
// ADMITTED round left at its ceiling exactly. Each is final until that window
// turns over on the turn's pinned clock, because the meter's ceilings are the
// turn's pinned ones and a counter only grows within a window
// (coord.SeatUncountedError: nothing is taken back).
//
// THE PINNED CLOCK IS THE ONE EDGE. The counter is shared, and a slot never
// rolls back (coord's package doc): a peer whose clock leads this node's
// across a boundary, or one on a newer epoch after the company's timezone
// moved east, rolls the slot onto the next window before this turn's clock
// says the held one is over. The turn's next charge would then be counted in
// that next window, and could be admitted while the meter still holds the
// refusal — for the skew, which is seconds, or after a timezone moved east
// for as long as the two calendars' boundaries differ, which is hours.
//
// It is left so deliberately. A turn's rounds are counted on the calendar it
// was pinned to, and on that calendar the window IS full; a round the counter
// places in a peer's next window is the skew it resolves in the closed
// direction itself (a round charged early costs the next window a round). A
// read before every hold would trade the turn's own calendar for whichever
// peer moved the slot. And the hold fails closed the way a refusal does: the
// turn ends budget_exhausted, the budget park reads the counter on the current
// calendar, finds nothing refusing and parks nothing, and the delivery runs
// again — or, for a turn that had already written outside the engine, is
// recorded, exactly as after a real refusal. What a false hold costs is the
// turn's work so far, never a call billed and then refused.
//
// NAMED BY THE COUNTER'S OWN RULE over the full windows still current: the
// company's before the seat's — the company is judged first, so while one of
// its windows is full every charge is the company's refusal, whichever scope
// refused the round that filled it — and within a scope the window that ends
// last ([coord.Outlasts]), which is when that scope next has room without a
// ceiling being raised. A window with some room left, but less than the next
// call would need, refuses that call too, and the counter would name whichever
// of the two ends last; the meter does not know the call's size, so it names
// the full one — a refusal the call is certain to meet, if not always the one
// the counter would have chosen.
func (m *meter) Refused(context.Context) (toolloop.SpendOutcome, bool) {
	now := m.now()
	m.mu.Lock()
	defer m.mu.Unlock()
	var named toolloop.SpendOutcome
	found := false
	for _, full := range m.full {
		if !now.Before(full.ResetsAt) {
			// Turned over: the window this was is gone, and the next
			// charge is judged against a fresh one.
			continue
		}
		if !found || namesBefore(full, named) {
			named, found = full, true
		}
	}
	return named, found
}

// namesBefore reports whether a refusal of a's window is the one a charge names
// over b's: the company before a seat, and within a scope the window a waits
// on longer.
func namesBefore(a, b toolloop.SpendOutcome) bool {
	if (a.Scope == "org") != (b.Scope == "org") {
		return a.Scope == "org"
	}
	return coord.Outlasts(
		period.Window{Period: a.Period, Label: a.Window, End: a.ResetsAt},
		period.Window{Period: b.Period, Label: b.Window, End: b.ResetsAt})
}

// keepFull records a window this meter has seen full, replacing what it held
// for the same scope and period: a window that turned over and filled again is
// one record, not two.
func (m *meter) keepFull(full toolloop.SpendOutcome) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for i, held := range m.full {
		if held.Scope == full.Scope && held.Period == full.Period {
			m.full[i] = full
			return
		}
	}
	m.full = append(m.full, full)
}

// keepFilled records every capped window a charge left with no room, in the
// counters its answer carries — an admission's and a refusal's alike
// ([coord.Spend.Org]). An admitted round fitted, so nothing refused it, and a
// refused one is refused in one window only — but nothing will fit after
// either in any window it filled, and this is the one moment the meter is
// told so without paying for a call to find out. A counter the answer does
// not carry (the zero Usage) shows nothing full.
func (m *meter) keepFilled(got coord.Spend) {
	m.keepFullIn("org", got.Org, m.basis.org)
	m.keepFullIn("agent", got.Agent, m.basis.seat)
}

// keepFullIn records every window of one scope's usage that its caps leave no
// room in, under the name a refusal by that scope carries ("org", "agent").
func (m *meter) keepFullIn(name string, usage coord.Usage, caps coord.Caps) {
	for p, ceiling := range caps {
		slot := usage.In(p)
		if !windowRefuses(slot, ceiling) {
			continue
		}
		m.keepFull(toolloop.SpendOutcome{
			Scope: name, Used: slot.Used, Limit: ceiling,
			Period: p, Window: slot.Window.Label, ResetsAt: slot.Window.End,
		})
	}
}

// observe reads both scopes' counters once and keeps every capped window
// already full, so a turn whose meter has charged nothing yet still knows a
// refusal the counter has made certain.
//
// FOR A RESUMED TURN, which is the one segment the budget park does not ask
// first: a fresh delivery is parked before its turn starts when a window is
// refusing, but a coding run's completion resumes its turn straight from the
// sandbox coordinator — and the coordinator has just post-charged the run,
// which can take a window past its ceiling with no refusal (a post-charge
// never refuses). The resumed executor's first round was then billed and
// refused. Read here, it is refused before it is sent.
//
// AN UNREADABLE COUNTER KEEPS NOTHING, for the reason the park lets a turn
// run on one: this is a saving, never the gate, and the next charge fails
// closed on the same counter.
func (m *meter) observe(ctx context.Context) {
	windows := m.windows()
	for _, scope := range []struct {
		name, key string
		caps      coord.Caps
	}{
		{"org", coord.OrgScope, m.basis.org},
		{"agent", m.agentScope, m.basis.seat},
	} {
		if len(scope.caps) == 0 {
			continue
		}
		usage, err := m.budgets.Used(ctx, scope.key, windows)
		if err != nil {
			log.DebugContext(ctx, "budget_observe_unreadable", "scope", scope.key, "error", err,
				"detail", "the resumed turn's first charge is the gate")
			continue
		}
		m.keepFullIn(scope.name, usage, scope.caps)
	}
}

// Remaining is this seat's headroom, in tokens: the least room left in any
// capped window of either scope.
//
// THREE-VALUED, and the third value is the whole reason this is not an int.
// [subagent.Config.ParentRemaining] reads ZERO AS UNCAPPED, so a counter that
// answered 0 for "I could not reach the store" would hand a fan-out no ceiling
// at all — the fail-OPEN direction, on the one path where money leaves the
// building per token. The error travels, and the caller refuses the spawn.
//
// The TIGHTEST window of both scopes, because a charge is admitted only while
// every capped window of both has room: a seat with a week to spare under its
// own cap but nothing left in the company's day has no room.
//
// A meter over a basis that caps nothing answers 0 with no counter read, which
// is the "no ceiling" [runner.Remaining]'s readers take a zero for — but they
// never ask it: [Engine.remainingFor] hands them no reader at all for such a
// seat, so a zero with a nil error from a reader they ARE handed always means
// an exhausted window.
func (m *meter) Remaining(ctx context.Context) (int, error) {
	windows := m.windows()
	headroom, capped := 0, false
	for _, scope := range []struct {
		key  string
		caps coord.Caps
	}{
		{coord.OrgScope, m.basis.org},
		{m.agentScope, m.basis.seat},
	} {
		if len(scope.caps) == 0 {
			continue
		}
		usage, err := m.budgets.Used(ctx, scope.key, windows)
		if err != nil {
			return 0, fmt.Errorf("engine: budget headroom for %s: %w", scope.key, err)
		}
		for p, ceiling := range scope.caps {
			// TRACKED WITH A FLAG, not by testing headroom against
			// zero: a window that has spent its whole allowance HAS
			// zero headroom, and reading that as "not set yet" would
			// let another window's room overwrite it — turning an
			// exhausted company into an uncapped one at exactly the
			// moment the cap matters.
			if left := max(ceiling-usage.In(p).Used, 0); !capped || left < headroom {
				headroom, capped = left, true
			}
		}
	}
	return headroom, nil
}

// refusal is the capped window a scope is waiting out, as [Engine.budgetPark]
// parks a seat on it.
type refusal struct {
	// Scope is the counter that refuses, coord.OrgScope or the seat's own.
	Scope string

	// Window is the refusing window, cut on the pinned clock. Its End is
	// when the scope next has room without a ceiling being raised.
	Window period.Window
	Used   int
	Limit  int
}

// refusing reports the capped window of either scope that turns the seat's
// next charge away, if any.
//
// A window REFUSES when it has no room left for a single token, which the gate
// would refuse on the next charge whatever its size ([windowRefuses]) — and a
// window the gate has refused a round in always has none, because the refused
// round is counted. Never on the refusal stamp alone: after a ceiling is
// raised the stamp outlives the refusal until an admitted charge clears it,
// and a park taken on it would hold the seat back from every charge that
// could. Where several windows refuse it is the one that ENDS LAST, across
// every capped window of both scopes, by [coord.Outlasts] — the tie-break the
// counter's own refusal names its window by: the seat can run nothing until
// that one turns over, and naming an earlier one would wake it into a
// refusal. A full window need carry no stamp at all — a month a collected
// coding run post-charged past its ceiling carries none until a charge is
// refused against it.
//
// THREE-VALUED. An unreachable counter is an error, never "not refusing" and
// never "refusing": the caller decides what an unknown answer is worth, and
// for the park the answer is to let the turn run, because its own meter is the
// gate and fails closed.
func (m *meter) refusing(ctx context.Context) (refusal, bool, error) {
	windows := m.windows()
	var out refusal
	found := false
	for _, scope := range []struct {
		key  string
		caps coord.Caps
	}{
		{coord.OrgScope, m.basis.org},
		{m.agentScope, m.basis.seat},
	} {
		if len(scope.caps) == 0 {
			continue
		}
		usage, err := m.budgets.Used(ctx, scope.key, windows)
		if err != nil {
			return refusal{}, false, fmt.Errorf("engine: budget windows of %s: %w", scope.key, err)
		}
		for p, ceiling := range scope.caps {
			slot := usage.In(p)
			if !windowRefuses(slot, ceiling) {
				continue
			}
			if !found || coord.Outlasts(slot.Window, out.Window) {
				out = refusal{Scope: scope.key, Window: slot.Window, Used: slot.Used, Limit: ceiling}
				found = true
			}
		}
	}
	return out, found, nil
}

// remainingFor is the seat's headroom reader, or nil.
//
// Nil where the seat has no ceiling in any window of either scope, and where
// there is no meter at all: with no ceiling there is no headroom to read, and
// the spawner and the sandbox's floor treat nil as uncapped — which is exactly
// what the seat is. The meter itself still COUNTS such a seat ([Engine.meterFor]);
// what it has no answer for is how much room is left under a ceiling nobody
// set.
//
// A NIL INTERFACE, never an interface holding a nil *meter: the spawner and
// the sandbox's floor both test the reader against nil to mean "uncapped", and
// a typed nil passes that test and panics on its first read.
func (e *Engine) remainingFor(c *Company, handle string) runner.Remaining {
	m := e.meterFor(c, handle)
	if m == nil || !m.basis.capped() {
		return nil
	}
	return m
}

// resumeMeterFor is the meter for a RESUMED segment of a seat's turn:
// [Engine.meterFor]'s, having read the counters once ([meter.observe]), since
// a resume is the one segment the budget park does not ask before it runs.
func (e *Engine) resumeMeterFor(ctx context.Context, c *Company, handle string) *meter {
	m := e.meterFor(c, handle)
	if m != nil {
		m.observe(ctx)
	}
	return m
}

// budget is m as a turn's runner is handed it, and auxiliary as the turn's
// auxiliary calls are: each a NIL INTERFACE where there is no meter, never an
// interface holding a nil *meter, which every reader tests against nil to mean
// "nothing to charge" and would otherwise call into and panic on.
func (m *meter) budget() toolloop.BudgetMeter {
	if m == nil {
		return nil
	}
	return m
}

func (m *meter) auxiliary() auxspend.Budget {
	if m == nil {
		return nil
	}
	return m
}

// meterFor builds the meter for one seat's turn, or nil.
//
// EVERY SEAT IS COUNTED, capped or not, wherever there is a fleet to count on.
// Nil only with no coordination store, or for a handle the epoch does not name
// as an agent seat — there is then no shared counter or no scope to charge.
//
// It used to be nil for a company with no ceiling too, on the argument that a
// meter answering "yes" to every round is a round trip with no question behind
// it. The question was the COUNT. A ceiling added mid-window then started from
// zero, because nothing had counted the window before it existed, so a company
// that capped its day at noon was handed the whole day again on top of what
// the morning had already spent; and every reader of the counters — the budgets
// answer, the live meter, `crewlet budgets` — showed an uncapped company as
// having spent nothing at all. A charge with no caps is admitted by the counter
// without a refusal to decide, and the round trip is the price of a figure that
// is true when somebody sets a ceiling on it.
//
// The basis — the company's ceilings, the seat's own and the clock the windows
// are cut on — is the epoch's c, which is the one the turn was PINNED to.
//
// The concrete *meter, so a caller that needs its headroom or its windows has
// them without an assertion; [meter.budget] and [meter.auxiliary] are how it
// becomes an interface, nil where it is nil.
func (e *Engine) meterFor(c *Company, handle string) *meter {
	if e.backends == nil || e.backends.Fleet == nil || c == nil || c.Org == nil {
		return nil
	}
	seat := c.Org.AgentSeatByHandle(handle)
	if seat == nil {
		return nil
	}
	agentID, ok := c.Org.AgentIDFor(seat)
	if !ok {
		return nil
	}
	return &meter{
		budgets: e.backends.Fleet, agentScope: coord.AgentScope(agentID.String()),
		basis: basisOf(c, seat), now: time.Now,
	}
}

// rfc3339 is an instant as RFC 3339 in UTC, the wire's spelling of one, and
// "" for the zero time — a refusal with no calendar window, a sub-agent's own
// slice, has no reset to name.
func rfc3339(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format(time.RFC3339)
}
