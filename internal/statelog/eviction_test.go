package statelog_test

import (
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/statelog"
)

// AN EVICTED NODE STAYS COUNTED UNTIL IT HAS HAD A CHANCE TO NOTICE.
//
// Dropping a node from the counted set is what lets the trim advance past its
// position — and the moment it does, that node's writes become records every
// applier drops, while it may still believe it holds everything it held. The
// window is how long the fleet waits for it to read its own tombstone.
func TestEvictionFenceHoldsTheTrimUntilTheNodeCanNotice(t *testing.T) {
	t.Parallel()
	now := time.Now()
	reported := []statelog.NodePosition{
		{NodeID: "a", Generation: 1, Seq: 9_000},
		{NodeID: "b", Generation: 1, Seq: 100},
	}

	fresh := statelog.CountedSet(now, reported, nil, []statelog.Tombstone{
		{NodeID: "b", At: now.Add(-time.Second), By: "operator"},
	})
	if len(fresh) != 2 {
		t.Fatalf("a node evicted a second ago is already uncounted (%d counted) "+
			"— it has not had a heartbeat in which to read its own tombstone, "+
			"and until it does it is still writing", len(fresh))
	}

	settled := statelog.CountedSet(now, reported, nil, []statelog.Tombstone{
		{NodeID: "b", At: now.Add(-statelog.EvictionFenceWindow - time.Second), By: "operator"},
	})
	if len(settled) != 1 || settled[0].NodeID != "a" {
		t.Fatalf("counted %v after the fence window, want only a — an eviction "+
			"that never takes effect leaves the floor pinned for ever", settled)
	}
}

// A NODE BETWEEN BOOT AND ITS FIRST HEARTBEAT COUNTS AT ZERO AND BLOCKS.
//
// That is exactly a node adopting a snapshot. Treating it as absent would let
// the trim advance past the tail it is about to replay — the state it is
// joining to escape.
func TestANodeWithNoPositionYetIsCountedAtZero(t *testing.T) {
	t.Parallel()
	now := time.Now()
	counted := statelog.CountedSet(now,
		[]statelog.NodePosition{{NodeID: "a", Generation: 1, Seq: 9_000}},
		[]statelog.Presence{{NodeID: "joiner"}}, nil)

	if len(counted) != 2 {
		t.Fatalf("counted %v, want both — a node holding a lease and no position "+
			"yet is a node adopting a snapshot", counted)
	}
	var found bool
	for _, n := range counted {
		if n.NodeID == "joiner" {
			found = true
			if n.Seq != 0 {
				t.Errorf("the joiner counts at %d, want 0 — a position it has "+
					"not reported is not a position it has reached", n.Seq)
			}
		}
	}
	if !found {
		t.Fatal("the joining node is not counted at all")
	}

	// AND IT BLOCKS THE TRIM, for at most one heartbeat.
	in := baseInputs()
	in.Counted = counted
	if d := statelog.Trim(in.Terms()); d.To != 0 || d.Blocked() != (d.To == 0) {
		t.Fatalf("the trim would remove up to %d with a node at position 0 — the "+
			"tail that node is about to replay must not be removed under it", d.To)
	}
}

// A LIVE LEASE REFUSES AN EVICTION, and that refusal is a precondition of the
// whole design.
//
// The write-path fence that stops an evicted node writing reads a tombstone
// cached on the coordination loop — the same loop the target still has if it
// is holding a lease. So the only state in which an eviction is safe is one
// where the target has been out of contact for several round trips, which is
// exactly why the publisher checks a third source that stays fresh when
// coordination is wedged.
func TestAnEvictionIsRefusedWhileTheTargetIsStillTalking(t *testing.T) {
	t.Parallel()
	live := []statelog.Presence{{NodeID: "b"}}

	err := statelog.PermitEviction("b", live, false)
	if err == nil {
		t.Fatal("a node holding a live presence lease was permitted to be evicted")
	}
	if !strings.Contains(err.Error(), "still reaching coordination") {
		t.Errorf("the refusal does not say why: %v", err)
	}
	if err := statelog.PermitEviction("gone", live, false); err != nil {
		t.Fatalf("a node that is out of contact was refused: %v", err)
	}
	// AND AN OPERATOR MAY OVERRIDE, because the refusal is about safety
	// rather than about permission and an operator can know something the
	// lease does not say.
	if err := statelog.PermitEviction("b", live, true); err != nil {
		t.Fatalf("a forced eviction was refused: %v", err)
	}
}
