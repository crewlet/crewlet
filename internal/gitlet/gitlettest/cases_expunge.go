package gitlettest

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/gitlet"
	"github.com/crewlet/crewlet/internal/gitlet/compact"
	"github.com/crewlet/crewlet/internal/gitlet/gitletfault"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/statelog/metrics"
)

// THE EXPUNGE CASES (F48–F50): a person's purge of a repository's unreachable
// objects, served by its maintainer's expunge loop (internal/gitlet/compact's
// expunge.go: begin, rescue, commit), raced by a push checked before the
// begin, under a push every 100 ms, and outlived by its retired packs for
// their grace. The purge is asked through Fleet.Expunge, as
// code_purge_unreachable asks one, and every Gitlet node's compactor, built as
// the engine builds it and never started, is driven pass by pass until the
// repository owes nothing. Each case is written once, over an expungeScene, so
// Run and RunFleet run the same body.

// expungePushEvery is F49's push rate: ten a second into one repository, past
// what an agent fleet sustains on code.limits' daily quota, so a cycle a busy
// repository would starve is starved here.
const expungePushEvery = 100 * time.Millisecond

// expungeScene is one expunge case's repository on its fleet. c carries the
// test, fleet and context the compaction cases' helpers take; url, through
// the node receiving the case's pushes, is read at every call, since a
// restarted node listens on a new one.
type expungeScene struct {
	c    *Case
	name string
	id   string
	url  func() string
}

// sceneOf is Run's case c as an expunge scene: RepoName, pushed to the first
// Gitlet node.
func sceneOf(c *Case) *expungeScene {
	c.T.Helper()
	return &expungeScene{c: c, name: RepoName, id: c.Rows().Repo.ID, url: func() string { return c.URL(0) }}
}

// fleetSceneOf is RunFleet's repository rc as an expunge scene, pushed to a
// member other than its committer, so each push is decided on another node
// than the one that checked it.
func fleetSceneOf(rc *repoCase) *expungeScene {
	rc.t.Helper()
	member := rc.other(rc.committer())
	return &expungeScene{c: &Case{T: rc.t, Fleet: rc.c, Ctx: rc.ctx}, name: rc.name, id: rc.id,
		url: func() string { return rc.url(member) }}
}

// rows is the repository's rows, read linearizable.
func (s *expungeScene) rows() Rows {
	s.c.T.Helper()
	rows, err := s.c.Fleet.Rows(s.c.Ctx, s.name, Linearizable)
	if err != nil {
		s.c.T.Fatalf("the rows of %s: %v", s.name, err)
	}
	return rows
}

// expungeRow is a repository's expunge state as its row records it
// (gitlet.ExpungeState) and the drop epoch every begin moves.
type expungeRow struct {
	gitlet.ExpungeState
	epoch int64
}

// state is the repository's expungeRow, read linearizable through the first
// Gitlet node.
func (s *expungeScene) state() expungeRow {
	s.c.T.Helper()
	n := s.c.Fleet.Nodes()[0]
	a, err := n.Read(s.c.Ctx, gitlet.ReadQuery{Asker: Reader, Kind: gitlet.ReadRepo, Repo: s.name, Fresh: Linearizable})
	switch {
	case err != nil:
		s.c.T.Fatalf("the detail of %s through %s: %v", s.name, n.ID(), err)
	case a.Repo == nil:
		s.c.T.Fatalf("the detail of %s through %s answered no repository", s.name, n.ID())
	}
	return expungeRow{ExpungeState: a.Repo.Expunge, epoch: a.Repo.DropEpoch}
}

// mustPush pushes and fails the case unless every ref went through.
func (s *expungeScene) mustPush(p *Person, dir string, refspecs ...string) {
	s.c.T.Helper()
	if res := p.Push(s.c.Ctx, dir, s.url(), refspecs...); res.Err != nil {
		s.c.T.Fatalf("git push %v: %s", refspecs, res)
	}
}

// branchGone pushes a commit on a branch of its own off main and deletes the
// branch, leaving a commit no ref reaches for a purge to drop; it answers the
// commit, the work tree back on main.
func (s *expungeScene) branchGone(p *Person, work, branch string) string {
	s.c.T.Helper()
	p.Git().Run(s.c.T, work, "checkout", "--quiet", "-B", branch, "main")
	oid := p.Commit(work, map[string]string{branch + ".bin": randomFile(1 << 10)}, branch)
	s.mustPush(p, work, branch)
	s.mustPush(p, work, ":refs/heads/"+branch)
	p.Git().Run(s.c.T, work, "checkout", "--quiet", "main")
	return oid
}

