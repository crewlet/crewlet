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
	Transport string // "stdio" (default) or "http"

	// The stdio fields.
	Command string
	Args    []string
	Env     map[string]string

	// The http fields.
	URL     string
	Headers map[string]string
}

// LaunchSpec is one server as a runner writes it into a box.
type LaunchSpec = map[string]any

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
	if server.Transport == "http" {
		spec := LaunchSpec{"type": "http", "url": server.URL}
		if headers := merged(server.Headers, seatCredentials); len(headers) > 0 {
			spec["headers"] = headers
		}
		return spec
	}
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
