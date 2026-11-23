package gitlet_test

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/gitlet"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/store"
)

// A HISTORY PAGE'S MARKS AND A MERGE'S GATE, read back from the rows the
// shipped applier wrote (and, for a check's attempts, rows written as its
// applier writes them) through the shipped reader.

// attemptOf is one attempt of a check as a mark's case writes it.
type attemptOf struct {
	name              string
	attempt           int
	state             gitlet.CheckState
	reason            gitlet.CheckReason
	required          bool
	started, finished time.Time
}

// attempted writes a of commit oid of repoID, checked for target, straight
// into n's estate, every column as the applier writes it: an instant left
// zero is none.
func (n *node) attempted(repoID, oid, target string, a attemptOf) {
	n.t.Helper()
	at := func(t time.Time) int64 {
		if t.IsZero() {
			return 0
		}
		return store.EncodeTime(t)
	}
	n.exec(`INSERT INTO code_checks (repo_id, commit_oid, target_ref, name, attempt, state, purpose, change_id,
		revision, candidate_id, entry_id, base_oid, defs_oid, required, timeout_seconds, reason,
		runner_node, placement, box_handle, reporter, queued_at, started_at, finished_at,
		duration_ms, exit_code, failures_omitted, queued_position, finished_position, row_bytes, overdue_at,
		url, summary, def, failures, tail)
		VALUES (?, ?, ?, ?, ?, ?, 'head', '', 0, '', '', '', '', ?, 600, ?,
			'node-a', 'container', '', '', ?, ?, ?, 1000, 0, 0, 1, 2, 4, ?, '', '', '', '', '')`,
		repoID, oid, target, a.name, a.attempt, string(a.state), a.required, string(a.reason),
		store.EncodeTime(harnessAt), at(a.started), at(a.finished),
		store.EncodeTime(harnessAt.Add(600*time.Second)))
}

// mergedAt is the instant change id's row says it merged at, zero for none.
func (n *node) mergedAt(id string) time.Time {
	n.t.Helper()
	var at sql.NullInt64
	if err := n.db.Reader().Read(n.t.Context(), func(tx *sql.Tx) error {
		return tx.QueryRowContext(n.t.Context(), `SELECT merged_at FROM code_changes WHERE id = ?`, id).Scan(&at)
	}); err != nil {
		n.t.Fatalf("read change %s: %v", id, err)
	}
	if !at.Valid {
		return time.Time{}
	}
	return store.DecodeTime(at.Int64)
}

// marksOf is the marks of commits of repo, as asker reads them.
func marksOf(t *testing.T, n *node, repo string, commits ...string) []gitlet.CommitMark {
	t.Helper()
	a, err := n.reader.Read(t.Context(), gitlet.ReadQuery{Kind: gitlet.ReadCommitMarks, Asker: asker, Repo: repo,
		CommitMarks: commits})
	if err != nil {
		t.Fatalf("read the marks of %d commits of %s: %v", len(commits), repo, err)
	}
	if len(a.CommitMarks) != len(commits) {
		t.Fatalf("the marks of %d commits of %s are %d", len(commits), repo, len(a.CommitMarks))
	}
	for i, m := range a.CommitMarks {
		if m.Commit != commits[i] {
			t.Fatalf("mark %d is of %s, want %s: the query's order", i, m.Commit, commits[i])
		}
	}
	return a.CommitMarks
}

// commitOf is the changes commit of repo belongs to, as a commit's page reads
// them.
func commitOf(t *testing.T, n *node, repo, commit string) *gitlet.CommitChanges {
	t.Helper()
	a, err := n.reader.Read(t.Context(), gitlet.ReadQuery{Kind: gitlet.ReadCommit, Asker: asker, Repo: repo,
		Commit: commit})
	if err != nil || a.Commit == nil {
		t.Fatalf("read commit %s of %s: %+v, %v", commit, repo, a.Commit, err)
	}
	return a.Commit
}

