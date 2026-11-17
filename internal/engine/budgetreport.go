package engine

import (
	"context"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/events"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/period"
	"github.com/crewlet/crewlet/internal/queue/topics"
	"github.com/crewlet/crewlet/internal/tracing"
)

// The LIVE TOKEN METERS, pushed rather than polled: the dashboard's header
// draws the company's token headroom from a websocket push, so an open screen
// moves as the company spends rather than a poll interval behind it.
//
// A TICK rather than a publish per charge: a charge happens on every LLM round
// of every seat, and the frame is a SNAPSHOT, so a consumer that missed ten of
// them is one frame out of date, not behind.
//
// Except a window's FIRST REFUSAL, the moment the company or a seat stops
// working: the gate tells the reporter ([refusalLatch]) and it publishes at
// once. Later refusals in the same window ride the tick, since they change
// nothing a header draws.

// BudgetReportInterval is how often a node publishes its snapshot.
//
// FIFTEEN SECONDS, from what the screen needs: "is the company about to run
// out" changes over minutes, while EVERY node publishes its own report, each
// costing a counter listing plus one read per scope and fanned out to every
// open dashboard. A window's first refusal does not wait for it ([refusalLatch]).
const BudgetReportInterval = 15 * time.Second

// refusalLatch is how the gate tells the live meters a window has started
// turning work away: the first refusal in each window wakes the reporter, and
// the ones after it do not.
//
// FIRST AS THE COUNTER COUNTS IT: a scope's next admitted charge clears its
// refusal stamp ([coord.Budgets.Charge]), making the next refusal a first
// again, so [refusalLatch.admitted] forgets those scopes too. Per NODE: a
// peer's refusal reaches this node's header on the frame the peer publishes.
//
// Nil-safe throughout, for an engine built without one.
type refusalLatch struct {
	mu   sync.Mutex
	seen map[refusedWindow]bool
	// wake is the reporter's nudge, buffered by one: a refusal that finds one
	// pending adds nothing to it.
	wake chan struct{}
}

// refusedWindow names one window of one scope: the counter's scope string
// ([coord.OrgScope], or a seat's [coord.AgentScope]), the period and the
// window's label.
type refusedWindow struct {
	scope  string
	period period.Period
	label  string
}

func newRefusalLatch() *refusalLatch {
	return &refusalLatch{seen: map[refusedWindow]bool{}, wake: make(chan struct{}, 1)}
}

// refused records that scope's window turned work away, waking the reporter
// the first time.
func (l *refusalLatch) refused(scope string, p period.Period, label string) {
	if l == nil {
		return
	}
	key := refusedWindow{scope: scope, period: p, label: label}
	l.mu.Lock()
	first := !l.seen[key]
	l.seen[key] = true
	l.mu.Unlock()
	if first {
		select {
		case l.wake <- struct{}{}:
		default:
		}
	}
}

// admitted forgets every window of the scopes an admitted charge charged,
// whose refusal stamps that charge cleared.
func (l *refusalLatch) admitted(scopes ...string) {
	if l == nil {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	for key := range l.seen {
		if slices.Contains(scopes, key.scope) {
			delete(l.seen, key)
		}
	}
}

// keep forgets every window that is not current, so the latch holds at most
// the windows the company's clock is in.
func (l *refusalLatch) keep(current coord.Windows) {
	if l == nil {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	for key := range l.seen {
		if !slices.ContainsFunc(current[:], func(w period.Window) bool {
			return w.Period == key.period && w.Label == key.label
		}) {
			delete(l.seen, key)
		}
	}
}

// woken is the reporter's wake, and nil — which never fires — for no latch.
func (l *refusalLatch) woken() <-chan struct{} {
	if l == nil {
		return nil
	}
	return l.wake
}

// budgetReporter is one node's meter loop.
type budgetReporter struct {
	engine *Engine

	// seq is monotonic within this process, the reorder guard the payload's
	// doc asks for: a broadcast subscription reads across topics, so an older
	// report can arrive after a newer one and walk a meter backwards.
	seq atomic.Int64

	stop context.CancelFunc
	done chan struct{}
}

// startBudgetReports arms the loop where the engine has a fleet and a queue.
//
// EVERY NODE PUBLISHES, deliberately: the counters are shared, so a consumer
// keyed on [types.BudgetMeters.MeterID] holds whichever frame arrived last,
// and a singleton's meters would stop whenever its lease flapped.
func (e *Engine) startBudgetReports(ctx context.Context) {
	if e.backends == nil || e.backends.Fleet == nil || e.backends.Queue == nil {
		return
	}
	r := &budgetReporter{engine: e, done: make(chan struct{})}
	// DETACHED from ctx: a loop bound to a signal context stops at SIGTERM,
	// before the drain that is still spending tokens has run.
	loop, stop := context.WithCancel(context.WithoutCancel(ctx))
	r.stop = stop
	e.budgetReports = r
	go r.run(loop)
}

// stopBudgetReports ends the loop, waiting for an in-flight frame.
func (e *Engine) stopBudgetReports() {
	if e.budgetReports == nil {
		return
	}
	e.budgetReports.stop()
	<-e.budgetReports.done
	e.budgetReports = nil
}

func (r *budgetReporter) run(ctx context.Context) {
	defer close(r.done)
	r.loop(ctx, BudgetReportInterval, r.publish)
}

// loop publishes at once, then on every tick of every and every wake of the
// engine's refusal latch, until ctx ends. publish is a parameter so the
// schedule is testable without a broker or a node.
func (r *budgetReporter) loop(ctx context.Context, every time.Duration,
	publish func(context.Context)) {
	tick := time.NewTicker(every)
	defer tick.Stop()
	// ONE FRAME AT ONCE: a ticker's first fire is an interval away, and until
	// a frame lands every open dashboard has no budget reading at all.
	publish(ctx)
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		case <-r.engine.refusals.woken():
			// A WINDOW'S FIRST REFUSAL — see [refusalLatch].
		}
		publish(ctx)
	}
}

