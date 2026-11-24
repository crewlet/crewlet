package gitlet_test

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/gitlet"
	"github.com/crewlet/crewlet/internal/gitref"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/objstore"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/store"
)

// publishCheck appends one record on repo's checks subject as its writer does
// — ADDITIVE, no expectation — and drains it.
func (n *node) publishCheck(repo string, op gitlet.OpKind, payload any) (statelog.Result, error) {
	n.t.Helper()
	body, err := json.Marshal(payload)
	if err != nil {
		return statelog.Result{}, err
	}
	subject, scope := gitlet.ChecksSubject(repo), gitlet.Scope{gitlet.ChecksPath(repo)}
	opID := statelog.NewOpID(time.Now(), "code_"+string(op))
	res, err := n.pub.Publish(n.t.Context(), statelog.Request{
		Subject: statelog.Subject{Kind: string(subject.Kind), ID: subject.ID},
		Scope:   scope.Set(), OpID: opID, Pattern: statelog.PatternAdditive,
		Decide: func(_ *sql.Tx, stamp statelog.Stamp) (statelog.Decision, error) {
			encoded, err := gitlet.Encode(gitlet.MutationRecord{
				RecordEnvelope: gitlet.RecordEnvelope{
					OpID: opID, Subject: subject, Op: op, CreatedAt: harnessAt,
					Gen: stamp.Gen, Writer: stamp.Writer, Scope: scope,
				},
				Mutation: body, Actor: "harness", ActorKind: gitlet.ActorSystem,
			})
			return statelog.Decision{Payload: encoded}, err
		},
	})
	if err == nil && n.waiter.running() {
		n.drain()
	}
	return res, err
}

// seedEnded writes count check rows of repoID, each ended at the instant at
// and keyed by it, straight into a node's estate: no record of this build ends
// a row at an instant a case chooses.
func (n *node) seedEnded(repoID string, at time.Time, count int) {
	n.t.Helper()
	for i := range count {
		n.exec(`INSERT INTO code_checks (repo_id, commit_oid, target_ref, name, attempt, state, purpose,
			change_id, revision, candidate_id, entry_id, base_oid, defs_oid, required, timeout_seconds,
			reason, runner_node, placement, box_handle, reporter, queued_at, started_at, finished_at,
			duration_ms, exit_code, failures_omitted, queued_position, finished_position, row_bytes, overdue_at,
			url, summary, def, failures, tail)
			VALUES (?, ?, 'refs/heads/main', ?, 1, 'passed', 'tip', '', 0, '', '', '', '', 0, 600, 'exit',
				'node-a', 'container', '', '', ?, ?, ?, 1000, 0, 0, 1, 2, 4, ?, '', 'ok', '', '', '')`,
			repoID, oidOf(fmt.Sprint(repoID, at.UnixNano(), i)), fmt.Sprintf("check-%d", i), store.EncodeTime(at),
			store.EncodeTime(at), store.EncodeTime(at), store.EncodeTime(at.Add(600*time.Second)))
	}
}

// checksOf counts repoID's check rows.
func (n *node) checksOf(repoID string) int {
	n.t.Helper()
	return n.count(`SELECT COUNT(*) FROM code_checks WHERE repo_id = ?`, repoID)
}

