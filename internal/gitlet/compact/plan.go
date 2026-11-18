package compact

import (
	"cmp"
	"slices"

	"github.com/crewlet/crewlet/internal/gitlet"
)

// InputByteCap is the most PACK bytes one roll, a compaction's or a
// publication's repack's, merges at a cut of cut: MaxCompactOutputs/2 cuts,
// 8 GiB at the nominal cut (gitlet.CompactPackBytes).
//
// HALF, because pack-objects closes an output before --max-pack-size and
// writes WHOLE a delta whose base went to an earlier output
// (builtin/pack-objects.c's write_one, 2.56.0), so outputs can outnumber cuts.
// At half, any two consecutive outputs pass one cut; a build past
// MaxCompactOutputs anyway is refused before any upload and halves the next
// plan ([Compactor.Compact]). BenchmarkCompact's cap= arm measured 1.0 outputs
// a cut (M3-decisions M3-O29); the half stays so the bound never rests on how
// one history deltas (package doc, "The builds' bounds").
func InputByteCap(cut int64) int64 { return gitlet.MaxCompactOutputs / 2 * cut }

// pack is one FILE of a repository's live set as a plan weighs it: a git
// checksum and every live row naming it. Two rows of one checksum (a pack
// pushed twice) are one input: read once, both ids retired. Any row's figures
// are the file's, since a checksum names one pack's bytes.
type pack struct {
	checksum string
	// ids are the rows naming the file, sorted.
	ids []string
	// objects is the file's object count, its WEIGHT as git's geometric
	// repack weighs a pack (repack.c's geometry_pack_weight); packBytes its
	// pack section and size its whole bundle.
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

// size is the inputs' whole bundles, each file once: about what the outputs
// will weigh in the object store, the estimate a full store holds them back by.
func (p planned) size() int64 {
	var n int64
	for _, in := range p.inputs {
		n += in.size
	}
	return n
}

// plan plans one compaction of live, its roll capped at byteCap, PURE with no
// clock, so the not-due [memo] keys on the only inputs that move, the
// repository's version and its cap (a plan beside a reservation is never
// memoized, [Compactor.compact]). Left out: leftOut's checksums (the packs a
// published layer names, which the decide's rule 2 refuses to retire and its
// publications' repacks merge, layerplan.go; and those an in-flight
// publication here reserved, [Compactor.claimCompaction]) and every pack FULL
// at b's cut (the decide's predicate; never merged again, so a published layer
// stays valid until an expunge). The rest, one input per file, lightest first,
// are split at gitlet.CompactFactor ([geometricSplit]); the roll is DUE at
// b.minMerge packs or, past gitlet.CompactTargetPacks not full, at the two
// lightest at least, so such a repository shrinks every poll; then [capped].
func plan(live []gitlet.PackRow, leftOut map[string]bool, byteCap int64, b bounds) planned {
	files := map[string]*pack{}
	var order []*pack
	for _, row := range live {
		if leftOut[row.GitChecksum] || row.FullAt(b.cut) {
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
	roll := geometricSplit(weights, gitlet.CompactFactor)
	out := planned{nonFull: len(order)}
	switch {
	case roll >= b.minMerge:
	case len(order) > gitlet.CompactTargetPacks:
		// PAST THE TARGET, whatever the roll merges: two at the least.
		roll = max(roll, 2)
	default:
		return out
	}
	out.inputs = capped(order[:roll], gitlet.MaxCompactInputs, byteCap)
	out.due = len(out.inputs) >= 2
	return out
}

// publishedPacks is every pack checksum a layer of rows names: what a
// compaction's plan leaves out beside a publication's reservation.
func publishedPacks(rows []gitlet.LayerRow) map[string]bool {
	out := map[string]bool{}
	for _, row := range rows {
		for _, cs := range row.Layer.Packs {
			out[cs] = true
		}
	}
	return out
}

// capped is the lightest part of roll (sorted lightest first) within maxIDs row
// ids and maxBytes pack bytes, keeping the lower part of the progression.
//
// THE ROW CAP IS ABSOLUTE: a committer refuses a plan naming more rows than one
// record retires (gitlet.MaxCompactInputs), and every poll would rebuild it. A
// file named by more rows than are left (one pack pushed many times) is passed
// over, not the roll's end, so lighter files after it still merge. THE BYTE CAP
// never cuts below two packs, the least a merge is, or a halved cap would merge
// nothing.
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
	p.inputs = capped(roll, gitlet.MaxCompactInputs, maxBytes)
	p.due = len(p.inputs) >= 2
	return p
}

// geometricSplit is how many of the lightest packs git's geometric repack rolls
// up (repack.c's split_pack_geometry) at factor, over weights, the packs'
// object counts sorted ascending.
//
// GIT'S ALGORITHM line for line, so a compaction merges exactly what
// `git repack --geometric=<factor>` would (TestTheRollIsGitsGeometricSplit):
// from the heaviest down, the progression holds while each pack weighs at
// least factor times the one below; the split is one right of the first pair
// that breaks it (none if none does); then each pack at or above it lighter
// than factor times the roll so far joins, until the first that is not, since
// swallowing one it would leave out breaks the rest's progression.
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
