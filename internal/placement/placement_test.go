package placement

import (
	"cmp"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"hash/fnv"
	"math"
	"slices"
	"strings"
	"testing"
)

// partitions is a set of FIXED groups, one per name, seeded as the estate
// map seeds its partitions: FNV-1a over the name. The object map's own group
// shape — groups that split — is objstore/placement's, and its tests pin the
// draw over it; these are the draw's own properties, which must hold whatever
// a map's groups are.
type partitions []uint64

func (p partitions) Count() int         { return len(p) }
func (p partitions) Seed(pg int) uint64 { return p[pg] }

// partitionsOf is n fixed groups named like a layout's partitions.
func partitionsOf(n int) partitions {
	out := make(partitions, n)
	for i := range out {
		h := fnv.New64a()
		fmt.Fprintf(h, "partition\x00tracker.%03d", i)
		out[i] = h.Sum64()
	}
	return out
}

// groupCount is how many groups the fixtures below place: enough that a
// count per member is a quantity rather than noise, and a whole layout of it
// takes milliseconds.
const groupCount = 1024

// drawOf is a valid draw of these members, sorted, over groupCount
// partitions under the object map's salt.
func drawOf(replicas int, failureDomain string, members ...Member) Draw {
	slices.SortFunc(members, func(a, b Member) int { return strings.Compare(a.Node, b.Node) })
	return Draw{Salt: ObjectSalt, Replicas: replicas, FailureDomain: failureDomain,
		Members: members, Groups: partitionsOf(groupCount)}
}

// member is a node of this weight at its default share, in this domain.
func member(node string, weight int, domain string) Member {
	return Member{Node: node, Weight: weight, Share: DefaultShare(weight), Domain: domain}
}

// fleet is n equal members, data-000 onwards.
func fleet(n, replicas int) Draw {
	members := make([]Member, n)
	for i := range members {
		members[i] = member(fmt.Sprintf("data-%03d", i), 1, "")
	}
	return drawOf(replicas, "", members...)
}

// mixedDraw exercises every feature that changes a ranking — unequal weights,
// a share the balancer moved, a failure domain with a member missing its
// label, an out member.
func mixedDraw() Draw {
	drifted := member("data-b", 1, "zone-a")
	drifted.Share = DefaultShare(1) + 12345
	drained := member("data-f", 2, "zone-b")
	drained.Out = true
	return drawOf(3, "zone",
		member("data-a", 1, "zone-a"), drifted, member("data-c", 2, "zone-b"),
		member("data-d", 1, "zone-c"), member("data-e", 4, ""), drained)
}

// limitedDraw is a draw with fewer failure domains than copies, whose groups
// need the fill pass: two zones, three copies, and an out member.
func limitedDraw() Draw {
	drained := member("n6", 1, "z1")
	drained.Out = true
	return drawOf(3, "zone",
		member("n0", 1, "z0"), member("n1", 2, "z0"), member("n2", 1, "z0"),
		member("n3", 1, "z1"), member("n4", 3, "z1"), member("n5", 1, "z1"), drained)
}

// THE DRAW'S PIECES ARE PINNED ONE BY ONE — a contract between builds, since
// two builds on one fleet during a rolling upgrade must rank every group
// identically — so a failure in a map's own pins says which piece moved.
// Each value was reproduced outside this code: a node key is
// SHA-256(salt + "data-a")'s first eight bytes, the mix is the two
// MurmurHash3 finalizer rounds computed independently, and the logarithm is
// floor(log2(u)·2^32) − 53·2^32 for u = (mix >> 11) + 1. The seed is the
// object map's for group 12 at ten bits (value 3, length 8), whose own pin is
// objstore/placement's.
func TestTheDrawIsPinnedPieceByPiece(t *testing.T) {
	t.Parallel()
	const seed = 8<<32 | 3
	for _, c := range []struct {
		salt                     Salt
		key                      uint64
		mixed                    uint64
		logarithm, straw2Weight3 int64
	}{
		// The object map's salt, as the draws were first written — true
		// logarithm −4337083689.74.
		{ObjectSalt, 0xb9387ae947cdd6ec, 0x7f220807ec32ea22, -4337083690, -1445694563},
		{EstateSalt, 0xc50f204d122a06d8, 0x64038023ea7a74ac, -5823746085, -1941248695},
	} {
		key := nodeKey(c.salt, "data-a")
		for name, p := range map[string]struct{ got, want int64 }{
			"node key":  {int64(key), int64(c.key)},
			"mix":       {int64(mix(seed, key)), int64(c.mixed)},
			"logarithm": {logDraw(seed, key), c.logarithm},
			// Weight three divides the Q32 logarithm by three,
			// truncating toward zero: the shift keeps the fraction a
			// share would otherwise divide away.
			"straw2": {straw2(logDraw(seed, key), DefaultShare(3)), c.straw2Weight3},
		} {
			if p.got != p.want {
				t.Errorf("%q: %s = %#x, pinned %#x", c.salt, name, p.got, p.want)
			}
		}
	}
}

