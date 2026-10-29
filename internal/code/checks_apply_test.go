package code

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/statelog"
)

// THE CHECKS SUBJECT'S APPLIES, CASE BY CASE: records built here and applied
// at positions each case chooses — the position a record HAS, whatever order a
// case applies it in, since an order other than the log's is what a join must
// survive — and the rows read back.

// checkRepo is the repository the checks cases are about.
const checkRepo = "0190f3c4-0000-7000-8000-0000000000c1"

// checkKeyOf is attempt n of the named check of the cases' one commit.
func checkKeyOf(name string, attempt int) CheckKey {
	return CheckKey{RepoID: checkRepo, OID: oid("checked"), Name: name, Attempt: attempt}
}

// checkRec is a record of op on the cases' repository's checks subject.
func checkRec(t testing.TB, op OpKind, payload any) built {
	return rec(t, ChecksSubject(checkRepo), op, Scope{ChecksPath(checkRepo)}, payload)
}

// queuedRec queues key at the instant at, for a change's head.
func queuedRec(t testing.TB, key CheckKey, at time.Time) built {
	return checkRec(t, OpCheckQueued, CheckQueued{
		CheckKey: key, Purpose: PurposeTip, Target: "refs/heads/main", Reason: QueueDispatch,
		Def:      &CheckDef{Name: key.Name, Run: "make test", TimeoutSeconds: 600},
		QueuedAt: at,
	})
}

// startedRec is runner's claim of key at the instant at, naming box ("" on
// the claim itself).
func startedRec(t testing.TB, key CheckKey, runner, box string, at time.Time) built {
	return checkRec(t, OpCheckStarted, CheckStarted{CheckKey: key, Runner: runner,
		Placement: PlacementContainer, Box: box, StartedAt: at})
}

// endedRec ends key in state for reason at the instant at, keeping log where
// one is named; a run that exited exits 0 passing and 1 failing.
func endedRec(t testing.TB, key CheckKey, state CheckState, reason CheckReason, at time.Time,
	log *CheckLog) built {

	f := CheckFinished{CheckKey: key, State: state, Reason: reason, Summary: string(state) + " by " +
		string(reason), FinishedAt: at, Log: log}
	if reason == CheckReasonExit {
		code := 1
		if state == CheckStatePassed {
			code = 0
		}
		f.ExitCode = &code
	}
	if state == CheckStateFailed {
		f.Tail = "--- FAIL: TestSomething"
	}
	return checkRec(t, OpCheckFinished, f)
}

// logOf is a kept log stored as the object a pack of id's would be.
func logOf(id string) *CheckLog {
	p := packOf(id)
	return &CheckLog{Object: p.Object, Hash: p.Hash, Size: p.Size}
}

// applyAt applies records at the sequences each is given, in the order given.
func (e *estate) applyAt(records map[uint64]built, order ...uint64) {
	e.t.Helper()
	for _, seq := range order {
		e.next = seq
		e.apply(records[seq])
	}
}

// checksDump is every row of code_checks and code_check_logs, in key order,
// as text: two estates hold one state of a repository's checks iff their
// dumps are equal.
func (e *estate) checksDump() string {
	e.t.Helper()
	var b strings.Builder
	for _, table := range []string{"code_checks", "code_check_logs"} {
		if err := e.db.Reader().Read(context.Background(), func(tx *sql.Tx) error {
			rows, err := tx.QueryContext(context.Background(),
				`SELECT * FROM `+table+` ORDER BY repo_id, oid, name, attempt`)
			if err != nil {
				return err
			}
			defer func() { _ = rows.Close() }()
			cols, err := rows.Columns()
			if err != nil {
				return err
			}
			for rows.Next() {
				values := make([]any, len(cols))
				ptrs := make([]any, len(cols))
				for i := range values {
					ptrs[i] = &values[i]
				}
				if err := rows.Scan(ptrs...); err != nil {
					return err
				}
				fmt.Fprintf(&b, "%s %v\n", table, values)
			}
			return rows.Err()
		}); err != nil {
			e.t.Fatalf("dump %s: %v", table, err)
		}
	}
	return b.String()
}

