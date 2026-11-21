package seataccess

import (
	"testing"

	"github.com/crewlet/crewlet/internal/config"
)

// AN ENGINE-HELD LANE'S TOOLS ARE THE ENGINE'S OWN: with no server declared
// and no credential sealed, a seat on it reads builtin, as SSH does, never
// no_server or waiting; and it reads off while the seat is off. The control
// is the same lane, not engine-held, which has no tool to offer.
//
// Mutation: drop the EngineHeld arm, and the lane reads no_server.
func TestAnEngineHeldLanesToolIsBuiltin(t *testing.T) {
	t.Parallel()
	none := &config.Company{}
	held := LaneSpec{Lane: LaneDatadog, EngineHeld: true}
	sealed := LaneSpec{Lane: LaneDatadog}
	on := Desired{Enabled: true, Level: "read"}
	have := Observed{Exists: true, Enabled: true}
	for _, c := range []struct {
		name string
		spec LaneSpec
		want Desired
		tool ToolState
	}{
		{"engine-held, on", held, on, ToolBuiltin},
		{"engine-held, off", held, Desired{}, ToolOff},
		{"not engine-held, on", sealed, on, ToolNoServer},
	} {
		if got := toolState(none, "sre", LaneDatadog, c.spec, c.want, have); got != c.tool {
			t.Errorf("%s: %q, want %q", c.name, got, c.tool)
		}
	}
}
