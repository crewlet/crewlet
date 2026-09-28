package partmap

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/membership"
	"github.com/crewlet/crewlet/internal/placement"
	"github.com/crewlet/crewlet/internal/statelog"
)

// A PARTITION'S SEED IS PINNED, and it is its NAME's: FNV-1a over
// "partition", a NUL and the name, each value reproduced outside this code.
// Two builds on one fleet must draw every partition identically, and a seed
// built from the partition's position would re-seed every partition sorted
// after a space a later layout adds.
func TestAPartitionsSeedIsItsNamesAndPinned(t *testing.T) {
	t.Parallel()
	for name, want := range map[string]uint64{
		"tracker.000": 0x5bb004dcc1161093,
		"tracker.007": 0x5bb009dcc1161912,
		"pages.063":   0x1335b218077699d8,
		"company.000": 0xbb37c26ff1e3cfec,
	} {
		p, err := statelog.ParsePartitionID(name)
		if err != nil {
			t.Fatal(err)
		}
		if got := SeedOf(p); got != want {
			t.Errorf("SeedOf(%s) = %#x, pinned %#x", name, got, want)
		}
	}
	// The same partition in a layout with another space before it keeps
	// its seed, and so its group's seed.
	small, big := testLayout(8, 2), testLayout(8, 64)
	for _, l := range []statelog.Layout{small, big} {
		p := statelog.PartitionID{Space: statelog.SpaceTracker, Index: 7}
		g, ok := groupOf(l, p)
		if !ok {
			t.Fatalf("layout %v has no %s", l, p)
		}
		if got := GroupsOf(l).Seed(g); got != 0x5bb009dcc1161912 {
			t.Errorf("tracker.007's group in a layout of %d partitions draws %#x",
				len(l.Partitions()), got)
		}
	}
}

// A PARTITION'S GROUP IS ITS PLACE IN THE LAYOUT'S ORDER, computed rather than
// searched, for every partition of the owner's layout — and a partition the
// layout does not have has none.
func TestAPartitionsGroupIsItsPlaceInTheLayoutsOrder(t *testing.T) {
	t.Parallel()
	for i, p := range ownerLayout.Partitions() {
		if g, ok := groupOf(ownerLayout, p); !ok || g != i {
			t.Fatalf("%s is group %d (%v), and the layout lists it %d", p, g, ok, i)
		}
	}
	for _, p := range []statelog.PartitionID{
		{Space: statelog.SpaceTracker, Index: 256},
		{Space: statelog.SpacePages, Index: 64},
		{Space: statelog.SpaceEstate},
		{},
	} {
		if g, ok := groupOf(ownerLayout, p); ok {
			t.Errorf("%v, which the owner's layout does not have, is group %d", p, g)
		}
	}
}

// pinnedMap is a map exercising every feature that changes a target — unequal
// weights, a share a balance moved, a failure domain with a member missing its
// label, an out member, and a move — at the owner's layout.
func pinnedMap() Map {
	member := func(node string, weight int, domain string) placement.Member {
		return placement.Member{Node: node, Weight: weight, Share: placement.DefaultShare(weight),
			Domain: domain}
	}
	drifted := member("data-b", 1, "zone-a")
	drifted.Share += 12345
	out := member("data-f", 2, "zone-b")
	out.Out = true
	m := Map{
		Generation: uuid.MustParse("00000000-0000-4000-8000-000000000001"),
		Epoch:      1, Layout: ownerLayout, Replicas: 3, FailureDomain: "zone",
		Members: []placement.Member{member("data-a", 1, "zone-a"), drifted,
			member("data-c", 2, "zone-b"), member("data-d", 1, "zone-c"),
			member("data-e", 4, ""), out, member("data-g", 1, "zone-c")},
		Partitions: emptyTables(ownerLayout),
		Moves: map[string]map[string]membership.Gesture{
			"tracker.007": {"data-a": {By: "op"}},
		},
	}
	return m
}