// checkColumn is one column of key's row.
func (e *estate) checkColumn(key CheckKey, column string) string {
	e.t.Helper()
	return fmt.Sprint(e.one(`SELECT `+column+` FROM code_checks WHERE repo_id = ? AND oid = ? AND name = ? AND
		attempt = ?`, key.RepoID, key.OID, key.Name, key.Attempt))
}

// openChecksEstate is an estate holding the cases' repository.
func openChecksEstate(t *testing.T) *estate {
	t.Helper()
	e := openApplyEstate(t)
	e.apply(create(t, checkRepo, "checked"))
	return e
}

// TWO STARTS OF ONE ATTEMPT CONVERGE IN EITHER ORDER (M5-D18): a check's
// records carry no expectation, so two runners' claims of one attempt — a
// duplicate across a writer's failover — both land, and every node must keep
// the same one whichever it applies first. The join keeps the claim with the
// lower (StartedAt, Runner), each record at its own position, so the two
// orders leave one row byte for byte — and the box the kept claim's holder
// names, never the other's.
//
// Mutation: keep whichever claim the row holds first (first in log order
// wins) — the two orders keep two runners, red.
func TestTwoStartsOfOneAttemptConvergeInEitherOrder(t *testing.T) {
	t.Parallel()
	key := checkKeyOf("unit", 1)
	for name, tc := range map[string]struct {
		early, late  built
		runner, box  string
		holdersBox   built
		losersBoxRec built
	}{
		"the earlier start": {
			early:  startedRec(t, key, "node-b", "", applyClock.Add(time.Second)),
			late:   startedRec(t, key, "node-a", "", applyClock.Add(2*time.Second)),
			runner: "node-b", box: "box-b",
			holdersBox:   startedRec(t, key, "node-b", "box-b", applyClock.Add(time.Second)),
			losersBoxRec: startedRec(t, key, "node-a", "box-a", applyClock.Add(2*time.Second)),
		},
		"one instant, the lower runner": {
			early:  startedRec(t, key, "node-a", "", applyClock.Add(time.Second)),
			late:   startedRec(t, key, "node-b", "", applyClock.Add(time.Second)),
			runner: "node-a", box: "box-a",
			holdersBox:   startedRec(t, key, "node-a", "box-a", applyClock.Add(time.Second)),
			losersBoxRec: startedRec(t, key, "node-b", "box-b", applyClock.Add(time.Second)),
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			records := map[uint64]built{
				10: queuedRec(t, key, applyClock), 11: tc.early, 12: tc.late,
				13: tc.holdersBox, 14: tc.losersBoxRec,
			}
			forward, backward := openChecksEstate(t), openChecksEstate(t)
			forward.applyAt(records, 10, 11, 12, 13, 14)
			backward.applyAt(records, 14, 13, 12, 11, 10)
			if got, want := backward.checksDump(), forward.checksDump(); got != want {
				t.Fatalf("two orders of one attempt's records left two rows:\n%s\nand\n%s", want, got)
			}
			if got := forward.checkColumn(key, "runner_node"); got != tc.runner {
				t.Fatalf("the attempt's runner is %q, want %s — the lower (StartedAt, Runner)", got, tc.runner)
			}
			if got := forward.checkColumn(key, "box_handle"); got != tc.box {
				t.Fatalf("the attempt's box is %q, want the kept claim's own, %s", got, tc.box)
			}
			if got := forward.checkColumn(key, "state"); got != string(CheckStateRunning) {
				t.Fatalf("the attempt is %s, want running", got)
			}
		})
	}
}

