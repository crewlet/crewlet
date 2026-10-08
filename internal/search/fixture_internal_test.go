package search

import (
	"math"
	"math/rand/v2"
	"slices"
	"testing"
)

// A BLOCK COMPOSES EXACTLY WHAT ONE DOCUMENT AT A TIME DOES, to the bit, and a
// fixture is the same corpus whatever its size's remainder against the block.
//
// Every recall figure this package's comments quote was measured on corpora
// the one-document loop generated, so a composition that summed one float32
// in another order would move every floor's margin while each gate still
// passed — or failed for a reason nobody could find. The reference here is
// that loop, written out: each direction in turn along its support, the mean
// last, a zero coefficient adding nothing. The blocks hold coefficient sets
// from both members of the family and sets whose zeros differ from their
// neighbours', and the fixture is a size that leaves a partial last block.
func TestABlockComposesExactlyWhatOneDocumentAtATimeDoes(t *testing.T) {
	t.Parallel()
	b := newBasis()
	rng := rand.New(rand.NewPCG(9, 9))
	var sets [][]float32
	for _, gen := range []generator{{weights: b.weights}, topicalGenerator(b.weights)} {
		mean := solveMeanWeight(gen)
		for d := range composeBlock + 3 {
			c := make([]float32, len(b.weights)+1)
			gen.draw(rng, c)
			c[len(b.weights)] = mean
			// ZEROS THAT DIFFER FROM A NEIGHBOUR'S, the mean's among them:
			// a block skips a zero per document, never per direction.
			if d%3 == 0 {
				c[d%len(b.weights)] = 0
			}
			if d%4 == 1 {
				c[len(b.weights)] = 0
			}
			sets = append(sets, c)
		}
	}
	for at := 0; at < len(sets); at += composeBlock {
		block := sets[at:min(at+composeBlock, len(sets))]
		codes := make([][]uint64, len(block))
		vectors := make([][]float32, len(block))
		for j := range vectors {
			vectors[j] = make([]float32, FixtureWidth)
		}
		b.composeInto(block, codes, vectors)
		for j, c := range block {
			want := composedOneAtATime(b, c)
			for d := range want {
				if math.Float32bits(vectors[j][d]) != math.Float32bits(want[d]) {
					t.Fatalf("set %d, coordinate %d: a block composed %v and one "+
						"document at a time %v", at+j, d, vectors[j][d], want[d])
				}
			}
			if !slices.Equal(codes[j], Quantize(want)) {
				t.Fatalf("set %d: a block's code is not the sign code of the "+
					"vector one document at a time composes", at+j)
			}
		}
	}

	// AND THE FIXTURE ITSELF: drawn in document order from its own stream,
	// each code that draw's — through a partial last block.
	const n, seed = 2*composeBlock + 3, 5
	for _, f := range []*Fixture{NewFixture(n, seed), NewTopicalFixture(n, seed)} {
		stream := rand.New(rand.NewPCG(seed, 0x5EEDC0DE))
		for i := range n {
			c := make([]float32, len(f.basis.weights)+1)
			if topic := f.gen.draw(stream, c); topic != f.Topics[i] {
				t.Fatalf("document %d was drawn around topic %d, and the stream "+
					"drawn one document at a time gives %d", i, f.Topics[i], topic)
			}
			c[len(f.basis.weights)] = f.meanWeight
			if !slices.Equal(f.coefficients[i], c) {
				t.Fatalf("document %d's coefficients are not the stream's draw "+
					"for it in document order", i)
			}
			if !slices.Equal(f.Codes[i], Quantize(composedOneAtATime(f.basis, c))) {
				t.Fatalf("document %d's code is not the sign code of the vector "+
					"one document at a time composes", i)
			}
		}
	}
}

// composedOneAtATime is the composition every recall figure was measured on:
// one document, each direction in turn along its support, the mean last.
func composedOneAtATime(b *basis, coefficients []float32) []float32 {
	vector := make([]float32, FixtureWidth)
	for k, c := range coefficients[:len(b.support)] {
		if c == 0 {
			continue
		}
		for i, d := range b.support[k] {
			vector[d] += c * b.values[k][i]
		}
	}
	if mean := coefficients[len(b.support)]; mean != 0 {
		for i, d := range b.meanSupport {
			vector[d] += mean * b.meanValues[i]
		}
	}
	return vector
}

// THE FIXTURE COMPOSES ON ITS OWN STACK.
//
// The accumulator being a variable the race detector does not instrument is
// the whole of why generating the gate corpus stopped taking two minutes under
// `-race` ([basis.composeInto]) — and nothing about the code says so: a block
// one document larger, or the array allocated rather than declared, compiles,
// composes the same bits, and quietly moves it to the heap. A composition that
// allocates nothing is one whose accumulator did not move.
//
// NOT PARALLEL, because an allocation count is the whole process's; it takes
// milliseconds, before any parallel test resumes.
func TestTheFixtureComposesOnItsOwnStack(t *testing.T) {
	b := newBasis()
	gen := generator{weights: b.weights}
	rng := rand.New(rand.NewPCG(3, 3))
	block := make([][]float32, composeBlock)
	vectors := make([][]float32, composeBlock)
	for j := range block {
		block[j] = make([]float32, len(b.weights)+1)
		gen.draw(rng, block[j])
		vectors[j] = make([]float32, FixtureWidth)
	}
	if allocs := testing.AllocsPerRun(5, func() {
		b.composeInto(block, nil, vectors)
	}); allocs != 0 {
		t.Fatalf("composing a block of %d documents allocated %.0f times — its "+
			"accumulator has moved to the heap, where the race detector "+
			"instruments every one of its writes", composeBlock, allocs)
	}
}
