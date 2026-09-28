package placement

import (
	"math"
)

// drawer is one draw prepared: every member's key hashed once, its
// failure domain resolved to a small integer, and the scratch one group's
// ranking needs, reused from group to group.
//
// NOT SAFE FOR CONCURRENT USE — it is the scratch of one computation, and each
// entry point below builds its own.
type drawer struct {
	m Draw

	// shares is what the draws divide by, per member — the draw's own,
	// except inside [Balance], which moves them between rounds.
	shares []uint32
	keys   []uint64
	domain []int // per member: its failure domain as an index, unique when it has none
	nDom   int   // how many domain indexes there are

	placeable []int // member indexes that take copies, in node order
	idle      []int // member indexes that take none — out or on probation — in node order
	size      int   // copies per group: Draw.Size
	distinct  int   // domains the placeable members span: Draw.DistinctDomains

	// logs, when set, is every (group, placeable member) logarithm
	// computed once — the part of a score a share does not change — so a
	// balancer re-dividing them round after round does not re-draw. Only
	// [Balance] sets it.
	logs []int64

	// heap is the group's candidates not yet taken, best on top; skipped
	// is the ones passed over for their domain, best first, from
	// skipFrom on.
	heap     []cand
	skipped  []cand
	skipFrom int
	seen     []uint32 // per domain: the stamp of the group that took it
	stamp    uint32
	up       []int
}

// cand is one member's score in one group.
type cand struct {
	score int64
	i     int
}

// better is the order a group ranks its members in: score descending, then
// node name — so a tie is broken the same way everywhere. Members are sorted
// by node, so their index order IS name order.
func better(a, b cand) bool {
	return a.score > b.score || a.score == b.score && a.i < b.i
}

func newDrawer(m Draw) *drawer {
	d := &drawer{
		m:      m,
		shares: make([]uint32, len(m.Members)),
		keys:   make([]uint64, len(m.Members)),
		domain: make([]int, len(m.Members)),
	}
	domains := map[string]int{}
	for i, member := range m.Members {
		d.shares[i] = member.Share
		d.keys[i] = nodeKey(m.Salt, member.Node)
		if m.FailureDomain != "" && member.Domain != "" {
			id, ok := domains[member.Domain]
			if !ok {
				id = d.nDom
				domains[member.Domain] = id
				d.nDom++
			}
			d.domain[i] = id
		} else {
			// No label, no collision: a member without a domain is
			// one of its own.
			d.domain[i] = d.nDom
			d.nDom++
		}
		if member.Placeable() {
			d.placeable = append(d.placeable, i)
		} else {
			d.idle = append(d.idle, i)
		}
	}
	placed := map[int]struct{}{}
	for _, i := range d.placeable {
		placed[d.domain[i]] = struct{}{}
	}
	d.distinct = len(placed)
	d.size = min(m.Replicas, len(d.placeable))
	d.heap = make([]cand, 0, len(m.Members))
	d.skipped = make([]cand, 0, len(m.Members))
	d.seen = make([]uint32, d.nDom)
	d.up = make([]int, 0, d.size)
	return d
}

// draw scores the placeable members for one group into the heap.
func (d *drawer) draw(pg int) {
	seed := d.m.Groups.Seed(pg)
	d.heap = d.heap[:0]
	for j, i := range d.placeable {
		var logU int64
		if d.logs != nil {
			logU = d.logs[pg*len(d.placeable)+j]
		} else {
			logU = logDraw(seed, d.keys[i])
		}
		d.heap = append(d.heap, cand{score: straw2(logU, d.shares[i]), i: i})
	}
	d.heapify()
}

// drawIdle scores the members that take no copies for one group into the
// heap.
func (d *drawer) drawIdle(pg int) {
	seed := d.m.Groups.Seed(pg)
	d.heap = d.heap[:0]
	for _, i := range d.idle {
		d.heap = append(d.heap, cand{score: straw2(logDraw(seed, d.keys[i]), d.shares[i]), i: i})
	}
	d.heapify()
}

// A HEAP RATHER THAN A SORT: a group's up set is its best few members, and a
// heap is built in linear time and yields them one at a time, so a group costs
// its draws plus a few pops rather than a full sort of every member — which
// measured a third of a layout's time before it was replaced. A walk that has
// to go deep for a domain pays the pops it takes, never more than the sort.
func (d *drawer) heapify() {
	for j := len(d.heap)/2 - 1; j >= 0; j-- {
		d.siftDown(j)
	}
}

func (d *drawer) siftDown(j int) {
	h := d.heap
	for {
		c := 2*j + 1
		if c >= len(h) {
			return
		}
		if r := c + 1; r < len(h) && better(h[r], h[c]) {
			c = r
		}
		if !better(h[c], h[j]) {
			return
		}
		h[j], h[c] = h[c], h[j]
		j = c
	}
}

// pop takes the best candidate left.
func (d *drawer) pop() cand {
	top := d.heap[0]
	last := len(d.heap) - 1
	d.heap[0] = d.heap[last]
	d.heap = d.heap[:last]
	d.siftDown(0)
	return top
}

