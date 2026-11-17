package gitlet

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"
)

// BenchmarkANarrowedPushHistory reads narrowed histories to their end, a transaction a page: the monorepo's week (705 600
// pushes), a steady repository's 181 days 200 a page, and a page of the widest pushes. Every page stays under a tenth of
// statelog.ReadBudget, and every read not made of full pages of kept pushes stays whole under twice it.
func BenchmarkANarrowedPushHistory(b *testing.B) {
	quiet(b)
	ctx := context.Background()
	const (
		week  = 705_600
		step  = int64(857_000) // microseconds between two of the monorepo's pushes
		day   = int64(24 * time.Hour / time.Microsecond)
		start = int64(1_900_000_000) * 1_000_000 / day * day
	)
	e := openApplyEstate(b)
	e.apply(create(b, "mono", "mono"), create(b, "steady", "steady"), create(b, "wide", "wide"))
	commands := `[{"ref":"refs/heads/seats/swe/eng-42","old":"` + strings.Repeat("a", 40) + `","new":"` +
		strings.Repeat("b", 40) + `","result":"ok"}]`
	minute := int64(time.Minute / time.Microsecond)
	// THE MONOREPO'S WEEK: who pushed each, by its instant's place in its UTC day.
	e.exec(`WITH RECURSIVE k(i) AS (SELECT 0 UNION ALL SELECT i + 1 FROM k WHERE i < ?1 - 1),
		p(n, at, tod) AS (SELECT i, ?2 + i * ?3, (i * ?3) % ?4 FROM k),
		w(n, at, seat, kind, merge) AS (SELECT n, at,
			CASE WHEN n IN (?7, ?8) THEN 'rare'
				WHEN tod >= 840 * ?9 AND tod < 840 * ?9 + ?3 THEN 'daily'
				WHEN (tod >= 5 * ?9 AND tod < 5 * ?9 + ?3) OR (tod >= 1435 * ?9 AND tod < 1435 * ?9 + ?3) THEN 'spread'
				WHEN tod % (60 * ?9) < ?3 THEN 'merger'
				WHEN n % 20 = 0 THEN 'dense' ELSE 'swe' END,
			CASE WHEN n IN (?7, ?8) THEN 'operator' ELSE 'agent' END,
			CASE WHEN n IN (?7, ?8) OR (tod >= 840 * ?9 AND tod < 840 * ?9 + ?3) THEN 0
				WHEN tod % (60 * ?9) < ?3 THEN 1 ELSE 0 END FROM p)
		INSERT INTO code_pushes (push_id, repo_id, position, at, actor, actor_kind, actor_seat, via, commands,
			received_at, row_bytes)
		SELECT printf('mono-%07d', n), 'mono', n + 1, at, seat, kind, seat,
			CASE WHEN merge = 1 THEN 'merge' ELSE 'git' END, ?5, at, ?6 FROM w`,
		week, start, step, day, commands, len(commands), week/3, 2*week/3, minute)
	e.recountActivity("mono")
	// THE STEADY REPOSITORY: 181 days of 100 pushes, the steady seat's the 50th of each day.
	steadyStep := day / 100
	e.exec(`WITH RECURSIVE k(i) AS (SELECT 0 UNION ALL SELECT i + 1 FROM k WHERE i < ?1 - 1)
		INSERT INTO code_pushes (push_id, repo_id, position, at, actor, actor_kind, actor_seat, via, commands,
			received_at, row_bytes)
		SELECT printf('steady-%07d', i), 'steady', i + 1, ?2 + i * ?3,
			CASE WHEN i % 100 = 50 THEN 'steady' ELSE 'swe' END, 'agent',
			CASE WHEN i % 100 = 50 THEN 'steady' ELSE 'swe' END, 'git', ?4, ?2 + i * ?3, ?5 FROM k`,
		181*100, start-174*day, steadyStep, commands, len(commands))
	e.recountActivity("steady")
	// THE WIDE PAGE: 40 pushes each setting MaxCommandsPerPush branches.
	var wideCmds []Command
	long := strings.Repeat("x", MaxRefNameBytes-len("refs/heads/")-len("-000"))
	for c := range MaxCommandsPerPush {
		wideCmds = append(wideCmds, Command{Ref: fmt.Sprintf("refs/heads/%s-%03d", long, c), Old: ZeroOID,
			New: fmt.Sprintf("%040x", c), Result: ResultOK})
	}
	wide, err := json.Marshal(wideCmds)
	if err != nil {
		b.Fatal(err)
	}
	e.exec(`WITH RECURSIVE k(i) AS (SELECT 1 UNION ALL SELECT i + 1 FROM k WHERE i < 40)
		INSERT INTO code_pushes (push_id, repo_id, position, at, actor, actor_kind, actor_seat, via, commands,
			received_at, row_bytes)
		SELECT printf('wide-%07d', i), 'wide', i, ?1 + i, 'swe', 'agent', 'swe', 'git', ?2, ?1 + i, ?3 FROM k`,
		start, string(wide), len(wide))
	e.recountActivity("wide")

	instant := func(at int64) time.Time { return time.UnixMicro(at).UTC() }
	midweek := PushFilter{Since: instant(start + 3*day), Until: instant(start + 4*day)}
	r := &Reader{pageRows: MaxSnapshotItems, pageBytes: MaxPageBytes, pushesScanned: MaxPushesScanned}
	for _, arm := range []struct {
		name  string
		repo  string
		f     *PushFilter
		limit int
		// kept is what the read answers, counted from the rows: SQL over the same predicate.
		kept string
		args []any
	}{
		{name: "pusher=dense", repo: "mono", f: &PushFilter{PusherKind: PusherAgent, Pusher: "dense"},
			kept: `actor_seat = 'dense'`},
		{name: "pusher=daily", repo: "mono", f: &PushFilter{PusherKind: PusherAgent, Pusher: "daily"},
			kept: `actor_seat = 'daily'`},
		{name: "pusher=rare", repo: "mono", f: &PushFilter{PusherKind: PusherPerson, Pusher: "rare"},
			kept: `actor_seat = 'rare'`},
		{name: "pusher=spread", repo: "mono", f: &PushFilter{PusherKind: PusherAgent, Pusher: "spread"},
			kept: `actor_seat = 'spread'`},
		{name: "pusher=gitlet", repo: "mono", f: &PushFilter{PusherKind: PusherGitlet}, kept: `via = 'merge'`},
		{name: "pusher=daily,window=a-day-midweek", repo: "mono", f: &PushFilter{PusherKind: PusherAgent,
			Pusher: "daily", Since: midweek.Since, Until: midweek.Until},
			kept: `actor_seat = 'daily' AND at >= ? AND at < ?`, args: []any{start + 3*day, start + 4*day}},
		{name: "window=today", repo: "mono", f: &PushFilter{Since: instant(start + 6*day)},
			kept: `at >= ?`, args: []any{start + 6*day}},
		{name: "window=a-day-midweek", repo: "mono", f: &midweek, kept: `at >= ? AND at < ?`,
			args: []any{start + 3*day, start + 4*day}},
		{name: "pusher=steady,limit=200", repo: "steady", f: &PushFilter{PusherKind: PusherAgent, Pusher: "steady"},
			limit: 200, kept: `actor_seat = 'steady'`},
		{name: "names=wide", repo: "wide", kept: `1 = 0`},
	} {
		var want int
		if err := e.db.Reader().Read(ctx, func(tx *sql.Tx) error {
			return tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM code_pushes WHERE repo_id = ? AND `+arm.kept,
				append([]any{arm.repo}, arm.args...)...).Scan(&want)
		}); err != nil {
			b.Fatal(err)
		}
		b.Run(arm.name, func(b *testing.B) {
			var pushes, pages int
			var slowest, whole time.Duration
			for range b.N {
				pushes, pages, slowest, whole = 0, 0, 0, 0
				q := ReadQuery{Kind: ReadPushes, Repo: arm.repo, PushFilter: arm.f, Limit: arm.limit}
				for {
					var out ReadAnswer
					began := time.Now()
					if err := e.db.Reader().Read(ctx, func(tx *sql.Tx) error {
						if arm.f == nil {
							return r.pushes(ctx, tx, ReadQuery{Kind: ReadPushes, Repo: arm.repo, Asker: Principal{
								Kind: PrincipalOperator, OperatorID: "pat:bench", Seat: "founder"}}, nil, &out)
						}
						return r.pushesNarrowed(ctx, tx, q, RepoRef{ID: arm.repo, Name: arm.repo}, &out)
					}); err != nil {
						b.Fatal(err)
					}
					took := time.Since(began) //fixedwait:measure a benchmark reports this; no gate reads it
					slowest, whole = max(slowest, took), whole+took
					pages++
					if arm.f == nil {
						break
					}
					pushes += len(out.Pushes)
					if out.Next == "" {
						break
					}
					q.Cursor = out.Next
				}
			}
			if pushes != want {
				b.Fatalf("the read answered %d pushes, want %d", pushes, want)
			}
			b.ReportMetric(float64(pages), "pages/op")
			b.ReportMetric(float64(pushes), "pushes/op")
			b.ReportMetric(float64(slowest.Microseconds())/1e3, "max-page-ms")
			b.ReportMetric(float64(whole.Microseconds())/1e3, "read-ms")
		})
	}
}
