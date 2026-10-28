package codetest

import (
	"context"
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/code"
	"github.com/crewlet/crewlet/internal/code/codefault"
	"github.com/crewlet/crewlet/internal/statelog"
)

// Cluster is a topology of at least three data nodes, each also a git node,
// routing to one another through the production estate router — the shape
// every claim about MORE THAN ONE data node needs: a push decided by another
// node's committer, a committer that dies, an evicted writer, a stopping one.
// Fleet's methods hold, every node id a member's; Kill is a PROCESS death,
// its broker member included, and Restart what that process comes back as.
//
// WHO THE ROUTERS ASK is the members whose PRESENCE is live, as the engine's
// watched presence view lists them: a member that is killed gives nothing
// back, so its peers keep listing it — and asking it, and hearing nothing —
// until it is restarted, while a member that is stopped gives its presence
// back first, as the engine's drain does.
type Cluster interface {
	Fleet
	// Members is every member's id, in start order, whether running or
	// not. DataNodes and Nodes are the running ones.
	Members() []string
	// Committer is the member every member routes repoID's pushes to: the
	// first of rendezvous.Order(repoID, the running members) — the router's
	// own rule, computed the way it computes it. A killed member still
	// listed is asked first and heard silent before the push reaches this
	// one.
	Committer(repoID string) string
	// Stop is a GRACEFUL stop in the engine's order: the member's presence
	// given back (the engine's drain), its git node drained and stopped
	// (the API's listener, then the teardown's first step, stopCodeHost —
	// so it takes no push or sync once its committer refuses), then its
	// committer refusing, then its estate subject no longer answering, then
	// its applier, its store and its broker member. Restart brings it back.
	// It reports through t and never ends the calling goroutine, so a case
	// may stop a member from another one.
	Stop(t testing.TB, node string)
	// Unanswered is how many times the members' routers have told the
	// placement that node did not answer them (estate.Placement.Unanswered)
	// — what a router does on meeting a member that is gone.
	Unanswered(node string) int
	// Appends is how many records node's committer has appended
	// (DataNode.Appends).
	Appends(node string) int
	// WriteAttempt is how long every member's router waits on one data node
	// for a write: what a push asked of a node that never answers costs
	// before the runner-up is asked.
	WriteAttempt() time.Duration
	// GitReadThrough is a read of a repository by object id asked through
	// member node's own router, as a seat's tool on that member asks it —
	// the repository's home git node first, the member's own git half at its
	// place in the order (ADR-0059).
	GitReadThrough(ctx context.Context, node string, q code.GitQuery) (code.GitAnswer, error)
	// Settle is the cluster as a case finds it: every member running —
	// one that is not is an error naming it — each one's applier level with
	// the code log, and every router's memory of a member that refused or
	// went silent lapsed, as the half minute a router keeps it does.
	Settle(ctx context.Context) error
	// SplitHomes has every member of nodes take itself for repoID's home —
	// its compactor's Homes answering yes — until the case ends: a presence
	// view split between them, which no lease or epoch arbitrates
	// (ADR-0059), so each compacts the repository as its home.
	SplitHomes(t testing.TB, repoID string, nodes ...string)
	// ProposeCompactionAgain asks member node's router to decide, again, the
	// last compaction a member's compactor proposed of repoID, under its own
	// compaction id — as a compactor whose answer came back late asks again.
	ProposeCompactionAgain(ctx context.Context, node, repoID string) (code.CompactResult, error)
	// ChangeThrough is a write of a change's lifecycle asked through member
	// node's own router, as a tool on that member asks it: of the
	// repository's committer first, its runner-up after (code.change).
	ChangeThrough(ctx context.Context, node string, p code.ChangeProposal) (code.ChangeResult, error)
}

// RunFleet runs every case only a fleet of data nodes can make, IN ORDER ON
// ONE CLUSTER: a three-member start is seconds to tens of seconds on a loaded
// runner, a fresh cluster per case and per crash point would be minutes of
// starts, and what a case changes is a repository of its own — so each case
// declares its own, and a member it kills or pauses it brings back before it
// returns. Between cases every member is running, level and its cache holds
// exactly the rows of every repository, so a case that broke the fleet fails
// there naming itself; and once a case has left the fleet unsettled, the rest
// are not run on it, and RunFleet says so rather than fail each of them for
// the first one's fault.
//
// A cluster of fewer than three members is refused: two members have no
// quorum without each other.
func RunFleet(t *testing.T, newCluster func(t *testing.T) Cluster) {
	t.Helper()
	c := newCluster(t)
	if err := quorum(c); err != nil {
		t.Fatal(err)
	}
	for _, fc := range fleetCases() {
		broken := false
		t.Run(fc.name, func(t *testing.T) {
			// AFTER THE CASE'S OWN DEFERS, which bring back what it
			// stopped, whether it passed or not — and before its context
			// ends, which its cleanups would run after.
			defer func() {
				if err := settled(t.Context(), c); err != nil {
					t.Errorf("codetest: the fleet after %s: %v", fc.name, err)
					broken = true
				}
			}()
			fc.run(t, c)
		})
		if broken {
			t.Fatalf("codetest: %s left the fleet unsettled, so the cases after it were not run on it",
				fc.name)
		}
	}
}