// A CHECK ROW ONLY MOVES FORWARD: queued, then running, then an ending — and
// never back, whatever arrives after it. A claim that lands after the ending
// it raced — its runner read the row before the ending applied — does not make
// an ended attempt running again, nor does a queuing record a failover sent
// twice make it queued; the state is derived from what the row holds, and an
// ending once held is held for good. The same records in the opposite order
// leave the same row.
//
// Mutation: derive the state from the claim before the ending (running where
// a runner is named) and the late claim reopens the ended attempt — red.
func TestACheckRowOnlyMovesForward(t *testing.T) {
	t.Parallel()
	key := checkKeyOf("unit", 1)
	records := map[uint64]built{
		10: queuedRec(t, key, applyClock),
		11: startedRec(t, key, "node-a", "", applyClock.Add(time.Second)),
		12: endedRec(t, key, CheckStatePassed, CheckReasonExit, applyClock.Add(time.Minute), nil),
		13: startedRec(t, key, "node-b", "", applyClock.Add(2*time.Second)),
		14: queuedRec(t, key, applyClock.Add(3*time.Second)),
	}
	e := openChecksEstate(t)
	for _, step := range []struct {
		seq  uint64
		want CheckState
	}{
		{10, CheckStateQueued}, {11, CheckStateRunning}, {12, CheckStatePassed},
		{13, CheckStatePassed}, {14, CheckStatePassed},
	} {
		e.applyAt(records, step.seq)
		if got := e.checkColumn(key, "state"); got != string(step.want) {
			t.Fatalf("after the record at %d the attempt is %s, want %s", step.seq, got, step.want)
		}
	}
	if got := e.checkColumn(key, "queued_position"); got != fmt.Sprint(statelog.Position{Generation: 1,
		Seq: 10}.Packed()) {
		t.Fatalf("the attempt's queue is the record at %s, want the first queuing record's", got)
	}
	reversed := openChecksEstate(t)
	reversed.applyAt(records, 14, 13, 12, 11, 10)
	if got, want := reversed.checksDump(), e.checksDump(); got != want {
		t.Fatalf("the reversed order left another row:\n%s\nwant\n%s", got, want)
	}
}

// A TERMINAL TIE KEEPS THE MEASUREMENT (M5-D18): two endings of one attempt —
// a dead runner's sweep that errored it and the runner's own report that it
// failed, say — keep the one that measured the commit, whichever came first
// and whichever landed first: a failed or passed run says more of the commit
// than one that never ran to a result. Between two measurements, or two of
// infrastructure's, the earlier ending is kept. The log follows the ending
// kept, so a failed run's log survives an infrastructure ending after it, and
// an ending kept with no log leaves none.
//
// Mutation: drop the measurement's precedence and the earlier errored ending
// is kept over the failed run — red.
func TestATerminalTieKeepsTheMeasurement(t *testing.T) {
	t.Parallel()
	key := checkKeyOf("unit", 1)
	for name, tc := range map[string]struct {
		first, second built
		want          CheckState
		log           bool
	}{
		"an earlier infrastructure ending and a later failure": {
			first:  endedRec(t, key, CheckStateErrored, CheckReasonRunnerLost, applyClock.Add(time.Minute), nil),
			second: endedRec(t, key, CheckStateFailed, CheckReasonExit, applyClock.Add(time.Hour), logOf("log-1")),
			want:   CheckStateFailed, log: true,
		},
		"two measurements": {
			first:  endedRec(t, key, CheckStateFailed, CheckReasonExit, applyClock.Add(time.Minute), logOf("log-2")),
			second: endedRec(t, key, CheckStatePassed, CheckReasonExit, applyClock.Add(time.Hour), nil),
			want:   CheckStateFailed, log: true,
		},
		"two endings of infrastructure": {
			first:  endedRec(t, key, CheckStateCancelled, CheckReasonSuperseded, applyClock.Add(time.Minute), nil),
			second: endedRec(t, key, CheckStateErrored, CheckReasonBoxFailed, applyClock.Add(time.Hour), nil),
			want:   CheckStateCancelled,
		},
		"a measurement with no log over a failure's": {
			first:  endedRec(t, key, CheckStatePassed, CheckReasonExit, applyClock.Add(time.Minute), nil),
			second: endedRec(t, key, CheckStateFailed, CheckReasonExit, applyClock.Add(time.Hour), logOf("log-3")),
			want:   CheckStatePassed,
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			records := map[uint64]built{10: queuedRec(t, key, applyClock), 11: tc.first, 12: tc.second}
			forward, backward := openChecksEstate(t), openChecksEstate(t)
			forward.applyAt(records, 10, 11, 12)
			backward.applyAt(records, 12, 11, 10)
			if got, want := backward.checksDump(), forward.checksDump(); got != want {
				t.Fatalf("two orders of one attempt's endings left two rows:\n%s\nand\n%s", want, got)
			}
			if got := forward.checkColumn(key, "state"); got != string(tc.want) {
				t.Fatalf("the attempt ended %s, want %s", got, tc.want)
			}
			if logs := forward.num(`SELECT COUNT(*) FROM code_check_logs`); (logs == 1) != tc.log || logs > 1 {
				t.Fatalf("the attempt keeps %d logs, want one: %v — the log is the kept ending's", logs, tc.log)
			}
		})
	}
}