// purge asks a person's purge of the repository through the fleet, as
// code_purge_unreachable does, and fails the case unless its record applied.
func (s *expungeScene) purge() gitlet.ExpungeResult {
	s.c.T.Helper()
	res, err := s.c.Fleet.Expunge(s.c.Ctx, gitlet.ExpungeProposal{Request: &gitlet.ExpungeRequest{RepoID: s.id,
		RequestID: statelog.NewOpID(time.Now(), ""), By: personAt("ceo")}})
	if err != nil {
		s.c.T.Fatalf("the purge of %s: %v", s.name, err)
	}
	if res.Outcome != statelog.OutcomeApplied || res.Request == "" {
		s.c.T.Fatalf("the purge of %s answered %+v, want its request applied", s.name, res)
	}
	return res
}

// owed is whether the repository owes an expunge (one open, or a purge
// requested), per the expunge listing through the first Gitlet node, read
// linearizable.
func (s *expungeScene) owed(ctx context.Context) (bool, error) {
	n := s.c.Fleet.Nodes()[0]
	q := gitlet.ReadQuery{Kind: gitlet.ReadExpungeDue, Fresh: Linearizable}
	for {
		a, err := n.Read(ctx, q)
		if err != nil {
			return false, fmt.Errorf("the expunge listing through %s: %w", n.ID(), err)
		}
		if slices.ContainsFunc(a.Expunge, func(cand gitlet.ExpungeCandidate) bool { return cand.ID == s.id }) {
			return true, nil
		}
		if a.Next == "" {
			return false, nil
		}
		q.Cursor = a.Next
	}
}

// tryServed runs every Gitlet node's expunge pass in turn (a node that does
// not maintain the repository skips it) until the repository owes no expunge,
// and answers what the passes did; a refused or failed expunge, a failed
// listing, or a minute without serving the purge is its error. It runs under
// ctx, never the case's test, so a case can run it on a goroutine while a
// fault holds the cycle.
//
// HOW MANY PASSES IT TOOK DECIDES NOTHING: a rescue that meets a busy answer
// leaves its expunge open for the next pass to finish from the rows
// (internal/gitlet/compact's package doc). A case judges what the fleet
// records: the row ([expungeScene.state]) and crewlet.code.expunge.runs
// ([expungesAnswered]).
func (s *expungeScene) tryServed(ctx context.Context) expungeServed {
	var o expungeServed
	deadline := time.Now().Add(time.Minute)
	for {
		for _, n := range s.c.Fleet.Nodes() {
			r, err := n.Compactor().ExpungePass(ctx)
			if err != nil {
				o.err = fmt.Errorf("%s's expunge pass: %w", n.ID(), err)
				return o
			}
			o.did.Listed += r.Listed
			o.did.Maintained += r.Maintained
			o.did.Expunged += r.Expunged
			o.did.Refused += r.Refused
			o.did.Failed += r.Failed
			if r.Refused+r.Failed > 0 {
				o.err = fmt.Errorf("%s's expunge pass refused or failed an expunge: %+v", n.ID(), r)
				return o
			}
		}
		switch owed, err := s.owed(ctx); {
		case err != nil:
			o.err = err
			return o
		case !owed:
			return o
		case time.Now().After(deadline): //fixedwait:measure a poll's own deadline, answered as the error every caller fails on
			o.err = fmt.Errorf("no Gitlet node's pass served the purge of %s in a minute: %+v", s.name, o.did)
			return o
		}
		time.Sleep(20 * time.Millisecond) //fixedwait:liveness
	}
}

// served is tryServed failing the case on its error.
func (s *expungeScene) served() compact.ExpungeReport {
	s.c.T.Helper()
	o := s.tryServed(s.c.Ctx)
	if o.err != nil {
		s.c.T.Fatal(o.err)
	}
	return o.did
}

// servedAsync runs tryServed on a goroutine of its own, its answer on the
// channel it returns.
func (s *expungeScene) servedAsync() <-chan expungeServed {
	done := make(chan expungeServed, 1)
	go func() { done <- s.tryServed(s.c.Ctx) }()
	return done
}

