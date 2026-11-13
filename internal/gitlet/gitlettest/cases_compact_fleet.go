package gitlettest

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/gitlet"
	"github.com/crewlet/crewlet/internal/gitlet/compact"
	"github.com/crewlet/crewlet/internal/gitlet/gitletfault"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/statelog/metrics"
)

// THE COMPACTION'S FLEET CASES: what only more than one data node can make of
// a compaction — two maintainers, a committer that dies under one, an answer
// that comes back late.
//
// EACH DRIVES WHOLE PASSES, and judges them by what a member's compactor
// counted and what the store holds since the case began, because a pass meets
// the case's own repository alone: RunFleet confines every case's maintenance
// to the repositories it made (Cluster.ConfineMaintenance), and the cluster
// holds others due throughout, an earlier case's among them.
//
// AND A PASS MAY FIND THE MAINTENANCE LANE BUSY, which the compactor answers
// busy and leaves to its next pass. The lane is the member's own maintainer's
// as much as its compactions' (cache.Cache.AdmitCompaction): its tick indexes
// a copy at whatever moment it falls, a copy the case just pushed to among
// them, and a compaction admitted beside that run is refused at once. So a
// case that needs a pass to compact takes passes until one does
// (passUntilCompacted, heldPass), as the compactor's loop would, and judges
// what that one was answered — never the first pass alone (#952).

// memberNode is the running member called id as a Gitlet node.
func memberNode(t *testing.T, c Cluster, id string) Node {
	t.Helper()
	for _, n := range c.Nodes() {
		if n.ID() == id {
			return n
		}
	}
	t.Fatalf("gitlettest: member %s serves no git right now", id)
	return nil
}

// fleetAppends is how many records every member's committer has appended
// (Cluster.Appends), the members down included: a dead member's count is what
// it appended before it died.
func fleetAppends(c Cluster) int {
	n := 0
	for _, m := range c.Members() {
		n += c.Appends(m)
	}
	return n
}

// pushN makes n pushes to main through member node, each one commit adding a
// small file.
func (rc *repoCase) pushN(p *Person, work, node string, n int) {
	rc.t.Helper()
	for i := range n {
		p.Commit(work, map[string]string{"c" + string(rune('a'+i)) + ".bin": randomFile(1 << 10)}, "push")
		rc.mustPush(p, work, node, "main")
	}
}

// outcomes is how many compactions node n's compactor answered outcome.
func outcomes(n Node, outcome compact.Outcome) uint64 {
	return counterOf(n, metrics.CodeCompactRuns, "outcome", string(outcome))
}

// landing is how many compactions a member's compactor answered with their
// record LANDED, by what each was answered: applied, or pending or unknown —
// the record appended and not yet known applied where it was decided. A pass
// counts the three alike as compacted (compact.Compactor.Poll), because which
// of them a compaction is answered says how far the deciding member's applier
// had got, not whether the compaction landed.
type landing struct{ applied, pending, unknown uint64 }

// landingOn is what node n's compactor has answered so far.
func landingOn(n Node) landing {
	return landing{applied: outcomes(n, compact.OutcomeApplied), pending: outcomes(n, compact.OutcomePending),
		unknown: outcomes(n, compact.OutcomeUnknown)}
}

// since is what l counted past before.
func (l landing) since(before landing) landing {
	return landing{applied: l.applied - before.applied, pending: l.pending - before.pending,
		unknown: l.unknown - before.unknown}
}

// landed is how many compactions landed, whatever each was answered.
func (l landing) landed() uint64 { return l.applied + l.pending + l.unknown }

// heldPass runs passes of node's compactor on a goroutine until one is held at
// CompactAfterUpload — its outputs stored, nothing proposed — while each before
// it is answered busy (the file's head), and answers once the hold is
// reached, with the hold's release and the held pass's end. A pass that ends
// any other way before the hold fails the case, with what it did.
func heldPass(t *testing.T, c Cluster, n Node) (release func(), done <-chan struct{}) {
	t.Helper()
	held, reached := make(chan struct{}), make(chan struct{})
	c.Faults().Arm(gitletfault.CompactAfterUpload, gitletfault.Action{Once: true, Block: held, Reached: reached})
	release = sync.OnceFunc(func() { close(held) })
	ended, stop := make(chan struct{}), make(chan struct{})
	var last compact.PollReport
	go func() {
		defer close(ended)
		for {
			busy := outcomes(n, compact.OutcomeBusy)
			report, err := n.Compactor().Poll(context.WithoutCancel(t.Context()))
			last = report
			select {
			case <-reached:
				// THE HELD PASS, released: it ends the passes, whatever it
				// was answered.
				return
			default:
			}
			if err != nil || !turnedAwayBusy(n, busy, report) {
				return
			}
			select {
			case <-stop:
				return
			case <-time.After(20 * time.Millisecond): //fixedwait:liveness
			}
		}
	}()
	select {
	case <-reached:
	case <-ended:
		release()
		t.Fatalf("%s's pass ended before its compaction's uploads did: %+v", n.ID(), last)
	case <-time.After(time.Minute): //fixedwait:liveness
		close(stop)
		release()
		t.Fatalf("%s's pass reached no upload in a minute", n.ID())
	}
	return release, ended
}