// A BOX HANDLE IS SET ONCE, by the holder of the kept claim: empty on the
// claim, named by the holder's later record — which repeats its claim's
// StartedAt and Runner, so it is the same claim — and moved by nothing after:
// not a record of a losing claim naming its own box, and not the holder's
// record again, which changes no row.
//
// Mutation: take the box of any record about the attempt — the losing
// claim's box replaces the holder's, red.
func TestABoxHandleIsSetOnce(t *testing.T) {
	t.Parallel()
	key := checkKeyOf("unit", 1)
	e := openChecksEstate(t)
	at := applyClock.Add(time.Second)
	e.apply(queuedRec(t, key, applyClock), startedRec(t, key, "node-a", "", at))
	if got := e.checkColumn(key, "box_handle"); got != "" {
		t.Fatalf("the claim named box %q; a claim is made before its box", got)
	}
	named := startedRec(t, key, "node-a", "box-a", at)
	e.apply(named, startedRec(t, key, "node-b", "box-b", at.Add(time.Second)))
	if got := e.checkColumn(key, "box_handle"); got != "box-a" {
		t.Fatalf("the attempt's box is %q, want the holder's box-a", got)
	}
	if rows := e.applyCounting(named); rows != 0 {
		t.Fatalf("the holder's record again wrote %d rows; the box is set once", rows)
	}
}

// THE CHECKS' HORIZON IS PER REPOSITORY: an ending removes its OWN
// repository's rows that ended before its broker instant less CheckHistory —
// with their logs — and no other repository's, nor an attempt of its own that
// has not ended, however long ago it was queued.
//
// Mutation: delete the horizon's rows across every repository (the delete's
// repository term made one every row meets) and the other repository's older
// row goes in its own repository's row's place — red.
func TestTheChecksHorizonIsPerRepo(t *testing.T) {
	t.Parallel()
	const other = "0190f3c4-0000-7000-8000-0000000000c2"
	e := openChecksEstate(t)
	e.apply(create(t, other, "other"))
	old := applyClock.Add(-CheckHistory - 24*time.Hour)
	stale, waiting := checkKeyOf("unit", 1), checkKeyOf("lint", 1)
	othersKey := CheckKey{RepoID: other, OID: oid("checked"), Name: "unit", Attempt: 1}
	// ENDED A MONTH AND A DAY AGO, each applied at its own old instant so
	// its own horizon spares it — the other repository's an hour earlier
	// still, so it is the oldest of all, and a horizon read across
	// repositories would take it first.
	older := old.Add(-time.Hour)
	e.apply(queuedRec(t, stale, old).at(old), queuedRec(t, waiting, old).at(old),
		endedRec(t, stale, CheckStateFailed, CheckReasonExit, old, logOf("stale-log")).at(old),
		rec(t, ChecksSubject(other), OpCheckQueued, Scope{ChecksPath(other)}, CheckQueued{
			CheckKey: othersKey, Purpose: PurposeTip, Target: "refs/heads/main", Reason: QueueDispatch,
			Def: &CheckDef{Name: "unit", Run: "make test", TimeoutSeconds: 600}, QueuedAt: older,
		}).at(older),
		rec(t, ChecksSubject(other), OpCheckFinished, Scope{ChecksPath(other)}, CheckFinished{
			CheckKey: othersKey, State: CheckStateErrored, Reason: CheckReasonRunnerLost, FinishedAt: older,
		}).at(older))
	if n := e.num(`SELECT COUNT(*) FROM code_checks`); n != 3 {
		t.Fatalf("the premise: %d rows, want the three old ones", n)
	}
	fresh := checkKeyOf("unit", 2)
	e.apply(queuedRec(t, fresh, applyClock), endedRec(t, fresh, CheckStatePassed, CheckReasonExit,
		applyClock, nil))
	if n := e.num(`SELECT COUNT(*) FROM code_checks WHERE repo_id = ? AND name = 'unit' AND attempt = 1`,
		checkRepo); n != 0 {
		t.Fatalf("the ending kept its repository's row that ended past the history")
	}
	if n := e.num(`SELECT COUNT(*) FROM code_check_logs`); n != 0 {
		t.Fatalf("the ending kept %d logs of the rows it removed", n)
	}
	if n := e.num(`SELECT COUNT(*) FROM code_checks WHERE repo_id = ?`, other); n != 1 {
		t.Fatalf("an ending of %s removed another repository's row", checkRepo)
	}
	if got := e.checkColumn(waiting, "state"); got != string(CheckStateQueued) {
		t.Fatalf("an attempt queued past the history and never ended is %q, want kept queued", got)
	}
}

