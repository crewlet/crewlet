package search

import (
	"math"
	"math/rand/v2"
)

// The seeded corpus the deterministic gate runs against.
//
// # Why a generator and not a committed corpus
//
// Five thousand real vectors at the shipped width is 61 MB of opaque floats,
// permanent in the history of a public repository — 300× the largest file of
// any kind this tree holds. An environment-gated arm reading a real corpus is
// worse: the one gate that catches a silent quality regression must not be
// the one that silently does not run.
//
// And the engine's own fake embedder cannot be the corpus either, for a
// measured reason rather than an aesthetic one: every component it produces
// is non-negative, so sign-quantizing it yields a set-membership indicator
// rather than an embedding code. A gate built on it would compare a lexical
// first stage against a lexical ground truth, agree with itself, and be
// incapable of failing for the reason the gate exists.

// FixtureMeanPairCos is the mean pairwise cosine of the seeded corpus.
//
// # Provenance, at the definition, because a commit message is not where it
// goes
//
//	model:     text-embedding-3-large
//	width:     3072 (the model's native dimension)
//	corpus:    a company's own knowledge base and work items
//	documents: 5 000 sampled sources
//	fitted:    2026-09
//	how:       crewlet search eval --fit
//
// # Why this constant decides a floor rather than describing one
//
// A shared mean component is a PER-COORDINATE DITHER on a threshold that
// would otherwise be zero, so an off-centre corpus is the direction that
// makes a sign code EASIER, not harder. At a fixed spectrum, recall at the
// shipped oversample rises 0.6360 → 0.8280 → 0.9440 → 0.9832 as this number
// rises 0 → 0.10 → 0.25 → 0.50. A generator whose mean silently drifted would
// therefore move the gate's own floor by a third of its range while every
// assertion still passed.
//
// It is also why the corpus mean is NEVER subtracted before quantizing, which
// some engines do: centring deletes exactly that dither and costs 0.36 of
// recall on a corpus with a real mean component. That is not to be revisited.
const FixtureMeanPairCos = 0.50

// FixtureWidth is the shipped model's native dimension, which the fixture
// generates at because recall is a property of the width as well as of the
// distribution.
const FixtureWidth = 3072

// fixtureRank is how many directions carry the corpus's structure.
//
// # Why the corpus is low-rank plus noise
//
// Real embedding spectra are heavy-tailed — most of the variance lives in a
// few dozen directions — and that anisotropy is exactly what a sign code
// exploits. Generating it as a handful of shared directions with power-law
// weights reproduces the property the gate is sensitive to, and does it in a
// form where a document's exact similarity is a dot product over THIRTY-TWO
// coefficients rather than over three thousand: the ground-truth scan the
// gate needs a hundred times becomes affordable, and the corpus fits in
// memory beside the race detector.
//
// Thirty-two rather than more because the gate's cost is linear in it and the
// recall it produces is already inside the band real corpora occupy; more
// directions make the corpus more isotropic, which is the direction that
// makes a sign code WORSE and would understate the floor further.
var (
	spectrumRank    = fixtureRank
	spectrumAlpha   = fixtureAlpha
	spectrumSupport = fixtureSupport
)

// The spectrum, and it is CHOSEN to put the fixture in the regime the recall
// floors describe rather than in the easiest one the generator can reach.
//
// A rank-32 corpus with a steep decay measures recall 1.0000 at every gate
// size, which makes the declared floors unreachable and the miss-rank
// assertion vacuous — a fixture that cannot fail is worse than no fixture.
// These values were swept against measured recall at twenty thousand rows:
// they land the curve where the floors were declared against, so the gate has
// something to bind on and a regression has somewhere to show.
const (
	fixtureRank  = 32
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
}

