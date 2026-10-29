package compact

import (
	"cmp"
	"slices"

	"github.com/crewlet/crewlet/internal/code"
	"github.com/crewlet/crewlet/internal/code/cache"
)

// LayerSplitFactor is how a publication merges the published chain's top
// layers: a layer is ABSORBED into the replacement once the objects gathered
// above it, times this factor, reach its own — so every layer the chain keeps
// holds more than this many times the layer directly above it, every object
// is written into a new layer O(log n) times over its life, and the chain is
// at most log₂(the history ÷ one publication) layers long.
//
// 2, KEPT (M3-decisions M3-O39): git's own repack.midxSplitFactor default
// (repack-midx.c, 2.56.0), and the commit-graph's --size-multiple, so the
// chain this host publishes is the chain git's own incremental repack would
// keep. TestThePlanIsGeometric holds the rule.
const LayerSplitFactor = 2

// LayerRepackPacks is how many of a new layer's packs may be NOT FULL
// (code.PackRow.Full) before the publication repacks them: past it, every
// non-full pack the layer gathers is merged first, the lightest within the
// roll's byte cap (InputByteCap), so a chain holds at most this many non-full
// packs a layer beside its full ones.
//
// 8, KEPT (M3-decisions M3-O39): git's repack.midxNewLayerThreshold default.
// It is the term that keeps a published repository's pack count bounded,
// since a published pack is never a compaction's input (code's decide rule
// 2): under 32 × 8 non-full packs and some 24 full ones at the default
// repo_max_bytes, against code.MaxPacksPerRepo.
const LayerRepackPacks = 8

// layerBytesPerObject and layerBytesPerCommit are what a published layer
// weighs, an object and a commit of what it covers: measured on git 2.56.0,
// a multi-pack-index layer is about 32 bytes an object (its object ids, its
// offsets and its reverse index, 20 + 8 + 4) and its bitmap 4 to 5 more, and a
// commit-graph layer with every version-2 changed-path filter about 69 bytes a
// commit. Each is rounded up, and a plan counts the commits as a third of the
// objects — more than any source repository's composition holds — so the
// estimate is an UPPER bound, which a plan refuses a layer by before anything
// is built (planLayer, layer_too_large) and a full store holds its publication
// back by.
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
	// written over; replaces is the ids of the chain's layers above them, in
	// chain order — the layers the new one replaces.
	base     int
	replaces []string
	// members is every pack the new layer covers before its repack, lightest
	// first: every live pack no layer of the chain names, and every pack of
	// the layers it replaces.
	members []pack
	// repack is the non-full members merged before the layer is written,
	// when more than LayerRepackPacks of them are not full, within the roll's
	// caps; not due otherwise.
	repack planned
	// objects is the objects the layer will hold as the rows count them, each
	// pack file once: an upper bound, since a repack's outputs hold each
	// object of its inputs once.
	objects int64
	// historyObjects and historyBytes are every live pack's objects and pack
	// bytes, each file once: what the layer's bitmap walks and reads — every
	// commit and what it reaches, the base's among them — and its deadline
	// is sized by.
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
	// layerTooLarge: the layer would weigh more than code.MaxLayerParts parts
	// of code.LayerPartBytes hold.
	layerTooLarge
)

// planLayer is ONE PUBLICATION'S PLAN over a copy's published chain and its
// live packs — PURE in those, its byte cap and its bounds: git's own
// geometric policy over published layers (repack-midx.c's split factor and
// new-layer threshold), applied to this code host's closed-chain rule.
//
//  1. THE TAIL is every live pack — grouped by git checksum, as a
//     compaction's plan groups them — that no layer of the chain names.
//  2. A LAYER THIS NODE REFUSED is REPLACED, with every layer above it,
//     whatever their sizes: the copy holds the chain only below it, a layer
//     is valid only over its exact base, and nothing is built over a layer
//     nobody loads — so the new layer is written over the installed prefix.
//     A layer neither installed nor refused here is still at the installer:
//     the plan WAITS, rather than replace a layer a moment from being
//     installed. A tail with nothing refused and the chain whole on this node
//     is NOTHING to publish.
//  3. GEOMETRIC: walking down from the top, the next layer is ABSORBED while
//     the objects gathered so far, times LayerSplitFactor, reach its own —
//     and regardless while the chain would pass code.MaxPublishedLayers — and
//     absorb more again past that, so a build git wrote no bitmap of (a tail
//     holding no commit) is planned once more over one more layer.
//  4. THE MEMBERS are the tail and every pack of the layers the new one
//     replaces: a bitmap needs a chain closed over everything its commits
//     reach, and every pack live at the plan is closed over the packs before
//     it, so a layer over every live pack its base does not cover is closed —
//     while one over the full packs of a cut alone was refused by git.
//  5. THE REPACK: when more than LayerRepackPacks members are not full, every
//     non-full member is merged first, the lightest within
//     code.MaxCompactInputs row ids and byteCap pack bytes (a compaction's
//     caps, the repository's capFor among them).
//  6. A layer that would weigh more than code.MaxLayerParts ×
//     code.LayerPartBytes (layerEstimate) is TOO LARGE, refused before
//     anything is built: its build would need more background memory than
//     any git node this release sizes holds.
func planLayer(chain cache.Chain, live []code.PackRow, byteCap int64, b bounds, absorb int) (layerPlan,
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
			// One predicate with the decide's (code.PackRow.FullAt), at
			// the cut the repack would cut at.
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
	take := func(row code.LayerRow) {
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
	for base > 0 && (gathered*LayerSplitFactor >= rows[base-1].Layer.Objects || base+1 > code.MaxPublishedLayers) {
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
		plan.repack.inputs = capped(nonFull, code.MaxCompactInputs, byteCap)
		plan.repack.nonFull = len(nonFull)
		plan.repack.due = len(plan.repack.inputs) >= 2
	}
	if layerEstimate(plan.objects) > code.MaxLayerParts*code.LayerPartBytes {
		return plan, layerTooLarge
	}
	return plan, layerDue
}

// layerPacks is the checksums of every pack the planned layer covers once its
// repack has written outs: the members its repack did not merge, and the
// outputs — sorted, as a layer row names them.
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
