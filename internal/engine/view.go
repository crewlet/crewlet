package engine

import (
	"context"
	"sync"
	"time"

	"github.com/crewlet/crewlet/internal/chart"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/org"
	"github.com/crewlet/crewlet/internal/statelog"
)

// THE COMPANY IS TWO THINGS NOW, AND A READ SEES ONE VALUE.
//
// # What changed
//
// A company used to be one document, so an epoch was one build of it and one
// atomic pointer served every reader. The org chart is a log now, and the two
// halves move on completely different rhythms:
//
//   - THE SETTINGS EPOCH changes when an operator activates a revision. That
//     is a few times a month, it rebuilds the providers and the tool
//     catalogue, and every node applies the same one.
//   - THE CHART VIEW changes when somebody is hired, moved or promoted. It is
//     derived from THIS NODE'S OWN ROWS at the position its applier has
//     reached, so two nodes legitimately hold different views for as long as
//     one is behind the other.
//
// Keeping them in one pointer would mean one of two wrong things: rebuilding
// the providers because a seat's goal was reworded, or leaving the chart stale
// until the next config activation, which may be weeks.
//
// # Why a read still gets ONE value
//
// [Engine.Company] is read by every turn, every prompt section, every route.
// Its contract is the reason the epoch exists at all: a caller takes it ONCE
// and reads that value, because two calls can straddle a publish and a caller
// that read the org from one and the models from the next is running a company
// that never existed.
//
// Two pointers would put that hazard back, one layer down — a reader that
// loaded the settings and then the view could straddle either swap. So the
// composition happens on the WRITE side: whichever half moves, one value is
// built from both and published, and readers keep the single atomic load they
// already had. The identity of that value is also load-bearing ([Engine.indexes]
// compares the party registry's company by pointer), which a value composed
// per read could not provide.

// chartView holds the two halves and the composition readers load.
//
// The mutex covers the two INPUTS and the build between them, and it is not
// what readers take: they load the composed pointer. There is one writer at a
// time by construction — an apply holds [Engine.applying], and the rebuild is
// serialized by the coalescing window below — and the lock is what makes the
// pair consistent rather than what makes it safe to read.
type chartView struct {
	mu       sync.Mutex
	settings *Company

	// rows are the chart this node last read, and `read` says whether it
	// has read one at all.
	//
	// THE ROWS RATHER THAN THE BUILT VIEW, and that is not an
	// implementation detail. A view carries the company's NAME, its
	// mission and its token budget, and every one of those is a SETTING —
	// so a view is a function of both halves, and storing the built one
	// would freeze whichever settings happened to be current when the
	// chart last moved.
	//
	// That is not a cosmetic staleness. A seat's agent id is derived from
	// the company name and the handle, so a view built before the first
	// settings epoch existed would derive every id from an empty name:
	// every mailbox, every budget row and every memory row under an
	// identity no other node would compute. Deriving inside the
	// composition makes the two halves impossible to pair wrongly.
	rows chart.Chart
	read bool

	// rebuilding is the coalescing window's state: whether a pass is
	// running, who arrived during the current one (`waiters`) and who is
	// owed the pass after it (`pending`).
	//
	// TWO LISTS because a waiter must not be woken by the pass it arrived
	// during: that pass read the applier's cursor before the waiter called,
	// so it may not carry what the waiter is waiting for. Arrivals move to
	// `pending` when a pass ends, the holder runs one more, and `pending`
	// is woken by THAT one.
	rebuilding bool
	waiters    []chan struct{}
	pending    []chan struct{}
}

// setSettings publishes a new settings epoch, recomposing with the view this
// node currently holds.
func (e *epoch) setSettings(c *Company) *Company {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.settings = c
	return e.compose()
}

// setRows publishes a chart this node has read, recomposing the company.
func (e *epoch) setRows(rows chart.Chart) *Company {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.rows, e.read = rows, true
	return e.compose()
}

