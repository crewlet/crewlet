package engine

import (
	"context"

	"github.com/crewlet/crewlet/internal/org"
)

// RefreshChartForTest brings this node's chart view up to its applier's cursor
// and reports the position the published view carries.
//
// EXPORTED FOR A TEST ONLY, because the four things that call it in production
// are a hook, a timer and two boot steps — none of which a test can drive
// deterministically, and all of which would turn a case about the DERIVATION
// into a case about a timer. What is asserted through it is exactly what those
// four share: the no-op at an equal cursor, and the view a rebuild publishes.
func RefreshChartForTest(ctx context.Context, e *Engine) (org.ViewPosition, error) {
	return e.refreshChart(ctx)
}

// ViewPosition is the chart position the published company was derived from,
// and whether this engine has read a chart at all.
//
// EXPORTED FOR A TEST ONLY. What it exists for is the one comparison that
// makes the activation stamp mean something: a case asserting the recorded
// position against a constant would pass for a stamp that wrote any number.
func (e *Engine) ViewPosition() (org.ViewPosition, bool) { return e.epoch.viewAt() }