// expungeServed is what tryServed answered.
type expungeServed struct {
	did compact.ExpungeReport
	err error
}

// hold arms point to block once, and answers the channel closed when the
// cycle reaches it and the function that lets it go, also run at the case's
// cleanup so a case that fails early never leaves the cycle held.
func (s *expungeScene) hold(point gitletfault.Point) (reached <-chan struct{}, release func()) {
	s.c.T.Helper()
	held, at := make(chan struct{}), make(chan struct{})
	s.c.Fleet.Faults().Arm(point, gitletfault.Action{Once: true, Block: held, Reached: at})
	release = sync.OnceFunc(func() { close(held) })
	s.c.T.Cleanup(release)
	return at, release
}

// reach waits for the cycle to reach a held point, failing the case if the
// purge was served — or failed — before it.
func (s *expungeScene) reach(reached <-chan struct{}, done <-chan expungeServed, point gitletfault.Point) {
	s.c.T.Helper()
	select {
	case <-reached:
	case o := <-done:
		s.c.T.Fatalf("the purge ended (%+v, %v) before its cycle reached %s", o.did, o.err, point)
	}
}

// dropped fails the case unless no Gitlet node's copy, synced linearizable
// after the commit, holds oid or names a dropping view: what the expunge took
// out is out of every copy.
func (s *expungeScene) dropped(p *Person, oid string) {
	t := s.c.T
	t.Helper()
	for _, n := range s.c.Fleet.Nodes() {
		sy, err := n.Cache().Sync(s.c.Ctx, s.id, Linearizable)
		if err != nil {
			t.Fatalf("%s's sync of %s: %v", n.ID(), s.name, err)
		}
		if sy.Dropping != "" {
			t.Errorf("%s's copy of %s names a dropping view (%s) after the commit", n.ID(), s.name, sy.Dropping)
		}
		if _, _, err := p.Git().Try(t, sy.Dir, nil, "cat-file", "-e", oid); err == nil {
			t.Errorf("%s's copy of %s still holds %s, which the expunge dropped", n.ID(), s.name, oid)
		}
		sy.Release()
	}
}

// expungesAnswered is how many expunge cycles f's Gitlet nodes have answered
// outcome: each compactor's crewlet.code.expunge.runs, counted for its node's
// life.
func expungesAnswered(f Fleet, outcome compact.Outcome) uint64 {
	var total uint64
	for _, n := range f.Nodes() {
		total += counterOf(n, metrics.CodeExpungeRuns, "outcome", string(outcome))
	}
	return total
}

// converged fails the case unless every Gitlet node's copy holds exactly the
// repository's rows.
func (s *expungeScene) converged() {
	s.c.T.Helper()
	if err := convergedOn(s.c.Ctx, s.c.Fleet, s.name); err != nil {
		s.c.T.Errorf("the caches are not the rows of %s: %v", s.name, err)
	}
}

