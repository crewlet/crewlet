package placement

import (
	"cmp"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"math"
	"slices"
	"strings"
	"testing"

	"github.com/google/uuid"
)

// generation is the lineage every test map carries.
var generation = uuid.MustParse("5f0c7a52-8e1d-4c3b-9a64-0d2e7f1b3c85")

// mapOf is a valid map of these members, sorted.
func mapOf(pgBits, replicas int, failureDomain string, members ...Member) Map {
	slices.SortFunc(members, func(a, b Member) int {
		switch {
		case a.Node < b.Node:
			return -1
		case a.Node > b.Node:
			return 1
		}
		return 0
	})
	return Map{Generation: generation, Epoch: 1, Replicas: replicas, PGBits: pgBits,
		FailureDomain: failureDomain, Members: members}
}

// member is a node of this weight at its default share, in this domain.
func member(node string, weight int, domain string) Member {
	return Member{Node: node, Weight: weight, Share: DefaultShare(weight), Domain: domain}
}

// fleet is n equal members, data-000 onwards.
func fleet(n, pgBits, replicas int) Map {
	members := make([]Member, n)
	for i := range members {
		members[i] = member(fmt.Sprintf("data-%03d", i), 1, "")
	}
	return mapOf(pgBits, replicas, "", members...)
}

// goldenMap is the map TestPlacementIsPinnedAcrossBuilds pins: every feature
// that changes a ranking — unequal weights, a share the balancer moved, a
// failure domain with a member missing its label, an out member.
func goldenMap() Map {
	drifted := member("data-b", 1, "zone-a")
	drifted.Share = DefaultShare(1) + 12345
	drained := member("data-f", 2, "zone-b")
	drained.Out = true
	return mapOf(10, 3, "zone",
		member("data-a", 1, "zone-a"), drifted, member("data-c", 2, "zone-b"),
		member("data-d", 1, "zone-c"), member("data-e", 4, ""), drained)
}

// THE ALGORITHM IS A CONTRACT BETWEEN BUILDS. Two builds on one fleet during a
// rolling upgrade must place every group identically, or each asks the other
// for a chunk neither stores — so these holders are pinned, and a change to
// the seed, the node key, the mixer, the logarithm, the division, the
// tie-break, the domain walk or the order of the tail fails here rather than
// in a fleet.
func TestPlacementIsPinnedAcrossBuilds(t *testing.T) {
	t.Parallel()
	m := goldenMap()
	if err := m.Validate(); err != nil {
		t.Fatalf("the pinned map does not validate: %v", err)
	}
	l := m.Layout()
	for pg, want := range pinnedUp {
		if got := l.Up(pg); !slices.Equal(got, want) {
			t.Errorf("group %d is held by %v, pinned %v — a placement change "+
				"re-places every chunk and two builds on one fleet disagree "+
				"about all of them", pg, got, want)
		}
	}
	if got := m.Ranked(5); !slices.Equal(got, pinnedRanked5) {
		t.Errorf("group 5 ranks %v, pinned %v", got, pinnedRanked5)
	}
}

// EVERY GROUP IS PINNED, not only the ones listed: a digest of the whole
// layout of the pinned map and of a map that needs the fill pass, so a change
// that happens to spare the groups above still fails here.
func TestEveryGroupIsPinnedAcrossBuilds(t *testing.T) {
	t.Parallel()
	for name, c := range map[string]struct {
		m      Map
		digest string
	}{
		"the pinned map": {goldenMap(),
			"e6fca59ad58499e444b2877e04527d41df035e4acce7c1474fbbed590d2c8eed"},
		"a domain-limited map": {limitedMap(),
			"5399a5dd9e562038b99101b194925bb8a18441b48f16b230bd2ed7683bea0001"},
	} {
		if err := c.m.Validate(); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		l := c.m.Layout()
		h := sha256.New()
		for pg := range c.m.Groups() {
			fmt.Fprintf(h, "%d:%s\n", pg, strings.Join(l.Up(pg), ","))
		}
		if got := hex.EncodeToString(h.Sum(nil)); got != c.digest {
			t.Errorf("%s: the layout's digest is %s, pinned %s", name, got, c.digest)
		}
	}
}

