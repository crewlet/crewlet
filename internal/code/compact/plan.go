package compact

import (
	"cmp"
	"slices"

	"github.com/crewlet/crewlet/internal/code"
)

// InputByteCap is the most PACK bytes one roll merges at a cut of cut:
// MaxCompactOutputs/2 cuts, 8 GiB at the nominal cut (code.CompactPackBytes).
//
// HALF THE OUTPUTS, because the build's own count is the authority on how
// many it wrote, and a cut does not bound it alone: pack-objects closes an
// output before the object that would take it past --max-pack-size, and writes
// WHOLE a delta whose base went to an earlier output (builtin/pack-objects.c's
// write_one, 2.56.0), so outputs but the last average under the cut and a
// roll's output bytes may pass its input bytes — at a cap of
// code.MaxCompactOutputs cuts a roll writes seventeen outputs or more. At half,
// any two consecutive outputs sum past one cut, so the count stays within
// MaxCompactOutputs absent that expansion, and a build past it is refused
// before any upload and halves the repository's next plan
// ([Compactor.Compact]). MEASURED (M3-decisions M3-O29): BenchmarkCompact's
// cap= arm — a roll at this cap over the 100 MB source history pushed as
// gittest.HistorySessions packs, at a cut scaled to 4 MiB, git 2.56.0 over
// the in-memory store, Go 1.27.2, one bounded run (-benchtime 1x, one count)
// on two E-cores of an Intel Core Ultra 7 265U, which has SHA instructions,
// 2026-10-27 — wrote 8 outputs for its 8 cuts: outputs ÷ ⌈input bytes ÷
// cut⌉ = 1.0, half the 2.0 this half absorbs. So the half is KEPT, not
// raised to what that composition would allow: past it, the count's bound
// would rest on how one history deltas — how few of its deltas are written
// whole — rather than on the rule above, which holds for any.
//
// ONE FUNCTION, for the two plans that cap a roll: this package's compaction
// and a repository's publication's repack, which caps its own the same.
func InputByteCap(cut int64) int64 { return code.MaxCompactOutputs / 2 * cut }

// pack is one FILE of a repository's live set as a plan weighs it: a git
// checksum, which names one pack's files, and every live row naming it. Two
// rows of one checksum are one file and one input — a pack pushed twice under
// two ids — so the file is read once and both ids retire. Its figures are
// every row's, since a row is never rewritten and a checksum names one pack's
// bytes.
type pack struct {
	checksum string
	// ids are the rows naming the file, sorted.
	ids []string
	// objects is the file's object count — its WEIGHT, as git's geometric
	// repack weighs a pack (repack.c's geometry_pack_weight) — and
	// packBytes and size its pack section and its whole bundle.
	objects, packBytes, size int64
}

// planned is one plan of a repository's live packs: whether a compaction is
// DUE, and the packs it merges, lightest first — the order the build lists
// them in.
type planned struct {
	due    bool
	inputs []pack
	// nonFull is how many of the live files are not full: what the target
	// rule counts.
	nonFull int
}

// ids is every row id the plan's inputs retire, sorted: the compaction's
// Retire.
func (p planned) ids() []string {
	var ids []string
	for _, in := range p.inputs {
		ids = append(ids, in.ids...)
	}
	slices.Sort(ids)
	return ids
}

// checksums is every input's git checksum, in the plan's order: the packs the
// build's store holds and its stdin names, each file once.
func (p planned) checksums() []string {
	out := make([]string, len(p.inputs))
	for i, in := range p.inputs {
		out[i] = in.checksum
	}
	return out
}

// objects is what the inputs hold, each file once: the build's memory charge
// is stated by it ([cache.Cache.AdmitCompaction]), and the outputs may hold
// no more.
func (p planned) objects() int64 {
	var n int64
	for _, in := range p.inputs {
		n += in.objects
	}
	return n
}

// packBytes is the inputs' pack sections, each file once: what the byte cap
// and the build's deadline read.
func (p planned) packBytes() int64 {
	var n int64
	for _, in := range p.inputs {
		n += in.packBytes
	}
	return n
}

// size is the inputs' whole bundles, each file once: what the outputs will
// weigh in the object store, about — the estimate a store with no room for
// them is held back against.
func (p planned) size() int64 {
	var n int64
	for _, in := range p.inputs {
		n += in.size
	}
	return n
}