// quorum refuses a cluster RunFleet cannot run on.
func quorum(c Cluster) error {
	if n := len(c.Members()); n < 3 {
		return fmt.Errorf("codetest: RunFleet needs a cluster of at least three data nodes and was handed "+
			"%d: two members have no quorum without each other, so the first case that kills one "+
			"would leave a code log nobody can write", n)
	}
	return nil
}

// settled is the state RunFleet holds the fleet to between cases: every member
// running and level (Settle), and every cache holding exactly the rows of
// every repository.
func settled(ctx context.Context, c Cluster) error {
	if err := c.Settle(ctx); err != nil {
		return err
	}
	if err := converged(ctx, c); err != nil {
		return fmt.Errorf("the caches are not the rows: %w", err)
	}
	return nil
}

// fleetCase is one case of RunFleet.
type fleetCase struct {
	name string
	run  func(t *testing.T, c Cluster)
}

// fleetCases is RunFleet's cases, in the order they run.
func fleetCases() []fleetCase {
	return []fleetCase{
		{"F12_APushIsVisibleThroughEveryNodeAtOnce", aPushIsVisibleThroughEveryMember},
		{"F13_AFetchFromABehindNodeFindsItsWants", aFetchFromABehindMemberFindsItsWants},
		{"F22_ABloblessCloneChecksOut", aBloblessCloneChecksOutOnAFleet},
		{"F23_AFilterABitmapCannotAnswerIsRefused", aFilterABitmapCannotAnswerIsRefusedOnAFleet},
		{"F4_TwoPushesToOneRefLandOnce", twoPushesThroughTwoMembersLandOnce},
		{"F19_PushesThroughEveryNodeMeetAtOneCommitter", pushesThroughEveryNodeMeetAtOneCommitter},
		{"F27_AHomesDeathFillsOneCopy", aHomesDeathFillsOneCopy},
		{"F1_ACommitterThatDiesAfterAppendingStillAnswersOk", aCommitterThatDiesAfterAppendingStillAnswersOk},
		{"F3_AGateDroppedPushIsNotApplied", aGateDroppedPushIsNotApplied},
		{"F20_AStoppingCommitterHandsItsRepositoriesOn", aStoppingCommitterHandsItsRepositoriesOn},
		{"R7_AnEvictedCommittersRepositoriesAreCommittedByTheirRunnerUp",
			anEvictedCommittersRepositoriesAreCommittedByTheirRunnerUp},
		{"F21_ACrashAtEveryPushStepLeavesATruthfulStateOnAFleet", aCrashAtEveryPushStepOnAFleet},
		{"F28_TwoHomesLandOneCompaction", twoHomesLandOneCompaction},
		{"F29_ACompactionFailsOverToTheRunnerUpCommitter", aCompactionFailsOverToTheRunnerUpCommitter},
		{"F30_ACompactionAnsweredLateIsCollapsed", aCompactionAnsweredLateIsCollapsed},
		{"F31_AChangeOpenSurvivesItsCommitter", aChangeOpenSurvivesItsCommitter},
	}
}

// repoCase is one fleet case's repository: declared `main` protected
// `direct_push: persons`, on a fleet every other case shares.
type repoCase struct {
	t    *testing.T
	c    Cluster
	ctx  context.Context
	name string
	id   string
}

// newRepoCase declares the repository called name on c.
func newRepoCase(t *testing.T, c Cluster, name string) *repoCase {
	t.Helper()
	c.Declare(t, code.Declared{Name: name, Settings: Settings()})
	rc := &repoCase{t: t, c: c, ctx: t.Context(), name: name}
	rc.id = rc.rows().Repo.ID
	return rc
}

// url is the repository's URL through member node — read again at every
// call, because a restarted member listens on a new one.
func (rc *repoCase) url(node string) string {
	rc.t.Helper()
	for _, n := range rc.c.Nodes() {
		if n.ID() == node {
			return n.URL() + "/git/" + rc.name
		}
	}
	rc.t.Fatalf("codetest: member %s serves no git right now (running: %v)", node, rc.c.DataNodes())
	return ""
}

