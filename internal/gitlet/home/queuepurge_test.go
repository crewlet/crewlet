package home_test

import (
	"context"
	"os"
	"slices"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/gitlet"
	"github.com/crewlet/crewlet/internal/gitlet/compact"
	"github.com/crewlet/crewlet/internal/gitlet/gitlettest"
	"github.com/crewlet/crewlet/internal/gitlet/home"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/statelog"
)

// THE MERGE QUEUE ON REAL GIT, ACROSS A PURGE: one solo fleet's Gitlet node is
// home to gitlettest.RepoName, whose main is a queue target, and the
// home's three workers — its change work, its check work and its queue — are
// turned one at a time over the fleet's own estate, its cache and its pool,
// the train built by the Gitlet node's own chain builder: every record but a
// runner's claim and ending of a check — the case's, as no runner runs here —
// is the one production asks for, decided by the data node's committer.

// queueChecksFile is the checks file main holds: its one check, the one the
// queue gate requires.
const queueChecksFile = "checks:\n  - name: unit\n    run: make test\n    timeout_seconds: 600\n"

// queuePat is the fixture chart's person, through their own token: the merger
// whose enqueue puts the change in main's queue, and who purges it.
var queuePat = gitlet.Principal{Kind: gitlet.PrincipalOperator, OperatorID: iam.TokenLogin(gitlettest.FixturePat),
	Seat: gitlettest.FixturePat}

// queueHome is a git home whose main is a queue target — a person merges it,
// with no approval, once `unit` has passed — and the home's loop over the
// fleet's estate, never started: the case turns each worker itself.
func queueHome(t *testing.T) (*gitHome, *home.Loop) {
	t.Helper()
	settings := gitlettest.Settings()
	settings.Protections[0].Gate = &gitlet.MergeGate{MergeBy: gitlet.MergeByPersons, Approvals: gitlet.ApprovalsNone,
		Checks: []string{"unit"}, Landing: gitlet.Landing{Depth: 4, Batch: 4}, DeleteSource: gitlet.DeleteSeats}
	h := newGitHome(t, settings)
	node := h.node()
	h.commit(map[string]string{".crewlet/checks.yaml": queueChecksFile}, "add the checks")
	h.push("HEAD:refs/heads/main")
	l, err := home.New(home.Options{Chart: noChart, Estate: h.f.Code(t),
		Placement: asNode{placement: &placement{homes: map[string]string{h.repo: node.ID()}}, node: node.ID()},
		Nudges:    newNudges(), Cache: node.Cache(), Git: node.Pool(), Chains: node.Host(), Tasks: unqueued{t: t},
		Share: units(2), NodeID: node.ID()})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return h, l
}

// rows is the repository's rows, read linearizable.
func (h *gitHome) rows() gitlettest.Rows {
	h.t.Helper()
	rows, err := h.f.Rows(context.Background(), gitlettest.RepoName, gitlettest.Linearizable)
	if err != nil {
		h.t.Fatalf("the rows of %s: %v", gitlettest.RepoName, err)
	}
	return rows
}

// at is the code log's position the repository's rows are read at now: the
// floor each turn of the case reads from.
func (h *gitHome) at() statelog.Position {
	h.t.Helper()
	return h.rows().Position
}

// queueDue is main's queue work as the home reads it, linearizable.
func (h *gitHome) queueDue() gitlet.QueueDue {
	h.t.Helper()
	a, err := h.f.Code(h.t).Read(context.Background(), gitlet.ReadQuery{Kind: gitlet.ReadQueueDue,
		Fresh: gitlettest.Linearizable, QueueDue: &gitlet.QueueDueQuery{RepoID: h.repo}})
	if err != nil || a.DueQueue == nil {
		h.t.Fatalf("main's queue work: %+v, %v", a.DueQueue, err)
	}
	return *a.DueQueue
}

// passCheck claims attempt key as this node's runner and ends it passed, as a
// check's box that ran it would.
func (h *gitHome) passCheck(key gitlet.CheckKey) {
	h.t.Helper()
	exit := 0
	served := h.f.Data().Served()
	for _, req := range []gitlet.ChecksRequest{
		{OpID: statelog.NewOpID(time.Now(), "code_check_claim"), Started: &gitlet.CheckStarted{CheckKey: key,
			Runner: h.node().ID(), Placement: gitlet.PlacementContainer, StartedAt: time.Now()}},
		{OpID: statelog.NewOpID(time.Now(), "code_check_end"), Finished: &gitlet.CheckFinished{CheckKey: key,
			State: gitlet.CheckStatePassed, Reason: gitlet.CheckReasonExit, ExitCode: &exit, DurationMs: 1_000,
			FinishedAt: time.Now()}},
	} {
		if res, err := served.PublishChecks(context.Background(), req); err != nil || res.Row == nil {
			h.t.Fatalf("a runner's record of %s %s on %s: %+v, %v", key.Name, key.Target, key.OID, res, err)
		}
	}
}

