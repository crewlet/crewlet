package placement

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"slices"
	"testing"
)

func members(weights map[string]int) []Member {
	var out []Member
	for node, w := range weights {
		out = append(out, Member{Node: node, Weight: w})
	}
	slices.SortFunc(out, func(a, b Member) int {
		switch {
		case a.Node < b.Node:
			return -1
		case a.Node > b.Node:
			return 1
		}
		return 0
	})
	return out
}

// THE ALGORITHM IS A CONTRACT BETWEEN BUILDS. Two builds on one fleet during a
// rolling upgrade must place every group identically, or each asks the other
// for an object neither stores — so these holders are pinned, and a change to
// the hash, the draw or the tie-break fails here rather than in a fleet.
func TestPlacementIsPinnedAcrossBuilds(t *testing.T) {
	t.Parallel()
	m := Map{Epoch: 1, Replicas: 3, Members: members(map[string]int{
		"data-a": 1, "data-b": 1, "data-c": 2, "data-d": 1,
	})}
	got := map[int][]string{}
	for _, pg := range []int{0, 1, 17, 128, 255} {
		got[pg] = m.Up(pg)
	}
	want := pinnedLayout
	for pg, holders := range want {
		if !slices.Equal(got[pg], holders) {
			t.Errorf("group %d is held by %v, pinned %v — a placement change "+
				"re-places every object and two builds on one fleet disagree "+
				"about all of them", pg, got[pg], holders)
		}
	}
}

// ONE MAP, ONE LAYOUT, however many times it is computed.
func TestALayoutIsAPureFunctionOfTheMap(t *testing.T) {
	t.Parallel()
	m := Map{Replicas: 2, Members: members(map[string]int{"a": 1, "b": 3, "c": 1})}
	if moved := Moved(m.Layout(), m.Layout()); len(moved) != 0 {
		t.Fatalf("the same map laid out twice disagreed on groups %v", moved)
	}
}

// EQUAL WEIGHTS SHARE THE GROUPS EVENLY, within what hashing 256 groups can
// promise: every member primary for a fair share, give or take.
func TestEqualWeightsShareTheGroups(t *testing.T) {
	t.Parallel()
	m := Map{Replicas: 1, Members: members(map[string]int{"a": 1, "b": 1, "c": 1, "d": 1})}
	counts := map[string]int{}
	for pg := range PGCount {
		counts[m.Up(pg)[0]]++
	}
	for node, n := range counts {
		// 64 expected; a binomial(256, 1/4) is within ±22 of it with
		// overwhelming probability, and the draw here is fixed anyway.
		if n < 42 || n > 86 {
			t.Errorf("%s is primary for %d groups, want about 64", node, n)
		}
	}
}

// A WEIGHT IS A SHARE: a member of weight three is primary about three times
// as often as a member of weight one, over enough groups to see it.
func TestAWeightIsAShare(t *testing.T) {
	t.Parallel()
	heavy, light := Member{Node: "heavy", Weight: 3}, Member{Node: "light", Weight: 1}
	wins := 0
	const trials = 20000
	for pg := range trials {
		if bestDraw(pg, heavy) > bestDraw(pg, light) {
			wins++
		}
	}
	// 3/4 of 20000 is 15000; the standard deviation is about 61.
	if wins < 14600 || wins > 15400 {
		t.Fatalf("the weight-3 member won %d of %d, want about 15000", wins, trials)
	}
}

// ADDING A MEMBER MOVES ONLY WHAT IT NOW HOLDS, and removing one moves only
// what it held — the two properties that make a map change copy the least
// data it can. A placement that reshuffled groups between members that were
// there before and after would re-replicate the company for every node that
// joined.
func TestAMapChangeMovesOnlyTheGroupsItMust(t *testing.T) {
	t.Parallel()
	before := Map{Replicas: 3, Members: members(map[string]int{"a": 1, "b": 1, "c": 1, "d": 1})}
	after := Map{Replicas: 3, Members: members(map[string]int{"a": 1, "b": 1, "c": 1, "d": 1, "e": 1})}
	lb, la := before.Layout(), after.Layout()
	moved := Moved(lb, la)
	if len(moved) == 0 {
		t.Fatal("a fifth member took no group at all")
	}
	for _, pg := range moved {
		for _, node := range la[pg] {
			if node != "e" && !slices.Contains(lb[pg], node) {
				t.Errorf("group %d gained %s, which held it neither before nor is new", pg, node)
			}
		}
		if !slices.Contains(la[pg], "e") {
			t.Errorf("group %d moved without the new member taking it: %v -> %v",
				pg, lb[pg], la[pg])
		}
	}
	// AND THE OTHER WAY: removing e puts every group back exactly.
	if back := Moved(la, lb); !slices.Equal(back, moved) {
		t.Errorf("removing the member moved %d groups, adding it moved %d", len(back), len(moved))
	}
}