// rows is the repository's rows, read linearizable.
func (rc *repoCase) rows() Rows {
	rc.t.Helper()
	rows, err := rc.c.Rows(rc.ctx, rc.name, Linearizable)
	if err != nil {
		rc.t.Fatalf("the rows of %s: %v", rc.name, err)
	}
	return rows
}

// committer is the member every member routes the repository's pushes to.
func (rc *repoCase) committer() string { return rc.c.Committer(rc.id) }

// other is the first running member that is none of not.
func (rc *repoCase) other(not ...string) string {
	rc.t.Helper()
	for _, n := range rc.c.DataNodes() {
		if !slices.Contains(not, n) {
			return n
		}
	}
	rc.t.Fatalf("codetest: no running member but %v (running: %v)", not, rc.c.DataNodes())
	return ""
}

// mustPush pushes through member node and fails the case unless every ref
// went through.
func (rc *repoCase) mustPush(p *Person, dir, node string, refspecs ...string) Pushed {
	rc.t.Helper()
	res := p.Push(rc.ctx, dir, rc.url(node), refspecs...)
	if res.Err != nil {
		rc.t.Fatalf("git push %v through %s: %s", refspecs, node, res)
	}
	return res
}

// converged fails the case unless every running member's cache holds exactly
// the repository's rows.
func (rc *repoCase) converged() {
	rc.t.Helper()
	if err := convergedOn(rc.ctx, rc.c, rc.name); err != nil {
		rc.t.Errorf("the caches are not the rows of %s: %v", rc.name, err)
	}
}

// pushRows is every push row of the repository, read linearizable.
func (rc *repoCase) pushRows() []code.PushRow {
	rc.t.Helper()
	var rows []code.PushRow
	q := code.ReadQuery{Asker: Reader, Kind: code.ReadPushes, Repo: rc.name, Limit: 200, Fresh: Linearizable}
	for {
		a, err := rc.c.Nodes()[0].Read(rc.ctx, q)
		if err != nil {
			rc.t.Fatalf("read the pushes of %s: %v", rc.name, err)
		}
		rows = append(rows, a.Pushes...)
		if a.Next == "" {
			return rows
		}
		q.Cursor = a.Next
	}
}

// F12: a push through one member is visible through EVERY member at once,
// under both protocols — a member whose copy has not applied it yet waits for
// it before it advertises (the barrier).
func aPushIsVisibleThroughEveryMember(t *testing.T, c Cluster) {
	rc := newRepoCase(t, c, "fleet-f12")
	r := rc.other(rc.committer())
	p := NewPerson(t, c, "ceo")
	work := p.Init()
	p.Commit(work, map[string]string{"README.md": "one\n"}, "one")
	rc.mustPush(p, work, r, "main")
	// EVERY CACHE WARM ON THE FIRST HEAD, so what the second push is checked
	// against is the barrier: a member that had never served the repository
	// would read it whole and pass for the wrong reason.
	for _, n := range c.DataNodes() {
		_ = p.LsRemote(rc.url(n), 0)
	}
	head := p.Commit(work, map[string]string{"README.md": "two\n"}, "two")
	rc.mustPush(p, work, r, "main")
	for _, n := range c.DataNodes() {
		for _, v := range []int{0, 2} {
			if got := p.LsRemote(rc.url(n), v)["refs/heads/main"]; got != head {
				t.Errorf("at once after the push through %s, %s advertises main at %q under protocol %d, "+
					"want %s", r, n, got, v, head)
			}
		}
	}
	rc.converged()
}

// F13: a fetch through a member whose own copy lags the log finds the commit
// the client learned of elsewhere: its router asks a member whose copy does
// not lag, and its cache syncs from that one.
func aFetchFromABehindMemberFindsItsWants(t *testing.T, c Cluster) {
	rc := newRepoCase(t, c, "fleet-f13")
	committer := rc.committer()
	r := rc.other(committer)
	// NEVER THE COMMITTER: a push it decides waits for its own applier.
	b := rc.other(committer, r)
	p := NewPerson(t, c, "ceo")
	work := p.Init()
	p.Commit(work, map[string]string{"README.md": "one\n"}, "one")
	rc.mustPush(p, work, r, "main")
	clone := p.Clone(rc.url(b))
	resume := c.PauseApplier(t, b)
	resumed := false
	defer func() {
		if !resumed {
			resume()
		}
	}()
	head := p.Commit(work, map[string]string{"README.md": "two\n"}, "two")
	rc.mustPush(p, work, r, "main")
	if got := p.LsRemote(rc.url(r), 2)["refs/heads/main"]; got != head {
		t.Fatalf("the pushing member advertises main at %q, want %s", got, head)
	}
	if _, stderr, err := p.Git().Try(t, clone, nil, "fetch", "--quiet", "origin"); err != nil {
		t.Fatalf("a fetch through %s, whose applier is held: %v\n%s", b, err, stderr)
	}
	if got := strings.TrimSpace(p.Git().Run(t, clone, "rev-parse", "origin/main")); got != head {
		t.Errorf("a fetch through %s, whose own copy lags, has main at %s, want %s", b, got, head)
	}
	if _, stderr, err := p.Git().Try(t, clone, nil, "fsck", "--strict"); err != nil {
		t.Errorf("the clone fetched through a lagging member fails fsck: %v\n%s", err, stderr)
	}
	resume()
	resumed = true
	rc.converged()
}

