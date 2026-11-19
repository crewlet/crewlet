package api

import (
	"context"

	"github.com/crewlet/crewlet/internal/engine"
)

// RetentionSource is the read surface's retention question over e — how far
// each domain's log may be trimmed, what is stopping it, and what this node
// costs to replace ([queries.Sources.Retention]) — or nil on a node that armed
// no trim: a maintenance-mode node, which publishes nothing, its trim included.
//
// ONE CONSTRUCTOR for `crewlet run` and the end-to-end suite, for
// [NativeSources]' reason. NIL RATHER THAN AN EMPTY DOCUMENT: a process that
// runs no trim has no floor or terms, and zeros would claim a perfectly
// trimmed log. IT TAKES THE CALLER'S CONTEXT FOR THE PROBE: a coordination
// read on an unreachable store is slow, and a `context.Background()` would
// leave a shutdown unable to abandon it. The answer is assembled per call:
// half is coordination that changes under it.
func RetentionSource(ctx context.Context, e *engine.Engine) func(context.Context) any {
	if _, runs := e.RetentionReport(ctx); !runs {
		return nil
	}
	return func(ctx context.Context) any {
		report, _ := e.RetentionReport(ctx)
		return report
	}
}
