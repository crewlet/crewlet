package code_test

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/code"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/objstore"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/store"
)

// publishCheck appends one record on repo's checks subject as its writer does
// — ADDITIVE, no expectation — and drains it.
func (n *node) publishCheck(repo string, op code.OpKind, payload any) (statelog.Result, error) {
	n.t.Helper()
	body, err := json.Marshal(payload)
	if err != nil {
		return statelog.Result{}, err
	}
	subject, scope := code.ChecksSubject(repo), code.Scope{code.ChecksPath(repo)}
	opID := statelog.NewOpID(time.Now(), "code."+string(op))
	res, err := n.pub.Publish(n.t.Context(), statelog.Request{
		Subject: statelog.Subject{Kind: string(subject.Kind), ID: subject.ID},
		Scope:   scope.Set(), OpID: opID, Pattern: statelog.PatternAdditive,
		Decide: func(_ *sql.Tx, stamp statelog.Stamp) (statelog.Decision, error) {
			encoded, err := code.Encode(code.MutationRecord{
				RecordEnvelope: code.RecordEnvelope{
					OpID: opID, Subject: subject, Op: op, CreatedAt: harnessAt,
					Gen: stamp.Gen, Writer: stamp.Writer, Scope: scope,
				},
				Mutation: body, Actor: "harness", ActorKind: code.ActorSystem,
			})
			return statelog.Decision{Payload: encoded}, err
		},
	})
	if err == nil && n.waiter.running() {
		n.drain()
	}
	return res, err
}

// seedEnded writes count check rows of repoID straight into a node's estate,
// each ended at the instant at and keyed by it — rows a case needs past or
// inside the history, which no record of this build can end at an instant it
// chooses.
func (n *node) seedEnded(repoID string, at time.Time, count int) {
	n.t.Helper()
	for i := range count {
		n.exec(`INSERT INTO code_checks (repo_id, commit_oid, target_ref, name, attempt, state, purpose,
			change_id, revision, candidate_id, entry_id, base_oid, defs_oid, required, run_in, timeout_seconds,
			reason, runner_node, placement, box_handle, borrowed, reporter, queued_at, started_at, finished_at,
			duration_ms, exit_code, failures_omitted, queued_position, finished_position, row_bytes, overdue_at,
			url, summary, def, failures, tail)
			VALUES (?, ?, 'refs/heads/main', ?, 1, 'passed', 'tip', '', 0, '', '', '', '', 0, '', 600, 'exit',
				'node-a', 'container', '', 0, '', ?, ?, ?, 1000, 0, 0, 1, 2, 4, ?, '', 'ok', '', '', '')`,
			repoID, oidOf(fmt.Sprint(repoID, at.UnixNano(), i)), fmt.Sprintf("check-%d", i), store.EncodeTime(at),
			store.EncodeTime(at), store.EncodeTime(at), store.EncodeTime(at.Add(600*time.Second)))
	}
}

// checksOf counts repoID's check rows.
func (n *node) checksOf(repoID string) int {
	n.t.Helper()
	return n.count(`SELECT COUNT(*) FROM code_checks WHERE repo_id = ?`, repoID)
}

