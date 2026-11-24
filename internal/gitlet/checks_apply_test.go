package gitlet

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/queue/topics"
	"github.com/crewlet/crewlet/internal/statelog"
)

// THE CHECKS SUBJECT'S APPLIES, CASE BY CASE: each record keeps the position
// it HAS whatever order a case applies it in, since an order other than the
// log's is what a join must survive.

// checkRepo is the repository the checks cases are about.
const checkRepo = "0190f3c4-0000-7000-8000-0000000000c1"

// checkKeyOf is attempt n of the named check of the cases' one commit.
func checkKeyOf(name string, attempt int) CheckKey {
	return CheckKey{RepoID: checkRepo, OID: oid("checked"), Target: "refs/heads/main", Name: name, Attempt: attempt}
}

// checkRec is a record of op on the cases' repository's checks subject.
func checkRec(t testing.TB, op OpKind, payload any) built {
	return rec(t, ChecksSubject(checkRepo), op, Scope{ChecksPath(checkRepo)}, payload)
}

// queuedRec queues key at the instant at, for a change's head.
func queuedRec(t testing.TB, key CheckKey, at time.Time) built {
	return checkRec(t, OpCheckQueued, CheckQueued{
		CheckKey: key, Purpose: PurposeTip, Reason: QueueDispatch,
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
				`SELECT * FROM `+table+` ORDER BY repo_id, commit_oid, name, attempt`)
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
	return fmt.Sprint(e.one(`SELECT `+column+` FROM code_checks WHERE repo_id = ? AND commit_oid = ? AND
		name = ? AND attempt = ?`, key.RepoID, key.OID, key.Name, key.Attempt))
}

// openChecksEstate is an estate holding the cases' repository.
func openChecksEstate(t *testing.T) *estate {
	t.Helper()
	e := openApplyEstate(t)
	e.apply(create(t, checkRepo, "checked"))
	return e
}

// TWO STARTS OF ONE ATTEMPT CONVERGE IN EITHER ORDER (M5-D18): a check's
// records carry no expectation, so two runners' claims of one attempt (a
// duplicate across a writer's failover) both land. The join keeps the lower
// (StartedAt, Runner), so both orders leave one row byte for byte, with the
// kept claim's box, never the other's.
//
// Mutation: first in log order wins, and the two orders keep two runners, red.
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

// A CHECK ROW ONLY MOVES FORWARD: queued, running, ended, never back. A claim
// landing after the ending it raced does not reopen the attempt, nor does a
// queuing record a failover sent twice requeue it: the state is derived from
// what the row holds, and an ending is held for good. The opposite order
// leaves the same row.
//
// Mutation: derive the state from the claim before the ending, and the late
// claim reopens the attempt, red.
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

// A TERMINAL TIE KEEPS THE MEASUREMENT (M5-D18): of two endings of one attempt
// (a dead runner's sweep that errored it, the runner's own failure), the one
// that measured the commit is kept, whichever is earlier and whichever lands
// first: a failed or passed run says more than one that never ran to a result. Between two measurements, or
// two of infrastructure's, the earlier is kept. The log follows the kept
// ending: a failed run's log survives a later infrastructure ending, and a
// kept ending with no log leaves none.
//
// Mutation: drop the measurement's precedence, and the earlier errored ending
// is kept, red.
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

// A BOX HANDLE IS SET ONCE, by the kept claim's holder: empty on the claim,
// named by the holder's later record (same StartedAt and Runner, so the same
// claim), and moved by nothing after: not a losing claim's box, and not the
// holder's record again, which changes no row.
//
// Mutation: take the box of any record about the attempt, and the losing
// claim's replaces the holder's, red.
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
// repository's rows, with their logs, that ended before its broker instant
// less CheckHistory: no other repository's, and no attempt of its own that has
// not ended, however long ago it was queued.
//
// Mutation: drop the delete's repository term, and the other repository's
// older row goes in its own repository's row's place, red.
func TestTheChecksHorizonIsPerRepo(t *testing.T) {
	t.Parallel()
	const other = "0190f3c4-0000-7000-8000-0000000000c2"
	e := openChecksEstate(t)
	e.apply(create(t, other, "other"))
	old := applyClock.Add(-CheckHistory - 24*time.Hour)
	stale, waiting := checkKeyOf("unit", 1), checkKeyOf("lint", 1)
	othersKey := CheckKey{RepoID: other, OID: oid("checked"), Target: "refs/heads/main", Name: "unit", Attempt: 1}
	// ENDED A MONTH AND A DAY AGO, each applied at its own old instant so its
	// own horizon spares it; the other repository's an hour earlier still, the
	// oldest of all, which a horizon read across repositories takes first.
	older := old.Add(-time.Hour)
	e.apply(queuedRec(t, stale, old).at(old), queuedRec(t, waiting, old).at(old),
		endedRec(t, stale, CheckStateFailed, CheckReasonExit, old, logOf("stale-log")).at(old),
		rec(t, ChecksSubject(other), OpCheckQueued, Scope{ChecksPath(other)}, CheckQueued{
			CheckKey: othersKey, Purpose: PurposeTip, Reason: QueueDispatch,
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

// A SWEEP NAMES AN INSTANT BEFORE ITS OWN: a checks_sweep through its broker
// instant or later would remove checks ended after it landed, and no writer
// forms one: an error, as a retire sweep's position past its own is, never a
// skip.
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
	stray := CheckKey{RepoID: "0190f3c4-0000-7000-8000-0000000000c9", OID: oid("checked"), Target: "refs/heads/main", Name: "unit",
		Attempt: 1}
	if err := e.applyErr(queuedRec(t, stray, applyClock)); err == nil ||
		!strings.Contains(err.Error(), "may write only that repository's checks") {
		t.Fatalf("a record about another repository's checks applied: %v", err)
	}
}

// A REPOSITORY'S DISPATCHES GO WITH ITS PURGE: a purge part with room takes a
// deleted repository's code_check_dispatches and code_check_targets rows once
// its checks are gone, as it takes every row the repository's records wrote.
//
// Mutation: leave the dispatches out of a purge part, and they wait for the
// final purge, red.
func TestAPurgePartTakesARepositorysDispatches(t *testing.T) {
	t.Parallel()
	e := openChecksEstate(t)
	tip := statelog.Position{Stream: topics.CodeLogStream, Generation: 1, Seq: 2}
	dispatched := func(target string, heads ...string) built {
		t.Helper()
		named := make([]DispatchedHead, 0, len(heads))
		for i, h := range heads {
			named = append(named, DispatchedHead{OID: oid(h),
				ChangeID: fmt.Sprintf("0190f3c4-0000-7000-8000-0000000000e%d", i), Revision: 1})
		}
		return checkRec(t, OpCheckDispatched, CheckDispatched{RepoID: checkRepo, Target: target, Tip: oid("tip"),
			TipPosition: tip, DefsOID: oid("defs"), Heads: named, DispatchedAt: applyClock})
	}
	held := func() int64 {
		t.Helper()
		return e.num(`SELECT COUNT(*) FROM code_check_dispatches`) + e.num(`SELECT COUNT(*) FROM code_check_targets`)
	}
	e.apply(dispatched("refs/heads/main", "a", "b"), dispatched("refs/heads/release", "c"))
	if n := held(); n != 5 {
		t.Fatalf("two targets' readings and three heads' dispatches left %d rows, want 5", n)
	}
	e.apply(rec(t, RepoSubject(checkRepo), OpDelete, Scope{RepoPath(checkRepo)},
		LifecycleRecord{RepoID: checkRepo, ConfigEpoch: 1}))
	e.apply(partOf(t, checkRepo, 1))
	if n := held(); n != 0 {
		t.Fatalf("a purge part with room left %d of the repository's dispatches and readings", n)
	}
}

// A PURGE PART IS BOUNDED BY WHAT ITS CHECKS WEIGH, in the budget its pushes
// and changes share: check rows go in the horizon's order, each with its log,
// until their row_bytes reach PurgePartBytes (at most one row past it), and
// the next part takes the rest. A failed run's row carries its definition,
// summary, tail and failures, whose overflow pages a row count does not see.
//
// Mutation: bound a part by rows alone, and the fifteen go in one part, red.
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

// EVERY ORDER OF A CHECK'S RECORDS JOINS TO ONE ROW: the join itself over all
// 8! orders of one attempt's parts (a queue sent twice, three claims of which
// two are one claim naming its box second, three endings). Each part carries
// its record's position, so the orders differ only in what the join meets
// first.
func TestEveryOrderOfACheckRecordsJoinsToOne(t *testing.T) {
	t.Parallel()
	parts := []checkPart{
		{queue: &checkQueue{purpose: "head", defsOID: oid("defs-1"), def: `{"name":"unit"}`, position: 10}},
		{queue: &checkQueue{purpose: "head", defsOID: oid("defs-2"), def: `{"name":"unit"}`, position: 11}},
		{run: &checkRun{runner: "node-b", placement: "container", startedAt: 5}},
		{run: &checkRun{runner: "node-b", placement: "container", box: "box-b", startedAt: 5}},
		{run: &checkRun{runner: "node-a", placement: "container", startedAt: 6}},
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
	if want.queue.position != 10 || want.run.box != "box-b" || want.run.runner != "node-b" ||
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

// countedRec is a failed exit of key at the instant at carrying tests, lines and cut beside the
// two-line tail "ok\nFAIL\n".
func countedRec(t testing.TB, key CheckKey, at time.Time, tests *CheckTests, lines int, cut bool) built {
	code := 1
	return checkRec(t, OpCheckFinished, CheckFinished{CheckKey: key, State: CheckStateFailed,
		Reason: CheckReasonExit, ExitCode: &code, Summary: "1 of 214 tests failed", FinishedAt: at,
		Tail: "ok\nFAIL\n", Tests: tests, LogLines: lines, TailCut: cut})
}

// AN ENDING'S COUNTS MOVE WITH THE ENDING THE JOIN KEEPS: an errored runner_lost ending without
// counts and a failed exit with them leave the failed ending's counts in both orders; an earlier
// failed ending with counts of its own replaces them.
//
// Mutation: write the counts outside checkEnding (the backward order keeps the runner_lost
// ending's NULL, red).
func TestAnEndingsCountsMoveWithTheEndingTheJoinKeeps(t *testing.T) {
	t.Parallel()
	key := checkKeyOf("unit", 1)
	records := map[uint64]built{
		10: queuedRec(t, key, applyClock),
		11: endedRec(t, key, CheckStateErrored, CheckReasonRunnerLost, applyClock.Add(time.Minute), nil),
		12: countedRec(t, key, applyClock.Add(time.Hour), &CheckTests{Total: 214, Failed: 1}, 1201, true),
		13: countedRec(t, key, applyClock.Add(30*time.Minute), &CheckTests{Total: 9, Failed: 2, Skipped: 3}, 40,
			false),
	}
	forward, backward := openChecksEstate(t), openChecksEstate(t)
	forward.applyAt(records, 10, 11, 12)
	backward.applyAt(records, 12, 11, 10)
	if got, want := backward.checksDump(), forward.checksDump(); got != want {
		t.Fatalf("two orders of one attempt's endings left two rows:\n%s\nand\n%s", want, got)
	}
	counts := func(e *estate) string {
		return strings.Join([]string{e.checkColumn(key, "tests_total"), e.checkColumn(key, "tests_failed"),
			e.checkColumn(key, "tests_skipped"), e.checkColumn(key, "log_lines"), e.checkColumn(key, "tail_cut")}, " ")
	}
	if got, want := counts(backward), "214 1 0 1201 1"; got != want {
		t.Fatalf("the failed ending's row counts %q, want %q", got, want)
	}
	forward.applyAt(records, 13)
	if got, want := counts(forward), "9 2 3 40 0"; got != want {
		t.Fatalf("the earlier failed ending's row counts %q, want its own %q", got, want)
	}
}

// ONLY AN ENDING THAT COUNTS STAMPS ITS RECORD at CheckCountsVersion, through the production
// encoder: an ending carrying tests, lines or a cut alone is stamped it, and a passed ending
// carrying none, and the home's runner_lost ending, are stamped the base.
//
// Mutations: drop a versionedFields row (that case stamped the base, red); tag Total omitempty and
// Tests a value (every ending stamped CheckCountsVersion, red).
func TestOnlyAnEndingThatCountsStampsItsRecord(t *testing.T) {
	t.Parallel()
	key := checkKeyOf("unit", 1)
	zero := 0
	passed := CheckFinished{CheckKey: key, State: CheckStatePassed, Reason: CheckReasonExit, ExitCode: &zero,
		FinishedAt: applyClock}
	withTests := passed
	withTests.Tests = &CheckTests{}
	for name, tc := range map[string]struct {
		record built
		want   int
	}{
		"an ending with tests alone":     {checkRec(t, OpCheckFinished, withTests), CheckCountsVersion},
		"an ending with log_lines alone": {countedRec(t, key, applyClock, nil, 2, false), CheckCountsVersion},
		"an ending with tail_cut alone":  {countedRec(t, key, applyClock, nil, 0, true), CheckCountsVersion},
		"a passed ending with none":      {checkRec(t, OpCheckFinished, passed), baseRecordVersion},
		"the home's runner_lost ending": {endedRec(t, key, CheckStateErrored, CheckReasonRunnerLost, applyClock,
			nil), baseRecordVersion},
	} {
		if got := stampOf(t, tc.record.payload); got != tc.want {
			t.Errorf("%s: stamped %d, want %d", name, got, tc.want)
		}
	}
}
