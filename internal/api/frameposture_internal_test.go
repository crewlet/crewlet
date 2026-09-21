package api

import (
	"testing"

	"github.com/crewlet/crewlet/internal/api/stream"
)

// TestTheSocketDegradesOnTheSameSetReadyRefusesOn.
//
// Three different things are called a posture and this is the boundary between
// two of them: the NODE posture the health body carries, and the FRAME posture
// one socket is served in. The set is read from [divergedPostures] rather than
// restated, because a node whose copy of the company is wrong takes itself out
// of rotation AND stops pushing that copy at the tabs still watching. Written
// twice, a node could leave rotation while its dashboards carried on rendering
// what it held — which is the exact pair of facts an operator is trying to
// reconcile when they look.
//
// `wait` and `isolated` stay LIVE for the reason /ready stays ready on them:
// wait is ordinary propagation during a rollout, and isolated means no node
// applied the revision, so degrading there would blind every operator at the
// moment the fleet most needs watching.
func TestTheSocketDegradesOnTheSameSetReadyRefusesOn(t *testing.T) {
	t.Parallel()
	for _, status := range []string{"shed", "stuck"} {
		if got := framePosture(stream.Health{Status: status}); got != stream.FrameDegraded {
			t.Errorf("a %q node serves its sockets %q, want degraded", status, got)
		}
		if _, diverged := divergedPostures[status]; !diverged {
			t.Errorf("%q is not on the set /ready refuses on, so the two "+
				"surfaces have drifted", status)
		}
	}
	for _, status := range []string{StatusOK, "wait", "isolated", StatusUnconfigured, StatusShuttingDown} {
		if got := framePosture(stream.Health{Status: status}); got != stream.FrameLive {
			t.Errorf("a %q node serves its sockets %q, want live: it is not "+
				"a posture /ready leaves rotation for either", status, got)
		}
	}
	// Whatever it answers is a posture the fan-out will take. The zero
	// FramePosture is invalid by construction, and a client set to one is
	// served nothing at all.
	if !framePosture(stream.Health{Status: "something-new"}).Valid() {
		t.Error("framePosture answered an invalid posture for an unknown status")
	}
}
