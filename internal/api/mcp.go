package api

import (
	"github.com/crewlet/crewlet/internal/api/httpjson"
	"github.com/crewlet/crewlet/internal/api/mcpbridge"
	"github.com/crewlet/crewlet/internal/api/operator"
)

// The MCP bridge edge.
//
// A seat's executor (a harness run in a box) calls the seat's tools HERE, so
// its tokens never enter a box running generated code (see mcpbridge).
//
// Reachable without the API's own auth, deliberately: the box holds no API
// token, and one would hand a sandbox the credential that reads the company.
// The per-run, expiring token in the PATH authenticates (the auth package
// exempts the prefix). A session lives in the process holding the seat, which
// may serve no API, so a call is relayed over the broker
// ([mcpbridge.Bridge.Join]). A node without the ingress role serves the route
// on api.port to this host's loopback alone ([Probes]).

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
	// AND EACH OF THE SEAT'S MCP SERVERS beneath it, under its own name and
	// behind the same token and the same routing (internal/api/mcpbridge,
	// servers.go): a harness keeps a server's tools out of its requests by
	// the server, so the engine's own tools and the seat's are served apart.
	mux.Handle(mcpbridge.ServerPattern, bridge.Handler())
	// AND THE RUN'S NOTES ROUTE beneath it, behind the same token and the
	// same routing: a harness's plugin polls it for its turn's notes
	// between steps (internal/api/mcpbridge, steer.go). Under the bridge's
	// prefix, so the exemption, the public listener, the drain and the access
	// log's redaction that cover the bridge cover it.
	mux.Handle(mcpbridge.SteerPattern, bridge.SteerHandler())
	// AND ITS SKILL ROUTE, the same way: a harness's plugin reports each of
	// the run's own skills it loads there (internal/api/mcpbridge, skills.go).
	mux.Handle(mcpbridge.SkillPattern, bridge.SkillHandler())
	// AND ITS BROWSER ROUTES, the same way: the relay in a box whose browser
	// is ready posts its screen there and polls for whether anybody watches
	// (internal/api/mcpbridge, browser.go). A session with no browser answers
	// every verb 404, so on the probe surface, whose direct boxes never run a
	// browser, they are routes nothing calls.
	mux.Handle(mcpbridge.BrowserPattern, bridge.BrowserHandler())
	log.Info("mcp_bridge_mounted", "path", mcpbridge.PathPrefix+"{token}",
		"servers", mcpbridge.ServerPattern, "notes", mcpbridge.SteerPattern,
		"skills", mcpbridge.SkillPattern, "browser", mcpbridge.BrowserPattern)
}

// mountOperator registers the operator surface's two transports — MCP for an
// operator's own assistant and the act route for a person at the dashboard —
// over the one dispatch.
//
// A nil server is a surface this API serves none of — a suite about something
// else — and both routes are then ABSENT. Otherwise each is mounted ONCE and
// every request reads the catalogue this node serves at that moment: a node
// that has not met its company answers `503 no_active_revision`, and every
// other is served the catalogue it finds, which the seat controls keep from
// ever being empty. The lines logged here therefore name the paths and no
// tool.
func (a *App) mountOperator(mux httpjson.Router, server *operator.Server) {
	if server == nil {
		return
	}
	// POST ONLY, unlike the bridge, which takes every verb: this transport
	// keeps no session, so it offers no server-to-client stream for a GET to
	// open and no session for a DELETE to end, and the mux's own 405 naming
	// POST is how streamable HTTP says so — see operator.MCPPattern. A
	// listening client's stream is a POST of its own.
	mux.Handle(operator.MCPPattern, server.MCPHandler())
	log.Info("operator_mcp_mounted", "path", operator.MCPPath,
		"detail", "an operator's own AI assistant can read and write "+
			"Worklet, Doclet and Chatlet here, authenticated with "+
			"an api.auth.tokens entry or a person's own credential (chat "+
			"only as a person, or a credential bound to a seat); the tools "+
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