// THE DRAW'S PIECES ARE PINNED ONE BY ONE, so a failure above says which
// piece moved. Each value was reproduced outside this code: the node key is
// SHA-256("crewlet-objstore-node\x00data-a")'s first eight bytes, the mix is
// the two MurmurHash3 finalizer rounds computed independently, and the
// logarithm is floor(log2(u)·2^32) − 53·2^32 for u = (mix >> 11) + 1 (true
// value −4337083689.74).
func TestTheDrawIsPinnedPieceByPiece(t *testing.T) {
	t.Parallel()
	key := nodeKey("data-a")
	seed := seedKey(12, 10) // 12 is 0b1100 in ten bits: value 3, length 8
	for name, c := range map[string]struct{ got, want int64 }{
		"node key":  {int64(key), int64(-0x46c78516b8322914)}, // 0xb9387ae947cdd6ec
		"seed":      {int64(seed), 8<<32 | 3},
		"mix":       {int64(mix(seed, key)), 0x7f220807ec32ea22},
		"logarithm": {logDraw(seed, key), -4337083690},
		// Weight three divides the Q32 logarithm by three, truncating
		// toward zero: the shift keeps the fraction a share would
		// otherwise divide away.
		"straw2": {straw2(logDraw(seed, key), DefaultShare(3)), -1445694563},
	} {
		if c.got != c.want {
			t.Errorf("%s = %#x, pinned %#x", name, c.got, c.want)
		}
	}
}

// pinnedUp is the golden answer for [TestPlacementIsPinnedAcrossBuilds].
var pinnedUp = map[int][]string{
	0:    {"data-e", "data-a", "data-d"},
	1:    {"data-c", "data-e", "data-d"},
	2:    {"data-d", "data-a", "data-e"},
	3:    {"data-b", "data-c", "data-e"},
	5:    {"data-a", "data-e", "data-d"},
	100:  {"data-c", "data-b", "data-e"},
	511:  {"data-b", "data-c", "data-e"},
	512:  {"data-e", "data-d", "data-c"},
	1023: {"data-e", "data-a", "data-c"},
}

// pinnedRanked5 is group 5's whole ranking: its up set, the placeable member
// it passed over (data-b shares zone-a with data-a), then the rest, then the
// out member.
var pinnedRanked5 = []string{"data-a", "data-e", "data-d", "data-b", "data-c", "data-f"}

// THE LOGARITHM IS PART OF THE CONTRACT, so it is pinned on its own: exact on
// every power of two, and on other values the true log2 in Q32 rounded DOWN —
// the references are floor(log2(x) · 2^32) to fifty digits, derived outside
// this code.
func TestTheLogarithmIsPinnedAndExactOnPowersOfTwo(t *testing.T) {
	t.Parallel()
	for k := range 64 {
		if got, want := log2fixed(1<<k), int64(k)<<fracBits; got != want {
			t.Errorf("log2fixed(2^%d) = %d, want exactly %d", k, got, want)
		}
	}
	for x, want := range map[uint64]int64{
		3:           6807362105,   // 6807362105.98…
		5:           9972605231,   // 9972605231.20…
		10:          14267572527,  // 14267572527.20…
		1000000:     85605435163,  // 85605435163.22…
		1<<53 - 1:   227633266687, // 227633266687.9999993…
		1<<53 + 1:   227633266688, // a hair over 53: its fraction is below 2^-32
		12345678901: 143981421846, // 143981421846.63…
	} {
		if got := log2fixed(x); got != want {
			t.Errorf("log2fixed(%d) = %d, want %d", x, got, want)
		}
	}
	// Every bit of it, over a hundred thousand inputs of every magnitude: a
	// change too small to move the values above — one unit in the last
	// place, now and then — still ranks some group differently on a build
	// that has it.
	digest := sha256.New()
	var buf [8]byte
	x := uint64(1)
	for range 100000 {
		x = x*6364136223846793005 + 1442695040888963407
		binary.BigEndian.PutUint64(buf[:], uint64(log2fixed(x>>(x%64)|1)))
		digest.Write(buf[:])
	}
	if got, want := hex.EncodeToString(digest.Sum(nil)),
		"13fe66f7f35d906f1a907f9bad2e5c782d0e9a529743a1e12f0f36ced58d5c1e"; got != want {
		t.Errorf("the logarithm's digest is %s, pinned %s", got, want)
	}

	// And against the float reference everywhere else: never above it, and
	// never more than two units of 2^-32 below.
	x = 1
	for range 100000 {
		x = x*6364136223846793005 + 1442695040888963407
		v := x>>11 + 1
		got := float64(log2fixed(v)) / (1 << fracBits)
		ref := math.Log2(float64(v))
		if got > ref+1e-9 || got < ref-2.0/(1<<fracBits)-1e-9 {
			t.Fatalf("log2fixed(%d) = %.12f, the float reference %.12f", v, got, ref)
		}
	}
}