// A COMMIT'S MARK COUNTS EACH CHECK'S LATEST ATTEMPT: unit failed then passed, lint
// running and e2e queued roll up as running.
// Mutation: drop latestOnly from latestChecksOfCommitSQL, or rank running below pending.
func TestACommitsMarkCountsEachChecksLatestAttempt(t *testing.T) {
	t.Parallel()
	n := newNode(t)
	repo := changeRepo(t, n, "web")
	c := oidOf("C")
	n.attempted(repo, c, mainRef, attemptOf{name: "unit", attempt: 1, state: gitlet.CheckStateFailed,
		reason: gitlet.CheckReasonExit, required: true, started: harnessAt, finished: harnessAt.Add(time.Minute)})
	n.attempted(repo, c, mainRef, attemptOf{name: "unit", attempt: 2, state: gitlet.CheckStatePassed,
		reason: gitlet.CheckReasonExit, required: true, started: harnessAt.Add(2 * time.Minute),
		finished: harnessAt.Add(3 * time.Minute)})
	n.attempted(repo, c, mainRef, attemptOf{name: "lint", attempt: 1, state: gitlet.CheckStateRunning,
		started: harnessAt.Add(4 * time.Minute)})
	n.attempted(repo, c, mainRef, attemptOf{name: "e2e", attempt: 1, state: gitlet.CheckStateQueued,
		required: true})

	got := marksOf(t, n, "web", c)[0]
	want := []gitlet.TargetChecks{{Target: mainRef, CheckRollup: gitlet.CheckRollup{State: gitlet.RollupRunning,
		Counts: gitlet.CheckCounts{Queued: 1, Running: 1, Passed: 1}, Required: 2, RequiredPassed: 1,
		LastEnded: harnessAt.Add(3 * time.Minute), RunningSince: harnessAt.Add(4 * time.Minute)}}}
	if !reflect.DeepEqual(got.Checks, want) || got.ChecksMore || got.More || got.Merged != nil || got.Heads != nil {
		t.Errorf("C's mark is %+v, want only its checks for main %+v", got, want)
	}
}

// A COMMIT'S MARK ROLLS UP EACH BRANCH APART, and of five branches names four with more.
// Mutation: roll every attempt up as one set.
func TestACommitsMarkRollsUpEachBranchApart(t *testing.T) {
	t.Parallel()
	n := newNode(t)
	repo := changeRepo(t, n, "web")
	c := oidOf("C")
	n.attempted(repo, c, mainRef, attemptOf{name: "unit", attempt: 1, state: gitlet.CheckStatePassed,
		reason: gitlet.CheckReasonExit, required: true, finished: harnessAt})
	n.attempted(repo, c, releaseRef, attemptOf{name: "unit", attempt: 1, state: gitlet.CheckStateFailed,
		reason: gitlet.CheckReasonExit, finished: harnessAt})
	got := marksOf(t, n, "web", c)[0]
	want := []gitlet.TargetChecks{
		{Target: mainRef, CheckRollup: gitlet.CheckRollup{State: gitlet.RollupPassed,
			Counts: gitlet.CheckCounts{Passed: 1}, Required: 1, RequiredPassed: 1, LastEnded: harnessAt}},
		{Target: releaseRef, CheckRollup: gitlet.CheckRollup{State: gitlet.RollupFailing,
			Counts: gitlet.CheckCounts{Failed: 1}, LastEnded: harnessAt,
			Failing: []gitlet.FailingCheck{{Name: "unit", State: gitlet.CheckStateFailed}}}},
	}
	if !reflect.DeepEqual(got.Checks, want) || got.ChecksMore {
		t.Errorf("C's checks are %+v (more %t), want main's and release's apart: %+v", got.Checks, got.ChecksMore,
			want)
	}

	d := oidOf("D")
	for i := range gitlet.MaxMarkTargets + 1 {
		n.attempted(repo, d, fmt.Sprintf("refs/heads/r%d", i), attemptOf{name: "unit", attempt: 1,
			state: gitlet.CheckStatePassed, reason: gitlet.CheckReasonExit, finished: harnessAt})
	}
	wide := marksOf(t, n, "web", d)[0]
	if len(wide.Checks) != gitlet.MaxMarkTargets || !wide.ChecksMore ||
		wide.Checks[gitlet.MaxMarkTargets-1].Target != fmt.Sprintf("refs/heads/r%d", gitlet.MaxMarkTargets-1) {
		t.Errorf("a commit checked for %d branches carries %d rollups (more %t), want the first %d and more",
			gitlet.MaxMarkTargets+1, len(wide.Checks), wide.ChecksMore, gitlet.MaxMarkTargets)
	}
}

