package compact

import (
	"cmp"
	"slices"

	"github.com/crewlet/crewlet/internal/gitlet"
	"github.com/crewlet/crewlet/internal/gitlet/cache"
)

// LayerSplitFactor is how a publication merges the published chain's top
// layers: a layer is ABSORBED into the replacement once the objects gathered
// above it, times this factor, reach its own, so each kept layer holds more
// than this many times the one above, every object is rewritten O(log n)
// times, and the chain is at most log₂(history ÷ one publication) layers.
//
// 2 (M3-O39): git's repack.midxSplitFactor default (repack-midx.c) and the commit-graph's
// --size-multiple, so the chain is the one git's own incremental repack would
// keep. TestThePlanIsGeometric holds the rule.
const LayerSplitFactor = 2

// LayerRepackPacks is how many of a new layer's packs may be NOT FULL
// (gitlet.PackRow.Full) before the publication merges them first, the lightest
// within the roll's byte cap (InputByteCap), so a chain holds at most this many
// non-full packs a layer beside its full ones.
//
// 8 (M3-O39): git's repack.midxNewLayerThreshold default. It bounds a
// published repository's pack count, since a published pack is never a
// compaction's input (gitlet's decide rule 2): under 32 × 8 non-full packs and
// some 24 full ones at the default repo_max_bytes, against
// gitlet.MaxPacksPerRepo.
const LayerRepackPacks = 8

// layerBytesPerObject and layerBytesPerCommit are what a published layer
// weighs per object and per commit it covers, measured on git 2.56.0: a
// multi-pack-index layer about 32 bytes an object (oid, offset, reverse index:
// 20 + 8 + 4) plus 4 to 5 of bitmap, a commit-graph layer with every version-2
// changed-path filter about 69 a commit. Both are rounded up and commits are
// counted as a third of the objects, more than any source repository holds, so
// layerEstimate is an UPPER bound: planLayer refuses a layer by it
// (layer_too_large) before any build, and a full store holds a publication
// back by it.
const (
	layerBytesPerObject = 40
	layerBytesPerCommit = 70
)

// layerEstimate is the upper bound of what a layer over objects objects
// weighs.
func layerEstimate(objects int64) int64 {
	return objects*layerBytesPerObject + objects/3*layerBytesPerCommit
}

// layerPlan is one publication's plan.
type layerPlan struct {
	// base is how many layers of the chain, from its root, the new layer is
	// written over; replaces is the ids of the layers above them, in chain
	// order.
	base     int
	replaces []string
	// members is every pack the new layer covers before its repack, lightest
	// first: every live pack no layer names, and every pack of the replaced
	// layers.
	members []pack
	// repack is the non-full members merged before the layer is written, when
	// more than LayerRepackPacks are not full, within the roll's caps.
	repack planned
	// objects is the layer's objects as the rows count them, each pack file
	// once: an upper bound, since a repack's outputs hold each object once.
	objects int64
	// historyObjects and historyBytes are every live pack's objects and pack
	// bytes, each file once: what the layer's bitmap walks (the base's history
	// too), and what its deadline is sized by.
	historyObjects, historyBytes int64
}

// layerVerdict is what a publication's plan found.
type layerVerdict uint8

const (
	// layerDue: a layer to build.
	layerDue layerVerdict = iota + 1
	// layerNothing: no pack lies outside the chain and the copy holds the
	// whole chain — nothing to publish.
	layerNothing
	// layerWaiting: a layer of the chain this node neither installed nor
	// refused — still at its installer — so the plan waits for it rather
	// than replace a layer a moment from being installed.
	layerWaiting
	// layerTooLarge: the layer would weigh more than gitlet.MaxLayerParts parts
	// of gitlet.LayerPartBytes hold.
	layerTooLarge
)

