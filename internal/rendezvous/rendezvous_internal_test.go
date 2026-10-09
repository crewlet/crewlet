package rendezvous

import (
	"cmp"
	"fmt"
	"hash/fnv"
	"slices"
	"strings"
	"testing"
)

// reference is the weight computed by a second, INDEPENDENT route: the
// standard library's FNV-1a over key, NUL and node, finished by fmix64 written
// out here from MurmurHash3's published constants. It shares no code with the
// package, so a drift in the package's inline loop, its shared key prefix or
// its finisher disagrees with it.
func reference(key, node string) uint64 {
	h := fnv.New64a()
	_, _ = h.Write([]byte(key))
	_, _ = h.Write([]byte{0})
	_, _ = h.Write([]byte(node))
	x := h.Sum64()
	x ^= x >> 33
	x *= 0xff51afd7ed558ccd
	x ^= x >> 33
	x *= 0xc4ceb9fe1a85ec53
	x ^= x >> 33
	return x
}

// THE WEIGHT IS A CONTRACT BETWEEN BUILDS. Two builds that rank one key
// differently disagree about which node the key prefers, and a rolling upgrade
// runs two builds side by side, so these vectors are pinned: a change to them
// is a decision about the arithmetic, never a refactor. They were derived from
// [reference], not from this package.
func TestTheWeightIsPinned(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		key, node string
		want      uint64
	}{
		{"", "", 0xb9034ad37056f5fb},
		{"node-0", "node-1", 0x1786876daf4b7c7c},
		{"agent-2", "data-a", 0xa94f3cc96abfba6d},
		{"0190c8f2-7a1b-7c3d-8e4f-5a6b7c8d9e0f", "crewlet-2", 0xf3f6b2790ed694b0},
	} {
		if got := weight(c.key, c.node); got != c.want {
			t.Errorf("weight(%q, %q) = %#016x, want %#016x: the arithmetic changed, and every "+
				"build that ranks by the old one now prefers a different node for this key",
				c.key, c.node, got, c.want)
		}
	}

	// THE FIRST STAGE IS FNV-1a, as the standard library spells it, over
	// the key, ONE NUL and the node — over a corpus the table does not
	// cover: empty strings, long ids, uuid keys.
	long := strings.Repeat("abcdefgh", 8)
	keys := []string{"", "k", "agent-2", "0190c8f2-7a1b-7c3d-8e4f-5a6b7c8d9e0f", long}
	nodes := []string{"", "n", "node-0", "node-4", "crewlet-2", "data-a", long + "-z"}
	for _, key := range keys {
		for _, node := range nodes {
			if got, want := weight(key, node), reference(key, node); got != want {
				t.Errorf("weight(%q, %q) = %#016x, the reference computes %#016x", key, node,
					got, want)
			}
		}
	}
}

// A TIE BREAKS ON THE NODE'S OWN BYTES, ascending, and never on where the node
// sat in the listing: two callers handed one set in two orders must agree on
// the answer, and a placement's listing is in no particular order. Held here,
// on the comparator, because a real tie needs two weights that collide in 64
// bits, which no test corpus will find.
func TestTiesBreakOnTheNodesOwnBytes(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		a, b candidate
		want int
	}{
		{candidate{"b", 7}, candidate{"a", 7}, 1},
		{candidate{"a", 7}, candidate{"b", 7}, -1},
		{candidate{"a", 1}, candidate{"b", 9}, 1}, // the weight decides first
		{candidate{"b", 9}, candidate{"a", 1}, -1},
		{candidate{"a", 7}, candidate{"a", 7}, 0},
	} {
		if got := compare(c.a, c.b); cmp.Compare(got, 0) != c.want {
			t.Errorf("compare(%v, %v) = %d, want the sign of %d", c.a, c.b, got, c.want)
		}
	}
	tied := []candidate{{"c", 5}, {"b", 5}, {"a", 5}}
	slices.SortFunc(tied, compare)
	if got := fmt.Sprint(tied); got != "[{a 5} {b 5} {c 5}]" {
		t.Errorf("three tied candidates sorted to %s, want a, b, c", got)
	}
}

// [Order] RANKS BY THE WEIGHT the golden test pins, through its own path to it
// — the key's prefix computed once and finished per node — so the shortcut
// cannot drift from the arithmetic. Judged against [reference]: the higher
// weight first, a tie by the node's bytes.
func TestOrderRanksByTheWeight(t *testing.T) {
	t.Parallel()
	sets := [][]string{
		{"crewlet-0", "crewlet-1", "crewlet-2"},
		{"node-0", "node-1", "node-2", "node-3", "node-4"},
		{"data-a", "data-b", "data-c"},
		{"", "eu-west-db", "us-east-db", "ap-south-db"},
	}
	for i := range 200 {
		key := fmt.Sprintf("key-%d", i)
		for _, nodes := range sets {
			want := slices.Clone(nodes)
			slices.SortFunc(want, func(a, b string) int {
				return cmp.Or(cmp.Compare(reference(key, b), reference(key, a)),
					strings.Compare(a, b))
			})
			if got := Order(key, nodes); !slices.Equal(got, want) {
				t.Fatalf("Order(%q, %q) = %q, the reference weights rank %q", key, nodes, got,
					want)
			}
		}
	}
}
