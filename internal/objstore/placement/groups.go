package placement

import (
	"math/bits"
)

// SplitGroups is the object map's groups ([placement.Groups]): 1<<PGBits runs
// of slots, seeded so that raising PGBits by one SPLITS every group in two and
// the lower half keeps its parent's holders exactly.
type SplitGroups struct {
	// PGBits is how many bits of a slot name its group.
	PGBits int
}

// Count is how many groups there are: 1<<PGBits.
func (g SplitGroups) Count() int { return 1 << g.PGBits }

// Seed is group pg's seed ([seedKey]).
func (g SplitGroups) Seed(pg int) uint64 { return seedKey(pg, g.PGBits) }

// seedKey is the placement seed of group pg in a map of k group bits: the
// group's bit string with its trailing zeros stripped, as the pair (value,
// length), packed into one word.
//
// WHY THE ZEROS ARE STRIPPED: raising k by one splits group p into 2p and
// 2p+1, and 2p is p's bit string with one more zero on the end — so its
// stripped pair is p's, it draws exactly what p drew, and every chunk in it
// stays where it was. 2p+1 ends in a one, so its pair keeps all k+1 bits; no
// group at any smaller k had a pair that long, so it draws fresh. A doubling
// therefore re-places the upper half of every group and nothing else. And two
// different groups at one k never share a seed, because the same value and
// length is the same bit string.
func seedKey(pg, k int) uint64 {
	tz := k
	if pg != 0 {
		tz = bits.TrailingZeros64(uint64(pg))
	}
	return uint64(k-tz)<<32 | uint64(pg)>>tz
}
