package engine

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/crewlet/crewlet/internal/chart"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/fleetsecrets"
	"github.com/crewlet/crewlet/internal/maintenance"
)

// THE VALUES NOTHING NAMES ANY MORE.
//
// A sealed value outlives the field that named it: clearing a seat's address,
// rotating a credential (which seals the new one under the rotating write's
// own name), replacing a literal token with the operator's own `${VAR}`,
// removing the object, a write that sealed and then never landed — each
// leaves a value in the store, and the store has no retention of its own, so
// every one of them outlived the company. The sweep collects them, and where
// it belongs follows from what it has to prove:
//
//   - NOT THE WRITER, after a landed removal or a clearing write. Other rows
//     may name the value, a concurrent write may be about to name it again,
//     and the writer's post-publish step is exactly the window in which that
//     write's record lands — so the writer would destroy a value a row it
//     cannot see is about to name.
//   - NOT AN APPLIER HOOK on every node, which is N nodes deleting one shared
//     row on N clocks, each proving nothing the others did not.
//   - THE RETENTION SWEEP: one fleet singleton, deciding from rows PROVED to
//     hold every record the chart log held ([chart.Reader.SealedNames]), only
//     once it has seen nothing name a value for [chart.SealGrace] and only
//     rows this domain wrote ([chart.OrphanedSeals]), deleting each at the
//     version it judged — so a value a writer held between the listing and
//     the delete is spared, and a second node running the same tick deletes
//     nothing twice.
//
// THE SIGHTINGS ARE THIS PROCESS'S ([Engine.chartSeals]), because they are the
// sweep's own series of judgements and a duty that moves on a lease carries
// no memory across the move: the node that takes the duty over starts with
// none and waits a whole grace before it deletes anything, which is the safe
// direction.

// chartSealJob is the retention sweep's collection of sealed values nothing
// names any more.
func (e *Engine) chartSealJob() maintenance.Job {
	return maintenance.Job{
		Name: "chart_sealed_values", Scope: maintenance.Fleet,
		Run: func(ctx context.Context, now, _ time.Time) (int64, error) {
			return e.collectChartSeals(ctx, now)
		},
	}
}

// collectChartSeals deletes every value the chart sealed that no row and no
// part of the running settings names, reporting how many it deleted.
//
// ROWS THAT CANNOT VOUCH FOR AN ABSENCE COLLECT NOTHING, and that is a tick
// with nothing to do rather than a failure: a node behind its log, or holding
// a record it could not apply, reads a reference as missing that the log
// holds, and the next tick — here or on the node that takes the duty — asks
// again.
func (e *Engine) collectChartSeals(ctx context.Context, now time.Time) (int64, error) {
	reader := e.Chart()
	if reader == nil || e.backends == nil || e.backends.Fleet == nil {
		return 0, nil
	}
	// A NODE SERVING NO COMPANY CANNOT VOUCH FOR THE SETTINGS' HALF, and
	// collects nothing for the reason rows that cannot vouch collect
	// nothing. A person may point a setting at a chart's sealed value by
	// hand, and a node that has not applied the fleet's settings — one that
	// booted before any revision, or cannot apply the current one — would
	// read every such value as named by nothing. The chart is the core's
	// and open on every node, so this sweep reaches such a node now; before
	// the chart was, a node with no company had no chart to sweep from.
	settings := e.Company()
	if settings == nil || settings.Config == nil {
		log.InfoContext(ctx, "chart_seal_sweep_deferred",
			"reason", "this node serves no company, so it cannot say whether "+
				"the running settings name a sealed value")
		return 0, nil
	}
	// THE END FIRST, then the rows proved against it: see
	// [chart.Reader.SealedNames].
	end, err := e.chartLogEnd(ctx)
	if err != nil {
		return 0, err
	}
	named, err := reader.SealedNames(ctx, end)
	if errors.Is(err, chart.ErrNotCurrent) {
		log.InfoContext(ctx, "chart_seal_sweep_deferred", "reason", err.Error())
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	// AND WHATEVER THE SETTINGS NAME, which a person may point at a chart's
	// sealed value by hand: a value a running provider resolves is not one
	// nothing names.
	for _, name := range config.ReferencedNames(settings.Config) {
		named[name] = true
	}
	store := fleetsecrets.New(e.backends.Fleet, e.cipher)
	held, err := store.List(ctx)
	if err != nil {
		return 0, err
	}
	e.chartSeals.mu.Lock()
	orphans, seen := chart.OrphanedSeals(held, named, e.chartSeals.seen, now)
	e.chartSeals.seen = seen
	e.chartSeals.mu.Unlock()
	var removed int64
	for _, row := range orphans {
		gone, err := store.UnsetAt(ctx, row.Name, row.Version)
		if err != nil {
			return removed, err
		}
		if gone {
			removed++
			log.InfoContext(ctx, "chart_sealed_value_collected", "name", row.Name,
				"detail", "no row of the org chart and nothing in the running "+
					"settings has named it for as long as the sweep's grace")
		}
	}
	return removed, nil
}

// chartSealSightings is the orphan sweep's memory of which sealed values it
// has seen nothing name, and since when — see [chart.SealSighting].
type chartSealSightings struct {
	mu   sync.Mutex
	seen map[string]chart.SealSighting
}

// chartLogEnd is the chart log's last sequence, as the broker holds it now —
// what a census of this node's chart rows is proved against.
func (e *Engine) chartLogEnd(ctx context.Context) (uint64, error) {
	s, err := e.stateLogOf()
	if err != nil {
		return 0, fmt.Errorf("engine: this node runs no chart domain, so there "+
			"is no chart log to read the end of: %w", err)
	}
	running := s.Domain(chart.Domain{}.Name())
	if running == nil {
		return 0, errors.New("engine: the chart log is not running on this node")
	}
	end, err := running.log.End(ctx)
	if err != nil {
		return 0, fmt.Errorf("engine: read how far the chart log goes: %w", err)
	}
	return end, nil
}
