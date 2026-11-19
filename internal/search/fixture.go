package search

import (
	"math"
	"math/rand/v2"
)

// The seeded corpus the deterministic gate runs against, generated rather than
// committed: five thousand real vectors at the shipped width are 61 MB of opaque
// floats, permanent in a public repository's history, and an environment-gated
// arm reading one is the gate that silently does not run. The engine's fake
// embedder cannot be the corpus either: its components are all non-negative, so
// sign-quantizing it yields a set-membership indicator rather than an embedding
// code, and the gate would compare a lexical first stage with a lexical ground
// truth and be incapable of failing.

// FixtureMeanPairCos is the mean pairwise cosine of the seeded corpus.
//
// Provenance: model text-embedding-3-large, width 3072 (native), a company's
// Doclet pages and Worklet work items, 5 000 sampled sources, fitted 2026-09
// with `crewlet search eval --fit`.
//
// It decides the gate's floor: a shared mean is a PER-COORDINATE DITHER on a
// threshold that would be zero, so an off-centre corpus makes a sign code
// EASIER (recall 0.6360 → 0.9832 as this rises 0 → 0.50), and a drifting mean
// would move the floor by a third of its range with every assertion passing.
// For the same reason the corpus mean is NEVER subtracted before quantizing:
// centring deletes that dither and costs 0.36 of recall. Not to be revisited.
const FixtureMeanPairCos = 0.50

// FixtureWidth is the shipped model's native dimension, which the fixture
// generates at because recall is a property of the width as well as of the
// distribution.
const FixtureWidth = 3072

// The spectrum is CHOSEN to put the fixture in the regime the recall floors
// describe, not the easiest one: a rank-32 corpus with a steep decay measures
// recall 1.0000 at every gate size, making the floors unreachable and the
// miss-rank assertion vacuous. These values were swept against measured recall
// at twenty thousand rows.
//
// CONSTANTS, never variables a test may set: every fixture constructor reads
// them and they run in parallel tests, so a setter would race every one.
const (
	// fixtureRank is how many directions carry the corpus's structure.
	//
	// Real embedding variance lives in a few dozen directions, which a sign
	// code exploits; a handful of shared directions with power-law weights
	// reproduces that, and makes a document's exact similarity a dot product
	// over THIRTY-TWO coefficients rather than three thousand, so the ground
	// truth scan the gate needs a hundred times is affordable beside the race
	// detector. Not more: cost is linear in it, recall is already inside the
	// real band, and more directions are more isotropic, which makes a sign
	// code WORSE and understates the floor.
	fixtureRank = 32

	// fixtureAlpha is the power law the directions' weights decay by:
	// direction k weighs 1/(k+1)^fixtureAlpha.
	fixtureAlpha = 0.4

	// fixtureSupport is how many coordinates each direction touches.
	fixtureSupport = FixtureWidth / 4
)

// Fixture is a seeded corpus: its 1-bit codes, and the coefficients its exact
// similarity is computed from.
//
// THE FULL VECTORS ARE NOT KEPT. They exist for exactly as long as it takes
// to quantize each one, because at half a million rows the f32 corpus is six
// gigabytes and the codes are 192 MB — and the exact similarity a rerank
// needs is recoverable from the coefficients alone, since every vector is a
// fixed combination of the same directions.
type Fixture struct {
	// Codes are the 1-bit codes, one per document.
	Codes [][]uint64

	// coefficients are each document's weights on the shared directions,
	// with its norm already folded in so a similarity is one dot product.
	coefficients [][]float32
	norms        []float32

	// mean is the shared component every document carries, in coefficient
	// space.
	meanWeight float32

	basis *basis

	// gen is how a document's structural coefficients are drawn — the
	// family member this fixture is, isotropic or topical.
	gen generator

	// Topics is the topic each document was drawn around, -1 throughout
	// the isotropic member: what a gate files documents in CONTAINERS by,
	// because a company files its documents by what they are about, and a
	// search scoped to one container is a search over one region of the
	// space rather than over a random sample of it.
	Topics []int32
}

// NewFixture generates n documents from the ISOTROPIC member of the family:
// low-rank structure, a shared mean, and no topics at all.
//
// SEEDED AND DETERMINISTIC: the same n and the same seed produce the same
// corpus on every machine, which is what makes a recall floor a gate rather
// than a coin flip.
func NewFixture(n int, seed uint64) *Fixture {
	basis := newBasis()
	return newFixture(n, seed, basis, generator{weights: basis.weights})
}

