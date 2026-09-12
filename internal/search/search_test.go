package search_test

import (
	"fmt"
	"math"
	"math/rand/v2"
	"slices"
	"sync"
	"testing"

	"github.com/crewlet/crewlet/internal/search"
)

// AN EXACT RERANK OVER A 1-BIT POOL RECOVERS THE EXACT RANKING.
//
// # What this gate measures, and what it deliberately does not
//
// The first stage keeps only each vector's ORTHANT. How much an orthant says
// about cosine rank is a property of the corpus's own distribution and of
// nothing else — over a family of embedding-shaped generators the same
// arithmetic spans 0.29 to 0.98 recall — so this measures the ARITHMETIC and
// never recall on a particular company's documents. `crewlet search eval` is
// what answers that, against that company's own vectors.
//
// # Why the floor is a curve
//
// Recall from a sign code DECREASES with the corpus size. Measured on this
// fixture at the shipped depth: 0.9888 at twenty thousand, 0.9677 at a
// hundred and twenty thousand, 0.9287 at half a million. A single scalar
// would certify the smallest system this engine runs on and say nothing about
// the largest it claims to support.
func TestBinaryRecallFloorAtShippedDepth(t *testing.T) {
	t.Parallel()
	for _, n := range gateSizes {
		t.Run(fmt.Sprint(n), func(t *testing.T) {
			t.Parallel()
			f := gateFixture(n)

			// THE FITTED CONSTANT IS ASSERTED BACK. Recall at the
			// shipped oversample moves by a third of its range across
			// the span this number covers, so a generator whose mean
			// silently drifted would move the gate's own floor while
			// every assertion below still passed.
			if mean := f.MeanPairwiseCosine(2_000, 7); math.Abs(mean-search.FixtureMeanPairCos) > 0.02 {
				t.Fatalf("the fixture's mean pairwise cosine is %.4f and the "+
					"fitted constant is %.4f — the floor below was declared "+
					"against the constant, and a corpus that is not at it is "+
					"being measured against somebody else's floor",
					mean, search.FixtureMeanPairCos)
			}

			recall := 0.0
			for q := range gateQueries {
				code, similarity := f.Query(uint64(1000 + q))
				want := search.Exact(f.Len(), similarity, search.ReturnDepth)
				got := search.TwoStage(f.Codes, code, similarity,
					search.Stage1Depth, search.ReturnDepth)
				recall += search.Recall(got, want)
			}
			recall /= float64(gateQueries)

			if floor := search.FloorAt(n); recall < floor {
				t.Fatalf("recall@%d from a stage-1 depth of %d is %.4f at "+
					"%d sources, below the %.4f floor — raise "+
					"BinaryOversample first, which measured free in latency, "+
					"and fall back to an int8 first stage in the same commit "+
					"that changes the model default",
					search.ReturnDepth, search.Stage1Depth, recall, n, floor)
			}
		})
	}
}

// MISSES STAY OFF THE HEAD OF THE ANSWER.
//
// # Why the aggregate is not enough on its own
//
// A recall of 0.98 is compatible with losing exactly the documents that
// matter. Measured on this fixture: at the shipped oversample the misses are
// tail-concentrated with NONE at all in the first ten ranks, at every corpus
// size; one step down, at 2×, the head starts losing documents while the
// aggregate is still respectable. The aggregate cannot tell those apart, and
// the difference is the whole question of whether the semantic half is worth
// having — a semantic-only document that stage one drops leaves the fused
// answer entirely.
func TestBinaryMissesStayOffTheHead(t *testing.T) {
	t.Parallel()
	for _, n := range gateSizes {
		t.Run(fmt.Sprint(n), func(t *testing.T) {
			t.Parallel()
			f := gateFixture(n)
			head := 0
			for q := range gateQueries {
				code, similarity := f.Query(uint64(1000 + q))
				want := search.Exact(f.Len(), similarity, search.ReturnDepth)
				got := search.TwoStage(f.Codes, code, similarity,
					search.Stage1Depth, search.ReturnDepth)
				for _, rank := range search.MissRanks(got, want) {
					if rank < 10 {
						head++
					}
				}
			}
			if head != 0 {
				t.Fatalf("the candidate pool dropped %d document(s) from the "+
					"first ten ranks at %d sources — an aggregate recall this "+
					"gate would still pass is compatible with losing exactly "+
					"the documents the semantic half exists to find", head, n)
			}
		})
	}
}

// gateFixture builds each corpus ONCE and shares it between the cases.
//
// The fixture is deterministic, so two cases building it twice produce two
// identical corpora at twice the cost — and generation is what the gate's
// whole wall clock is, so sharing it halves the price every contributor pays
// on every run.
func gateFixture(n int) *search.Fixture { return fixtures[n]() }

