package search

import (
	"container/heap"
	"sort"
)

// The two multipliers, and why conflating them is a real bug rather than a
// tidiness.
//
// They multiply different things at different stages, and the plain reading of
// "over-fetch" covers both:
//
//	SemanticOverfetch — the FILTER over-fetch: how many results the semantic
//	half must RETURN, so that post-filtering still leaves a full page.
//	BinaryOversample — the QUANTIZATION over-fetch: how many candidates stage
//	one must CONSIDER, so that the exact rerank has the right documents to
//	rank.
//
//	stage-1 depth  = FuseN × SemanticOverfetch × BinaryOversample = 1 200
//	returned depth = FuseN × SemanticOverfetch                    =   150
//
// Applying the RETURN ceiling to stage one gives an effective oversample of
// 3.3×, and 2× measured 0.6552 recall against 1.0000 at 8× — so the two
// ceilings are separate constants for the same reason the two multipliers
// are.
const (
	// FuseN is how many results each half contributes to the fusion.
	FuseN = 50

	// SemanticOverfetch is the filter over-fetch.
	SemanticOverfetch = 3

	// BinaryOversample is the quantization over-fetch.
	//
	// EIGHT, and it is nearly free here where every published system pays
	// real latency for it. Their stage one is a graph walk whose cost
	// grows with the depth; this one is a full scan of a narrow table
	// whose cost does not depend on K at all — measured, 2× → 8× moved
	// recall 0.6552 → 1.0000 while latency moved 147.6 → 143.0 ms, inside
	// the noise. When oversampling is free the correct move is to buy the
	// margin.
	BinaryOversample = 8

	// Stage1Depth and ReturnDepth are the shipped pair, derived from the
	// three above rather than written again.
	Stage1Depth = FuseN * SemanticOverfetch * BinaryOversample
	ReturnDepth = FuseN * SemanticOverfetch

	// BinaryCandidateCeiling caps stage one, and FuseCandidateCeiling caps
	// what enters the fusion. See the note above on why they are two.
	BinaryCandidateCeiling = 4_000
	FuseCandidateCeiling   = 500

	// FuseK is reciprocal rank fusion's constant, at the value the
	// literature ships.
	FuseK = 60
)

// BinaryRecallFloor is the quality gate's floor, PER CORPUS SIZE.
//
// # Why a curve and not a scalar
//
// Recall from a sign code is a decreasing function of the corpus size: the
// same generator family clears 0.98 comfortably at twenty thousand rows and
// misses it by a wide margin at half a million. A single number would
// certify the smallest system this engine runs on and say nothing about the
// largest it claims to support — so the floor is declared at three sizes and
// the value for a corpus in between is interpolated.
//
// Each value sits about a point of recall below what the seeded fixture
// measures, and the point is seed variance rather than slack. THEY ARE
// DECIDED VALUES RATHER THAN MEASURED ONES, and saying so is the point: a
// floor a first run defines cannot fail on that run. What makes them
// defensible is that synthetic isotropic clusters UNDERSTATE real 1-bit
// recall — real embedding spectra are heavy-tailed, and that anisotropy is
// exactly what binary quantization exploits — so a real corpus is expected to
// clear them, and one that does not is the signal the gate exists for.
//
// The escalation is decided in advance rather than discovered: raise
// [BinaryOversample] first, which measured free in latency; if that does not
// reach it, the documented fallback is an int8 first stage, as a code change
// in the same commit that changes the model default and never as a runtime
// knob.
var BinaryRecallFloor = map[int]float64{
	20_000:  0.98,
	120_000: 0.93,
	500_000: 0.88,
}

// FloorAt is the floor for a corpus of n sources, interpolated between the
// declared sizes and clamped outside them.
//
// LINEAR IN N rather than in log N, because the three declared points are
// close to linear over the range they span and a curve fitted through three
// points would be a claim about the shape that nothing measured.
func FloorAt(n int) float64 {
	sizes := []int{20_000, 120_000, 500_000}
	switch {
	case n <= sizes[0]:
		return BinaryRecallFloor[sizes[0]]
	case n >= sizes[len(sizes)-1]:
		return BinaryRecallFloor[sizes[len(sizes)-1]]
	}
	for i := 1; i < len(sizes); i++ {
		if n > sizes[i] {
			continue
		}
		lo, hi := sizes[i-1], sizes[i]
		t := float64(n-lo) / float64(hi-lo)
		return BinaryRecallFloor[lo] + t*(BinaryRecallFloor[hi]-BinaryRecallFloor[lo])
	}
	return BinaryRecallFloor[sizes[len(sizes)-1]]
}

// Stage1 is the candidate pool: the depth nearest codes by Hamming distance,
// in ascending distance with the index breaking ties.
//
// THE TIE BREAK IS THE INDEX, deliberately and to match the SQL, which orders
// by `vector_distance_cos(bits, :qb), source_id`. Hamming distance over a
// 3072-bit code takes at most 3 073 distinct values across a corpus of any
// size, so ties are the common case rather than the corner: without a
// declared tie break the candidate pool would depend on the scan's own row
// order and two nodes would rerank different pools.
func Stage1(codes [][]uint64, query []uint64, depth int) []int {
	if depth <= 0 || len(codes) == 0 {
		return nil
	}
	if depth > BinaryCandidateCeiling {
		depth = BinaryCandidateCeiling
	}
	top := newTopK(depth)
	for i, code := range codes {
		top.offer(i, float64(-Hamming(code, query)))
	}
	return top.ordered()
}