// purged asks the repository's purge as queuePat and serves it with the Gitlet
// node's compactor, the maintainer of every repository of a solo fleet, until
// the expunge listing names the repository no more.
func (h *gitHome) purged() {
	h.t.Helper()
	ctx := context.Background()
	served := h.f.Data().Served()
	req := gitlet.ExpungeRequest{RepoID: h.repo, RequestID: statelog.NewOpID(time.Now(), "code_purge_unreachable"),
		By: queuePat}
	if res, err := served.ProposeExpunge(ctx, gitlet.ExpungeProposal{Request: &req}); err != nil ||
		res.Outcome != statelog.OutcomeApplied {
		h.t.Fatalf("the purge request: %+v, %v", res, err)
	}
	for range 12 {
		// THE NODE'S MAINTAINER keeps the one slot of the maintenance lane an
		// expunge's build may take for a moment at a time: a busy answer is
		// asked again, for a minute at the most — of what the listing names
		// then, as the next pass asks it. A cycle's busy answer may come after
		// its begin has applied, and the listing names that expunge open from
		// then on: a cycle of the candidate read before it, which names none,
		// would plan over a copy whose every live pack the begin dated at its
		// scan, find the purge in hand and answer not_due (#840).
		deadline := time.Now().Add(time.Minute)
		for {
			a, err := served.Read(ctx, gitlet.ReadQuery{Kind: gitlet.ReadExpungeDue, Fresh: gitlettest.Linearizable})
			if err != nil {
				h.t.Fatalf("the expunge listing: %v", err)
			}
			i := slices.IndexFunc(a.Expunge, func(c gitlet.ExpungeCandidate) bool { return c.Name == gitlettest.RepoName })
			if i < 0 {
				return
			}
			out, err := h.node().Compactor().Expunge(ctx, a.Expunge[i])
			if out == compact.OutcomeBusy && time.Now().Before(deadline) { //fixedwait:measure a poll's own deadline: a busy answer past it fails the case
				time.Sleep(20 * time.Millisecond) //fixedwait:liveness
				continue
			}
			switch out {
			case compact.OutcomeApplied, compact.OutcomePending, compact.OutcomeUnknown:
			default:
				h.t.Fatalf("an expunge of %s answered %s, %v", gitlettest.RepoName, out, err)
			}
			break
		}
	}
	h.t.Fatalf("a dozen expunges left %s's purge unserved", gitlettest.RepoName)
}