// planLayer is ONE PUBLICATION'S PLAN, pure in its arguments (package doc, "A
// layer covers every pack its base does not, and is written over exactly it").
// THE TAIL is every live pack, by git checksum, no layer names: empty over a
// whole chain, NOTHING to publish. A layer this node REFUSED is replaced with
// all above it (nothing is built over one nobody loads); one neither installed
// nor refused is at its installer, so the plan WAITS. Layers are absorbed from
// the top by LayerSplitFactor and while the chain would pass
// gitlet.MaxPublishedLayers, then absorb more (a tail holding no commit gave
// git nothing to bitmap). The MEMBERS are the tail and the replaced layers'
// packs; past LayerRepackPacks non-full ones, those merge first within the
// compaction caps (gitlet.MaxCompactInputs, byteCap). A layerEstimate past
// gitlet.MaxLayerParts × gitlet.LayerPartBytes is TOO LARGE for any node.
func planLayer(chain cache.Chain, live []gitlet.PackRow, byteCap int64, b bounds, absorb int) (layerPlan,
	layerVerdict) {

	rows := chain.Rows
	files := map[string]*pack{}
	full := map[string]bool{}
	var order []*pack
	for _, row := range live {
		p := files[row.GitChecksum]
		if p == nil {
			p = &pack{checksum: row.GitChecksum, objects: row.Objects, packBytes: row.Ref().PackBytes(),
				size: row.Size}
			files[row.GitChecksum] = p
			// The decide's predicate (gitlet.PackRow.FullAt), at the
			// repack's cut.
			full[row.GitChecksum] = row.FullAt(b.cut)
			order = append(order, p)
		}
		p.ids = append(p.ids, row.PackID)
	}
	named := map[string]bool{}
	for _, row := range rows {
		for _, cs := range row.Layer.Packs {
			named[cs] = true
		}
	}
	base := len(rows)
	if chain.Installed < len(rows) {
		if _, refused := chain.Refused[rows[chain.Installed].Layer.LayerID]; !refused {
			return layerPlan{}, layerWaiting
		}
		base = chain.Installed
	}
	var tail []*pack
	for _, p := range order {
		if !named[p.checksum] {
			tail = append(tail, p)
		}
	}
	if len(tail) == 0 && base == len(rows) {
		return layerPlan{}, layerNothing
	}
	members := slices.Clone(tail)
	in := map[string]bool{}
	for _, p := range members {
		in[p.checksum] = true
	}
	gathered := int64(0)
	for _, p := range members {
		gathered += p.objects
	}
	take := func(row gitlet.LayerRow) {
		for _, cs := range row.Layer.Packs {
			if p := files[cs]; p != nil && !in[cs] {
				in[cs] = true
				members = append(members, p)
				gathered += p.objects
			}
		}
	}
	for _, row := range rows[base:] {
		take(row)
	}
	for base > 0 && (gathered*LayerSplitFactor >= rows[base-1].Layer.Objects || base+1 > gitlet.MaxPublishedLayers) {
		base--
		take(rows[base])
	}
	for ; absorb > 0 && base > 0; absorb-- {
		base--
		take(rows[base])
	}
	plan := layerPlan{base: base, objects: gathered}
	for _, p := range order {
		plan.historyObjects += p.objects
		plan.historyBytes += p.packBytes
	}
	for _, row := range rows[base:] {
		plan.replaces = append(plan.replaces, row.Layer.LayerID)
	}
	for _, p := range members {
		slices.Sort(p.ids)
	}
	slices.SortFunc(members, func(a, b *pack) int {
		return cmp.Or(cmp.Compare(a.objects, b.objects), cmp.Compare(a.ids[0], b.ids[0]))
	})
	var nonFull []*pack
	for _, p := range members {
		plan.members = append(plan.members, *p)
		if !full[p.checksum] {
			nonFull = append(nonFull, p)
		}
	}
	if len(nonFull) > LayerRepackPacks {
		plan.repack.inputs = capped(nonFull, gitlet.MaxCompactInputs, byteCap)
		plan.repack.nonFull = len(nonFull)
		plan.repack.due = len(plan.repack.inputs) >= 2
	}
	if layerEstimate(plan.objects) > gitlet.MaxLayerParts*gitlet.LayerPartBytes {
		return plan, layerTooLarge
	}
	return plan, layerDue
}

// layerPacks is the checksums of every pack the layer covers once its repack
// wrote outs: the unmerged members and the outputs, sorted as a layer row names
// them.
func (p layerPlan) layerPacks(outs []output) []string {
	merged := map[string]bool{}
	if p.repack.due {
		for _, in := range p.repack.inputs {
			merged[in.checksum] = true
		}
	}
	var out []string
	for _, m := range p.members {
		if !merged[m.checksum] {
			out = append(out, m.checksum)
		}
	}
	for _, o := range outs {
		out = append(out, o.ref.GitChecksum)
	}
	slices.Sort(out)
	return slices.Compact(out)
}
