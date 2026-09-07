package integration

import "testing"

// THE READER'S VOCABULARY IS SHORTER THAN THE WIRE'S, and this is the seam.
//
// Six phases exist because the engine has to say precisely which of six
// situations a surface is in, and that value is stored, shared with peers and
// documented. An operator scanning a list wants a narrower answer, so the
// phases collapse. Pinned because the collapse is a decision, not a
// formatting rule: a later edit that made `activating` read "connected", or
// that split "setting up" back into two, would change what the screen claims.
func TestEveryPhaseHasAConciseLabel(t *testing.T) {
	// The control plane's own words (backlet's ReconcilePhase, rendered by
	// the console), so one company reads the same status in either product.
	want := map[Phase]string{
		PhaseDisconnecting: "Disconnecting",
		PhaseUnconfigured:  "Failed",
		PhaseAwaitingAdmin: "Action needed",
		PhaseProvisioning:  "Setting up agents",
		PhaseActivating:    "Waiting for the provider",
		PhaseDegraded:      "Action required",
		PhaseReady:         "Connected",
	}
	for _, phase := range Phases {
		got := phase.Label()
		if got != want[phase] {
			t.Errorf("%s labels as %q, want %q", phase, got, want[phase])
		}
	}
	// Every phase is covered, so adding one without deciding its label
	// fails here rather than reaching a screen as a raw wire value.
	if len(want) != len(Phases) {
		t.Errorf("%d phases and %d labels; a phase without a label reaches the reader as its wire value",
			len(Phases), len(want))
	}
	// READY IS THE ONLY ONE THAT CLAIMS THE INTEGRATION WORKS. Anything
	// else labelled "connected" would tell an operator a surface was fine
	// while the engine was still bringing it up or a person was still owed
	// something.
	for _, phase := range Phases {
		if phase != PhaseReady && phase.Label() == "Connected" {
			t.Errorf("%s reads as connected, but only ready means observed matches desired", phase)
		}
	}
}

// A phase this build does not know is rendered as itself, never guessed at.
func TestAnUnknownPhaseIsNotLabelled(t *testing.T) {
	got := Phase("something_a_newer_build_wrote").Label()
	if got != "something a newer build wrote" {
		t.Errorf("unknown phase labelled %q; it must render as its own value", got)
	}
}