// THE LOGARITHM NEVER DECREASES as its argument grows — a draw that did would
// rank a larger random value below a smaller one and bias every straw.
func TestTheLogarithmNeverDecreases(t *testing.T) {
	t.Parallel()
	values := []uint64{}
	x := uint64(7)
	for range 20000 {
		x = x*6364136223846793005 + 1442695040888963407
		values = append(values, x>>11+1)
	}
	for k := range 63 {
		// Around every power of two, where the integer part turns over.
		p := uint64(1) << k
		values = append(values, p, p+1, 2*p-1)
		if p > 1 {
			values = append(values, p-1)
		}
	}
	slices.Sort(values)
	for i := 1; i < len(values); i++ {
		if log2fixed(values[i]) < log2fixed(values[i-1]) {
			t.Fatalf("log2fixed(%d) = %d is below log2fixed(%d) = %d", values[i],
				log2fixed(values[i]), values[i-1], log2fixed(values[i-1]))
		}
	}
}

// A SPLIT INHERITS BY ITS SEED: the lower child of every group draws exactly
// its parent's seed, the upper child a seed no group at any smaller count
// had, and no two groups at one count share one. The whole half-moves
// property rests on these three facts.
func TestASplitsLowerChildInheritsItsParentsSeed(t *testing.T) {
	t.Parallel()
	earlier := map[uint64]int{}
	for k := MinPGBits; k <= MaxPGBits; k++ {
		here := map[uint64]int{}
		for pg := range 1 << k {
			seed := seedKey(pg, k)
			if other, dup := here[seed]; dup {
				t.Fatalf("groups %d and %d at %d bits share seed %#x", other, pg, k, seed)
			}
			here[seed] = pg
			if k == MinPGBits {
				continue
			}
			parent := pg >> 1
			_, seenBefore := earlier[seed]
			switch {
			case pg%2 == 0 && seed != seedKey(parent, k-1):
				t.Fatalf("group %d at %d bits draws %#x, its parent %d drew %#x",
					pg, k, seed, parent, seedKey(parent, k-1))
			case pg%2 == 1 && seenBefore:
				t.Fatalf("group %d at %d bits reuses a seed from a smaller count", pg, k)
			}
		}
		for seed, pg := range here {
			earlier[seed] = pg
		}
	}
}

// A DOUBLING MOVES HALF THE DATA, NOT ALL OF IT: every lower child keeps its
// parent's holders in their order, so at most the upper half of the slots can
// move — and the upper children are drawn afresh, so about that many do,
// whatever the weights, domains and out members. A fresh draw lands on its
// parent's holders by chance, more often the fewer members there are to
// choose from, which is what each case's floor allows for.
func TestADoublingMovesAboutHalfTheData(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		name     string
		m        Map
		redrawn  float64 // the least fraction of upper children whose holders change
		slotsMin float64 // the least fraction of all slots that move
	}{
		// C(10,3) = 120 possible up sets, so nearly every fresh draw differs.
		{"ten equal members", fleet(10, 9, 3), 0.95, 0.47},
		// Five placeable members, and zone-a's two can never share a group:
		// far fewer possible up sets.
		{"the pinned map", goldenMap(), 0.7, 0.35},
	} {
		for from := c.m.PGBits; from < min(c.m.PGBits+3, MaxPGBits); from++ {
			before := c.m
			before.PGBits = from
			after := c.m
			after.PGBits = from + 1
			lb, la := before.Layout(), after.Layout()
			redrawn := 0
			for p := range before.Groups() {
				if !slices.Equal(la.Up(2*p), lb.Up(p)) {
					t.Fatalf("%s: group %d at %d bits is held by %v, its lower child by %v",
						c.name, p, from, lb.Up(p), la.Up(2*p))
				}
				if !sameMembers(la.Up(2*p+1), lb.Up(p)) {
					redrawn++
				}
			}
			if frac := float64(redrawn) / float64(before.Groups()); frac < c.redrawn {
				t.Errorf("%s: only %.2f of upper children at %d bits moved, want %.2f",
					c.name, frac, from+1, c.redrawn)
			}
			moved := 0
			for _, r := range Moved(lb, la) {
				moved += r.Len()
			}
			t.Logf("%s: %d -> %d bits: %.3f of upper children redrawn, %.3f of slots moved", c.name, from, from+1, float64(redrawn)/float64(before.Groups()), float64(moved)/Slots)
			if frac := float64(moved) / Slots; frac < c.slotsMin || frac > 0.5 {
				t.Errorf("%s: a doubling from %d bits moved %.3f of the slots, "+
					"want %.2f..0.5", c.name, from, frac, c.slotsMin)
			}
		}
	}
}