// choose is one group's up set, as member indexes in selection order.
//
// Two passes over one order, best first. The first takes a member unless its
// domain already holds a copy; it stops at [Draw.Size], or once every domain
// the fleet has holds one, since nothing further down can add a new one. The
// second — reached only when the fleet spans fewer domains than copies — fills
// what is left with the best members not yet chosen, domain notwithstanding:
// degrade, never refuse to place. Those are the ones the first pass skipped,
// in the order it skipped them, and then whoever it never reached.
//
// What was not chosen is left for [Draw.Ranked]: the skipped from skipFrom,
// then the heap.
func (d *drawer) choose(pg int) []int {
	d.draw(pg)
	d.stamp++
	if d.stamp == 0 {
		// Wrapped: clear the marks, so one from four billion groups
		// ago is not read as this group's.
		clear(d.seen)
		d.stamp = 1
	}
	d.up = d.up[:0]
	d.skipped = d.skipped[:0]
	for domains := 0; len(d.up) < d.size && domains < d.distinct; {
		c := d.pop()
		if dom := d.domain[c.i]; d.seen[dom] != d.stamp {
			d.seen[dom] = d.stamp
			d.up = append(d.up, c.i)
			domains++
			continue
		}
		d.skipped = append(d.skipped, c)
	}
	d.skipFrom = 0
	for ; len(d.up) < d.size && d.skipFrom < len(d.skipped); d.skipFrom++ {
		d.up = append(d.up, d.skipped[d.skipFrom].i)
	}
	for len(d.up) < d.size {
		d.up = append(d.up, d.pop().i)
	}
	return d.up
}

// Ranked is every member in the order a group prefers them: its up set in the
// order it was chosen (the first is the group's PRIMARY, the member a writer
// and a reader try before the others), then every other placeable member best
// first, then every member that takes no copies — out or on probation — best
// first.
//
// The tail is where a writer goes when a holder does not answer — a copy on
// the next member in the order rather than one copy fewer — and where a reader
// and a repair look when no holder has what they want, because that is where
// such a copy is. The members that take no copies come last, and a writer skips
// them, because they take no new copies but may still hold old ones: an out
// member the share it is shedding, and one on probation whatever it held when
// the map removed it — which, for a group whose every other holder went with
// it, is the only copy there is.
//
// It hashes every member's key on each call, which is right for the paths
// that want it — a write past a silent holder, a read of a copy outside the up
// set — and wrong for the per-item question of who holds a group: that is
// [Layout.Up], computed once per map.
func (d Draw) Ranked(pg int) []string {
	dr := newDrawer(d)
	out := make([]string, 0, len(d.Members))
	for _, i := range dr.choose(pg) {
		out = append(out, d.Members[i].Node)
	}
	for _, c := range dr.skipped[dr.skipFrom:] {
		out = append(out, d.Members[c.i].Node)
	}
	for len(dr.heap) > 0 {
		out = append(out, d.Members[dr.pop().i].Node)
	}
	dr.drawIdle(pg)
	for len(dr.heap) > 0 {
		out = append(out, d.Members[dr.pop().i].Node)
	}
	return out
}

// Layout is every group's holders under one draw, computed once — the answer a
// node places everything by, rather than drawing a group's members again for
// each item that falls in it.
//
// IMMUTABLE once computed, so any number of goroutines may read one — a node
// swaps in a new layout with a new map rather than changing the one it has.
// The zero Layout is a map that places nothing: every group's up set is
// empty.
type Layout struct {
	m      Draw
	up     [][]string
	copies []int // per member: how many groups' up sets hold it
}

// Layout computes where every group lives.
//
// ITS COST is groups × members draws, each an integer logarithm of about 85 ns,
// plus a few heap pops per group: BenchmarkLayout measured 12 ms for 50
// members over 2048 groups and 185 ms for 200 members over 8192 — the counts
// the object map picks for those fleets at three copies — on one core of a
// 2.1 GHz Xeon. It is paid once per map a node installs, never per item:
// placing per item is what had an object server spending seconds on one
// request.
func (d Draw) Layout() *Layout {
	dr := newDrawer(d)
	groups := d.Groups.Count()
	l := &Layout{m: d, up: make([][]string, groups), copies: make([]int, len(d.Members))}
	flat := make([]string, groups*dr.size)
	for pg := range groups {
		row := flat[pg*dr.size : (pg+1)*dr.size : (pg+1)*dr.size]
		for j, i := range dr.choose(pg) {
			row[j] = d.Members[i].Node
			l.copies[i]++
		}
		l.up[pg] = row
	}
	return l
}

// Draw is the draw this layout was computed from.
func (l *Layout) Draw() Draw { return l.m }

// Up is the members that hold a group, in the order they were chosen: the
// first is the group's PRIMARY. It equals the first [Draw.Size] entries of
// [Draw.Ranked], and is nil for a group the draw does not have. The slice is
// the layout's own: read it, never write it.
func (l *Layout) Up(pg int) []string {
	if pg < 0 || pg >= len(l.up) {
		return nil
	}
	return l.up[pg]
}

// Copies is how many groups each member holds a copy of under this layout —
// the members that take none, out or on probation, included at zero.
func (l *Layout) Copies() map[string]int {
	out := make(map[string]int, len(l.m.Members))
	for i, member := range l.m.Members {
		out[member.Node] = l.copies[i]
	}
	return out
}

// Deviation is how far the layout is from proportional: the largest relative
// difference, over the placeable members, between the copies a member holds
// and the copies its weight entitles it to (see [Balance]). Zero is exact.
func (l *Layout) Deviation() float64 {
	if len(l.copies) == 0 {
		return 0
	}
	return deviation(l.copies, targets(newDrawer(l.m)))
}

// deviation is the largest relative difference between a member's copies and
// its target, over the members with a target.
func deviation(copies []int, targets []float64) float64 {
	worst := 0.0
	for i, t := range targets {
		if t > 0 {
			worst = math.Max(worst, math.Abs(float64(copies[i])-t)/t)
		}
	}
	return worst
}
