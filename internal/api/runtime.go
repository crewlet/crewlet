// Package api serves the REST surface and the dashboard.
//
// ONE WIRING for the embedded and standalone deployments. What differs between
// them is not how the app is assembled but what it can SEE: a node running the
// engine in the same process can answer how many turns are in flight and which
// seats it holds, and a standalone API cannot. That difference is one seam —
// [NodeRuntime] — and everything else is identical, so a route cannot behave
// differently depending on which process it happens to be in.
package api

import (
	"github.com/crewlet/crewlet/internal/logging"
)

var log = logging.Get("api")

// RuntimeState is what only a co-located engine can answer.
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

	// StartedAt is when the ENGINE started, which on the standalone
	// deployment is a different process on a different clock from the
	// API's own start. Kept separate for that reason: one merged uptime
	// would be the two-different-windows error in a new place.
	StartedAt string

	// Seats are the handles this node is serving. The first question about
	// any fleet, and one previously answerable only by reading three
	// processes' logs at debug level.
	Seats []string

	// RoutedSources names the integrations whose deliveries can actually
	// wake a seat — the ones with a parser, not the ones with a config
	// block.
	//
	// Nil means "cannot say", never "none route": a standalone API has no
	// engine to ask, and a co-located one mid-boot has not started
	// notifications yet. The two are the same answer for a reader, and
	// both are the opposite of an empty slice, which is a real claim that
	// nothing routes.
	RoutedSources []string
}

// NodeRuntime is the seam for facts only a co-located engine can answer.
//
// Nil on a standalone API, and that is a real answer rather than a missing
// one: the health surface reports engine=false and OMITS the fields it cannot
// know, so a client can tell "nothing is running" from "this process cannot
// know". Without the distinction a dashboard renders a confident zero for both.
type NodeRuntime interface {
	Snapshot() RuntimeState
}