// A SWEEP NAMES AN INSTANT BEFORE ITS OWN: a checks_sweep whose Through is at
// or past the record's broker instant would remove checks that ended after it
// landed, and is a record no writer forms — an error, as a retire sweep's
// position past its own is, never a skip.
func TestASweepPastItsOwnInstantIsRefused(t *testing.T) {
	t.Parallel()
	e := openChecksEstate(t)
	sweep := checkRec(t, OpChecksSweep, ChecksSweep{RepoID: checkRepo, Through: applyClock})
	if err := e.applyErr(sweep); err == nil || !strings.Contains(err.Error(), "no writer forms it") {
		t.Fatalf("a sweep through its own instant applied: %v", err)
	}
	if err := e.applyErr(sweep.at(applyClock.Add(time.Second))); err != nil {
		t.Fatalf("a sweep through an instant before its own was refused: %v", err)
	}
}

// A RECORD ON ONE REPOSITORY'S CHECKS SUBJECT WRITES ONLY ITS CHECKS: one
// naming another repository in its payload is a record no writer forms, and
// would escape that repository's deletion gate, which reads the subject.
func TestAChecksRecordNamesItsOwnRepository(t *testing.T) {
	t.Parallel()
	e := openChecksEstate(t)
	stray := CheckKey{RepoID: "0190f3c4-0000-7000-8000-0000000000c9", OID: oid("checked"), Name: "unit",
		Attempt: 1}
	if err := e.applyErr(queuedRec(t, stray, applyClock)); err == nil ||
		!strings.Contains(err.Error(), "may write only that repository's checks") {
		t.Fatalf("a record about another repository's checks applied: %v", err)
	}
}

