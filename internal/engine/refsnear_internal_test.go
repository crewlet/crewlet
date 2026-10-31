package engine

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/code"
	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/store"
)

// refsAlarm is code_refs_near_limit among alarms, and whether it fires.
func refsAlarm(alarms []statelog.Alarm) (statelog.Alarm, bool) {
	for _, a := range alarms {
		if a.Kind == statelog.KindCodeRefsNearLimit {
			return a, true
		}
	}
	return statelog.Alarm{}, false
}

// THE REFS ALARM IS MEASURED ON THE TRIM'S TICK, never on a report: ten
// evaluations between two ticks — the beats, operator requests and dashboard
// polls a quarter-hour holds — read the rows once, at the tick, and each
// carries the tick's figure with the bound and the window it was judged at. A
// tick whose read fails leaves the alarm silent, not standing on the last
// figure, and says so once — at the tick, never at every evaluation.
//
// Mutation: read the rows in retention.reading rather than on the tick — ten
// reads, red; keep the last figure when a read fails — the alarm stands, red;
// leave the bound or the window out of the reading — the detail names no
// bound, red.
func TestTheRefsAlarmIsMeasuredOnTheTrimTick(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	var (
		reads int
		fail  error
		logs  bytes.Buffer
	)
	r := &retention{
		logger: slog.New(slog.NewJSONHandler(&logs, nil)),
		refs: func(context.Context, time.Time) (code.RefsNear, bool, error) {
			reads++
			if fail != nil {
				return code.RefsNear{}, false, fail
			}
			return code.RefsNear{Repos: 1, Worst: "web", Live: 60_000, Recent: 45_000}, true, nil
		},
	}
	now := time.Now().UTC()
	evaluate := func() (alarm statelog.Alarm, firing bool) {
		for range 10 {
			alarm, firing = refsAlarm(statelog.Evaluate(r.reading(ctx, now, coord.BackupPoint{}, false, nil)))
		}
		return alarm, firing
	}

	if _, firing := evaluate(); firing || reads != 0 {
		t.Fatalf("before the trim's first tick the alarm fires %v after %d read(s), want silent after none",
			firing, reads)
	}
	r.measureRefs(ctx) // the tick
	alarm, firing := evaluate()
	if !firing || reads != 1 {
		t.Fatalf("after one tick and ten evaluations the alarm fires %v after %d read(s), want it firing "+
			"after the tick's one", firing, reads)
	}
	want := fmt.Sprintf("1 repository(ies) set refs so fast that they reach the %d-ref bound within 14 days "+
		"unless refs are deleted (the most: web, 60000 live and 45000 set in the last 14 days)",
		code.MaxRefsPerRepo)
	if !strings.HasPrefix(alarm.Detail, want) {
		t.Errorf("the alarm says %q, want it to begin %q: the bound and the window it was judged at", alarm.Detail,
			want)
	}

	fail = errors.New("the estate could not be read")
	r.measureRefs(ctx)
	if _, firing := evaluate(); firing || reads != 2 {
		t.Fatalf("after a tick whose read failed the alarm fires %v after %d read(s), want silent after the "+
			"tick's one more", firing, reads)
	}
	if got := strings.Count(logs.String(), `"msg":"code_refs_near_limit_unread"`); got != 1 {
		t.Errorf("a tick whose read failed, and ten evaluations after it, logged code_refs_near_limit_unread "+
			"%d time(s), want once: %s", got, logs.String())
	}
}

// THE REFS ALARM FIRES ON A RUNNING NODE (alarmwiring_internal_test.go's
// rule): a data node of a company hosting code, whose estate holds a
// repository with more than half the ref bound live, every one set within the
// lifecycle's window — past the point — raises code_refs_near_limit through
// the retention loop the engine built: its trim tick's evaluation takes the
// measurement, and the report every surface reads carries it to the table.
// The rows are seeded into the node's estate, the bulk no push of a case could
// make in reasonable time.
//
// Mutation: drop the measurement from retention.evaluate — silent, red; drop
// the refs field from startRetention's literal — silent, red; drop
// retention.reading's copy — silent, red.
func TestTheRefsNearLimitAlarmFiresOnARunningNode(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	e := newSandboxNode(t, parseCompany(t, codeReposDoc(webRepo(""), "")))
	servesCodeReads(t, e)
	reconciled(t, e)
	web := mustRepo(t, e, "web").ID
	r := quietRetention(t, e)

	past := code.MaxRefsPerRepo/2 + 1
	if err := e.backends.Store.Replicated().Tx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `WITH RECURSIVE k(i) AS (SELECT 0 UNION ALL SELECT i + 1 FROM k
			WHERE i + 1 < ?) INSERT INTO code_refs (repo_id, ref, target, position, at)
			SELECT ?, printf('refs/heads/b%06d', i), ?, 1, ? FROM k`, past, web, strings.Repeat("1", 40),
			store.EncodeTime(time.Now().Add(-time.Hour))); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, `UPDATE code_repos SET refs = refs + ? WHERE id = ?`, past, web)
		return err
	}); err != nil {
		t.Fatalf("seed web's refs: %v", err)
	}
	if _, firing := refsAlarm(r.Report(ctx).Alarms); firing {
		t.Fatal("the alarm fires before the trim's tick measured the seeded refs: a report read the rows")
	}

	r.evaluate(ctx)
	alarm, firing := refsAlarm(r.Report(ctx).Alarms)
	if !firing {
		t.Fatalf("after the trim's tick the node's report carries no %s for web, %d live refs every one "+
			"set within %s", statelog.KindCodeRefsNearLimit, past, code.RefExpiry)
	}
	if want := fmt.Sprintf("(the most: web, %d live and %d set", past, past); !strings.Contains(alarm.Detail,
		want) {
		t.Errorf("the alarm says %q, want web's figures %q", alarm.Detail, want)
	}
}