// viewAt is the position the published company was derived from.
func (e *epoch) viewAt() (org.ViewPosition, bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if !e.read {
		return org.ViewPosition{}, false
	}
	return org.ViewPosition{
		Generation: e.rows.Position.Generation,
		Seq:        e.rows.Position.Seq,
	}, true
}

// compose derives the value readers see from the two halves, and publishes it.
//
// # The derivation runs HERE rather than where the rows were read
//
// A company view is a function of BOTH halves — the rows give the units, the
// seats and the edges, and the settings give the name, the mission, the
// policies and the token budget. Deriving it where the rows arrive would pair
// them with whichever settings epoch happened to be current then, and the
// first such pairing at boot has no settings epoch at all.
//
// Running it here means the two can never come apart, at the cost of
// re-deriving the tree when the SETTINGS move. That is the cheaper side of the
// trade by a wide margin: settings move a few times a month and the derivation
// is 44 ms at twenty thousand seats, while the chart moves on every hire.
//
// # A node with no rows read yet serves the document's own tree
//
// That is the `crewlet validate` shape — an engine built to check a document,
// which applies to nothing and never opens a chart — and the boot window
// before the first read returns. A running node leaves it within the boot:
// the seed publishes and the first rebuild reads, both before the first epoch
// is installed.
//
// Called under the lock.
func (e *epoch) compose() *Company {
	out := e.composed(e.settings)
	e.current.Store(out)
	return out
}

// withView is [epoch.composed] for a settings epoch that is not published yet.
//
// # Why an apply needs this before it runs a single stage
//
// A config apply builds its epoch from the revision's own bytes, and those
// carry no org chart — so the value every stage would otherwise wire against
// has a roster of NOBODY. Measured, that was an apply reporting `seats=0` on a
// company with seats, wiring each integration against an empty roster and
// leaving every code-host webhook naming a stranger until something else
// happened to republish.
//
// It is the SAME derivation the read side publishes, reached through the same
// function, because two derivations of one company is how the value a stage
// wired against and the value a turn reads stop agreeing.
func (e *epoch) withView(settings *Company) *Company {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.composed(settings)
}

// composed derives the value a reader sees from one settings epoch and this
// node's rows, and publishes nothing.
//
// Called under the lock.
func (e *epoch) composed(settings *Company) *Company {
	if settings == nil {
		return nil
	}
	if !e.read {
		return settings
	}
	// A COPY rather than a mutation of the settings epoch: an in-flight
	// turn is holding the previous composition, and editing the value it
	// points at would move the org underneath it — which is the one thing
	// this whole design exists to prevent.
	composed := *settings
	view := org.FromRows(e.rows, config.OrgSettings(settings.Config))
	composed.Org = view.Org
	if len(view.Reparented) > 0 {
		// SAID ON EVERY COMPOSITION rather than once: a cycle cannot
		// reach the rows through the write path, so a build that finds
		// one is reading rows written by a build without that rule or
		// repaired by hand, and the operator has to know which team
		// moved.
		log.Warn("chart_cycle_broken", "units", view.Reparented,
			"detail", "these units form a cycle in this node's chart rows and "+
				"were re-parented to the root so the company can run; every "+
				"node breaks it identically. A write through the chart's own "+
				"log is what moves them back")
	}
	return &composed
}

