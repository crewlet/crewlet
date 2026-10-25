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
// [AnchorsPerTrimmedRecord] anchors, in a committed write transaction of its
// own on the pinned driver under the store's own pragmas — the number
// [AnchorsPerTrimmedRecord] is set against [ApplyTxTimeBudget], and
// re-measurable on an operator's own disk:
//
//	go test ./internal/statelog -run '^$' -bench BenchmarkATrimmedRecordsSweep -benchtime 200x
//
// AT THREE TABLE SIZES, each the anchors on the swept log — twenty thousand, a
// young company's; three hundred thousand; and a million, twice the half
// million the code host's model holds on one log alone (about seventy thousand
// subjects a day, kept the seven days of the default `min_age`).
//
// LAID OUT AS PRODUCTION LAYS IT OUT, because a delete's cost is which pages it
// touches. Every row goes in through [tables.advanceAnchor], the applier's own
// upsert, in creation order — so a subject keeps the rowid of its first record
// — and every subject is then advanced again in a random order, as its last
// record lands whenever somebody last touched it: the order the sweep removes
// anchors in is unrelated to the order the table stores them in, and each
// record's [AnchorsPerTrimmedRecord] it removes are scattered across the whole
// table. A second log's anchors are interleaved with the first's, as the
// tracker's and the knowledge base's share the one table. Subjects are
// uuid-shaped, so the primary key's pages are random too.
//
// THE TAIL IS REPORTED BESIDE THE MEAN (`p50-ms`, `p99-ms`, `max-ms`), because
// what the budget bounds is the slowest transaction a waiter sits behind, and a
// mean hides exactly that. Run enough iterations for a p99 to mean something;
// the command above does.
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

	// seed writes one generation's anchors on the swept log — and, the
	// first time, the other log's beside them — in transactions of a
	// bounded size, and answers the packed floor above all of the swept
	// log's.
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
