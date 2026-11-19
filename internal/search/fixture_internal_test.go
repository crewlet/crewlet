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
// Every recall figure quoted in this package was measured on corpora the
// one-document loop, [basis.vector], generated, so a block summing one float32
// in another order would move every floor's margin while each gate still passed.
// The coefficient sets come from both members of the family, include sets whose
// zeros differ from their neighbours', and the fixture size leaves a partial
// last block.
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
			want := b.vector(c)
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
	// each code that draw's — through a partial last block — and each
	// document's vector, recomposed alone, the one its code was taken from.
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
			if !slices.Equal(f.Codes[i], Quantize(f.Vector(i))) {
				t.Fatalf("document %d's code is not the sign code of the vector "+
					"one document at a time composes", i)
			}
		}
	}
}

// THE FIXTURE COMPOSES ON ITS OWN STACK: an accumulator the race detector does
// not instrument is why generating the gate corpus stopped taking two minutes
// under `-race` ([basis.composeInto]), and nothing in the code says so: a
// larger block, or the array allocated, composes the same bits on the heap.
//
// NOT PARALLEL: an allocation count is the whole process's. Counted over A
// HUNDRED BLOCKS because [testing.AllocsPerRun] rounds down and this binary is
// never quiet (nats-server, which it links, allocates about ten times a second
// on its CPU sampler): over five blocks a sample once in two thousand runs read
// as an accumulator on the heap, while a hundred (about fifty milliseconds)
// divide a sample or two to zero and one allocation a block to one.
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
	if allocs := testing.AllocsPerRun(100, func() {
		b.composeInto(block, nil, vectors)
	}); allocs != 0 {
		t.Fatalf("composing a block of %d documents allocated %.0f times — its "+
			"accumulator has moved to the heap, where the race detector "+
			"instruments every one of its writes", composeBlock, allocs)
	}
}
