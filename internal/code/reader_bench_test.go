package code

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/queue/topics"
	"github.com/crewlet/crewlet/internal/statelog"
)

// BenchmarkRunPushes is the engine's whole read of what one coding run pushed —
// every page of a `run_pushes` read at its default limit, each in a read
// transaction of its own, as each is an estate request of its own — over a
// repository taking 70 pushes a minute (the large model's monorepo), for a run
// of an hour and one of a week that pushed once every five minutes, beside an
// hour's run in a quiet repository that takes a push every ten minutes. What
// it costs is the scan: the read has no index of its own (replicated 0068), so
// it reads every push the repository took during the run, a lookup of each
// row's page to test the run it names, and `rows/op` is how many. Each row is a
// push of one command and three introduced commits, about 400 bytes, as an
// ordinary push row is; the repositories' pushes below the run's launch are
// what its seek steps over. WHAT IT DECIDES: the hour's read must come in under
// a tenth of a second and the week's under twice statelog.ReadBudget — the
// week's whole scan is what one page costs where the run pushed once in that
// week — or a page ends at a count of rows scanned rather than kept; runPushes
// states the figures and what they decided.
func BenchmarkRunPushes(b *testing.B) {
	quiet(b)
	ctx := context.Background()
	const (
		perMinute = 70
		hour      = 60 * perMinute
		week      = 7 * 24 * hour
		// every is how many of the busy repository's pushes there are to
		// one of the run's: a push of the run every five minutes.
		every = 5 * perMinute
		// quietPushes is a week of the quiet repository's pushes, one every
		// ten minutes, and quietHour an hour of them.
		quietPushes = 7 * 24 * 6
		quietHour   = 6
	)
	weekRun, hourRun, quietRun := "3f1c2b4a-5d6e-4f70-8a9b-0c1d2e3f4a5b", "7d2e1f0a-9b8c-4d7e-a6f5-e4d3c2b1a090",
		"5b4a3c2d-1e0f-4a9b-8c7d-6e5f4a3b2c1d"
	e := openApplyEstate(b)
	e.apply(create(b, "busy", "busy"), create(b, "quiet", "quiet"))
	commands := `[{"ref":"refs/heads/seats/swe/eng-42","old":"` + strings.Repeat("a", 40) + `","new":"` +
		strings.Repeat("b", 40) + `","result":"ok"}]`
	introduced := strings.Repeat("c", 40) + " " + strings.Repeat("d", 40) + " " + strings.Repeat("e", 40)
	// THE BUSY REPOSITORY'S WEEK: the week's run pushes every `every` pushes
	// from its first; the hour's run every `every` pushes in the last hour,
	// half way between the week run's.
	e.exec(`WITH RECURSIVE k(i) AS (SELECT 1 UNION ALL SELECT i + 1 FROM k WHERE i < ?1)
		INSERT INTO code_pushes (push_id, repo_id, position, at, actor, actor_kind, actor_seat, via, turn_id,
			run_id, coding_agent, commands, introduced, introduced_count, received_at, row_bytes)
		SELECT printf('busy-%07d', i), 'busy', i, i, 'swe', 'agent', 'swe', 'git', 'turn',
			CASE WHEN i % ?2 = 0 THEN ?3 WHEN i > ?1 - ?4 AND i % ?2 = ?2 / 2 THEN ?5 ELSE '' END,
			CASE WHEN i % ?2 = 0 OR (i > ?1 - ?4 AND i % ?2 = ?2 / 2) THEN 'claude-code' ELSE '' END,
			?6, ?7, 3, i, ?8 FROM k`,
		week, every, weekRun, hour, hourRun, commands, introduced, len(commands)+len(introduced))
	e.exec(`WITH RECURSIVE k(i) AS (SELECT 1 UNION ALL SELECT i + 1 FROM k WHERE i < ?1)
		INSERT INTO code_pushes (push_id, repo_id, position, at, actor, actor_kind, actor_seat, via, turn_id,
			run_id, coding_agent, commands, introduced, introduced_count, received_at, row_bytes)
		SELECT printf('quiet-%07d', i), 'quiet', i, i, 'swe', 'agent', 'swe', 'git', 'turn',
			CASE WHEN i > ?1 - ?2 AND i % 2 = 0 THEN ?3 ELSE '' END,
			CASE WHEN i > ?1 - ?2 AND i % 2 = 0 THEN 'claude-code' ELSE '' END,
			?4, ?5, 3, i, ?6 FROM k`,
		quietPushes, quietHour, quietRun, commands, introduced, len(commands)+len(introduced))

	r := &Reader{pageRows: MaxSnapshotItems, pageBytes: MaxPageBytes}
	for _, arm := range []struct {
		name   string
		repo   string
		run    string
		launch int64
		// rows is what the read scans — every push above the launch — and
		// pushes what it answers.
		rows, pushes int
	}{
		{"run=hour/repo=busy", "busy", hourRun, week - hour, hour, hour / every},
		{"run=week/repo=busy", "busy", weekRun, 0, week, week / every},
		{"run=hour/repo=quiet", "quiet", quietRun, quietPushes - quietHour, quietHour, quietHour / 2},
	} {
		b.Run(arm.name, func(b *testing.B) {
			var pushes, pages int
			for range b.N {
				pushes, pages = 0, 0
				q := ReadQuery{Kind: ReadRunPushes, RunPushes: &RunPushesQuery{Run: arm.run,
					Repos: []string{arm.repo}, After: unpacked(arm.launch)}}
				for {
					var out ReadAnswer
					if err := e.db.Reader().Read(ctx, func(tx *sql.Tx) error {
						return r.runPushes(ctx, tx, q, &out)
					}); err != nil {
						b.Fatal(err)
					}
					pushes, pages = pushes+len(out.Pushes), pages+1
					if out.Next == "" {
						break
					}
					q.Cursor = out.Next
				}
			}
			if pushes != arm.pushes {
				b.Fatalf("the read answered %d of the run's pushes, want %d", pushes, arm.pushes)
			}
			b.ReportMetric(float64(arm.rows), "rows/op")
			b.ReportMetric(float64(pushes), "pushes/op")
			b.ReportMetric(float64(pages), "pages/op")
		})
	}
}

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
			_, err := r.fill(ctx, tx, "repo-a", &out, cursor, true, false)
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
