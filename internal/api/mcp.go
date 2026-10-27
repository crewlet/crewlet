package api

import (
	"github.com/crewlet/crewlet/internal/api/httpjson"
	"github.com/crewlet/crewlet/internal/api/mcpbridge"
	"github.com/crewlet/crewlet/internal/api/operator"
)

// The MCP bridge edge.
//
// A seat's executor — its coding CLI's own run, in a box — calls the seat's
// tools HERE rather than holding them, so the seat's credentials — its chat token, its tracker token, its
// code-host token — never enter a box running generated code. See
// internal/api/mcpbridge for what the bridge does with a call once it arrives.
//
// # This route is deliberately reachable without the API's own auth
//
// It has to be: the MCP client inside the box holds no API token, and giving
// it one would be handing a sandbox the credential that reads the whole
// company. What authenticates a request instead is the per-run, expiring token
// in its own PATH, and the auth package exempts this prefix for exactly that
// reason. Two gates stand behind it: the signature says the token was minted
// by this fleet, and the session map says the run it names is still going.
//
// # It is ingress's, and answers for every node
//
// A session is a live tool surface in the process holding the run's seat, and
// that process may serve no API at all. So the route is served where the API
// is, and the node a call reaches relays it to the seat's holder over the
// broker (see [mcpbridge.Bridge.Join]): one address for the fleet, whichever
// node holds the seat.

// mountBridge registers the bridge, or says why it did not.
//
// A nil bridge is an ordinary configuration — a node that names no
// [mcpbridge.BaseURLVar] and has no direct box of its own to serve builds
// none, and every executor launch that needs it is refused naming the
// variable — and the route is then ABSENT rather than answering 503: an
// endpoint that exists and refuses everything reads to an operator as broken,
// while one that is not there matches what the config says.
func mountBridge(mux httpjson.Router, bridge *mcpbridge.Bridge) {
	if bridge == nil {
		return
	}
	// EVERY METHOD, not just POST. Streamable HTTP is a GET for the
	// server-to-client stream and a DELETE to end a session, and a pattern
	// naming one verb answers 405 to the other two — which an MCP client
	// reports as a transport that does not support streaming rather than as
	// a route that is registered wrong.
	mux.Handle(mcpbridge.PathPrefix+"{token}", bridge.Handler())
	// AND THE RUN'S NOTES ROUTE beneath it, behind the same token and the
	// same routing: a coding CLI's plugin polls it for its turn's notes
	// between steps (internal/api/mcpbridge, steer.go). Under the bridge's
	// prefix, so the exemption, the public listener, the drain and the access
	// log's redaction that cover the bridge cover it.
	mux.Handle(mcpbridge.SteerPattern, bridge.SteerHandler())
	log.Info("mcp_bridge_mounted", "path", mcpbridge.PathPrefix+"{token}",
		"notes", mcpbridge.SteerPattern)
}

// mountOperator registers the operator surface's two transports — MCP for an
// operator's own assistant and the act route for a person at the dashboard —
// over the one dispatch.
//
// A nil server is a surface this API serves none of — a suite about something
// else — and both routes are then ABSENT. Otherwise each is mounted ONCE and
// every request reads the catalogue this node serves at that moment: a node
// that has not met its company answers `503 no_active_revision`, and one whose
// catalogue is empty answers as the route's absence would — an endpoint that
// exists and lists no tools reads to an operator as broken.
func (a *App) mountOperator(mux httpjson.Router, server *operator.Server) {
	if server == nil {
		return
	}
	// EVERY METHOD, so the transport's own verbs reach the SDK: this surface
	// is served statelessly and the SDK answers a GET for a stream and a
	// DELETE for a session with its own `405 Allow: POST`, which a client
	// reads as "no stream offered" — a mux 405 would read as a route
	// registered wrong.
	mux.Handle(operator.MCPPath, server.MCPHandler())
	log.Info("operator_mcp_mounted", "path", operator.MCPPath,
		"detail", "an operator's own AI assistant can read and write "+
			"Worklet and Doclet here, authenticated with "+
			"an api.auth.tokens entry or a person's own credential; the tools "+
			"it lists are the ones this node serves when it asks")
	// AND THE PERSON'S OWN TRANSPORT over the same catalogue: POST only, one
	// tool per request, as the principal the request resolved to
	// (ADR-0024). Guarded like every route the exemption list does not
	// name, and refused by the drain gate like every other write.
	mux.Handle(operator.ActPattern, server.ActHandler())
	log.Info("operator_act_mounted", "path", operator.ActPathPrefix+"{tool}",
		"detail", "the dashboard writes here as the principal its session "+
			"or token resolves to, one catalogue tool per request, under the "+
			"Idempotency-Key it names")
}