// F22 on a fleet: a blobless clone through one member, its checkout's lazy
// fetch through another — neither of them the committer, so each serves from a
// copy that applied the push rather than decided it.
func aBloblessCloneChecksOutOnAFleet(t *testing.T, c Cluster) {
	rc := newRepoCase(t, c, "fleet-f22")
	committer := rc.committer()
	r := rc.other(committer)
	bloblessCloneChecksOut(t, c, rc.url(r), rc.url(rc.other(committer, r)))
	rc.converged()
}

// F23 on a fleet: a member serves only the filters a bitmap answers.
func aFilterABitmapCannotAnswerIsRefusedOnAFleet(t *testing.T, c Cluster) {
	rc := newRepoCase(t, c, "fleet-f23")
	filtersABitmapCannotAnswerAreRefused(t, c, rc.url(rc.other(rc.committer())))
	rc.converged()
}

// F4: two pushes of one ref from one base, through two DIFFERENT members, land
// once: one ok, one stale — decided at the one committer they both reach.
func twoPushesThroughTwoMembersLandOnce(t *testing.T, c Cluster) {
	rc := newRepoCase(t, c, "fleet-f4")
	committer := rc.committer()
	r1 := rc.other(committer)
	r2 := rc.other(committer, r1)
	a, b := NewPerson(t, c, "ceo"), NewPerson(t, c, "dev")
	workA := a.Init()
	a.Commit(workA, map[string]string{"README.md": "base\n"}, "base")
	rc.mustPush(a, workA, r1, "main")
	workB := b.Clone(rc.url(r2))
	a.Commit(workA, map[string]string{"a.txt": "a\n"}, "from a")
	fromB := b.Commit(workB, map[string]string{"b.txt": "b\n"}, "from b")

	held, reached := make(chan struct{}), make(chan struct{})
	c.Faults().Arm(codefault.HostAfterSync, codefault.Action{Once: true, Block: held, Reached: reached})
	release := sync.OnceFunc(func() { close(held) })
	defer release()
	urlA := rc.url(r1)
	first := make(chan Pushed, 1)
	go func() { first <- a.Push(rc.ctx, workA, urlA, "main") }()
	<-reached
	second := b.Push(rc.ctx, workB, rc.url(r2), "main")
	release()
	resA := <-first
	if !second.OK("refs/heads/main") {
		t.Fatalf("the push through %s that passed the held one through %s: %s", r2, r1, second)
	}
	reason, rejected := resA.Rejected("refs/heads/main")
	if !rejected || !strings.Contains(reason, "(stale: refs/heads/main is at ") || !strings.Contains(reason, "fetch first") {
		t.Errorf("the held push printed %q, want (stale: refs/heads/main is at …; fetch first): %s", reason, resA)
	}
	if got := rc.rows().Refs["refs/heads/main"]; got != fromB {
		t.Errorf("main is at %s, want the push that landed's %s", got, fromB)
	}
	rc.converged()
}

