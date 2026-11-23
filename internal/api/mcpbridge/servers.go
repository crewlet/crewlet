package mcpbridge

import (
	"net/url"
	"slices"
	"strings"

	"github.com/crewlet/crewlet/internal/sandbox"
	"github.com/crewlet/crewlet/internal/tools"
	"github.com/crewlet/crewlet/internal/toolsession"
)

// EACH OF A SEAT'S MCP SERVERS UNDER ITS OWN NAME. An opening serves the
// engine's own tools as one server, [serverName], at the job's endpoint, and
// each MCP server a tool on its surface comes from as its own server beneath it
// ([ServerPattern]): named as the company names it (the namespace its model
// sees), listing its tools alone, the company's description as its
// instructions. Every call on any of them is the opening's ([binding.Call]).
// Apart, because a harness defers tools by SERVER (Claude Code's tool search,
// OpenCode's Code Mode): the engine's tools are needed on every request, a
// seat's vendor tools a few at a time ([sandbox.MCPServer.AlwaysLoaded]). The
// set is fixed at the opening ([Servers]): a run is handed one URL per server
// and dials no other, so a tool arriving later from an unserved server is
// served as one of the engine's own.

// ServerPattern is the mux pattern of one of an opening's servers, beneath the
// job's endpoint: under the bridge's prefix, so every gate covering the bridge
// (the credential guard's exemption, the public listener, serving under drain,
// the access log's token redaction) covers it, as it covers [SteerPattern].
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
// names, sorted: the server of every tool a seat's surface offers that an MCP
// server serves. A surface that is not a seat's own [*tools.Surface] names
// none, and its opening serves every tool as the engine's own.
//
// ONE RULE FOR BOTH ENDS: the engine hands the run's harness a URL per server
// from it ([ServerURL]) and an opening serves its servers from it, so the box
// and the bridge cannot disagree.
func Servers(surface Surface) []string {
	seat := seatOf(surface)
	if seat == nil {
		return nil
	}
	universe := seat.Universe()
	var out []string
	for _, name := range seat.Active() {
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

// Guidance is what each MCP server an opening of surface serves under its own
// name says it is for, by that name ([Servers]): the instructions the server's
// endpoint answers ([instructionsOf]), which a run's harness may hand its
// model again in the box that holds its session, so a launch charges them.
// A server that says nothing is left out; nil where none says anything.
func Guidance(surface Surface) map[string]string {
	seat := seatOf(surface)
	var out map[string]string
	for _, name := range Servers(surface) {
		if said := instructionsOf(seat, name); said != "" {
			if out == nil {
				out = map[string]string{}
			}
			out[name] = said
		}
	}
	return out
}

// Catalogs is the tools each MCP server an opening of surface serves under its
// own name offers now, by that name ([Servers]), as the surface offers them:
// what a run's harness may list its model again in the box that holds its
// session (OpenCode's Code Mode catalog), so a launch charges them. Nil where
// no such server offers a tool.
func Catalogs(surface Surface) map[string][]sandbox.CatalogTool {
	seat := seatOf(surface)
	if seat == nil {
		return nil
	}
	universe := seat.Universe()
	var out map[string][]sandbox.CatalogTool
	for _, def := range seat.ToolDefs() {
		entry, ok := universe.Lookup(def.Name)
		if !ok {
			continue
		}
		server, fromMCP := entry.FromMCP()
		if !fromMCP {
			continue
		}
		if out == nil {
			out = map[string][]sandbox.CatalogTool{}
		}
		out[server] = append(out[server], sandbox.CatalogTool{Name: def.Name, Description: def.Description,
			Schema: def.Parameters})
	}
	return out
}

// Listed is how many tools an opening of the session lists its run's harness
// now, across its servers, the engine's own and each MCP server's: what a
// launch weighs a harness that hides every MCP tool past a count of them
// against (internal/sandbox's Opening.Tools). Counted by the rule each server
// lists by ([Session.lists]), so the count and the lists cannot disagree.
func (s *Session) Listed() int {
	n := 0
	for _, def := range s.Surface.ToolDefs() {
		if s.lists(def.Name) {
			n++
		}
	}
	return n
}

// seatOf is surface where it is a seat's, or nil: the one surface a seat's
// MCP servers reach an opening through.
func seatOf(surface Surface) *tools.Surface {
	seat, _ := surface.(*tools.Surface)
	return seat
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

// Call is the opening's, through whichever endpoint the harness reached. The
// calling endpoint syncs its own list once answered ([toolsession.Server]); the
// others are synced here, since a call on one can move what another offers.
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
	if seat := o.session.seat(); seat != nil {
		if entry, ok := seat.Universe().Lookup(tool); ok {
			if server, fromMCP := entry.FromMCP(); fromMCP {
				if ep, served := o.servers[server]; served {
					return ep
				}
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
	if surface == nil {
		return ""
	}
	for _, entry := range surface.Universe().Entries() {
		if s, fromMCP := entry.FromMCP(); fromMCP && s == server {
			if description := strings.TrimSpace(entry.ServerDescription); description != "" {
				return description
			}
		}
	}
	return ""
}