// A PURGE PART IS BOUNDED BY WHAT ITS CHECKS WEIGH, in the budget its pushes
// and its changes share: check rows go in the horizon's order, each with its
// log, until their row_bytes reach PurgePartBytes — at most one row past it —
// and the next part takes the rest. A check row carries a failed run's
// definition, summary, tail and failures, and deleting it frees their overflow
// pages, which a count of rows does not see.
//
// Mutation: take a part's checks by their rows alone, and the fifteen go in one
// part — red.
func TestAPurgePartIsBoundedByWhatItsChecksWeigh(t *testing.T) {
	t.Parallel()
	e := openChecksEstate(t)
	e.apply(rec(t, RepoSubject(checkRepo), OpDelete, Scope{RepoPath(checkRepo)},
		LifecycleRecord{RepoID: checkRepo, ConfigEpoch: 1}))
	// A WIDTH THAT DOES NOT DIVIDE THE BUDGET: eleven rows of 3 MiB are the
	// first to reach 32 MiB.
	const width, seeded = 3 << 20, 15
	for i := range seeded {
		key := checkKeyOf(fmt.Sprintf("check-%02d", i), 1)
		e.apply(queuedRec(t, key, applyClock),
			endedRec(t, key, CheckStateFailed, CheckReasonExit, applyClock.Add(time.Duration(i)*time.Second),
				logOf(key.Name)))
	}
	e.exec(`UPDATE code_checks SET row_bytes = ? WHERE repo_id = ?`, width, checkRepo)
	e.apply(partOf(t, checkRepo, 1))
	took := (PurgePartBytes + width - 1) / width
	if left := e.num(`SELECT COUNT(*) FROM code_checks`); left != int64(seeded-took) {
		t.Fatalf("part 1 left %d of %d checks of %d bytes, want %d: rows until their bytes reach "+
			"PurgePartBytes (%d), and no further", left, seeded, width, seeded-took, PurgePartBytes)
	}
	if left := e.num(`SELECT COUNT(*) FROM code_check_logs`); left != int64(seeded-took) {
		t.Fatalf("part 1 left %d logs beside %d checks — a check's log goes with it", left, seeded-took)
	}
	if first := e.num(`SELECT COUNT(*) FROM code_checks WHERE name = 'check-00'`); first != 0 {
		t.Fatalf("part 1 kept the oldest check: a part takes them oldest first")
	}
	e.apply(partOf(t, checkRepo, 2))
	if checks, logs := e.num(`SELECT COUNT(*) FROM code_checks`), e.num(`SELECT COUNT(*) FROM code_check_logs`); checks != 0 || logs != 0 {
		t.Fatalf("part 2 left %d checks and %d logs", checks, logs)
	}
}

// EVERY ORDER OF A CHECK'S RECORDS JOINS TO ONE ROW — the join itself, over
// every order of one attempt's parts: a queue sent twice, three claims (two of
// them one claim, its box named by the second), and three endings. Each part
// carries the position its record has, so the orders differ only in what the
// join meets first.
func TestEveryOrderOfACheckRecordsJoinsToOne(t *testing.T) {
	t.Parallel()
	parts := []checkPart{
		{queue: &checkQueue{purpose: "head", target: "refs/heads/main", def: `{"name":"unit"}`, position: 10}},
		{queue: &checkQueue{purpose: "head", target: "refs/heads/other", def: `{"name":"unit"}`, position: 11}},
		{run: &checkRun{runner: "node-b", placement: "container", startedAt: 5}},
		{run: &checkRun{runner: "node-b", placement: "container", box: "box-b", borrowed: true, startedAt: 5}},
		{run: &checkRun{runner: "node-a", placement: "e2b", startedAt: 6}},
		{ending: &checkEnding{state: "errored", reason: "runner_lost", finishedAt: 7, position: 14}},
		{ending: &checkEnding{state: "failed", reason: "exit", finishedAt: 9, position: 15}},
		{ending: &checkEnding{state: "passed", reason: "exit", finishedAt: 8, position: 16}},
	}
	join := func(order []int) checkRow {
		var row checkRow
		for _, i := range order {
			p := parts[i]
			if p.queue != nil {
				row.joinQueue(*p.queue)
			}
			if p.run != nil {
				row.joinRun(*p.run)
			}
			if p.ending != nil {
				row.joinEnding(*p.ending)
			}
		}
		return row
	}
	identity := make([]int, len(parts))
	for i := range identity {
		identity[i] = i
	}
	want := join(identity)
	if want.queue.position != 10 || want.run.box != "box-b" || !want.run.borrowed || want.run.runner != "node-b" ||
		want.ending.state != "passed" {
		t.Fatalf("the join kept %+v; want the first queue, node-b's claim with its box, and the earlier "+
			"measurement", want)
	}
	orders := 0
	var permute func(k int)
	permute = func(k int) {
		if k == len(identity) {
			orders++
			if got := join(identity); got != want {
				t.Fatalf("the order %v joined to %+v, and the log's order to %+v", identity, got, want)
			}
			return
		}
		for i := k; i < len(identity); i++ {
			identity[k], identity[i] = identity[i], identity[k]
			permute(k + 1)
			identity[k], identity[i] = identity[i], identity[k]
		}
	}
	permute(0)
	if orders != 40320 {
		t.Fatalf("joined %d orders, want every one of 8!", orders)
	}
}
