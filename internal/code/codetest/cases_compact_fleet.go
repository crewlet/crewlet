package codetest

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/code"
	"github.com/crewlet/crewlet/internal/code/codefault"
	"github.com/crewlet/crewlet/internal/code/compact"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/statelog/metrics"
)

// THE COMPACTION'S FLEET CASES: what only more than one data node can make of
// a compaction — two maintainers, a committer that dies under one, an answer
// that comes back late.

// memberNode is the running member called id as a git node.
func memberNode(t *testing.T, c Cluster, id string) Node {
	t.Helper()
	for _, n := range c.Nodes() {
		if n.ID() == id {
			return n
		}
	}
	t.Fatalf("codetest: member %s serves no git right now", id)
	return nil
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

// heldPass runs a pass of node's compactor on a goroutine, held at
// CompactAfterUpload — its outputs stored, nothing proposed — and answers once
// the hold is reached, with the hold's release and the pass's end.
func heldPass(t *testing.T, c Cluster, n Node) (release func(), done <-chan struct{}) {
	t.Helper()
	held, reached := make(chan struct{}), make(chan struct{})
	c.Faults().Arm(codefault.CompactAfterUpload, codefault.Action{Once: true, Block: held, Reached: reached})
	release = sync.OnceFunc(func() { close(held) })
	ended := make(chan struct{})
	go func() {
		defer close(ended)
		_, _ = n.Compactor().Poll(context.WithoutCancel(t.Context()))
	}()
	select {
	case <-reached:
	case <-ended:
		release()
		t.Fatalf("%s's pass ended before its compaction's uploads did", n.ID())
	case <-time.After(time.Minute):
		release()
		t.Fatalf("%s's pass reached no upload in a minute", n.ID())
	}
	return release, ended
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
		case report.Refused+report.Failed > 0 || time.Now().After(deadline):
			t.Fatalf("%s's pass did not compact the repository: %+v", n.ID(), report)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// F28: two git nodes that each take themselves for a repository's maintainer
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
	rc.pushN(p, work, a, code.CompactMinMerge)
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
// compaction id: the compaction is applied once, and the dead committer, back,
// applies the one record and holds the rows every member does.
func aCompactionFailsOverToTheRunnerUpCommitter(t *testing.T, c Cluster) {
	rc := newRepoCase(t, c, "fleet-f29")
	committer := rc.committer()
	x := rc.other(committer)
	// THE MAINTAINER IS NOT THE COMMITTER, so killing the committer leaves
	// the compaction's builder running.
	c.SplitMaintainers(t, rc.id, x)
	p := NewPerson(t, c, "ceo")
	work := p.Init()
	rc.pushN(p, work, x, code.CompactMinMerge)
	nx := memberNode(t, c, x)
	appliedBefore := outcomes(nx, compact.OutcomeApplied)

	release, done := heldPass(t, c, nx)
	c.Kill(t, committer)
	restarted := false
	defer func() {
		if !restarted {
			c.Restart(t, committer)
		}
	}()
	release()
	<-done
	if got := outcomes(nx, compact.OutcomeApplied) - appliedBefore; got != 1 {
		t.Fatalf("the compaction whose committer died before its proposal was applied %d times, want once "+
			"by the runner-up", got)
	}
	if after := rc.rows(); len(after.Packs) != 1 {
		t.Errorf("the rows hold %d live packs, want the one output", len(after.Packs))
	}
	c.Restart(t, committer)
	restarted = true
	if err := c.Settle(rc.ctx); err != nil {
		t.Fatal(err)
	}
	rc.converged()
}

// F30: a compaction whose committer's applier is paused is answered pending —
// durable, applied nowhere yet — and the same compaction asked again under its
// id once the applier has resumed is collapsed onto the record that landed,
// answered applied from its rows: one record, not two.
func aCompactionAnsweredLateIsCollapsed(t *testing.T, c Cluster) {
	rc := newRepoCase(t, c, "fleet-f30")
	committer := rc.committer()
	x := rc.other(committer)
	c.SplitMaintainers(t, rc.id, x)
	p := NewPerson(t, c, "ceo")
	work := p.Init()
	rc.pushN(p, work, x, code.CompactMinMerge)
	before := rc.rows()
	nx := memberNode(t, c, x)
	pendingBefore := outcomes(nx, compact.OutcomePending)

	resume := c.PauseApplier(t, committer)
	resumed := false
	defer func() {
		if !resumed {
			resume()
		}
	}()
	if _, err := nx.Compactor().Poll(rc.ctx); err != nil {
		t.Fatal(err)
	}
	if got := outcomes(nx, compact.OutcomePending) - pendingBefore; got != 1 {
		t.Fatalf("the compaction whose committer's applier is paused was answered pending %d times, want once",
			got)
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