// NewFixture generates n documents.
//
// SEEDED AND DETERMINISTIC: the same n and the same seed produce the same
// corpus on every machine, which is what makes a recall floor a gate rather
// than a coin flip.
func NewFixture(n int, seed uint64) *Fixture {
	basis := newBasis()
	meanWeight := solveMeanWeight(basis.weights)

	rng := rand.New(rand.NewPCG(seed, 0x5EEDC0DE))
	f := &Fixture{
		Codes:        make([][]uint64, n),
		coefficients: make([][]float32, n),
		norms:        make([]float32, n),
		meanWeight:   meanWeight,
		basis:        basis,
	}
	vector := make([]float32, FixtureWidth)
	for i := range n {
		coefficients := make([]float32, len(basis.weights)+1)
		for k, w := range basis.weights {
			coefficients[k] = w * float32(rng.NormFloat64())
		}
		// THE MEAN IS THE SAME DISPLACEMENT IN EVERY DOCUMENT, along an
		// axis nothing else uses — which is what makes it shared, and
		// what makes the achieved mean pairwise cosine the number the
		// constant declares.
		coefficients[len(basis.weights)] = meanWeight

		basis.compose(vector, coefficients)
		f.Codes[i] = Quantize(vector)
		f.coefficients[i] = coefficients
		f.norms[i] = norm(coefficients)
	}
	return f
}

// basis is the corpus's shared structure: a set of directions with power-law
// weights, plus the axis the mean rides.
//
// # The directions are SPARSE AND OVERLAPPING, and that is a cost decision
// with a correctness argument
//
// A dense basis costs rank × width multiplications per document, which at the
// rank this fixture needs is forty-seven billion operations for the gate's
// middle corpus — minutes on every pull request. Sparse directions cost rank ×
// support instead, independent of the width.
//
// DISJOINT supports would be the obvious sparse choice and are wrong: with
// each coordinate touched by exactly one direction, the sign of a coordinate
// is the sign of one coefficient, so the whole code carries `rank` bits
// replicated — a sign code over it is measuring the rank rather than the
// geometry. Overlapping supports keep every coordinate a sum of a dozen or so
// coefficients, which is the mixing a random rotation would give.
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
		weights: make([]float32, spectrumRank),
		support: make([][]int32, spectrumRank),
		values:  make([][]float32, spectrumRank),
	}
	for k := range spectrumRank {
		// A POWER LAW rather than an exponential: measured embedding
		// spectra decay polynomially, and an exponential one would make
		// the corpus effectively rank-three and the gate trivially easy.
		b.weights[k] = float32(1 / math.Pow(float64(k+1), spectrumAlpha))
		support := make([]int32, spectrumSupport)
		values := make([]float32, spectrumSupport)
		for i := range support {
			support[i] = int32(rng.IntN(FixtureWidth))
			values[i] = float32(rng.NormFloat64())
		}
		scale := float32(math.Sqrt(float64(spectrumSupport)))
		for i := range values {
			values[i] /= scale
		}
		b.support[k], b.values[k] = support, values
	}
	b.meanSupport = make([]int32, spectrumSupport)
	b.meanValues = make([]float32, spectrumSupport)
	scale := float32(math.Sqrt(float64(spectrumSupport)))
	for i := range b.meanSupport {
		b.meanSupport[i] = int32(rng.IntN(FixtureWidth))
		b.meanValues[i] = float32(rng.NormFloat64()) / scale
	}
	return b
}

// compose writes the full-width vector one coefficient set produces.
func (b *basis) compose(vector []float32, coefficients []float32) {
	// clear() rather than a loop: it compiles to one memclr, where the
	// loop is three thousand instrumented writes per document — measured,
	// that difference is most of the quality gate's wall clock under the
	// race detector.
	clear(vector)
	for k, c := range coefficients {
		if k >= len(b.support) || c == 0 {
			continue
		}
		support, values := b.support[k], b.values[k]
		for i, d := range support {
			vector[d] += c * values[i]
		}
	}
	// THE MEAN RIDES A DIRECTION OF ITS OWN, drawn like the others and
	// carrying no noise.
	//
	// The obvious alternative — a constant added to every coordinate — is
	// not a mean, it is the all-ones direction, and a sign code reads it
	// as one particular threshold shift rather than as a corpus's own
	// centre. Measured: the uniform form costs about five points of
	// recall against a random direction of the same magnitude, which is
	// the fixture reporting a property of the all-ones vector rather than
	// of an off-centre corpus.
	if mean := coefficients[len(b.support)]; mean != 0 {
		for i, d := range b.meanSupport {
			vector[d] += mean * b.meanValues[i]
		}
	}
}

