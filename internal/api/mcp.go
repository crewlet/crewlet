package api

import (
	"net/http"

	"github.com/crewlet/crewlet/internal/api/mcpbridge"
	"github.com/crewlet/crewlet/internal/api/opsmcp"
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

// mountBridge registers the bridge, or says why it did not.
//
// A nil bridge is an ordinary configuration — most deployments run no agent
// mode — and the route is then ABSENT rather than answering 503: an endpoint
// that exists and refuses everything reads to an operator as broken, while one
// that is not there matches what the config says.
func (a *App) mountBridge(mux *http.ServeMux, bridge *mcpbridge.Bridge) {
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

// mountOperator registers the operator MCP surface, or says why it did not.
//
// A nil server is an ordinary configuration — a company on Jira and
// Confluence has no native record for this to manage — and the route is then
// ABSENT rather than answering 404 from a registered handler: an endpoint
// that exists and lists no tools reads to an operator as broken, while one
// that is not there matches what their config says.
func (a *App) mountOperator(mux *http.ServeMux, server *opsmcp.Server) {
	if server == nil {
		return
	}
	// EVERY METHOD, for the reason the bridge takes every method: streamable
	// HTTP is a GET for the server-to-client stream and a DELETE to end a
	// session.
	mux.Handle(opsmcp.Path, server.Handler())
	log.Info("operator_mcp_mounted", "path", opsmcp.Path,
		"tools", server.Tools(),
		"detail", "an operator's own AI assistant can read and write the "+
			"company's tracker and knowledge base here, authenticated with "+
			"an api.auth.tokens entry")
}
