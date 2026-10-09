package rendezvous_test

import (
	"fmt"
	"math/rand/v2"
	"slices"
	"testing"

	"github.com/crewlet/crewlet/internal/rendezvous"
)

// The node sets the order is judged over: a StatefulSet's ordinals (what
// docs/concepts/configuration.md recommends naming a fleet's nodes), the
// default node id's shape, nodes named alike but for a last letter, and names
// with nothing in common. The first three are the ones plain FNV-1a ranked by
// their last character.
var nodeSets = [][]string{
	{"crewlet-0", "crewlet-1", "crewlet-2"},
	{"node-0", "node-1", "node-2", "node-3", "node-4"},
	{"data-a", "data-b", "data-c"},
	{"eu-west-db", "us-east-db", "ap-south-db"},
}

// uuidKeys are n uuid-shaped keys, the shape every id this engine mints has,
// drawn from a FIXED seed: every statistical verdict below is a constant of
// the code, so none of them can flake.
func uuidKeys(n int) []string {
	rng := rand.New(rand.NewPCG(1, 2))
	out := make([]string, n)
	for i := range out {
		var b [16]byte
		for j := range b {
			b[j] = byte(rng.Uint32())
		}
		b[6] = b[6]&0x0f | 0x70 // version 7
		b[8] = b[8]&0x3f | 0x80 // RFC 9562 variant
		out[i] = fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:])
	}
	return out
}

// nodeKeys are n keys shaped like the node ids the router orders by: an
// asker's own id is the key there.
func nodeKeys(n int) []string {
	out := make([]string, n)
	for i := range out {
		out[i] = fmt.Sprintf("agent-%d", i)
	}
	return out
}

// THE ORDER IS PINNED, end to end, for the vectors a reader can check by hand
// against the weight's own golden test. Plain FNV-1a ranked the first one
// [data-a data-c data-b].
func TestTheOrderIsPinned(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		key   string
		nodes []string
		want  []string
	}{
		{"agent-2", []string{"data-a", "data-b", "data-c"},
			[]string{"data-c", "data-a", "data-b"}},
		{"0190c8f2-7a1b-7c3d-8e4f-5a6b7c8d9e0f",
			[]string{"crewlet-0", "crewlet-1", "crewlet-2", "crewlet-3", "crewlet-4"},
			[]string{"crewlet-2", "crewlet-3", "crewlet-4", "crewlet-0", "crewlet-1"}},
	} {
		reversed := slices.Clone(c.nodes)
		slices.Reverse(reversed)
		for _, nodes := range [][]string{c.nodes, reversed} {
			if got := rendezvous.Order(c.key, nodes); !slices.Equal(got, c.want) {
				t.Errorf("Order(%q, %q) = %q, want %q", c.key, nodes, got, c.want)
			}
		}
	}
}

// THE ORDER IS A FUNCTION OF THE SET: every permutation of one set of members
// gives one answer, the answer is the members themselves, and the caller's
// listing is neither reordered nor shared with the answer. (A tie broken by
// listing position rather than by name is invisible here without a 64-bit
// weight collision, which is why TestTiesBreakOnTheNodesOwnBytes holds that.)
func TestTheOrderIsAFunctionOfTheSet(t *testing.T) {
	t.Parallel()
	members := []string{"node-0", "node-1", "node-2", "node-3", "node-4"}
	for _, key := range uuidKeys(50) {
		want := rendezvous.Order(key, members)
		permutations(members, func(listing []string) {
			held := slices.Clone(listing)
			got := rendezvous.Order(key, listing)
			if !slices.Equal(got, want) {
				t.Fatalf("Order(%q, %q) = %q, but the same members listed as %q give %q",
					key, listing, got, members, want)
			}
			if !slices.Equal(listing, held) {
				t.Fatalf("Order rewrote its caller's listing %q to %q", held, listing)
			}
			got[0] = "rewritten"
			if !slices.Equal(listing, held) {
				t.Fatalf("Order's answer shares its caller's listing: writing the answer "+
					"rewrote %q to %q", held, listing)
			}
		})
		if !slices.Equal(slices.Sorted(slices.Values(want)), members) {
			t.Fatalf("Order(%q, %q) = %q, which is not the same members", key, members, want)
		}
	}

	// Nothing is filtered: a duplicate stays, beside its twin.
	key := uuidKeys(1)[0]
	got := rendezvous.Order(key, []string{"b", "a", "b"})
	if !slices.Equal(got, []string{"a", "b", "b"}) && !slices.Equal(got, []string{"b", "b", "a"}) {
		t.Errorf("Order of [b a b] = %q, want both b kept and adjacent", got)
	}
	if got := rendezvous.Order(key, nil); len(got) != 0 {
		t.Errorf("Order of no nodes = %q, want none", got)
	}
}

// permutations calls visit with every ordering of xs, each in a slice of its
// own.
func permutations(xs []string, visit func([]string)) {
	if len(xs) <= 1 {
		visit(slices.Clone(xs))
		return
	}
	for i := range xs {
		rest := slices.Concat(xs[:i], xs[i+1:])
		permutations(rest, func(tail []string) {
			visit(append([]string{xs[i]}, tail...))
		})
	}
}