// NewTopicalFixture generates n documents from the TOPICAL member of the
// family: the same basis, spectrum and fitted mean, with every document drawn
// around one of [FixtureTopics] topic centres.
//
// The isotropic member is one continuous cloud, so an inverted file over it
// partitions arbitrarily and meeting the floor takes half the lists, while a
// company's documents cluster by project, space and subject. Measured only on
// the isotropic member the gate would certify an index never worth installing;
// only here, recall on a friendlier corpus than the worst it must survive. So
// [ADR-0028]'s gate runs both, and the training's own measurement decides which
// a company's corpus resembles.
func NewTopicalFixture(n int, seed uint64) *Fixture {
	basis := newBasis()
	return newFixture(n, seed, basis, topicalGenerator(basis.weights))
}

func newFixture(n int, seed uint64, basis *basis, gen generator) *Fixture {
	meanWeight := solveMeanWeight(gen)

	rng := rand.New(rand.NewPCG(seed, 0x5EEDC0DE))
	f := &Fixture{
		Codes:        make([][]uint64, n),
		coefficients: make([][]float32, n),
		norms:        make([]float32, n),
		Topics:       make([]int32, n),
		meanWeight:   meanWeight,
		basis:        basis,
		gen:          gen,
	}
	// A BLOCK OF DOCUMENTS IS DRAWN, THEN COMPOSED: every draw is the
	// stream's next, in document order, exactly as one document at a time
	// drew them — composing consumes nothing from the stream — so the block
	// changes how often the basis is read and never what a document is
	// ([basis.composeInto]).
	for at := 0; at < n; at += composeBlock {
		block := f.coefficients[at:min(at+composeBlock, n)]
		for j := range block {
			coefficients := make([]float32, len(basis.weights)+1)
			f.Topics[at+j] = gen.draw(rng, coefficients)
			// THE MEAN IS THE SAME DISPLACEMENT IN EVERY DOCUMENT, along
			// an axis nothing else uses — which is what makes it shared,
			// and what makes the achieved mean pairwise cosine the number
			// the constant declares.
			coefficients[len(basis.weights)] = meanWeight
			block[j] = coefficients
			f.norms[at+j] = norm(coefficients)
		}
		basis.composeInto(block, f.Codes[at:at+len(block)], nil)
	}
	return f
}

// The topical member's two parameters, CHOSEN rather than fitted, for the
// reason the spectrum is chosen: to put the gate where it can fail.
//
// Swept at 120 000 sources and 1 024 lists: at a within-topic spread of 0.5
// the index met the floor probing 1.5 % of the rows, a regime so easy no
// regression in the training could show; at 0.8 the smallest probe count with
// no head miss is a quarter of the lists, where a worse training has room to
// fail the floor. Five hundred and twelve topics is the order of a company's
// projects and wiki spaces times the subjects inside them — enough that a
// list holds a slice of a topic rather than several whole ones at every gate
// size.
const (
	FixtureTopics      = 512
	FixtureTopicSpread = 0.8
)

// generator draws one document's structural coefficients (every coordinate
// but the mean's).
type generator struct {
	weights []float32

	// centres are the topics' own coefficients, nil for the isotropic
	// member; spread is how far a document sits from its topic's centre, in
	// units of the spectrum's own weight.
	centres [][]float32
	spread  float32
}

// draw fills out's structural coordinates and returns the topic it drew
// around, -1 for the isotropic member. The isotropic member draws them
// exactly as the fixture always did — the same stream, the same order — so
// every recall figure measured before the family had a second member still
// describes the first.
func (g generator) draw(rng *rand.Rand, out []float32) int32 {
	if g.centres == nil {
		for k, w := range g.weights {
			out[k] = w * float32(rng.NormFloat64())
		}
		return -1
	}
	topic := rng.IntN(len(g.centres))
	centre := g.centres[topic]
	for k, w := range g.weights {
		out[k] = centre[k] + g.spread*w*float32(rng.NormFloat64())
	}
	return int32(topic)
}

// topicalGenerator places [FixtureTopics] centres on the spectrum.
//
// SEEDED BY THE BASIS'S OWN SEED, not the caller's, for the basis's reason:
// the topics are the distribution's structure rather than one corpus's
// content, so fixtures of two sizes are two samples of one company.
func topicalGenerator(weights []float32) generator {
	rng := rand.New(rand.NewPCG(fixtureSeed, 0x70D1C5))
	centres := make([][]float32, FixtureTopics)
	for t := range centres {
		centre := make([]float32, len(weights))
		for k, w := range weights {
			centre[k] = w * float32(rng.NormFloat64())
		}
		centres[t] = centre
	}
	return generator{weights: weights, centres: centres, spread: FixtureTopicSpread}
}

