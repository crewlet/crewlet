package api

import (
	"net/http"

	"github.com/crewlet/crewlet/internal/api/auth"
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
// is why a node whose roles leave out ingress still serves this one route, and
// only this one, through [BridgeOnly].

// mountBridge registers the bridge, or says why it did not.
//
// A nil bridge is an ordinary configuration — most deployments run no agent
// mode — and the route is then ABSENT rather than answering 503: an endpoint
// that exists and refuses everything reads to an operator as broken, while one
// that is not there matches what the config says.
func mountBridge(mux *http.ServeMux, bridge *mcpbridge.Bridge) {
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

// BridgeOnly is the HTTP handler of a node that runs seats without the ingress
// role: the tool bridge and no other route.
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
	return pagepolicy.Apply(auth.New(bootstrap).Middleware(mux))
}

// mountOperator registers the operator surface's transports, or says why it
// did not.
//
// A nil server is an ordinary configuration — a company on Jira and
// Confluence has no native record for this to manage — and the route is then
// ABSENT rather than answering 404 from a registered handler: an endpoint
// that exists and lists no tools reads to an operator as broken, while one
// that is not there matches what their config says.
func (a *App) mountOperator(mux *http.ServeMux, server *operator.Server) {
	if server == nil {
		return
	}
	// EVERY METHOD, for the reason the bridge takes every method: streamable
	// HTTP is a GET for the server-to-client stream and a DELETE to end a
	// session.
	mux.Handle(operator.MCPPath, server.MCPHandler())
	log.Info("operator_mcp_mounted", "path", operator.MCPPath,
		"tools", server.Tools(),
		"detail", "an operator's own AI assistant can read and write the "+
			"company's tracker and knowledge base here, authenticated with "+
			"an api.auth.tokens entry")
}