// turnedAwayBusy reports whether a pass compacted nothing only because its
// compaction was answered busy: n's count of busy answers moved past busy,
// and the pass compacted, refused and failed nothing.
func turnedAwayBusy(n Node, busy uint64, r compact.PollReport) bool {
	return outcomes(n, compact.OutcomeBusy) > busy && r.Compacted+r.Refused+r.Failed == 0
}

// passUntilCompacted is passes of node's compactor until one compacted the
// repository, while it finds the maintenance lane busy — a minute at most.
func passUntilCompacted(t *testing.T, n Node) {
	t.Helper()
	deadline := time.Now().Add(time.Minute)
	for {
		report, err := n.Compactor().Poll(t.Context())
		switch {
		case err != nil:
			t.Fatalf("%s's pass: %v", n.ID(), err)
		case report.Compacted > 0:
			return
		case report.Refused+report.Failed > 0 || time.Now().After(deadline): //fixedwait:measure a poll's own deadline, whose firing only fails the case
			t.Fatalf("%s's pass did not compact the repository: %+v", n.ID(), report)
		}
		time.Sleep(20 * time.Millisecond) //fixedwait:liveness
	}
}

// F28: two Gitlet nodes that each take themselves for a repository's maintainer
// — a presence view split for a moment, which no lease decides — both build a
// compaction from the same rows, and the committer lands one: the other is
// refused stale, its outputs named by no row, which the collector deletes.
func twoMaintainersLandOneCompaction(t *testing.T, c Cluster) {
	rc := newRepoCase(t, c, "fleet-f28")
	committer := rc.committer()
	a := rc.other(committer)
	b := rc.other(committer, a)
	c.SplitMaintainers(t, rc.id, a, b)
	p := NewPerson(t, c, "ceo")
	work := p.Init()
	rc.pushN(p, work, a, gitlet.CompactMinMerge)
	before := rc.rows()
	na, nb := memberNode(t, c, a), memberNode(t, c, b)
	staleBefore := outcomes(na, compact.OutcomeStale)
	storedBefore := map[string]bool{}
	for _, n := range objectNames(t, c.Objects()) {
		storedBefore[n] = true
	}

	release, aDone := heldPass(t, c, na)
	passUntilCompacted(t, nb)
	release()
	<-aDone
	if got := outcomes(na, compact.OutcomeStale) - staleBefore; got != 1 {
		t.Errorf("%s's compaction, built from the rows %s's landed over, was answered stale %d times, want once",
			a, b, got)
	}
	after := rc.rows()
	if len(after.Packs) != 1 {
		t.Fatalf("two maintainers left %d live packs, want one compaction's one output", len(after.Packs))
	}
	retired := retiredSince(&Case{T: t, Fleet: c, Ctx: rc.ctx}, rc.id, before.Position)
	if len(retired) != len(before.Packs) {
		t.Errorf("%d retirements, want one compaction's %d", len(retired), len(before.Packs))
	}
	for _, r := range retired[1:] {
		if r.Position != retired[0].Position {
			t.Errorf("the retirements landed at %s and %s: two compaction records", retired[0].Position, r.Position)
			break
		}
	}
	// THE LOSER'S OUTPUTS ARE THE COLLECTOR'S: stored by the two passes and
	// named by no row, they are deleted at its first pass past their grace,
	// while the winner's output and the retired inputs, which rows name, stay.
	output := after.Packs[0].Object.Name()
	var losers []string
	for _, n := range objectNames(t, c.Objects()) {
		if !storedBefore[n] && n != output {
			losers = append(losers, n)
		}
	}
	if len(losers) == 0 {
		t.Errorf("%s's stale compaction stored no output: the case did not build two", a)
	}
	c.CollectAt(t, time.Now().Add(25*time.Hour))
	stored := map[string]bool{}
	for _, n := range objectNames(t, c.Objects()) {
		stored[n] = true
	}
	for _, n := range losers {
		if stored[n] {
			t.Errorf("the stale compaction's output %s, named by no row, outlived the collection", n)
		}
	}
	if !stored[output] {
		t.Errorf("the collection deleted the applied compaction's output %s", output)
	}
	for _, r := range before.Packs {
		if !stored[r.Object.Name()] {
			t.Errorf("the collection deleted %s, retired inside its grace", r.Object.Name())
		}
	}
	rc.converged()
}

