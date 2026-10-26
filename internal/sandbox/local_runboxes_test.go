//go:build unix

package sandbox

import (
	"cmp"
	"os"
	"slices"
	"testing"
)

// A LOCAL BOX RECORDS THE RUN IT WAS MADE FOR, and its host lists it by that
// run — every box under the root, whichever of the two local cells made it,
// each under the cell its own record names, and each once however many
// backends share the root. A box made for no run is not listed: nothing can
// say whose it is. Nor is one whose record cannot be read.
//
// Mutation: list a box whatever its record says, and the box made for no run
// is listed; list it by the backend that found it, and the container box is
// named direct.
func TestAHostListsItsBoxesByTheRunEachWasMadeFor(t *testing.T) {
	t.Parallel()
	direct := newDirect(t)
	// THE SECOND CELL ON THE SAME ROOT, as a catalogue's two local cells
	// share providers.sandbox.local.state_dir.
	container := &Local{opts: LocalOptions{Placement: Container}, root: direct.root}

	made := mustCreate(t, direct, Spec{TurnID: "t-direct", Placement: Direct})
	nobodys := mustCreate(t, direct, Spec{})
	unreadable := mustCreate(t, direct, Spec{TurnID: "t-unreadable"})
	if err := os.WriteFile(layoutOf(t, unreadable).runFile(), []byte("{not json"), 0o600); err != nil {
		t.Fatalf("spoil the record: %v", err)
	}
	// A box the container cell made, recorded as its backend records it.
	contained := mustCreate(t, direct, Spec{})
	if err := recordRun(layoutOf(t, contained), boxRun{TurnID: "t-container", Placement: Container}); err != nil {
		t.Fatalf("recordRun: %v", err)
	}

	manager, err := NewManager(ManagerOptions{
		Providers:        map[Placement]Provider{Direct: direct, Container: container},
		Runners:          map[string]Runner{"claude-code": NewFakeRunner("claude-code")},
		DefaultPlacement: Direct,
	})
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	boxes, err := manager.HostBoxes()
	if err != nil {
		t.Fatalf("HostBoxes: %v", err)
	}
	want := []HostBox{
		{SandboxID: made.ID(), TurnID: "t-direct", Placement: Direct},
		{SandboxID: contained.ID(), TurnID: "t-container", Placement: Container},
	}
	slices.SortFunc(boxes, func(a, b HostBox) int { return cmp.Compare(a.TurnID, b.TurnID) })
	slices.SortFunc(want, func(a, b HostBox) int { return cmp.Compare(a.TurnID, b.TurnID) })
	if !slices.Equal(boxes, want) {
		t.Errorf("the host lists %+v; want %+v (and never %s, made for no run)", boxes, want, nobodys.ID())
	}
}