// A COMMIT'S MARK NAMES THE CHANGES IT LANDED WITH, with no gate or reason, never
// another repository's, and of five merged at one commit four with more.
// Mutation: drop the bound on the merged list.
func TestACommitsMarkNamesTheChangesItLandedWith(t *testing.T) {
	t.Parallel()
	n := newNode(t)
	c, _ := committerOn(t, n, n.pub)
	ctx := t.Context()
	web := changeRepo(t, n, "web")
	api := changeRepo(t, n, "api")
	opened, err := c.ProposeChange(ctx, opening(web, featureRef, mainRef, dev))
	requireChanged(t, opened, err)
	other, err := c.ProposeChange(ctx, opening(api, featureRef, mainRef, dev))
	requireChanged(t, other, err)
	pushedBy(t, c, web, ada, update(featureRef, oidOf("B"), oidOf("C")))
	pushedBy(t, c, api, ada, update(featureRef, oidOf("B"), oidOf("C")))
	pushedBy(t, c, web, ada, update(mainRef, oidOf("A"), oidOf("C")))

	landed := gitlet.ChangeLanding{ChangeName: gitlet.ChangeName{Repo: "web", Ref: "web!1",
		Title: "Retry the upload", State: gitlet.ChangeStateMerged}, MergeOID: oidOf("C"),
		MergedAt: n.mergedAt(opened.ChangeID), MergedVia: gitlet.MergedViaPush}
	if landed.MergedAt.IsZero() {
		t.Fatal("web!1's row holds no instant it merged at")
	}
	marks := marksOf(t, n, "web", oidOf("B"), oidOf("C"), oidOf("Z"))
	want := []gitlet.CommitMark{
		{Commit: oidOf("B"), Heads: []gitlet.CommitRevision{{Change: landed, N: 1}}},
		{Commit: oidOf("C"), Merged: []gitlet.ChangeLanding{landed},
			Heads: []gitlet.CommitRevision{{Change: landed, N: 2}}},
		{Commit: oidOf("Z")},
	}
	if !reflect.DeepEqual(marks, want) {
		t.Errorf("the marks of B, C and Z are\n  %+v\nwant\n  %+v", marks, want)
	}

	// FIVE CHANGES AT ONE COMMIT, each from a branch of its own at X, all
	// merged by one push setting main to X.
	wide := changeRepo(t, n, "wide")
	var cmds []gitlet.Command
	for i := range gitlet.MaxMarkChanges + 1 {
		cmds = append(cmds, set(fmt.Sprintf("refs/heads/f%d", i), gitlet.ZeroOID, oidOf("X")))
	}
	n.push(wide, entry(uuid.Must(uuid.NewV7()).String(), cmds...))
	for i := range gitlet.MaxMarkChanges + 1 {
		res, err := c.ProposeChange(ctx, opening(wide, fmt.Sprintf("refs/heads/f%d", i), mainRef, dev))
		requireChanged(t, res, err)
	}
	pushedBy(t, c, wide, ada, update(mainRef, oidOf("A"), oidOf("X")))
	x := marksOf(t, n, "wide", oidOf("X"))[0]
	if len(x.Merged) != gitlet.MaxMarkChanges || len(x.Heads) != gitlet.MaxMarkChanges || !x.More {
		t.Errorf("a commit five changes merged at names %d merged and %d heads (more %t), want %d of each and "+
			"more", len(x.Merged), len(x.Heads), x.More, gitlet.MaxMarkChanges)
	}
}