// F19: pushes received by EVERY member meet at the repository's one committer,
// which batches them: the committer's first record is held until every other
// push has uploaded — so each is queued behind it, whichever member received
// it — and then they land in fewer records than pushes, all appended by the
// committer alone. That is the keyed walk's whole point: three receivers, one
// group commit.
func pushesThroughEveryNodeMeetAtOneCommitter(t *testing.T, c Cluster) {
	const perMember = 16
	rc := newRepoCase(t, c, "fleet-f19")
	committer := rc.committer()
	members := c.DataNodes()
	pushers := perMember * len(members)
	p := NewPerson(t, c, "ceo")
	work := p.Init()
	p.Commit(work, map[string]string{"README.md": "base\n"}, "base")
	rc.mustPush(p, work, rc.other(committer), "main")
	urls := make([]string, pushers)
	for i := range pushers {
		p.Git().Run(t, work, "checkout", "--quiet", "-B", fmt.Sprintf("b%02d", i), "main")
		p.Commit(work, map[string]string{fmt.Sprintf("f%02d.txt", i): fmt.Sprintf("%d\n", i)}, fmt.Sprintf("b%02d", i))
		urls[i] = rc.url(members[i%len(members)])
	}
	before := map[string]int{}
	for _, m := range members {
		before[m] = c.Appends(m)
	}
	uploads := c.Faults().Hits(codefault.HostAfterUpload)
	held, reached := make(chan struct{}), make(chan struct{})
	c.Faults().Arm(codefault.CommitterAfterAppend, codefault.Action{Once: true, Block: held, Reached: reached})
	release := sync.OnceFunc(func() { close(held) })
	defer release()
	results := make([]Pushed, pushers)
	var wg sync.WaitGroup
	for i := range pushers {
		wg.Go(func() { results[i] = p.Push(rc.ctx, work, urls[i], fmt.Sprintf("b%02d", i)) })
	}
	<-reached
	// EVERY OTHER PUSH IS PROPOSING, queued behind the held record at the
	// committer, before the record is let go: deterministic, where a race
	// against the committer would commit singletons on a fast machine.
	if !within(time.Minute, func() bool { return c.Faults().Hits(codefault.HostAfterUpload)-uploads == pushers }) {
		t.Errorf("only %d of %d pushes uploaded", c.Faults().Hits(codefault.HostAfterUpload)-uploads, pushers)
	}
	release()
	wg.Wait()
	for i, res := range results {
		if ref := fmt.Sprintf("refs/heads/b%02d", i); !res.OK(ref) {
			t.Errorf("push %d through %s: %s", i, members[i%len(members)], res)
		}
	}
	positions := map[statelog.Position]bool{}
	for _, row := range rc.pushRows() {
		positions[row.Position] = true
	}
	if len(positions) >= pushers+1 {
		t.Errorf("%d pushes landed in %d records: none shared one", pushers, len(positions)-1)
	}
	for _, m := range members {
		grew := c.Appends(m) - before[m]
		switch {
		case m == committer && grew == 0:
			t.Errorf("the committer %s appended none of the %d pushes", m, pushers)
		case m != committer && grew != 0:
			t.Errorf("%s, which is not the repository's committer (%s), appended %d records", m, committer, grew)
		}
	}
	rc.converged()
}

// F1: a committer that dies after appending a push's record, before it answers,
// still has its push answered ok: the receiving member's router hears nothing
// for a whole write attempt, asks the repository's runner-up with the same push
// id, and the runner-up answers from the row the dead committer's record wrote.
func aCommitterThatDiesAfterAppendingStillAnswersOk(t *testing.T, c Cluster) {
	rc := newRepoCase(t, c, "fleet-f1")
	committer := rc.committer()
	r := rc.other(committer)
	p := NewPerson(t, c, "ceo")
	work := p.Init()
	p.Commit(work, map[string]string{"README.md": "one\n"}, "one")
	rc.mustPush(p, work, r, "main")
	head := p.Commit(work, map[string]string{"README.md": "two\n"}, "two")
	held, reached := make(chan struct{}), make(chan struct{})
	c.Faults().Arm(codefault.CommitterAfterAppend, codefault.Action{Once: true, Block: held, Reached: reached})
	defer close(held)
	url := rc.url(r)
	done := make(chan Pushed, 1)
	go func() { done <- p.Push(rc.ctx, work, url, "main") }()
	select {
	case <-reached:
	case res := <-done:
		t.Fatalf("the push ended before its committer appended it: %s", res)
	}
	c.Kill(t, committer)
	restarted := false
	defer func() {
		if !restarted {
			c.Restart(t, committer)
		}
	}()
	if res := <-done; !res.OK("refs/heads/main") {
		t.Errorf("a push whose committer died after appending it: %s, want ok", res)
	}
	if got := rc.rows().Refs["refs/heads/main"]; got != head {
		t.Errorf("main is at %s, want the push's %s", got, head)
	}
	rc.converged()
	c.Restart(t, committer)
	restarted = true
	rc.converged()
}

