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
	"github.com/crewlet/crewlet/internal/origin"
	"github.com/crewlet/crewlet/internal/period"
)

// Enforcing the token budget, which fails CLOSED: money leaves for every
// token, so an unreachable counter stops the round rather than un-capping the
// company. Caps and the zone are read off the turn's pinned epoch, so a
// revision takes effect on the next turn; usage is the fleet's shared
// counters, since a per-node counter lets N nodes each spend the whole
// allowance. A round is charged in the windows current when it is charged, so
// a turn across midnight charges its later rounds to the new day (ADR-0019).
// Every seat is counted, uncapped ones included ([Engine.meterFor]), so a
// ceiling set mid-window judges the spend already in it; a seat whose capped
// window refuses is parked until it turns over (budgetpark.go). Every site
// that cuts a window reads its instant from one clock, [Engine.now], so a
// charge and the read that checks it fall in one window.

// now is the instant the engine cuts its budget windows at: [Engine.clock], or
// the wall clock where none is set.
//
// NOT AN [Options] FIELD: the API and the coordination store in this process
// judge and stamp the same windows on the wall clock, so an engine on another
// instant would cut the gate's day on a second calendar. The seam is for this
// package's cases; a case that runs a whole node moves the company's
// `timezone` instead.
func (e *Engine) now() time.Time {
	if e.clock != nil {
		return e.clock()
	}
	return time.Now()
}

// budgetCounter is the slice of the fleet's counters a meter calls.
//
// Charge counts the round it refuses, so a refused round needs no second
// write. PostCharge is only for a turn's AUXILIARY calls ([meter.Record]),
// whose size is known only from the answer; it goes through the meter so the
// window it fills is one the turn's next round is held on. Refuse stamps a
// refusal a gate made with no charge ([meter.Refused], [meter.turnAway]): the
// charge that would have stamped the window is the one the gate prevented.
type budgetCounter interface {
	Charge(ctx context.Context, req coord.ChargeRequest) (coord.Spend, error)
	PostCharge(ctx context.Context, seat string, tokens int, windows coord.Windows) (coord.Spend, error)
	Refuse(ctx context.Context, scope string, caps coord.Caps, windows coord.Windows) (coord.Usage, error)
	Used(ctx context.Context, scope string, windows coord.Windows) (coord.Usage, error)
}

// noticedCounter is the counter every gate on this node charges and refuses
// through, telling the live meters ([refusalLatch]) what each answer says: a
// refused charge names the window it refused in, a recorded refusal
// ([coord.Budgets.Refuse]) every window it stamped, and an admitted charge of
// any tokens clears both scopes' stamps. At the counter's answer because every
// refusal passes it, so no gate has to remember to tell the header.
type noticedCounter struct {
	budgetCounter
	refusals *refusalLatch
}

// Charge is [coord.Budgets.Charge], noticed.
func (n noticedCounter) Charge(ctx context.Context, req coord.ChargeRequest) (coord.Spend, error) {
	got, err := n.budgetCounter.Charge(ctx, req)
	if err != nil {
		return got, err
	}
	if got.OK {
		// Only a charge of something: a charge of nothing writes nothing
		// ([coord.Budgets.Charge]) so clears no stamp, and forgetting the
		// window anyway would hear its next refusal as a first and wake
		// the meters again.
		if req.Tokens > 0 {
			n.refusals.admitted(coord.OrgScope, req.Seat)
		}
		return got, nil
	}
	scope := coord.OrgScope
	if got.RefusedScope != coord.OrgScope {
		scope = req.Seat
	}
	n.refusals.refused(scope, got.RefusedPeriod, got.RefusedWindow.Label)
	return got, nil
}

// Refuse is [coord.Budgets.Refuse], noticed.
func (n noticedCounter) Refuse(ctx context.Context, scope string, caps coord.Caps,
	windows coord.Windows) (coord.Usage, error) {
	got, err := n.budgetCounter.Refuse(ctx, scope, caps, windows)
	if err != nil {
		return got, err
	}
	for p := range caps {
		if slot := got.In(p); !slot.RefusedAt.IsZero() {
			n.refusals.refused(scope, p, slot.Window.Label)
		}
	}
	return got, nil
}

// budgets is the counter this node's gates charge through — see
// [noticedCounter].
func (e *Engine) budgets() budgetCounter {
	return noticedCounter{budgetCounter: e.backends.Fleet, refusals: e.refusals}
}