// basis is the corpus's shared structure: a set of directions with power-law
// weights, plus the axis the mean rides.
//
// The directions are SPARSE AND OVERLAPPING. A dense basis costs rank × width
// multiplications a document, forty-seven billion for the gate's middle corpus;
// sparse costs rank × support, independent of width. DISJOINT supports are the
// wrong sparse choice: each coordinate would be the sign of one coefficient, so
// the code would carry `rank` bits replicated and a sign code over it would
// measure the rank, not the geometry. Overlapping supports keep every
// coordinate a sum of a dozen or so coefficients, as a random rotation would.
type basis struct {
	weights []float32
	support [][]int32
	values  [][]float32

	// meanSupport and meanValues are the direction the shared mean rides.
	meanSupport []int32
	meanValues  []float32
}

func newBasis() *basis {
	rng := rand.New(rand.NewPCG(fixtureSeed, 0xBA515))
	b := &basis{
		weights: make([]float32, fixtureRank),
		support: make([][]int32, fixtureRank),
		values:  make([][]float32, fixtureRank),
	}
	for k := range fixtureRank {
		// A POWER LAW rather than an exponential: measured embedding
		// spectra decay polynomially, and an exponential one would make
		// the corpus effectively rank-three and the gate trivially easy.
		b.weights[k] = float32(1 / math.Pow(float64(k+1), fixtureAlpha))
		support := make([]int32, fixtureSupport)
		values := make([]float32, fixtureSupport)
		for i := range support {
			support[i] = int32(rng.IntN(FixtureWidth))
			values[i] = float32(rng.NormFloat64())
		}
		scale := float32(math.Sqrt(float64(fixtureSupport)))
		for i := range values {
			values[i] /= scale
		}
		b.support[k], b.values[k] = support, values
	}
	b.meanSupport = make([]int32, fixtureSupport)
	b.meanValues = make([]float32, fixtureSupport)
	scale := float32(math.Sqrt(float64(fixtureSupport)))
	for i := range b.meanSupport {
		b.meanSupport[i] = int32(rng.IntN(FixtureWidth))
		b.meanValues[i] = float32(rng.NormFloat64()) / scale
	}
	return b
}

// composeBlock is how many documents [basis.composeInto] composes in one pass
// over the basis: as many full-width float32 accumulators as fit under
// [maxStackVarBytes] — ten at the shipped width, 120 KiB. One more and the
// accumulator moves to the heap, where every write to it is instrumented
// again (TestTheFixtureComposesOnItsOwnStack). Each document more in a block
// is one share fewer of the basis's reads a document pays for, and at ten
// the sign code's own reads of each vector are already most of what is left.
const composeBlock = maxStackVarBytes / (FixtureWidth * 4)

// maxStackVarBytes is the largest variable the compiler keeps on a function's
// stack rather than moving it to the heap: 128 KiB, cmd/compile's
// MaxStackVarSize for a variable a function declares.
const maxStackVarBytes = 128 << 10