// F48: an expunge raced by a write it must refuse and one it must keep. A PUSH
// CHECKED BEFORE THE BEGIN and decided after it is refused `expunging`, whole,
// naming the epochs: its pack carries its commit alone, on topic's tip (live
// where checked), which topic's deletion under it left to the purge, so landed
// it would name a retired object; run again, it lands, carrying that tip. A
// BRANCH REVIVED by an engine write between the scan and the begin, at a
// commit the scan found unreachable, is kept by the rescue. After the commit
// every copy passes git fsck (I1) and none holds the commit nothing revived.
//
// Mutation: drop the drop-epoch comparison from the push's decide, and the
// held push lands after the rescue read the refs; the commit retires what its
// branch names, and git fsck fails (I1).
func (s *expungeScene) vsAPushCheckedBeforeIt() {
	t := s.c.T
	p := NewPerson(t, s.c.Fleet, "ceo")
	work := p.Init()
	p.Commit(work, map[string]string{"README.md": "base\n"}, "base")
	s.mustPush(p, work, "main")
	p.Git().Run(t, work, "checkout", "--quiet", "-B", "topic", "main")
	topic := p.Commit(work, map[string]string{"topic.bin": randomFile(1 << 10)}, "topic")
	s.mustPush(p, work, "topic")
	gone := s.branchGone(p, work, "gone")
	p.Git().Run(t, work, "checkout", "--quiet", "-B", "onto", topic)
	onto := p.Commit(work, map[string]string{"onto.bin": randomFile(1 << 10)}, "onto")

	// THE PUSH HELD once its hook is admitted: receive-pack has checked its
	// objects against the copy, and the committer has decided nothing.
	admitted, letPush := s.hold(gitletfault.HostHookAdmitted)
	url := s.url()
	pushed := make(chan Pushed, 1)
	go func() { pushed <- p.Push(s.c.Ctx, work, url, "onto") }()
	select {
	case <-admitted:
	case res := <-pushed:
		t.Fatalf("the push ended before its hook was admitted: %s", res)
	}
	dev := NewPerson(t, s.c.Fleet, "dev")
	if res := dev.Push(s.c.Ctx, dev.Init(), url, ":refs/heads/topic"); !res.OK("refs/heads/topic") {
		t.Fatalf("topic's deletion under the held push: %s", res)
	}

	// THE PURGE, held after its build (its scan has read the refs) and after
	// its rescue's upload (its begin has applied, its rescue read the refs).
	s.purge()
	built, letBuild := s.hold(gitletfault.ExpungeAfterBuild)
	rescued, letRescue := s.hold(gitletfault.ExpungeAfterRescueUpload)
	done := s.servedAsync()
	s.reach(built, done, gitletfault.ExpungeAfterBuild)
	s.c.mustLand(gitlet.EngineWrite{Kind: gitlet.WriteCreateBranch, RepoID: s.id, Principal: personAt("ceo"),
		OpID: newOp(t), Fresh: Linearizable, Ref: "refs/heads/revived", Commit: gone})
	letBuild()
	s.reach(rescued, done, gitletfault.ExpungeAfterRescueUpload)
	letPush()
	res := <-pushed
	wantRefused(t, res, "refs/heads/onto", string(gitlet.RefusalExpunging))
	if reason, _ := res.Rejected("refs/heads/onto"); !strings.Contains(reason, "began after this push was checked") ||
		!strings.Contains(reason, "run git push again") {
		t.Errorf("the push checked before the begin printed %q, want the epochs named and git push asked again", reason)
	}
	letRescue()
	if o := <-done; o.err != nil {
		t.Fatalf("the purge: %v", o.err)
	}

	rows := s.rows()
	if got := rows.Refs["refs/heads/onto"]; got != "" {
		t.Errorf("onto is at %s: the push checked before the begin landed after it", got)
	}
	if got := rows.Refs["refs/heads/revived"]; got != gone {
		t.Errorf("revived is at %q, want the revived commit %s", got, gone)
	}
	if rows.Repo.DropEpoch != 1 {
		t.Errorf("after one expunge the repository's drop epoch is %d, want 1", rows.Repo.DropEpoch)
	}
	fsckCaches(s.c, p, s.id)
	s.dropped(p, topic)
	clone := p.Clone(url)
	if got := strings.TrimSpace(p.Git().Run(t, clone, "rev-parse", "origin/revived")); got != gone {
		t.Errorf("a clone has revived at %s, want %s", got, gone)
	}
	p.Git().Run(t, clone, "fsck", "--no-dangling")
	if again := p.Push(s.c.Ctx, work, s.url(), "onto"); !again.OK("refs/heads/onto") {
		t.Errorf("the push run again after the expunge: %s, want ok", again)
	}
	if got := s.rows().Refs["refs/heads/onto"]; got != onto {
		t.Errorf("onto is at %q after the push ran again, want %s", got, onto)
	}
	fsckCaches(s.c, p, s.id)
	s.converged()
}

