// Package api serves the REST surface and the dashboard.
//
// # There is one shape: the API is served beside the engine
//
// `crewlet run` is the only thing that builds an [App], and it builds one inside
// the engine's own process, over the engine's own store, broker and
// coordination plane. What `node.roles` changes is what that engine does (an
// ingress-only node claims no seats and runs no worker duties), never whether
// it exists, and a node with `api.port: 0` builds no App at all. A node
// without the ingress role builds none either: it serves its two probes
// alone, through [Probes], answered by the same engine runtime. There is no
// API process without an engine.
//
// So every dependency the engine supplies is REQUIRED, and [New] refuses a
// missing one by name. A nil here is a wiring mistake, and an answer built
// around it (an omitted field, a 503, an unregistered route) would read to an
// operator as a deliberate one, hiding the mistake at the one place it could
// have been caught.
//
// [NodeRuntime] is a seam for one reason: it declares, in the consumer, the
// facts this package asks the engine for, so a route can be tested against
// fixed answers without standing a node up.
//
// # One route table, behind one chain, on one or two listeners
//
// Every surface mounts on ONE table ([routeTable]), and every route there
// states the authority it is decided by where it is mounted — a grant through
// internal/authz, or its place on internal/api/auth's exemption list. With
// Tier A's `api.public` set, the routes outside parties call ([auth.Public]:
// the webhooks, the vendor app landings and the sandbox endpoints) are served
// on a listener of their own ([App.Public]) and api.port serves the rest — the
// same table and the same chain behind a partition, so a route is guarded,
// cross-site checked and drained exactly once whichever socket answers it.
package api

