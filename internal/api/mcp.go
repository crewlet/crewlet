package api

import (
	"net/http"

	"github.com/crewlet/crewlet/internal/api/auth"
	"github.com/crewlet/crewlet/internal/api/httpjson"
	"github.com/crewlet/crewlet/internal/api/mcpbridge"
	"github.com/crewlet/crewlet/internal/api/operator"
	"github.com/crewlet/crewlet/internal/api/pagepolicy"
	"github.com/crewlet/crewlet/internal/config"
)

// The MCP bridge edge.
//
// A coding agent in agent mode calls its seat's tools HERE rather than holding
// them, so the seat's credentials — its chat token, its tracker token, its
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
// # It belongs to the node that runs the seat, not to ingress
//
// A session is a live tool surface in the process that claimed the seat, so the
// only node that can answer a box is the one that opened the run's session (see
// [mcpbridge.Bridge.Mounted]). Every other route here is the ingress role's:
// webhooks, the dashboard and the REST surface can be served by any peer. That
// is why a node whose roles leave out ingress still serves this route, beside
// its probes and nothing else, through [Probes].

// mountBridge registers the bridge, or says why it did not.
//
// A nil bridge is an ordinary configuration — most deployments run no agent
// mode — and the route is then ABSENT rather than answering 503: an endpoint
// that exists and refuses everything reads to an operator as broken, while one
// that is not there matches what the config says.
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
	log.Info("mcp_bridge_mounted", "path", mcpbridge.PathPrefix+"{token}")
}

// BridgeOnly is the HTTP handler a node that runs seats without the ingress role
// binds on api.public: the tool bridge and no other route. Without api.public
// the bridge rides api.port beside the node's probes instead (see [Probes]), so
// the bridge is served where the file says public routes are served, on every
// node.
//
// It is wrapped in the same guard and security headers as the full [App], so a
// request for any other path is refused or answered 404 exactly as the full
// surface would answer an unknown path, never served by a bare mux. The bridge
// route itself is exempt from the guard by prefix, as it is on the full
// surface.
//
// A nil bridge returns nil: there is nothing for such a node to serve, and the
// caller binds no listener rather than one that answers every request with a
// refusal.
func BridgeOnly(bootstrap *config.Bootstrap, bridge *mcpbridge.Bridge) http.Handler {
	if bridge == nil {
		return nil
	}
	mux := http.NewServeMux()
	mountBridge(mux, bridge)
	return pagepolicy.Apply(auth.New(bootstrap).Middleware(httpjson.Mux(mux)))
}

// mountOperator registers the operator surface's two transports, each serving
// the catalogue the surface answers at the request — see [OperatorSurface].
//
// MOUNTED ON EVERY APP, from the one fact known at assembly — an engine is
// beside it — rather than from a catalogue built then: the tools a node serves
// appear with its native runtime, which can be built after this runs. The
// lines logged here therefore name the paths and no tool; the catalogue logs
// its tools where it is built (`operator_surface_built`).
func (a *App) mountOperator(mux httpjson.Router, surface OperatorSurface) {
	// POST ONLY, unlike the bridge, which takes every verb: this transport
	// keeps no session, so it offers no server-to-client stream for a GET to
	// open and no session for a DELETE to end, and the mux's own 405 naming
	// POST is how streamable HTTP says so — see operator.MCPPattern.
	mux.Handle(operator.MCPPattern, operatorRoute(surface, (*operator.Server).MCPHandler))
	log.Info("operator_mcp_mounted", "path", operator.MCPPath,
		"detail", "an operator's own AI assistant can read and write the "+
			"company's tracker and knowledge base here, authenticated with "+
			"an api.auth.tokens entry")
	// AND THE PERSON'S OWN TRANSPORT over the same catalogue: POST only,
	// one tool per request, admitting a token bound to a seat and nobody
	// else (ADR-0024). Under the same always-guarded prefix, so the guard in
	// front of it is the one in front of /operator/mcp.
	mux.Handle(operator.ActPattern, operatorRoute(surface, (*operator.Server).ActHandler))
	log.Info("operator_act_mounted", "path", operator.ActPathPrefix+"{tool}",
		"detail", "the dashboard writes here as the person the presented "+
			"token is bound to by contact.crewlet_operator_id")
}

// operatorRoute serves one transport of the catalogue surface answers at this
// request, or the mux's own 404 where it answers none.
//
// ONE RESOLUTION, AT THE TOP, so the request is served by one catalogue from
// its first byte to its last whatever the surface answers meanwhile; and the
// 404 is byte for byte the one a route this node does not serve answers
// ([httpjson.NoRoute]), since to the caller it is the same fact.
func operatorRoute(surface OperatorSurface,
	transport func(*operator.Server) http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		server, ok := surface.Operator()
		if !ok {
			httpjson.NoRoute(w, r)
			return
		}
		transport(server).ServeHTTP(w, r)
	})
}