// refreshChart brings this node's chart view up to the position its applier
// has reached, publishes the company that composes, and reconciles what
// follows a company. It reports the position the published view carries.
func (e *Engine) refreshChart(ctx context.Context) (org.ViewPosition, error) {
	at, err := e.rebuildChart(ctx)
	if err != nil {
		return org.ViewPosition{}, err
	}

	// AND WHAT FOLLOWS THE COMPANY FOLLOWS THIS TOO. A chart write publishes
	// a new company exactly as a config apply does, so anything an apply
	// reconciles against its new epoch has to be reconciled here, or it
	// follows only half of what a company is. Two things do today.
	//
	// THE PARTY REGISTRY is derived from one org and answers for it
	// permanently, so a company with a new seat needs a new one. A registry
	// rebuilt only on an apply leaves a seat hired this morning
	// unaddressable until somebody happens to change a provider — with
	// nothing failing, because every lookup answers "nobody matches" the
	// way it does for a stranger.
	//
	// THE SCHEDULER is the other. A seat's `schedules:` ride the chart, so
	// a founder giving somebody their first standup is a chart write, and a
	// loop armed only on an apply fires nothing until the next one — which
	// on a company nobody is reconfiguring is never.
	//
	// # Why this runs on EVERY call and not only on a rebuild
	//
	// Because a caller that finds the view already current has to be able
	// to rely on the answer. The rebuild is coalesced — a caller arriving
	// while one runs waits for the next pass, and one arriving after it
	// returns early — so a hook inside the rebuild is a hook some callers
	// never reach, on a pass another goroutine may still be finishing.
	// Running it here makes the guarantee the same for all three exits.
	//
	// Both are idempotent at an unchanged answer: the registry is skipped
	// when it was built from exactly this company, and the scheduler is
	// armed or disarmed rather than rebuilt. So the ordinary call costs two
	// comparisons. It takes the rebuild's own context, which outlives any
	// one request; [Engine.armSchedulerLocked] strips its cancellation.
	if published := e.Company(); published != nil {
		if !e.indexes(published) {
			e.refreshParties(published)
		}
		e.reconcileScheduler(ctx, published)
	}
	return at, nil
}

// rebuildChart is [Engine.refreshChart]'s derivation, without what follows it.
//
// # It is a no-op at an equal cursor
//
// The triggers fire on every committed record, on a timer, and at two points
// during boot and rejoin. Most of those find the view already current, and
// rebuilding it anyway would re-derive a twenty-thousand-seat tree to arrive
// at the same bytes — so the cheap comparison comes first.
//
// # One rebuild serves every waiter, and a waiter gets a FRESH pass
//
// An import publishes a thousand records; a config apply publishes one per
// object. Without coalescing, each committed record would queue a full
// derivation and the node would spend the import re-deriving the same tree.
//
// So a caller that arrives while one is running does not start a second: it
// waits. What it waits for is not the running pass, which read the applier's
// cursor BEFORE that caller arrived and may not carry what the caller is
// waiting for — it is another pass, which the holder runs before it wakes
// anybody. So a burst of N records costs two derivations rather than N, and
// the guarantee a waiter gets is the one it needs: everything committed
// before it called.
func (e *Engine) rebuildChart(ctx context.Context) (org.ViewPosition, error) {
	reader := e.Chart()
	if reader == nil {
		// A node with no chart runtime has nothing to derive from. It is
		// not an error: `crewlet validate` builds an engine that applies
		// to nothing, and the composition falls back to the settings
		// epoch's own tree.
		return org.ViewPosition{}, nil
	}
	at := reader.At()
	if held, ok := e.epoch.viewAt(); ok && held.Generation == at.Generation &&
		held.Seq == at.Seq {
		return held, nil
	}

	wait, mine := e.epoch.claimRebuild()
	if !mine {
		select {
		case <-wait:
			held, _ := e.epoch.viewAt()
			return held, nil
		case <-ctx.Done():
			return org.ViewPosition{}, ctx.Err()
		}
	}

	// THIS NODE'S OWN ROWS, never a linearizable read. What is being
	// derived IS this node's view: asking the fleet whether it is caught
	// up would put a broker round trip on every committed record, and the
	// answer would be out of date by the time the derivation finished. A
	// node behind its peers serves a view that says so — the position is
	// on it — which is the honest shape rather than a wait.
	// ANOTHER PASS FOR ANYBODY WHO ARRIVED DURING THIS ONE, before they
	// are woken: see the note above for why waking them on this pass would
	// hand them a view that predates their own call.
	for {
		rows, err := reader.Read(ctx, statelog.Freshness{Level: statelog.ReadStale})
		if err != nil {
			e.epoch.finishRebuild()
			return org.ViewPosition{}, err
		}
		e.epoch.setRows(rows)
		if e.epoch.finishRebuild() {
			break
		}
	}
	held, _ := e.epoch.viewAt()
	return held, nil
}

