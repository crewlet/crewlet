package gitlet

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/store"
)

// BenchmarkARepositoryListingAnswersItsSparks is the read a listing's sparks add: a page of
// MaxSparkReposPerPage repositories (`listing`; `listing-500` is MaxReposPerPage, which it is held
// under), each with a day row for every one of the 16 UTC days a MaxListingDaySpan touches and every
// quarter-hour counted, folded at the listing's widest bounds; and a detail's spark and pushers at
// MaxDayBounds bounds over 33 days of twenty pushers (`detail`). `max-read-ms` is held against a tenth of statelog.ReadBudget, the page rule runPushes keeps.
func BenchmarkARepositoryListingAnswersItsSparks(b *testing.B) {
	quiet(b)
	ctx := context.Background()
	e := openApplyEstate(b)
	start := time.Date(2031, 4, 1, 0, 0, 0, 0, time.UTC)
	ids := make([]string, MaxReposPerPage)
	for i := range ids {
		ids[i] = fmt.Sprintf("repo-%06d", i)
	}
	list, err := jsonList(ids)
	if err != nil {
		b.Fatal(err)
	}
	ones := strings.Repeat("1, ", pushQuarters-1) + "1"
	e.exec(`WITH RECURSIVE d(i) AS (SELECT 0 UNION ALL SELECT i + 1 FROM d WHERE i + 1 < 16)
		INSERT INTO code_push_days (repo_id, day, pushes, `+pushQuarterColumns+`)
		SELECT CAST(j.value AS TEXT), ? + d.i * ?, ?, `+ones+` FROM json_each(?) AS j CROSS JOIN d`,
		store.EncodeTime(start), dayMicros, pushQuarters, list)
	e.exec(`WITH RECURSIVE d(i) AS (SELECT 0 UNION ALL SELECT i + 1 FROM d WHERE i + 1 < 33),
		p(k) AS (SELECT 0 UNION ALL SELECT k + 1 FROM p WHERE k + 1 < 20)
		INSERT INTO code_pusher_days (repo_id, day, pusher_kind, pusher, pushes)
		SELECT ?, ? + d.i * ?, 'agent', printf('seat-%02d', p.k), 5 FROM d CROSS JOIN p`,
		ids[0], store.EncodeTime(start), dayMicros)
	e.exec(`WITH RECURSIVE d(i) AS (SELECT 16 UNION ALL SELECT i + 1 FROM d WHERE i + 1 < 33)
		INSERT INTO code_push_days (repo_id, day, pushes, `+pushQuarterColumns+`)
		SELECT ?, ? + d.i * ?, ?, `+ones+` FROM d`, ids[0], store.EncodeTime(start), dayMicros, pushQuarters)
	// The +5:45 zone's midnights: a listing's widest span and a detail's widest count.
	bounds := func(n int) []time.Time {
		out := make([]time.Time, n)
		for i := range out {
			out[i] = start.Add(18*time.Hour + 15*time.Minute + time.Duration(i)*24*time.Hour)
		}
		return out
	}
	for _, arm := range []struct {
		name   string
		ids    []string
		bounds []time.Time
		rows   int
	}{
		{"listing", ids[:MaxSparkReposPerPage], bounds(16), MaxSparkReposPerPage * 16},
		{"listing-500", ids, bounds(16), MaxReposPerPage * 16},
		{"detail", ids[:1], bounds(MaxDayBounds), 33},
	} {
		b.Run(arm.name, func(b *testing.B) {
			var slowest time.Duration
			for range b.N {
				began := time.Now()
				if err := e.db.Reader().Read(ctx, func(tx *sql.Tx) error {
					days, readErr := pushesOnDays(ctx, tx, arm.ids, arm.bounds)
					if readErr != nil {
						return readErr
					}
					if len(days) != len(arm.ids) || days[arm.ids[0]][0] == 0 {
						return fmt.Errorf("the fold answered %d repositories, the first %v", len(days),
							days[arm.ids[0]])
					}
					if len(arm.ids) == 1 {
						_, readErr = pushersIn(ctx, tx, arm.ids[0], arm.bounds, MaxPushersAnswered)
					}
					return readErr
				}); err != nil {
					b.Fatal(err)
				}
				slowest = max(slowest, time.Since(began)) //fixedwait:measure a benchmark reports this; no gate reads it
			}
			b.ReportMetric(float64(arm.rows), "day-rows/op")
			b.ReportMetric(float64(slowest.Microseconds())/1e3, "max-read-ms")
			b.ReportMetric(float64((statelog.ReadBudget / 10).Milliseconds()), "budget-ms")
		})
	}
}

// BenchmarkRederiveThePushActivity is the push activity's pass of the applier's re-derivation over
// 100 000 push rows of ten repositories, each pushed on 182 UTC days by twenty pushers: `upgrade`
// writes every row from empty tables, as a node's first boot of a build that adds them does, and
// `kept` reads every push row and writes nothing. `ns/push` extrapolates to any history.
func BenchmarkRederiveThePushActivity(b *testing.B) {
	quiet(b)
	ctx := context.Background()
	const repos, rows, pushers = 10, 100_000, 20
	e := openApplyEstate(b)
	e.exec(`WITH RECURSIVE k(i) AS (SELECT 0 UNION ALL SELECT i + 1 FROM k WHERE i + 1 < ?)
		INSERT INTO code_pushes (push_id, repo_id, position, at, actor, actor_kind, actor_seat, via, commands,
			received_at, row_bytes)
		SELECT printf('push-%07d', i), printf('repo-%02d', i % ?), i + 1, (i / ? % 182) * ? + (i % 96) * ?,
			printf('seat-%02d', i / ? % ?), 'agent', printf('seat-%02d', i / ? % ?), 'git', '[]', 0, 2 FROM k`,
		rows, repos, repos, dayMicros, quarterMicros, repos, pushers, repos, pushers)
	rederive := func() int {
		var n int
		if err := e.db.Tx(ctx, func(tx *sql.Tx) (err error) {
			n, err = rederivePushActivity(ctx, tx, e.db.Caps().MaxVariables)
			return err
		}); err != nil {
			b.Fatal(err)
		}
		return n
	}
	rederive()
	for _, arm := range []struct {
		name   string
		forget bool
	}{{"upgrade", true}, {"kept", false}} {
		b.Run(arm.name, func(b *testing.B) {
			b.ResetTimer()
			for range b.N {
				if arm.forget {
					b.StopTimer()
					e.forgetActivity()
					b.StartTimer()
				}
				if n := rederive(); (n == 0) == arm.forget {
					b.Fatalf("the re-derivation wrote %d rows", n)
				}
			}
			b.ReportMetric(float64(b.Elapsed().Nanoseconds())/float64(b.N)/rows, "ns/push")
		})
	}
}
