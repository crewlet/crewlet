package mcpbridge

import (
	"net/url"
	"slices"
	"strings"
	"sync"

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
// seat's vendor tools a few at a time (internal/sandbox's MCPServer.AlwaysLoaded). The
// set is fixed at the opening ([Servers]): a run is handed one URL per server
// and dials no other, so a tool arriving later from an unserved server is one
// of the engine's own, under an alias where its name needs one ([aliased]).

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

// Listed is what the engine's own server of an opening of s lists as it
// opens, by name, sorted: each tool the surface offers that no server of the
// opening's own serves ([Servers], [binding.endpointOf]), under its alias where
// it has one ([aliased]), by the rule each server lists by ([Session.lists]).
// A surface only adds tools ([tools.Surface.Activate]), so a run's harness
// lists every one of them for as long as the run lasts: what a runner holds
// the harness's offer to ([sandbox.MCPServer.Tools]).
func Listed(s *Session) []string {
	served := Servers(s.Surface)
	seat := seatOf(s.Surface)
	var out []string
	for _, def := range s.Surface.ToolDefs() {
		if !s.lists(def) {
			continue
		}
		if seat != nil {
			if entry, ok := seat.Universe().Lookup(def.Name); ok {
				if server, fromMCP := entry.FromMCP(); fromMCP && slices.Contains(served, server) {
					continue
				}
			}
		}
		out = append(out, def.Name)
	}
	// UNDER THE NAMES IT LISTS, an alias for one it cannot ([aliasesOf]).
	alias := aliasesOf(out)
	for i, name := range out {
		if a, ok := alias[name]; ok {
			out[i] = a
		}
	}
	slices.Sort(out)
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
		if s.lists(def) {
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

	// mu guards toolOf: the tool each alias the engine's own server last
	// listed names ([aliased]), which a call on the alias is made as.
	mu     sync.Mutex
	toolOf map[string]string
}

var _ toolsession.Dispatcher = (*endpoint)(nil)

// Offered is what the surface offers now of this endpoint's tools.
//
// THE ENGINE'S OWN SERVER LISTS ONLY A NAME NO OTHER SERVER CAN SPELL in a
// harness joining server and tool with `__` or `-`
// ([harnessprofile.BridgeToolName]): every built-in has one, and any other
// tool of its, one of a server the opening does not serve, is listed under
// its alias ([aliased]; the package doc, "A name `crewlet` cannot list").
func (e *endpoint) Offered() []toolsession.Tool {
	all := e.opening.Offered()
	out := make([]toolsession.Tool, 0, len(all))
	for _, tool := range all {
		if e.opening.endpointOf(tool.Def.Name) == e {
			out = append(out, tool)
		}
	}
	if e.name != "" {
		return out
	}
	out, toolOf := aliased(out)
	e.mu.Lock()
	e.toolOf = toolOf
	e.mu.Unlock()
	return out
}

// Revision is the surface's: whatever moves what the surface offers may move
// what this endpoint offers.
func (e *endpoint) Revision() uint64 { return e.opening.Revision() }

// Call is the opening's, through whichever endpoint the harness reached. The
// calling endpoint syncs its own list once answered ([toolsession.Server]); the
// others are synced here, since a call on one can move what another offers.
//
// A call on an alias is made as the tool it names, so the surface, the record
// and the seat's guards see the tool's own name.
func (e *endpoint) Call(in toolsession.Call) toolsession.Result {
	e.mu.Lock()
	if name, ok := e.toolOf[in.Name]; ok {
		in.Name = name
	}
	e.mu.Unlock()
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