// plan plans one compaction of a repository whose live rows are live and whose
// published chain names the pack checksums in published, its roll capped at
// byteCap pack bytes, held to b — PURE in those, with no clock, so the memo
// of a plan found not due ([memo]) is keyed by every input it reads that
// moves: the repository's version (which moves with its live set, and with
// its chain, which only a record on its subject moves) and its cap; b is the
// compactor's for its life. The steps:
//
//   - every pack a PUBLISHED LAYER names is left out: a node's chain loads
//     only over every pack it names, so the decide refuses a compaction
//     retiring one stale (its rule 2), and a published repository's packs are
//     merged by its publications' repacks instead (layerplan.go);
//   - every FULL pack is left out (code.PackRow.FullAt at b's cut — one
//     predicate with the decide's): a full pack is never merged again, so
//     what a later published layer names stays valid until an expunge;
//   - the rest are grouped by git checksum, each file weighted by its objects
//     and sorted lightest first, the lowest row id breaking a tie;
//   - git's own geometric split, factor code.CompactFactor (repack.c's
//     split_pack_geometry, [geometricSplit]): the packs below the split are
//     the ROLL;
//   - DUE when the roll merges b's minimum (code.CompactMinMerge) or more, or when
//     more than code.CompactTargetPacks are not full and the roll merges two
//     at the least — and where that second rule meets a roll of fewer than
//     two, the two LIGHTEST packs are the roll, so a repository past the
//     target shrinks by one at the least at every poll;
//   - capped at code.MaxCompactInputs row ids and at byteCap pack bytes, the
//     LIGHTEST first either way — the cap keeps the progression's lower part,
//     which is where the next roll starts — the byte cap never below the two
//     lightest, so a cap halved past them still merges something, and the row
//     cap never passed at all ([capped]).
//
// A plan whose capped roll holds fewer than two packs is not due: one pack is
// nothing to merge.
func plan(live []code.PackRow, published map[string]bool, byteCap int64, b bounds) planned {
	files := map[string]*pack{}
	var order []*pack
	for _, row := range live {
		if published[row.GitChecksum] || row.FullAt(b.cut) {
			continue
		}
		p := files[row.GitChecksum]
		if p == nil {
			p = &pack{checksum: row.GitChecksum, objects: row.Objects, packBytes: row.Ref().PackBytes(),
				size: row.Size}
			files[row.GitChecksum] = p
			order = append(order, p)
		}
		p.ids = append(p.ids, row.PackID)
	}
	for _, p := range order {
		slices.Sort(p.ids)
	}
	slices.SortFunc(order, func(a, b *pack) int {
		return cmp.Or(cmp.Compare(a.objects, b.objects), cmp.Compare(a.ids[0], b.ids[0]))
	})
	weights := make([]int64, len(order))
	for i, p := range order {
		weights[i] = p.objects
	}
	roll := geometricSplit(weights, code.CompactFactor)
	out := planned{nonFull: len(order)}
	switch {
	case roll >= b.minMerge:
	case len(order) > code.CompactTargetPacks:
		// PAST THE TARGET, whatever the roll merges: two at the least.
		roll = max(roll, 2)
	default:
		return out
	}
	out.inputs = capped(order[:roll], code.MaxCompactInputs, byteCap)
	out.due = len(out.inputs) >= 2
	return out
}

// publishedPacks is every pack checksum a layer of rows names: what a
// compaction's plan leaves out.
func publishedPacks(rows []code.LayerRow) map[string]bool {
	out := map[string]bool{}
	for _, row := range rows {
		for _, cs := range row.Layer.Packs {
			out[cs] = true
		}
	}
	return out
}

// capped is the lightest part of roll — sorted lightest first — that holds at
// most maxIDs row ids and maxBytes pack bytes: the caps keep the lower part of
// the progression.
//
// THE ROW CAP IS ABSOLUTE: a plan naming more rows than one record retires is
// one its committer refuses (code.MaxCompactInputs), and the builder would
// build and refuse it again at every poll. A file named by more rows than are
// left is passed over rather than ending the roll — a pack pushed again and
// again byte for byte is one file under many rows — so the lighter files after
// it still merge. THE BYTE CAP never cuts below the two lightest packs taken,
// since a roll of two is the least a merge is, and a cap halved past them
// would otherwise merge nothing at all.
func capped(roll []*pack, maxIDs int, maxBytes int64) []pack {
	var (
		out   []pack
		ids   int
		bytes int64
	)
	for _, p := range roll {
		if ids+len(p.ids) > maxIDs {
			continue
		}
		if len(out) >= 2 && bytes+p.packBytes > maxBytes {
			break
		}
		out = append(out, *p)
		ids += len(p.ids)
		bytes += p.packBytes
	}
	return out
}

// recap is p's inputs capped again at maxBytes pack bytes, the lightest
// first and never below the two lightest — what a build cut smaller than the
// nominal cut merges ([InputByteCap] of that cut).
func (p planned) recap(maxBytes int64) planned {
	roll := make([]*pack, len(p.inputs))
	for i := range p.inputs {
		roll[i] = &p.inputs[i]
	}
	p.inputs = capped(roll, code.MaxCompactInputs, maxBytes)
	p.due = len(p.inputs) >= 2
	return p
}

// geometricSplit is git's own split of a repository's packs into the ones a
// geometric repack rolls up and the ones it keeps — repack.c's
// split_pack_geometry, at factor — over weights, the packs' object counts
// sorted ascending, answering how many of the lightest are rolled.
//
// GIT'S ALGORITHM, line for line, so a compaction merges exactly the packs
// `git repack --geometric=<factor>` would (TestTheRollIsGitsGeometricSplit
// holds it against the pinned git): walking down from the heaviest, the
// progression holds while each pack weighs at least factor times the one
// below it; the split is one right of the first pair that breaks it, or none
// when the whole list is a progression; then every pack at or above the split
// that weighs less than factor times the roll's total so far joins the roll,
// until the first that does not — a roll that swallowed a pack it would have
// left out would leave the rest out of progression with the new pack.
func geometricSplit(weights []int64, factor int64) int {
	n := len(weights)
	if n == 0 {
		return 0
	}
	i := n - 1
	for ; i > 0; i-- {
		if weights[i] < factor*weights[i-1] {
			break
		}
	}
	split := i
	if split > 0 {
		// The top element of the pair that broke the progression cannot be
		// in it either.
		split++
	}
	var total int64
	for _, w := range weights[:split] {
		total += w
	}
	for _, w := range weights[split:] {
		if w >= factor*total {
			break
		}
		split++
		total += w
	}
	return split
}
