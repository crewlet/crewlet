package seataccess

import (
	"github.com/crewlet/crewlet/internal/config"
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

// ToolStateOf is whether a seat can use its credential through a tool on lane
// right now, as the access route reports it: off while the seat or its
// account is off; live or waiting where a per-seat server reads the lane's
// credential (waiting until the credential is sealed); and otherwise builtin
// for Jira and Confluence, whose credential the engine's own tools read, and
// no_server for the rest.
func ToolStateOf(c *config.Company, lane Lane, want Desired, have Observed) ToolState {
	if !want.Enabled || !have.Exists || !have.Enabled {
		return ToolOff
	}
	if c == nil || len(c.SeatServersFor(string(lane.Kind()))) == 0 {
		if lane == LaneJira || lane == LaneConfluence {
			return ToolBuiltin
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
