package seataccess

import (
	"slices"

	"github.com/crewlet/crewlet/internal/config"
)

// ToolBinding is a recognised server's binding for one integration (D15). The
// table is the org model's ([config.SeatToolBindings]), which is where a
// seat's credential is bound, so what a lane reports and what a seat's server
// is started with cannot disagree.
type ToolBinding = config.SeatToolBinding

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

// ToolStateOf is whether a seat can use its credential through a tool on lane
// right now, as the access route reports it: off while the seat or its
// account is off; live or waiting where a per-seat server reads the lane's
// credential (waiting until the credential is sealed); server_off where every
// such server is turned off; and otherwise builtin for Jira and Confluence,
// whose credential the engine's own tools read, and no_server for the rest.
//
// A SERVER TURNED OFF READS NO CREDENTIAL ([config.MCPServer.IsEnabled]): it
// starts for nobody, so a lane whose only server is off is not live however
// well its credential is sealed. Jira and Confluence stay builtin then, since
// the engine's own tools read their credential with or without a server.
func ToolStateOf(c *config.Company, lane Lane, want Desired, have Observed) ToolState {
	if !want.Enabled || !have.Exists || !have.Enabled {
		return ToolOff
	}
	var servers []*config.MCPServer
	if c != nil {
		servers = c.SeatServersFor(string(lane.Kind()))
	}
	if !slices.ContainsFunc(servers, (*config.MCPServer).IsEnabled) {
		if lane == LaneJira || lane == LaneConfluence {
			return ToolBuiltin
		}
		if len(servers) > 0 {
			return ToolServerOff
		}
		return ToolNoServer
	}
	if have.Credential == nil && lane != LaneGitHub {
		// GITHUB'S TOKEN IS ISSUED AT USE from the app's sealed key, so an
		// installed app is a live tool; every other credential is the
		// sealed value itself.
		return ToolWaiting
	}
	return ToolLive
}