// ONE MEMO PER SIZE rather than one lock over all of them: the cases for
// different sizes are meant to build in parallel, and a single mutex would
// serialise the two largest generations behind each other — measured, that is
// slower than building all four.
var fixtures = map[int]func() *search.Fixture{
	20_000:  sync.OnceValue(func() *search.Fixture { return search.NewFixture(20_000, 1) }),
	120_000: sync.OnceValue(func() *search.Fixture { return search.NewFixture(120_000, 1) }),
}

// gateSizes are the corpus sizes the pull request gates on.
//
// The half-million arm is a BENCHMARK rather than a gate, and the reason is
// its cost: generating it is twenty seconds against six for these two, and a
// gate every contributor runs is the wrong place for it. It is measured in
// BenchmarkSemanticScanUnderLoad, where a number that moves is read rather
// than waited on.
var gateSizes = []int{20_000, 120_000}

// gateQueries is how many held-out queries each size is measured over.
//
// TWENTY-FIVE, which puts the standard error of a recall near 0.97 at about
// 0.003 — an order of magnitude under the margin between the measured curve
// and the declared floor, so a passing run is a measurement rather than a
// coin flip.
const gateQueries = 25

// THE QUANTIZER IS A SIGN FUNCTION, AND ZERO TAKES THE NEGATIVE BIT.
//
// Both halves are measurements against the driver rather than conventions:
// the stage-1 scan orders values the driver's own vector1bit produced, so a
// disagreement here is a pure gate certifying a ranking the database does not
// produce. The zero is not a corner case — a truncated or padded embedding
// carries exact zeros in whole dimensions.
func TestQuantizeIsASignFunctionWithZeroNegative(t *testing.T) {
	t.Parallel()
	code := search.Quantize([]float32{0, 1, -1, 0, 0.5, -0.5, 2, -2})
	// LSB-first: bit i at position i%64 of word i/64.
	var want uint64
	for i, set := range []bool{false, true, false, false, true, false, true, false} {
		if set {
			want |= 1 << uint(i)
		}
	}
	if len(code) != 1 || code[0] != want {
		t.Fatalf("Quantize gave %#x, want %#x — the driver's own encoding of "+
			"the same vector is 0x52, whose bits read LSB-first are "+
			"[0,1,0,0,1,0,1,0]", code, want)
	}
	if search.Hamming(code, code) != 0 {
		t.Fatal("a code differs from itself")
	}
	if search.CodeBytes(3072) != 387 || search.CodeBytes(1536) != 195 {
		t.Fatalf("the encoded size is %d at 3072 and %d at 1536, and the "+
			"driver's own are 387 and 195 — the table's size arithmetic is "+
			"written against those",
			search.CodeBytes(3072), search.CodeBytes(1536))
	}
}

// FUSION IS NOT MONOTONE IN THE SEMANTIC LIST.
//
// # The claim this replaces
//
// It is tempting to say rank fusion makes semantic recall a lower bound on
// answer quality — that a document stage one drops merely slides down. It is
// false, and the arithmetic says so: a document ABSENT from a list contributes
// nothing from it, so a drop is the LARGEST possible perturbation to that
// document's score rather than an invisible one.
//
// What fusion absorbs is the drop of a document the keyword half also found.
// What it does not absorb is the drop of a SEMANTIC-ONLY document, which
// leaves the fused answer entirely — and that is exactly the class the
// semantic half exists for, the question whose answer shares no word with it.
func TestFuseIsNotMonotoneInTheSemanticList(t *testing.T) {
	t.Parallel()
	keyword := []string{"shared-a", "shared-b", "shared-c"}
	semantic := []string{"semantic-only", "shared-c", "shared-a"}

	full := search.Fuse(keyword, semantic)
	if !slices.Contains(full, "semantic-only") {
		t.Fatal("the fixture's own semantic-only document is not in the fused " +
			"answer, so this case asserts nothing")
	}

	// STAGE ONE DROPS IT. Nothing else changes.
	dropped := search.Fuse(keyword, []string{"shared-c", "shared-a"})
	if slices.Contains(dropped, "semantic-only") {
		t.Fatal("a document only the semantic half found survived being " +
			"dropped from the semantic list — fusion cannot recover it, " +
			"because a document absent from a list contributes nothing from it")
	}

	// AND A DOCUMENT BOTH HALVES FOUND SURVIVES THE SAME DROP, sliding
	// down rather than vanishing. That is the difference the claim above
	// conflates.
	if !slices.Contains(dropped, "shared-b") {
		t.Fatal("a document the keyword half also found vanished, so this " +
			"case cannot distinguish the two")
	}

	// THE ARITHMETIC BEHIND IT, asserted rather than described: at k = 60
	// a document in BOTH lists at any rank beats a semantic-only document
	// at rank one, so a semantic-only document's best possible fused
	// position is already one past the overlap.
	both := search.FusedScore("x", []string{"x"}, []string{"x"})
	onlyOne := search.FusedScore("y", []string{}, []string{"y"})
	if !(both > onlyOne) {
		t.Fatalf("a document in both lists scores %v and a semantic-only "+
			"rank-1 document scores %v", both, onlyOne)
	}
	// AND THE CROSSOVER IS PINNED, not described: a document in both lists
	// beats a semantic-only rank-1 document at every rank below 62 and
	// exactly ties at 62. That number is the fusion constant's doing, so
	// changing k moves where the two halves trade off — which is the one
	// consequence a reader of `FuseK = 60` would not otherwise see.
	atRank := func(rank int) float64 {
		list := make([]string, rank)
		list[rank-1] = "z"
		return search.FusedScore("z", list, list)
	}
	if !(atRank(61) > onlyOne) {
		t.Fatalf("a document in both lists at rank 61 scores %v, not above a "+
			"semantic-only rank-1 document's %v", atRank(61), onlyOne)
	}
	if atRank(62) != onlyOne {
		t.Fatalf("a document in both lists at rank 62 scores %v and a "+
			"semantic-only rank-1 document scores %v — the crossover is "+
			"exactly there, and a fusion constant that moved it would change "+
			"which half wins a tie", atRank(62), onlyOne)
	}
}

