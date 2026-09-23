package engine

import (
	"context"
	"slices"
	"strings"
	"sync/atomic"
	"time"

	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/events"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/queue/topics"
	"github.com/crewlet/crewlet/internal/tracing"
)

// The LIVE TOKEN METERS, pushed rather than polled.
//
// # Why this exists at all
//
// The dashboard's header renders the company's token headroom, and it renders
// it from a websocket push — a screen that is open while a company works has
// to move as the company spends, and a thirty-second poll behind it would
// report the ceiling being hit half a minute after every seat had already
// stopped. That push has been wired the whole time: the live projection folds a
// report into its meter, the stream service broadcasts the result the moment a
// report moves it, and the dashboard has a hook for it. Nothing published the
// report, so every one of those carried zeroes.
//
// # Why a tick rather than a charge hook
//
// A charge happens on every LLM round of every seat. A publish per charge
// would put an event on the fleet's stream per round for a screen nobody may
// have open, and the frame is a SNAPSHOT — a consumer that missed ten of them
// is not behind, it is one frame out of date.

// BudgetReportInterval is how often a node publishes its snapshot.
//
// FIFTEEN SECONDS, from what the screen needs rather than what the counter
// can bear. The question a header answers, "is the company about to run out",
// changes over minutes, while each report costs a listing of the counter plus
// one read per scope it holds, and is fanned out to every open dashboard on
// every node the moment it lands, EVERY node publishing its own. A refusal is
// carried on the next frame, so a gate that starts turning turns away shows up
// inside one interval, which is sooner than an operator reading a header acts
// on it.
const BudgetReportInterval = 15 * time.Second

// budgetReporter is one node's meter loop.
type budgetReporter struct {
	engine *Engine

	// current latches once the fleet's protocol floor has reached
	// [coord.WindowedCountersProtocol]: from then on every node charges the
	// counters this build reads, and a floor that has reached it only falls
	// again by a downgrade, which needs the whole fleet stopped. See
	// [budgetReporter.countersCurrent].
	current atomic.Bool

	// seq is monotonic within this process, and it is the reorder guard
	// the payload's own doc asks for: broker ordering holds within one
	// topic and a broadcast subscription reads across all of them, so an
	// older report can arrive after a newer one and walk a meter backwards.
	seq atomic.Int64

	stop context.CancelFunc
	done chan struct{}
}

// startBudgetReports arms the loop, or says why it did not.
//
// EVERY NODE PUBLISHES, deliberately — this is not a fleet duty. The counters
// are shared, so two nodes report the same numbers and a consumer keyed on
// [types.BudgetReported.MeterID] simply holds whichever arrived last; making
// it a singleton would mean the meters stop the moment one node's lease flaps,
// for a frame that costs one coordination read.
func (e *Engine) startBudgetReports(ctx context.Context) {
	if e.backends == nil || e.backends.Fleet == nil || e.backends.Queue == nil {
		return
	}
	r := &budgetReporter{engine: e, done: make(chan struct{})}
	// DETACHED from the caller's context, for the reason every other
	// long-running loop here is: a loop bound to a signal context stops at
	// SIGTERM, before the drain that is still spending tokens has run.
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
	tick := time.NewTicker(BudgetReportInterval)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
			r.publish(ctx)
		}
	}
}