// publish puts this tick's frame on the stream, when there is one.
func (r *budgetReporter) publish(ctx context.Context) {
	report, ok := r.frame(ctx)
	if !ok {
		return
	}
	e := r.engine
	// THE INCARNATION, not the node id: see [types.BudgetMeters.MeterID].
	report.MeterID = e.node.Owner()
	report.Seq = int(r.seq.Add(1))
	ev := events.New(report, tracing.TraceOf(ctx))
	ev.Source = e.node.ID()
	if err := e.backends.Queue.Publish(ctx, topics.Event(ev.Type), ev); err != nil {
		if ctx.Err() == nil && !strings.Contains(err.Error(), "context canceled") {
			log.DebugContext(ctx, "budget_report_not_published",
				"error", err.Error(),
				"detail", "the live meters keep the last frame they had")
		}
	}
}

// frame is the snapshot this node would publish now, and false for every
// reason it publishes nothing: the whole decision, split from
// [budgetReporter.publish] (which stamps the incarnation, sequence and
// envelope) so it is testable without a broker or a node.
//
// CUT AT [Engine.now], the instant the gate charges a round at, so the window
// a screen draws is the window the gate counts in.
//
// A FAILED READ PUBLISHES NOTHING rather than zeroes: the consumer REPLACES
// what it holds on every report, and zeroes would show a spending company as
// having spent nothing. A COMPANY THAT CAPS NOTHING IS A FRAME, needing no
// read, so a consumer can tell "no ceiling" from "no report yet".
func (r *budgetReporter) frame(ctx context.Context) (types.BudgetMeters, bool) {
	e := r.engine
	company := e.Company()
	if company == nil || company.Org == nil {
		return types.BudgetMeters{}, false
	}
	windows := coord.WindowsAt(e.now(), company.Config.Location())
	e.refusals.keep(windows)
	if uncapped := budgetSnapshot(company, windows, nil); !uncapped.Metered() {
		return uncapped, true
	}
	usage, err := e.backends.Fleet.Usage(ctx, windows)
	if err != nil {
		if ctx.Err() == nil {
			log.DebugContext(ctx, "budget_report_skipped", "error", err.Error(),
				"detail", "the shared counter could not be read, so the live "+
					"meters keep the last frame they had")
		}
		return types.BudgetMeters{}, false
	}
	return budgetSnapshot(company, windows, usage), true
}

// BudgetNearFraction is the share of a window's ceiling at which the engine
// calls it `near` ([types.BudgetNear]), and the ONE such threshold: every
// answer that carries a state serves it beside the state, so a surface draws
// this mark rather than one of its own.
//
// NINE TENTHS: under a flat rate a window's spend tracks its elapsed share, so
// a lower mark fires in the last week of every healthy month and on every
// healthy evening, and is never read. At nine tenths a window is ahead of its
// pace or in its last tenth, when raising a ceiling is still a decision.
const BudgetNearFraction = 0.9

// windowRefuses reports whether a capped window turns the next charge away:
// it has no room left for a single token.
//
// THE SPEND DECIDES, NEVER THE REFUSAL STAMP. A refused round is counted
// ([coord.Budgets.Charge]), so a refused window already reads past its
// ceiling. A raised ceiling leaves the stamp stale (only an admitted charge or
// the window turning over clears it), and reading it would hold the seat in
// the budget park with no charge ever admitted to clear it. The stamp is still
// reported, as when the gate last said no.
//
// ONE PREDICATE for the budget park ([meter.refusing]) and every meter's state
// ([budgetState]), so a seat is never parked under a meter that reads as near.
func windowRefuses(slot coord.WindowUsage, ceiling int) bool {
	return slot.Used >= ceiling
}

