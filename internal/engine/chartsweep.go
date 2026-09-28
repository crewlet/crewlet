package engine

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/crewlet/crewlet/internal/chart"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/fleetsecrets"
	"github.com/crewlet/crewlet/internal/maintenance"
)

// THE VALUES NOTHING NAMES ANY MORE.
//
// A sealed value outlives the field that named it: clearing a seat's address,
// replacing a literal token with the operator's own `${VAR}`, removing the
// object — each leaves the value in the store, and the store has no retention
// of its own, so every one of them outlived the company. The sweep collects
// them, and where it belongs follows from what it has to prove:
//
//   - NOT THE WRITER, after a landed removal or a clearing write. Other rows
//     may name the value, a concurrent write on the same field re-seals under
//     the same name, and the writer's post-publish step is exactly the window
//     in which that write's seal lands — so the writer would destroy a value
//     a row it cannot see is about to name.
//   - NOT AN APPLIER HOOK on every node, which is N nodes deleting one shared
//     row on N clocks, each proving nothing the others did not.
//   - THE RETENTION SWEEP: one fleet singleton, deciding from rows PROVED to
//     hold every record the chart log held ([chart.Reader.SealedNames]), only
//     after [chart.SealGrace] and only rows this domain wrote
//     ([chart.OrphanedSeals]), deleting each at the version it judged — so a
//     re-seal between the listing and the delete is spared, and a second
//     node running the same tick deletes nothing twice.

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
	if c := e.Company(); c != nil && c.Config != nil {
		for _, name := range config.ReferencedNames(c.Config) {
			named[name] = true
		}
	}
	store := fleetsecrets.New(e.backends.Fleet, e.cipher)
	held, err := store.List(ctx)
	if err != nil {
		return 0, err
	}
	var removed int64
	for _, row := range chart.OrphanedSeals(held, named, now) {
		gone, err := store.UnsetAt(ctx, row.Name, row.Version)
		if err != nil {
			return removed, err
		}
		if gone {
			removed++
			log.InfoContext(ctx, "chart_sealed_value_collected", "name", row.Name,
				"detail", "no row of the org chart and nothing in the running "+
					"settings names it any more")
		}
	}
	return removed, nil
}

// chartLogEnd is the chart log's last sequence, as the broker holds it now —
// what a census of this node's chart rows is proved against.
func (e *Engine) chartLogEnd(ctx context.Context) (uint64, error) {
	if e.native == nil || e.native.log == nil {
		return 0, errors.New("engine: this node runs no chart domain, so there " +
			"is no chart log to read the end of")
	}
	running := e.native.log.Domain(chart.Domain{}.Name())
	if running == nil {
		return 0, errors.New("engine: the chart log is not running on this node")
	}
	end, err := running.log.End(ctx)
	if err != nil {
		return 0, fmt.Errorf("engine: read how far the chart log goes: %w", err)
	}
	return end, nil
}