// THE ESTATE MAP'S TARGETS ARE PINNED ACROSS BUILDS: every partition's target
// and whole ranking at the owner's layout. Two builds on one fleet during a
// rolling upgrade must place every partition identically, or each would join
// what the other retires; anything that would re-place a partition — the
// seed, the salt, the groups' order, how a move is drawn around — fails here
// rather than in a fleet. The draw's own pieces are internal/placement's pins.
func TestTheEstateMapsTargetsArePinned(t *testing.T) {
	t.Parallel()
	m := pinnedMap()
	if err := m.Validate(); err != nil {
		t.Fatal(err)
	}
	h := sha256.New()
	targets := m.targets()
	for g, p := range ownerLayout.Partitions() {
		if !slices.Equal(targets[g], m.Target(p)) {
			t.Fatalf("%s: the targets computed at once say %v and one at a time %v",
				p, targets[g], m.Target(p))
		}
		fmt.Fprintf(h, "%s:%s|%s\n", p, strings.Join(targets[g], ","),
			strings.Join(m.Draw().Ranked(g), ","))
	}
	const pinned = "cc669914974924480f7ff1be29747fa08ea85e8f62448288387e2e9c6bc17a3e"
	if got := hex.EncodeToString(h.Sum(nil)); got != pinned {
		t.Errorf("the owner's layout places to digest %s, pinned %s", got, pinned)
	}
}

// THE ESTATE MAP DRAWS UNDER ITS OWN SALT, so the same nodes rank
// independently in it and in the object map even where a partition's seed and
// an object group's coincide: the node that is the primary of the most
// partitions is not thereby the primary of the most object groups, and one
// failure does not cost both maps their most.
func TestTheEstateMapDrawsUnderItsOwnSalt(t *testing.T) {
	t.Parallel()
	m := pinnedMap()
	m.Moves = nil
	d := m.Draw()
	if d.Salt != placement.EstateSalt {
		t.Fatalf("the estate map draws under %q", d.Salt)
	}
	objects := d
	objects.Salt = placement.ObjectSalt
	same := 0
	groups := d.Groups.Count()
	for g := range groups {
		if d.Ranked(g)[0] == objects.Ranked(g)[0] {
			same++
		}
	}
	// Six placeable members: independent draws agree on the primary
	// about a sixth of the time, weights aside. Identical draws would
	// agree every time.
	if same > groups/2 {
		t.Fatalf("the estate map and a map under the object salt agree on %d of %d "+
			"partitions' primaries: the salts do not separate them", same, groups)
	}
	t.Logf("the two salts agree on %d of %d primaries", same, groups)
}

// A MOVE KEEPS THE PARTITION'S COPIES AND ITS SPREAD: the target of a
// partition moved off a node is drawn without it, so it still has the map's
// copies, still one per failure domain where there are domains enough, and
// the node is in no other partition's target the less for it.
func TestAMoveKeepsThePartitionsCopiesAndItsSpread(t *testing.T) {
	t.Parallel()
	m := pinnedMap()
	p := statelog.PartitionID{Space: statelog.SpaceTracker, Index: 7}
	target := m.Target(p)
	if slices.Contains(target, "data-a") {
		t.Fatalf("%s is moved off data-a and its target %v names it", p, target)
	}
	if len(target) != m.Size() {
		t.Fatalf("%s's target %v has %d copies, want the map's %d", p, target, len(target), m.Size())
	}
	domains := map[string]bool{}
	for _, node := range target {
		member, _ := m.Draw().Member(node)
		if member.Domain != "" && domains[member.Domain] {
			t.Fatalf("%s's target %v puts two copies in %s", p, target, member.Domain)
		}
		domains[member.Domain] = true
	}
	unmoved := m
	unmoved.Moves = nil
	for _, q := range ownerLayout.Partitions() {
		if q != p && !slices.Equal(m.Target(q), unmoved.Target(q)) {
			t.Fatalf("moving %s off data-a changed %s's target", p, q)
		}
	}
}

