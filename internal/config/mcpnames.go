package config

import (
	"fmt"
	"maps"
	"net/netip"
	"net/url"
	"regexp"
	"slices"
	"strings"

	"github.com/crewlet/crewlet/internal/envref"
	"github.com/crewlet/crewlet/internal/providers/llm/cliagent/cliprofile"
)

// WHAT AN MCP SERVER ENTRY MAY NAME: its own name, the prefix of its tools,
// the headers or variables it is started with, and the address it is dialled
// at.
//
// Each was accepted as written and failed further on, where nobody was
// looking: a header name with a space in it failed every request the server's
// client sent, a variable name with a dot reached a child that could not read
// it, a server name with a slash could not be named in an address, and an
// address carrying a password was published on every read of the server's
// row. So each is refused where it is written, with what to write instead.
//
// ADMISSION RULES ([Company.ValidateAdmission]): a stored revision that breaks
// one still applies, and is warned about, because nothing about running
// depends on a name being well formed. Two of them are also held where the
// engine acts on the value, since a document cannot always show it: a tool's
// name is held to [cliprofile.ToolNameMax] where the MCP bridge files it, and
// an address written with a ${VAR} in it is judged once that resolves, before
// the server is dialled ([MCPAddressProblem]).

// MaxMCPServerName is the most characters a server's name may hold.
//
// The name is spelled in more places than any other key of the entry: the
// origin of every tool it serves (`mcp:<name>`), the key seats declare its
// credentials under, a path segment of the routes that act on it, and, for a
// server an executor's box runs, the middle of every tool name the box's CLI
// hands its model (`mcp__<name>__<tool>` for Claude Code), under the same 64
// characters [cliprofile.ModelToolNameMax] counts. 32 keeps it a short
// identifier in all of them, and leaves such a box tool 25 characters of its
// own.
const MaxMCPServerName = 32

// mcpServerName is a server name's grammar: lowercase letters, digits and
// hyphens, starting and ending with a letter or a digit. The one spelling
// every place the name lands takes as it is: an origin, a path segment, a
// header or variable name once its hyphens are underscores, and a key in each
// CLI's server list.
var mcpServerName = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]*[a-z0-9])?$`)

// headerName is an HTTP field name, RFC 9110's token: the characters an
// http server's header may be named with.
var headerName = regexp.MustCompile("^[!#$%&'*+.^_`|~0-9A-Za-z-]+$")

// MaxMCPToolPrefix is the longest `tool_prefix` a server may take: one less
// than [cliprofile.ToolNameMax], so a prefixed name has room for at least one
// character of the tool's own.
func MaxMCPToolPrefix() int { return cliprofile.ToolNameMax() - 1 }

// validateMCPNames refuses every name and address an `mcp_servers` entry, or
// a seat's or a unit's `mcp_env` block for it, holds that no client can use.
// See the file comment.
func (c *Company) validateMCPNames() error {
	var p problems
	for i := range c.MCPServers {
		server := &c.MCPServers[i]
		path := idx(field("mcp_servers"), i)
		server.validateName(&p, at(path, "name"))
		server.validateToolPrefix(&p, at(path, "tool_prefix"))
		switch server.Kind() {
		case TransportHTTP:
			server.validateKeys(&p, at(path, "headers"), server.Headers)
			// An address with a ${VAR} in it is judged once that resolves,
			// where the engine dials it; an empty one is the runnable
			// rule's to refuse.
			if strings.TrimSpace(server.URL) != "" && !envref.Has(server.URL) {
				if problem := MCPAddressProblem(server.URL); problem != "" {
					p.add(at(path, "url"), ErrUnknownValue, "%s", problem)
				}
			}
		case TransportStdio:
			server.validateKeys(&p, at(path, "env"), server.Env)
		}
		for j, credential := range server.SeatCredentials {
			if credential.Key == "" {
				continue
			}
			if problem := server.keyProblem(credential.Key); problem != "" {
				p.add(at(idx(at(path, "seat_credentials"), j), "key"), ErrUnknownValue, "%s", problem)
			}
		}
		if server.Name == "" {
			continue
		}
		for role, rolePath := range c.EachRole() {
			server.validateKeys(&p, entry(at(rolePath, "mcp_env"), server.Name), role.MCPEnv[server.Name])
		}
		for unit, unitPath := range c.EachUnit() {
			server.validateKeys(&p, entry(at(unitPath, "mcp_env"), server.Name), unit.MCPEnv[server.Name])
		}
	}
	return p.err()
}

// validateName holds the server's name to [mcpServerName] and
// [MaxMCPServerName]. An empty name is the runnable rule's to refuse, and
// [ToolServerName] the document's.
func (m *MCPServer) validateName(p *problems, path Path) {
	switch {
	case m.Name == "":
	case !mcpServerName.MatchString(m.Name):
		p.add(path, ErrUnknownValue,
			"%q is not a server name: use lowercase letters, digits and hyphens, starting "+
				"and ending with a letter or a digit. The name is the origin of every tool "+
				"the server serves and the key seats declare its credentials under", m.Name)
	case len(m.Name) > MaxMCPServerName:
		p.add(path, ErrOutOfRange,
			"is %d characters, and a server name is at most %d: it is spelled inside "+
				"every tool name a box's coding CLI hands its model, which a model "+
				"holds to 64 characters", len(m.Name), MaxMCPServerName)
	}
}