// AN IDLE REPOSITORY'S CHECKS ARE SWEPT (M5-D34): a repository whose checks
// keep ending removes its own history with each ending, and one whose checks
// stopped ending has nothing of its own that ever would — so the retention
// pass lists the repositories holding a check row that ended before its clock
// less CheckHistory less CheckSweepGrace, and publishes a checks_sweep on
// each one's checks subject which removes EVERY row of it past the history,
// the ones inside the grace too: a swept repository is swept whole. A
// repository whose rows are past the history but inside the grace — its next
// ending may yet come — is not listed and keeps them, and nor is one whose
// rows are inside the history.
//
// Mutation: leave the checks step out of the pass, and the idle repository
// keeps its rows — red; sweep to the listing's cutoff rather than the
// history, and it keeps the two inside the grace — red; list at the history
// itself, and the recent repository is swept too — red.
func TestAnIdleRepositorysChecksAreSwept(t *testing.T) {
	t.Parallel()
	n := newNode(t)
	n.ensure("idle", described("idle"), t1)
	n.ensure("recent", described("recent"), t1)
	n.ensure("busy", described("busy"), t1)
	idle, recent, busy := n.mustID("idle"), n.mustID("recent"), n.mustID("busy")
	n.seedEnded(idle, harnessAt.Add(-code.CheckHistory-code.CheckSweepGrace-time.Hour), 3)
	n.seedEnded(idle, harnessAt.Add(-code.CheckHistory-time.Hour), 2)
	n.seedEnded(recent, harnessAt.Add(-code.CheckHistory-time.Hour), 1)
	n.seedEnded(busy, harnessAt.Add(-24*time.Hour), 2)
	// THE BROKER'S INSTANT IS THE PASS'S CLOCK, so the sweep's cutoff — the
	// pass's clock less the history — falls before the record's own.
	n.storedAt = func(uint64, time.Time) time.Time { return harnessAt }
	before := n.end()

	var p purged
	report, err := passOver(n.store, n.reader, &p).Pass(t.Context())
	if err != nil {
		t.Fatalf("Pass: %v", err)
	}
	if report.ChecksSweeps != 1 || report.ChecksSweptRows != 5 || len(report.Failed) != 0 || report.Remain {
		t.Fatalf("the pass reported %+v; want one checks sweep of the idle repository's five rows", report)
	}
	want := []string{fmt.Sprintf("%s %s", code.OpChecksSweep, code.ChecksSubject(idle))}
	if got := n.opsAfter(before); !slices.Equal(got, want) {
		t.Fatalf("the pass appended %v, want %v", got, want)
	}
	if left := n.checksOf(idle); left != 0 {
		t.Fatalf("the idle repository keeps %d check rows past the history", left)
	}
	if kept := n.checksOf(recent); kept != 1 {
		t.Fatalf("the recent repository keeps %d of its 1 row inside the grace", kept)
	}
	if kept := n.checksOf(busy); kept != 2 {
		t.Fatalf("the busy repository keeps %d of its 2 rows inside the history", kept)
	}
	again, err := passOver(n.store, n.reader, &p).Pass(t.Context())
	if err != nil || again.ChecksSweeps != 0 {
		t.Fatalf("a second pass swept again (%+v, %v): nothing is past the history by the grace now", again, err)
	}
}

// AN ACTIVE REPOSITORY'S CHECKS ARE LEFT TO ITS ENDINGS: a repository whose
// checks keep ending removes its history with each ending, so a row that
// crossed the history since its last ending — here half a minute before the
// pass, its last ending a minute before — is its next ending's to remove. The
// pass appends nothing for it: a sweep would be a record that ending makes
// anyway, applied by every data node, ahead of the purges on the pass's one
// budget — at the code host's design point, thousands of them a pass. Its
// next ending removes the row.
//
// Mutation: list the repositories holding rows past the history itself, with
// no grace, and the pass sweeps the active repository — red.
func TestAnActiveRepositorysChecksAreLeftToItsEndings(t *testing.T) {
	t.Parallel()
	n := newNode(t)
	n.ensure("active", described("active"), t1)
	repo := n.mustID("active")
	crossed := harnessAt.Add(-code.CheckHistory - 30*time.Second)
	n.seedEnded(repo, crossed, 1)
	// end publishes one attempt of a check, queued and ended at the broker
	// instant at, as its runner's records are.
	end := func(name string, at time.Time) {
		t.Helper()
		n.storedAt = func(uint64, time.Time) time.Time { return at }
		key := code.CheckKey{RepoID: repo, OID: oidOf(name), Target: mainRef, Name: name, Attempt: 1}
		exit := 0
		if _, err := n.publishCheck(repo, code.OpCheckQueued, code.CheckQueued{CheckKey: key,
			Purpose: code.PurposeTip, Reason: code.QueueDispatch,
			Def: &code.CheckDef{Name: name, Run: "make test", TimeoutSeconds: 600}, QueuedAt: at}); err != nil {
			t.Fatalf("queue %s: %v", name, err)
		}
		if _, err := n.publishCheck(repo, code.OpCheckFinished, code.CheckFinished{CheckKey: key,
			State: code.CheckStatePassed, Reason: code.CheckReasonExit, ExitCode: &exit,
			FinishedAt: at}); err != nil {
			t.Fatalf("end %s: %v", name, err)
		}
	}
	end("unit", harnessAt.Add(-time.Minute))
	if held := n.checksOf(repo); held != 2 {
		t.Fatalf("the premise: the repository holds %d rows, want the one that crossed the history after its "+
			"last ending's cutoff, and that ending's own", held)
	}
	n.storedAt = func(uint64, time.Time) time.Time { return harnessAt }
	before := n.end()
	var p purged
	report, err := passOver(n.store, n.reader, &p).Pass(t.Context())
	if err != nil {
		t.Fatalf("Pass: %v", err)
	}
	if report.ChecksSweeps != 0 || len(report.Failed) != 0 {
		t.Fatalf("the pass reported %+v; want no checks sweep of a repository whose checks keep ending", report)
	}
	if got := n.opsAfter(before); len(got) != 0 {
		t.Fatalf("the pass appended %v, want nothing: the repository's next ending removes its row", got)
	}
	end("lint", harnessAt.Add(time.Minute))
	if left := n.count(`SELECT COUNT(*) FROM code_checks WHERE repo_id = ? AND finished_at = ?`, repo,
		store.EncodeTime(crossed)); left != 0 || n.checksOf(repo) != 2 {
		t.Fatalf("the repository's next ending left the row that crossed the history (%d of %d rows)", left,
			n.checksOf(repo))
	}
}