// SERVING IS THE ROUTING ORDER: a partition's servers, its target's first in
// the target's order, then the rest in its ranking — never a joiner or a
// leaver, which a router must not send the partition's requests to.
func TestServingIsInRoutingOrder(t *testing.T) {
	t.Parallel()
	m := pinnedMap()
	m.Moves = nil
	p := statelog.PartitionID{Space: statelog.SpacePages, Index: 3}
	g, _ := groupOf(ownerLayout, p)
	target := m.Target(p)
	rest := slices.DeleteFunc(m.Draw().Ranked(g), func(n string) bool { return slices.Contains(target, n) })
	holders := []Holder{
		{Node: target[1], State: Serving, Since: 1}, {Node: target[0], State: Joining, Since: 1},
		{Node: target[2], State: Serving, Since: 1}, {Node: rest[0], State: Serving, Since: 1},
		{Node: rest[1], State: Leaving, Since: 1},
	}
	sortHolders(holders)
	m.Partitions[g].Holders = holders
	want := []string{target[1], target[2], rest[0]}
	if got := m.Serving(p); !slices.Equal(got, want) {
		t.Fatalf("Serving(%s) = %v, want %v", p, got, want)
	}
	if got := m.Serving(statelog.PartitionID{Space: statelog.SpaceTracker, Index: 999}); got != nil {
		t.Fatalf("a partition the layout does not have is served by %v", got)
	}
}

// A MAP NOBODY MAY PLACE BY IS REFUSED, naming what is wrong with it: a map
// read off the store by a node that cannot place by it must say so rather
// than route by it.
func TestAMapNobodyMayPlaceByIsRefused(t *testing.T) {
	t.Parallel()
	valid := pinnedMap()
	valid.Partitions[0].Holders = []Holder{{Node: "data-a", State: Serving, Since: 1}}
	if err := valid.Validate(); err != nil {
		t.Fatalf("the valid map was refused: %v", err)
	}
	zero := statelog.Layout{Number: 0, Spaces: []statelog.SpaceLayout{
		{Space: statelog.SpaceEstate, Partitions: 1, Domains: []string{"tracker"}}}}
	for name, c := range map[string]struct {
		change func(*Map)
		says   string
	}{
		"no generation": {func(m *Map) { m.Generation = uuid.Nil }, "generation"},
		"epoch 0":       {func(m *Map) { m.Epoch = 0 }, "epoch is 0"},
		"no layout":     {func(m *Map) { m.Layout = statelog.Layout{} }, "layout"},
		"layout 0": {func(m *Map) {
			m.Layout = zero
			m.Partitions = emptyTables(zero)
		}, "layout 0"},
		"a table short": {func(m *Map) { m.Partitions = m.Partitions[1:] }, "partition tables"},
		"a table out of order": {func(m *Map) {
			m.Partitions[0], m.Partitions[1] = m.Partitions[1], m.Partitions[0]
		}, "partition table 0"},
		"holders unsorted": {func(m *Map) {
			m.Partitions[0].Holders = []Holder{{Node: "data-b", State: Serving, Since: 1},
				{Node: "data-a", State: Serving, Since: 1}}
		}, "not sorted"},
		"a state unknown": {func(m *Map) { m.Partitions[0].Holders[0].State = "resting" }, "resting"},
		"a holder not a member": {func(m *Map) {
			m.Partitions[0].Holders[0].Node = "data-z"
		}, "not a member"},
		"a holder from the future": {func(m *Map) { m.Partitions[0].Holders[0].Since = 2 }, "outside 1..1"},
		"a holder from nowhere":    {func(m *Map) { m.Partitions[0].Holders[0].Since = 0 }, "outside 1..1"},
		"a move of nothing": {func(m *Map) {
			m.Moves["tracker.300"] = map[string]membership.Gesture{"data-a": {}}
		}, "tracker.300"},
		"a move of nobody": {func(m *Map) {
			m.Moves["tracker.001"] = map[string]membership.Gesture{}
		}, "name no node"},
		"a move of a stranger": {func(m *Map) {
			m.Moves["tracker.001"] = map[string]membership.Gesture{"data-z": {}}
		}, "data-z"},
		"no replicas": {func(m *Map) { m.Replicas = 0 }, "replicas"},
	} {
		m := valid.Clone()
		c.change(&m)
		err := m.Validate()
		switch {
		case err == nil:
			t.Errorf("%s: accepted", name)
		case !errors.Is(err, placement.ErrInvalid):
			t.Errorf("%s: %v is not placement.ErrInvalid", name, err)
		case !strings.Contains(err.Error(), c.says):
			t.Errorf("%s: %q does not say %q", name, err, c.says)
		}
	}
}