// ADDING A MEMBER MOVES ONLY WHAT IT NOW HOLDS, and removing one moves only
// what it held — the two properties that make a map change copy the least
// data it can. A placement that reshuffled groups between members that were
// there before and after would re-replicate the company for every node that
// joined. Measured here: an eleventh member joining ten at three copies over
// 1024 groups enters 266 groups' up sets (26%) and each of those changes by
// exactly that one member, so the copies moved are exactly the newcomer's —
// 8.7% of them, against the 1/11 it is entitled to.
func TestAMapChangeMovesOnlyTheGroupsItMust(t *testing.T) {
	t.Parallel()
	before := fleet(10, 10, 3)
	after := fleet(11, 10, 3)
	newcomer := "data-010"
	lb, la := before.Layout(), after.Layout()
	entered := 0
	for pg := range before.Groups() {
		was, is := lb.Up(pg), la.Up(pg)
		if !slices.Contains(is, newcomer) {
			if !slices.Equal(was, is) {
				t.Fatalf("group %d moved %v -> %v without the new member taking it", pg, was, is)
			}
			continue
		}
		entered++
		gained := 0
		for _, node := range is {
			if !slices.Contains(was, node) {
				gained++
			}
		}
		if gained != 1 {
			t.Fatalf("group %d changed by %d members, %v -> %v: only the new one "+
				"should enter", pg, gained, was, is)
		}
	}
	if entered == 0 {
		t.Fatal("an eleventh member took no group at all")
	}
	if got, want := entered, la.Copies()[newcomer]; got != want {
		t.Fatalf("the newcomer entered %d groups and holds %d", got, want)
	}
	t.Logf("the newcomer entered %d of %d groups; %.1f%% of copies moved", entered,
		before.Groups(), 100*float64(entered)/float64(after.Groups()*after.Size()))

	// AND THE OTHER WAY: removing it puts every group back exactly.
	if back, forth := Moved(la, lb), Moved(lb, la); !slices.Equal(back, forth) {
		t.Errorf("removing the member moved %v, adding it moved %v", back, forth)
	}
}

// A FAILURE DOMAIN SPREADS EVERY GROUP'S COPIES: with as many domains as
// copies, no group has two in one; with fewer, every group spans all there
// are and the map says it is limited; a member without the label is a domain
// of its own and never collides.
func TestCopiesAreSpreadAcrossFailureDomains(t *testing.T) {
	t.Parallel()
	zones := func(m Map, l *Layout, pg int) map[string]int {
		out := map[string]int{}
		for _, node := range l.Up(pg) {
			mem, _ := m.Member(node)
			d := mem.Domain
			if d == "" {
				d = "node:" + node
			}
			out[d]++
		}
		return out
	}
	var nine, six, mixed []Member
	for i := range 9 {
		nine = append(nine, member(fmt.Sprintf("n%d", i), 1+i%2, fmt.Sprintf("z%d", i%3)))
	}
	for i := range 6 {
		six = append(six, member(fmt.Sprintf("n%d", i), 1, fmt.Sprintf("z%d", i%2)))
	}
	mixed = []Member{
		member("a1", 1, "za"), member("a2", 1, "za"), member("a3", 1, "za"),
		member("u1", 1, ""), member("u2", 1, ""),
	}
	for _, c := range []struct {
		name    string
		m       Map
		spans   int
		limited bool
	}{
		{"nine members in three zones", mapOf(10, 3, "zone", nine...), 3, false},
		{"six members in two zones", mapOf(10, 3, "zone", six...), 2, true},
		{"three in one zone and two unlabelled", mapOf(10, 3, "zone", mixed...), 3, false},
	} {
		if err := c.m.Validate(); err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		if got := c.m.DomainLimited(); got != c.limited {
			t.Errorf("%s: DomainLimited = %v, want %v", c.name, got, c.limited)
		}
		if got := c.m.DistinctDomains(); got != c.spans {
			t.Errorf("%s: DistinctDomains = %d, want %d", c.name, got, c.spans)
		}
		l := c.m.Layout()
		for pg := range c.m.Groups() {
			if got := zones(c.m, l, pg); len(got) != c.spans || len(l.Up(pg)) != 3 {
				t.Fatalf("%s: group %d is held by %v across %v, want %d copies "+
					"spanning %d domains", c.name, pg, l.Up(pg), got, 3, c.spans)
			}
		}
	}
	// WITHOUT A LABEL NOTHING IS CONSTRAINED, even between members that
	// would share a domain — the map names none, so there are none.
	plain := fleet(6, 10, 3)
	if plain.DomainLimited() || plain.DistinctDomains() != 6 {
		t.Errorf("a map with no failure domain: limited %v over %d domains",
			plain.DomainLimited(), plain.DistinctDomains())
	}
}

