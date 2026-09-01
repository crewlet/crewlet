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
// This is a pure translation, deliberately: the engine's own server
// configuration and the seat's credentials in, the generic launch-spec shape
// the runners render out. What each CLI's config file looks like belongs to
// its runner, not here.

// MCPServer is the engine's view of one configured server.
//
// Declared here rather than imported from config so this package depends on
// the SHAPE and not on the config tree — the same reason the setup step is a
// translation. A caller assembles these from whatever its config says.
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

// LaunchSpec is one server as a runner writes it into a box.
//
// A DEFINED TYPE rather than an alias for `map[string]any`. As an alias it
// named nothing the compiler could hold on to, and two of the three places
// that carry this shape — [LaunchRequest.MCPServers] and
// [RunRequest.MCPServers] — spelled out `map[string]map[string]any` instead,
// so the concept had a name in exactly one file.
//
// The KEYS are the wire contract with each coding agent's config writer:
// "type"/"url"/"headers" for HTTP, "command"/"args"/"env" for stdio. They are
// generic on purpose — what each CLI's own config file looks like belongs to
// its runner, not here.
type LaunchSpec map[string]any

// RenderMCP builds the launch specs for a seat's scoped sandbox surface.
//
// Only the named servers are included, and one with no matching configuration
// is SKIPPED rather than rendered empty: the agent must never be shown a
// server the engine does not know, because it would spend a round discovering
// that it cannot connect.
//
// Order does not matter to the output — it is a map — but the input list is
// walked in the seat's own order so a duplicate name resolves once.
func RenderMCP(servers []MCPServer, scoped []string, credentials map[string]map[string]string) map[string]LaunchSpec {
	if len(scoped) == 0 {
		return nil
	}
	byName := make(map[string]MCPServer, len(servers))
	for _, s := range servers {
		byName[s.Name] = s
	}
	out := make(map[string]LaunchSpec, len(scoped))
	for _, name := range scoped {
		server, known := byName[name]
		if !known {
			log.Warn("sandbox_mcp_server_unknown", "server", name,
				"hint", "role.sandbox.mcp.servers names a server that is not "+
					"in mcp.servers, so the coding agent will not get it")
			continue
		}
		// SKIPPED like an unknown name, for the same reason: rendering it
		// would hand the agent a server it cannot reach, and a round spent
		// discovering that is worse than not offering it. Rendering an
		// unrecognised transport as stdio — which is what a bare string
		// field did — produced `{"command": ""}` and said nothing at all.
		if !server.Transport.Valid() {
			log.Warn("sandbox_mcp_transport_unknown", "server", name,
				"transport", string(server.Transport),
				"hint", "mcp.servers."+name+".transport must be stdio or http, "+
					"so the coding agent will not get this server")
			continue
		}
		out[name] = launchSpec(server, credentials[name])
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// launchSpec renders one server, merging the seat's credentials over the
// server's own.
//
// The seat's win: a server declares the shape and a seat declares who it is,
// so a seat that supplies a token is saying something more specific than the
// company-wide default it replaces.
func launchSpec(server MCPServer, seatCredentials map[string]string) LaunchSpec {
	if server.Transport == TransportHTTP {
		spec := LaunchSpec{"type": string(TransportHTTP), "url": server.URL}
		if headers := merged(server.Headers, seatCredentials); len(headers) > 0 {
			spec["headers"] = headers
		}
		return spec
	}
	// Stdio, which an empty transport also means. An unrecognised one never
	// reaches here — RenderMCP skips it — so this branch is not the
	// fall-through it used to be.
	spec := LaunchSpec{"command": server.Command, "args": slices.Clone(server.Args)}
	if env := merged(server.Env, seatCredentials); len(env) > 0 {
		spec["env"] = env
	}
	return spec
}

func merged(base, over map[string]string) map[string]string {
	if len(base) == 0 && len(over) == 0 {
		return nil
	}
	out := make(map[string]string, len(base)+len(over))
	maps.Copy(out, base)
	maps.Copy(out, over)
	return out
}