// A PURGED REPOSITORY'S CHECK WRITES NOTHING: a repository's purge removes
// its checks and their logs with the rest of it, and a check record about it
// that lands after the purge — a runner that ended a check its repository's
// purge outran — is dropped by the deletion gate on every node, which reads
// the repository off the checks subject itself, record body or none.
//
// Mutation: leave the checks subject out of the deletion gate (gatedRepo) and
// the late ending writes a row for a purged repository — red.
func TestAPurgedRepositorysCheckWritesNothing(t *testing.T) {
	t.Parallel()
	n := newNode(t)
	n.ensure("web", described("web"), t1)
	repo := n.mustID("web")
	key := code.CheckKey{RepoID: repo, OID: oidOf("head"), Target: mainRef, Name: "unit", Attempt: 1}
	def := &code.CheckDef{Name: "unit", Run: "make test", TimeoutSeconds: 600}
	if _, err := n.publishCheck(repo, code.OpCheckQueued, code.CheckQueued{CheckKey: key,
		Purpose: code.PurposeTip, Reason: code.QueueDispatch, Def: def,
		QueuedAt: harnessAt}); err != nil {
		t.Fatalf("queue a check: %v", err)
	}
	exit := 1
	failed := code.CheckFinished{CheckKey: key, State: code.CheckStateFailed, Reason: code.CheckReasonExit,
		ExitCode: &exit, Tail: "FAIL", FinishedAt: harnessAt,
		Log: &code.CheckLog{Object: keyOf("log"), Hash: objstore.HashOf([]byte("log")), Size: 3}}
	if err := failed.Validate(); err != nil {
		t.Fatalf("the premise: the ending is not one a writer forms: %v", err)
	}
	if _, err := n.publishCheck(repo, code.OpCheckFinished, failed); err != nil {
		t.Fatalf("end the check: %v", err)
	}
	if n.count(`SELECT COUNT(*) FROM code_checks WHERE repo_id = ?`, repo) != 1 ||
		n.count(`SELECT COUNT(*) FROM code_check_logs WHERE repo_id = ?`, repo) != 1 {
		t.Fatal("the premise: the repository holds its check and its log")
	}
	n.deleteAt(repo, harnessAt.Add(-2*keep))
	for {
		res, err := n.store.PurgeStep(t.Context(), repo, keep)
		if err != nil {
			t.Fatalf("PurgeStep: %v", err)
		}
		if res.Done {
			break
		}
	}
	if got := n.count(`SELECT COUNT(*) FROM code_checks WHERE repo_id = ?`, repo) +
		n.count(`SELECT COUNT(*) FROM code_check_logs WHERE repo_id = ?`, repo); got != 0 {
		t.Fatalf("the purge left %d rows of the repository's checks", got)
	}

	late := failed
	late.Attempt = 2
	_, err := n.publishCheck(repo, code.OpCheckFinished, late)
	var refusal *statelog.Unavailable
	if !errors.As(err, &refusal) || refusal.Reason != statelog.ReasonDeleted {
		t.Fatalf("an ending of a purged repository's check published %v, want it dropped by the deletion gate",
			err)
	}
	if got := n.count(`SELECT COUNT(*) FROM code_checks WHERE repo_id = ?`, repo); got != 0 {
		t.Fatalf("an ending of a purged repository's check wrote %d rows", got)
	}
	if got := n.count(`SELECT rejects FROM code_deletions WHERE repo_id = ?`, repo); got != 1 {
		t.Fatalf("the deletion marker counts %d records dropped, want the one ending", got)
	}
	gates := code.NewGates(n.db.Reader())
	subject := statelog.Subject{Kind: string(code.KindChecks), ID: repo}
	at := statelog.Position{Stream: code.Domain{}.Stream().Name, Seq: n.end() + 1}
	if reason, gated, err := gates.GatedAt(t.Context(), subject, "", "op-late", at, nil); err != nil || !gated ||
		reason != statelog.ReasonDeleted {
		t.Fatalf("the gate reader judged a record on the purged repository's checks %q, %v, %v; want deleted — "+
			"its subject names the repository", reason, gated, err)
	}
}