// AN OUT MEMBER HOLDS NOTHING AND RANKS LAST: it is in no up set, it is the
// tail of every ranking (so a reader still finds the copies it has), and the
// ranking is every member once, starting with the up set.
func TestAnOutMemberHoldsNothingAndRanksLast(t *testing.T) {
	t.Parallel()
	m := fleet(5, 9, 3)
	m.Members[2].Out = true
	drained := m.Members[2].Node
	if got := m.Size(); got != 3 {
		t.Fatalf("Size = %d with four placeable members, want 3", got)
	}
	l := m.Layout()
	if got := l.Copies()[drained]; got != 0 {
		t.Fatalf("the out member holds %d groups", got)
	}
	all := make([]string, 0, len(m.Members))
	for _, mem := range m.Members {
		all = append(all, mem.Node)
	}
	for pg := range m.Groups() {
		ranked := m.Ranked(pg)
		if !slices.Equal(ranked[:m.Size()], l.Up(pg)) {
			t.Fatalf("group %d: ranking %v does not start with the up set %v",
				pg, ranked, l.Up(pg))
		}
		if ranked[len(ranked)-1] != drained {
			t.Fatalf("group %d: ranking %v does not end with the out member", pg, ranked)
		}
		if !slices.Equal(slices.Sorted(slices.Values(ranked)), all) {
			t.Fatalf("group %d: ranking %v is not every member once", pg, ranked)
		}
	}
	// And a map whose every member is out places nothing at all.
	for i := range m.Members {
		m.Members[i].Out = true
	}
	if m.Size() != 0 || len(m.Layout().Up(0)) != 0 {
		t.Fatalf("an all-out map places on %v", m.Layout().Up(0))
	}
}

// A MEMBER ON PROBATION IS PLACED EXACTLY AS AN OUT ONE: in no up set, no part
// of the copies a map places or the rate a newcomer joins at, and at the tail
// of every ranking beside the out members — where a reader and a repair look
// for the copies it held before the map removed it. The two are separate
// flags, and either alone is enough.
func TestAMemberOnProbationIsPlacedAsAnOutOne(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		out, probation, placeable bool
	}{{false, false, true}, {true, false, false}, {false, true, false}, {true, true, false}} {
		if got := (Member{Out: c.out, Probation: c.probation}).Placeable(); got != c.placeable {
			t.Errorf("out %v, probation %v: placeable %v, want %v", c.out, c.probation, got,
				c.placeable)
		}
	}

	m := fleet(6, 9, 3)
	m.Members[1].Probation = true
	m.Members[4].Out = true
	idle := []string{m.Members[1].Node, m.Members[4].Node}
	if got := m.Size(); got != 3 {
		t.Fatalf("Size = %d with four placeable members, want 3", got)
	}
	if got := len(m.Placeable()); got != 4 {
		t.Fatalf("%d placeable members, want 4", got)
	}
	if got := m.DistinctDomains(); got != 4 {
		t.Fatalf("the placeable members span %d domains, want 4", got)
	}
	l := m.Layout()
	for _, node := range idle {
		if got := l.Copies()[node]; got != 0 {
			t.Fatalf("%s holds %d groups", node, got)
		}
	}
	for pg := range m.Groups() {
		ranked := m.Ranked(pg)
		if !slices.Equal(ranked[:m.Size()], l.Up(pg)) {
			t.Fatalf("group %d: ranking %v does not start with the up set %v",
				pg, ranked, l.Up(pg))
		}
		if tail := slices.Sorted(slices.Values(ranked[len(ranked)-2:])); !slices.Equal(tail, idle) {
			t.Fatalf("group %d: ranking %v does not end with the members that take no copies %v",
				pg, ranked, idle)
		}
		if len(ranked) != len(m.Members) {
			t.Fatalf("group %d: ranking %v is not every member once", pg, ranked)
		}
	}

	// ITS SHARE IS NOBODY'S RATE: it is the share it was removed with.
	rate := mapOf(8, 3, "", member("a", 1, ""), member("b", 1, ""))
	rate.Members[0].Share, rate.Members[1].Share = 3*shareOne, 3*shareOne
	stale := member("c", 1, "")
	stale.Share, stale.Probation = 40*shareOne, true
	rate.Members = append(rate.Members, stale)
	if got, want := rate.ShareFor(1), uint32(3*shareOne); got != want {
		t.Errorf("ShareFor(1) beside a member on probation = %d, want %d", got, want)
	}

	// AND A BALANCE LEAVES IT WHERE IT IS: it has no target to be moved toward.
	balanced, _ := Balance(m, BalanceOptions{})
	if balanced.Members[1] != m.Members[1] {
		t.Fatalf("the balance moved the member on probation: %+v → %+v", m.Members[1],
			balanced.Members[1])
	}
}

// THE UP SET IS THE HEAD OF THE RANKING, in every group of a map exercising
// every rule — the cached layout and the on-demand ranking can never disagree
// about who holds a group.
func TestTheLayoutAgreesWithTheRanking(t *testing.T) {
	t.Parallel()
	m := goldenMap()
	l := m.Layout()
	for pg := range m.Groups() {
		if got, want := l.Up(pg), m.Ranked(pg)[:m.Size()]; !slices.Equal(got, want) {
			t.Fatalf("group %d: layout %v, ranking %v", pg, got, want)
		}
	}
	if got := l.Up(m.Groups()); got != nil {
		t.Fatalf("a group past the last is held by %v", got)
	}
	if got := (&Layout{}).Up(0); got != nil {
		t.Fatalf("the zero layout places group 0 on %v", got)
	}
}

