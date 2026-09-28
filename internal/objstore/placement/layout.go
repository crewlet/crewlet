package placement

import (
	"slices"

	"github.com/crewlet/crewlet/internal/placement"
)

// Layout is every group's holders under one map, computed once — the answer a
// node places every chunk by, rather than drawing a group's members again for
// each chunk that falls in it ([placement.Layout], with the map it was drawn
// from, so a slot finds its group).
//
// IMMUTABLE once computed, so any number of goroutines may read one — a node
// swaps in a new layout with a new map rather than changing the one it has.
// The zero Layout is a map that places nothing: every group's up set is
// empty.
type Layout struct {
	m     Map
	drawn placement.Layout
}

// Layout computes where every group lives. Its cost is the draw's
// ([placement.Draw.Layout]): paid once per map a node installs, never per
// chunk.
func (m Map) Layout() *Layout { return &Layout{m: m, drawn: *m.Draw().Layout()} }

// Map is the map this layout was computed from.
func (l *Layout) Map() Map { return l.m }

// Up is the members that hold a group, in the order they were chosen: the
// first is the group's PRIMARY. It equals the first [Map.Size] entries of
// [Map.Ranked], and is nil for a group the map does not have. The slice is
// the layout's own: read it, never write it.
func (l *Layout) Up(pg int) []string { return l.drawn.Up(pg) }

// Group is the group a slot belongs to under this layout's map.
func (l *Layout) Group(slot int) int { return l.m.GroupOf(slot) }

// Copies is how many groups each member holds a copy of under this layout —
// the members that take none, out or on probation, included at zero.
func (l *Layout) Copies() map[string]int { return l.drawn.Copies() }

// Deviation is how far the layout is from proportional
// ([placement.Layout.Deviation]). Zero is exact.
func (l *Layout) Deviation() float64 { return l.drawn.Deviation() }

// Range is a run of slots, Lo inclusive and Hi exclusive.
type Range struct {
	Lo, Hi int
}

// Len is how many slots the range holds.
func (r Range) Len() int { return r.Hi - r.Lo }

// Moved is every run of slots whose holders differ between two layouts — what
// a map change asks the fleet to copy — in slot order, adjacent runs merged.
//
// HOLDERS AS A SET: two up sets naming the same members in another order moved
// no data, only which member is tried first. And BY SLOT, so two layouts at
// different group counts compare as readily as two at the same one: each group
// of the finer layout is held against the group of the coarser one that
// contains its slots. That is the comparison a split has to pass — the lower
// half of every group keeps its holders — and the one a map change's cost is
// counted in, since a group is a different amount of data at every count.
func Moved(before, after *Layout) []Range {
	fine, coarse := after, before
	if before.m.PGBits > after.m.PGBits {
		fine, coarse = before, after
	}
	shift := fine.m.PGBits - coarse.m.PGBits
	var out []Range
	for pg := range fine.m.Groups() {
		// A zero layout places nothing, so against one every group
		// that is held at all has moved — and it has the fewest group
		// bits there are, so it is always the coarser side, where every
		// group's up set is empty.
		if sameMembers(fine.Up(pg), coarse.Up(pg>>shift)) {
			continue
		}
		lo, hi := fine.m.SlotRange(pg)
		if n := len(out); n > 0 && out[n-1].Hi == lo {
			out[n-1].Hi = hi
			continue
		}
		out = append(out, Range{Lo: lo, Hi: hi})
	}
	return out
}

// sameMembers reports whether two up sets name the same members. They are at
// most [placement.MaxReplicas] long, so a scan beats building a set.
func sameMembers(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for _, node := range a {
		if !slices.Contains(b, node) {
			return false
		}
	}
	return true
}