// F49: an expunge under a push to main every 100 ms is served in ONE CYCLE:
// a begin decides on its inputs and the epoch, a commit on its open expunge
// alone, and no push moves either. Held after its build and its rescue's
// upload, each until two more pushes land, the cycle decides over refs moved
// since it read them. Every push lands (one refused `expunging` runs again)
// and every copy passes git fsck. The row holds the purge's begin while the
// commit is held and its commit after, at one drop epoch: no other expunge
// began. Every node's crewlet.code.expunge.runs{outcome=stale} stays as before
// the purge: a pass's report misses a stale commit the next pass finishes.
//
// Mutations: refuse the commit stale once a push landed since the rescue's
// sync, or keep the row's last commit instant on the commit's apply: red.
func (s *expungeScene) underAPushEvery100ms() {
	t := s.c.T
	p := NewPerson(t, s.c.Fleet, "ceo")
	work := p.Init()
	p.Commit(work, map[string]string{"README.md": "base\n"}, "base")
	s.mustPush(p, work, "main")
	gone := s.branchGone(p, work, "gone")
	pushes := startPushing(s.c.Ctx, p, work, s.url())
	t.Cleanup(pushes.stop)
	// TWO MORE PUSHES LANDED while the cycle is held: the pusher pushes one
	// at a time, so the second was begun, checked and decided while it was.
	awaitPushes := func(at gitletfault.Point) {
		t.Helper()
		from := pushes.now().landed
		if !within(time.Minute, func() bool { return pushes.now().landed >= from+2 }) {
			now := pushes.now()
			t.Fatalf("with the cycle held at %s, %d pushes landed in a minute, want 2 (%s)", at, now.landed-from,
				now.failed)
		}
	}

	staleBefore := expungesAnswered(s.c.Fleet, compact.OutcomeStale)
	purge := s.purge()
	built, letBuild := s.hold(gitletfault.ExpungeAfterBuild)
	rescued, letRescue := s.hold(gitletfault.ExpungeAfterRescueUpload)
	done := s.servedAsync()
	s.reach(built, done, gitletfault.ExpungeAfterBuild)
	awaitPushes(gitletfault.ExpungeAfterBuild)
	letBuild()
	s.reach(rescued, done, gitletfault.ExpungeAfterRescueUpload)
	// THE PURGE'S OWN BEGIN, as the row records it with the commit held.
	begun := s.state()
	if begun.Open == "" || begun.Request != purge.Request || begun.epoch != 1 {
		t.Fatalf("with the commit held, the row holds open expunge %q, request %q and drop epoch %d; want the "+
			"purge's own begun, serving %s, at epoch 1", begun.Open, begun.Request, begun.epoch, purge.Request)
	}
	awaitPushes(gitletfault.ExpungeAfterRescueUpload)
	letRescue()
	o := <-done
	pushes.stop()

	if o.err != nil {
		t.Fatalf("the purge under a push every %s: %v", expungePushEvery, o.err)
	}
	if o.did.Expunged != 1 {
		t.Errorf("the purge under a push every %s took %d cycles, want one: %+v", expungePushEvery, o.did.Expunged,
			o.did)
	}
	if stale := expungesAnswered(s.c.Fleet, compact.OutcomeStale) - staleBefore; stale != 0 {
		t.Errorf("the purge under a push every %s had its begin or its commit refused stale %d times, want each "+
			"decided once over the refs that moved since it read them", expungePushEvery, stale)
	}
	// AND ITS COMMIT, in whichever pass it landed.
	if after := s.state(); after.Open != "" || after.Request != "" || after.ExpungedAt.IsZero() ||
		after.epoch != begun.epoch {
		t.Errorf("after the passes the row holds open expunge %q, request %q, last commit %v and drop epoch %d; "+
			"want %s committed — none open, the purge served, the commit's instant recorded — at epoch %d",
			after.Open, after.Request, after.ExpungedAt, after.epoch, begun.Open, begun.epoch)
	}
	last := pushes.now()
	if last.failed != "" {
		t.Fatalf("a push beside the expunge: %s", last.failed)
	}
	rows := s.rows()
	if got := rows.Refs["refs/heads/main"]; got != last.head {
		t.Errorf("main is at %s, want the last push's %s (%d landed, %d refused expunging and run again)", got,
			last.head, last.landed, last.refused)
	}
	fsckCaches(s.c, p, s.id)
	s.dropped(p, gone)
	s.converged()
}

// mainPusher is F49's pusher: a commit to main every expungePushEvery from a
// goroutine of its own, each pushed until it lands (a refusal `expunging` is
// run again, as it says), any other answer ending it, said.
type mainPusher struct {
	ctx           context.Context
	p             *Person
	work, url     string
	halt, stopped chan struct{}
	stop          func()

	mu  sync.Mutex
	got pushedSoFar
}

// pushedSoFar is what a mainPusher has done: pushes landed, pushes refused
// `expunging` and run again, the last commit landed, and why it ended of its
// own, if it did.
type pushedSoFar struct {
	landed, refused int
	head, failed    string
}

// startPushing starts a mainPusher of work's main to url.
func startPushing(ctx context.Context, p *Person, work, url string) *mainPusher {
	m := &mainPusher{ctx: ctx, p: p, work: work, url: url, halt: make(chan struct{}), stopped: make(chan struct{})}
	m.stop = sync.OnceFunc(func() {
		close(m.halt)
		<-m.stopped
	})
	go m.run()
	return m
}

