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
// So every dependency the engine supplies is REQUIRED, and [New] refuses a
// missing one by name. A nil here is a wiring mistake, and an answer built
// around it (an omitted field, a 503, an unregistered route) would read to an
// operator as a deliberate one, hiding the mistake at the one place it could
// have been caught.
//
// [NodeRuntime] is a seam for one reason: it declares, in the consumer, the
// facts this package asks the engine for, so a route can be tested against
// fixed answers without standing a node up.
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
	// serve, wait, shed, isolated or stuck. It is WHY a node left rotation
	// when it was not draining, and the two call for opposite responses:
	// "draining" is an operator's own stop, "cannot apply epoch 41" is a
	// fault to go and read.
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

	// Domains are the state-log domains THIS NODE runs, which is derived
	// from node.roles and is not the same on every member of a fleet.
	//
	// ON THE PROBE BECAUSE IT IS NOT VISIBLE ANYWHERE ELSE. An operator
	// who narrows a node's roles narrows what it applies, and the only
	// other symptom is a peer's board answering a question this node's
	// cannot — which reads as a bug in the node rather than as the
	// declaration it is. It is also what an operator checks after ADDING a
	// role: the domain appears here once the node is actually applying it.
	Domains []string

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

	// ShuttingDown reports whether this node has begun to drain: the same
	// fact as Snapshot's ShuttingDown, read from the same place.
	//
	// A SECOND WAY TO READ IT because the drain gate asks on every request
	// that would start work, and Snapshot reaches the coordination plane on
	// every call. A write refused for draining must not first wait on a
	// network read that has nothing to do with the answer.
	ShuttingDown() bool

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

	// Source is the registry's ORIGIN GRAMMAR verbatim — `builtin`, or
	// `mcp:` and the bare server name — which is what the tool screen
	// groups on and what the API reference documents.
	//
	// The prefix is load-bearing and was once stripped here: a reader
	// cannot tell a server called `builtin` from the engine's own tools
	// without it, and every consumer that tests for it — the screen's
	// origin column, its per-origin counts — silently read a company
	// running MCP servers as one running none.
	Source string `json:"source"`

	// Annotations are the behavioural hints recorded at REGISTRATION —
	// what the engine's own delivery fence, the operator MCP surface and
	// the sandbox bridge all read, and what the tool screen could not show
	// because the catalogue on the wire carried three strings.
	//
	// It is the difference between a list of names and a surface an
	// operator can audit: which of a fresh server's tools can write, which
	// can write IRREVERSIBLY, and which reach outside the company at all.
	Annotations ToolAnnotations `json:"annotations"`

	// Delivers names WHERE calling this tool puts something in front of
	// somebody outside the turn, and is empty for a tool that reaches
	// nobody. The registry's own predicate, never re-derived here: a tool
	// that delivers through the fence and not on the screen is the drift
	// this field exists to make visible.
	Delivers string `json:"delivers"`

	// InputSchema is the tool's JSON Schema, exactly as the model is
	// offered it. Carried whole rather than summarised, because the screen
	// that renders "how would I call this" substitutes an object's ids
	// into the schema's OWN field names — a summary would be a second,
	// drifting description of the one thing the engine already states.
	InputSchema map[string]any `json:"input_schema,omitempty"`
}

// ToolAnnotations is one tool's behavioural hints on the wire.
//
// EVERY HINT IS THREE-VALUED and rendered as a word, never as a bool. "The
// server did not advertise this" and "the server said no" are different facts
// — the whole reason [mcp.Hint] exists — and a JSON bool cannot hold the
// difference: an absent hint would arrive as `false` and read as a positive
// denial, which is exactly how a fresh MCP server's unannotated tools would
// come to look like proven reads on the one screen an operator audits them on.
//
// Words rather than `true | false | null` for the client's sake: all three
// values are truthy, so a careless `if (!ann.read_only)` cannot silently mean
// "not read-only" for a tool nobody annotated.
type ToolAnnotations struct {
	// Title is the human-readable name a server advertised, empty when it
	// advertised none.
	Title string `json:"title,omitempty"`

	ReadOnly    string `json:"read_only"`
	Destructive string `json:"destructive"`
	Idempotent  string `json:"idempotent"`
	OpenWorld   string `json:"open_world"`
}
