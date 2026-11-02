package sandbox

import "testing"

// A box stands the kernel's whole scale ahead of its engine, held to the
// kernel's ceiling: the one lead at which the kernel takes a box's largest
// process before the engine whatever the two hold, placed from the engine's
// own value — its deployment's decision — rather than over it.
func TestABoxStandsAWholeScaleAheadOfItsEngineUpToTheKernelsCeiling(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name   string
		engine int
		want   int
	}{
		{"a plain host, a systemd unit or a container, at the default", 0, 1000},
		{"a Kubernetes Guaranteed pod, which its node's kernel takes last", -997, 3},
		{"an engine the kernel never kills", -1000, 0},
		{"a systemd unit that set OOMScoreAdjust=-500", -500, 500},
		{"a Kubernetes Burstable pod", 875, 1000},
		{"a Kubernetes BestEffort pod, at the ceiling already", 1000, 1000},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := boxOOMScoreAdj(tc.engine); got != tc.want {
				t.Fatalf("an engine at %d puts its boxes at %d, want %d", tc.engine, got, tc.want)
			}
		})
	}
}

// For every value an engine can run at, a box is never placed below it — the
// one direction an unprivileged process cannot move — never past the kernel's
// ceiling, and short of a whole scale ahead only where the ceiling stops it.
func TestNoEngineValueLeavesItsBoxesBehindIt(t *testing.T) {
	t.Parallel()
	for engine := -oomScoreAdjMax; engine <= oomScoreAdjMax; engine++ {
		box := boxOOMScoreAdj(engine)
		switch {
		case box < engine:
			t.Fatalf("an engine at %d puts its boxes below it, at %d: lowering needs CAP_SYS_RESOURCE", engine, box)
		case box > oomScoreAdjMax:
			t.Fatalf("an engine at %d puts its boxes at %d, past the kernel's ceiling of %d",
				engine, box, oomScoreAdjMax)
		case box-engine < oomScoreScale && box != oomScoreAdjMax:
			t.Fatalf("an engine at %d puts its boxes at %d: under a whole scale ahead with room left "+
				"below the ceiling, so an engine holding enough more memory goes first", engine, box)
		}
	}
}