// A DIRECT MERGE'S GATE IS ITS HEAD'S CHECKS AND ITS APPROVALS of the merged revision;
// a mark names the landing alone, and a merge past the gate carries its reason.
// Mutation: roll up the merge commit's checks, skip belonging.marked, or drop the revision term.
func TestADirectMergesGateIsItsHeadsChecksAndItsApprovals(t *testing.T) {
	t.Parallel()
	n := newNode(t)
	n.chart = testChart()
	c, _ := mergeStream(t, n)
	w, _ := writerOn(t, n, n.pub)
	repo, change := mergeRepo(t, n, c, "web", gitlet.DirectPersons, mergeGate())
	approved(t, n, w, repo, change)
	converse(t, w, reviewing(repo, change, agent("cto"), 1, gitlet.VerdictApprove, nil))
	converse(t, w, n.merging(repo, change, 1, oidOf("B"), mainRef, gitlet.MergeClean))
	n.passed(repo, oidOf("B"), mainRef, "unit")
	n.passed(repo, oidOf("B"), mainRef, "e2e")
	res, err := c.ProposeMerge(t.Context(), merging(repo, change, dev, gitlet.MergeMethodMerge, oidOf("A"),
		oidOf("B"), oidOf("M")))
	requireMerged(t, res, err, oidOf("M"))
	at := n.mergedAt(change)
	n.readAt.Store(&at)

	landed := gitlet.ChangeLanding{ChangeName: gitlet.ChangeName{Repo: "web", Ref: "web!1",
		Title: "Retry the upload", State: gitlet.ChangeStateMerged}, MergeOID: oidOf("M"), MergedAt: at,
		MergedVia: gitlet.MergedViaDirect, MergedBy: "dev", MergedByKind: gitlet.PartyAgent}
	gated := landed
	gated.Gate = &gitlet.LandingGate{Approvals: []gitlet.GateApproval{
		{Handle: "cto", Kind: gitlet.PartyAgent, Revision: 1}, {Handle: "plat-lead", Kind: gitlet.PartyAgent,
			Revision: 1}},
		ChecksOn: oidOf("B"), Checks: &gitlet.CheckRollup{State: gitlet.RollupPassed,
			Counts: gitlet.CheckCounts{Passed: 2}, Required: 2, RequiredPassed: 2, LastEnded: harnessAt}}
	if got := commitOf(t, n, "web", oidOf("M")); !reflect.DeepEqual(got.Merged, []gitlet.ChangeLanding{gated}) {
		t.Errorf("M's page names %+v, want web!1 merged directly through its gate %+v", got.Merged, gated)
		if len(got.Merged) == 1 && got.Merged[0].Gate != nil {
			t.Logf("its gate: %+v, checks %+v", *got.Merged[0].Gate, got.Merged[0].Gate.Checks)
		}
	}
	if got := marksOf(t, n, "web", oidOf("M"))[0]; !reflect.DeepEqual(got.Merged, []gitlet.ChangeLanding{landed}) {
		t.Errorf("M's mark names %+v, want the landing alone %+v", got.Merged, landed)
	}

	// AN APPROVAL OF AN EARLIER REVISION is not named: cto approved
	// revision 1, plat-lead revision 2, which merged.
	twice, revised := mergeRepo(t, n, c, "two", gitlet.DirectPersons, mergeGate())
	converse(t, w, n.revising(twice, revised, 1, oidOf("B"), mainRef, nil))
	converse(t, w, reviewing(twice, revised, agent("cto"), 1, gitlet.VerdictApprove, nil))
	n.push(twice, entry(uuid.Must(uuid.NewV7()).String(), set(seatRef, oidOf("B"), oidOf("C"))))
	converse(t, w, n.revising(twice, revised, 2, oidOf("C"), mainRef, nil))
	converse(t, w, reviewing(twice, revised, platLead, 2, gitlet.VerdictApprove, nil))
	converse(t, w, n.merging(twice, revised, 2, oidOf("C"), mainRef, gitlet.MergeClean))
	n.passed(twice, oidOf("C"), mainRef, "unit")
	res, err = c.ProposeMerge(t.Context(), merging(twice, revised, dev, gitlet.MergeMethodMerge, oidOf("A"),
		oidOf("C"), oidOf("M")))
	requireMerged(t, res, err, oidOf("M"))
	want := []gitlet.GateApproval{{Handle: "plat-lead", Kind: gitlet.PartyAgent, Revision: 2}}
	if got := commitOf(t, n, "two", oidOf("M")).Merged; len(got) != 1 || got[0].Gate == nil ||
		!reflect.DeepEqual(got[0].Gate.Approvals, want) {
		t.Errorf("two's M names %+v, want its gate to name %+v alone", got, want)
	}

	// A PERSON'S MERGE PAST A QUEUE TARGET'S GATE, naming why.
	tipped, landing := queueRepo(t, n, c, "tip")
	ready(t, n, w, tipped, landing)
	override := merging(tipped, landing, ada, gitlet.MergeMethodMerge, oidOf("A"), oidOf("B"), oidOf("M"))
	override.Merge.Override = "the queue's runner is down"
	merged, err := c.ProposeMerge(t.Context(), override)
	requireMerged(t, merged, err, oidOf("M"))
	got := commitOf(t, n, "tip", oidOf("M")).Merged
	if len(got) != 1 || got[0].OverrideReason != override.Merge.Override || got[0].MergedVia != gitlet.MergedViaDirect ||
		got[0].Gate == nil || got[0].Gate.ChecksOn != oidOf("B") {
		t.Errorf("tip's M names %+v, want tip!1 merged past its gate naming why, its gate's checks on B", got)
	}
	if mark := marksOf(t, n, "tip", oidOf("M"))[0]; len(mark.Merged) != 1 || mark.Merged[0].OverrideReason != "" {
		t.Errorf("tip's M is marked %+v, want its landing with no reason", mark.Merged)
	}
}

