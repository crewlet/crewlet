package mattermost

import (
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/coord"
)

// THE CLAIM OUTLIVES THE REPLAY HORIZON, AND THE STORE KEEPS IT.
//
// A post can be read again for as long as a reconnecting peer's replay reaches
// back — MaxBackfill — and that replay may run only after the reconnect waited
// out the backoff ceiling, jittered. A claim that lapsed inside that span lets
// the peer deliver the post a second time; one longer than the coordination
// store keeps is refused outright, and every post then fails open.
//
// Mutation: set ClaimTTL to coord.ClaimTTL (the webhook's five minutes), or to
// anything past coord.MaxClaimTTL.
func TestTheClaimOutlivesTheReplayHorizon(t *testing.T) {
	ceiling := ReconnectBackoff[len(ReconnectBackoff)-1]
	jittered := ceiling + time.Duration(reconnectJitter*float64(ceiling))
	// A minute of clock difference between the server, this node and the
	// peer that claims next: the replay's floor falls back to this node's
	// clock when the server cannot be asked.
	const margin = time.Minute
	if floor := MaxBackfill + jittered + margin; ClaimTTL < floor {
		t.Errorf("ClaimTTL = %v, want at least %v: MaxBackfill (%v) + the jittered backoff "+
			"ceiling (%v) + %v of clock margin", ClaimTTL, floor, MaxBackfill, jittered, margin)
	}
	if ClaimTTL > coord.MaxClaimTTL {
		t.Errorf("ClaimTTL = %v exceeds coord.MaxClaimTTL (%v), which the coordination "+
			"store refuses — every claim would error and every post fail open", ClaimTTL, coord.MaxClaimTTL)
	}
}