// budgetBasis is what one epoch says a seat's spend is judged by: the
// company's ceilings, the seat's own, the zone their windows are cut in, and
// the static executor launch floor (turn_engine.executor_min_budget_tokens):
// the one threshold below "full" at which a window turns a seat's turns away
// (a launch's brief is counted beside it, [launchFloor]; a launch's own floor
// is [floorOf]).
type budgetBasis struct {
	org, seat coord.Caps
	zone      *time.Location
	floor     int
}

// basisOf reads a seat's budget basis off one epoch.
//
// The ROLE's ceilings, deliberately not the unit's: a unit budget would need a
// third counter scope and a rule for which cap a refusal names, and no config
// field declares one.
func basisOf(c *Company, seat *org.Role) budgetBasis {
	b := budgetBasis{zone: time.UTC}
	if c == nil {
		return b
	}
	if c.Config != nil {
		b.zone = c.Config.Location()
		b.floor = c.Config.TurnEngine.ExecutorFloor()
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
// One per turn segment, holding the caps and clock the turn was PINNED to, so
// a mid-turn config change cannot move a round's ceiling or day; each round is
// still charged to the windows current when it is charged. Every phase, judge,
// worker and auxiliary call of the turn charges through it ([meter.Record]), so
// a window it has seen full is held for the rest of the turn ([meter.Refused])
// rather than sent a call that is billed and refused. A held call is a refusal
// and is stamped as one (coord.WindowUsage.RefusedAt), or `refused_at` and
// `crewlet budgets show` miss it. It tallies its turn's own spend per window
// ([meter.chargedIn]), so a launch's room is at most its ceiling less what the
// turn already charged ([windowRoom]).
type meter struct {
	budgets    budgetCounter
	agentScope string
	basis      budgetBasis
	now        func() time.Time

	// mu guards full, recorded and charged.
	mu sync.Mutex
	// full is every capped window this meter has seen with no room left
	// for a single token, at most one per scope and period, each as the
	// refusal it makes certain. See [meter.Refused].
	full []toolloop.SpendOutcome
	// recorded is every window whose refusal this meter has handed the
	// counter, or is handing it now: a refusal is one event, written once
	// per window per METER, so a resumed segment's own meter records its
	// own. See [meter.Refused].
	recorded map[heldWindow]bool
	// charged is what this meter has put on the counter in each window:
	// every round [meter.Spend] charged, admitted or refused (the counter
	// records both), and every auxiliary call [meter.Record] posted. Per
	// meter, as recorded is. See [meter.chargedIn].
	charged map[chargedWindow]int
}

// heldWindow names one window a meter holds: the scope as a refusal names it
// ("org", "agent"), the period, and the window's label.
type heldWindow struct {
	scope  string
	period period.Period
	window string
}

// chargedWindow names one window a meter has charged in: the period and the
// window's label, which is its identity ([period.Window]). No scope, since
// every charge a meter makes reaches the company's window and the seat's
// alike.
type chargedWindow struct {
	period period.Period
	label  string
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
// THE WRITE OUTLIVES THE CALLER'S CONTEXT: the round was billed when its reply
// arrived, so a turn cancelled before this write would leak its spend. The
// caller is still told it hung up, so it never runs the round's tools. It
// cannot hang: the counter retries a bounded number of times (coord/kv's
// casline.go), and the client bounds every request on a context with no
// deadline.
func (m *meter) Spend(ctx context.Context, tokens int) (_ toolloop.SpendOutcome, err error) {
	defer origin.Stamp(&err)
	windows := m.windows()
	got, err := m.budgets.Charge(context.WithoutCancel(ctx), coord.ChargeRequest{
		Seat: m.agentScope, Tokens: tokens, Windows: windows,
		OrgCaps: m.basis.org, SeatCaps: m.basis.seat,
	})
	if err != nil {
		// NOT a refusal: "out of tokens" is a budget event an operator
		// acts on, "the counter is unreachable" an outage.
		return toolloop.SpendOutcome{}, fmt.Errorf("engine: budget: %w", err)
	}
	// Tallied refused or not: the counter counts both.
	m.tally(windows, tokens)
	// Kept before the caller is told anything, even one that hung up: the
	// window is full regardless, and the turn's next call is somebody
	// else's. Kept too is every other capped window the round left full,
	// of either scope: a round the seat refuses can leave the company's day
	// at its ceiling, and the company, judged first, refuses the next one.
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

// Record post-charges an AUXILIARY call of the turn — tokens already spent, in
// the windows current when it returned, on the turn's pinned clock — refusing
// nothing, and keeps every capped window the answer shows full, as a round's
// answer is kept. See [auxspend.Budget.Record].
//
// RECORDED WHOLE, past a ceiling included: the call happened, and refusing it
// would leave the money gone and the counter short. The hold on the turn's
// NEXT call ([meter.Refused], [meter.Held]) stamps the refusal, not this. A
// partial write ([coord.SeatUncountedError]) is neither kept, so the next
// charge is the gate, nor tallied ([meter.chargedIn]), so a launch may reckon
// more room than there is and wait a window longer: the side a floor errs on.
func (m *meter) Record(ctx context.Context, tokens int, at time.Time) error {
	windows := coord.WindowsAt(at, m.basis.zone)
	got, err := m.budgets.PostCharge(ctx, m.agentScope, tokens, windows)
	if err != nil {
		return fmt.Errorf("engine: record auxiliary spend: %w", err)
	}
	m.tally(windows, tokens)
	m.keepFilled(got)
	return nil
}

// tally adds tokens to what this meter has charged in each window of w.
func (m *meter) tally(w coord.Windows, tokens int) {
	if tokens <= 0 {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.charged == nil {
		m.charged = map[chargedWindow]int{}
	}
	for _, window := range w {
		m.charged[chargedWindow{period: window.Period, label: window.Label}] += tokens
	}
}

// chargedIn is what this meter has charged in each window of w, by period,
// zero where it charged nothing: its turn's own share of what the counters
// hold. By the window's LABEL, not its period alone: a turn across a boundary
// spent its earlier rounds in the window before.
func (m *meter) chargedIn(w coord.Windows) map[period.Period]int {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make(map[period.Period]int, len(w))
	for _, window := range w {
		if tokens := m.charged[chargedWindow{period: window.Period, label: window.Label}]; tokens > 0 {
			out[window.Period] = tokens
		}
	}
	return out
}

// Held is [meter.Refused] as the turn's auxiliary calls ask it: the error the
// turn's loop would stop on ([toolloop.Refusal]), or nil. See
// [auxspend.Budget.Held].
func (m *meter) Held(ctx context.Context) error { return toolloop.Refusal(ctx, m) }

// Refused reports the refusal every further charge of this turn is certain to
// meet, if this meter has seen one. See [toolloop.BudgetMeter.Refused].
//
// From answers the counter already gave, never a read ([meter.standing]):
// each full window is final until it turns over on the turn's pinned clock,
// because the ceilings are pinned and a counter only grows (package doc,
// "Budget holds on the pinned clock", for the one edge). Every caller asks
// immediately before a call and makes none on true, so a true answer is the
// gate turning that call away, and the first per window is stamped as a
// refusal ([meter.record]). The answer is then named again from the stamp's
// reading, so budget_exhausted reports the spend at the refusal.
func (m *meter) Refused(ctx context.Context) (toolloop.SpendOutcome, bool) {
	named, found := m.standing()
	if found && m.claim(named) {
		m.record(ctx, named)
		named, found = m.standing()
	}
	return named, found
}

// standing is the refusal this meter holds, asking the counter nothing and
// recording nothing: of the capped windows an answer showed with no room for a
// single token ([windowRefuses]) still current on its clock, the one the
// counter's own rule names ([namesBefore]). A window with some room but less
// than the next call needs refuses it too, but the meter does not know the
// call's size, so it names the full one: certain, if not always the counter's
// choice. See [meter.Refused].
func (m *meter) standing() (toolloop.SpendOutcome, bool) {
	now := m.now()
	m.mu.Lock()
	defer m.mu.Unlock()
	var named toolloop.SpendOutcome
	found := false
	for _, full := range m.full {
		if !now.Before(full.ResetsAt) {
			// Turned over: the next charge meets a fresh window.
			continue
		}
		if !found || namesBefore(full, named) {
			named, found = full, true
		}
	}
	return named, found
}

// claim reports whether this is the first time the meter refuses a call in the
// window named, marking it so: true exactly once per window, claimed before
// the write so concurrent callers (a fan-out's workers, the judge beside a
// round) stamp one refusal between them.
func (m *meter) claim(named toolloop.SpendOutcome) bool {
	key := heldWindow{scope: named.Scope, period: named.Period, window: named.Window}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.recorded[key] {
		return false
	}
	if m.recorded == nil {
		m.recorded = map[heldWindow]bool{}
	}
	m.recorded[key] = true
	return true
}

// record stamps the refusal the meter just made ([coord.Budgets.Refuse]) on the
// scope it names, keeps what the counter answers, and claims every window that
// write stamped. Without it, a window an auxiliary call, a launch's
// post-charge or an admitted round filled refuses every call of the turn
// while `refused_at` and `crewlet budgets show` say nothing was refused. See
// [meter.Refused].
func (m *meter) record(ctx context.Context, named toolloop.SpendOutcome) {
	var (
		scope string
		caps  coord.Caps
	)
	switch named.Scope {
	case "org":
		scope, caps = coord.OrgScope, m.basis.org
	case "agent":
		scope, caps = m.agentScope, m.basis.seat
	default:
		// Not a scope the shared counter keeps: nothing could read a
		// record of it.
		return
	}
	usage, ok := m.stamp(ctx, scope, caps, named.Period, named.Window)
	if !ok {
		return
	}
	m.keepFullIn(named.Scope, usage, caps)
	m.mu.Lock()
	defer m.mu.Unlock()
	for p, ceiling := range caps {
		if slot := usage.In(p); windowRefuses(slot, ceiling) {
			m.recorded[heldWindow{scope: named.Scope, period: p, window: slot.Window.Label}] = true
		}
	}
}

// turnAway records a refusal a GATE made on r, a reading [meter.refusing]
// answered: work turned away before any call because a window had no room for
// a single token — a delivery the budget park deferred ([Engine.budgetPark]), a
// person's question refused ([answerBudget.Refusing]), an optional auxiliary
// pass declined ([Engine.auxRoom]). The charge that would have stamped the
// window is the one the gate exists not to make, so it is stamped here, on the
// scope a charge would be refused by ([refusal.By]). Once per refusal: each
// gate builds a meter per question, asks once and makes no call on a no.
func (m *meter) turnAway(ctx context.Context, r refusal) {
	caps := m.basis.seat
	if r.By == coord.OrgScope {
		caps = m.basis.org
	}
	m.stamp(ctx, r.By, caps, r.Window.Period, r.Window.Label)
}

// stamp hands the counter a refusal made without a charge
// ([coord.Budgets.Refuse]) on scope under caps, judged against the counter as
// it stands, and answers the scope's counter as the write left it, or false
// where it failed. A failure is only logged, never retried: the refusal stands
// and only its report failed, and a counter that cannot be written fails the
// turn's next charge closed anyway. p and window name the refusal for that
// log line. On a context that outlives the caller's, since the refusal
// happened whatever the caller does next; the counter's attempts and the
// client's request timeout bound it.
func (m *meter) stamp(ctx context.Context, scope string, caps coord.Caps, p period.Period, window string) (coord.Usage, bool) {
	usage, err := m.budgets.Refuse(context.WithoutCancel(ctx), scope, caps, m.windows())
	if err != nil {
		log.WarnContext(ctx, "budget_refusal_unrecorded", "scope", scope,
			"period", string(p), "window", window, "error", err,
			"detail", "the work was refused all the same; the window's refused_at "+
				"does not show this refusal until the gate next turns a call away in it")
		return coord.Usage{}, false
	}
	return usage, true
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
// ([coord.Spend.Org]): nothing fits after either in a window it filled, and
// this is the one moment the meter learns so without paying for a call. A
// counter the answer does not carry (the zero Usage) shows nothing full.
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
// For a RESUMED turn, the one segment the budget park does not ask first: the
// sandbox coordinator resumes it right after post-charging the launch, which
// can take a window past its ceiling without refusing, so the executor's first
// round would be billed and then refused. An unreadable counter keeps nothing:
// this is a saving, never the gate, and the next charge fails closed.
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
// capped window of either scope, since a charge is admitted only while every
// one has room.
//
// THREE-VALUED: [subagent.Config.ParentRemaining] reads ZERO AS UNCAPPED, so
// answering 0 for an unreachable store would fail OPEN; the error travels and
// the caller refuses the spawn. A basis that caps nothing answers 0 unread,
// but [Engine.remainingFor] hands no reader for such a seat, so a zero with a
// nil error always means an exhausted window.
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
			// Tracked with a flag, not headroom == 0: an exhausted
			// window HAS zero headroom, and reading that as unset would
			// let another window's room overwrite it.
			if left := max(ceiling-usage.In(p).Used, 0); !capped || left < headroom {
				headroom, capped = left, true
			}
		}
	}
	return headroom, nil
}

// refusal is the capped window a scope is waiting out, as [Engine.budgetPark]
// parks a seat on it, and the scope that turns the next charge away, as every
// gate records its refusal on it ([meter.turnAway]).
type refusal struct {
	// Scope is the counter Window belongs to, coord.OrgScope or the seat's
	// own: the window the seat waits on. See By for the one that refuses.
	Scope string

	// Window is the refusing window, cut on the pinned clock. Its End is
	// when the scope next has room without a ceiling being raised.
	Window period.Window
	Used   int
	Limit  int

	// By is the scope a charge would be refused by now, coord.OrgScope or
	// the seat's own, which a gate's refusal is recorded on
	// ([meter.turnAway]): the company's wherever any of its capped windows
	// is full, since the counter judges the company first. Not always
	// Scope: a seat's own month can outlast the company's full day, and the
	// company still refuses the next charge.
	By string
}

// refusing reports the capped window of either scope that turns the seat's
// next charge away, if any.
//
// A window refuses when it has no room for a single token ([windowRefuses]),
// never on its refusal stamp alone, which outlives a raised ceiling until an
// admitted charge clears it. Among several it names the one that ENDS LAST
// ([coord.Outlasts], the counter's own tie-break): the seat runs nothing until
// it turns over. This read stamps nothing; a caller that refuses on it records
// with [meter.turnAway].
//
// THREE-VALUED: an unreachable counter is an error, and the park lets the turn
// run on one, because its own meter is the gate and fails closed.
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
				// The company is read first, so the first full
				// window's scope is the one that refuses.
				by := out.By
				if !found {
					by = scope.key
				}
				out = refusal{Scope: scope.key, Window: slot.Window, Used: slot.Used, Limit: ceiling, By: by}
				found = true
			}
		}
	}
	return out, found, nil
}

