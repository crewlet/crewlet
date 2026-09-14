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

// publish reads every counter and puts one snapshot on the stream.
//
// A FAILED READ PUBLISHES NOTHING, rather than a frame of zeroes: the
// consumer REPLACES what it holds on every report, so a zeroed one would
// render a company that is spending as a company that has spent nothing —
// which is the one reading an operator acts on by doing nothing.
func (r *budgetReporter) publish(ctx context.Context) {
	e := r.engine
	company := e.Company()
	if company == nil || company.Org == nil {
		return
	}
	usage, err := e.backends.Fleet.Usage(ctx)
	if err != nil {
		if ctx.Err() == nil {
			log.DebugContext(ctx, "budget_report_skipped", "error", err.Error(),
				"detail", "the shared counter could not be read, so the live "+
					"meters keep the last frame they had")
		}
		return
	}
	report, metered := budgetSnapshot(company, usage)
	if !metered {
		return
	}
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

// budgetSnapshot is the frame one read of the shared counter makes, and false
// when nothing in the company is capped.
//
// Split from the publish so what a frame SAYS is testable without a broker, a
// node and a fleet: the reading of the counter is the whole of what can be
// wrong with it, and it was the half nothing exercised.
func budgetSnapshot(company *Company, usage []coord.Usage) (types.BudgetReported, bool) {
	report := types.BudgetReported{OrgMaxTokens: company.Config.TokenBudget}
	// ONLY METERED SEATS, which is what the payload promises: absence
	// means "no cap and no meter", and a seat listed at a cap of zero
	// would be drawn as an empty bar rather than as no bar at all.
	byScope := make(map[string]*types.BudgetMeter, len(usage))
	var scopes []string
	for seat := range company.Org.AllRoles() {
		if !seat.IsAgent() {
			continue
		}
		limit := seatBudget(company.Org, seat)
		if limit <= 0 {
			continue
		}
		agentID, ok := company.Org.AgentIDFor(seat)
		if !ok {
			continue
		}
		scope := coord.AgentScope(agentID.String())
		byScope[scope] = &types.BudgetMeter{
			AgentID: agentID.String(), Role: seat.Name, MaxTokens: limit,
		}
		scopes = append(scopes, scope)
	}
	for _, row := range usage {
		if row.Scope == coord.OrgScope {
			report.OrgUsedTokens = row.Used
			report.OrgRefusedAt = refusedAt(row)
			continue
		}
		if meter, metered := byScope[row.Scope]; metered {
			meter.UsedTokens = row.Used
			meter.RefusedAt = refusedAt(row)
		}
	}
	// SORTED BY SCOPE, so two frames of an unchanged company are
	// byte-identical and a consumer diffing them sees nothing move.
	slices.Sort(scopes)
	for _, scope := range scopes {
		report.Agents = append(report.Agents, *byScope[scope])
	}
	if report.OrgMaxTokens <= 0 && len(report.Agents) == 0 {
		// NOTHING IS CAPPED, so there is no meter to render and a frame
		// would be a header bar over an unlimited budget.
		return types.BudgetReported{}, false
	}
	return report, true
}

// refusedAt renders a scope's refusal stamp the way the payload carries it:
// RFC 3339 in UTC, and empty for a scope that is not refusing.
//
// EMPTY, never the zero instant spelled out. The dashboard tests the field for
// presence, so "0001-01-01T00:00:00Z" would put every capped seat in its
// attention queue as refusing charges since the first century.
func refusedAt(row coord.Usage) string {
	if row.RefusedAt.IsZero() {
		return ""
	}
	return row.RefusedAt.UTC().Format(time.RFC3339Nano)
}
