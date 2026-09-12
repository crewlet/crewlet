package store_test

import (
	"database/sql"
	"fmt"
	"math/rand/v2"
	"strconv"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/search"
)

// THE PURE QUANTIZER AGREES WITH THE DRIVER, BIT FOR BIT AND ORDER FOR ORDER.
//
// # Why this test is the seam it is
//
// The engine's semantic first stage scans a column the DRIVER filled, with
// `vector1bit`, and orders it by a function the DRIVER computes,
// `vector_distance_cos`. Everything that reasons about whether that ranking is
// any good — the recall floor, the miss-rank distribution, the operator's own
// evaluation — is pure Go over slices, because a ranking that can only be
// exercised through a database is a ranking nobody re-measures.
//
// Those two halves are only allowed to be separate if they agree. This is
// where that is established, and it is a MEASUREMENT of the driver rather
// than a restatement of a convention:
//
//   - vector_distance_cos over two 1-bit vectors IS the raw Hamming distance.
//     That identity is what lets the SQL order by a cosine function while the
//     pure side counts bits; without it the two are ranking by different
//     quantities and neither would say so.
//   - An exact zero takes the NEGATIVE bit. Not a corner case: a truncated or
//     padded embedding carries exact zeros in whole dimensions, and mapping
//     them the other way flips every one of them against what was stored.
//   - The encoded width is 387 bytes at 3 072 and 195 at 1 536, which is what
//     the narrow table's own size arithmetic is written against.
func TestQuantizationMatchesTheDriver(t *testing.T) {
	t.Parallel()
	db := open(t)
	if err := db.Read(t.Context(), func(tx *sql.Tx) error {
		// THE IDENTITY, over a thousand random pairs.
		rng := rand.New(rand.NewPCG(7, 11))
		const width = 64
		for probe := range 1_000 {
			a, b := randomVector(rng, width), randomVector(rng, width)
			if probe%97 == 0 {
				// EXACT ZEROS, in whole dimensions, which is what a
				// truncated embedding carries and the one input where
				// a convention could differ from the driver's.
				for i := range a[:width/2] {
					a[i] = 0
				}
			}
			var driver float64
			if err := tx.QueryRowContext(t.Context(), `
				SELECT vector_distance_cos(vector1bit(vector32(?)),
				                           vector1bit(vector32(?)))`,
				literal(a), literal(b)).Scan(&driver); err != nil {
				return err
			}
			pure := search.Hamming(search.Quantize(a), search.Quantize(b))
			if int(driver) != pure || driver != float64(int(driver)) {
				return fmt.Errorf("the driver answers %v and the pure pair counts "+
					"%d bits on probe %d — the SQL orders a candidate pool by "+
					"the first and every recall figure is measured with the "+
					"second", driver, pure, probe)
			}
		}

		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

// AN EXACT ZERO TAKES THE NEGATIVE BIT, asserted on its own because it is the
// half a convention could get wrong without any random probe noticing.
func TestAnExactZeroQuantizesNegative(t *testing.T) {
	t.Parallel()
	db := open(t)
	if err := db.Read(t.Context(), func(tx *sql.Tx) error {
		// All zeros against all ones: if zero took the positive bit the
		// two would be identical and the distance would be nought.
		var distance float64
		if err := tx.QueryRowContext(t.Context(), `
			SELECT vector_distance_cos(vector1bit(vector32('[0,0,0,0,0,0,0,0]')),
			                           vector1bit(vector32('[1,1,1,1,1,1,1,1]')))`).
			Scan(&distance); err != nil {
			return err
		}
		if distance != 8 {
			return fmt.Errorf("the driver puts all-zero and all-positive %v bits "+
				"apart, not 8 — an exact zero must take the negative bit, "+
				"which is what a truncated embedding's empty dimensions "+
				"depend on", distance)
		}
		zeros := search.Quantize(make([]float32, 8))
		ones := search.Quantize([]float32{1, 1, 1, 1, 1, 1, 1, 1})
		if got := search.Hamming(zeros, ones); got != 8 {
			return fmt.Errorf("the pure quantizer puts them %d bits apart", got)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

// THE ENCODED WIDTH IS TWO-BYTE ALIGNED, and the flat "+3" every reader would
// write is wrong at half of all widths.
//
// The size arithmetic behind `kb_vectors_bin` is written from CodeBytes, and
// the temptation is to state it as the packed bits plus a fixed trailer —
// which happens to be right at 3 072 and 1 536, the two widths anybody
// measures, because both have an EVEN byte-count. The whole value is padded to
// an odd length, so an odd byte-count pays two bytes rather than three. Nothing
// but a sweep over widths would say so.
func TestTheEncodedWidthIsTwoByteAligned(t *testing.T) {
	t.Parallel()
	db := open(t)
	if err := db.Read(t.Context(), func(tx *sql.Tx) error {
		// Every width the engine plausibly meets, plus the boundaries
		// either side of a byte and the odd byte-counts in between.
		for _, dimensions := range []int{
			1, 7, 8, 9, 16, 17, 24, 25, 32, 63, 64, 100, 127, 128, 255, 256,
			384, 768, 1024, 1536, 2048, 3072, 4096,
		} {
			v := make([]float32, dimensions)
			for i := range v {
				v[i] = 1
			}
			blob, err := db.EncodeVector(v)
			if err != nil {
				return err
			}
			var length int
			if err := tx.QueryRowContext(t.Context(),
				`SELECT length(vector1bit(?))`, blob).Scan(&length); err != nil {
				return err
			}
			if want := search.CodeBytes(dimensions); length != want {
				return fmt.Errorf("the driver encodes %d dimensions in %d bytes "+
					"and CodeBytes says %d — the narrow table's size arithmetic "+
					"is written against that number, and a rule that only holds "+
					"at an even byte-count is a rule that is wrong at half of "+
					"all widths", dimensions, length, want)
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

// A RAW f32 BLOB IS A VECTOR, so the applier's own INSERT needs no conversion.
//
// The whole reason `kb_vectors_bin` costs no provider call, no second stream
// and no second cursor is that `vector1bit(?)` over the bytes the record
// already carried is a pure function computed inside the transaction that
// writes `kb_vectors`. If the driver needed `vector32(<text literal>)` on the
// way in, that INSERT would have to render 3 072 floats as decimal text per
// row.
func TestTheDriverQuantizesTheBlobTheRecordCarries(t *testing.T) {
	t.Parallel()
	db := open(t)
	v := []float32{0, 1, -1, 0, 0.5, -0.5, 2, -2}
	blob, err := db.EncodeVector(v)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Read(t.Context(), func(tx *sql.Tx) error {
		var distance float64
		if err := tx.QueryRowContext(t.Context(),
			`SELECT vector_distance_cos(vector1bit(?), vector1bit(vector32(?)))`,
			blob, literal(v)).Scan(&distance); err != nil {
			return err
		}
		if distance != 0 {
			return fmt.Errorf("the same vector quantizes to codes %v bits "+
				"apart depending on whether it arrived as the stored blob or "+
				"as a text literal", distance)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func randomVector(rng *rand.Rand, n int) []float32 {
	out := make([]float32, n)
	for i := range out {
		out[i] = float32(rng.NormFloat64())
	}
	return out
}

func literal(v []float32) string {
	parts := make([]string, len(v))
	for i, x := range v {
		parts[i] = strconv.FormatFloat(float64(x), 'g', -1, 32)
	}
	return "[" + strings.Join(parts, ",") + "]"
}