// Rerank orders a candidate pool by an exact similarity and keeps the top
// depth.
//
// THE SIMILARITY IS A CALLBACK rather than a slice of vectors, and that is
// what makes the half-million-row arm of the quality gate affordable: held as
// f32 a corpus that size is six gigabytes, so it holds only the 1-bit codes
// and streams the exact vectors past this function one at a time.
func Rerank(candidates []int, similarity func(int) float64, depth int) []int {
	if depth <= 0 || len(candidates) == 0 {
		return nil
	}
	top := newTopK(depth)
	for _, candidate := range candidates {
		top.offer(candidate, similarity(candidate))
	}
	return top.ordered()
}

// TwoStage is the shipped composition: scan the codes, rerank the pool
// exactly.
//
// The two depths are separate arguments because they are the two multipliers
// above, and a single "depth" would be the conflation this package's own
// constants are arranged to prevent.
func TwoStage(codes [][]uint64, query []uint64, similarity func(int) float64,
	stage1Depth, returnDepth int) []int {

	return Rerank(Stage1(codes, query, stage1Depth), similarity, returnDepth)
}

// Exact is the ground truth: the top depth by the same similarity, over the
// whole corpus.
//
// It exists HERE rather than only in the test, because it is what the
// operator's own evaluation compares against too — and a second
// implementation of "the exact answer" is a gate that can agree with itself
// while both halves are wrong.
func Exact(n int, similarity func(int) float64, depth int) []int {
	top := newTopK(depth)
	for i := range n {
		top.offer(i, similarity(i))
	}
	return top.ordered()
}

// Recall is the fraction of the exact answer a candidate list recovered.
func Recall(got, want []int) float64 {
	if len(want) == 0 {
		return 1
	}
	found := make(map[int]bool, len(got))
	for _, i := range got {
		found[i] = true
	}
	hits := 0
	for _, i := range want {
		if found[i] {
			hits++
		}
	}
	return float64(hits) / float64(len(want))
}

// MissRanks are the positions in the exact answer that a candidate list
// dropped, zero-based.
//
// # Why the gate reads this and not only the aggregate
//
// A recall of 0.98 is compatible with losing exactly the documents that
// matter. Measured on the pinned fixture: in the regime the design ships in,
// misses are tail-concentrated with none at all in the first ten ranks; in a
// regime one oversample step below it, they are uniform across the whole
// answer and the true best document is dropped on a quarter of the queries.
// The aggregate cannot tell those apart, and the difference is the entire
// question of whether the semantic half is worth having.
func MissRanks(got, want []int) []int {
	found := make(map[int]bool, len(got))
	for _, i := range got {
		found[i] = true
	}
	var missed []int
	for rank, i := range want {
		if !found[i] {
			missed = append(missed, rank)
		}
	}
	return missed
}

// topK keeps the best depth of an arbitrary stream, by score descending with
// the index breaking ties.
//
// # Why a bounded heap and not a sort
//
// The gate's ground truth is the exact top of a half-million-row corpus,
// asked once per query — a full sort is O(N log N) per query where the answer
// needs O(N log depth), and at a depth of 150 that is a factor of eight in
// the dominant cost of the whole quality gate. It matters in production for
// the same reason: `crewlet search eval` runs the same selection over an
// operator's own corpus.
//
// THE TIE BREAK IS THE INDEX, and it is not cosmetic. Hamming distance over a
// 3072-bit code takes at most 3 073 distinct values whatever the corpus size,
// so ties are the common case rather than the corner — and the SQL that does
// this in the database orders by `vector_distance_cos(bits, :qb), source_id`
// for exactly that reason. Without a declared tie break the candidate pool
// would depend on the scan's own row order, and two nodes would rerank
// different pools.
type topK struct {
	depth int
	worst entries
}

type entry struct {
	index int
	score float64
}

type entries []entry

func (e entries) Len() int { return len(e) }
func (e entries) Less(a, b int) bool {
	// A MIN-HEAP ON THE ANSWER'S OWN ORDER: the root is the entry the next
	// better one displaces, so "less" here means "worse".
	if e[a].score != e[b].score {
		return e[a].score < e[b].score
	}
	return e[a].index > e[b].index
}
func (e entries) Swap(a, b int) { e[a], e[b] = e[b], e[a] }
func (e *entries) Push(x any)   { *e = append(*e, x.(entry)) }
func (e *entries) Pop() any     { old := *e; n := len(old); x := old[n-1]; *e = old[:n-1]; return x }

func newTopK(depth int) *topK { return &topK{depth: depth} }

func (t *topK) offer(index int, score float64) {
	if t.depth <= 0 {
		return
	}
	if len(t.worst) < t.depth {
		heap.Push(&t.worst, entry{index: index, score: score})
		return
	}
	root := t.worst[0]
	if score < root.score || (score == root.score && index > root.index) {
		return
	}
	t.worst[0] = entry{index: index, score: score}
	heap.Fix(&t.worst, 0)
}

// ordered drains the heap into the answer's own order.
func (t *topK) ordered() []int {
	held := make([]entry, len(t.worst))
	copy(held, t.worst)
	sort.Slice(held, func(a, b int) bool {
		if held[a].score != held[b].score {
			return held[a].score > held[b].score
		}
		return held[a].index < held[b].index
	})
	out := make([]int, len(held))
	for i, e := range held {
		out[i] = e.index
	}
	return out
}
