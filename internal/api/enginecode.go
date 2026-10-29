package api

import (
	"github.com/crewlet/crewlet/internal/agent/builtin"
	"github.com/crewlet/crewlet/internal/api/queries"
	"github.com/crewlet/crewlet/internal/code/host"
	"github.com/crewlet/crewlet/internal/engine"
)

// EngineCode is [CodeSurface] over a running engine — its git host, the code
// domain's reads and who read a unit's repository, each resolved on EVERY
// CALL, and the box tokens every node holds from boot — or nil for a nil
// engine, which [New] refuses by name.
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
// copy that serves, wherever it is (ADR-0049) — or why this node's running
// runtime serves none ([engine.OperatorCode], [engine.Engine.CodeUnserved]),
// asked again after the reason for GitRoutes' reason.
func (c engineCode) CodeReads() (queries.CodeReads, error) {
	if seam, ok := engine.OperatorCode(c.e); ok {
		return seam, nil
	}
	why := c.e.CodeUnserved()
	if seam, ok := engine.OperatorCode(c.e); ok {
		return seam, nil
	}
	return nil, why
}

// RepoReaders is who read a unit's repository — the usage domain's readers
// through this node's estate router, with the company's clock — on CodeReads'
// terms ([engine.CodeReaderDeps]): where this node's running runtime serves no
// code host, why, asked again after the reason for GitRoutes' reason.
func (c engineCode) RepoReaders() (builtin.ReadersDeps, error) {
	if deps := engine.CodeReaderDeps(c.e); deps.Readers != nil {
		return deps, nil
	}
	why := c.e.CodeUnserved()
	if deps := engine.CodeReaderDeps(c.e); deps.Readers != nil {
		return deps, nil
	}
	return builtin.ReadersDeps{}, why
}

// GitReads is the code domain's reads by object id through this node's estate
// router — the repository's home git node first — on CodeReads' terms
// ([engine.OperatorCode]): where this node's running runtime serves no code
// host, why, asked again after the reason for GitRoutes' reason.
func (c engineCode) GitReads() (GitReader, error) {
	if seam, ok := engine.OperatorCode(c.e); ok {
		return seam, nil
	}
	why := c.e.CodeUnserved()
	if seam, ok := engine.OperatorCode(c.e); ok {
		return seam, nil
	}
	return nil, why
}

// VerifyBox is whether token is the credential user presents on a box route,
// signed by this fleet (host.VerifyBox), by the engine's own box and check
// tokens ([engine.Engine.BoxTokens], [engine.Engine.CheckTokens]): built at
// New on every node over the keyring Tier A requires there, so it answers on
// a node whose code host is not built — the one place it is asked.
func (c engineCode) VerifyBox(user, token string) bool {
	return host.VerifyBox(c.e.BoxTokens(), c.e.CheckTokens(), user, token)
}