// AN IDLE REPOSITORY'S CHECKS ARE SWEPT (M5-D34): one whose checks stopped
// ending has no ending left to remove its history, so the retention pass lists
// each repository holding a row ended before its clock less CheckHistory less
// CheckSweepGrace and publishes a checks_sweep removing EVERY row of it past
// the history, those inside the grace too. A repository whose rows are past
// the history but inside the grace (its next ending may yet come), or inside
// the history, is not listed and keeps them.
//
// Mutation: leave the checks step out of the pass (the idle repository keeps
// its rows), sweep to the listing's cutoff rather than the history (it keeps
// the two inside the grace), or list at the history itself (the recent
// repository is swept) — each red.
func TestAnIdleRepositorysChecksAreSwept(t *testing.T) {
	t.Parallel()
	n := newNode(t)
	n.ensure("idle", described("idle"), t1)
	n.ensure("recent", described("recent"), t1)
	n.ensure("busy", described("busy"), t1)
	idle, recent, busy := n.mustID("idle"), n.mustID("recent"), n.mustID("busy")
	n.seedEnded(idle, harnessAt.Add(-gitlet.CheckHistory-gitlet.CheckSweepGrace-time.Hour), 3)
	n.seedEnded(idle, harnessAt.Add(-gitlet.CheckHistory-time.Hour), 2)
	n.seedEnded(recent, harnessAt.Add(-gitlet.CheckHistory-time.Hour), 1)
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
	want := []string{fmt.Sprintf("%s %s", gitlet.OpChecksSweep, gitlet.ChecksSubject(idle))}
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

// AN ACTIVE REPOSITORY'S CHECKS ARE LEFT TO ITS ENDINGS: each ending removes
// its repository's history, so a row that crossed the history since the last
// ending (here half a minute before the pass, the ending a minute before) is
// the next ending's to remove, and the pass appends nothing: a sweep would
// repeat that ending's work on every data node, ahead of the purges on the
// pass's one budget, thousands a pass at Gitlet's design point.
//
// Mutation: list the repositories holding rows past the history with no
// grace, and the pass sweeps the active repository — red.
func TestAnActiveRepositorysChecksAreLeftToItsEndings(t *testing.T) {
	t.Parallel()
	n := newNode(t)
	n.ensure("active", described("active"), t1)
	repo := n.mustID("active")
	crossed := harnessAt.Add(-gitlet.CheckHistory - 30*time.Second)
	n.seedEnded(repo, crossed, 1)
	// end publishes one attempt of a check, queued and ended at the broker
	// instant at, as its runner's records are.
	end := func(name string, at time.Time) {
		t.Helper()
		n.storedAt = func(uint64, time.Time) time.Time { return at }
		key := gitlet.CheckKey{RepoID: repo, OID: oidOf(name), Target: mainRef, Name: name, Attempt: 1}
		exit := 0
		if _, err := n.publishCheck(repo, gitlet.OpCheckQueued, gitlet.CheckQueued{CheckKey: key,
			Purpose: gitlet.PurposeTip, Reason: gitlet.QueueDispatch,
			Def: &gitlet.CheckDef{Name: name, Run: "make test", TimeoutSeconds: 600}, QueuedAt: at}); err != nil {
			t.Fatalf("queue %s: %v", name, err)
		}
		if _, err := n.publishCheck(repo, gitlet.OpCheckFinished, gitlet.CheckFinished{CheckKey: key,
			State: gitlet.CheckStatePassed, Reason: gitlet.CheckReasonExit, ExitCode: &exit,
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

// A PURGED REPOSITORY'S CHECK WRITES NOTHING: the purge removes its checks
// and their logs, and a check record landing after it (a runner's ending the
// purge outran) is dropped by the deletion gate on every node, which reads the
// repository off the checks subject itself, record body or none.
//
// Mutation: leave the checks subject out of the deletion gate (gatedRepo) and
// the late ending writes a row for a purged repository — red.
func TestAPurgedRepositorysCheckWritesNothing(t *testing.T) {
	t.Parallel()
	n := newNode(t)
	n.ensure("web", described("web"), t1)
	repo := n.mustID("web")
	key := gitlet.CheckKey{RepoID: repo, OID: oidOf("head"), Target: mainRef, Name: "unit", Attempt: 1}
	def := &gitlet.CheckDef{Name: "unit", Run: "make test", TimeoutSeconds: 600}
	if _, err := n.publishCheck(repo, gitlet.OpCheckQueued, gitlet.CheckQueued{CheckKey: key,
		Purpose: gitlet.PurposeTip, Reason: gitlet.QueueDispatch, Def: def,
		QueuedAt: harnessAt}); err != nil {
		t.Fatalf("queue a check: %v", err)
	}
	exit := 1
	failed := gitlet.CheckFinished{CheckKey: key, State: gitlet.CheckStateFailed, Reason: gitlet.CheckReasonExit,
		ExitCode: &exit, Tail: "FAIL", FinishedAt: harnessAt,
		Log: &gitlet.CheckLog{Object: keyOf("log"), Hash: objstore.HashOf([]byte("log")), Size: 3}}
	if err := failed.Validate(); err != nil {
		t.Fatalf("the premise: the ending is not one a writer forms: %v", err)
	}
	if _, err := n.publishCheck(repo, gitlet.OpCheckFinished, failed); err != nil {
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
	_, err := n.publishCheck(repo, gitlet.OpCheckFinished, late)
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
	gates := gitlet.NewGates(n.db.Reader())
	subject := statelog.Subject{Kind: string(gitlet.KindChecks), ID: repo}
	at := statelog.Position{Stream: gitlet.Domain{}.Stream().Name, Seq: n.end() + 1}
	if reason, gated, err := gates.GatedAt(t.Context(), subject, "", "op-late", at, nil); err != nil || !gated ||
		reason != statelog.ReasonDeleted {
		t.Fatalf("the gate reader judged a record on the purged repository's checks %q, %v, %v; want deleted — "+
			"its subject names the repository", reason, gated, err)
	}
}

// A CHECK'S LOG IS NAMED BY THE ENDING THAT KEEPS ONE (ADR-0082's contract for
// a Required table): an ending carrying a log names the attempt's
// code_check_logs row and its object, spelled as the walk spells the row; one
// carrying none names NOTHING, never a removal, since the join may keep
// another ending's log, which a removal would lose to a backup; every other
// record names none; and an unreadable record that may reach a repository's
// checks refuses, while one scoped to a repository's own rows does not.
//
// Mutation: answer an ending with no log as a removal of the row, and the
// ending-with-no-log case goes red; read a newer record scoped to checks as
// naming none, and the unreadable case does.
func TestACheckLogIsNamedByTheEndingThatKeepsOne(t *testing.T) {
	t.Parallel()
	records := gitlet.CheckLogReferences.Records
	const repo = "0190f3c4-0000-7000-8000-0000000000c1"
	key := gitlet.CheckKey{RepoID: repo, OID: oidOf("head"), Target: mainRef, Name: "unit", Attempt: 2}
	log := &gitlet.CheckLog{Object: keyOf("log"), Hash: objstore.HashOf([]byte("log")), Size: 3}
	exit := 1
	ending := func(l *gitlet.CheckLog) []byte {
		body, err := json.Marshal(gitlet.CheckFinished{CheckKey: key, State: gitlet.CheckStateFailed,
			Reason: gitlet.CheckReasonExit, ExitCode: &exit, FinishedAt: harnessAt, Log: l})
		if err != nil {
			t.Fatal(err)
		}
		return mustEncode(t, gitlet.MutationRecord{RecordEnvelope: gitlet.RecordEnvelope{
			OpID: "op-end", Subject: gitlet.ChecksSubject(repo), Op: gitlet.OpCheckFinished, Gen: 1,
			Scope: gitlet.Scope{gitlet.ChecksPath(repo)},
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
	for _, kind := range []gitlet.ObjectKind{gitlet.KindName, gitlet.KindRepo, gitlet.KindChecks} {
		body, err := encodeSuiteRecord(string(kind), repo, "op-1", gitlet.RecordVersion)
		if err != nil {
			t.Fatal(err)
		}
		if got, err := records(body); err != nil || len(got) != 0 {
			t.Errorf("a %s record's log references = %+v, %v; want none", kind, got, err)
		}
	}
	future, err := encodeSuiteRecord(string(gitlet.KindChecks), repo, "op-2", gitlet.RecordVersion+1)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := records(future); !errors.Is(err, objstore.ErrRecordUnreadable) {
		t.Fatalf("a newer check record = %v, want ErrRecordUnreadable", err)
	}
	pushOnly := mustEncode(t, gitlet.MutationRecord{RecordEnvelope: gitlet.RecordEnvelope{
		V: gitlet.RecordVersion + 1, OpID: "op-3", Subject: gitlet.RepoSubject(repo), Op: gitlet.OpPush, Gen: 1,
		Scope: gitlet.Scope{gitlet.RepoPath(repo)},
	}, Mutation: json.RawMessage(`{"novel":1}`)})
	if got, err := records(pushOnly); err != nil || len(got) != 0 {
		t.Fatalf("a newer record scoped to a repository's own rows = %+v, %v; want none", got, err)
	}
	// AND THE PACKS' READER, the other way round: a newer record scoped to a
	// repository's checks alone writes no pack row.
	if got, err := gitlet.PackReferences.Records(future); err != nil || len(got) != 0 {
		t.Fatalf("the packs' reader answered a newer check record %+v, %v; want none", got, err)
	}
}

// THE LARGEST CHECK RECORDS FIT A RECORD: the widest queued attempt (every
// list at its bound, every text in the character JSON spells in six) and the
// widest ending validate and, with the longest node id and widest generation,
// encode under MaxRecordBytes, so the committer's log refuses no check a
// writer forms: the ending too with a failed head's routing at its roles'
// bounds, and with all of MaxRoutingBytes beside it. One record's apply (its
// row, its log dropped and written, its horizon's rows with theirs, its
// routing's notices) stays inside MaxRecordRows.
//
// Mutation: raise MaxRoutingBytes past what the widest ending leaves, or
// charge a routing more notice rows than a record leaves (routingNoticeRows)
// — each red.
func TestTheLargestCheckRecordsFitARecord(t *testing.T) {
	t.Parallel()
	const repo = "0190f3c4-0000-7000-8000-00000000b0b0"
	wide := func(n int) string { return strings.Repeat("&", n) }
	glob := func(i int) string {
		head := fmt.Sprintf("%02d/", i)
		return head + wide(128-len(head))
	}
	// A REPORT'S PATH AT ITS BOUND: sixteen components of the 255 bytes a
	// file name takes, under gitlet.MaxTreePathBytes whole.
	report := func(lead string) string { return lead + strings.Repeat("/"+wide(254), 15) + "/x" }
	def := gitlet.CheckDef{Name: strings.Repeat("c", gitlet.MaxCheckNameBytes), Run: wide(gitlet.MaxCheckRunBytes),
		TimeoutSeconds: int(gitlet.CheckTimeoutCeiling / time.Second),
		Report:         gitlet.CheckReport{JUnit: report(wide(254)), SARIF: report("s" + wide(253))}}
	for i := range gitlet.MaxCheckPaths {
		def.Paths = append(def.Paths, glob(i))
	}
	for i := range 64 {
		def.Checkout.Cone = append(def.Checkout.Cone, fmt.Sprintf("%02d", i)+wide(253))
	}
	for i := range gitlet.MaxCheckEnv {
		def.Env = append(def.Env, fmt.Sprintf("V%02d_", i)+strings.Repeat("A", gitlet.MaxCheckEnvNameBytes-4))
	}
	// THE LONGEST TARGET a ref may be, in the key every record of the
	// attempt carries.
	key := gitlet.CheckKey{RepoID: repo, OID: oidOf("head"),
		Target: "refs/heads/" + strings.Repeat("b", gitlet.MaxRefNameBytes-len("refs/heads/")), Name: def.Name,
		Attempt: gitlet.MaxCheckAttempts}
	person := &gitlet.Principal{Kind: gitlet.PrincipalOperator, OperatorID: strings.Repeat("o", 64),
		Seat: strings.Repeat("s", 64)}
	queued := gitlet.CheckQueued{CheckKey: key, Purpose: gitlet.PurposeHead,
		ChangeID: "0190f3c4-0000-7000-8000-00000000c0c0", Revision: 1 << 30,
		CandidateID: wide(gitlet.MaxProvenanceBytes), EntryID: wide(gitlet.MaxProvenanceBytes),
		Base: oidOf("base"), DefsOID: oidOf("defs"), Def: &def, Required: true, Reason: gitlet.QueueRerun,
		By: person, QueuedAt: harnessAt}
	exit := 1
	var failures []gitlet.CheckFailure
	for i := 0; ; i++ {
		f := gitlet.CheckFailure{Path: "p", Line: i, TestID: "t", Message: wide(100)}
		encoded, _ := json.Marshal(append(failures, f))
		if len(failures) == gitlet.MaxCheckFailures || len(encoded) > gitlet.MaxCheckFailureBytes {
			break
		}
		failures = append(failures, f)
	}
	finished := gitlet.CheckFinished{CheckKey: key, State: gitlet.CheckStateFailed, Reason: gitlet.CheckReasonReported,
		ExitCode: &exit, DurationMs: 1 << 40, Summary: wide(gitlet.CheckSummaryBytes),
		Tail: wide(gitlet.CheckTailBytes), Failures: failures, FailuresOmitted: 1 << 30,
		Tests:    &gitlet.CheckTests{Total: math.MaxInt, Failed: 1 << 30, Skipped: 1 << 30},
		LogLines: math.MaxInt, TailCut: true,
		Log:      &gitlet.CheckLog{Object: keyOf("log"), Hash: objstore.HashOf([]byte("log")), Size: gitlet.MaxCheckLogBytes},
		Reporter: person, URL: "https://ci.example.com/" + wide(gitlet.MaxCheckURLBytes-len("https://ci.example.com/")),
		FinishedAt: harnessAt}
	// WHOM A FAILED HEAD CONCERNS, as routeFailedHead stamps it: the authors at
	// MaxRoutedParticipants and the pusher, each at a login's whole width, the
	// title in the character JSON spells in six, and a work item, whose fields
	// only the routing's room bounds.
	handle := func(i int) string { return fmt.Sprintf("%02d", i) + strings.Repeat("h", iam.MaxLogin-2) }
	routing := &gitlet.ChangeRouting{ChangeID: queued.ChangeID,
		ChangeRef: gitlet.ChangeRef(strings.Repeat("r", gitref.MaxRepoNameBytes), 1<<40),
		RepoName:  strings.Repeat("r", gitref.MaxRepoNameBytes), Title: wide(gitlet.MaxChangeTitleBytes),
		Actor: handle(gitlet.MaxRoutedParticipants), Owner: handle(0), OwnerKind: gitlet.PartyPerson,
		WorkItem:     &types.WorkItem{Backend: types.WorkJira, ID: wide(256), Key: wide(256), Project: wide(256)},
		ChecksFailed: true}
	for i := range gitlet.MaxRoutedParticipants {
		routing.Authors = append(routing.Authors, handle(i))
	}
	routing.Persons = slices.Sorted(slices.Values(append(slices.Clone(routing.Authors), routing.Actor)))
	if encoded, err := json.Marshal(routing); err != nil || len(encoded) > gitlet.MaxRoutingBytes {
		t.Fatalf("the widest failed head's routing is %d bytes (%v), past MaxRoutingBytes (%d): no decide "+
			"forms it", len(encoded), err, gitlet.MaxRoutingBytes)
	}
	heard := 0
	for _, c := range gitlet.Concerns([]*gitlet.ChangeRouting{routing}) {
		if c.Hears() {
			heard++
		}
	}
	if heard > gitlet.RoutingNoticeRows {
		t.Fatalf("the widest failed head's routing tells %d, past the %d notice rows a routing is charged",
			heard, gitlet.RoutingNoticeRows)
	}
	routed := finished
	routed.Routing = routing

	nodeID := strings.Repeat("n", 64)
	widest := map[string]int{}
	for _, tc := range []struct {
		what    string
		op      gitlet.OpKind
		payload interface{ Validate() error }
		stamped any
	}{{"queued attempt", gitlet.OpCheckQueued, queued, queued}, {"ending", gitlet.OpCheckFinished, finished, finished},
		{"failed head's ending", gitlet.OpCheckFinished, finished, routed}} {
		if err := tc.payload.Validate(); err != nil {
			t.Fatalf("the widest %s is not one a writer forms: %v", tc.what, err)
		}
		body, err := json.Marshal(tc.stamped)
		if err != nil {
			t.Fatal(err)
		}
		record, err := gitlet.Encode(gitlet.MutationRecord{
			RecordEnvelope: gitlet.RecordEnvelope{
				OpID: statelog.NewOpID(harnessAt, "code_checks"), Subject: gitlet.ChecksSubject(repo), Op: tc.op,
				CreatedAt: harnessAt, Gen: 1<<32 - 1, Writer: nodeID, Scope: gitlet.Scope{gitlet.ChecksPath(repo)},
			},
			Mutation: body, Actor: nodeID, ActorKind: gitlet.ActorSystem,
		})
		if err != nil {
			t.Fatal(err)
		}
		if len(record) > gitlet.MaxRecordBytes {
			t.Errorf("the widest %s record is %d bytes, past MaxRecordBytes (%d)", tc.what, len(record),
				gitlet.MaxRecordBytes)
		}
		widest[tc.what] = len(record)
		t.Logf("the widest %s record is %d bytes of %d", tc.what, len(record), gitlet.MaxRecordBytes)
	}
	// ANY ROUTING A DECIDE FORMS FITS BESIDE THE WIDEST ENDING: routingFor
	// refuses one past MaxRoutingBytes, so a role a later op adds to a
	// routing never takes a failed head's ending past its record.
	if bytes := widest["ending"] + gitlet.MaxRoutingBytes; bytes > gitlet.MaxRecordBytes {
		t.Errorf("the widest ending and the most a routing may take are %d bytes, past MaxRecordBytes (%d)",
			bytes, gitlet.MaxRecordBytes)
	}
	// ITS ROW, ITS LOG'S TWO, ITS HORIZON'S ROWS EACH WITH ITS LOG, AND ITS
	// ROUTING'S NOTICES.
	if rows := 1 + 2 + 2*gitlet.CheckHorizonRows + gitlet.RoutingNoticeRows; rows > gitlet.MaxRecordRows {
		t.Errorf("one check record's apply touches up to %d rows, past MaxRecordRows (%d)", rows,
			gitlet.MaxRecordRows)
	}
}

// AN ENDING'S COUNTS ARE HELD TO ITS TAIL: CheckFinished.Validate refuses, naming the field, tests
// on a run that measured nothing, failed and skipped past the tests that ran, a negative line
// count, lines with no tail or fewer than the tail's own, and a cut with no tail; and it accepts a
// report of no tests on a passed run, lines equal to the tail's, and a cut without lines.
//
// Mutation: drop any one of the rules (its case validates, red).
func TestAnEndingsCountsAreHeldToItsTail(t *testing.T) {
	t.Parallel()
	key := gitlet.CheckKey{RepoID: "0190f3c4-0000-7000-8000-0000000000c1", OID: oidOf("head"), Target: mainRef,
		Name: "unit", Attempt: 1}
	zero, one := 0, 1
	failed := func(edit func(*gitlet.CheckFinished)) gitlet.CheckFinished {
		f := gitlet.CheckFinished{CheckKey: key, State: gitlet.CheckStateFailed, Reason: gitlet.CheckReasonExit,
			ExitCode: &one, Tail: "a\nb\nc\n", FinishedAt: harnessAt}
		edit(&f)
		return f
	}
	for name, tc := range map[string]struct {
		f     gitlet.CheckFinished
		field string
	}{
		"tests on an errored run": {gitlet.CheckFinished{CheckKey: key, State: gitlet.CheckStateErrored,
			Reason: gitlet.CheckReasonBoxFailed, Tests: &gitlet.CheckTests{Total: 1}, FinishedAt: harnessAt}, "tests"},
		"failed and skipped past the total": {failed(func(f *gitlet.CheckFinished) {
			f.Tests = &gitlet.CheckTests{Total: 3, Failed: 2, Skipped: 2}
		}), "tests"},
		"a negative total": {failed(func(f *gitlet.CheckFinished) {
			f.Tests = &gitlet.CheckTests{Total: -1}
		}), "tests"},
		"a negative line count": {failed(func(f *gitlet.CheckFinished) { f.LogLines = -1 }), "log_lines"},
		"lines with no tail": {failed(func(f *gitlet.CheckFinished) { f.Tail, f.LogLines = "", 3 }),
			"log_lines"},
		"fewer lines than the tail's": {failed(func(f *gitlet.CheckFinished) { f.LogLines = 2 }), "log_lines"},
		"a cut with no tail":          {failed(func(f *gitlet.CheckFinished) { f.Tail, f.TailCut = "", true }), "tail_cut"},
	} {
		err := tc.f.Validate()
		if !errors.Is(err, gitlet.ErrInvalid) || !strings.Contains(err.Error(), ": "+tc.field+": ") {
			t.Errorf("%s: Validate = %v, want ErrInvalid naming %s", name, err, tc.field)
		}
	}
	for name, f := range map[string]gitlet.CheckFinished{
		"a report of no tests on a passed run": {CheckKey: key, State: gitlet.CheckStatePassed,
			Reason: gitlet.CheckReasonExit, ExitCode: &zero, Tests: &gitlet.CheckTests{}, FinishedAt: harnessAt},
		"lines equal to the tail's": failed(func(f *gitlet.CheckFinished) {
			f.Tests, f.LogLines = &gitlet.CheckTests{Total: 3, Failed: 1, Skipped: 2}, 3
		}),
		"a cut without lines": failed(func(f *gitlet.CheckFinished) { f.TailCut = true }),
	} {
		if err := f.Validate(); err != nil {
			t.Errorf("%s: Validate = %v, want it valid", name, err)
		}
	}
}

// A TAIL IS NUMBERED FROM ITS OUTPUT'S LINES: its first line is the output's lines less the tail's
// own, plus one, whether or not the two end in a newline.
//
// Mutation: number it the output's lines less the tail's newlines (the first case reads 0, red).
func TestATailIsNumberedFromItsOutputsLines(t *testing.T) {
	t.Parallel()
	failing18 := strings.Repeat("--- FAIL: TestRetry\n", 18) // a failing run's last 18 lines
	for _, tc := range []struct {
		tail        string
		lines, want int
	}{
		{"a\nb\nc\n", 3, 1}, {"a\nb", 2, 1}, {failing18, 1201, 1184}, {"a", 1, 1}, {"a\n", 0, 0}, {"", 4, 0},
	} {
		if got := gitlet.TailFirstLine(tc.lines, tc.tail); got != tc.want {
			t.Errorf("TailFirstLine(%d, %q) = %d, want %d", tc.lines, tc.tail, got, tc.want)
		}
	}
	for tail, want := range map[string]int{"": 0, "a": 1, "a\n": 1, "a\nb": 2, "\n\n": 2} {
		if got := gitlet.TailLines(tail); got != want {
			t.Errorf("TailLines(%q) = %d, want %d", tail, got, want)
		}
	}
}

// A CHECK READ ANSWERS ITS COUNTS AND NUMBERS ITS TAIL: the `check` read of an attempt whose
// ending counted answers its tests, its lines, its cut and its tail's first line; an attempt
// whose ending counted nothing answers none of them; and the `checks` listing answers none.
//
// Mutation: add the columns to checkColumns and the listing's rows (the listing carries
// log_lines, red); drop the derivation (tail_first_line 0, red).
func TestACheckReadAnswersItsCountsAndNumbersItsTail(t *testing.T) {
	t.Parallel()
	n := newNode(t)
	n.ensure("web", described("web"), t1)
	repo := n.mustID("web")
	w, _, _, _ := checksWriterOn(t, n)
	ctx := t.Context()
	counted := gitlet.CheckKey{RepoID: repo, OID: oidOf("B"), Target: mainRef, Name: "unit", Attempt: 1}
	plain := gitlet.CheckKey{RepoID: repo, OID: oidOf("B"), Target: mainRef, Name: "lint", Attempt: 1}
	for _, key := range []gitlet.CheckKey{counted, plain} {
		for _, r := range []gitlet.ChecksRequest{queueing(key, gitlet.QueueDispatch), claiming(key, "git-a", "")} {
			res, err := w.Publish(ctx, r)
			requireChecked(t, "queue and claim "+key.Name, res, err)
		}
		end := ending(key, gitlet.CheckStateFailed, gitlet.CheckReasonExit)
		end.Finished.Tail = strings.Repeat("--- FAIL: TestRetry\n", 18)
		if key == counted {
			end.Finished.Tests = &gitlet.CheckTests{Total: 214, Failed: 1}
			end.Finished.LogLines, end.Finished.TailCut = 1201, true
		}
		res, err := w.Publish(ctx, end)
		requireChecked(t, "end "+key.Name, res, err)
	}
	stale := statelog.Freshness{Level: statelog.ReadStale}
	read := func(key gitlet.CheckKey) gitlet.CheckDetail {
		one, err := n.reader.Read(ctx, gitlet.ReadQuery{Kind: gitlet.ReadCheck, Asker: ada, Repo: "web", Fresh: stale,
			Checks: &gitlet.CheckQuery{OID: key.OID, Target: key.Target, Name: key.Name, Attempt: key.Attempt}})
		if err != nil || one.Check == nil {
			t.Fatalf("read %s: (%+v, %v)", key.Name, one.Check, err)
		}
		return *one.Check
	}
	got := read(counted)
	if got.Tests == nil || *got.Tests != (gitlet.CheckTests{Total: 214, Failed: 1}) || got.LogLines != 1201 ||
		!got.TailCut || got.TailFirstLine != 1184 {
		t.Errorf("the counted attempt reads tests %+v, lines %d, cut %v, first line %d; want 214 and 1 failed, "+
			"1201, cut, 1184", got.Tests, got.LogLines, got.TailCut, got.TailFirstLine)
	}
	if got := read(plain); got.Tests != nil || got.LogLines != 0 || got.TailCut || got.TailFirstLine != 0 {
		t.Errorf("an attempt that counted nothing reads tests %+v, lines %d, cut %v, first line %d; want none",
			got.Tests, got.LogLines, got.TailCut, got.TailFirstLine)
	}
	listed, err := n.reader.Read(ctx, gitlet.ReadQuery{Kind: gitlet.ReadChecks, Asker: ada, Repo: "web", Fresh: stale})
	if err != nil || len(listed.Checks) != 2 {
		t.Fatalf("the listing read (%+v, %v), want both attempts", listed.Checks, err)
	}
	if encoded, err := json.Marshal(listed); err != nil || strings.Contains(string(encoded), "log_lines") {
		t.Errorf("the listing answers an ending's counts (%v): %s", err, encoded)
	}
}