// composeInto composes each coefficient set in block (at most [composeBlock])
// into its full-width vector and writes its sign code to codes[j] and, where
// vectors is not nil, the vector to vectors[j]. Held to [basis.vector]'s, to
// the bit, by TestABlockComposesExactlyWhatOneDocumentAtATimeDoes.
//
// The accumulators are local arrays: the race detector never instruments a
// function's own stack, while the ~25 000 `+=` a document costs on a heap
// slice made the 120 000-row corpus take 116 s under `-race` (15 s now). The
// accumulator is COORDINATE-MAJOR so a basis entry touches one cache line.
// Codes are BIT-IDENTICAL to one document at a time: documents are the
// INNERMOST loop, so each coordinate takes its additions in [basis.vector]'s
// order, and a zero coefficient it adds rather than skips cannot move a bit.
func (b *basis) composeInto(block [][]float32, codes [][]uint64, vectors [][]float32) {
	if len(block) > composeBlock {
		panic("search: a fixture block larger than its accumulator")
	}
	// ONE FUNCTION, NO CLOSURE, AND NO SLICE OF THESE ARRAYS IN THE LOOPS:
	// the detector skips an address only when it can see the address is
	// this frame's own, and an array reached through a captured pointer or
	// a slice header is not one it can see that of.
	var (
		acc    [FixtureWidth][composeBlock]float32
		weight [composeBlock]float32
		vector [FixtureWidth]float32
	)
	for k := 0; k <= len(b.support); k++ {
		// THE MEAN RIDES A DIRECTION OF ITS OWN, last, drawn like the
		// others and carrying no noise.
		//
		// The obvious alternative — a constant added to every coordinate
		// — is not a mean, it is the all-ones direction, and a sign code
		// reads it as one particular threshold shift rather than as a
		// corpus's own centre. Measured: the uniform form costs about
		// five points of recall against a random direction of the same
		// magnitude, which is the fixture reporting a property of the
		// all-ones vector rather than of an off-centre corpus.
		support, values := b.meanSupport, b.meanValues
		if k < len(b.support) {
			support, values = b.support[k], b.values[k]
		}
		// A row past the block keeps the zero weight it was declared
		// with, so the fixed-length loop below adds nothing to it.
		touched := false
		for j, coefficients := range block {
			weight[j] = coefficients[k]
			touched = touched || weight[j] != 0
		}
		if !touched {
			continue
		}
		for i, d := range support {
			v := values[i]
			for j := range composeBlock {
				acc[d][j] += weight[j] * v
			}
		}
	}
	for j := range block {
		for d := range vector {
			vector[d] = acc[d][j]
		}
		if codes != nil {
			codes[j] = Quantize(vector[:])
		}
		if vectors != nil {
			copy(vectors[j], vector[:])
		}
	}
}

// vector is the full-width vector one coefficient set produces: THE
// ONE-DOCUMENT LOOP every recall figure quoted in this package was measured
// on (each direction along its support, the mean last, a zero coefficient
// skipped), which [basis.composeInto] reproduces to the bit for a block.
//
// ONE DOCUMENT IS NOT A BLOCK OF ONE: a block's accumulator is ten lanes and a
// 120 KiB frame however few hold a document, so composed as a block it costs
// ten (236–256 µs against 23–26 µs). Under the detector the block is cheaper,
// but a query composes one document at a time, and a benchmark writing vectors
// to a store never runs under the detector.
func (b *basis) vector(coefficients []float32) []float32 {
	vector := make([]float32, FixtureWidth)
	for k, c := range coefficients[:len(b.support)] {
		if c == 0 {
			continue
		}
		values := b.values[k]
		for i, d := range b.support[k] {
			vector[d] += c * values[i]
		}
	}
	if mean := coefficients[len(b.support)]; mean != 0 {
		for i, d := range b.meanSupport {
			vector[d] += mean * b.meanValues[i]
		}
	}
	return vector
}

// Len is how many documents the fixture holds.
func (f *Fixture) Len() int { return len(f.Codes) }

// Vector is document i as the full-width vector it was quantized from —
// recomposed from its coefficients, for the benchmark that writes a fixture
// into a real store rather than scanning its codes.
func (f *Fixture) Vector(i int) []float32 { return f.basis.vector(f.coefficients[i]) }

// QueryVector is the full-width vector of the query [Fixture.Query] draws for
// the same seed.
func (f *Fixture) QueryVector(seed uint64) []float32 {
	return f.basis.vector(f.queryCoefficients(seed))
}

// queryCoefficients is the draw behind a query.
//
// A SEPARATE STREAM from the corpus's, so adding a document does not change
// what the queries are. A shared stream would make every recall figure a
// function of the corpus size twice over — once through the retrieval and once
// through the questions.
func (f *Fixture) queryCoefficients(seed uint64) []float32 {
	rng := rand.New(rand.NewPCG(seed, 0xC0FFEE))
	coefficients := make([]float32, len(f.basis.weights)+1)
	f.gen.draw(rng, coefficients)
	coefficients[len(f.basis.weights)] = f.meanWeight
	return coefficients
}

// Query is a held-out draw from the same distribution: its code, and the
// coefficients an exact similarity against it is computed from.
func (f *Fixture) Query(seed uint64) (code []uint64, similarity func(int) float64) {
	coefficients := f.queryCoefficients(seed)
	code = Quantize(f.basis.vector(coefficients))
	queryNorm := norm(coefficients)
	return code, func(i int) float64 {
		dot := float32(0)
		for k, c := range f.coefficients[i] {
			dot += c * coefficients[k]
		}
		denominator := f.norms[i] * queryNorm
		if denominator == 0 {
			return 0
		}
		return float64(dot / denominator)
	}
}