// A PENDING LANDING'S TIP SURVIVES A PURGE, ON REAL GIT (M6-D2, M6-D4): a
// queued change's train is a merge commit its home builds in a scratch and
// stores in a pack, which no ref reaches until the landing moves main onto it
// — so a purge between the train and its landing, which keeps no ref drop,
// keeps the candidate's tip all the same, because the expunge's tips name
// every candidate's; and the landing, read after the purge, states the drop
// epoch the purge's begin moved, so it lands. A cache rebuilt from the rows
// alone then holds every object main reaches (git fsck), its tip the
// candidate's merge commit among them.
//
// Mutations: drop the candidates' phase from an expunge's tips, and the purge
// drops the merge commit only the train's pack held — main lands on an object
// no pack holds, and the rebuilt cache's fsck fails; land at the drop epoch
// the train was built at rather than the one its read held, and the landing
// is refused expunging after the purge — main never moves.
func TestAPendingLandingsTipSurvivesAPurge(t *testing.T) {
	t.Parallel()
	h, l := queueHome(t)
	ctx := t.Context()
	base := h.p.Head(h.work)
	head := h.commit(map[string]string{"fix.txt": "fixed\n"}, "fix the upload")
	url := h.node().URL() + "/git/" + gitlettest.RepoName
	if res := h.p.PushArgs(ctx, h.work, []string{"-o", "change"}, url, "HEAD:refs/heads/feature"); !res.OK(
		"refs/heads/feature") {
		t.Fatalf("the push opening a change: %s", res)
	}
	// MAIN MOVES PAST THE CHANGE'S BASE, so the queue's merge is a commit of
	// its own, never the head a fast-forward would land.
	h.git("reset", "--hard", base)
	h.commit(map[string]string{"docs.txt": "documented\n"}, "document the upload")
	h.push("HEAD:refs/heads/main")

	// THE CHANGE WORK records the revision's facts and its merge into main;
	// the check work dispatches main's required check on its head, which a
	// runner passes.
	if err := home.ChangeWork(ctx, l, h.repo, h.at()); err != nil {
		t.Fatalf("the change work: %v", err)
	}
	if err := home.CheckWork(ctx, l, h.repo, h.at()); err != nil {
		t.Fatalf("the check work on the head: %v", err)
	}
	h.passCheck(gitlet.CheckKey{RepoID: h.repo, OID: head, Target: "refs/heads/main", Name: "unit", Attempt: 1})
	a, err := h.f.Code(t).Read(ctx, gitlet.ReadQuery{Kind: gitlet.ReadChanges, Asker: queuePat,
		Repo: gitlettest.RepoName, Fresh: gitlettest.Linearizable})
	if err != nil || len(a.Changes) != 1 {
		t.Fatalf("the changes: %+v, %v", a.Changes, err)
	}
	merged, err := h.node().Host().Write(ctx, gitlet.EngineWrite{Kind: gitlet.WriteMergeChange, RepoID: h.repo,
		Principal: queuePat, OpID: statelog.NewOpID(time.Now(), ""), Fresh: gitlettest.Linearizable,
		Ref: "refs/heads/main", ExpectedHead: head, Change: a.Changes[0].Number, Method: gitlet.MergeMethodMerge})
	if err != nil || merged.Merge == nil || !merged.Merge.Queued || merged.Merge.Refusal != "" {
		t.Fatalf("the merge of the change answered %+v, %v; want it queued", merged.Merge, err)
	}

	// THE QUEUE builds its train, then dispatches its check on the
	// candidate's tip: a merge commit no ref reaches.
	if err := home.QueueWork(ctx, l, h.repo, h.at()); err != nil {
		t.Fatalf("the queue's build: %v", err)
	}
	if err := home.QueueWork(ctx, l, h.repo, h.at()); err != nil {
		t.Fatalf("the queue's dispatch: %v", err)
	}
	d := h.queueDue()
	if len(d.Train) != 1 || len(d.Train[0].Tip().Checks) != 1 {
		t.Fatalf("the premise: main's train is %+v; want one candidate, its check dispatched", d.Train)
	}
	tip := d.Train[0].Tip()
	rows := h.rows()
	for ref, id := range rows.Refs {
		if id == tip.Commit {
			t.Fatalf("the premise: %s holds the candidate's tip %s, which no ref may reach", ref, tip.Commit)
		}
	}

	h.purged()
	if epoch := h.rows().Repo.DropEpoch; epoch < 1 {
		t.Fatalf("the premise: the repository is at drop epoch %d after a purge, want the expunge counted", epoch)
	}
	h.passCheck(gitlet.CheckKey{RepoID: h.repo, OID: tip.Commit, Target: "refs/heads/main", Name: "unit",
		Attempt: tip.Checks[0].Attempt})
	if err := home.QueueWork(ctx, l, h.repo, h.at()); err != nil {
		t.Fatalf("the queue's landing: %v", err)
	}
	if main := h.rows().Refs["refs/heads/main"]; main != tip.Commit {
		t.Fatalf("main is at %s after the landing, want the candidate's tip %s", main, tip.Commit)
	}

	// A CACHE REBUILT FROM THE ROWS ALONE: the node killed, its cache's
	// directory removed, the node restarted over the rest.
	id, dir := h.node().ID(), h.node().CacheDir()
	h.f.Kill(t, id)
	if err := os.RemoveAll(dir); err != nil {
		t.Fatalf("remove %s: %v", dir, err)
	}
	h.f.Restart(t, id)
	s, err := h.node().Cache().Sync(ctx, h.repo, gitlettest.Linearizable)
	if err != nil {
		t.Fatalf("sync the rebuilt cache: %v", err)
	}
	t.Cleanup(s.Release)
	if out, stderr, err := h.p.Git().Try(t, "", nil, "--git-dir="+s.Dir, "fsck", "--connectivity-only",
		"--no-dangling"); err != nil {
		t.Fatalf("git fsck of a cache rebuilt from the rows after the landing: %v\n%s%s", err, out, stderr)
	}
	if !h.holds(s.Dir, tip.Commit) {
		t.Errorf("a cache rebuilt from the rows lacks main's tip %s, the candidate's merge commit", tip.Commit)
	}
}