// A CHECK'S LOG IS NAMED BY THE ENDING THAT KEEPS ONE (ADR-0075's contract for
// a Required table): an ending carrying a log names the attempt's
// code_check_logs row and its object, spelled as the walk spells the row; an
// ending carrying none names NOTHING — never a removal, since the join may keep
// another ending's log, and a removal read where the row stands would lose
// that log to a backup; every other record names none; and a record this build
// cannot read that may reach a repository's checks refuses, while one whose
// writer scoped it to a repository's own rows does not.
//
// Mutation: answer an ending with no log as a removal of the row, and the
// ending-with-no-log case goes red; read a newer record scoped to checks as
// naming none, and the unreadable case does.
func TestACheckLogIsNamedByTheEndingThatKeepsOne(t *testing.T) {
	t.Parallel()
	records := code.CheckLogReferences.Records
	const repo = "0190f3c4-0000-7000-8000-0000000000c1"
	key := code.CheckKey{RepoID: repo, OID: oidOf("head"), Target: mainRef, Name: "unit", Attempt: 2}
	log := &code.CheckLog{Object: keyOf("log"), Hash: objstore.HashOf([]byte("log")), Size: 3}
	exit := 1
	ending := func(l *code.CheckLog) []byte {
		body, err := json.Marshal(code.CheckFinished{CheckKey: key, State: code.CheckStateFailed,
			Reason: code.CheckReasonExit, ExitCode: &exit, FinishedAt: harnessAt, Log: l})
		if err != nil {
			t.Fatal(err)
		}
		return mustEncode(t, code.MutationRecord{RecordEnvelope: code.RecordEnvelope{
			OpID: "op-end", Subject: code.ChecksSubject(repo), Op: code.OpCheckFinished, Gen: 1,
			Scope: code.Scope{code.ChecksPath(repo)},
		}, Mutation: body})
	}
	got, err := records(ending(log))
	want := []objstore.RecordReference{{NamedBy: objstore.JoinOwner(repo, key.OID, key.Target, "unit", "2"),
		Object: log.Content(), Names: true}}
	if err != nil || !slices.Equal(got, want) {
		t.Fatalf("an ending keeping a log names %+v, %v; want its attempt's row naming the log: %+v", got, err, want)
	}
	if got, err := records(ending(nil)); err != nil || len(got) != 0 {
		t.Fatalf("an ending keeping no log names %+v, %v; want nothing — never a removal", got, err)
	}
	for _, kind := range []code.ObjectKind{code.KindName, code.KindRepo, code.KindChecks} {
		body, err := encodeSuiteRecord(string(kind), repo, "op-1", code.RecordVersion)
		if err != nil {
			t.Fatal(err)
		}
		if got, err := records(body); err != nil || len(got) != 0 {
			t.Errorf("a %s record's log references = %+v, %v; want none", kind, got, err)
		}
	}
	future, err := encodeSuiteRecord(string(code.KindChecks), repo, "op-2", code.RecordVersion+1)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := records(future); !errors.Is(err, objstore.ErrRecordUnreadable) {
		t.Fatalf("a newer check record = %v, want ErrRecordUnreadable", err)
	}
	pushOnly := mustEncode(t, code.MutationRecord{RecordEnvelope: code.RecordEnvelope{
		V: code.RecordVersion + 1, OpID: "op-3", Subject: code.RepoSubject(repo), Op: code.OpPush, Gen: 1,
		Scope: code.Scope{code.RepoPath(repo)},
	}, Mutation: json.RawMessage(`{"novel":1}`)})
	if got, err := records(pushOnly); err != nil || len(got) != 0 {
		t.Fatalf("a newer record scoped to a repository's own rows = %+v, %v; want none", got, err)
	}
	// AND THE PACKS' READER, the other way round: a newer record scoped to a
	// repository's checks alone writes no pack row.
	if got, err := code.PackReferences.Records(future); err != nil || len(got) != 0 {
		t.Fatalf("the packs' reader answered a newer check record %+v, %v; want none", got, err)
	}
}