// ONE MAP, ONE LAYOUT, and the comparison is by slot: nothing moves between a
// layout and itself, and against a layout of nothing every held slot has.
func TestMovedComparesBySlot(t *testing.T) {
	t.Parallel()
	m := fleet(4, 9, 2)
	if moved := Moved(m.Layout(), m.Layout()); len(moved) != 0 {
		t.Fatalf("the same map laid out twice moved %v", moved)
	}
	if moved := Moved(&Layout{}, m.Layout()); !slices.Equal(moved, []Range{{0, Slots}}) {
		t.Fatalf("from nothing, moved %v, want every slot", moved)
	}
	if moved := Moved(m.Layout(), &Layout{}); !slices.Equal(moved, []Range{{0, Slots}}) {
		t.Fatalf("to nothing, moved %v, want every slot", moved)
	}
	// A reorder of one group's holders moves no data.
	a, b := m.Layout(), m.Layout()
	b.up[3] = []string{b.up[3][1], b.up[3][0]}
	if moved := Moved(a, b); len(moved) != 0 {
		t.Fatalf("a reordered up set moved %v", moved)
	}
	// A changed one moves exactly its slots.
	b.up[3] = []string{"somebody-else", b.up[3][0]}
	lo, hi := m.SlotRange(3)
	if moved := Moved(a, b); !slices.Equal(moved, []Range{{lo, hi}}) {
		t.Fatalf("one changed group moved %v, want [%d, %d)", moved, lo, hi)
	}
}

// A SLOT IS THE ADDRESS'S FIRST TWO BYTES, and every group is a contiguous run
// of slots: the runs tile the slot space in order at every group count.
func TestAGroupIsAContiguousRunOfSlots(t *testing.T) {
	t.Parallel()
	if got := SlotOf([]byte{0xab, 0xcd, 0xef}); got != 0xabcd {
		t.Fatalf("SlotOf = %#x, want 0xabcd", got)
	}
	if got := SlotOf([]byte{0xab}); got != 0 {
		t.Fatalf("a one-byte address is slot %#x, want 0", got)
	}
	for k := MinPGBits; k <= MaxPGBits; k++ {
		m := Map{PGBits: k}
		next := 0
		for pg := range m.Groups() {
			lo, hi := m.SlotRange(pg)
			if lo != next || hi <= lo {
				t.Fatalf("group %d at %d bits holds [%d, %d), want it to start at %d",
					pg, k, lo, hi, next)
			}
			if m.GroupOf(lo) != pg || m.GroupOf(hi-1) != pg {
				t.Fatalf("group %d at %d bits: its own slots map to %d and %d",
					pg, k, m.GroupOf(lo), m.GroupOf(hi-1))
			}
			next = hi
		}
		if next != Slots {
			t.Fatalf("the groups at %d bits end at %d, want %d", k, next, Slots)
		}
	}
}

// A TIE IS BROKEN BY THE NAME, the same way everywhere: two draws colliding
// is a curiosity, two nodes ordering it differently would be a bug. Members
// are sorted by node, so the lower index is the lower name.
func TestATieIsBrokenByName(t *testing.T) {
	t.Parallel()
	if !better(cand{score: -5, i: 1}, cand{score: -5, i: 2}) ||
		better(cand{score: -5, i: 2}, cand{score: -5, i: 1}) {
		t.Fatal("a tie is not broken toward the lower name")
	}
	if !better(cand{score: -4, i: 9}, cand{score: -5, i: 1}) {
		t.Fatal("a score closer to zero does not win")
	}
}

// A SHARE IS A CHANCE OF THE TOP PLACE: at one copy, a member's primary count
// is its share over the total, to within what 65536 groups can show — a
// chi-square over four members below its 0.1% critical value.
func TestTheFirstCopyIsProportionalToShare(t *testing.T) {
	t.Parallel()
	m := mapOf(MaxPGBits, 1, "",
		member("w1", 1, ""), member("w2", 2, ""), member("w3", 3, ""), member("w4", 4, ""))
	m.Members[2].Share = DefaultShare(3) + 7777 // a share that is not a whole weight
	counts := m.Layout().Copies()
	var total float64
	for _, mem := range m.Members {
		total += float64(mem.Share)
	}
	chi := 0.0
	for _, mem := range m.Members {
		want := float64(m.Groups()) * float64(mem.Share) / total
		got := float64(counts[mem.Node])
		chi += (got - want) * (got - want) / want
	}
	if chi > 16.27 { // three degrees of freedom at p = 0.001
		t.Fatalf("primaries %v against shares — chi-square %.2f", counts, chi)
	}
}