// validateToolPrefix holds `tool_prefix` to a model's tool-name characters
// and to [MaxMCPToolPrefix]: every tool of the server is filed under the
// prefix and its own name, run together, and a prefix a model's name cannot
// hold, or one that leaves no room, refuses every tool the server lists.
func (m *MCPServer) validateToolPrefix(p *problems, path Path) {
	switch prefix := m.ToolPrefix; {
	case prefix == "":
	case !cliprofile.ToolNameChars(prefix):
		p.add(path, ErrUnknownValue,
			"%q holds characters a model's tool name cannot: a prefix is put in front "+
				"of every tool the server serves, so use only letters, digits, _ and -", prefix)
	case len(prefix) > MaxMCPToolPrefix():
		p.add(path, ErrOutOfRange,
			"is %d characters, and a tool's whole name, prefix included, is at most %d "+
				"beside the longest prefix a coding CLI gives the engine's tools: a "+
				"prefix of at most %d leaves room for a name. Keep it short",
			len(prefix), cliprofile.ToolNameMax(), MaxMCPToolPrefix())
	}
}

// validateKeys holds every key of block, written at path, to what the server
// is started with it as ([MCPServer.keyProblem]).
func (m *MCPServer) validateKeys(p *problems, path Path, block map[string]string) {
	for _, key := range slices.Sorted(maps.Keys(block)) {
		if problem := m.keyProblem(key); problem != "" {
			p.add(entry(path, key), ErrUnknownValue, "%s", problem)
		}
	}
}

// keyProblem says why key cannot name a value this server is started with,
// or "": an http server sends each as a request header, and a stdio server
// starts its process with each as an environment variable.
func (m *MCPServer) keyProblem(key string) string {
	if m.Kind() == TransportHTTP {
		if headerName.MatchString(key) {
			return ""
		}
		return fmt.Sprintf("%q is not a header name: an http server is sent this value as a "+
			"request header, and a header's name has no spaces, colons or quotes. "+
			"Use letters, digits and - (as in X-Api-Key)", key)
	}
	if envref.ValidName(key) {
		return ""
	}
	return fmt.Sprintf("%q is not an environment variable name: a stdio server's process "+
		"is started with this value under the name, so use letters, digits and _, "+
		"not starting with a digit (as in GITHUB_TOKEN)", key)
}

// MCPAddressProblem says why the engine will not dial an http tool server at
// address, or "" when it will. It never quotes the address, which can carry a
// secret once a ${VAR} in it resolves.
//
// HTTPS, OR PLAIN HTTP TO A HOST ON NO PUBLIC NETWORK: every request to a
// tool server carries its credentials (an Authorization header, a seat's own
// token), and plain http to the public internet hands them to every network
// on the way. A server on this host or a private network is the operator's
// own to run unencrypted ([privateHost]). No user name or password in the
// address: the address is published wherever the server is shown (its row on
// `GET /mcp-servers`, a config read, every log line naming it), and a
// credential belongs in a header, where the engine keeps it out of all of
// them.
//
// EXPORTED for the engine, which judges an address written with a ${VAR} in it
// once that resolves, before it dials the server or hands it to a box: the
// document can only be judged where the address is written out.
func MCPAddressProblem(address string) string {
	u, err := url.Parse(address)
	switch {
	case err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https"):
		return "is not an http or https address: write the whole address the server " +
			"answers at, starting https://"
	case u.User != nil:
		return "carries a user name or password, and an address is shown wherever the " +
			"server is: send the credential as a header instead (headers: " +
			"{Authorization: \"Bearer ${TOKEN}\"}), which the engine keeps secret"
	case u.Scheme == "http" && !privateHost(u.Hostname()):
		return "is plain http to a host on a public network, where every request and the " +
			"credentials in it travel unencrypted: use https. Plain http is for a server " +
			"on this host or a private network"
	}
	return ""
}

// privateHost reports whether host names a machine no public network routes
// to: a loopback, private or link-local address, or a name that resolves
// nowhere on the internet. A one-label name (a container's or a pod's service
// name), localhost, and the suffixes set aside for private use: `.localhost`
// (RFC 6761), `.local` (RFC 6762, and a Kubernetes cluster's
// `svc.cluster.local`), `.home.arpa` (RFC 8375) and `.internal` (reserved by
// ICANN for private networks).
//
// BY WHAT THE ADDRESS SAYS, not what it resolves to: the rule is about the
// operator's own address, which names where they run the server, and a public
// name that resolves to a private address today can resolve to anything
// tomorrow.
func privateHost(host string) bool {
	if ip, err := netip.ParseAddr(host); err == nil {
		ip = ip.Unmap()
		return ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast()
	}
	name := strings.TrimSuffix(strings.ToLower(host), ".")
	if name == "" {
		return false
	}
	if name == "localhost" || !strings.Contains(name, ".") {
		return true
	}
	for _, suffix := range []string{".localhost", ".local", ".home.arpa", ".internal"} {
		if strings.HasSuffix(name, suffix) {
			return true
		}
	}
	return false
}
