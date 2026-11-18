package gitlet

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

// BenchmarkRunPushes is every page of a `run_pushes` read, each in its own read
// transaction as each is its own estate request, for a run pushing every five
// minutes to a repository taking 70 a minute (the large model's monorepo), over
// an hour and a week, and for an hour in a quiet repository (a push per ten
// minutes). The read has no index of its own (replicated 0098), so it scans
// every push since the run's launch: `rows/op`. Rows are ~400-byte ordinary
// pushes; `repo=wide` makes 200 of them 999 refused refs at MaxRefNameBytes
// (403 KB, scanIntroduced's widest) so pages end at MaxPageBytes, and
// `repos=8` is MaxRunRepos such repositories. WHAT IT DECIDES: `max-page-ms`,
// the slowest page (one request's cost to its node), under a tenth of
// statelog.ReadBudget and the week's read under twice it, or the page's row
// bound (MaxSnapshotItems) is set lower; runPushes states the figures.
func BenchmarkRunPushes(b *testing.B) {
	quiet(b)
	ctx := context.Background()
	const (
		perMinute = 70
		hour      = 60 * perMinute
		week      = 7 * 24 * hour
		// every is the busy repository's pushes per one of the run's.
		every = 5 * perMinute
		// quietPushes is a week of the quiet repository's, quietHour an hour.
		quietPushes = 7 * 24 * 6
		quietHour   = 6
		// wideEvery makes 200 of the wide hour's 4 200 pushes wide.
		wideEvery = 21
	)
	weekRun, hourRun, quietRun := "3f1c2b4a-5d6e-4f70-8a9b-0c1d2e3f4a5b", "7d2e1f0a-9b8c-4d7e-a6f5-e4d3c2b1a090",
		"5b4a3c2d-1e0f-4a9b-8c7d-6e5f4a3b2c1d"
	e := openApplyEstate(b)
	e.apply(create(b, "busy", "busy"), create(b, "quiet", "quiet"), create(b, "wide", "wide"))
	commands := `[{"ref":"refs/heads/seats/swe/eng-42","old":"` + strings.Repeat("a", 40) + `","new":"` +
		strings.Repeat("b", 40) + `","result":"ok"}]`
	introduced := strings.Repeat("c", 40) + " " + strings.Repeat("d", 40) + " " + strings.Repeat("e", 40)
	// The busy week: the hour's run pushes in the last hour, half way between
	// the week run's.
	e.exec(`WITH RECURSIVE k(i) AS (SELECT 1 UNION ALL SELECT i + 1 FROM k WHERE i < ?1)
		INSERT INTO code_pushes (push_id, repo_id, position, at, actor, actor_kind, actor_seat, via, turn_id,
			run_id, harness, commands, introduced, introduced_count, received_at, row_bytes)
		SELECT printf('busy-%07d', i), 'busy', i, i, 'swe', 'agent', 'swe', 'git', 'turn',
			CASE WHEN i % ?2 = 0 THEN ?3 WHEN i > ?1 - ?4 AND i % ?2 = ?2 / 2 THEN ?5 ELSE '' END,
			CASE WHEN i % ?2 = 0 OR (i > ?1 - ?4 AND i % ?2 = ?2 / 2) THEN 'claude-code' ELSE '' END,
			?6, ?7, 3, i, ?8 FROM k`,
		week, every, weekRun, hour, hourRun, commands, introduced, len(commands)+len(introduced))
	e.exec(`WITH RECURSIVE k(i) AS (SELECT 1 UNION ALL SELECT i + 1 FROM k WHERE i < ?1)
		INSERT INTO code_pushes (push_id, repo_id, position, at, actor, actor_kind, actor_seat, via, turn_id,
			run_id, harness, commands, introduced, introduced_count, received_at, row_bytes)
		SELECT printf('quiet-%07d', i), 'quiet', i, i, 'swe', 'agent', 'swe', 'git', 'turn',
			CASE WHEN i > ?1 - ?2 AND i % 2 = 0 THEN ?3 ELSE '' END,
			CASE WHEN i > ?1 - ?2 AND i % 2 = 0 THEN 'claude-code' ELSE '' END,
			?4, ?5, 3, i, ?6 FROM k`,
		quietPushes, quietHour, quietRun, commands, introduced, len(commands)+len(introduced))
	// The wide hour: positions 1 mod 21 are wide, never the run's (which sit
	// at `every`/2 mod `every`).
	wideCmds := []Command{{Ref: "refs/heads/topic", Old: ZeroOID, New: strings.Repeat("f", 40),
		Result: ResultOK}}
	long := strings.Repeat("x", MaxRefNameBytes-len("refs/heads/")-len("-000"))
	for c := range MaxCommandsPerPush - 1 {
		wideCmds = append(wideCmds, Command{Ref: fmt.Sprintf("refs/heads/%s-%03d", long, c), Old: ZeroOID,
			New: strings.Repeat("f", 40), Result: ResultNG, Reason: RefusalNotPermitted})
	}
	wide, err := json.Marshal(wideCmds)
	if err != nil {
		b.Fatal(err)
	}
	e.exec(`WITH RECURSIVE k(i) AS (SELECT 1 UNION ALL SELECT i + 1 FROM k WHERE i < ?1)
		INSERT INTO code_pushes (push_id, repo_id, position, at, actor, actor_kind, actor_seat, via, turn_id,
			run_id, harness, commands, introduced, introduced_count, received_at, row_bytes)
		SELECT printf('wide-%07d', i), 'wide', i, i, 'swe', 'agent', 'swe', 'git', 'turn',
			CASE WHEN i % ?2 = ?2 / 2 THEN ?3 ELSE '' END,
			CASE WHEN i % ?2 = ?2 / 2 THEN 'claude-code' ELSE '' END,
			CASE WHEN i % ?4 = 1 THEN ?5 ELSE ?6 END, ?7, 3, i,
			CASE WHEN i % ?4 = 1 THEN ?8 ELSE ?9 END FROM k`,
		hour, every, hourRun, wideEvery, string(wide), commands, introduced, len(wide)+len(introduced),
		len(commands)+len(introduced))
	// MaxRunRepos repositories, each an hour of the busy one's pushes.
	repos := make([]string, MaxRunRepos)
	for k := range repos {
		repos[k] = fmt.Sprintf("many-%d", k)
		e.apply(create(b, repos[k], repos[k]))
		e.exec(`WITH RECURSIVE k(i) AS (SELECT 1 UNION ALL SELECT i + 1 FROM k WHERE i < ?1)
			INSERT INTO code_pushes (push_id, repo_id, position, at, actor, actor_kind, actor_seat, via, turn_id,
				run_id, harness, commands, introduced, introduced_count, received_at, row_bytes)
			SELECT printf('%s-%07d', ?2, i), ?2, i, i, 'swe', 'agent', 'swe', 'git', 'turn',
				CASE WHEN i % ?3 = ?3 / 2 THEN ?4 ELSE '' END,
				CASE WHEN i % ?3 = ?3 / 2 THEN 'claude-code' ELSE '' END,
				?5, ?6, 3, i, ?7 FROM k`,
			hour, repos[k], every, hourRun, commands, introduced, len(commands)+len(introduced))
	}

	r := &Reader{pageRows: MaxSnapshotItems, pageBytes: MaxPageBytes}
	for _, arm := range []struct {
		name   string
		repos  []string
		run    string
		launch int64
		// rows is what the read scans (every push above launch), pushes
		// what it answers.
		rows, pushes int
	}{
		{"run=hour/repo=busy", []string{"busy"}, hourRun, week - hour, hour, hour / every},
		{"run=week/repo=busy", []string{"busy"}, weekRun, 0, week, week / every},
		{"run=hour/repo=quiet", []string{"quiet"}, quietRun, quietPushes - quietHour, quietHour, quietHour / 2},
		{"run=hour/repo=wide", []string{"wide"}, hourRun, 0, hour, hour / every},
		{"run=hour/repos=8", repos, hourRun, 0, MaxRunRepos * hour, MaxRunRepos * hour / every},
	} {
		b.Run(arm.name, func(b *testing.B) {
			var pushes, pages int
			var slowest time.Duration
			for range b.N {
				pushes, pages, slowest = 0, 0, 0
				q := ReadQuery{Kind: ReadRunPushes, RunPushes: &RunPushesQuery{Run: arm.run,
					Repos: arm.repos, After: unpacked(arm.launch)}}
				for {
					var out ReadAnswer
					began := time.Now()
					if err := e.db.Reader().Read(ctx, func(tx *sql.Tx) error {
						return r.runPushes(ctx, tx, q, &out)
					}); err != nil {
						b.Fatal(err)
					}
					slowest = max(slowest, time.Since(began)) //fixedwait:measure a benchmark reports this; no gate reads it
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
			b.ReportMetric(float64(slowest.Microseconds())/1e3, "max-page-ms")
		})
	}
}

// BenchmarkServeASnapshotPage is one full page (MaxSnapshotItems) of a full
// snapshot's refs from a repository of MaxRefsPerRepo refs: read, scan and
// measure in one read transaction. The `serve` arms hold no tombstones or
// MaxTombstonesPerRepo, which the walk passes since it reads the refs' primary
// key and skips the deleted: INTERLEAVED, one after each live ref (what a
// whole snapshot pays), and CLUSTERED before the first live ref (the worst
// one page pays). The `decide` arms price page 1's choose where it reads the
// most, a delta from below every row beside MaxFullRetirements retirements:
// `decide/cache` asks as a node's cache does, one position for both floors;
// `decide/worst` asks retirements from below the delta's floor, the deepest
// any ask reads. The `measure` arms price the computed measure (rowbytes.go)
// against encoding each row.
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
		// spelled is a tombstone's ref as a printf of its number.
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
		// Every row moves above the delta's floor (position 3) beside
		// MaxFullRetirements retirements at 2: the most a full answer serves,
		// so choose counts every one. From 1 for both floors (cache) the full
		// answer's retirements cancel the delta's; from 1 and 2 (worst) every
		// one lies between the floors, so the refs are counted past them too.
		// One retirement more and the full answer serves none, and either ask
		// stops about a page shallower.
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