// A FIXED GROUP COUNT CANNOT SPREAD A GROWING FLEET, and the count this
// package picks can: at 256 groups two hundred equal members leave the
// fullest at twice its fair share or worse before any balancing, while at
// [TargetPGBits] it is within a third of it — which the balancer then closes.
func TestTheGroupCountGrowsWithTheFleet(t *testing.T) {
	t.Parallel()
	fullest := func(m Map) float64 {
		most := 0
		for _, n := range m.Layout().Copies() {
			most = max(most, n)
		}
		return float64(most) / (float64(m.Groups()*m.Size()) / float64(len(m.Members)))
	}
	if got := fullest(fleet(200, MinPGBits, 3)); got < 2 {
		t.Errorf("at 256 groups the fullest of 200 holds %.2f of its share — the "+
			"motivation for growing the count has gone", got)
	}
	if got := fullest(fleet(200, TargetPGBits(200, 3), 3)); got > 1.34 {
		t.Errorf("at the target count the fullest of 200 holds %.2f of its share", got)
	}
}

func TestTheTargetGroupCountGivesEachMemberAHundredCopies(t *testing.T) {
	t.Parallel()
	for _, c := range []struct{ placeable, replicas, want int }{
		{0, 3, MinPGBits}, {1, 0, MinPGBits},
		{1, 3, MinPGBits}, {7, 3, 8}, {8, 3, 9}, {10, 3, 9},
		{50, 3, 11}, {200, 3, 13}, {1966, 3, 16}, {5000, 3, MaxPGBits},
		// More copies than members: every member holds every group
		// at the smallest count already.
		{3, 10, MinPGBits},
	} {
		if got := TargetPGBits(c.placeable, c.replicas); got != c.want {
			t.Errorf("TargetPGBits(%d, %d) = %d, want %d", c.placeable, c.replicas, got, c.want)
		}
	}
}

// A MEMBER JOINS AT THE FLEET'S RATE: its weight at the placeable members'
// mean share per unit of weight, or the default in a map with none.
func TestANewMembersShareIsTheFleetsRate(t *testing.T) {
	t.Parallel()
	m := mapOf(8, 3, "", member("a", 1, ""), member("b", 3, ""))
	m.Members[0].Share = 3 * shareOne // the fleet runs at 1.5 per weight
	m.Members[1].Share = 3 * shareOne
	if got, want := m.ShareFor(2), uint32(3*shareOne); got != want {
		t.Errorf("ShareFor(2) = %d, want %d", got, want)
	}
	// An out member's share is nobody's rate: the balancer leaves it where
	// it was when the member was drained.
	stale := member("c", 1, "")
	stale.Share, stale.Out = 40*shareOne, true
	m.Members = append(m.Members, stale)
	if got, want := m.ShareFor(2), uint32(3*shareOne); got != want {
		t.Errorf("ShareFor(2) beside an out member = %d, want %d", got, want)
	}
	m.Members[0].Out, m.Members[1].Out = true, true
	if got := m.ShareFor(2); got != DefaultShare(2) {
		t.Errorf("ShareFor(2) with nothing placeable = %d, want the default", got)
	}
}

func TestAMapThatCannotPlaceIsRefused(t *testing.T) {
	t.Parallel()
	valid := func(mutate func(*Map)) Map {
		m := mapOf(8, 3, "zone", member("a", 1, "eu"), member("b", 1, ""))
		mutate(&m)
		return m
	}
	if err := valid(func(*Map) {}).Validate(); err != nil {
		t.Fatalf("the valid map is refused: %v", err)
	}
	for name, m := range map[string]Map{
		"no generation":               valid(func(m *Map) { m.Generation = uuid.Nil }),
		"no replicas":                 valid(func(m *Map) { m.Replicas = 0 }),
		"replicas past the cap":       valid(func(m *Map) { m.Replicas = MaxReplicas + 1 }),
		"too few group bits":          valid(func(m *Map) { m.PGBits = MinPGBits - 1 }),
		"too many group bits":         valid(func(m *Map) { m.PGBits = MaxPGBits + 1 }),
		"a domain label with a space": valid(func(m *Map) { m.FailureDomain = "my zone" }),
		"an empty node":               valid(func(m *Map) { m.Members[0].Node = " " }),
		"weight zero":                 valid(func(m *Map) { m.Members[0].Weight = 0 }),
		"weight past the cap":         valid(func(m *Map) { m.Members[0].Weight = MaxWeight + 1 }),
		"share zero":                  valid(func(m *Map) { m.Members[0].Share = 0 }),
		"out of order":                valid(func(m *Map) { m.Members[0], m.Members[1] = m.Members[1], m.Members[0] }),
		"a duplicate":                 valid(func(m *Map) { m.Members[1].Node = "a" }),
		"a domain with no label": valid(func(m *Map) {
			m.FailureDomain = ""
		}),
	} {
		if err := m.Validate(); !errors.Is(err, ErrInvalid) {
			t.Errorf("%s: Validate = %v, want ErrInvalid", name, err)
		}
	}
}