// landedTrain seeds repository name with two changes (from feature, head B, and
// release, head R) landed into main by ONE land record of a two-candidate train
// whose required check passed on M2 alone; it returns the repository and changes.
func landedTrain(t *testing.T, n *node, c *gitlet.Committer, w *gitlet.ChangeWriter,
	name string) (repo string, changes [2]string) {

	t.Helper()
	s := suiteSettings()
	s.Owner = gitlet.Owner{Kind: gitlet.OwnerUnit, Ref: "Engineering"}
	s.Protections[0].Gate = queueGate()
	repo = n.ensure(name, s, t1).RepoID
	n.push(repo, entry(uuid.Must(uuid.NewV7()).String(), set(mainRef, gitlet.ZeroOID, oidOf("A")),
		set(featureRef, gitlet.ZeroOID, oidOf("B")), set(releaseRef, gitlet.ZeroOID, oidOf("R"))))
	var entries [2]string
	for i, source := range []struct{ ref, head string }{{featureRef, oidOf("B")}, {releaseRef, oidOf("R")}} {
		opened, err := c.ProposeChange(t.Context(), opening(repo, source.ref, mainRef, dev))
		requireChanged(t, opened, err)
		changes[i] = opened.ChangeID
		converse(t, w, n.revising(repo, changes[i], 1, source.head, mainRef, nil))
		converse(t, w, reviewing(repo, changes[i], platLead, 1, gitlet.VerdictApprove, nil))
		n.passed(repo, source.head, mainRef, "unit")
		enqueue := enqueueing(repo, changes[i], ada, source.head)
		res, err := c.ProposeQueue(t.Context(), enqueue)
		requireQueued(t, res, err, gitlet.QueueWaiting, i+1)
		entries[i] = enqueue.Enqueue.EntryID
	}
	train := trainOf(repo, oidOf("A"), gitlet.CandidateEntry{EntryID: entries[0], Commit: oidOf("M1")},
		gitlet.CandidateEntry{EntryID: entries[1], Commit: oidOf("M2")})
	res, err := c.ProposeQueue(t.Context(), train)
	requireRecorded(t, res, err)
	n.passed(repo, oidOf("M2"), mainRef, "unit")
	res, err = c.ProposeQueue(t.Context(), landingOf(repo, train.Candidate.Train, oidOf("A"),
		gitlet.LandEntry{EntryID: entries[0], ChangeID: changes[0], Commit: oidOf("M1")},
		gitlet.LandEntry{EntryID: entries[1], ChangeID: changes[1], Commit: oidOf("M2")}))
	requireRecorded(t, res, err)
	return repo, changes
}

