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

	fresh := statelog.CountedSet(now, "tracker", reported, nil, []statelog.Tombstone{
		{NodeID: "b", At: now.Add(-time.Second), By: "operator"},
	})
	if len(fresh) != 2 {
		t.Fatalf("a node evicted a second ago is already uncounted (%d counted) "+
			"— it has not had a heartbeat in which to read its own tombstone, "+
			"and until it does it is still writing", len(fresh))
	}

	settled := statelog.CountedSet(now, "tracker", reported, nil, []statelog.Tombstone{
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
	counted := statelog.CountedSet(now, "tracker",
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

// A LIVE NODE THAT DOES NOT RUN THIS DOMAIN IS NOT COUNTED FOR IT, which is
// the opposite disposition from the joiner above and turns on the same
// silence.
//
// A node publishes a position for every domain it applies and none for the
// rest. Read as a joiner, a satellite whose roles exclude a domain would be
// unioned in at zero on every tick for as long as it lives — so that domain's
// log would never trim and would grow without bound on any fleet with one.
func TestALiveNodeThatDoesNotRunADomainIsNotCountedForIt(t *testing.T) {
	t.Parallel()
	now := time.Now()
	reported := []statelog.NodePosition{{NodeID: "ingress", Generation: 1, Seq: 9_000}}
	satellite := statelog.Presence{NodeID: "satellite", Domains: []string{"tracker"}}

	counted := statelog.CountedSet(now, "people", reported,
		[]statelog.Presence{satellite}, nil)
	if len(counted) != 1 || counted[0].NodeID != "ingress" {
		t.Errorf("counted %v for a domain the satellite does not run, want only "+
			"the node that does — counted at zero it pins this log's floor for "+
			"as long as it lives", counted)
	}

	// THE SAME NODE IS COUNTED FOR A DOMAIN IT DOES RUN, which is what
	// says the clause above is a filter rather than a node being dropped.
	counted = statelog.CountedSet(now, "tracker", reported,
		[]statelog.Presence{satellite}, nil)
	if len(counted) != 2 {
		t.Errorf("counted %v for a domain the satellite runs, want both", counted)
	}

	// AND A PEER THAT NAMES NO DOMAINS IS COUNTED FOR EVERY ONE. That row
	// is what a build predating the field writes, and it is exactly what a
	// rolling upgrade puts in front of the new nodes; reading it as "runs
	// nothing" would trim past a peer that is still applying.
	counted = statelog.CountedSet(now, "people", reported,
		[]statelog.Presence{{NodeID: "older-build"}}, nil)
	if len(counted) != 2 {
		t.Errorf("counted %v for a peer that names no domains, want both — a "+
			"presence row with no domain list is a build that predates the "+
			"field, not a node that applies nothing", counted)
	}
}