// A QUORUM IS A MAJORITY OF THE COPIES THE MAP PLACES, bounded by the members
// it can place on — an out member is not one.
func TestAQuorumIsAMajorityOfTheCopies(t *testing.T) {
	t.Parallel()
	for _, c := range []struct{ replicas, members, out, want int }{
		{1, 1, 0, 1}, {2, 2, 0, 2}, {3, 3, 0, 2}, {3, 5, 0, 2}, {5, 5, 0, 3},
		{3, 2, 0, 2}, {3, 1, 0, 1},
		{3, 3, 1, 2}, {3, 3, 2, 1},
	} {
		m := fleet(c.members, 8, c.replicas)
		for i := range c.out {
			m.Members[i].Out = true
		}
		if got := m.Quorum(); got != c.want {
			t.Errorf("replicas %d over %d members, %d out: quorum %d, want %d",
				c.replicas, c.members, c.out, got, c.want)
		}
	}
}

// limitedMap is a map with fewer failure domains than copies, whose groups
// need the fill pass: two zones, three copies, and an out member.
func limitedMap() Map {
	drained := member("n6", 1, "z1")
	drained.Out = true
	return mapOf(9, 3, "zone",
		member("n0", 1, "z0"), member("n1", 2, "z0"), member("n2", 1, "z0"),
		member("n3", 1, "z1"), member("n4", 3, "z1"), member("n5", 1, "z1"), drained)
}

// THE UP SET IS THE SPECIFIED WALK, whatever structure computes it: every
// placeable member scored and sorted best first, a member taken unless its
// domain already holds a copy, and — when the fleet has fewer domains than
// copies — the rest filled from the best members not yet taken; then the
// others in score order, then the members out or on probation, one tail in
// score order. Written here the plain way, over a full sort, and held against
// the layout and the ranking in every group of maps that exercise each step.
func TestTheUpSetIsTheSpecifiedWalk(t *testing.T) {
	t.Parallel()
	reference := func(m Map, pg int) []string {
		type scored struct {
			m     Member
			score int64
		}
		seed := seedKey(pg, m.PGBits)
		var placeable, out []scored
		for _, mem := range m.Members {
			s := scored{mem, straw2(logDraw(seed, nodeKey(mem.Node)), mem.Share)}
			if mem.Out || mem.Probation {
				out = append(out, s)
			} else {
				placeable = append(placeable, s)
			}
		}
		order := func(a, b scored) int {
			if c := cmp.Compare(b.score, a.score); c != 0 {
				return c
			}
			return strings.Compare(a.m.Node, b.m.Node)
		}
		slices.SortFunc(placeable, order)
		slices.SortFunc(out, order)
		domain := func(mem Member) string {
			if m.FailureDomain != "" && mem.Domain != "" {
				return "domain:" + mem.Domain
			}
			return "node:" + mem.Node
		}
		taken := map[string]bool{}
		held := map[string]bool{}
		var ranked []string
		for _, s := range placeable {
			if len(ranked) < m.Size() && !held[domain(s.m)] {
				held[domain(s.m)] = true
				taken[s.m.Node] = true
				ranked = append(ranked, s.m.Node)
			}
		}
		for _, s := range placeable {
			if len(ranked) < m.Size() && !taken[s.m.Node] {
				taken[s.m.Node] = true
				ranked = append(ranked, s.m.Node)
			}
		}
		for _, s := range placeable {
			if !taken[s.m.Node] {
				ranked = append(ranked, s.m.Node)
			}
		}
		for _, s := range out {
			ranked = append(ranked, s.m.Node)
		}
		return ranked
	}
	plain := fleet(12, 8, 3)
	plain.Members[3].Out = true
	plain.Members[5].Weight, plain.Members[5].Share = 7, DefaultShare(7)
	plain.Members[8].Probation = true
	for name, m := range map[string]Map{
		"the pinned map":       goldenMap(),
		"a domain-limited map": limitedMap(),
		"no failure domain":    plain,
	} {
		l := m.Layout()
		for pg := range m.Groups() {
			want := reference(m, pg)
			if got := m.Ranked(pg); !slices.Equal(got, want) {
				t.Fatalf("%s, group %d: ranked %v, the walk gives %v", name, pg, got, want)
			}
			if got := l.Up(pg); !slices.Equal(got, want[:m.Size()]) {
				t.Fatalf("%s, group %d: up %v, the walk gives %v", name, pg, got, want[:m.Size()])
			}
		}
	}
}
