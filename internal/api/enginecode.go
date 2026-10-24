package api

import "github.com/crewlet/crewlet/internal/engine"

// EngineCode is [CodeSurface] over a running engine — its git host and the
// code domain's reads, each resolved on EVERY CALL — or nil for a nil engine,
// which [New] refuses by name.
//
// HERE, beside the seam it satisfies, so `crewlet run` and any harness serving
// an engine's API reach the code host through the same adapter, and a case
// that clones through the API exercises the wiring a deployment runs.
//
// PER CALL, never once, as the native halves are read (native.go): the code
// host is built
// with the native runtime, at boot or at the first apply whose company sets
// code.native — after the API was assembled on a node that booted with no
// company. A host captured at assembly served such a node's people no git for
// the life of the process. Each call costs what the engine's accessors cost:
// an atomic load, no lock and no I/O.
func EngineCode(e *engine.Engine) CodeSurface {
	if e == nil {
		return nil
	}
	return engineCode{e: e}
}

// engineCode is [EngineCode]'s adapter.
type engineCode struct{ e *engine.Engine }

// GitRoutes is the engine's code host, or why this node runs none
// ([engine.Engine.CodeUnserved]). A nil host is answered absent rather than
// handed over inside the interface, where it would pass the route's check
// and panic on the first request.
func (c engineCode) GitRoutes() (GitRoutes, error) {
	if h, ok := c.e.CodeHost(); ok && h != nil {
		return h, nil
	}
	why := c.e.CodeUnserved()
	// ASKED AGAIN after the reason, because an apply publishes the runtime
	// before its epoch: a host that came up between the two reads would
	// otherwise be answered with the reason the new epoch now gives — a
	// restart this node does not need.
	if h, ok := c.e.CodeHost(); ok && h != nil {
		return h, nil
	}
	return nil, why
}

// CodeReads is the code domain's read through this node's estate router — the
// copy that serves, wherever it is (ADR-0045) — or why this node's running
// runtime serves none ([engine.OperatorCode], [engine.Engine.CodeUnserved]),
// asked again after the reason for GitRoutes' reason.
func (c engineCode) CodeReads() (CodeReads, error) {
	if seam, ok := engine.OperatorCode(c.e); ok {
		return seam, nil
	}
	why := c.e.CodeUnserved()
	if seam, ok := engine.OperatorCode(c.e); ok {
		return seam, nil
	}
	return nil, why
}
