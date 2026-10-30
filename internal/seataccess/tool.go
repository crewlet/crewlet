package seataccess

import (
	"github.com/crewlet/crewlet/internal/config"
)

// ToolBinding is how a seat's own credential reaches its tool server for one
// lane (D15). The table is the org model's ([config.SeatToolBindings]), which
// is where the binding is made, so what a lane reports and what a seat's
// server is started with cannot disagree.
type ToolBinding = config.SeatToolBinding

// ToolFor is lane's binding, or nil where the engine's own tools read the
// credential directly (Jira, Confluence, Mattermost).
func ToolFor(lane Lane) *ToolBinding {
	b, ok := config.SeatToolBindingFor(string(lane))
	if !ok {
		return nil
	}
	return &b
}

// ToolStateOf is whether a seat can use its credential through a tool on lane
// right now, as the access route reports it: off while the seat or its
// account is off, builtin where the engine's own tools read the credential,
// no_server where the company declares no per-seat server for it, waiting
// until the credential is sealed, and live after.
func ToolStateOf(c *config.Company, lane Lane, want Desired, have Observed) ToolState {
	if !want.Enabled || !have.Exists || !have.Enabled {
		return ToolOff
	}
	b := ToolFor(lane)
	if b == nil {
		return ToolBuiltin
	}
	if c == nil || c.PerSeatServer(b.Server) == nil {
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