// A QUEUE LANDING'S GATE IS ITS LAND RECORD'S TIP: both changes of a two-entry train
// name the checks on M2 and their enqueuer as who merged them.
// Mutation: read a queue landing's gate on its own merge commit (skip landedTip).
func TestAQueueLandingsGateIsItsLandRecordsTip(t *testing.T) {
	t.Parallel()
	n := newNode(t)
	n.chart = testChart()
	c, _ := committerOn(t, n, n.pub)
	w, _ := writerOn(t, n, n.pub)
	repo, changes := landedTrain(t, n, c, w, "web")
	if n.count(`SELECT COUNT(*) FROM code_candidates WHERE repo_id = ?`, repo) != 0 {
		t.Fatal("the land left its candidates: this case is about a gate read without them")
	}
	at := n.mergedAt(changes[0])
	n.readAt.Store(&at)
	for i, commit := range []string{oidOf("M1"), oidOf("M2")} {
		got := commitOf(t, n, "web", commit).Merged
		if len(got) != 1 || got[0].MergedVia != gitlet.MergedViaQueue || got[0].MergeOID != commit ||
			got[0].MergedBy != "founder" || got[0].MergedByKind != gitlet.PartyPerson {
			t.Errorf("%s's page names %+v, want web!%d landed there by the queue for its enqueuer founder, a "+
				"person", commit[:7], got, i+1)
			continue
		}
		g := got[0].Gate
		if g == nil || g.ChecksOn != oidOf("M2") || g.Checks == nil || g.Checks.State != gitlet.RollupPassed ||
			g.Checks.RequiredPassed != 1 || g.Gone || len(g.Approvals) != 1 || g.Approvals[0].Handle != "plat-lead" {
			t.Errorf("web!%d's gate is %+v, want plat-lead's approval and the checks on the land's tip M2, "+
				"passed", i+1, g)
		}
	}
	if mark := marksOf(t, n, "web", oidOf("M1"))[0]; mark.Checks != nil || len(mark.Merged) != 1 {
		t.Errorf("M1's mark is %+v, want web!1 landed and no checks of its own", mark)
	}
}