// F3: a committer evicted by the fleet while its own applier is held has not
// seen its eviction, so it appends — and every applier drops the record, its
// writer evicted earlier in the log. The receiver, told pending, finds the
// push absent at its position on a member that applied it: not_applied, and
// no ref moved. Once the evicted committer has applied its eviction it refuses
// before appending, and the repository's runner-up commits the same push.
func aGateDroppedPushIsNotApplied(t *testing.T, c Cluster) {
	rc := newRepoCase(t, c, "fleet-f3")
	committer := rc.committer()
	r := rc.other(committer)
	p := NewPerson(t, c, "ceo")
	work := p.Init()
	base := p.Commit(work, map[string]string{"README.md": "one\n"}, "one")
	rc.mustPush(p, work, r, "main")
	head := p.Commit(work, map[string]string{"README.md": "two\n"}, "two")
	// HELD PAST THE RECEIVER'S SYNC, so the position the push was checked
	// against — which the committer must reach before it decides — is below
	// the eviction written next.
	held, reached := make(chan struct{}), make(chan struct{})
	c.Faults().Arm(codefault.HostAfterSync, codefault.Action{Once: true, Block: held, Reached: reached})
	release := sync.OnceFunc(func() { close(held) })
	defer release()
	url := rc.url(r)
	done := make(chan Pushed, 1)
	go func() { done <- p.Push(rc.ctx, work, url, "main") }()
	select {
	case <-reached:
	case res := <-done:
		t.Fatalf("the push ended before its sync: %s", res)
	}
	resume := c.PauseApplier(t, committer)
	resumed := false
	defer func() {
		if !resumed {
			resume()
		}
	}()
	c.Evict(t, committer)
	readmitted := false
	defer func() {
		if !readmitted {
			c.Readmit(t, committer)
		}
	}()
	release()
	res := <-done
	reason, rejected := res.Rejected("refs/heads/main")
	if !rejected || !strings.Contains(reason, "(not_applied: the push was durable at ") ||
		!strings.Contains(reason, "applied nowhere; re-run git push") {
		t.Errorf("a push its evicted committer appended printed %q, want (not_applied: the push was durable "+
			"at … and applied nowhere; re-run git push): %s", reason, res)
	}
	if got := rc.rows().Refs["refs/heads/main"]; got != base {
		t.Errorf("main is at %s after a dropped push, want it unmoved at %s", got, base)
	}
	rc.converged()
	resume()
	resumed = true
	again := p.Push(rc.ctx, work, rc.url(r), "main")
	if !again.OK("refs/heads/main") {
		t.Errorf("the push again, its committer now refusing as evicted: %s, want ok from the runner-up", again)
	}
	if got := rc.rows().Refs["refs/heads/main"]; got != head {
		t.Errorf("main is at %s, want %s", got, head)
	}
	c.Readmit(t, committer)
	readmitted = true
	rc.converged()
}

// stopSettle is how long F20 lets a graceful stop run before it lets the held
// record go: long enough that the stop's steps up to the committer's refusal
// have happened — the presence given back, the member's git node stopped with
// nothing running on it, the refusal itself a lock taken and a flag set — and
// a tenth of the stop's own bound on the batch it waits for
// (statelog.DefaultResolveBudget), which a later release would run into. Only
// the case's power to catch a stop in the wrong order rests on it: let go
// sooner, the held record and the pushes queued behind it commit before the
// committer refuses, which passes the right order and the wrong one alike.
const stopSettle = 500 * time.Millisecond

