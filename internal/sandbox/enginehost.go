package sandbox

import (
	"maps"
	"net"
	"net/url"
	"strings"
)

// How a box dials a service on the engine host. The engine hands a box
// addresses in the ENGINE HOST'S network, one value per node: the tool bridge
// (CREWLET_MCP_BRIDGE_URL), the telemetry receiver
// (CREWLET_SANDBOX_OTEL_RECEIVER_URL) and every HTTP MCP server a seat scopes
// in. A container's 127.0.0.1 is itself, while a `direct` box shares the
// host's network, where the runtime's host alias does not resolve, so no one
// value serves both. So only where it crosses into a box whose network is not
// the host's is a loopback host replaced by the runtime's alias for the host.
// Any other host names a machine both sides see, and what an operator declares
// for the box (`role.sandbox.env`, a setup step's `env`) is in its network, so
// neither moves. A remote box gets no translation: nothing there reaches a
// laptop's loopback (docs/concepts/code-sandbox.md).

// engineHostAlias is a box whose network is not the engine host's, and which
// knows the name the host answers to inside it.
type engineHostAlias interface {
	// EngineHost is the hostname that reaches the engine host from inside
	// the box.
	EngineHost() string
}

// EngineAddress is raw as box dials it: a loopback host replaced by the name
// the engine host answers to inside the box, where the box's network is not
// the host's. Anything else comes back unchanged.
func EngineAddress(box Sandbox, raw string) string {
	aliased, ok := box.(engineHostAlias)
	if !ok {
		return raw
	}
	return rehost(raw, aliased.EngineHost())
}

// EngineHostName reports whether host (a bare name, no port, in any case) is
// the name Docker or Podman gives the engine host inside a box: the one
// [EngineAddress] hands a box in place of loopback. The tool bridge
// (internal/api/mcpbridge) admits it as the Host of a loopback-held session's
// call, since Docker Desktop and Podman's machine on macOS and Windows deliver
// a container's call under the name it dialled. Both names are under
// `.internal`, kept for private use, so no public DNS or internet page answers
// to either.
func EngineHostName(host string) bool {
	return strings.EqualFold(host, dockerHostAlias) || strings.EqualFold(host, podmanHostAlias)
}

// EngineServers is servers as box dials them: [EngineAddress] applied to every
// HTTP server's URL; a stdio server runs inside the box and dials nothing of
// the engine's, so it is copied as it is. A copy, because the caller's map is
// the seat's rendered servers, which a later run in another cell reads again.
func EngineServers(box Sandbox, servers map[string]MCPServer) map[string]MCPServer {
	if _, ok := box.(engineHostAlias); !ok || len(servers) == 0 {
		return servers
	}
	out := make(map[string]MCPServer, len(servers))
	for name, server := range servers {
		if server.Transport == TransportHTTP {
			server.Headers = maps.Clone(server.Headers)
			server.URL = EngineAddress(box, server.URL)
		}
		out[name] = server
	}
	return out
}

// rehost replaces a loopback host in raw with alias, keeping the scheme,
// port, path, query and any user information. A value that is not an
// absolute URL, or whose host is anything but loopback, is returned as it is.
func rehost(raw, alias string) string {
	if alias == "" {
		return raw
	}
	u, err := url.Parse(raw)
	if err != nil || u.Scheme == "" || u.Host == "" || !LoopbackHost(u.Hostname()) {
		return raw
	}
	if port := u.Port(); port != "" {
		u.Host = net.JoinHostPort(alias, port)
	} else {
		u.Host = alias
	}
	return u.String()
}

// LoopbackHost reports whether host names this machine's loopback interface
// alone: `localhost` or a name under it (RFC 6761 reserves both for loopback,
// in any case), or a loopback address in either family, bracketed or not.
func LoopbackHost(host string) bool {
	host = strings.Trim(host, "[]")
	const suffix = ".localhost"
	if strings.EqualFold(host, "localhost") ||
		(len(host) > len(suffix) && strings.EqualFold(host[len(host)-len(suffix):], suffix)) {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}