// THE LARGEST CHECK RECORDS FIT A RECORD: a queued attempt carrying the
// widest definition a file can hold — every list at its bound, every text at
// its bytes in the character JSON spells in six — and an ending carrying every
// field at its bound each validate, and encode with the longest node id and
// widest generation under MaxRecordBytes, so no check a writer forms is one
// the committer's log refuses; and one record's apply — its row, its log
// dropped and written, and its horizon's check rows with theirs — stays inside
// MaxRecordRows.
func TestTheLargestCheckRecordsFitARecord(t *testing.T) {
	t.Parallel()
	const repo = "0190f3c4-0000-7000-8000-00000000b0b0"
	wide := func(n int) string { return strings.Repeat("&", n) }
	glob := func(i int) string {
		head := fmt.Sprintf("%02d/", i)
		return head + wide(128-len(head))
	}
	// A REPORT'S PATH AT ITS BOUND: sixteen components of the 255 bytes a
	// file name takes, under code.MaxTreePathBytes whole.
	report := func(lead string) string { return lead + strings.Repeat("/"+wide(254), 15) + "/x" }
	def := code.CheckDef{Name: strings.Repeat("c", code.MaxCheckNameBytes), Run: wide(code.MaxCheckRunBytes),
		TimeoutSeconds: int(code.CheckTimeoutCeiling / time.Second),
		Report:         code.CheckReport{JUnit: report(wide(254)), SARIF: report("s" + wide(253))},
		RunIn:          code.CellE2B}
	for i := range code.MaxCheckPaths {
		def.Paths = append(def.Paths, glob(i))
	}
	for i := range 64 {
		def.Checkout.Cone = append(def.Checkout.Cone, fmt.Sprintf("%02d", i)+wide(253))
	}
	for i := range code.MaxCheckEnv {
		def.Env = append(def.Env, fmt.Sprintf("V%02d_", i)+strings.Repeat("A", code.MaxCheckEnvNameBytes-4))
	}
	// THE LONGEST TARGET a ref may be, in the key every record of the
	// attempt carries.
	key := code.CheckKey{RepoID: repo, OID: oidOf("head"),
		Target: "refs/heads/" + strings.Repeat("b", code.MaxRefNameBytes-len("refs/heads/")), Name: def.Name,
		Attempt: code.MaxCheckAttempts}
	person := &code.Principal{Kind: code.PrincipalOperator, OperatorID: strings.Repeat("o", 64),
		Seat: strings.Repeat("s", 64)}
	queued := code.CheckQueued{CheckKey: key, Purpose: code.PurposeHead,
		ChangeID: "0190f3c4-0000-7000-8000-00000000c0c0", Revision: 1 << 30,
		CandidateID: wide(code.MaxProvenanceBytes), EntryID: wide(code.MaxProvenanceBytes),
		Base: oidOf("base"), DefsOID: oidOf("defs"), Def: &def, Required: true, Reason: code.QueueRerun,
		By: person, QueuedAt: harnessAt}
	exit := 1
	var failures []code.CheckFailure
	for i := 0; ; i++ {
		f := code.CheckFailure{Path: "p", Line: i, TestID: "t", Message: wide(100)}
		encoded, _ := json.Marshal(append(failures, f))
		if len(failures) == code.MaxCheckFailures || len(encoded) > code.MaxCheckFailureBytes {
			break
		}
		failures = append(failures, f)
	}
	finished := code.CheckFinished{CheckKey: key, State: code.CheckStateFailed, Reason: code.CheckReasonReported,
		ExitCode: &exit, DurationMs: 1 << 40, Summary: wide(code.CheckSummaryBytes),
		Tail: wide(code.CheckTailBytes), Failures: failures, FailuresOmitted: 1 << 30,
		Log:      &code.CheckLog{Object: keyOf("log"), Hash: objstore.HashOf([]byte("log")), Size: code.MaxCheckLogBytes},
		Reporter: person, URL: "https://ci.example.com/" + wide(code.MaxCheckURLBytes-len("https://ci.example.com/")),
		FinishedAt: harnessAt}
	nodeID := strings.Repeat("n", 64)
	for _, tc := range []struct {
		op      code.OpKind
		payload interface{ Validate() error }
	}{{code.OpCheckQueued, queued}, {code.OpCheckFinished, finished}} {
		if err := tc.payload.Validate(); err != nil {
			t.Fatalf("the widest %s is not one a writer forms: %v", tc.op, err)
		}
		body, err := json.Marshal(tc.payload)
		if err != nil {
			t.Fatal(err)
		}
		record, err := code.Encode(code.MutationRecord{
			RecordEnvelope: code.RecordEnvelope{
				OpID: statelog.NewOpID(harnessAt, "code.checks"), Subject: code.ChecksSubject(repo), Op: tc.op,
				CreatedAt: harnessAt, Gen: 1<<32 - 1, Writer: nodeID, Scope: code.Scope{code.ChecksPath(repo)},
			},
			Mutation: body, Actor: nodeID, ActorKind: code.ActorSystem,
		})
		if err != nil {
			t.Fatal(err)
		}
		if len(record) > code.MaxRecordBytes {
			t.Errorf("the widest %s record is %d bytes, past MaxRecordBytes (%d)", tc.op, len(record),
				code.MaxRecordBytes)
		}
		t.Logf("the widest %s record is %d bytes of %d", tc.op, len(record), code.MaxRecordBytes)
	}
	// ITS ROW, ITS LOG'S TWO, AND ITS HORIZON'S ROWS EACH WITH ITS LOG.
	if rows := 1 + 2 + 2*code.CheckHorizonRows; rows > code.MaxRecordRows {
		t.Errorf("one check record's apply touches up to %d rows, past MaxRecordRows (%d)", rows,
			code.MaxRecordRows)
	}
}

// A CHECK AT THE COMPANY'S DEFAULT TIMEOUT FITS THE SHORTEST REMOTE SESSION: an
// unset code.checks.max_timeout_seconds and the time a check's box is given
// past its timeout for its setup (CheckBoxGrace) sum to the hour a box on
// E2B's smallest plan lives — the one figure config's default is derived from,
// which config cannot read here, since the grace is the sandbox's.
//
// Mutation: raise config.CodeDefaultCheckMaxTimeout to an hour — red.
func TestTheDefaultCheckTimeoutFitsTheShortestRemoteSession(t *testing.T) {
	t.Parallel()
	const shortestSession = time.Hour
	if got := config.CodeDefaultCheckMaxTimeout + code.CheckBoxGrace; got != shortestSession {
		t.Errorf("a check at the default timeout holds its box %s with its setup, want %s: the hour a box "+
			"on E2B's smallest plan lives", got, shortestSession)
	}
}