// A LANDING PAST THE HISTORY READS GONE once its merged revision's push is
// CheckHistory old, not before, and names an attempt still kept.
// Mutation: read Gone by age alone, by absence alone, or age it from merged_at.
func TestALandingPastTheHistoryReadsGone(t *testing.T) {
	t.Parallel()
	n := newNode(t)
	n.chart = testChart()
	c, _ := mergeStream(t, n)
	w, _ := writerOn(t, n, n.pub)
	direct, change := mergeRepo(t, n, c, "web", gitlet.DirectPersons, mergeGate())
	approved(t, n, w, direct, change)
	converse(t, w, n.merging(direct, change, 1, oidOf("B"), mainRef, gitlet.MergeClean))
	n.passed(direct, oidOf("B"), mainRef, "unit")
	res, err := c.ProposeMerge(t.Context(), merging(direct, change, dev, gitlet.MergeMethodMerge, oidOf("A"),
		oidOf("B"), oidOf("M")))
	requireMerged(t, res, err, oidOf("M"))
	queued, landed := landedTrain(t, n, c, w, "api")
	for _, repo := range []string{direct, queued} {
		n.exec(`DELETE FROM code_checks WHERE repo_id = ?`, repo)
		n.exec(`DELETE FROM code_landings WHERE repo_id = ?`, repo)
	}

	gate := func(repo, commit string) *gitlet.LandingGate {
		t.Helper()
		got := commitOf(t, n, repo, commit).Merged
		if len(got) != 1 || got[0].Gate == nil {
			t.Fatalf("%s's %s names %+v, want one change merged through its gate", repo, commit[:7], got)
		}
		return got[0].Gate
	}
	for _, tc := range []struct {
		repo, commit, change string
	}{{"web", oidOf("M"), change}, {"api", oidOf("M2"), landed[1]}} {
		at := n.mergedAt(tc.change)
		inside := at.Add(time.Hour)
		n.readAt.Store(&inside)
		if g := gate(tc.repo, tc.commit); g.Gone || g.Checks != nil || g.ChecksOn != "" || len(g.Approvals) != 1 {
			t.Errorf("%s's gate read inside the history is %+v, want a gate that required no check, not gone",
				tc.repo, g)
		}
		past := at.Add(gitlet.CheckHistory + time.Hour)
		n.readAt.Store(&past)
		if g := gate(tc.repo, tc.commit); !g.Gone || g.Checks != nil || g.ChecksOn != "" ||
			len(g.Approvals) != 1 || g.Approvals[0].Handle != "plat-lead" {
			t.Errorf("%s's gate read past the history is %+v, want gone, its approval still named", tc.repo, g)
		}
	}
	// A REVISION PUSHED LONG BEFORE ITS MERGE: its checks may be swept while
	// the merge is still inside the history, so the gate reads gone.
	at := n.mergedAt(change)
	n.exec(`UPDATE code_revisions SET at = ? WHERE change_id = ?`, store.EncodeTime(at.Add(-10*24*time.Hour)),
		change)
	late := at.Add(gitlet.CheckHistory - 5*24*time.Hour)
	n.readAt.Store(&late)
	if g := gate("web", oidOf("M")); !g.Gone || g.Checks != nil {
		t.Errorf("a gate whose revision was pushed past the history is %+v, want gone", g)
	}
	// A HEAD CHECKED AGAIN since: past the history, its attempt kept, the
	// gate names it rather than calling it gone.
	n.passed(direct, oidOf("B"), mainRef, "unit")
	if g := gate("web", oidOf("M")); g.Gone || g.ChecksOn != oidOf("B") || g.Checks == nil {
		t.Errorf("a gate whose head's attempt is kept past the history is %+v, want it named", g)
	}
}

// A COMMIT MARKS READ IS BOUNDED AND REFUSES WHAT IT CANNOT READ: a full page marked
// in order, each invalid query refused by field, a seat outside a unit repository refused.
// Mutation: drop validateCommitMarks' duplicate check, or judge no grant in commitMarks.
func TestACommitMarksReadIsBoundedAndRefusesWhatItCannotRead(t *testing.T) {
	t.Parallel()
	n, _ := grantedChanges(t)
	var page []string
	for i := range gitlet.MaxCommitMarks {
		page = append(page, oidOf(fmt.Sprintf("page-%d", i)))
	}
	marksOf(t, n, "platform", page...)

	for name, tc := range map[string]struct {
		q     gitlet.ReadQuery
		field string
	}{
		"one past the page": {gitlet.ReadQuery{CommitMarks: append(page[:len(page):len(page)], oidOf("past"))},
			"commit_marks"},
		"an abbreviation": {gitlet.ReadQuery{CommitMarks: []string{oidOf("B")[:12]}}, "commit_marks"},
		"the zero id":     {gitlet.ReadQuery{CommitMarks: []string{gitlet.ZeroOID}}, "commit_marks"},
		"a commit twice":  {gitlet.ReadQuery{CommitMarks: []string{oidOf("B"), oidOf("B")}}, "commit_marks"},
		"no commit":       {gitlet.ReadQuery{}, "commit_marks"},
		"a limit":         {gitlet.ReadQuery{CommitMarks: []string{oidOf("B")}, Limit: 1}, "limit"},
		"a cursor":        {gitlet.ReadQuery{CommitMarks: []string{oidOf("B")}, Cursor: "x"}, "limit"},
		"a resolution":    {gitlet.ReadQuery{CommitMarks: []string{oidOf("B")}, Resolve: true}, "resolve"},
	} {
		q := tc.q
		q.Kind, q.Asker, q.Repo = gitlet.ReadCommitMarks, asker, "platform"
		if _, err := n.reader.Read(t.Context(), q); !errors.Is(err, gitlet.ErrInvalid) ||
			!strings.Contains(err.Error(), tc.field+":") {
			t.Errorf("a commit_marks read naming %s = %v, want ErrInvalid naming %s", name, err, tc.field)
		}
	}
	if _, err := n.reader.Read(t.Context(), gitlet.ReadQuery{Kind: gitlet.ReadCommitMarks,
		Asker: agent("vp-sales"), Repo: "plat-private", CommitMarks: []string{oidOf("B")}}); !errors.Is(err,
		gitlet.ErrNotPermitted) {
		t.Errorf("vp-sales reading plat-private's marks = %v, want ErrNotPermitted", err)
	}
}

