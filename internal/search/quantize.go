// Package search is the arithmetic behind the engine's semantic half: the
// 1-bit quantization its first retrieval stage scans, the Hamming distance
// that orders it, the two-stage composition itself, and the rank fusion that
// joins it to the lexical half.
//
// # Pure functions over values, and why that is the whole point
//
// Everything here takes values and returns values. No database, no store, no
// query — the SQL that scans `kb_vectors_bin` lives with the rest of the
// projection's statements, and this package is what says whether the ranking
// that SQL produces is the right one.
//
// The reason is the reason [textindex] gives for the same shape: a ranking
// that can only be exercised through a database is a ranking nobody
// re-measures, and an index rebuild can then quietly change it. Held as
// functions over slices, the quality gate runs in a unit test, at three
// corpus sizes, with a seeded corpus and no I/O at all.
//
// # What the gate measures, and what it does NOT
//
// [Quantize] keeps only each vector's ORTHANT — the sign of every coordinate
// and nothing else. How much an orthant says about cosine rank is a property
// of the corpus's own distribution and of nothing else: over a family of
// embedding-shaped generators at one corpus size, recall at the shipped
// oversample spans 0.29 to 0.98. So the deterministic gate here measures the
// ARITHMETIC — that an exact rerank over a 1-bit candidate pool recovers the
// exact ranking at sufficient depth — and never recall on a particular
// company's documents. `crewlet search eval` is what answers that, against
// that company's own vectors.
package search

import "math/bits"

// Quantize turns a vector into its sign code, LSB-first, with ZERO TAKING THE
// NEGATIVE BIT.
//
// # Both halves are measurements against the driver, not conventions
//
// The stage-1 scan orders by `vector_distance_cos(bits, :qb)` over values the
// driver's own `vector1bit` produced, so this function has to agree with it
// bit for bit or the pure gate certifies a ranking the database does not
// produce. Measured on `tursogo` at the pin: `vector1bit` of
// [0,1,-1,0,0.5,-0.5,2,-2] is 0x52, whose bits read LSB-first are
// [0,1,0,0,1,0,1,0] — so bit i lives at position i%8 of byte i/8, a set bit
// means STRICTLY POSITIVE, and an exact zero takes the negative bit.
//
// The zero is not a corner case to shrug at. A truncated or padded embedding
// carries exact zeros in whole dimensions, and mapping them to the positive
// bit would flip every one of them against what the database stored.
//
// The result is words rather than bytes because [Hamming] counts over words:
// the last word is zero-padded, and padding bits are identical in every code
// so they contribute nothing to any distance.
func Quantize(v []float32) []uint64 {
	code := make([]uint64, (len(v)+63)/64)
	for i, x := range v {
		if x > 0 {
			code[i/64] |= 1 << uint(i%64)
		}
	}
	return code
}

// Hamming counts the bits two codes differ in.
//
// # This IS the driver's cosine distance, measured rather than assumed
//
// `vector_distance_cos` over two `vector1bit` values returns the raw Hamming
// distance — verified at the pin over 200 random 64-dimensional pairs, all
// 200 agreeing exactly. That identity is what lets the stage-1 scan order by
// a cosine function while this package reasons about bit counts, and it is
// the one fact the whole two-stage design rests on: without it the pure gate
// and the SQL would be ranking by two different quantities.
//
// A length mismatch is a caller error rather than a silent short read: two
// codes of different widths are two different embedding spaces, which is the
// state `kb_vectors_bin`'s `model` and `dim` columns exist to make
// impossible.
func Hamming(a, b []uint64) int {
	if len(a) != len(b) {
		panic("search: Hamming over codes of different widths — two widths are " +
			"two embedding spaces, and the scan filters on model and dim so " +
			"they can never meet")
	}
	total := 0
	for i := range a {
		total += bits.OnesCount64(a[i] ^ b[i])
	}
	return total
}

// CodeBytes is how many bytes the DRIVER's encoding of an n-dimensional sign
// code occupies, which is what the table's own size arithmetic is written
// against.
//
// # The alignment is measured, and the obvious formula is wrong
//
// The packed bits are followed by a trailer, and the whole value is padded to
// an ODD length — two-byte alignment plus a one-byte type tag. So the overhead
// is three bytes over an even byte-count and two over an odd one, which is not
// something a reader would guess: at 3 072 and 1 536 dimensions the byte-count
// is even and a flat `+3` is right, and at 100 dimensions it is one byte over.
//
// Measured at the pin across nineteen widths (TestTheEncodedWidthIsTwoByte
// Aligned): 3 B at 1, 7 and 8 dimensions, 5 B at 9 through 24, 15 B at 100,
// 195 B at 1 536 and 387 B at 3 072.
//
// It is a function rather than two constants because the width is a property
// of the company's model, and a size claim that only held at one width would
// be re-derived wrongly at the other.
func CodeBytes(dimensions int) int {
	packed := (dimensions + 7) / 8
	// The trailer, then padding to an odd total.
	return packed + 3 - packed%2
}