// F20: a committer stopping hands its repositories on. (a) Gracefully: its
// committer refuses before its subject stops answering, so the pushes queued
// at it are answered "not serving" and committed by the runner-up, and the
// batch in flight finishes — every push ok, none waiting out a write attempt
// — where a node that stopped answering on its subject first would fail every
// push it was holding. (b) A committer that dies: its peers still list it —
// a process that dies gives no presence back — so the router of the member a
// push is received by asks it first, hears that nothing answers on its
// subject, reports it to the placement and asks the repository's runner-up:
// the push is ok, at the cost of at most one write attempt.
func aStoppingCommitterHandsItsRepositoriesOn(t *testing.T, c Cluster) {
	rc := newRepoCase(t, c, "fleet-f20")
	committer := rc.committer()
	r := rc.other(committer)
	p := NewPerson(t, c, "ceo")
	work := p.Init()
	p.Commit(work, map[string]string{"README.md": "one\n"}, "one")
	rc.mustPush(p, work, r, "main")
	const pushes = 4
	for i := range pushes {
		p.Git().Run(t, work, "checkout", "--quiet", "-B", fmt.Sprintf("s%d", i), "main")
		p.Commit(work, map[string]string{fmt.Sprintf("s%d.txt", i): "s\n"}, fmt.Sprintf("s%d", i))
	}
	p.Git().Run(t, work, "checkout", "--quiet", "main")

	// (a) The first push's record held at the committer, the others queued
	// behind it, and the committer stopped gracefully under them.
	held, reached := make(chan struct{}), make(chan struct{})
	c.Faults().Arm(codefault.CommitterAfterAppend, codefault.Action{Once: true, Block: held, Reached: reached})
	release := sync.OnceFunc(func() { close(held) })
	defer release()
	url := rc.url(r)
	results := make([]Pushed, pushes)
	took := make([]time.Duration, pushes)
	push := func(i int) {
		start := time.Now()
		results[i] = p.Push(rc.ctx, work, url, fmt.Sprintf("s%d", i))
		took[i] = time.Since(start)
	}
	uploads := c.Faults().Hits(codefault.HostAfterUpload)
	var wg sync.WaitGroup
	wg.Go(func() { push(0) })
	<-reached
	for i := 1; i < pushes; i++ {
		wg.Go(func() { push(i) })
	}
	if !within(time.Minute, func() bool { return c.Faults().Hits(codefault.HostAfterUpload)-uploads == pushes }) {
		t.Errorf("only %d of %d pushes uploaded", c.Faults().Hits(codefault.HostAfterUpload)-uploads, pushes)
	}
	// THE LAST TRIP, from the receiver to the committer's queue, after the
	// upload the count above saw.
	time.Sleep(stopSettle)
	down := true
	defer func() {
		if down {
			c.Restart(t, committer)
		}
	}()
	stopped := make(chan struct{})
	go func() {
		defer close(stopped)
		c.Stop(t, committer)
	}()
	time.Sleep(stopSettle)
	release()
	wg.Wait()
	<-stopped
	for i, res := range results {
		ref := fmt.Sprintf("refs/heads/s%d", i)
		if !res.OK(ref) {
			t.Errorf("push %d while its committer stopped: %s", i, res)
		}
		if limit := c.WriteAttempt() / 2; took[i] > limit {
			t.Errorf("push %d while its committer stopped took %s, past half a write attempt (%s): a "+
				"member went silent rather than refusing", i, took[i], limit)
		}
	}
	c.Restart(t, committer)
	down = false
	rc.converged()
	// THE REFUSALS OF (a) LAPSE, so (b)'s push asks the committer first:
	// every router asks a member that refused a repository's pushes last
	// for half a minute, and a push that went straight to the runner-up
	// would pass (b) without ever meeting the dead committer.
	if err := c.Settle(rc.ctx); err != nil {
		t.Fatalf("settle the fleet once %s is back: %v", committer, err)
	}

	// (b) The committer killed: a push received after it is ok, in at most
	// one write attempt, through a router that met the dead committer.
	head := p.Commit(work, map[string]string{"README.md": "two\n"}, "two")
	reported := c.Unanswered(committer)
	c.Kill(t, committer)
	down = true
	start := time.Now()
	res := p.Push(rc.ctx, work, rc.url(r), "main")
	if !res.OK("refs/heads/main") {
		t.Errorf("a push whose committer died before it: %s, want ok", res)
	}
	if elapsed, limit := time.Since(start), c.WriteAttempt(); elapsed > limit {
		t.Errorf("a push whose committer died before it took %s, past one write attempt (%s)", elapsed, limit)
	}
	if c.Unanswered(committer) == reported {
		t.Errorf("no router reported the dead committer %s unanswered: the push never asked it", committer)
	}
	if got := rc.rows().Refs["refs/heads/main"]; got != head {
		t.Errorf("main is at %s, want %s", got, head)
	}
	c.Restart(t, committer)
	down = false
	rc.converged()
}

// R7: an evicted committer's repositories are committed by their runner-up,
// and once it is readmitted — and the half minute every router remembers a
// refusal has passed — it commits them again.
func anEvictedCommittersRepositoriesAreCommittedByTheirRunnerUp(t *testing.T, c Cluster) {
	rc := newRepoCase(t, c, "fleet-r7")
	committer := rc.committer()
	r := rc.other(committer)
	p := NewPerson(t, c, "ceo")
	work := p.Init()
	p.Commit(work, map[string]string{"README.md": "one\n"}, "one")
	rc.mustPush(p, work, r, "main")
	c.Evict(t, committer)
	readmitted := false
	defer func() {
		if !readmitted {
			c.Readmit(t, committer)
		}
	}()
	evicted := c.Appends(committer)
	head := p.Commit(work, map[string]string{"README.md": "two\n"}, "two")
	if res := p.Push(rc.ctx, work, rc.url(r), "main"); !res.OK("refs/heads/main") {
		t.Errorf("a push while its committer is evicted: %s, want ok from the runner-up", res)
	}
	if got := c.Appends(committer); got != evicted {
		t.Errorf("the evicted committer %s appended %d records", committer, got-evicted)
	}
	if got := rc.rows().Refs["refs/heads/main"]; got != head {
		t.Errorf("main is at %s, want %s", got, head)
	}
	c.Readmit(t, committer)
	readmitted = true
	if err := c.Settle(rc.ctx); err != nil {
		t.Fatalf("settle the fleet once %s is readmitted: %v", committer, err)
	}
	before := c.Appends(committer)
	head = p.Commit(work, map[string]string{"README.md": "three\n"}, "three")
	if res := p.Push(rc.ctx, work, rc.url(r), "main"); !res.OK("refs/heads/main") {
		t.Errorf("a push once its committer is readmitted: %s, want ok", res)
	}
	if c.Appends(committer) == before {
		t.Errorf("the readmitted committer %s appended nothing: its repository's pushes still go to its "+
			"runner-up", committer)
	}
	if got := rc.rows().Refs["refs/heads/main"]; got != head {
		t.Errorf("main is at %s, want %s", got, head)
	}
	rc.converged()
}

