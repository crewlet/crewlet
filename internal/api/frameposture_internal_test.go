package api

import (
	"testing"

	"github.com/crewlet/crewlet/internal/api/stream"
)

// TestTheSocketDegradesOnTheSameSetReadyRefusesOn.
//
// This is the boundary between the NODE posture the health body carries and
// the FRAME posture one socket is served in. The set is read from
// [divergedPostures], not restated: written twice, a node could leave rotation
// while its dashboards carried on rendering the wrong company it held.
//
// `wait` and `isolated` stay LIVE as /ready stays ready on them: wait is
// ordinary rollout propagation, and isolated means no node applied the
// revision, so degrading would blind operators when the fleet most needs
// watching.
func TestTheSocketDegradesOnTheSameSetReadyRefusesOn(t *testing.T) {
	t.Parallel()
	for _, status := range []string{"shed", "stuck"} {
		if got := framePosture(Health{Status: status}); got != stream.FrameDegraded {
			t.Errorf("a %q node serves its sockets %q, want degraded", status, got)
		}
		if _, diverged := divergedPostures[status]; !diverged {
			t.Errorf("%q is not on the set /ready refuses on, so the two "+
				"surfaces have drifted", status)
		}
	}
	for _, status := range []string{StatusOK, "wait", "isolated", StatusUnconfigured, StatusShuttingDown} {
		if got := framePosture(Health{Status: status}); got != stream.FrameLive {
			t.Errorf("a %q node serves its sockets %q, want live: it is not "+
				"a posture /ready leaves rotation for either", status, got)
		}
	}
	// Whatever it answers is a posture the fan-out will take. The zero
	// FramePosture is invalid by construction, and a client set to one is
	// served nothing at all.
	if !framePosture(Health{Status: "something-new"}).Valid() {
		t.Error("framePosture answered an invalid posture for an unknown status")
	}
}
