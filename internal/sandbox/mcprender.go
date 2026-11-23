package sandbox

import (
	"maps"
	"slices"
	"time"
)

// The scoped MCP surface the in-box harness gets: ONLY the servers the seat
// named in role.sandbox.mcp.servers, each with the seat's own credentials.
//
// SCOPING IS PER SERVER because neither supported harness offers a per-tool
// allowlist the engine could enforce (one has no such flag, the other runs
// with permissions bypassed so no prompt hangs a headless run): a curated tool
// list would be a claim, not a control.
//
// A pure SELECTION over servers already resolved and merged with the seat's
// credentials by the engine, beside its may-it-start rule: an optional key is
// left out of the MERGED values, so a seat value resolving to nothing never
// restores the default. Config file shape and key names are each runner's.

// MCPServer is the engine's view of one configured server.
//
// Declared here, not imported from config, so this package depends on the
// SHAPE rather than the config tree. Every `${VAR}` is already RESOLVED and
// the seat's credentials merged: it is written into the harness's config as
// is, and neither harness reads a reference as the engine does.
type MCPServer struct {
	Name      string
	Transport Transport

	// The stdio fields.
	Command string
	Args    []string
	Env     map[string]string

	// The http fields.
	URL     string
	Headers map[string]string

	// Secrets is what the references in the url resolved to (a secret kept
	// in a URL path, `https://mcp.example.com/s/${TOKEN}/mcp`). Never
	// rendered, since the url carries it, but read by the session-capture
	// scrub ([HandedSecrets]) beside env and headers, credentials whole.
	Secrets []string

	// AlwaysLoaded asks the harness to offer this server's tools on every
	// request rather than defer them: Claude Code's `alwaysLoad` (exempt from
	// its tool search), OpenCode's `codemode: false` (not behind Code Mode's
	// `execute`), Hermes's tool search turned off (it exempts no server).
	//
	// Set on THE ENGINE'S OWN TOOLS ALONE: the bridge's `crewlet` server
	// (submit_work, ask_and_wait, the seat's colleagues, work and knowledge).
	// Every other server is left to the harness's deferral, so a seat with 150
	// MCP tools sends their definitions only on requests that need them.
	AlwaysLoaded bool

	// CallTimeout is how long the harness waits on one call before cancelling
	// it: Claude Code's per-server `timeout`, OpenCode's `timeout.execution`.
	// Zero leaves the harness its own.
	//
	// SET ON THE BRIDGE'S SERVERS ALONE, to the bridge's longest answer
	// (internal/api/mcpbridge's Bridge.CallTimeout): the bridge keeps a running
	// call alive past every silence check, so a hung call would otherwise wait
	// out the harness default (Claude Code's 1e8 ms, OpenCode's twelve hours,
	// per their bundles). An in-box server is the harness's to
	// time.
	CallTimeout time.Duration

	// Tools are the tools the server lists as the run starts, by name, where
	// the engine knows them: the bridge's own server's
	// (internal/api/mcpbridge's Listed). A runner whose harness reports what
	// it offers its model holds it to them, so a home's or a repository's
	// setting that takes one away fails the run rather than leaving it to
	// work without. Nil for a server whose list is its own to give.
	Tools []string
}

// Transport is how the in-box agent reaches one server.
//
// A NAMED TYPE over a closed set, spelled a third time beside
// `config.MCPTransport` and `mcp.TransportKind` because this package depends
// on the server's SHAPE, not the config tree. It buys [Transport.Valid]: an
// unrecognised value would otherwise fall to stdio and render
// `{"command": ""}`, a server the agent spends a round failing to launch.
type Transport string

const (
	// TransportStdio launches the server as a child process inside the box.
	TransportStdio Transport = "stdio"
	// TransportHTTP points the agent at a remote server.
	TransportHTTP Transport = "http"
)

// Valid reports whether t is a transport this package can render.
//
// EMPTY IS VALID and means stdio, the default `config.MCPServer.Kind` applies.
func (t Transport) Valid() bool {
	switch t {
	case "", TransportStdio, TransportHTTP:
		return true
	default:
		return false
	}
}

// RenderMCP selects a seat's scoped sandbox surface.
//
// It answers in the SAME type it takes, not a `map[string]any` of one
// harness's config keys, which each runner would assert its way back out of.
//
// Only the named servers are included, and one with no matching configuration
// is SKIPPED rather than returned empty: the agent would spend a round
// discovering it cannot connect. The input is walked in the seat's own order
// so a duplicate name resolves once.
func RenderMCP(servers []MCPServer, scoped []string) map[string]MCPServer {
	if len(scoped) == 0 {
		return nil
	}
	byName := make(map[string]MCPServer, len(servers))
	for _, s := range servers {
		byName[s.Name] = s
	}
	out := make(map[string]MCPServer, len(scoped))
	for _, name := range scoped {
		server, known := byName[name]
		if !known {
			log.Warn("sandbox_mcp_server_unknown", "server", name,
				"hint", "role.sandbox.mcp.servers names a server that is not "+
					"in mcp.servers, so the harness will not get it")
			continue
		}
		// SKIPPED like an unknown name: a server a runner cannot render is a
		// broken entry the agent would spend a round on.
		if !server.Transport.Valid() {
			log.Warn("sandbox_mcp_transport_unknown", "server", name,
				"transport", string(server.Transport),
				"hint", "mcp.servers."+name+".transport must be stdio or http, "+
					"so the harness will not get this server")
			continue
		}
		out[name] = forTransport(server)
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// forTransport is one server as its transport reads it, sharing nothing with
// the server it was made from.
//
// The OTHER transport's fields are cleared: a stdio server with headers is a
// config nobody wrote, and a runner could render that credential into a file
// the transport never reads.
func forTransport(server MCPServer) MCPServer {
	out := server
	if server.Transport == TransportHTTP {
		out.Command, out.Args, out.Env = "", nil, nil
		out.Headers = maps.Clone(server.Headers)
		out.Secrets = slices.Clone(server.Secrets)
		return out
	}
	// Stdio, which an empty transport also means. An unrecognised one never
	// reaches here — RenderMCP skips it.
	out.URL, out.Headers, out.Secrets = "", nil, nil
	out.Args = slices.Clone(server.Args)
	out.Env = maps.Clone(server.Env)
	return out
}