// now is what the pusher has done so far.
func (m *mainPusher) now() pushedSoFar {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.got
}

func (m *mainPusher) run() {
	defer close(m.stopped)
	tick := time.NewTicker(expungePushEvery) //fixedwait:simulated a busy repository, a push to main every expungePushEvery
	defer tick.Stop()
	for i := 0; ; i++ {
		select {
		case <-m.halt:
			return
		case <-tick.C:
		}
		if !m.push(i) {
			return
		}
	}
}

// push commits the i-th commit and pushes it until it lands, answering
// whether it did.
func (m *mainPusher) push(i int) bool {
	fail := func(words string) bool {
		m.mu.Lock()
		defer m.mu.Unlock()
		m.got.failed = words
		return false
	}
	git := m.p.Git()
	if _, stderr, err := git.TryContext(m.ctx, m.work, nil, "commit", "--quiet", "--allow-empty", "-m",
		fmt.Sprintf("push %d", i)); err != nil {
		return fail(fmt.Sprintf("commit %d: %v\n%s", i, err, stderr))
	}
	out, stderr, err := git.TryContext(m.ctx, m.work, nil, "rev-parse", "HEAD")
	if err != nil {
		return fail(fmt.Sprintf("rev-parse after commit %d: %v\n%s", i, err, stderr))
	}
	oid := strings.TrimSpace(out)
	for {
		res := m.p.Push(m.ctx, m.work, m.url, "main")
		if res.OK("refs/heads/main") {
			m.mu.Lock()
			m.got.landed, m.got.head = m.got.landed+1, oid
			m.mu.Unlock()
			return true
		}
		reason, rejected := res.Rejected("refs/heads/main")
		if !rejected || !strings.Contains(reason, "("+string(gitlet.RefusalExpunging)+": ") {
			return fail(fmt.Sprintf("the push of %s: %s", oid, res))
		}
		m.mu.Lock()
		m.got.refused++
		m.mu.Unlock()
	}
}

// outlives is F50: an expunge's retirements (its begin's inputs and the
// dropping packs its commit retires) keep their objects from the collector
// for gitlet.RetireGrace, as a compaction's inputs do (C4): a retention pass
// 23 h after the commit sweeps none and a collection past
// collect.PendingGrace deletes none while the rows still serve them; once a
// retention pass past the grace sweeps them, the next collection deletes them
// all, the kept packs untouched throughout. The fleet's clocks move for every
// repository it holds, so each count is this repository's own but the first
// pass's, which no repository's rows are old enough to meet.
func (s *expungeScene) outlives() error {
	t := s.c.T
	t.Helper()
	p := NewPerson(t, s.c.Fleet, "ceo")
	work := p.Init()
	for i := range 3 {
		p.Commit(work, map[string]string{fmt.Sprintf("c%d.bin", i): randomFile(1 << 10)}, fmt.Sprintf("push %d", i))
		s.mustPush(p, work, "main")
	}
	s.branchGone(p, work, "gone")
	before := s.rows()
	stored := map[string]bool{}
	for _, n := range objectNames(t, s.c.Fleet.Objects()) {
		stored[n] = true
	}
	s.purge()
	s.served()
	now := time.Now()

	kept := map[string]bool{}
	for _, r := range s.rows().Packs {
		kept[r.Object.Name()] = true
	}
	retired := map[string]bool{}
	for _, r := range before.Packs {
		retired[r.Object.Name()] = true
	}
	dropping := 0
	for _, n := range objectNames(t, s.c.Fleet.Objects()) {
		if !stored[n] && !kept[n] {
			retired[n] = true
			dropping++
		}
	}
	if dropping == 0 {
		return errors.New("the expunge stored no dropping pack: the branch deleted before the purge left the " +
			"case nothing to hold")
	}
	missing := func(want map[string]bool) []string {
		held := map[string]bool{}
		for _, n := range objectNames(t, s.c.Fleet.Objects()) {
			held[n] = true
		}
		var out []string
		for n := range want {
			if !held[n] {
				out = append(out, n)
			}
		}
		return out
	}

	if r := s.c.Fleet.Retain(t, now.Add(23*time.Hour)); r.SweptRows != 0 {
		return fmt.Errorf("a retention pass 23 h after the expunge's commit swept %d retired rows, inside the grace",
			r.SweptRows)
	}
	s.c.Fleet.CollectAt(t, now.Add(24*time.Hour+30*time.Minute))
	if gone := missing(retired); len(gone) > 0 {
		return fmt.Errorf("a collection 24 h 30 min after the expunge's commit deleted %d retired objects inside "+
			"their grace (%v)", len(gone), gone)
	}
	if rows, served := retiredOf(s.c, s.id, before.Position); !served || len(rows) != len(retired) {
		return fmt.Errorf("inside the grace the rows hold %d retirements (served %v), want the %d inputs the "+
			"begin retired and the %d dropping packs the commit did", len(rows), served, len(before.Packs), dropping)
	}
	if gone := missing(kept); len(gone) > 0 {
		return fmt.Errorf("the collection deleted the packs the expunge kept %v", gone)
	}
	if r := s.c.Fleet.Retain(t, now.Add(24*time.Hour+31*time.Minute)); r.SweptRows < len(retired) {
		return fmt.Errorf("a retention pass past the grace swept %d retired rows, want at least the expunge's %d",
			r.SweptRows, len(retired))
	}
	s.c.Fleet.CollectAt(t, now.Add(25*time.Hour+32*time.Minute))
	if gone := missing(retired); len(gone) != len(retired) {
		return fmt.Errorf("once swept, %d retired objects of %d stayed past the collection",
			len(retired)-len(gone), len(retired))
	}
	if gone := missing(kept); len(gone) > 0 {
		return fmt.Errorf("the second collection deleted the packs the expunge kept %v", gone)
	}
	if rows, served := retiredOf(s.c, s.id, before.Position); served && len(rows) > 0 {
		return fmt.Errorf("once swept, the rows still serve %d retirements from below the sweep", len(rows))
	}
	return convergedOn(s.c.Ctx, s.c.Fleet, s.name)
}