// A FLEET WITH FEWER MEMBERS THAN REPLICAS HOLDS EACH OBJECT ON ALL OF THEM.
func TestReplicasAreBoundedByMembers(t *testing.T) {
	t.Parallel()
	m := Map{Replicas: 3, Members: members(map[string]int{"solo": 1})}
	if got := m.Up(7); !slices.Equal(got, []string{"solo"}) {
		t.Fatalf("a one-member map placed a group on %v", got)
	}
	if got := (Map{Replicas: 3}).Up(7); len(got) != 0 {
		t.Fatalf("an empty map placed a group on %v", got)
	}
}

func TestAMapThatCannotPlaceIsRefused(t *testing.T) {
	t.Parallel()
	for name, m := range map[string]Map{
		"no replicas":   {Members: []Member{{Node: "a", Weight: 1}}},
		"an empty node": {Replicas: 1, Members: []Member{{Node: "", Weight: 1}}},
		"weight zero":   {Replicas: 1, Members: []Member{{Node: "a", Weight: 0}}},
		"weight past the cap": {Replicas: 1,
			Members: []Member{{Node: "a", Weight: MaxWeight + 1}}},
		"out of order": {Replicas: 1,
			Members: []Member{{Node: "b", Weight: 1}, {Node: "a", Weight: 1}}},
		"a duplicate": {Replicas: 1,
			Members: []Member{{Node: "a", Weight: 1}, {Node: "a", Weight: 1}}},
	} {
		if err := m.Validate(); !errors.Is(err, ErrInvalid) {
			t.Errorf("%s: Validate = %v", name, err)
		}
	}
}

// A GROUP IS THE CONTENT HASH'S LEADING BYTES, so it is uniform because the
// hash is — and identical for one object everywhere.
func TestAGroupIsTheHashsLeadingBytes(t *testing.T) {
	t.Parallel()
	seen := map[int]int{}
	for i := range 4096 {
		sum := sha256.Sum256(fmt.Appendf(nil, "object-%d", i))
		seen[PG(sum[:])]++
	}
	if len(seen) < PGCount*9/10 {
		t.Fatalf("4096 objects landed in only %d of %d groups", len(seen), PGCount)
	}
}

// pinnedLayout is the golden answer for [TestPlacementIsPinnedAcrossBuilds].
var pinnedLayout = map[int][]string{
	0:   {"data-d", "data-b", "data-c"},
	1:   {"data-d", "data-c", "data-b"},
	17:  {"data-b", "data-c", "data-d"},
	128: {"data-c", "data-a", "data-b"},
	255: {"data-c", "data-d", "data-a"},
}

// THE RANKING IS THE UP SET FOLLOWED BY EVERY OTHER MEMBER, once each — a
// writer that skips a silent holder lands on the next member of the SAME
// order a reader looks through.
func TestTheRankingStartsWithTheUpSetAndHoldsEveryMember(t *testing.T) {
	t.Parallel()
	m := Map{Replicas: 2, Members: []Member{
		{Node: "a", Weight: 1}, {Node: "b", Weight: 2}, {Node: "c", Weight: 1},
		{Node: "d", Weight: 3},
	}}
	for pg := range PGCount {
		ranked := m.Ranked(pg)
		if !slices.Equal(ranked[:m.Size()], m.Up(pg)) {
			t.Fatalf("group %d: ranking %v does not start with the up set %v",
				pg, ranked, m.Up(pg))
		}
		sorted := slices.Sorted(slices.Values(ranked))
		if !slices.Equal(sorted, []string{"a", "b", "c", "d"}) {
			t.Fatalf("group %d: ranking %v is not every member once", pg, ranked)
		}
	}
}

// A QUORUM IS A MAJORITY OF THE COPIES THE MAP ASKS FOR, bounded by the
// members there are.
func TestAQuorumIsAMajorityOfTheCopies(t *testing.T) {
	t.Parallel()
	members := func(n int) []Member {
		out := make([]Member, n)
		for i := range out {
			out[i] = Member{Node: string(rune('a' + i)), Weight: 1}
		}
		return out
	}
	for _, c := range []struct{ replicas, members, want int }{
		{1, 1, 1}, {2, 2, 2}, {3, 3, 2}, {3, 5, 2}, {5, 5, 3},
		// Bounded by the members: three copies asked of two nodes is two.
		{3, 2, 2},
		{3, 1, 1},
	} {
		m := Map{Replicas: c.replicas, Members: members(c.members)}
		if got := m.Quorum(); got != c.want {
			t.Errorf("replicas %d over %d members: quorum %d, want %d",
				c.replicas, c.members, got, c.want)
		}
	}
}
