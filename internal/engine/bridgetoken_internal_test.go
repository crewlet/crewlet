package engine

import (
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/api/mcpbridge"
	"github.com/crewlet/crewlet/internal/config"
)

// A LAUNCHED RUN'S BRIDGE TOKEN CARRIES NO EXPIRY, ITS TURN CAPPED OR NOT: a
// scheduled turn's run is handed a token no clock ends, as any other turn's
// is, and the collection after its cap closes its session, which ends the
// token (mcpbridge's package doc, "A job's token ends with its opening").
// Through the real launcher, where the turn's cap is known.
//
// Mutation: hand the launch's session the turn's cap and mint its token until
// it, and the scheduled row's token carries the cap.
func TestALaunchedRunsBridgeTokenCarriesNoExpiryItsTurnCappedOrNot(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		cap  time.Time
	}{
		{name: "a turn with no cap"},
		{name: "a scheduled turn", cap: time.Now().UTC().Add(3 * time.Hour)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			rig := bridgedRig(t, "t-token")
			rig.launcher.turn.Deadline = tc.cap
			rig.launch(t, nil)
			endpoint := rig.jobs.Started()[0].MCPServers[config.ToolServerName].URL
			_, token, _ := strings.Cut(endpoint, mcpbridge.PathPrefix)
			// crewlet_rt1.<tag>.<subject>.<expiry>.<signature>, "0" the
			// expiry of a token with none (runtoken's Lifelong).
			if parts := strings.Split(token, "."); len(parts) != 5 || parts[3] != "0" {
				t.Errorf("the run was handed the token %q, want one with no expiry", token)
			}
		})
	}
}
