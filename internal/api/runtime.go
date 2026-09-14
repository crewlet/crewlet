// Package api serves the REST surface and the dashboard.
//
// # There is one shape: the API is served beside the engine
//
// `crewlet run` is the only thing that builds an [App], and it builds one inside
// the engine's own process, over the engine's own store, broker and
// coordination plane. What `node.roles` changes is what that engine does (an
// ingress-only node claims no seats and runs no worker duties), never whether
// it exists, and a node with `api.port: 0` builds no App at all. There is no
// API process without an engine.
//
// This package was once written for a "standalone API" as well, and every
// dependency the engine supplies was optional to serve it: a nil [NodeRuntime]
// reported `engine: false` and omitted the engine's fields, a nil budget counter
// answered `no_coordination_store`, a nil config service left /config
// unregistered. No process ever took those branches, and they described a
// deployment the binary cannot run. [New] now refuses a missing dependency by
// name instead.
//
// [NodeRuntime] stays a seam for what it always was underneath: the facts this
// package asks the engine for, declared by the consumer, so a route can be
// tested against fixed answers without standing a node up.
package api

import (
	"context"
	"time"

	"github.com/crewlet/crewlet/internal/logging"
)

var log = logging.Get("api")

// RuntimeState is what only the engine can answer.
//
// A SNAPSHOT, taken in one call, rather than a field read per question. Six
// independent reads describe six different instants, and a health body that
// mixed them would report a node draining with turns in flight from before the
// drain began — which is exactly the window an operator is watching.
type RuntimeState struct {
	// InFlight is how many turns are running.
	InFlight int

	// ShuttingDown is true from the moment a drain begins. It is what
	// takes the node out of rotation while it stays alive.
	ShuttingDown bool

	// Posture is what this node concluded about its own config lag:
	// serve, wait, shed, isolated or stuck. The only place an operator can
	// see WHY a node left rotation — /ready reports a bare 503 either way,
	// and "draining" and "cannot apply epoch 41" call for opposite
	// responses.
	Posture string

	// AppliedEpoch is the config revision this node is running.
	AppliedEpoch int64

	// StallLag is how far behind the node's watched duty is, and the
	// early warning before the watchdog ends the process at the lease TTL.
	// Zero is "nothing to report" rather than "measured as zero", which is
	// the same answer a health surface wants for both.
	StallLag time.Duration

	// StartedAt is when the engine started, which is this node's start:
	// the API is served inside the engine's process.
	StartedAt string

	// Seats are the handles this node is serving. The first question about
	// any fleet, and one previously answerable only by reading three
	// processes' logs at debug level.
	Seats []string

	// RoutedSources names the integrations whose deliveries can actually
	// wake a seat — the ones with a parser, not the ones with a config
	// block.
	//
	// Nil means "cannot say", never "none route": an engine mid-boot, or
	// one with no active revision, has not started notifications yet.
	// That is the opposite of an empty slice, which is a real claim that
	// nothing routes.
	RoutedSources []string

	// VerifiableSources names the integrations whose RESOLVED verification
	// material could accept a delivery right now.
	//
	// The other half of RoutedSources, and it answers the earlier question:
	// routed says a verified delivery would reach a seat, this says one
	// would be verified at all. Both depend on what the process resolved
	// rather than on what the document says — a secret is a ${VAR}, and one
	// that did not resolve renders as configured while the route answers
	// 503 to every delivery and the third-party app reports a healthy hook.
	//
	// Nil is "cannot say", exactly as above, and an empty slice is the real
	// claim that nothing here can verify anything.
	VerifiableSources []string
}

// NodeRuntime is the seam for facts only the engine can answer.
//
// Required: every process that serves the API runs the engine beside it, so
// there is no process for which these facts are unknowable, and [New] refuses
// an App without one.
type NodeRuntime interface {
	// Snapshot is this node's live state.
	//
	// It TAKES A CONTEXT because it does I/O: the posture is read from the
	// coordination plane on every call, deliberately — a cached one is a
	// node that reports healthy through the whole window in which it
	// stopped being so. It was the only method on this seam with no ctx
	// and the only one that reaches the network, and both /health and
	// /ready spelled the discard as `_ *http.Request` while calling it.
	Snapshot(ctx context.Context) RuntimeState

	// Tools is the tool catalogue this node serves, for the dashboard's
	// tool screen.
	//
	// A SECOND METHOD rather than a field on RuntimeState, because
	// Snapshot is called on every health tick and this is the one answer
	// that is expensive to build — a company's catalogue is hundreds of
	// entries once its MCP servers are up, and rebuilding it every few
	// seconds to throw it away is work nobody asked for.
	//
	// A nil slice is "this node serves none", which is a real claim: a
	// node with no active revision has no catalogue.
	Tools() []ToolInfo
}

// ToolInfo is one catalogue entry on the wire.
//
// The field names are the CLIENT's — name, description, source — because the
// dashboard is the compatibility reference for a frame's shape and it
// groups the tool screen by `source`.
type ToolInfo struct {
	Name        string `json:"name"`
	Description string `json:"description"`

	// Source is "builtin" or the MCP server that serves it, which is
	// exactly the grouping the tool screen renders.
	Source string `json:"source"`
}
