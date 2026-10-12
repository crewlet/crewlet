package code

import (
	"context"
	"database/sql"
	"encoding/json"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/queue/topics"
	"github.com/crewlet/crewlet/internal/statelog"
)

// BenchmarkServeASnapshotPage is one full page of a full snapshot's refs — the
// MaxSnapshotItems a cold sync of a large repository reads a page at a time —
// served from a repository of 100 000 refs (MaxRefsPerRepo): the read, the
// scan and the page's measure, in one read transaction. Its `measure`
// sub-benchmarks price the measure alone over the same page, computed
// (rowbytes.go) against the encoding it replaced, which every row of every
// page on the serving node used to pay.
func BenchmarkServeASnapshotPage(b *testing.B) {
	e := openApplyEstate(b)
	e.apply(create(b, "repo-a", "a"))
	e.seed("repo-a", 0, MaxRefsPerRepo, 0)
	r := &Reader{pageRows: MaxSnapshotItems, pageBytes: MaxPageBytes}
	at := statelog.Position{Stream: topics.CodeLogStream, Generation: 1, Seq: 2}
	cursor := StateCursor{Full: true, Phase: phaseRefs, From: at, Seen: at, Stream: time.Unix(1, 0)}
	ctx := context.Background()
	serve := func() RepoSnapshot {
		var out RepoSnapshot
		if err := e.db.Reader().Read(ctx, func(tx *sql.Tx) error {
			_, err := r.fill(ctx, tx, "repo-a", &out, cursor, true)
			return err
		}); err != nil {
			b.Fatal(err)
		}
		if len(out.Refs) != MaxSnapshotItems {
			b.Fatalf("the page served %d refs, want a full page of %d", len(out.Refs), MaxSnapshotItems)
		}
		return out
	}

	b.Run("serve", func(b *testing.B) {
		b.ReportAllocs()
		for range b.N {
			serve()
		}
		b.ReportMetric(float64(b.Elapsed().Nanoseconds())/float64(b.N)/MaxSnapshotItems, "ns/ref")
	})
	served := serve()
	b.Run("measure=computed", func(b *testing.B) {
		b.ReportAllocs()
		for range b.N {
			pg := page{maxRows: MaxSnapshotItems + 1, maxB: MaxPageBytes * 2}
			for _, ref := range served.Refs {
				pg.add(ref.encodedBytes())
			}
		}
		b.ReportMetric(float64(b.Elapsed().Nanoseconds())/float64(b.N)/MaxSnapshotItems, "ns/ref")
	})
	b.Run("measure=encoded", func(b *testing.B) {
		b.ReportAllocs()
		for range b.N {
			pg := page{maxRows: MaxSnapshotItems + 1, maxB: MaxPageBytes * 2}
			for _, ref := range served.Refs {
				encoded, err := json.Marshal(ref)
				if err != nil {
					b.Fatal(err)
				}
				pg.add(len(encoded))
			}
		}
		b.ReportMetric(float64(b.Elapsed().Nanoseconds())/float64(b.N)/MaxSnapshotItems, "ns/ref")
	})
}
