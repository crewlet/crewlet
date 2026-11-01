package sandbox

import (
	"maps"
	"slices"
)

// The scoped MCP surface the in-box coding agent gets.
//
// The coding agent is itself an MCP client, so the runner hands it a config of
// its own: ONLY the servers the seat named in role.sandbox.mcp.servers, each
// with the seat's own credentials.
//
// SCOPING IS AT THE SERVER LEVEL, and that is a platform fact rather than a
// simplification. Neither supported CLI offers a per-tool allowlist the engine
// could enforce — one has no such flag, and the other runs with permissions
// bypassed because a prompt would hang a headless run — so a curated tool list
// would be a claim rather than a control. What the engine decides is which
// SERVERS reach the box, which it decides before the agent starts.
//
// This is a pure SELECTION, deliberately: the engine's servers in, each with
// its values already resolved and the seat's credentials already laid over
// them, the same server shape out with the unscoped and unreachable entries
// dropped. The merge is the engine's, beside the rule that decides whether a
// server may start at all, because the two cannot be separated: a key a
// server may go without is left out of the MERGED values, so a seat's value
// that resolves to nothing never hands the key back to the server's default.
// What each CLI's config file looks like belongs to its runner — which is
// where the key names live, because this package once rendered them and then
// told the reader they belonged elsewhere.

// MCPServer is the engine's view of one configured server.
//
// Declared here rather than imported from config so this package depends on
// the SHAPE and not on the config tree — the same reason the setup step is a
// translation. A caller assembles these from whatever its config says, with
// every `${VAR}` reference already RESOLVED and the seat's own credentials
// already merged in: what lands here is written into the coding CLI's own
// config as it stands, and neither CLI reads a reference the way the engine
// does.
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
}

// Transport is how the in-box agent reaches one server.
//
// A NAMED TYPE over a closed set, not a bare string, and this is the third
// place the same set is spelled — `config.MCPTransport` and `mcp.TransportKind`
// are the other two, each owned by the layer that acts on it, because this
// package deliberately depends on the SHAPE of a server rather than on the
// config tree.
//
// What the type buys is [Transport.Valid]: as a bare string an unrecognised
// value fell through to the stdio branch and rendered `{"command": ""}` — a
// server the agent inside the box then spends a round failing to launch, with
// nothing anywhere saying why.
type Transport string

const (
	// TransportStdio launches the server as a child process inside the box.
	TransportStdio Transport = "stdio"
	// TransportHTTP points the agent at a remote server.
	TransportHTTP Transport = "http"
)

// Valid reports whether t is a transport this package can render.
//
// EMPTY IS VALID and means stdio, which is the default an operator gets by
// saying nothing — the same default `config.MCPServer.Kind` applies.
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
// It answers in the SAME type it takes — not a generic `map[string]any` of
// each CLI's own config keys. That shape erased everything a runner needed:
// internal/sandbox/executor's OpenCode writer had to assert its way back
// out of it, and its args branch carried two fallbacks (`[]string` and
// `[]any`) because nothing but a convention said which one it would find. The
// keys it erased into were Claude Code's, which this package's own doc said
// belonged to a runner.
//
// Only the named servers are included, and one with no matching configuration
// is SKIPPED rather than returned empty: the agent must never be shown a
// server the engine does not know, because it would spend a round discovering
// that it cannot connect.
//
// Order does not matter to the output — it is a map — but the input list is
// walked in the seat's own order so a duplicate name resolves once.
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
					"in mcp.servers, so the coding agent will not get it")
			continue
		}
		// SKIPPED like an unknown name, for the same reason: handing a
		// runner a server it cannot render would put a broken entry in the
		// agent's config, and a round spent discovering that is worse than
		// not offering it. Rendering an unrecognised transport as stdio —
		// which is what a bare string field did — produced an empty command
		// and said nothing at all.
		if !server.Transport.Valid() {
			log.Warn("sandbox_mcp_transport_unknown", "server", name,
				"transport", string(server.Transport),
				"hint", "mcp.servers."+name+".transport must be stdio or http, "+
					"so the coding agent will not get this server")
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
// The OTHER transport's field is cleared rather than carried: a stdio server
// with headers on it is a config nobody wrote, and leaving one there would let
// a future runner render a credential into a file the transport never reads.
func forTransport(server MCPServer) MCPServer {
	out := server
	if server.Transport == TransportHTTP {
		out.Command, out.Args, out.Env = "", nil, nil
		out.Headers = maps.Clone(server.Headers)
		return out
	}
	// Stdio, which an empty transport also means. An unrecognised one never
	// reaches here — RenderMCP skips it.
	out.URL, out.Headers = "", nil
	out.Args = slices.Clone(server.Args)
	out.Env = maps.Clone(server.Env)
	return out
}
