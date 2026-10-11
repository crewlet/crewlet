package api

import (
	"context"

	"github.com/crewlet/crewlet/internal/api/queries"
	"github.com/crewlet/crewlet/internal/engine"
)

// EngineReads is [queries.Sources.Native] over a running engine: the company's
// tracker and knowledge base as the read surface reaches them AT EACH QUESTION,
// through this node's estate router as a seat's tools reach them
// ([engine.OperatorWork]) — so an operator and an agent looking at one item see
// one item, answered from this node's copy while it serves and from another
// data node's while it does not.
//
// HERE, beside the surface it feeds, for [EngineNative]'s reason: `crewlet run`
// and the end-to-end suite build their read surfaces through this one
// constructor, so a case asking a page or a ranked search of a harness node is
// exercising the view a deployment serves rather than a narrower one the
// harness assembled for itself.
//
// PER QUESTION, never once: the native runtime is built at boot OR at the first
// apply whose company runs a state log, and on a node that booted with no
// company that apply runs after the API was assembled — see
// [queries.Sources.Native]. Each call is ONE reading of the runtime
// ([engine.OperatorNative]): one atomic load on a node without `data`, two on a
// data node, no lock, no I/O and no allocation.
//
// THE CONVERSION IS THE POINT of the function it returns. A typed nil pointer
// in an interface field would pass every answer's guard and then panic on the
// first call, so a field is set only where the runtime holds its backend, and
// is otherwise a genuinely nil interface. Five fields from three halves,
// because the backends are absent independently: the file rows ride the
// tracker's reader; the backlinks are the knowledge base's reader, answered by
// a data node whose copies of both the knowledge base and the tracker serve
// (ADR-0031); and the ranked search is its own half, because it needs this
// node's index as well as the tracker ([engine.OperatorReads.Search]).
func EngineReads(e *engine.Engine) func() queries.Native {
	return func() queries.Native { return nativeView(engine.OperatorNative(e)) }
}

// nativeView is one reading of the runtime as the read surface's view of it.
func nativeView(reads engine.OperatorReads) queries.Native {
	var out queries.Native
	if reads.Tracker {
		out.Work, out.Files = reads.Work, reads.Work
	}
	if reads.Wiki {
		out.Pages, out.Backlinks = reads.Pages, reads.Pages
	}
	if reads.Search {
		out.WorkSearch = reads.Work
	}
	return out
}

// EngineRetention is [queries.Sources.Retention] over a running engine: what
// this node can say about the state log's own history — how far each domain's
// log may be trimmed, what is stopping it, and what this node costs to replace
// — assembled per call, because half of it is coordination that changes under
// the answer and the other half is this node's own loops. WHETHER the node runs
// a state log at all is read per call too, for [EngineReads]' reason: the trim
// starts with the runtime, at an apply that can follow the API's assembly, and
// it stops at the engine's teardown.
func EngineRetention(e *engine.Engine) func(context.Context) (any, bool) {
	return func(ctx context.Context) (any, bool) {
		report, runs := e.RetentionReport(ctx)
		if !runs {
			return nil, false
		}
		return report, true
	}
}
