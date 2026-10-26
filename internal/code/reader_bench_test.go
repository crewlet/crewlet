package code

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/queue/topics"
	"github.com/crewlet/crewlet/internal/statelog"
)

// BenchmarkServeASnapshotPage is one full page of a full snapshot's refs — the
// MaxSnapshotItems a cold sync of a large repository reads a page at a time —
// served from a repository of 100 000 refs (MaxRefsPerRepo): the read, the
// scan and the page's measure, in one read transaction. Its `serve` arms hold
// no tombstones and the most a repository may (MaxTombstonesPerRepo) — the
// full read's walk passes every one of them in its pages' key ranges, since
// it reads the refs' primary key and skips the deleted — laid out two ways:
// INTERLEAVED, one after every live ref in key order, which is what a whole
// snapshot pays (every page walks its share), and CLUSTERED, every one sorting
// before the first live ref, which is the worst ONE page pays (a clean-up of
// one prefix: the first page walks the whole cluster). Its `decide` arm
// prices page 1's choice between a full answer and a delta (deltaOutweighs)
// at its dearest: a delta from below every row of the interleaved
// repository, which the count reads one row past the full answer deep — a
// repository holding no retired pack, so the count's third term, the
// retirements, is one seek of an empty range. Its
// `measure` sub-benchmarks price the measure alone over the same page, computed
// (rowbytes.go) against the encoding it replaced, which every row of every
// page on the serving node used to pay.
func BenchmarkServeASnapshotPage(b *testing.B) {
	quiet(b)
	at := statelog.Position{Stream: topics.CodeLogStream, Generation: 1, Seq: 2}
	cursor := StateCursor{Full: true, Phase: phaseRefs, From: at, Seen: at, Stream: time.Unix(1, 0)}
	ctx := context.Background()
	r := &Reader{pageRows: MaxSnapshotItems, pageBytes: MaxPageBytes}
	estateOf := func(b *testing.B, tombstones int, name string) *estate {
		e := openApplyEstate(b)
		e.apply(create(b, "repo-a", "a"))
		e.seed("repo-a", 0, MaxRefsPerRepo, 0)
		if tombstones > 0 {
			e.exec(`WITH RECURSIVE k(i) AS (SELECT 0 UNION ALL SELECT i + 1 FROM k WHERE i + 1 < ?)
				INSERT INTO code_refs (repo_id, ref, target, position, at)
				SELECT 'repo-a', printf(?, i), '', 1, 0 FROM k`, tombstones, name)
			e.exec(`UPDATE code_repos SET tombstones = ? WHERE id = 'repo-a'`, tombstones)
		}
		return e
	}
	serve := func(b *testing.B, e *estate) RepoSnapshot {
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

	var served RepoSnapshot
	for _, arm := range []struct {
		name       string
		tombstones int
		// spelled is a tombstone's ref as a printf of its number: after
		// "r000001" and before "r000002", or before every live ref.
		spelled string
	}{
		{"tombstones=0", 0, ""},
		{fmt.Sprintf("tombstones=%d/interleaved", MaxTombstonesPerRepo), MaxTombstonesPerRepo, "refs/heads/r%06d-gone"},
		{fmt.Sprintf("tombstones=%d/clustered", MaxTombstonesPerRepo), MaxTombstonesPerRepo, "refs/heads/gone-%06d"},
	} {
		e := estateOf(b, arm.tombstones, arm.spelled)
		b.Run("serve/"+arm.name, func(b *testing.B) {
			b.ReportAllocs()
			for range b.N {
				serve(b, e)
			}
			b.ReportMetric(float64(b.Elapsed().Nanoseconds())/float64(b.N)/MaxSnapshotItems, "ns/ref")
		})
		if arm.tombstones == 0 {
			served = serve(b, e)
			continue
		}
		if arm.spelled != "refs/heads/r%06d-gone" {
			continue
		}
		// THE SIZE RULE AT ITS DEAREST: a delta from below every row of a
		// full repository holding its bound of tombstones, which the count
		// reads one past the full answer deep before answering in full.
		b.Run("decide/worst", func(b *testing.B) {
			b.ReportAllocs()
			for range b.N {
				if err := e.db.Reader().Read(ctx, func(tx *sql.Tx) error {
					v, _, err := repoViewIn(ctx, tx, "repo-a", at)
					if err != nil {
						return err
					}
					full, err := deltaOutweighs(ctx, tx, "repo-a", 0, v)
					if err == nil && !full {
						err = fmt.Errorf("a delta of every row was not answered in full")
					}
					return err
				}); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
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