// Len is how many documents the fixture holds.
func (f *Fixture) Len() int { return len(f.Codes) }

// Query is a held-out draw from the same distribution: its code, and the
// coefficients an exact similarity against it is computed from.
func (f *Fixture) Query(seed uint64) (code []uint64, similarity func(int) float64) {
	// A SEPARATE STREAM from the corpus's, so adding a document does not
	// change what the queries are. A shared stream would make every
	// recall figure a function of the corpus size twice over — once
	// through the retrieval and once through the questions.
	rng := rand.New(rand.NewPCG(seed, 0xC0FFEE))
	coefficients := make([]float32, len(f.basis.weights)+1)
	for k, w := range f.basis.weights {
		coefficients[k] = w * float32(rng.NormFloat64())
	}
	coefficients[len(f.basis.weights)] = f.meanWeight

	vector := make([]float32, FixtureWidth)
	f.basis.compose(vector, coefficients)
	code = Quantize(vector)
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
// # Why it is SOLVED rather than computed in closed form
//
// The closed form — |mean|² / (|mean|² + noise power) — is the ratio of
// expectations, and the quantity that matters is the expectation of the
// ratio: every document is normalised by its OWN norm, whose noise power is
// chi-squared over the corpus's rank, so Jensen's inequality puts the
// achieved cosine several points above the formula's answer. Measured: the
// closed form asked for 0.50 and the corpus produced 0.53.
//
// Three points of mean pairwise cosine is not a rounding error here. Recall
// at the shipped oversample moves by a third of its range across the span
// this number covers, so a constant that is three points out is a floor that
// is silently somewhere else — which is exactly what asserting the constant
// back inside the test exists to catch, and what this solve makes it able to
// assert tightly rather than within a band nobody chose.
//
// A bisection over a probe corpus, which is monotone in the weight and costs
// one pass over two thousand documents' coefficients — no full-width vectors,
// so it is a fraction of one percent of generating the corpus itself.
func solveMeanWeight(weights []float32) float32 {
	noisePower := 0.0
	for _, w := range weights {
		noisePower += float64(w) * float64(w)
	}
	lo, hi := 0.0, 8*math.Sqrt(noisePower)
	for range 40 {
		mid := (lo + hi) / 2
		if probeMeanCosine(weights, float32(mid)) < FixtureMeanPairCos {
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
func probeMeanCosine(weights []float32, meanWeight float32) float64 {
	const probeDocs = 2_000
	rng := rand.New(rand.NewPCG(0xA11CE, 0x5EEDC0DE))
	coefficients := make([][]float32, probeDocs)
	norms := make([]float32, probeDocs)
	for i := range coefficients {
		c := make([]float32, len(weights)+1)
		for k := range weights {
			c[k] = weights[k] * float32(rng.NormFloat64())
		}
		c[len(weights)] = meanWeight
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

func normalise(v []float32) {
	n := norm(v)
	if n == 0 {
		return
	}
	for i := range v {
		v[i] /= n
	}
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

// SetSpectrumForTest is how the fixture's own parameters were chosen.
//
// It exists so the spectrum could be swept against measured recall rather
// than guessed, and it is exported for that one purpose: the shipped values
// are the constants above, and nothing in the engine calls this.
func SetSpectrumForTest(rank int, alpha float64, support int) {
	spectrumRank, spectrumAlpha, spectrumSupport = rank, alpha, support
}