// fixtureSeed is the BASIS's own seed, fixed for every corpus.
//
// The basis is the corpus's shared structure rather than one corpus's
// content, so it does not vary with the caller's seed: two fixtures of
// different sizes are two samples from ONE distribution, which is what makes
// the recall curve across the three gate sizes a curve rather than three
// unrelated numbers.
const fixtureSeed = 1

// solveMeanWeight finds the shared component that makes the corpus's mean
// pairwise cosine the fitted constant.
//
// SOLVED, not closed-form: |mean|² / (|mean|² + noise power) is the ratio of
// expectations, but each document is normalised by its OWN chi-squared norm, so
// by Jensen's inequality the achieved cosine lands several points above it (0.53
// for 0.50 asked). Three points matter: recall moves a third of its range across
// this number, so the floor would silently be elsewhere, and the solve lets the
// test assert the constant tightly. A bisection over a probe corpus, monotone in
// the weight, one pass over two thousand documents' coefficients, no full-width
// vectors.
func solveMeanWeight(gen generator) float32 {
	noisePower := 0.0
	for _, w := range gen.weights {
		noisePower += float64(w) * float64(w)
	}
	// A TOPICAL DOCUMENT CARRIES ITS CENTRE AND ITS SPREAD, so its noise
	// power is the spectrum's times (1 + spread²) and the bracket widens
	// with it.
	noisePower *= 1 + float64(gen.spread)*float64(gen.spread)
	lo, hi := 0.0, 8*math.Sqrt(noisePower)
	for range 40 {
		mid := (lo + hi) / 2
		if probeMeanCosine(gen, float32(mid)) < FixtureMeanPairCos {
			lo = mid
			continue
		}
		hi = mid
	}
	return float32((lo + hi) / 2)
}

// probeMeanCosine is the achieved mean pairwise cosine at one candidate
// weight, over a fixed probe corpus.
//
// THE PROBE IS SEEDED SEPARATELY from the corpus, so the solve's answer does
// not depend on how many documents the caller asked for — a generator whose
// mean moved with n would make every recall figure a function of the corpus
// size twice over.
func probeMeanCosine(gen generator, meanWeight float32) float64 {
	const probeDocs = 2_000
	rng := rand.New(rand.NewPCG(0xA11CE, 0x5EEDC0DE))
	coefficients := make([][]float32, probeDocs)
	norms := make([]float32, probeDocs)
	for i := range coefficients {
		c := make([]float32, len(gen.weights)+1)
		gen.draw(rng, c)
		c[len(gen.weights)] = meanWeight
		coefficients[i] = c
		norms[i] = norm(c)
	}
	total, pairs := 0.0, 0
	for i := 0; i+1 < probeDocs; i += 2 {
		dot := float32(0)
		for k := range coefficients[i] {
			dot += coefficients[i][k] * coefficients[i+1][k]
		}
		if d := norms[i] * norms[i+1]; d != 0 {
			total += float64(dot / d)
			pairs++
		}
	}
	if pairs == 0 {
		return 0
	}
	return total / float64(pairs)
}

func norm(v []float32) float32 {
	total := float64(0)
	for _, x := range v {
		total += float64(x) * float64(x)
	}
	return float32(math.Sqrt(total))
}

// MeanPairwiseCosine measures what the fixture actually produced, over a
// sample of pairs.
//
// THE FITTED CONSTANT IS ASSERTED BACK against this, because a generator
// whose mean silently drifts is a gate whose floor silently moves — and the
// floor moves by a third of its range across the span this number covers.
func (f *Fixture) MeanPairwiseCosine(pairs int, seed uint64) float64 {
	if f.Len() < 2 {
		return 0
	}
	rng := rand.New(rand.NewPCG(seed, 0xFACE))
	total := 0.0
	for range pairs {
		a := rng.IntN(f.Len())
		b := rng.IntN(f.Len())
		for b == a {
			b = rng.IntN(f.Len())
		}
		dot := float32(0)
		for k := range f.coefficients[a] {
			dot += f.coefficients[a][k] * f.coefficients[b][k]
		}
		denominator := f.norms[a] * f.norms[b]
		if denominator != 0 {
			total += float64(dot / denominator)
		}
	}
	return total / float64(pairs)
}
