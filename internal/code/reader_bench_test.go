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
// one prefix: the first page walks the whole cluster). Its `decide` arms
// price page 1's choice between a full answer and a delta (choose) where it
// reads the most: a delta from below every row of the interleaved repository,
// beside MaxFullRetirements retirements — the most a full answer serves, so
// the choice counts every one. `decide/cache` asks as a git node's cache
// does, from one position for both floors, so the full answer would serve
// the very retirements the delta carries and the refs are counted one row
// past the full answer's own rows deep; `decide/worst` asks the full answer's
// retirements from below the delta's floor, every one of them between the
// two, which is the deepest any ask reads: every retirement counted, and the
// refs one row past the full answer's rows and those retirements deep. Its
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
		// THE CHOICE WHERE IT READS THE MOST, of a full repository holding
		// its bound of tombstones, every row of it moved (to position 3)
		// above the delta's floor, beside MaxFullRetirements retirements (at
		// position 2): the most a full answer serves, so it would serve every
		// one and the choice counts every one before it answers in full.
		// Asked as a git node's cache asks it, from one position (1) for
		// both floors, the full answer would serve the very retirements the
		// delta carries: they cancel, and the refs are counted one row past
		// the full answer's own rows deep. Asked for the full answer's
		// retirements from below the delta's floor (1 and 2), every
		// retirement lies between the two, the delta carrying none and the
		// full answer every one: the refs are counted one row past the full
		// answer's rows AND those retirements deep, the deepest any ask
		// reads. One retirement more and the full answer would serve none,
		// and either count of the refs stops about a page shallower.
		e.exec(`UPDATE code_refs SET position = 3 WHERE repo_id = 'repo-a'`)
		e.exec(`WITH RECURSIVE k(i) AS (SELECT 0 UNION ALL SELECT i + 1 FROM k WHERE i + 1 < ?)
			INSERT INTO code_packs_retired (repo_id, pack_id, part, object, hash, size, index_bytes,
				reverse_bytes, git_checksum, objects, kind, added_position, retired_position, retired_at)
			SELECT 'repo-a', printf('retired-%06d', i), 'pack', printf('retired-%06d', i), '', 100, 0, 0,
				printf('%040d', i), 1, 'push', 1, 2, 0 FROM k`, MaxFullRetirements)
		below := statelog.Position{Stream: topics.CodeLogStream, Seq: 1}
		between := statelog.Position{Stream: topics.CodeLogStream, Seq: 2}
		above := statelog.Position{Stream: topics.CodeLogStream, Seq: 3}
		for _, ask := range []struct {
			name string
			q    StateQuery
		}{
			{"decide/cache", StateQuery{RepoID: "repo-a", Since: below, RetiredSince: below}},
			{"decide/worst", StateQuery{RepoID: "repo-a", Since: between, RetiredSince: below}},
		} {
			b.Run(ask.name, func(b *testing.B) {
				b.ReportAllocs()
				for range b.N {
					if err := e.db.Reader().Read(ctx, func(tx *sql.Tx) error {
						v, _, err := repoViewIn(ctx, tx, "repo-a", above)
						if err != nil {
							return err
						}
						full, from, err := choose(ctx, tx, ask.q, above, v)
						if err == nil && (!full || from != below) {
							err = fmt.Errorf("a delta from %v of every row beside a page of retirements above %v "+
								"= full %t serving retirements from %v, want the full answer serving them from %v",
								ask.q.Since, below, full, from, below)
						}
						return err
					}); err != nil {
						b.Fatal(err)
					}
				}
			})
		}
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