// F48 (expungeScene.vsAPushCheckedBeforeIt) on two Gitlet nodes: the push
// received by the first, the expunge run by whichever maintains the
// repository.
func expungeVsAPushCheckedBeforeIt(t *testing.T, newFleet NewFleet) {
	sceneOf(newCase(t, newFleet, Shape{GitletNodes: 2}, Settings())).vsAPushCheckedBeforeIt()
}

// F49 (expungeScene.underAPushEvery100ms) on two Gitlet nodes.
func expungeUnderAPushEvery100ms(t *testing.T, newFleet NewFleet) {
	sceneOf(newCase(t, newFleet, Shape{GitletNodes: 2}, Settings())).underAPushEvery100ms()
}

// ExpungeRetiredPackOutlivesItsLongestRead is F50 (expungeScene.outlives) as
// a check a topology that lies can be run through, nil when the topology
// behaves.
func ExpungeRetiredPackOutlivesItsLongestRead(t *testing.T, newFleet NewFleet) error {
	t.Helper()
	return sceneOf(newCase(t, newFleet, Shape{}, Settings())).outlives()
}

// F50 (ExpungeRetiredPackOutlivesItsLongestRead).
func expungeRetiredPackOutlivesItsLongestRead(t *testing.T, newFleet NewFleet) {
	if err := ExpungeRetiredPackOutlivesItsLongestRead(t, newFleet); err != nil {
		t.Error(err)
	}
}

// F48 on a fleet: the push received by a member that is not the
// repository's committer, the expunge run by whichever member maintains it.
func expungeVsAPushCheckedBeforeItOnAFleet(t *testing.T, c Cluster) {
	fleetSceneOf(newRepoCase(t, c, "fleet-f48")).vsAPushCheckedBeforeIt()
}

// F49 on a fleet.
func expungeUnderAPushEvery100msOnAFleet(t *testing.T, c Cluster) {
	fleetSceneOf(newRepoCase(t, c, "fleet-f49")).underAPushEvery100ms()
}

// F50 on a fleet: LAST of RunFleet's cases, since its passes move the
// retention's and collector's clocks a day on for every repository the shared
// cluster holds.
func expungeRetiredPackOutlivesItsLongestReadOnAFleet(t *testing.T, c Cluster) {
	if err := fleetSceneOf(newRepoCase(t, c, "fleet-f50")).outlives(); err != nil {
		t.Error(err)
	}
}