// THE ESTATE'S DRAWS ARE A CONTRACT FROM THE FIRST BUILD THAT HAS THEM: every
// group of a draw exercising every feature, over partitions, under the estate
// salt, as a digest — so a change to anything that ranks fails here rather
// than as two builds disagreeing about who holds a partition.
func TestTheEstateDrawIsPinned(t *testing.T) {
	t.Parallel()
	for name, c := range map[string]struct {
		d      Draw
		digest string
	}{
		"every feature":        {mixedDraw(), "a50aad86e7a1c7bc595812bcd0bfd863a86ef24d8147f10d03e7a69b887383c3"},
		"a domain-limited map": {limitedDraw(), "89f7059a851e9e9870c677af7ea9f7e49e6d8ddba87c3275fa13140bf4af8316"},
	} {
		c.d.Salt = EstateSalt
		if err := c.d.Validate(); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		l := c.d.Layout()
		h := sha256.New()
		for pg := range c.d.Groups.Count() {
			fmt.Fprintf(h, "%d:%s|%s\n", pg, strings.Join(l.Up(pg), ","),
				strings.Join(c.d.Ranked(pg), ","))
		}
		if got := hex.EncodeToString(h.Sum(nil)); got != c.digest {
			t.Errorf("%s: the layout's digest is %s, pinned %s", name, got, c.digest)
		}
	}
}

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

// THE UP SET IS THE SPECIFIED WALK, whatever structure computes it: every
// placeable member scored and sorted best first, a member taken unless its
// domain already holds a copy, and — when the fleet has fewer domains than
// copies — the rest filled from the best members not yet taken; then the
// others in score order, then the members out or on probation, one tail in
// score order. Written here the plain way, over a full sort, and held against
// the layout and the ranking in every group of draws that exercise each step,
// under both salts.
func TestTheUpSetIsTheSpecifiedWalk(t *testing.T) {
	t.Parallel()
	reference := func(m Draw, pg int) []string {
		type scored struct {
			m     Member
			score int64
		}
		seed := m.Groups.Seed(pg)
		var placeable, out []scored
		for _, mem := range m.Members {
			s := scored{mem, straw2(logDraw(seed, nodeKey(m.Salt, mem.Node)), mem.Share)}
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
	plain := fleet(12, 3)
	plain.Members[3].Out = true
	plain.Members[5].Weight, plain.Members[5].Share = 7, DefaultShare(7)
	plain.Members[8].Probation = true
	for name, m := range map[string]Draw{
		"every feature":        mixedDraw(),
		"a domain-limited map": limitedDraw(),
		"no failure domain":    plain,
	} {
		for _, salt := range []Salt{ObjectSalt, EstateSalt} {
			m.Salt = salt
			l := m.Layout()
			for pg := range m.Groups.Count() {
				want := reference(m, pg)
				if got := m.Ranked(pg); !slices.Equal(got, want) {
					t.Fatalf("%s under %q, group %d: ranked %v, the walk gives %v",
						name, salt, pg, got, want)
				}
				if got := l.Up(pg); !slices.Equal(got, want[:m.Size()]) {
					t.Fatalf("%s under %q, group %d: up %v, the walk gives %v",
						name, salt, pg, got, want[:m.Size()])
				}
			}
		}
	}
}