// A READER IGNORES WHAT A NEWER BUILD ADDED, AND A WRITER REFUSES IT: a reader
// that stopped routing for a field it did not know would stop its node's
// remote reads for a whole upgrade, and a writer that rewrote the record in
// its own shape would silently drop what the newer build added. And a record
// is stored only if it validates.
func TestTheRecordIsReadLeniently(t *testing.T) {
	t.Parallel()
	s := newSim(t, smallLayout, 2, nodeIDs(3)...)
	s.settle(100)
	raw, err := s.state.Encode()
	if err != nil {
		t.Fatal(err)
	}
	back, err := DecodeMapStateForUpdate(raw)
	if err != nil {
		t.Fatalf("a record this build wrote does not decode for an update: %v", err)
	}
	if !sameState(back, s.state) {
		t.Fatal("the record did not round-trip")
	}

	newer := strings.Replace(string(raw), `{"map":{`, `{"map":{"quorum_hint":2,`, 1)
	if _, err := DecodeMapState([]byte(newer)); err != nil {
		t.Fatalf("a reader refused a newer build's record: %v", err)
	}
	if _, err := DecodeMapStateForUpdate([]byte(newer)); err == nil {
		t.Fatal("a writer accepted a record carrying a field it would drop")
	}
	if _, err := DecodeMapStateForUpdate(append(raw, raw...)); err == nil {
		t.Fatal("a writer accepted data after the record")
	}

	broken := s.state.Clone()
	broken.Map.Epoch = 0
	if _, err := broken.Encode(); err == nil {
		t.Fatal("a record with no epoch was encoded")
	}
	probation := s.state.Clone()
	probation.Map.Members[0].Probation = true
	if _, err := probation.Encode(); err == nil {
		t.Fatal("a record whose map and memory disagree about probation was encoded")
	}
}

// A PARTITION'S HOLDERS ARE EVERY STATE: the trim counts a joining node's tail
// as surely as a server's, since the joiner is exactly the node whose tail
// must not be trimmed from under it. The table handed out is the caller's own.
func TestHoldersOfIsEveryStateAndTheCallersOwn(t *testing.T) {
	t.Parallel()
	m := pinnedMap()
	m.Moves = nil
	p := statelog.PartitionID{Space: statelog.SpaceCompany}
	g, _ := groupOf(ownerLayout, p)
	m.Partitions[g].Holders = []Holder{{Node: "data-a", State: Joining, Since: 1},
		{Node: "data-b", State: Serving, Since: 1}, {Node: "data-c", State: Leaving, Since: 1}}
	got := m.HoldersOf(p)
	if !slices.Equal(got, m.Partitions[g].Holders) {
		t.Fatalf("HoldersOf(%s) = %+v, want every holder in every state", p, got)
	}
	got[0].State = Serving
	if m.Partitions[g].Holders[0].State != Joining {
		t.Fatal("changing the holders handed out changed the map")
	}
	if got := m.HoldersOf(statelog.PartitionID{Space: statelog.SpacePages, Index: 64}); got != nil {
		t.Fatalf("a partition the layout does not have is held by %+v", got)
	}
}
