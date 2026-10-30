package code

import (
	"context"
	"database/sql"
	"fmt"
	"testing"
	"time"
)

// THE CHECKS' BACKLOG, READ OVER APPLIED ROWS: attempts queued by records the
// shipped applier applies, and the alarm's reading of them at a moment each
// case chooses.

// queuedWithTimeout queues key at the instant at under a definition given
// timeout seconds to run.
func queuedWithTimeout(t testing.TB, key CheckKey, timeout int, at time.Time) built {
	return checkRec(t, OpCheckQueued, CheckQueued{
		CheckKey: key, Purpose: PurposeTip, Target: "refs/heads/main", Reason: QueueDispatch,
		Def:      &CheckDef{Name: key.Name, Run: "make " + key.Name, TimeoutSeconds: timeout},
		QueuedAt: at,
	})
}

// backlog is the checks' backlog the estate's rows hold at now.
func (e *estate) backlog(now time.Time) ChecksBacklog {
	e.t.Helper()
	var out ChecksBacklog
	if err := e.db.Reader().Read(context.Background(), func(tx *sql.Tx) error {
		var err error
		out, err = checksBacklogIn(context.Background(), tx, now)
		return err
	}); err != nil {
		e.t.Fatalf("read the backlog at %s: %v", now, err)
	}
	return out
}

// THE BACKLOG FINDS AN OVERDUE CHECK BEHIND A QUEUE OF LONGER ONES: more
// attempts of a fifty-minute check than a reading counts, all queued first,
// and then one attempt of a one-minute lint. Ten minutes on, the lint alone
// has waited past its own timeout — and the reading counts it, named, however
// many attempts were queued ahead of it, since it reads the queue in the order
// the attempts fell overdue. An hour on every attempt is past its own: the
// reading counts BacklogScan of them, says there are more, and names the
// longest wait among those it counted.
//
// Mutation: walk the queue by when each attempt was queued, as the reading
// once did — the window holds the long check's attempts, none of them overdue
// ten minutes on, and the lint is never read — red.
func TestTheBacklogFindsAnOverdueCheckBehindLongerOnes(t *testing.T) {
	t.Parallel()
	e := openChecksEstate(t)
	for i := range BacklogScan + 1 {
		e.apply(queuedWithTimeout(t, CheckKey{RepoID: checkRepo, OID: oid(fmt.Sprintf("long-%d", i)), Name: "unit",
			Attempt: 1}, 3_000, applyClock))
	}
	lint := CheckKey{RepoID: checkRepo, OID: oid("short"), Name: "lint", Attempt: 1}
	e.apply(queuedWithTimeout(t, lint, 60, applyClock.Add(time.Minute)))

	want := ChecksBacklog{Behind: 1, Oldest: 9 * time.Minute,
		Check: "checked@" + lint.OID[:12] + ": lint (attempt 1)"}
	if got := e.backlog(applyClock.Add(10 * time.Minute)); got != want {
		t.Fatalf("ten minutes on, behind %d attempts of a fifty-minute check, the backlog is %+v, want the "+
			"lint's one attempt %+v", BacklogScan+1, got, want)
	}
	got := e.backlog(applyClock.Add(time.Hour))
	if got.Behind != BacklogScan || !got.More || got.Oldest != time.Hour {
		t.Fatalf("an hour on, with %d attempts past their own timeouts, the backlog is %+v, want %d counted, "+
			"more said, and the longest wait an hour", BacklogScan+2, got, BacklogScan)
	}
	if got := e.backlog(applyClock.Add(time.Minute)); got != (ChecksBacklog{}) {
		t.Fatalf("a minute on, with every attempt inside its own timeout, the backlog is %+v, want none", got)
	}
}

// A DELETED REPOSITORY'S QUEUE IS NOBODY'S BACKLOG: an attempt past its own
// timeout counts while its repository is active, and while it is archived —
// a runner still claims and ends an archived repository's attempts — and not
// once the repository is deleted: every write of a deleted repository's
// checks is refused repo_deleted, the home's cancel and a runner's claim
// alike, so the attempt stays queued until the purge removes it, and counted
// it raised the alarm on every data node for the retention's days, sending an
// operator to git nodes that were healthy. Restored, it counts again.
//
// Mutation: drop the walk's test of the repository's state, and the deleted
// repository's attempt is counted — red.
func TestTheBacklogLeavesADeletedRepositorysChecksOut(t *testing.T) {
	t.Parallel()
	e := openChecksEstate(t)
	lint := CheckKey{RepoID: checkRepo, OID: oid("short"), Name: "lint", Attempt: 1}
	e.apply(queuedWithTimeout(t, lint, 60, applyClock))
	later := applyClock.Add(2 * time.Hour)
	archived := settingsFor("")
	archived.Archived = true
	restored := settingsFor("")
	for _, step := range []struct {
		what    string
		records []built
		behind  int
	}{
		{"active", nil, 1},
		{"archived", []built{rec(t, RepoSubject(checkRepo), OpSettings, Scope{RepoPath(checkRepo)},
			SettingsRecord{RepoID: checkRepo, Settings: archived})}, 1},
		{"deleted", []built{rec(t, RepoSubject(checkRepo), OpDelete, Scope{RepoPath(checkRepo)},
			LifecycleRecord{RepoID: checkRepo, ConfigEpoch: 1})}, 0},
		{"restored", []built{rec(t, RepoSubject(checkRepo), OpRestore, Scope{RepoPath(checkRepo)},
			LifecycleRecord{RepoID: checkRepo, ConfigEpoch: 1, Settings: &restored})}, 1},
	} {
		e.apply(step.records...)
		if got := e.backlog(later); got.Behind != step.behind {
			t.Fatalf("with its repository %s, an attempt two hours past its one-minute timeout reads as %+v, "+
				"want %d behind", step.what, got, step.behind)
		}
	}
}
