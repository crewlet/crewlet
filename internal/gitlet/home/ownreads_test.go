package home_test

import (
	"context"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/crewlet/crewlet/internal/gitlet"
	"github.com/crewlet/crewlet/internal/gitlet/home"
)

// heldReads is an estate whose git reads each wait for the case to let them
// go, counting how many are in flight at once and the most there ever were.
type heldReads struct {
	*estate
	mu       sync.Mutex
	inFlight int
	most     int
	release  chan struct{}
	began    chan struct{}
}

func newHeldReads(e *estate) *heldReads {
	return &heldReads{estate: e, release: make(chan struct{}), began: make(chan struct{}, 64)}
}

func (h *heldReads) GitRead(ctx context.Context, q gitlet.GitQuery) (gitlet.GitAnswer, error) {
	h.mu.Lock()
	h.inFlight++
	h.most = max(h.most, h.inFlight)
	h.mu.Unlock()
	h.began <- struct{}{}
	select {
	case <-h.release:
	case <-ctx.Done():
	}
	h.mu.Lock()
	h.inFlight--
	h.mu.Unlock()
	return h.estate.GitRead(ctx, q)
}

// peak is the most git reads that were in flight at once.
func (h *heldReads) peak() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.most
}

// A HOME LEAVES ITS SEATS A UNIT: its own git reads (a revision's facts) hold
// at most all but one of the node's seat git-work share, at least one (package
// doc, "Its bounds"), so on a one-unit share two turns at once read ONE AT A
// TIME, the second waiting in the home, not ahead of the seats in the share's
// queue. In the bubble, synctest.Wait returns with both turns parked (the
// first in its held read, the second waiting for a read or, unbounded, beside
// it in the estate) and the clock, which holds the loop's passes and bounds,
// unmoved.
//
// Mutations: build the workers over the estate as given, reads unbounded →
// two in flight on a one-unit share, red; OwnReads answering the whole share →
// the table, red.
func TestAHomeLeavesItsSeatsAUnit(t *testing.T) {
	t.Parallel()
	for u, want := range map[int]int{1: 1, 2: 1, 3: 2, 4: 3, 32: 31} {
		if got := home.OwnReads(u); got != want {
			t.Errorf("OwnReads(%d) = %d, want %d: all but one of the share, at least one", u, got, want)
		}
	}

	synctest.Test(t, func(t *testing.T) {
		e := newEstate()
		e.hold(repoA, 1, oid("a"), oid("1"), gitlet.RevisionFactsAnswer{MergeBase: oid("1"), PatchID: oid("e"),
			Touched: []string{"a.go"}})
		e.hold(repoB, 1, oid("b"), oid("2"), gitlet.RevisionFactsAnswer{MergeBase: oid("2"), PatchID: oid("f"),
			Touched: []string{"b.go"}})
		held := newHeldReads(e)
		p := &placement{homes: map[string]string{repoA: "node-a", repoB: "node-a"}}
		loopOver(t, home.Options{Chart: noChart,
			Estate: held, Placement: asNode{placement: p, node: "node-a"}, Nudges: newNudges(),
			Cache: unmerged{t: t}, Git: unmerged{t: t}, Chains: unqueued{t: t}, Tasks: unqueued{t: t},
			NodeID: "node-a", Share: units(1)}, time.Hour)
		synctest.Wait()
		// BOTH TURNS ARE UNDER WAY, each having read its repository's due
		// changes, and one facts read is held.
		if e.countAsked(repoA) == 0 || e.countAsked(repoB) == 0 || len(held.began) == 0 {
			t.Fatalf("with the loop parked the home read repo A's due changes %d times and repo B's %d, and "+
				"began %d facts reads: want both turns under way, one read held", e.countAsked(repoA),
				e.countAsked(repoB), len(held.began))
		}
		if got := held.peak(); got != 1 {
			t.Errorf("%d of the home's facts reads were in flight at once on a share of one unit, want 1", got)
		}
		close(held.release)
		synctest.Wait()
		for _, repo := range []string{repoA, repoB} {
			if left := e.left(repo); left != 0 {
				t.Errorf("repository %s still holds %d due changes once the loop is parked again, want none",
					repo, left)
			}
		}
		if got := held.peak(); got != 1 {
			t.Errorf("%d of the home's facts reads were in flight at once on a share of one unit, want 1", got)
		}
	})
}

// A WORKER THE ENGINE REGISTERS READS THROUGH THE SAME BOUND: its git reads,
// made through the loop ([home.Loop.GitRead]), run one at a time on a share of
// one unit, as the loop's own do, however many of its turns ask at once.
//
// Mutation: answer Loop.GitRead from the estate as it was given, and three
// reads are in flight at once.
func TestARegisteredWorkersGitReadsWaitInTheHomesBound(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		held := newHeldReads(newEstate())
		p := &placement{homes: map[string]string{repoA: "node-a"}}
		l, _ := loopOver(t, home.Options{Chart: noChart,
			Estate: held, Placement: asNode{placement: p, node: "node-a"}, Nudges: newNudges(),
			Cache: unmerged{t: t}, Git: unmerged{t: t}, Chains: unqueued{t: t}, Tasks: unqueued{t: t},
			NodeID: "node-a", Share: units(1)}, time.Hour)
		synctest.Wait()
		q := gitlet.GitQuery{Kind: gitlet.GitRevisionFacts, RepoID: repoA,
			RevisionFacts: &gitlet.RevisionFactsQuery{Head: oid("a"), Target: oid("1")}}
		var wg sync.WaitGroup
		for range 3 {
			wg.Go(func() { _, _ = l.GitRead(t.Context(), q) })
		}
		synctest.Wait()
		if got := held.peak(); got != 1 {
			t.Errorf("%d of a registered worker's reads were in flight at once on a share of one unit, want 1", got)
		}
		close(held.release)
		wg.Wait()
		if got := held.peak(); got != 1 {
			t.Errorf("%d of a registered worker's reads were in flight at once on a share of one unit, want 1", got)
		}
	})
}

// left is how many changes repo's due list and its rechecks hold.
func (e *estate) left(repo string) int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return len(e.due[repo]) + len(e.rechecks[repo])
}

// countAsked is how many times the loop has read repo's due changes.
func (e *estate) countAsked(repo string) int {
	e.mu.Lock()
	defer e.mu.Unlock()
	n := 0
	for _, q := range e.asked {
		if q.RepoID == repo {
			n++
		}
	}
	return n
}