// THE ORDER SPREADS KEYS EVENLY, which is the bug this package fixed. The
// router's private weight was plain FNV-1a, whose last step multiplies by a
// prime of 2^40 + 0x1b3: two nodes named alike but for a last character were
// ranked by that character's low bits. Over node-0 … node-4, node-4 came first
// for half of all keys — half a fleet's askers on one data node — and every
// other node had ONE fixed runner-up, so all of a silent node's askers failed
// over to a single neighbour. Every node must come first for its fair share,
// and every node's keys must spread their second choice across all the
// others.
//
// Tolerances: 10% of the fair share for first place, 15% for second. Measured
// with the finished weight: 1.5% and 4.1% at worst; with plain FNV-1a, 150% and
// 300%.
func TestTheOrderSpreadsKeysEvenly(t *testing.T) {
	t.Parallel()
	const keys = 30_000
	for _, shape := range []struct {
		name string
		keys []string
	}{{"uuid", uuidKeys(keys)}, {"agent-N", nodeKeys(keys)}} {
		for _, nodes := range nodeSets {
			first := map[string]int{}
			second := map[string]map[string]int{}
			for _, key := range shape.keys {
				order := rendezvous.Order(key, nodes)
				first[order[0]]++
				if second[order[0]] == nil {
					second[order[0]] = map[string]int{}
				}
				second[order[0]][order[1]]++
			}
			n := float64(len(nodes))
			for _, node := range nodes {
				share := float64(first[node]) / keys
				if dev := relative(share, 1/n); dev > 0.10 {
					t.Errorf("over %q, with %s keys, %s comes first for %.1f%% of keys, "+
						"%.0f%% off its fair %.1f%%: the askers this places do not spread "+
						"across the nodes, and one of them carries the load of several",
						nodes, shape.name, node, 100*share, 100*dev, 100/n)
				}
				for _, runnerUp := range nodes {
					if runnerUp == node {
						continue
					}
					share := float64(second[node][runnerUp]) / float64(first[node])
					if dev := relative(share, 1/(n-1)); dev > 0.15 {
						t.Errorf("over %q, with %s keys, of the keys %s comes first for, "+
							"%s is second for %.1f%%, %.0f%% off its fair %.1f%%: when %s "+
							"goes silent its load does not spread across the survivors",
							nodes, shape.name, node, runnerUp, 100*share, 100*dev,
							100/(n-1), node)
					}
				}
			}
		}
	}
}

// relative is how far got is from want, as a share of want.
func relative(got, want float64) float64 {
	d := (got - want) / want
	if d < 0 {
		return -d
	}
	return d
}

// A NODE LEAVING OR JOINING MOVES ONLY ITS OWN KEYS — the property that makes
// rendezvous the rule rather than a modulo over the member count. With a
// member gone, every key orders the survivors exactly as before; with one
// joined, every key orders the old members exactly as before, and the joiner
// takes its fair share of first places from all of them.
func TestANodeLeavingOrJoiningMovesOnlyItsOwnKeys(t *testing.T) {
	t.Parallel()
	members := []string{"node-0", "node-1", "node-2", "node-3", "node-4"}
	const joiner = "node-5"
	grown := append(slices.Clone(members), joiner)
	without := func(order []string, node string) []string {
		return slices.DeleteFunc(slices.Clone(order), func(n string) bool { return n == node })
	}
	for _, key := range uuidKeys(2_000) {
		before := rendezvous.Order(key, members)
		for _, gone := range members {
			after := rendezvous.Order(key, without(members, gone))
			if want := without(before, gone); !slices.Equal(after, want) {
				t.Fatalf("with %s gone, %q orders the survivors %q; before, it ordered "+
					"them %q — a departure moved keys it never held", gone, key, after, want)
			}
		}
		if after := without(rendezvous.Order(key, grown), joiner); !slices.Equal(after, before) {
			t.Fatalf("with %s joined, %q orders the old members %q; before, %q — an "+
				"arrival moved keys between nodes that stayed", joiner, key, after, before)
		}
	}

	const keys = 30_000
	taken := 0
	for _, key := range uuidKeys(keys) {
		if rendezvous.Order(key, grown)[0] == joiner {
			taken++
		}
	}
	if share := float64(taken) / keys; relative(share, 1.0/6) > 0.10 {
		t.Errorf("%s joining five members comes first for %.1f%% of keys, want about "+
			"%.1f%%", joiner, 100*share, 100.0/6)
	}
}

// BenchmarkOrder is the cost of one ordering, which the router pays on every
// request it does not answer in-process.
func BenchmarkOrder(b *testing.B) {
	key := uuidKeys(1)[0]
	for _, n := range []int{3, 5, 16} {
		nodes := make([]string, n)
		for i := range nodes {
			nodes[i] = fmt.Sprintf("crewlet-%d", i)
		}
		b.Run(fmt.Sprintf("nodes=%d", n), func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				_ = rendezvous.Order(key, nodes)
			}
		})
	}
}
