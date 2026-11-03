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
// [NativeSources]' reason: a harness that wired its own, or none, would serve
// a node whose read surface the binary never builds — and the harness did
// serve one without it, so no end-to-end case could ask a node its retention.
//
// NIL RATHER THAN AN EMPTY DOCUMENT, on the rule every optional source
// follows: a process that runs no trim has no floor it published and no terms
// it evaluated, and a report of zeros would claim a fleet whose log is
// perfectly trimmed. The question is simply unregistered instead.
//
// IT TAKES THE CALLER'S CONTEXT FOR THE PROBE and not one of its own: the
// probe is a coordination read, so on a node whose store cannot be reached it
// is exactly as slow as every other one — and a `context.Background()` here
// made the ONE call that decides whether this question exists at all the one
// call a shutting-down process could not abandon.
//
// The answer itself is assembled per call, because half of it is coordination
// that changes under the answer and the other half is this node's own loops.
func RetentionSource(ctx context.Context, e *engine.Engine) func(context.Context) any {
	if _, runs := e.RetentionReport(ctx); !runs {
		return nil
	}
	return func(ctx context.Context) any {
		report, _ := e.RetentionReport(ctx)
		return report
	}
}