// THE WIDEST COMMIT MARKS FIT A PAGE: a hundred marks at every bound encode
// within MaxPageBytes, so the read answers whole.
//
// Mutation: raise MaxMarkChanges to MaxCommitChanges: past the page, red.
func TestTheWidestCommitMarksFitAPage(t *testing.T) {
	t.Parallel()
	ref := "refs/heads/" + strings.Repeat("b", gitlet.MaxRefNameBytes-len("refs/heads/"))
	name := strings.Repeat("r", 64)
	title := strings.Repeat("<", gitlet.MaxChangeTitleBytes)
	at := time.Date(2031, 4, 2, 3, 14, 15, 123456789, time.UTC)
	rollup := gitlet.CheckRollup{State: gitlet.RollupFailing, Counts: gitlet.CheckCounts{Queued: 100,
		Running: 100, Passed: 100, Failed: 100, Errored: 100, Cancelled: 100, Skipped: 100, Other: 100},
		Required: 1000, RequiredPassed: 1000, LastEnded: at, RunningSince: at, FailingMore: 1000, More: true}
	for i := range gitlet.MaxRollupFailing {
		rollup.Failing = append(rollup.Failing, gitlet.FailingCheck{Name: fmt.Sprintf("%064d", i),
			State: gitlet.CheckStateErrored, Required: true})
	}
	change := gitlet.ChangeLanding{ChangeName: gitlet.ChangeName{Repo: name, Ref: name + "!999999999",
		Title: title, State: gitlet.ChangeStateMerged}, MergeOID: oidOf("M"), MergedAt: at,
		MergedVia: gitlet.MergedViaDirect, MergedBy: strings.Repeat("h", 64), MergedByKind: gitlet.PartyPerson}
	var answer gitlet.ReadAnswer
	for i := range gitlet.MaxCommitMarks {
		mark := gitlet.CommitMark{Commit: oidOf(fmt.Sprintf("c%d", i)), ChecksMore: true, More: true}
		for range gitlet.MaxMarkTargets {
			mark.Checks = append(mark.Checks, gitlet.TargetChecks{Target: ref, CheckRollup: rollup})
		}
		for range gitlet.MaxMarkChanges {
			mark.Merged = append(mark.Merged, change)
			mark.Heads = append(mark.Heads, gitlet.CommitRevision{Change: change, N: 1 << 40})
		}
		answer.CommitMarks = append(answer.CommitMarks, mark)
	}
	answer.Position = statelog.Position{Stream: "CREWLET_CODE_LOG", Generation: 1 << 30, Seq: 1 << 62}
	answer.Level = statelog.ReadLinearizable
	b, err := json.Marshal(answer)
	if err != nil {
		t.Fatal(err)
	}
	if len(b) > gitlet.MaxPageBytes {
		t.Errorf("the widest page of marks encodes to %d bytes, past the %d a page carries", len(b),
			gitlet.MaxPageBytes)
	}
	t.Logf("the widest page of marks encodes to %d bytes (%d a commit) of %d", len(b),
		len(b)/gitlet.MaxCommitMarks, gitlet.MaxPageBytes)
}