// claimRebuild reports whether this caller runs the rebuild, and hands a
// waiter the channel the NEXT one closes.
func (e *epoch) claimRebuild() (<-chan struct{}, bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.rebuilding {
		ch := make(chan struct{})
		e.waiters = append(e.waiters, ch)
		return ch, false
	}
	e.rebuilding = true
	return nil, true
}

// finishRebuild reports whether the holder may stop.
//
// IT KEEPS THE CLAIM when somebody arrived during the pass, so the holder runs
// another one and nobody else starts a competing derivation in between. It
// releases and wakes them only when the last pass had no arrivals — at which
// point every waiter it wakes is waking on a view built after its own call.
func (e *epoch) finishRebuild() (done bool) {
	e.mu.Lock()
	if len(e.waiters) > 0 {
		e.waiters, e.pending = nil, e.waiters
		e.mu.Unlock()
		return false
	}
	waiters := e.pending
	e.pending, e.rebuilding = nil, false
	e.mu.Unlock()
	for _, ch := range waiters {
		close(ch)
	}
	return true
}

// nudgeChart is what the chart's applier calls after a committed batch.
//
// A NON-BLOCKING SIGNAL, never the rebuild itself. This runs on the apply
// loop's own goroutine with the next batch waiting behind it, and a derivation
// that read the estate there would make every import as slow as its own
// re-derivations.
//
// ONE SLOT, which is the coalescing window. An import publishes a thousand
// records and a config apply one per object; a queue would run a full
// derivation per record to arrive at the tree the last one produced. A single
// pending signal collapses a burst into the one rebuild that follows it, and
// that rebuild reads the applier's cursor when it starts — so it sees
// everything committed before the last signal, which is everything the burst
// wrote.
func (e *Engine) nudgeChart() {
	select {
	case e.chartNudge <- struct{}{}:
	default:
	}
}

// watchChartNudges is the committed-hook trigger's own loop.
//
// SEPARATE FROM THE TICKER, because the two answer different questions and
// merging them would answer neither promptly: a nudge means "a write landed,
// show it", and the ticker means "nothing woke me, check anyway".
func (e *Engine) watchChartNudges(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case <-e.chartNudge:
			if _, err := e.refreshChart(ctx); err != nil && ctx.Err() == nil {
				log.WarnContext(ctx, "chart_view_rebuild_failed",
					"error", err, "detail", "a committed chart batch did not "+
						"reach this node's view; it keeps serving the one it "+
						"had and the periodic rebuild retries")
			}
		}
	}
}

// ViewRefresh is how often a node compares its applier's cursor against the
// view it is serving, with nothing having woken it.
//
// THE SAFETY NET RATHER THAN THE MECHANISM. The committed hook is what makes a
// write visible promptly; this covers the ways a cursor moves with no hook at
// all — an adoption that replaces the replicated file wholesale, a node that
// rejoined, a hook that returned while the derivation was failing. Thirty
// seconds, which is the same figure the alarm table calls a stall: a view
// behind its own rows for longer than that is already something an operator is
// being told about, so a slower net would be reporting a fault nothing was
// fixing.
const ViewRefresh = 30 * time.Second

// watchChart is the periodic trigger.
//
// ITS OWN GOROUTINE rather than a tick inside an existing loop, because every
// existing loop is a DUTY: a lease flap would stop the node's own view
// tracking its own rows, which is not a fleet decision and has no business
// depending on one.
func (e *Engine) watchChart(ctx context.Context) {
	ticker := time.NewTicker(ViewRefresh)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if _, err := e.refreshChart(ctx); err != nil && ctx.Err() == nil {
				log.WarnContext(ctx, "chart_view_rebuild_failed",
					"error", err, "detail", "this node keeps serving the view "+
						"it already had, which is behind its own rows")
			}
		}
	}
}
