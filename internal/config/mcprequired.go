package config

import (
	"fmt"

	"github.com/crewlet/crewlet/internal/mcp/catalogue"
	"github.com/crewlet/crewlet/internal/org"
)

// validateMCPRequiredKeys refuses a recognised remote server that would connect
// without a header its catalogue entry requires and nothing sends. OAuth
// servers and local servers, which may read their host's own sign-in, are not judged.
func (c *Company) validateMCPRequiredKeys(o *org.Organization) error {
	var p problems
	for i := range c.MCPServers {
		server := &c.MCPServers[i]
		if server.Kind() != TransportHTTP || server.OAuth != nil {
			continue
		}
		known, ok := catalogue.Recognise(server.Launch())
		if !ok {
			continue
		}
		for _, in := range known.Headers {
			if !in.Required || in.Value != "" || c.suppliesKey(server, in.Name, o) {
				continue
			}
			where := "this entry's headers"
			if !server.IsShared() {
				where += ", or the mcp_env block of an agent it reaches"
			}
			p.addSaying(entry(at(idx(field("mcp_servers"), i), "headers"), in.Name), ErrMissing,
				fmt.Sprintf("%s needs %s, and nothing sends it. Give it a value here.", known.Title, in.Name),
				"%s requires the %s header and nothing supplies it, so the server would refuse every "+
					"request: give it in %s", known.Title, in.Name, where)
		}
	}
	return p.err()
}

// suppliesKey reports whether something hands server's instances key: its own
// values, its integration, or the block of an agent seat it reaches. A
// per-seat server reaching no seat connects nowhere, so it lacks nothing.
func (c *Company) suppliesKey(server *MCPServer, key string, o *org.Organization) bool {
	if server.writesKey(server.ownValues(), key) {
		return true
	}
	for _, k := range server.SeatBindings() {
		if !k.Sign && server.ValueKey(k.Key) == server.ValueKey(key) {
			return true
		}
	}
	for _, k := range c.HandedOrgKeys(server) {
		if server.ValueKey(k.Key) == server.ValueKey(key) {
			return true
		}
	}
	if server.IsShared() || o == nil {
		return false
	}
	reached := false
	for seat := range o.AllRoles() {
		if _, ok := server.Reach(seat); !ok {
			continue
		}
		reached = true
		if server.writesKey(seat.MCPEnv[server.Name], key) {
			return true
		}
	}
	return !reached
}
