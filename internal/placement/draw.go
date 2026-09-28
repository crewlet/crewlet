package placement

import (
	"crypto/sha256"
	"encoding/binary"
	"math"
	"math/bits"
)

// nodeKey is a member's half of every draw it takes part in: a hash of its
// node id under its map's salt, computed once per member per layout rather
// than once per draw.
//
// THE SALT SEPARATES THE MAPS: the same node under two salts has two
// unrelated keys, so two maps over the same nodes rank them independently
// even for groups whose seeds coincide. It also separates this key from every
// other SHA-256 this engine takes of a node id.
func nodeKey(salt Salt, node string) uint64 {
	sum := sha256.Sum256([]byte(string(salt) + node))
	return binary.BigEndian.Uint64(sum[:8])
}

// golden is 2^64 over the golden ratio: the multiplier that spreads a small,
// structured seed (a group number) across the whole word before the node's key
// is folded in.
const golden = 0x9E3779B97F4A7C15

// mix is the random word one member draws for one group.
//
// PURE INTEGER ARITHMETIC rather than a SHA-256 per draw: a layout is groups ×
// members draws — up to 65536 × hundreds — computed by every node for every
// map and by the balancer for every candidate, and a cryptographic hash per
// draw is what had a node's object server spending seconds on one request.
// Nothing here needs collision resistance: the inputs are a group's seed and a
// node id the fleet chose itself, and what the draw needs is only that it is
// uniform and independent across (group, member) pairs, which two rounds of
// MurmurHash3's finalizer give. The seed is folded in twice so a structured
// seed cannot cancel against a structured key in one round.
func mix(seed, node uint64) uint64 {
	x := seed*golden ^ node
	x = fmix64(x)
	x ^= seed
	return fmix64(x)
}

// fmix64 is MurmurHash3's 64-bit finalizer: a bijection in which every input
// bit reaches every output bit.
func fmix64(k uint64) uint64 {
	k ^= k >> 33
	k *= 0xff51afd7ed558ccd
	k ^= k >> 33
	k *= 0xc4ceb9fe1a85ec53
	k ^= k >> 33
	return k
}

// fracBits is the fixed point of [log2fixed]: a logarithm carries 32
// fractional bits.
const fracBits = 32

// drawBits is how many of a draw's 64 bits the logarithm reads: the top 53,
// the precision of the float straw2 was first written against, and plenty for
// a Q32 result.
const drawBits = 53

// log2fixed is log2(x) in Q32 fixed point, for x >= 1, computed with integers
// only: the integer part is the position of the top bit, and each fractional
// bit comes from squaring the normalised mantissa — squaring doubles a
// logarithm, so whether the square reaches 2 is the next bit.
//
// INTEGERS, so every CPU agrees to the last bit. Two nodes computing a draw a
// last bit apart would rank a group's members differently and each ask the
// other for something neither holds — which is why Ceph's own crush_ln is
// integer arithmetic too, and why math.Log (per-architecture assembly) is
// not an option. It truncates, so it is at most a few units of 2^-32 below the
// true value, identically everywhere, and never decreases as x grows.
//
// Zero has no logarithm and answers the most negative value there is, which
// orders it below every real draw.
func log2fixed(x uint64) int64 {
	if x == 0 {
		return math.MinInt64
	}
	n := bits.Len64(x) - 1
	m := x << (63 - n) // the mantissa in [1, 2), as Q63
	var frac uint64
	for range fracBits {
		hi, lo := bits.Mul64(m, m) // m² as Q126
		// The square reached 2 exactly when the high word's top bit is
		// set: then this bit is one and the square halved back into
		// [1, 2) is m²/2^64, the high word; otherwise the square is in
		// [1, 2) already and is m²/2^63.
		//
		// SELECTED BY A MASK rather than a branch: the bit is a coin
		// flip, so a branch mispredicts half the time, and a layout
		// takes millions of these — the masked form measured 2.4 times
		// faster with bit-identical results.
		b := hi >> 63
		frac = frac<<1 | b
		keep := b - 1 // all ones when b is zero, none when it is one
		m = hi&^keep | (hi<<1|lo>>63)&keep
	}
	return int64(n)<<fracBits | int64(frac)
}

// logDraw is the logarithm of one member's draw for one seed: log2 of a
// uniform value in (0, 1], as Q32 — never positive.
//
// The part of a score that does not depend on the member's share, which is
// what lets the balancer compute it once and re-divide it every round.
func logDraw(seed, key uint64) int64 {
	u := mix(seed, key)>>(64-drawBits) + 1 // in [1, 2^53]
	return log2fixed(u) - drawBits<<fracBits
}

// straw2 is a member's score from its logarithm and its share: HIGHER wins,
// and the score closest to zero is the highest.
//
// STRAW2, Ceph's formula: ln(u)/w for a uniform u is an exponential race, and
// the member with the largest value wins with probability w over the sum of
// every w — for the top draw AND, because the race is memoryless, for each
// next place among whoever is left. The base of the logarithm scales every
// score alike and so changes no order. Shifting by 16 before dividing keeps
// the Q32 fraction a share of 1<<16 (weight 1.0) would otherwise divide away,
// and int64 division truncates toward zero the same way on every CPU.
func straw2(logU int64, share uint32) int64 {
	return (logU << 16) / int64(share)
}