// F29: a committer that dies after a compaction's outputs are stored and
// before its proposal is asked of its repository's runner-up, under the same
// compaction id: the compaction LANDS ONCE — one record, the runner-up's, the
// only one the fleet appends, which retires every input and adds the one
// output on every member — and the dead committer, back, applies that record
// and holds the rows every member does.
//
// LANDED, NOT APPLIED. A pass counts a compaction compacted once its record
// landed, whatever the member that decided it answered ([landing]): applied
// where that member's own applier reached the record in time, pending or
// unknown where it had not. The leader change the kill causes stalls the
// survivors' appliers for as long as it lasts, and once that outlasted the
// five seconds the runner-up waits for its own record
// (statelog.DefaultResolveBudget) the answer was pending, so a case that
// counted applied alone read a failover that worked as one that never
// happened (#1130). So the case makes that stall CERTAIN — every survivor's
// applier held from before the kill until the pass has returned, so the
// record cannot apply where it was decided and the runner-up answers pending
// (or unknown), never applied — and it reads what the record did only once
// every running member has applied it, waiting on each as the case's end does
// (Fleet.Level, bounded by the case's own context alone).
//
// PROPOSED ONCE THE KILL'S ELECTION IS OVER. The runner-up's admission reads
// the code log's usage from its own broker before it appends
// (statelog.Reserve), and a broker the kill left with no meta leader answers
// that read 503, err_code 10008: the runner-up refuses the compaction
// `usage_unknown`, and its asker takes it to the member after it, which
// decides it — a record that is not the runner-up's — or refuses it too, and
// the pass counts it failed, landed nowhere. So the held pass is let go only
// once every survivor's own broker answers that read
// (Cluster.AwaitElection), bounded by the case's own context alone as the
// wait for the record is: what the case decides never turns on how soon an
// election ends on a loaded runner.
//
// Mutation: count applied alone and the case fails on every run, where main's
// failed on a run whose election outlasted the pass.
func aCompactionFailsOverToTheRunnerUpCommitter(t *testing.T, c Cluster) {
	rc := newRepoCase(t, c, "fleet-f29")
	committer := rc.committer()
	x := rc.other(committer)
	survivors := []string{x, rc.other(committer, x)}
	// THE MAINTAINER IS NOT THE COMMITTER, so killing the committer leaves
	// the compaction's builder running.
	c.SplitMaintainers(t, rc.id, x)
	p := NewPerson(t, c, "ceo")
	work := p.Init()
	rc.pushN(p, work, x, gitlet.CompactMinMerge)
	before := rc.rows()
	nx := memberNode(t, c, x)
	landedBefore := landingOn(nx)

	release, done := heldPass(t, c, nx)
	// THE HELD PASS ENDS WITH THE CASE, however the case ends: held, it keeps
	// its member's compactor — one compaction at a time a node — from every
	// later case's pass. Let go last, once whatever the case stopped is back,
	// and waited for.
	defer func() {
		release()
		<-done
	}()
	down := false
	defer func() {
		if down {
			c.Restart(t, committer)
		}
	}()
	var held []func()
	resume := func() {
		for _, r := range held {
			r()
		}
		held = nil
	}
	defer resume()
	// EVERY SURVIVOR'S APPLIER HELD BEFORE THE KILL: each is levelled with
	// the log first (Cluster.PauseApplier), which after the kill would wait
	// on the very leader change this hold makes certain.
	for _, s := range survivors {
		held = append(held, c.PauseApplier(t, s))
	}
	appendedBefore := fleetAppends(c)
	c.Kill(t, committer)
	down = true
	runnerUp := rc.committer()
	// THE PROPOSAL WAITS FOR THE KILL'S ELECTION: asked while it runs, the
	// runner-up's admission cannot read the log's usage, and the compaction
	// lands by the member after it, or nowhere. Never Fleet.Level, which
	// would wait on the appliers held above.
	if err := c.AwaitElection(rc.ctx); err != nil {
		t.Fatalf("the survivors' brokers after %s's death: %v", committer, err)
	}
	release()
	<-done
	if got := landingOn(nx).since(landedBefore); got.landed() != 1 {
		t.Fatalf("the compaction whose committer died before its proposal landed %d times (applied %d, "+
			"pending %d, unknown %d), want once, by the runner-up %s", got.landed(), got.applied, got.pending,
			got.unknown, runnerUp)
	}
	if n := fleetAppends(c) - appendedBefore; n != 1 {
		t.Errorf("the fleet's committers appended %d records for one compaction, want one", n)
	}

	// EVERY RUNNING MEMBER HOLDS THE RECORD before anything reads what it
	// did: the survivors let go, and each level with the code log.
	resume()
	for _, m := range c.DataNodes() {
		if err := c.Level(rc.ctx, m); err != nil {
			t.Fatalf("%s applying the compaction's record: %v", m, err)
		}
	}
	retired := retiredSince(&Case{T: t, Fleet: c, Ctx: rc.ctx}, rc.id, before.Position)
	if len(retired) != len(before.Packs) {
		t.Fatalf("%d retirements, want one compaction's %d", len(retired), len(before.Packs))
	}
	at := retired[0].Position
	for _, r := range retired[1:] {
		if r.Position != at {
			t.Errorf("the retirements landed at %s and %s: two compaction records", at, r.Position)
			break
		}
	}
	switch writer, err := c.RecordWriter(rc.ctx, at); {
	case err != nil:
		t.Errorf("the writer of the compaction's record at %s: %v", at, err)
	case writer != runnerUp:
		t.Errorf("the compaction's record at %s is %s's, want the runner-up %s's", at, writer, runnerUp)
	}
	switch after := rc.rows(); {
	case len(after.Packs) != 1:
		t.Errorf("the rows hold %d live packs, want the one output", len(after.Packs))
	case after.Packs[0].Added != at:
		t.Errorf("the one live pack was added at %s, and the compaction's record is at %s",
			after.Packs[0].Added, at)
	}
	c.Restart(t, committer)
	down = false
	if err := c.Settle(rc.ctx); err != nil {
		t.Fatal(err)
	}
	rc.converged()
}