// THE TIE BREAK IS THE INDEX, AT BOTH STAGES.
//
// Hamming distance over a 3072-bit code takes at most 3 073 distinct values
// whatever the corpus size, so ties are the common case rather than the
// corner. The SQL that does this in the database orders by
// `vector_distance_cos(bits, :qb), source_id` for the same reason: without a
// declared tie break the candidate pool depends on the scan's own row order,
// and two nodes rerank different pools.
func TestSelectionBreaksTiesOnTheIndex(t *testing.T) {
	t.Parallel()
	// Four identical codes: every distance is the same.
	codes := [][]uint64{{0b1010}, {0b1010}, {0b1010}, {0b1010}}
	got := search.Stage1(codes, []uint64{0b1010}, 2)
	if !slices.Equal(got, []int{0, 1}) {
		t.Fatalf("Stage1 over identical codes gave %v, want [0 1]", got)
	}
	flat := search.Rerank([]int{3, 1, 2, 0}, func(int) float64 { return 1 }, 2)
	if !slices.Equal(flat, []int{0, 1}) {
		t.Fatalf("Rerank over identical scores gave %v, want [0 1]", flat)
	}
}

// THE BOUNDED SELECTION AGREES WITH A FULL SORT.
//
// It is what makes the gate affordable — a full sort is O(N log N) per query
// where the answer needs O(N log depth) — so it has to produce the same
// answer, including the tie break, on every input.
func TestBoundedSelectionAgreesWithAFullSort(t *testing.T) {
	t.Parallel()
	rng := rand.New(rand.NewPCG(4, 5))
	for range 200 {
		n := 1 + rng.IntN(300)
		scores := make([]float64, n)
		for i := range scores {
			// COARSE SCORES, deliberately: a continuous score would
			// make every tie break unreachable, and the tie is the
			// common case in the real thing.
			scores[i] = float64(rng.IntN(5))
		}
		depth := 1 + rng.IntN(n)
		candidates := make([]int, n)
		for i := range candidates {
			candidates[i] = i
		}
		got := search.Rerank(candidates, func(i int) float64 { return scores[i] }, depth)

		want := slices.Clone(candidates)
		slices.SortStableFunc(want, func(a, b int) int {
			switch {
			case scores[a] != scores[b]:
				if scores[a] > scores[b] {
					return -1
				}
				return 1
			}
			return a - b
		})
		want = want[:depth]
		if !slices.Equal(got, want) {
			t.Fatalf("bounded selection gave %v and a full sort gives %v", got, want)
		}
	}
}

// THE FLOOR INTERPOLATES BETWEEN THE SIZES IT DECLARES.
//
// The corpus a company actually has is never one of the three, and a floor
// that only answered at the declared points would be read as the nearest one
// — which at half a million against a hundred and twenty thousand is five
// points of recall.
func TestTheRecallFloorIsACurve(t *testing.T) {
	t.Parallel()
	if got := search.FloorAt(20_000); got != 0.98 {
		t.Errorf("FloorAt(20 000) = %v", got)
	}
	if got := search.FloorAt(1_000); got != 0.98 {
		t.Errorf("FloorAt below the smallest declared size = %v, and a corpus "+
			"smaller than the gate's own is not a corpus with no floor", got)
	}
	if got := search.FloorAt(2_000_000); got != 0.88 {
		t.Errorf("FloorAt above the largest declared size = %v", got)
	}
	middle := search.FloorAt(70_000)
	if !(middle < 0.98 && middle > 0.93) {
		t.Errorf("FloorAt(70 000) = %v, which is not between the floors it "+
			"sits between", middle)
	}
	// AND IT DECREASES, which is the shape the whole per-size declaration
	// exists for.
	previous := 1.0
	for n := 20_000; n <= 500_000; n += 20_000 {
		got := search.FloorAt(n)
		if got > previous {
			t.Fatalf("the floor rises from %v to %v at %d sources", previous, got, n)
		}
		previous = got
	}
}
