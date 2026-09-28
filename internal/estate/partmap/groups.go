package partmap

import (
	"hash/fnv"

	"github.com/crewlet/crewlet/internal/placement"
	"github.com/crewlet/crewlet/internal/statelog"
)

// FixedGroups is what the estate map places: one group per partition of a
// layout, in the layout's own order ([statelog.Layout.Partitions]), each with
// a seed of its own. A partition is never split — its count changes only by a
// repartition, which is a new layout and a new map — so a group's seed is a
// function of the partition's name and nothing else.
type FixedGroups struct {
	// Seeds is one seed per partition, in the layout's order.
	Seeds []uint64
}

var _ placement.Groups = FixedGroups{}

// Count is how many partitions the layout has.
func (g FixedGroups) Count() int { return len(g.Seeds) }

// Seed is partition i's seed.
func (g FixedGroups) Seed(i int) uint64 { return g.Seeds[i] }

// GroupsOf is a layout's partitions as the estate map's groups.
func GroupsOf(l statelog.Layout) FixedGroups {
	parts := l.Partitions()
	seeds := make([]uint64, len(parts))
	for i, p := range parts {
		seeds[i] = SeedOf(p)
	}
	return FixedGroups{Seeds: seeds}
}

// SeedOf is one partition's seed: FNV-1a over "partition", a NUL and the
// partition's name.
//
// THE NAME, NOT THE GROUP'S POSITION. A position is where the partition falls
// in its layout's order, so a seed built from it would rank `pages.000` the
// way another layout ranked `company.000` — and a layout adding a space would
// re-seed every partition sorted after it. A name is the partition's identity
// in every name the grammar produces, and its seed follows it. The prefix
// separates it from every other hash this engine takes of a partition's name.
func SeedOf(p statelog.PartitionID) uint64 {
	h := fnv.New64a()
	h.Write([]byte("partition\x00" + p.String()))
	return h.Sum64()
}

// groupOf is p's group in a layout — its position in the layout's order — and
// false for a partition the layout does not have.
//
// COMPUTED, NOT SEARCHED: the order is by space and then index, so a
// partition's position is the partitions of every space sorted before its own
// plus its index.
func groupOf(l statelog.Layout, p statelog.PartitionID) (int, bool) {
	if int(p.Index) >= l.Count(p.Space) || !p.Valid() {
		return 0, false
	}
	offset := 0
	for _, s := range l.Spaces {
		if s.Space < p.Space {
			offset += s.Partitions
		}
	}
	return offset + int(p.Index), true
}