// starved reports the capped window of either scope that leaves the seat less
// room than floor ([launchFloor]) when none is refusing outright: the window a
// launch's floor refusal waits on. The same windows [meter.Remaining] reads,
// and among several the one that ENDS LAST, as [meter.refusing] names: no
// launch fits until every one has turned over. No floor or no cap is no
// starvation. THREE-VALUED for the reason refusing is.
func (m *meter) starved(ctx context.Context, floor int) (refusal, bool, error) {
	if floor <= 0 {
		return refusal{}, false, nil
	}
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
			if ceiling-slot.Used >= floor {
				continue
			}
			if !found || coord.Outlasts(slot.Window, out.Window) {
				out = refusal{Scope: scope.key, Window: slot.Window, Used: slot.Used,
					Limit: ceiling, By: scope.key}
				found = true
			}
		}
	}
	return out, found, nil
}

// remainingFor is the seat's headroom reader, or nil where the seat has no
// ceiling in any window, or no meter: the spawner and the sandbox's floor read
// nil as uncapped, which it is. The meter still COUNTS such a seat
// ([Engine.meterFor]).
//
// A NIL INTERFACE, never one holding a nil *meter, which passes their nil test
// and panics on its first read.
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
// EVERY SEAT IS COUNTED, capped or not, wherever there is a fleet: nil only
// with no coordination store or for a handle the epoch does not name as an
// agent seat. Counting an uncapped seat is what lets a ceiling set mid-window
// judge the spend already in it, and every counter reader (`crewlet budgets`,
// the live meter) show a true figure; the round trip is the price.
//
// The basis is the epoch c, the one the turn was PINNED to; the instant is
// [Engine.now], which every reader built from the meter inherits. Concrete so
// callers reach its headroom and windows; [meter.budget] and [meter.auxiliary]
// make it an interface, nil where it is nil.
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
		budgets: e.budgets(), agentScope: coord.AgentScope(agentID.String()),
		basis: basisOf(c, seat), now: e.now,
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
