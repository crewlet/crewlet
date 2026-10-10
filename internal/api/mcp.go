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
// It has to be: the MCP client inside the box holds no API key, and giving
// it one would be handing a sandbox a credential that reads the company. What
// authenticates a request instead is the per-run, expiring token in its own
// PATH, which is why the route is mounted OPEN and the auth package names this
// prefix among the public ones. Two gates stand behind it: the signature says the token was minted
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
func mountBridge(mux auth.Router, bridge *mcpbridge.Bridge) {
	if bridge == nil {
		return
	}
	// EVERY METHOD, not just POST. Streamable HTTP is a GET for the
	// server-to-client stream and a DELETE to end a session, and a pattern
	// naming one verb answers 405 to the other two — which an MCP client
	// reports as a transport that does not support streaming rather than as
	// a route that is registered wrong.
	mux.Handle(mcpbridge.PathPrefix+"{token}", auth.ReachOpen, bridge.Handler())
	log.Info("mcp_bridge_mounted", "path", mcpbridge.PathPrefix+"{token}")
}

// BridgeOnly is the HTTP handler a node that runs seats without the ingress role
// binds on api.public: the tool bridge and no other route. Without api.public
// the bridge rides api.port beside the node's probes instead (see [Probes]), so
// the bridge is served where the file says public routes are served, on every
// node.
//
// It is mounted on the same kind of route table, behind the same guard and
// security headers, as the full [App], so a request for any other path is
// answered 404 exactly as the full surface would answer an unknown path, never
// served by a bare mux. The bridge route itself is open, as it is on the full
// surface.
//
// A nil bridge returns nil: there is nothing for such a node to serve, and the
// caller binds no listener rather than one that answers every request with a
// refusal.
func BridgeOnly(bootstrap *config.Bootstrap, bridge *mcpbridge.Bridge) http.Handler {
	if bridge == nil {
		return nil
	}
	guard := auth.New(bootstrap)
	mux := newRouteTable(guard, nil)
	mountBridge(mux, bridge)
	return pagepolicy.Apply(guard.Middleware(httpjson.Mux(mux.mux)))
}

// mountOperator registers the operator surface's transports, or says why it
// did not.
//
// A nil server is an ordinary configuration — a company on Jira and
// Confluence has no native record for this to manage — and the route is then
// ABSENT rather than answering 404 from a registered handler: an endpoint
// that exists and lists no tools reads to an operator as broken, while one
// that is not there matches what their config says.
func (a *App) mountOperator(mux auth.Router, server *operator.Server) {
	if server == nil {
		return
	}
	// EVERY METHOD, for the reason the bridge takes every method: streamable
	// HTTP is a GET for the server-to-client stream and a DELETE to end a
	// session.
	//
	// MEMBER REACH, because the catalogue is the company's own work and
	// pages — what a member reads and writes — and nothing of how the engine
	// runs. Who may write as whom is the handler's to decide past that: a
	// member key no seat links is refused there, since a member acts only
	// as the person the key is linked to (see [operator.Server.MCPHandler]).
	mux.Handle(operator.MCPPath, auth.ReachMember, server.MCPHandler())
	log.Info("operator_mcp_mounted", "path", operator.MCPPath,
		"tools", server.Tools(),
		"detail", "a person's own AI assistant can read and write the "+
			"company's tracker and knowledge base here, authenticated with "+
			"a key linked to their seat or an admin key")
	// AND THE PERSON'S OWN TRANSPORT over the same catalogue: POST only,
	// one tool per request, admitting a key linked to a seat and nobody
	// else (ADR-0024) — at the same reach as /operator/mcp.
	mux.Handle(operator.ActPattern, auth.ReachMember, server.ActHandler())
	log.Info("operator_act_mounted", "path", operator.ActPathPrefix+"{tool}",
		"tools", server.Acts(),
		"detail", "the dashboard writes here as the person the presented "+
			"key is linked to by contact.crewlet_operator_id")
}
