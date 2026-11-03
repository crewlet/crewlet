package seataccess

import (
	"slices"
	"strings"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/envref"
	"github.com/crewlet/crewlet/internal/mcp/catalogue"
)

// ToolBinding is a recognised server's binding for one integration (D15). The
// recognition is the MCP catalogue's ([catalogue.Bindings]), which the org
// model binds a seat's credential by, so what a lane reports and what a
// seat's server is started with cannot disagree.
type ToolBinding = catalogue.Binding

// ToolFor is the conventional server this build recognises for lane's
// integration, or nil where it recognises none. A company binds any other
// server by declaring its `seat_credentials`.
func ToolFor(lane Lane) *ToolBinding {
	b, ok := config.SeatToolBindingFor(string(lane))
	if !ok {
		return nil
	}
	return &b
}

// ToolStateOf is whether the seat with handle can use its credential through
// a tool on lane right now, as the access route reports it: off while the seat
// or its account is off; live or waiting where a per-seat server reads the
// lane's credential, or the seat's box sets the lane's [LaneSpec.BoxEnv] to
// exactly its sealed name (waiting until the credential is sealed);
// server_off where every such server is turned off; and otherwise builtin for
// Jira and Confluence, whose credential the engine's own tools read, and
// no_server for the rest, a box CLI whose variable is unset or points
// elsewhere included.
//
// A SERVER TURNED OFF READS NO CREDENTIAL ([config.MCPServer.IsEnabled]): it
// starts for nobody, so a lane whose only server is off is not live however
// well its credential is sealed. Jira and Confluence stay builtin then, since
// the engine's own tools read their credential with or without a server.
func ToolStateOf(c *config.Company, handle string, lane Lane, want Desired, have Observed) ToolState {
	if !want.Enabled || !have.Exists || !have.Enabled {
		return ToolOff
	}
	var servers []*config.MCPServer
	if c != nil {
		servers = c.SeatServersFor(string(lane.Kind()))
	}
	if !slices.ContainsFunc(servers, (*config.MCPServer).IsEnabled) {
		switch {
		case lane == LaneJira || lane == LaneConfluence:
			return ToolBuiltin
		case boxReads(c, handle, lane):
			// THE SEAT'S OWN BOX IS ITS TOOL: Expo's EAS CLI reads the
			// seat's token as EXPO_TOKEN.
		case len(servers) > 0:
			return ToolServerOff
		default:
			return ToolNoServer
		}
	}
	if have.Credential == nil && lane != LaneGitHub {
		// GITHUB'S TOKEN IS ISSUED AT USE from the app's sealed key, so an
		// installed app is a live tool; every other credential is the
		// sealed value itself.
		return ToolWaiting
	}
	return ToolLive
}

// boxReads reports whether the seat with handle sets lane's [LaneSpec.BoxEnv]
// in its own box to exactly `${<its sealed credential>}`.
func boxReads(c *config.Company, handle string, lane Lane) bool {
	vendor, ok := Lookup(lane.Kind())
	if c == nil || !ok || strings.TrimSpace(handle) == "" {
		return false
	}
	cat := vendor.Catalog()
	spec, ok := cat.Lane(lane)
	if !ok || spec.BoxEnv == "" {
		return false
	}
	want := SinkName(cat.Kind, cat.Credentials[0], handle)
	for role := range c.EachRole() {
		if strings.TrimSpace(role.IdentityKey()) != handle {
			continue
		}
		value, set := role.Sandbox.EnvValue(spec.BoxEnv)
		name, whole := envref.Whole(value)
		return set && whole && name == want
	}
	return false
}
