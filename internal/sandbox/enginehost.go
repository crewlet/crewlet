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
// (CREWLET_SANDBOX_OTEL_RECEIVER_URL), every HTTP MCP server a seat scopes in,
// Gitlet's base ([BoxGitURLVar]) and a check's reads (CREWLET_CHECK_API). A
// container's 127.0.0.1 is itself, while a `direct` box shares the host's
// network, where the runtime's host alias does not resolve, so no one value
// serves both: only where it crosses into a box whose network is not the
// host's is a loopback host replaced by the runtime's alias for the host. Any
// other host names a machine both sides see, and what an operator declares for
// the box (`role.sandbox.env`, a setup step's `env`) is in its network, so
// neither moves (docs/concepts/code-sandbox.md).

// engineHostAlias is a box whose network is not the engine host's, or a
// backend making such boxes, and knows the name the host answers to inside
// one; "" for one on the host's network.
type engineHostAlias interface {
	// EngineHost is the hostname that reaches the engine host from inside
	// the box.
	EngineHost() string
}

// EngineHost is the name the engine host answers to inside a box: its
// container runtime's alias for the host, or "" for a box sharing the host's
// network, which dials every address as written. Known before the box exists
// ([Manager.EngineHost]), so an address handed in its environment is written
// as it dials it.
type EngineHost string

// Address is raw as a box behind h dials it: a loopback host replaced by h,
// keeping everything else. Anything else, and every value under "", comes
// back unchanged.
func (h EngineHost) Address(raw string) string { return rehost(raw, string(h)) }

// engineHostOf is the name the engine host answers to inside box.
func engineHostOf(box Sandbox) EngineHost {
	if aliased, ok := box.(engineHostAlias); ok {
		return EngineHost(aliased.EngineHost())
	}
	return ""
}

// EngineAddress is raw as box dials it: a loopback host replaced by the name
// the engine host answers to inside the box, where the box's network is not
// the host's. Anything else comes back unchanged.
func EngineAddress(box Sandbox, raw string) string { return engineHostOf(box).Address(raw) }

// EngineServers is servers as box dials them: [EngineAddress] applied to every
// HTTP server's URL; a stdio server runs inside the box and dials nothing of
// the engine's, so it is copied as it is. A copy, because the caller's map is
// the seat's rendered servers, which a later run in another cell reads again.
func EngineServers(box Sandbox, servers map[string]MCPServer) map[string]MCPServer {
	host := engineHostOf(box)
	if host == "" || len(servers) == 0 {
		return servers
	}
	out := make(map[string]MCPServer, len(servers))
	for name, server := range servers {
		if server.Transport == TransportHTTP {
			server.Headers = maps.Clone(server.Headers)
			server.URL = host.Address(server.URL)
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