import (
	"context"
	"time"

	"github.com/crewlet/crewlet/internal/api/queries"
	"github.com/crewlet/crewlet/internal/logging"
	"github.com/crewlet/crewlet/internal/statelog"
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
	// serve, wait, shed, isolated or stuck. It is WHY a node left rotation
	// when it was not draining, and the two call for opposite responses:
	// "draining" is an operator's own stop, "cannot apply epoch 41" is a
	// fault to go and read.
	Posture string

	// AppliedEpoch is the config revision this node is running.
	AppliedEpoch int64

	// ConfigAttempt is this node's last concluded apply attempt, nil before
	// this process has concluded one. See [ConfigAttempt].
	ConfigAttempt *ConfigAttempt

	// StallLag is how far behind the node's watched duty is: the time
	// since its last beat, once it has missed one, and the early warning
	// before the watchdog ends the process at the lease TTL. Zero is
	// "nothing to report" (the duty is on its beat) rather than "measured
	// as zero", which is the same answer a health surface wants for both.
	StallLag time.Duration

	// StartedAt is when the engine started, which is this node's start:
	// the API is served inside the engine's process.
	StartedAt string

	// IdentityDuties are the identity estate's fleet-singleton duties THIS
	// NODE armed, each with the interval it runs at when it holds the
	// lease. Nil is "armed none", which is a real answer: a node running
	// no workers arms none — and a duty that was never armed looks from
	// every other vantage point exactly like one quietly finding nothing
	// to do.
	IdentityDuties map[string]time.Duration

	// Seats are the handles this node is serving. The first question about
	// any fleet, and one previously answerable only by reading three
	// processes' logs at debug level.
	Seats []string

	// Unproven is how long each seat whose teardown could not be proven
	// has been stranded: still leased by this node, so no peer can claim
	// it, while this node will not run it either.
	//
	// The DURATION is the alarm, not the membership. A release that fails
	// once and succeeds on the next heartbeat is a working system, and a
	// seat still here minutes later is a seat nothing in the fleet runs.
	// Empty is "none stranded".
	Unproven map[string]time.Duration

	// RoutedSources names the integrations whose deliveries can actually
	// wake a seat — the ones with a parser, not the ones with a config
	// block.
	//
	// Nil means "cannot say", never "none route": an engine mid-boot, or
	// one with no active revision, has not started notifications yet.
	// That is the opposite of an empty slice, which is a real claim that
	// nothing routes.
	RoutedSources []string

	// DirectUpload is whether this node's object store hands clients
	// presigned puts: "available", "unavailable (<reason>)", or "" on a
	// node running no object store ([engine.Engine.DirectUploadStatus]).
	DirectUpload string

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

// FleetState is what the health ENVELOPE says about the fleet beyond this
// node: two counts, and nothing a probe decides on.
//
// NOT PART OF [RuntimeState], because /ready and every other Snapshot reader
// would then pay for it. The presence count is a key-iterating coordination
// scan, and a readiness probe that scans the fleet on every call to throw the
// answer away turns a wedged broker into a probe hang for a fact it never
// reads. Only the envelope asks, through [NodeRuntime.Fleet].
type FleetState struct {
	// LiveNodes is how many nodes hold a presence lease — the fleet's
	// size as this node's fan-outs see it. NIL IS A READ THAT DID NOT
	// HAPPEN, never zero: a health card told "0 nodes" by the node that
	// is serving it has been told something false, where "cannot say" is
	// what an unreachable coordination plane (or a read that outran its
	// budget) actually means.
	LiveNodes *int

	// Alarms are the kinds this node's latest alarm evaluation found
	// firing, the longest-standing first: the evaluation that set the
	// `crewlet.alarm.active` gauge and wrote the alarm log lines, never a
	// second one. ONE ENTRY PER KIND, however many logs a per-log kind
	// stands on, because the count is how many problems an operator has to
	// look at and one cause on several logs is one of them, as the gauge
	// beside it says ([statelog.Alarm.Domains]). Nil is "no evaluation has
	// run" (mid-boot, or a node running no state log), which is the
	// opposite of an empty slice: the real claim that nothing is firing.
	// Held in memory, so a presence read that did not finish never costs
	// the envelope its alarms.
	Alarms []statelog.Kind
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

	// ShuttingDown reports whether this node has begun to drain: the same
	// fact as Snapshot's ShuttingDown, read from the same place.
	//
	// A SECOND WAY TO READ IT because the drain gate asks on every request
	// that would start work, and Snapshot reaches the coordination plane on
	// every call. A write refused for draining must not first wait on a
	// network read that has nothing to do with the answer.
	ShuttingDown() bool

	// Seats is the handles this node is serving: the same fact as
	// Snapshot's Seats, read from the same place.
	//
	// A SECOND WAY TO READ IT for ShuttingDown's reason. The seat placement
	// asks on every shared tick and every socket connect, and wants this
	// node's seats and nothing else — while Snapshot reaches the
	// coordination plane for the config posture on every call, which the
	// placement read paid for and threw away: a second listing of the
	// fleet's apply status on every tick, beside the health body's own,
	// and one more per tab that connected.
	Seats() []string

	// Fleet is the envelope's fleet counts. It reads the coordination
	// plane, and its caller BOUNDS it (engine.ProbeReadBudget): the
	// envelope is the liveness probe's body, and the counts are what it can
	// most afford to leave out. It must honour ctx, and answer what it has
	// (the alarms, which are in memory) when ctx ends before presence does.
	Fleet(ctx context.Context) FleetState

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
	Tools() []queries.ToolRow

	// Builtins is the engine's own tools as this node's seats are offered
	// them: the registry's built-ins and the discovery pair the runner
	// hands every phase that discovers, which no registry holds. The MCP
	// page's built-in catalogue, nil on a node with no active revision.
	//
	// NOT Tools filtered by origin, because that is the registry alone,
	// and a catalogue of what an agent holds that left out the two tools
	// every executor reaches its MCP servers through would be short by
	// exactly the pair a reader of the MCP page asks about.
	Builtins() []queries.ToolRow

	// Listings is the fleet's listings of its MCP servers' tools as this
	// node's watched copy holds them, and the schemas they name: what a
	// server's detail (`GET /mcp-servers/{name}`) is read from. Nil on a
	// node that keeps no copy, whose detail then answers unavailable.
	Listings() queries.MCPListingReader
}
