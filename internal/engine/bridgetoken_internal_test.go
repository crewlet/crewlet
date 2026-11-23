package engine

import (
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/api/mcpbridge"
	"github.com/crewlet/crewlet/internal/config"
)

// A LAUNCHED RUN'S BRIDGE TOKEN ENDS AT ITS TURN'S OWN CAP, AND ONLY THERE: a
// scheduled turn's run is handed a token that expires at the turn's wall-clock
// cap, and any other turn's one with no expiry of its own, which its run's end
// alone ends (mcpbridge.Session.Deadline). Through the real launcher, because
// it is a line of wiring in LaunchExecutor.
//
// Mutation: leave the turn's cap off the launch's session, and the scheduled
// row's token carries none.
func TestALaunchedRunsBridgeTokenEndsAtItsTurnsOwnCap(t *testing.T) {
	t.Parallel()
	capped := time.Now().UTC().Add(3 * time.Hour).Truncate(time.Second)
	for _, tc := range []struct {
		name   string
		cap    time.Time
		expiry string // the token's expiry field, as it travels
	}{
		{name: "a turn with no cap", expiry: "0"},
		{name: "a scheduled turn", cap: capped, expiry: strconv.FormatInt(capped.Unix(), 10)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			rig := bridgedRig(t, "t-token")
			rig.launcher.turn.Deadline = tc.cap
			rig.launch(t, nil)
			endpoint := rig.jobs.Started()[0].MCPServers[config.ToolServerName].URL
			_, token, _ := strings.Cut(endpoint, mcpbridge.PathPrefix)
			// crewlet_rt1.<tag>.<subject>.<expiry>.<signature>
			if parts := strings.Split(token, "."); len(parts) != 5 || parts[3] != tc.expiry {
				t.Errorf("the run was handed the token %q, want one whose expiry is %q", token, tc.expiry)
			}
		})
	}
}