// F21: a crash at every step of a push on a fleet — the receiving member at a
// step of its own, the committer at its append — leaves a state the client was
// told the truth about, that every member's cache converges to, that leaves no
// object no row names once the collector has run, and that the same push
// re-run completes. Each point on a repository of its own.
func aCrashAtEveryPushStepOnAFleet(t *testing.T, c Cluster) {
	for i, point := range codefault.PushPath() {
		t.Run(string(point), func(t *testing.T) {
			crashOnAFleet(t, c, fmt.Sprintf("fleet-f21-%d", i), point)
			// THE KILLED MEMBER'S SILENCE LAPSES before the next point,
			// whose repository it may commit for.
			if err := c.Settle(t.Context()); err != nil {
				t.Fatalf("settle the fleet after a crash at %s: %v", point, err)
			}
		})
	}
}

// crashOnAFleet is F21 at one point. A CRASH, never a Block: a member killed
// in this process cancels the hit's context, and a Block would let the step's
// own error path tidy up before the restart — which a process that died never
// does (internal/code/codefault).
func crashOnAFleet(t *testing.T, c Cluster, name string, point codefault.Point) {
	rc := newRepoCase(t, c, name)
	committer := rc.committer()
	r := rc.other(committer)
	p := NewPerson(t, c, "ceo")
	work := p.Init()
	p.Commit(work, map[string]string{"README.md": "one\n"}, "one")
	rc.mustPush(p, work, r, "main")
	head := p.Commit(work, map[string]string{"big.bin": randomFile(64 << 10)}, "two")

	reached := make(chan struct{})
	c.Faults().Arm(point, codefault.Action{Once: true, Crash: true, Reached: reached})
	url := rc.url(r)
	done := make(chan Pushed, 1)
	go func() { done <- p.Push(rc.ctx, work, url, "main") }()
	select {
	case <-reached:
	case res := <-done:
		t.Fatalf("the push ended before it reached %s: %s", point, res)
	}
	killed := r
	if point == codefault.CommitterAfterAppend {
		killed = committer
	}
	c.Kill(t, killed)
	res := <-done
	c.Restart(t, killed)

	if res.OK("refs/heads/main") {
		if at := rc.rows().Refs["refs/heads/main"]; at != head {
			t.Errorf("git printed ok for main at %s and the rows hold %s: %s", head, at, res)
		}
	}
	for _, n := range c.Nodes() {
		for _, sub := range []string{"scratch", "tmp", "trash"} {
			dir := filepath.Join(n.CacheDir(), sub)
			if !within(10*time.Second, func() bool { return emptyDir(dir) }) {
				t.Errorf("%s's %s/ still holds %v after %s's restart", n.ID(), sub, entries(dir), killed)
			}
		}
	}
	rc.converged()
	assertEveryObjectNamed(t, c)

	again := p.Push(rc.ctx, work, rc.url(r), "main")
	if again.Err != nil {
		t.Errorf("the push re-run after a crash at %s: %s", point, again)
	}
	if got := rc.rows().Refs["refs/heads/main"]; got != head {
		t.Errorf("after the re-run main is at %s, want %s", got, head)
	}
	rc.converged()
}

// assertEveryObjectNamed fails t unless, once the collector has run, every
// object the store holds is named by a pack row of SOME repository — the
// fleet's repositories share one store, so one repository's rows name only a
// part of it.
func assertEveryObjectNamed(t *testing.T, f Fleet) {
	t.Helper()
	report := f.Collect(t)
	names, err := f.Repos(t.Context())
	if err != nil {
		t.Fatalf("the repositories: %v", err)
	}
	named := map[string]bool{}
	for _, name := range names {
		rows, err := f.Rows(t.Context(), name, Linearizable)
		if err != nil {
			t.Fatalf("the rows of %s: %v", name, err)
		}
		for _, p := range rows.Packs {
			named[p.Object.Name()] = true
		}
	}
	for _, name := range objectNames(t, f.Objects()) {
		if !named[name] {
			t.Errorf("%s is named by no pack row and the collector left it (%+v)", name, report)
		}
	}
}
