package sandbox

import (
	"maps"
	"net"
	"net/url"
	"strings"
)

// How a box dials a service on the engine host.
//
// The engine hands a box addresses written in THE ENGINE HOST'S NETWORK: the
// tool bridge's endpoint (CREWLET_MCP_BRIDGE_URL), the telemetry receiver's
// (CREWLET_SANDBOX_OTEL_RECEIVER_URL) and the URL of every HTTP MCP server a
// seat scopes into its box, which the engine's own children dial from the
// host. Each is one value for the whole node, because it is a fact about the
// deployment rather than about any one box.
//
// LOOPBACK IS THE ONE ADDRESS THAT MEANS SOMETHING ELSE IN A CONTAINER.
// `127.0.0.1` names whatever machine it is dialled from. A `direct` box shares
// the host's network, so there it names the engine host, as it does for the
// engine. A container has a loopback interface of its own, so the same URL
// reaches the container itself, where nothing listens. A single-host install
// that sets the bridge to `http://127.0.0.1:8000`, as the examples do, could
// therefore run an executor in `direct` and in no other local cell, and naming
// the runtime's host alias instead broke `direct`, where that name does not
// resolve. A company that put one seat in each could not be served by any
// value. (A `direct` box on a node that serves the API dials that node's own
// listener, which the node derives, and reads no variable at all; the rule
// here is what keeps every other `direct` box, and every value the seat
// scopes into it, working as written.)
//
// So the address is translated where it crosses into the box's network, and
// nowhere else: a box whose network is not the host's names the alias its
// runtime gives the host, and a loopback host in an engine-side URL is
// replaced by it. Any other host is left exactly as written, because it
// already names a machine both sides can see (a LAN address, a load
// balancer, a public name), and so is every value an operator declared for
// the box itself (`role.sandbox.env`, a setup step's `env`), which is written
// in the box's network to begin with.
//
// A remote box gets no translation: there is no address in the box's network
// for a laptop's loopback, and docs/concepts/code-sandbox.md says so.

// engineHostAlias is a box whose network is not the engine host's, and which
// knows the name the host answers to inside it.
type engineHostAlias interface {
	// EngineHost is the hostname that reaches the engine host from inside
	// the box.
	EngineHost() string
}

// EngineAddress is raw as box dials it: a URL whose host is the engine host's
// loopback, with that host replaced by the name the host answers to inside
// the box, where the box's network is not the host's. Everything else, and
// every URL for a box that shares the host's network, comes back unchanged.
func EngineAddress(box Sandbox, raw string) string {
	aliased, ok := box.(engineHostAlias)
	if !ok {
		return raw
	}
	return rehost(raw, aliased.EngineHost())
}

// EngineHostName reports whether host — a bare name, no port — is one a
// container runtime gives the engine host inside a box it starts: the name
// [EngineAddress] hands a box in place of loopback, under Docker or under
// Podman. In any case, as DNS compares names.
//
// For the tool bridge to admit, beside a loopback name, as the Host of a call
// to a session it holds to this host's loopback (internal/api/mcpbridge):
// Docker Desktop and Podman's machine on macOS and Windows deliver a
// container's call to the engine host there, under the name it dialled. Both
// names are in `.internal`, which ICANN keeps for private use, so no public
// DNS answers either and no page a browser loads from the internet is served
// under one.
func EngineHostName(host string) bool {
	return strings.EqualFold(host, dockerHostAlias) || strings.EqualFold(host, podmanHostAlias)
}

// EngineServers is servers as box dials them: [EngineAddress] applied to
// every HTTP server's URL, in a map of its own. A stdio server runs inside
// the box and dials nothing of the engine's, so it is copied as it is.
//
// A COPY, because the caller's map is the seat's rendered servers, which a
// later run in another cell reads again.
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