// publish puts this tick's frame on the stream, when there is one.
func (r *budgetReporter) publish(ctx context.Context) {
	report, ok := r.frame(ctx, time.Now())
	if !ok {
		return
	}
	e := r.engine
	// THE INCARNATION, not the node id: see [types.BudgetReported.MeterID].
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

// frame is the snapshot this node would publish at now, and false for every
// reason it publishes nothing.
//
// THE WHOLE DECISION, split from the publish so it is testable without a
// broker or a node: whether this node's reading of the counters is the fleet's
// at all ([budgetReporter.countersCurrent]), whether the counters could be
// read, and whether anything is capped. Everything [budgetReporter.publish]
// adds — the incarnation, the sequence, the envelope — is stamped on a frame
// this already decided to send.
//
// A FAILED READ PUBLISHES NOTHING, rather than a frame of zeroes: the
// consumer REPLACES what it holds on every report, so a zeroed one would
// render a company that is spending as a company that has spent nothing —
// which is the one reading an operator acts on by doing nothing.
func (r *budgetReporter) frame(ctx context.Context, now time.Time) (types.BudgetReported, bool) {
	e := r.engine
	company := e.Company()
	if company == nil || company.Org == nil {
		return types.BudgetReported{}, false
	}
	if !r.countersCurrent(ctx) {
		return types.BudgetReported{}, false
	}
	windows := coord.WindowsAt(now, company.Config.Location())
	usage, err := e.backends.Fleet.Usage(ctx, windows)
	if err != nil {
		if ctx.Err() == nil {
			log.DebugContext(ctx, "budget_report_skipped", "error", err.Error(),
				"detail", "the shared counter could not be read, so the live "+
					"meters keep the last frame they had")
		}
		return types.BudgetReported{}, false
	}
	return budgetSnapshot(company, windows, usage)
}

// countersCurrent reports whether this node's reading of the counters is the
// fleet's: whether every live lease is at [coord.WindowedCountersProtocol] or
// later.
//
// DURING THE ROLLING UPGRADE THAT WINDOWED THE COUNTERS, it is not. The older
// nodes run every seat and charge the lifetime counter; this build reads the
// windowed ones, which nothing charges until the older nodes have left, since
// a newer node claims no seat beside them. Its frames would read the company
// as having spent nothing, and every dashboard folds each node's frame over
// the last — so the header would flicker between the two readings for as long
// as the rollout took. It publishes nothing until then, and the older nodes'
// frames are the meter.
//
// A floor that cannot be read, or that sees no live lease at all, is not yet:
// a frame skipped costs one interval of a meter that keeps its last reading.
func (r *budgetReporter) countersCurrent(ctx context.Context) bool {
	if r.current.Load() {
		return true
	}
	leases := r.engine.backends.Coord
	if leases == nil {
		// No lease store is one process with nobody to disagree with.
		r.current.Store(true)
		return true
	}
	floor, live, err := leases.FleetProtocolFloor(ctx)
	if err != nil || !live || floor < coord.WindowedCountersProtocol {
		return false
	}
	r.current.Store(true)
	return true
}

// budgetSnapshot is the frame one read of the shared counters makes, and false
// when nothing in the company is capped.
//
// Split from the publish so what a frame SAYS is testable without a broker, a
// node and a fleet: the reading of the counter is the whole of what can be
// wrong with it, and it was the half nothing exercised.
//
// ONE FIGURE PER SCOPE, because that is what the frame carries: each scope's
// [coord.Usage.Binding] window — a refusing window first, else the capped one
// with the least room left — with that window's ceiling and its refusal, so a
// meter never pairs one window's spend with another's cap. A scope nothing
// has charged reads [coord.Unspent].
func budgetSnapshot(company *Company, windows coord.Windows, usage []coord.Usage) (types.BudgetReported, bool) {
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

	var report types.BudgetReported
	orgCaps := coord.Caps(company.Org.TokenBudget)
	orgSlot, orgLimit, _ := read(coord.OrgScope).Binding(orgCaps)
	report.OrgUsedTokens, report.OrgMaxTokens = orgSlot.Used, orgLimit
	report.OrgRefusedAt = refusedAt(orgSlot)

	// ONLY METERED SEATS, which is what the payload promises: absence
	// means "no cap and no meter", and a seat listed at a cap of zero
	// would be drawn as an empty bar rather than as no bar at all.
	type seatMeter struct {
		scope string
		meter types.BudgetMeter
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
		slot, limit, _ := read(scope).Binding(caps)
		seats = append(seats, seatMeter{scope: scope, meter: types.BudgetMeter{
			AgentID: agentID.String(), Role: seat.Name,
			UsedTokens: slot.Used, MaxTokens: limit, RefusedAt: refusedAt(slot),
		}})
	}
	// SORTED BY SCOPE, so two frames of an unchanged company are
	// byte-identical and a consumer diffing them sees nothing move.
	slices.SortFunc(seats, func(a, b seatMeter) int { return strings.Compare(a.scope, b.scope) })
	for _, seat := range seats {
		report.Agents = append(report.Agents, seat.meter)
	}
	if len(orgCaps) == 0 && len(report.Agents) == 0 {
		// NOTHING IS CAPPED, so there is no meter to render and a frame
		// would be a header bar over an unlimited budget.
		return types.BudgetReported{}, false
	}
	return report, true
}

// refusedAt renders a window's refusal stamp the way the payload carries it:
// RFC 3339 in UTC, and empty for a window that is not refusing.
//
// EMPTY, never the zero instant spelled out. The dashboard tests the field for
// presence, so "0001-01-01T00:00:00Z" would put every capped seat in its
// attention queue as refusing charges since the first century.
func refusedAt(slot coord.WindowUsage) string {
	if slot.RefusedAt.IsZero() {
		return ""
	}
	return slot.RefusedAt.UTC().Format(time.RFC3339Nano)
}
