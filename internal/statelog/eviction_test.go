package statelog_test

import (
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/coord"
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

// A LIVE LEASE REFUSES AN EVICTION.
//
// A node renewing its presence lease is reaching the fleet and almost always
// running, and an eviction drops every record it writes on every node and
// moves its seats: the gesture is for a node that is not coming back, and a
// live lease is the fleet's own evidence that this one is. The refusal is what
// stops a mistyped node id taking a healthy machine out.
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

// A NODE THE FLOOR HAS PASSED IS NOT READMITTED, and every branch of the
// judgement is reachable without a fleet.
//
// The operator documentation promised this refusal and nothing implemented it:
// `crewlet retention readmit` wrote the inverse record for any node, including
// one still offline at a position the log had long since trimmed — which put
// back exactly the pin the eviction had been run to lift, and reported
// success. The boundary is [statelog.Replayable]'s, against the HIGHER of the
// published floor and the log's first surviving sequence, because that is the
// bound the fence the readmitted node becomes subject to reads.
func TestAReadmissionIsRefusedBelowTheFloor(t *testing.T) {
	t.Parallel()
	reported := time.Date(2031, 4, 2, 3, 14, 0, 0, time.UTC)
	// THE TWO WITNESSES CARRY ONE DOMAIN EACH, so a judgement that read only
	// one of them fails a case below: the tracker's floor is ahead of its
	// stream, and the pages floor is unpublished while its stream has been
	// purged.
	bounds := []statelog.ReadmissionBound{
		{Domain: "tracker", Generation: 2, Floor: 9_000, First: 8_800},
		{Domain: "pages", Generation: 2, Floor: 0, First: 500},
	}
	row := func(node string, domains map[string]coord.DomainPosition) coord.NodePositions {
		return coord.NodePositions{NodeID: node, At: reported, Domains: domains}
	}
	at := func(gen uint32, seq uint64) coord.DomainPosition {
		return coord.DomainPosition{Generation: gen, Seq: seq, AppliedThrough: seq}
	}
	// ANOTHER NODE, FAR AHEAD, in every register below: a judgement that
	// read the wrong row, or the fleet's minimum, would clear everybody.
	peer := row("node-1", map[string]coord.DomainPosition{
		"tracker": at(2, 12_000), "pages": at(2, 900),
	})

	for name, tc := range map[string]struct {
		register    []coord.NodePositions
		bounds      []statelog.ReadmissionBound
		refused     string // the domain a refusal names; empty for a permit
		published   bool
		mentions    []string // what the operator's sentence must carry
		unavailable bool
	}{
		"a node that has applied every record up to the one before each floor": {
			register: []coord.NodePositions{peer, row("node-4", map[string]coord.DomainPosition{
				"tracker": at(2, 8_999), "pages": at(2, 499),
			})},
		},
		"the published floor carries a stream that has not caught up with it": {
			register: []coord.NodePositions{peer, row("node-4", map[string]coord.DomainPosition{
				"tracker": at(2, 8_998), "pages": at(2, 900),
			})},
			refused: "tracker", published: true,
			mentions: []string{"is 8998", "below 9000", "published floor 9000",
				"first surviving sequence 8800"},
		},
		"the stream's first sequence carries a floor nobody has published": {
			register: []coord.NodePositions{peer, row("node-4", map[string]coord.DomainPosition{
				"tracker": at(2, 12_000), "pages": at(2, 498),
			})},
			refused: "pages", published: true,
			mentions: []string{"is 498", "below 500", "published floor 0",
				"first surviving sequence 500"},
		},
		"a position from a generation the log has left": {
			register: []coord.NodePositions{peer, row("node-4", map[string]coord.DomainPosition{
				"tracker": at(1, 99_999), "pages": at(2, 900),
			})},
			refused: "tracker", published: true,
			mentions: []string{"99999 at generation 1", "at generation 2"},
		},
		"a position from a generation this node has not reached is not judged": {
			register: []coord.NodePositions{peer, row("node-4", map[string]coord.DomainPosition{
				"tracker": at(3, 1), "pages": at(2, 900),
			})},
			unavailable: true,
		},
		"a node that has never published, against a log that has lost records": {
			register: []coord.NodePositions{peer},
			refused:  "tracker",
			mentions: []string{"never published", "below 9000"},
		},
		"a node that has never published, against logs that have lost nothing": {
			register: []coord.NodePositions{peer},
			bounds: []statelog.ReadmissionBound{
				{Domain: "tracker", Generation: 2, Floor: 0, First: 1},
				{Domain: "pages", Generation: 2, Floor: 0, First: 0},
			},
		},
		"a domain the node does not run is not a domain it is at zero in": {
			register: []coord.NodePositions{peer, row("node-4", map[string]coord.DomainPosition{
				"tracker": at(2, 9_000),
			})},
		},
	} {
		t.Run(name, func(t *testing.T) {
			judged := bounds
			if tc.bounds != nil {
				judged = tc.bounds
			}
			err := statelog.PermitReadmission("node-4", tc.register, judged)
			var refusal *statelog.ReadmissionRefusal
			switch {
			case tc.unavailable:
				if !errors.Is(err, statelog.ErrUnavailable) || errors.As(err, &refusal) {
					t.Fatalf("err = %v, want %v and no refusal — a position this "+
						"node cannot compare is not a fault in the node", err,
						statelog.ErrUnavailable)
				}
				return
			case tc.refused == "":
				if err != nil {
					t.Fatalf("a node that can replay what it lacks was refused: %v", err)
				}
				return
			}
			if !errors.As(err, &refusal) {
				t.Fatalf("err = %v, want a readmission refusal naming %s", err, tc.refused)
			}
			if refusal.NodeID != "node-4" || refusal.Domain != tc.refused ||
				refusal.Published != tc.published {
				t.Fatalf("refused %s in %s (published %v), want node-4 in %s "+
					"(published %v)", refusal.NodeID, refusal.Domain,
					refusal.Published, tc.refused, tc.published)
			}
			// THE INEQUALITY IS THE REASON, so both sides of it are in the
			// sentence an operator reads.
			for _, want := range append([]string{"node-4", tc.refused}, tc.mentions...) {
				if !strings.Contains(refusal.Error(), want) {
					t.Errorf("the refusal never mentions %q: %s", want, refusal)
				}
			}
			// WHERE TO LOOK, in no surface's vocabulary: the retention
			// report, which the command line prints and the dashboard draws.
			if remedy := refusal.Remedy(); !strings.Contains(remedy.Detail, "snapshots") ||
				!slices.Equal(remedy.Actions, []statelog.GateAction{statelog.GateWait}) {
				t.Errorf("the remedy does not say to wait and where to look: %+v", remedy)
			}
		})
	}

	if err := statelog.PermitReadmission("", []coord.NodePositions{peer}, bounds); err == nil {
		t.Fatal("a readmission naming no node was permitted")
	}
}
