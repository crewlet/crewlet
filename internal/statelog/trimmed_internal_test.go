package statelog

import (
	"database/sql"
	"fmt"
	"math/rand/v2"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/store"
	"github.com/crewlet/crewlet/internal/store/storetest"
)

// BenchmarkATrimmedRecordsSweep is ONE trimmed record's removal of
// [AnchorsPerTrimmedRecord] anchors in its own committed transaction on the
// pinned driver and the store's pragmas, what that constant is set against
// [ApplyTxTimeBudget] by; re-measure it on an operator's own disk with:
//
//	go test ./internal/statelog -run '^$' -bench BenchmarkATrimmedRecordsSweep -benchtime 200x
//
// Tables of 20k, 300k and 1M anchors (twice Gitlet's modelled week on one log)
// are laid out as production lays them, since a delete's cost is the pages it
// touches: rows through [tables.advanceAnchor] in creation order, then each
// re-advanced in random order, uuid-shaped, with another log's interleaved. It
// reports p50, p99 and max, since the budget bounds the slowest transaction.
func BenchmarkATrimmedRecordsSweep(b *testing.B) {
	for _, anchors := range []int{20_000, 300_000, 1_000_000} {
		b.Run(fmt.Sprintf("anchors=%d", anchors), func(b *testing.B) {
			benchmarkSweep(b, anchors)
		})
	}
}

// benchmarkSweep is [BenchmarkATrimmedRecordsSweep] over a table holding
// anchors on the swept log and as many on another.
func benchmarkSweep(b *testing.B, anchors int) {
	const stream, other = "CREWLET_BENCH_LOG", "CREWLET_BENCH_OTHER_LOG"
	db, estate := storetest.OpenEstate(b, filepath.Join(b.TempDir(), "node.db"), store.Options{}, 1)
	b.Cleanup(func() { _ = db.Close() })
	w, err := estate.Writer(b.Context())
	if err != nil {
		b.Fatalf("pin a writer: %v", err)
	}
	b.Cleanup(func() { _ = w.Close() })
	swept := tables{stream: stream, prefix: "crewlet.bench.log"}
	beside := tables{stream: other, prefix: "crewlet.bench.other"}

	// seed writes one generation's anchors on the swept log (and, if
	// interleave, the other log's beside them) in bounded transactions, and
	// answers the floor above all of the swept log's.
	rng := rand.New(rand.NewPCG(0x7472696d, 0x6d6564))
	uuid := func() string {
		hi, lo := rng.Uint64(), rng.Uint64()
		return fmt.Sprintf("%08x-%04x-4%03x-a%03x-%012x",
			hi>>32, (hi>>16)&0xffff, hi&0xfff, (lo>>48)&0xfff, lo&0xffffffffffff)
	}
	seed := func(gen uint32, interleave bool) Position {
		const perTx = 10_000
		subjects := make([]string, anchors)
		at := func(seq uint64) Position { return Position{Stream: stream, Generation: gen, Seq: seq} }
		write := func(from, to int, step func(tx *sql.Tx, i int) error) {
			for start := from; start < to; start += perTx {
				end := min(start+perTx, to)
				if err := w.Tx(b.Context(), func(tx *sql.Tx) error {
					for i := start; i < end; i++ {
						if err := step(tx, i); err != nil {
							return err
						}
					}
					return nil
				}); err != nil {
					b.Fatalf("seed the anchors: %v", err)
				}
			}
		}
		// CREATION ORDER: the row, and its rowid, of each subject's first
		// record — the other log's interleaved one for one.
		write(0, anchors, func(tx *sql.Tx, i int) error {
			subjects[i] = "crewlet.bench.log.task." + uuid()
			if err := swept.advanceAnchor(b.Context(), tx, subjects[i], at(uint64(2*i+1))); err != nil {
				return err
			}
			if !interleave {
				return nil
			}
			return beside.advanceAnchor(b.Context(), tx, "crewlet.bench.other.page."+uuid(),
				Position{Stream: other, Generation: gen, Seq: uint64(i + 1)})
		})
		// EACH SUBJECT'S LAST RECORD, in an order unrelated to its first.
		last := rng.Perm(anchors)
		write(0, anchors, func(tx *sql.Tx, i int) error {
			return swept.advanceAnchor(b.Context(), tx, subjects[last[i]], at(uint64(2*anchors+i+1)))
		})
		return Position{Stream: stream, Generation: gen + 1, Seq: 1}
	}

	gen := uint32(1)
	floor := seed(gen, true)
	left := anchors
	var took []time.Duration
	for b.Loop() {
		if left < AnchorsPerTrimmedRecord {
			b.StopTimer()
			gen++
			floor = seed(gen, false)
			left = anchors
			b.StartTimer()
		}
		started := time.Now()
		if err := w.Tx(b.Context(), func(tx *sql.Tx) error {
			n, err := swept.sweepAnchors(b.Context(), tx, floor)
			if err == nil && n != AnchorsPerTrimmedRecord {
				err = fmt.Errorf("removed %d anchors, want %d", n, AnchorsPerTrimmedRecord)
			}
			return err
		}); err != nil {
			b.Fatalf("sweep: %v", err)
		}
		took = append(took, time.Since(started))
		left -= AnchorsPerTrimmedRecord
	}
	slices.Sort(took)
	quantile := func(q float64) float64 {
		return float64(took[int(q*float64(len(took)-1))].Microseconds()) / 1000
	}
	b.ReportMetric(quantile(0.50), "p50-ms")
	b.ReportMetric(quantile(0.99), "p99-ms")
	b.ReportMetric(quantile(1), "max-ms")
}