// budgetState is the engine's judgement of one window, computed here once so
// no surface has a threshold of its own.
//
// A window nothing caps is [types.BudgetOK]: with no ceiling it is neither near
// one nor refused by one.
func budgetState(slot coord.WindowUsage, ceiling int, capped bool) types.BudgetState {
	switch {
	case !capped:
		return types.BudgetOK
	case windowRefuses(slot, ceiling):
		return types.BudgetRefusing
	case float64(slot.Used) >= BudgetNearFraction*float64(ceiling):
		return types.BudgetNear
	}
	return types.BudgetOK
}

// BudgetWindows is one scope's counter as the wire states it: every window, in
// [period.Periods] order, with its span, its spend, its ceiling where caps sets
// one, its refusal and its [types.BudgetState].
//
// Every window, not only the capped ones: a reader asking a scope's spend this
// week is owed it with or without a ceiling (the live frame keeps the capped
// ones, [cappedWindows]). The frame and the `budgets` answer are both built
// from it, so the two never state one counter differently.
func BudgetWindows(caps coord.Caps, u coord.Usage) []types.BudgetWindow {
	out := make([]types.BudgetWindow, 0, len(period.Periods))
	for i, p := range period.Periods {
		slot := u.Windows[i]
		ceiling, capped := caps[p]
		w := types.BudgetWindow{
			Period:   string(p),
			Window:   slot.Window.Label,
			StartsAt: rfc3339(slot.Window.Start),
			ResetsAt: rfc3339(slot.Window.End),
			Used:     slot.Used,
			State:    budgetState(slot, ceiling, capped),
		}
		if capped {
			w.Limit = &ceiling
			w.RefusedAt = refusedAt(slot)
		}
		out = append(out, w)
	}
	return out
}

// cappedWindows is [BudgetWindows] less the windows caps does not cap, and an
// empty list rather than nil where it caps none, which the wire states as `[]`.
func cappedWindows(caps coord.Caps, u coord.Usage) []types.BudgetWindow {
	out := []types.BudgetWindow{}
	for _, w := range BudgetWindows(caps, u) {
		if w.Limit != nil {
			out = append(out, w)
		}
	}
	return out
}

// budgetSnapshot is the frame one read of the shared counters makes. A company
// that caps nothing frames the org as `[]` and lists no seat, which
// [types.BudgetMeters.Metered] reports: "there is no ceiling", a reading in
// its own right.
//
// EVERY CAPPED WINDOW OF EVERY SCOPE, each with its own ceiling, refusal and
// state, so a seat capped by the day and the month shows two bars rather than
// one jumping between them. A scope nothing has charged reads [coord.Unspent].
func budgetSnapshot(company *Company, windows coord.Windows, usage []coord.Usage) types.BudgetMeters {
	byScope := make(map[string]coord.Usage, len(usage))
	for _, row := range usage {
		byScope[row.Scope] = row
	}
	read := func(scope string) coord.Usage {
		if row, found := byScope[scope]; found {
			return row
		}
		return coord.Unspent(scope, windows)
	}

	orgCaps := coord.Caps(company.Org.TokenBudget)
	report := types.BudgetMeters{
		Timezone: company.Config.Location().String(),
		Org:      types.BudgetScopeMeter{Windows: cappedWindows(orgCaps, read(coord.OrgScope))},
	}

	// ONLY METERED SEATS, as the payload promises: absence means "no cap and
	// no meter", and a seat listed with no windows would draw an empty bar.
	type seatMeter struct {
		scope string
		meter types.BudgetSeatMeter
	}
	var seats []seatMeter
	for seat := range company.Org.AllRoles() {
		if !seat.IsAgent() {
			continue
		}
		caps := coord.Caps(seat.TokenBudget)
		if len(caps) == 0 {
			continue
		}
		agentID, ok := company.Org.AgentIDFor(seat)
		if !ok {
			continue
		}
		scope := coord.AgentScope(agentID.String())
		seats = append(seats, seatMeter{scope: scope, meter: types.BudgetSeatMeter{
			AgentID: agentID.String(), Role: seat.Name, Handle: seat.Handle(),
			Windows: cappedWindows(caps, read(scope)),
		}})
	}
	// SORTED BY SCOPE, so two frames of an unchanged company are
	// byte-identical and a consumer diffing them sees nothing move.
	slices.SortFunc(seats, func(a, b seatMeter) int { return strings.Compare(a.scope, b.scope) })
	for _, seat := range seats {
		report.Seats = append(report.Seats, seat.meter)
	}
	return report
}

// refusedAt renders a window's refusal stamp the way the payload carries it:
// RFC 3339 in UTC, and empty for a window that is not refusing. Never the zero
// instant spelled out: the dashboard tests the field for presence, and would
// queue every capped seat as refusing since the first century.
func refusedAt(slot coord.WindowUsage) string {
	if slot.RefusedAt.IsZero() {
		return ""
	}
	return slot.RefusedAt.UTC().Format(time.RFC3339Nano)
}