// F30: a compaction whose committer's applier is paused is answered pending —
// durable, applied nowhere yet — and the same compaction asked again under its
// id once the applier has resumed is collapsed onto the record that landed,
// answered applied from its rows: one record, not two. The pass is the first
// that compacts, after any the maintenance lane turned away busy (the file's
// head).
func aCompactionAnsweredLateIsCollapsed(t *testing.T, c Cluster) {
	rc := newRepoCase(t, c, "fleet-f30")
	committer := rc.committer()
	x := rc.other(committer)
	c.SplitMaintainers(t, rc.id, x)
	p := NewPerson(t, c, "ceo")
	work := p.Init()
	rc.pushN(p, work, x, gitlet.CompactMinMerge)
	before := rc.rows()
	nx := memberNode(t, c, x)
	pendingBefore := outcomes(nx, compact.OutcomePending)
	appliedBefore := outcomes(nx, compact.OutcomeApplied)

	resume := c.PauseApplier(t, committer)
	resumed := false
	defer func() {
		if !resumed {
			resume()
		}
	}()
	passUntilCompacted(t, nx)
	if got := outcomes(nx, compact.OutcomePending) - pendingBefore; got != 1 {
		t.Fatalf("the compaction whose committer's applier is paused was answered pending %d times, want once "+
			"(and applied %d times)", got, outcomes(nx, compact.OutcomeApplied)-appliedBefore)
	}
	resume()
	resumed = true
	res, err := c.ProposeCompactionAgain(rc.ctx, x, rc.id)
	if err != nil || res.Outcome != statelog.OutcomeApplied {
		t.Fatalf("the same compaction asked again once applied: %+v, %v; want applied", res, err)
	}
	after := rc.rows()
	if len(after.Packs) != 1 {
		t.Errorf("the rows hold %d live packs, want one compaction's output", len(after.Packs))
	}
	retired := retiredSince(&Case{T: t, Fleet: c, Ctx: rc.ctx}, rc.id, before.Position)
	if len(retired) != len(before.Packs) || (len(retired) > 0 && retired[0].Position != res.Position) {
		t.Errorf("the compaction asked twice retired %d rows at %v, want %d at its one record's %s",
			len(retired), retired, len(before.Packs), res.Position)
	}
	rc.converged()
}
