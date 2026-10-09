// Package rendezvous is the ONE order in which a fleet's nodes are preferred
// for a key: rendezvous, or highest-random-weight, hashing. Every placement
// that picks among nodes by a key takes its order from [Order] — ADR-0008, a
// rule more than one package needs gets exactly one implementation.
//
// # Why rendezvous, rather than a table, a ring or a modulo
//
//   - NOTHING TO AGREE ON. [Order] is a pure function of the key and the SET of
//     nodes, so any node that sees the same members computes the same order:
//     no routing table to build, publish or keep current, and no coordinator.
//   - A MEMBERSHIP CHANGE MOVES ONLY WHAT IT MUST. A node that leaves hands
//     each key it ranked first to that key's own runner-up — spread across
//     every survivor, where a ring without virtual nodes hands them all to one
//     successor — and a node that joins takes about 1/N of the keys while no
//     other key moves between the nodes that stayed. A modulo over the member
//     count moves almost every key.
//   - O(N) A KEY, which a fleet of tens of nodes can afford on every request:
//     about 270 ns and two allocations for five nodes, 1.1 µs for sixteen
//     (BenchmarkOrder).
//
// # The weight, and why it is FINISHED
//
// A node's weight for a key is FNV-1a-64 over the key, one NUL and the node,
// finished by MurmurHash3's 64-bit finaliser, fmix64. The finisher is the
// point. FNV-1a's last step multiplies by a prime of 2^40 + 0x1b3, so for two
// nodes named alike but for a last character the difference that character
// makes never reaches the high bits, and the comparison is decided by its low
// bits: over node-0 … node-4 — the default node id's shape, and a
// StatefulSet's ordinals — plain FNV-1a ranked node-4 first for half of all
// keys, and every other node had ONE fixed runner-up (node-0's was always
// node-1), so all of a silent node's load fell on a single neighbour. That was
// internal/estate's router before this package. Finished, every node comes
// first within 1.5% of its fair share and every node's keys spread their
// second choice within 4.1% of fair across all the others
// (TestTheOrderSpreadsKeysEvenly). fmix64 is a bijection on 64 bits, so it
// adds no collision FNV did not already have.
//
// # A total order over the SET
//
// Ties on weight break on the node's own bytes, ascending, never on where a
// node sat in the listing: a placement's listing arrives in no particular
// order, and two nodes handed the same members in two orders must still agree.
//
// # The arithmetic is a contract
//
// Golden vectors pin it (TestTheWeightIsPinned, TestTheOrderIsPinned). Two
// builds that rank one key differently disagree about which node the key
// prefers, and a rolling upgrade runs two builds side by side: a change here
// is a decision taken in this paragraph, never a refactor.
//
// # Why the weight is not exported
//
// Comparing two weights outside this package IS a second rendezvous order —
// exactly what TestNoPackageOrdersByAHashOfItsOwn refuses — so an exported
// weight would have no use but that one. Every placement is a prefix of
// [Order]: the first node, the first two, the first that is live.
//
// # Callers
//
// [internal/estate]'s router orders the data nodes it asks by its own node id.
// The order picks each asker's FIRST data node and every node it fails over
// to, spread evenly across the data nodes; it does not decide where an asker
// stays, because the router keeps asking the node that last answered until
// that node goes silent — so a data node that joins is asked only by askers
// with no such node yet, or whose node went silent.
//
// # One implementation, held by a gate
//
// TestNoPackageOrdersByAHashOfItsOwn fails the build on any other package that
// ORDERS two values derived from a hash — a comparison, a cmp, bytes or
// strings compare, a sort, max or search over hash values, raw digests and
// scaled weights included — because that is rendezvous or a hash ring
// whatever its candidates are called. Its doc states the rule, what it never
// flags, and its boundary.
//
// A leaf: it imports nothing from the engine, so any layer may use it.
package rendezvous

import (
	"cmp"
	"slices"
	"strings"
)

// Order returns nodes in rendezvous order for key: the node key prefers first,
// then its runner-up, and so on.
//
// It is a function of the SET it is handed: ties on weight break on the node's
// own bytes, ascending, so two callers handed the same members in a different
// order get the same answer. It returns a new slice of len(nodes) and never
// modifies or aliases nodes, so a caller may hand it a listing somebody else
// holds. Duplicates are kept, adjacent; nothing is filtered — dropping this
// node, an empty id or a dead node is the caller's decision, and removing
// members from the result leaves the rest in the order Order would have given
// them.
func Order(key string, nodes []string) []string {
	prefix := keyed(key)
	ranked := make([]candidate, len(nodes))
	for i, n := range nodes {
		ranked[i] = candidate{node: n, weight: weightFrom(prefix, n)}
	}
	// Not a stable sort, and it need not be: compare is a total order on
	// distinct nodes, and two equal candidates are the same id twice.
	slices.SortFunc(ranked, compare)
	out := make([]string, len(ranked))
	for i, c := range ranked {
		out[i] = c.node
	}
	return out
}

// candidate is one node and its weight for the key being ordered: each weight
// computed ONCE, where a comparator computing it per comparison paid two
// hashes for each of O(n log n) comparisons.
type candidate struct {
	node   string
	weight uint64
}

// compare is Order's total order: the higher weight first, and on a tie the
// node's own bytes, ascending.
func compare(a, b candidate) int {
	if c := cmp.Compare(b.weight, a.weight); c != 0 {
		return c
	}
	return strings.Compare(a.node, b.node)
}

const (
	// offset64 and prime64 are FNV-1a's 64-bit offset basis and prime, the
	// IETF draft's and hash/fnv's.
	offset64 = 0xcbf29ce484222325
	prime64  = 0x100000001b3

	// separator ends the key. A node id cannot contain it (config's
	// node.id pattern refuses one), so the input's LAST NUL is always this
	// one, and no two (key, node) pairs feed FNV the same bytes.
	separator = 0x00
)

// weight is the score Order ranks by. Unexported on purpose: see the package
// doc.
func weight(key, node string) uint64 { return weightFrom(keyed(key), node) }

// keyed is FNV-1a's state after the key and the separator: the half of every
// weight one Order shares across all its nodes, computed once.
func keyed(key string) uint64 { return (fnv1a(offset64, key) ^ separator) * prime64 }

// weightFrom finishes a weight from keyed's state.
func weightFrom(prefix uint64, node string) uint64 { return mix(fnv1a(prefix, node)) }

// fnv1a continues FNV-1a over s from h: XOR the byte, THEN multiply — FNV-1
// is the other order, and the golden test tells them apart. Written over the
// string rather than through hash/fnv, whose Hash64 is an interface taking a
// []byte: the hasher and the conversion both escape, two allocations a weight.
func fnv1a(h uint64, s string) uint64 {
	for i := 0; i < len(s); i++ {
		h ^= uint64(s[i])
		h *= prime64
	}
	return h
}

// mix is MurmurHash3's 64-bit finaliser, fmix64 (Austin Appleby, public
// domain): every input bit moves every output bit with probability about a
// half. A bijection, so it adds no collision FNV did not have.
func mix(h uint64) uint64 {
	h ^= h >> 33
	h *= 0xff51afd7ed558ccd
	h ^= h >> 33
	h *= 0xc4ceb9fe1a85ec53
	h ^= h >> 33
	return h
}
