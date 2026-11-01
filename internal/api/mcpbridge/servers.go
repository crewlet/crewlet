package mcpbridge

import (
	"net/url"
	"slices"
	"strings"

	"github.com/crewlet/crewlet/internal/tools"
	"github.com/crewlet/crewlet/internal/toolsession"
)

// EACH OF A SEAT'S MCP SERVERS UNDER ITS OWN NAME.
//
// An opening serves the engine's own tools — the seat's colleagues, its work,
// its knowledge, and the calls a run reports through and asks a person through
// — as one MCP server, [serverName], at the job's endpoint; and every MCP server
// a tool its surface offers comes from as a server of its own beneath it
// ([ServerPattern]), named as the company names it and listing that server's
// tools alone, with what the company says the server is for as its MCP
// instructions. Every call on every one of them is the opening's: decided by
// the same surface, fenced, ordered and recorded exactly as a call to
// [serverName] is ([binding.Call]).
//
// WHY APART. A harness keeps a large tool list out of each request by SERVER —
// Claude Code defers every MCP server's tools behind its tool search unless the
// server is marked always loaded, and OpenCode offers a server's tools through
// Code Mode unless the server says otherwise — so the engine's own tools, which
// a run needs on every request, and a seat's hundred vendor tools, which it
// needs a few of, have to be two kinds of server for the harness to tell them
// apart ([sandbox.MCPServer.AlwaysLoaded]). One server holding both left a
// choice between sending every definition on every request and making a run
// search for the call that ends it, and the engine made its own: it offered
// the vendor tools only after `list_mcp_server_tools` and `activate_tool`,
// two rounds a harness that defers tools natively never needed. Named as the
// company names it, a server is also the namespace the harness shows its
// model — `mcp__github__create_issue`, `tools.github.create_issue` — rather
// than one flat list under the engine's name.
//
// THE SET IS FIXED AT THE OPENING, from what the surface offers then
// ([Servers]), because the box's configuration is: a run is handed one URL per
// server when it starts and dials no other. A tool that comes onto the surface
// later from a server with no endpoint of its own is served as one of the
// engine's own, so nothing the surface offers is out of the run's reach.

// ServerPattern is the mux pattern of one of an opening's servers, beneath the
// job's endpoint: under the bridge's prefix, so every gate that covers the
// bridge — the exemption from the API's credential guard, the public listener,
// serving under drain, the access log's redaction of the token — covers it by
// prefix, as it covers the notes route ([SteerPattern]).
const ServerPattern = PathPrefix + "{token}/server/{server}"

// serverSegment joins a job's endpoint to one of its servers.
const serverSegment = "/server/"

// ServerURL is where the bridge serves one of a job's servers, beneath the
// job's endpoint — what the engine hands the run's harness under that server's
// name — or "" for a job with no endpoint.
func ServerURL(endpoint, server string) string {
	if endpoint == "" || server == "" {
		return ""
	}
	return endpoint + serverSegment + url.PathEscape(server)
}

// Servers names the MCP servers an opening of surface serves under their own
// names, sorted: the server of every tool the surface offers that an MCP server
// serves.
//
// ONE RULE FOR BOTH ENDS: the engine reads it to hand the run's harness a URL
// per server ([ServerURL]), and an opening to decide which servers it serves,
// so the box and the bridge cannot disagree about one.
func Servers(surface *tools.Surface) []string {
	if surface == nil {
		return nil
	}
	universe := surface.Universe()
	var out []string
	for _, name := range surface.Active() {
		entry, ok := universe.Lookup(name)
		if !ok {
			continue
		}
		if server, fromMCP := entry.FromMCP(); fromMCP && !slices.Contains(out, server) {
			out = append(out, server)
		}
	}
	slices.Sort(out)
	return out
}

// endpoint is one MCP server an opening serves: the engine's own tools under
// [serverName] (name ""), or one of the seat's MCP servers under its own name.
type endpoint struct {
	opening *binding
	name    string
	server  *toolsession.Server
}

var _ toolsession.Dispatcher = (*endpoint)(nil)

// Offered is what the surface offers now of this endpoint's tools.
func (e *endpoint) Offered() []toolsession.Tool {
	all := e.opening.Offered()
	out := make([]toolsession.Tool, 0, len(all))
	for _, tool := range all {
		if e.opening.endpointOf(tool.Def.Name) == e {
			out = append(out, tool)
		}
	}
	return out
}

// Revision is the surface's: whatever moves what the surface offers may move
// what this endpoint offers.
func (e *endpoint) Revision() uint64 { return e.opening.Revision() }

// Call is the opening's, through whichever endpoint the harness reached the tool
// at. The endpoint the call came through brings its own list up to what the
// surface offers once it is answered ([toolsession.Server]); the opening's
// other endpoints are brought up here, since a call on one can move what
// another offers.
func (e *endpoint) Call(in toolsession.Call) toolsession.Result {
	res := e.opening.Call(in)
	for _, other := range e.opening.order {
		if other != e {
			other.server.Sync()
		}
	}
	return res
}

// listedName is the name the endpoint's server is listed under.
func (e *endpoint) listedName() string {
	if e.name == "" {
		return serverName
	}
	return e.name
}

// endpointOf is the endpoint that serves tool: its MCP server's, where the
// opening serves that server under its own name, and the engine's own
// otherwise.
func (o *binding) endpointOf(tool string) *endpoint {
	if entry, ok := o.session.Surface.Universe().Lookup(tool); ok {
		if server, fromMCP := entry.FromMCP(); fromMCP {
			if ep, served := o.servers[server]; served {
				return ep
			}
		}
	}
	return o.servers[""]
}

// endpoint is the endpoint a request's route names — the engine's own for the
// job's endpoint itself, where server is "" — or nil for a server the opening
// does not serve.
func (o *binding) endpoint(server string) *endpoint { return o.servers[server] }

// instructionsOf is what a server is for, as the company says it on the
// server's mcp_servers entry, which every tool the server serves carries: the
// server's own MCP instructions on its endpoint.
func instructionsOf(surface *tools.Surface, server string) string {
	for _, entry := range surface.Universe().Entries() {
		if s, fromMCP := entry.FromMCP(); fromMCP && s == server {
			if description := strings.TrimSpace(entry.ServerDescription); description != "" {
				return description
			}
		}
	}
	return ""
}
