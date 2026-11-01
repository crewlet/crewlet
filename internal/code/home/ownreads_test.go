package home_test

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/code"
	"github.com/crewlet/crewlet/internal/code/home"
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

func (h *heldReads) GitRead(ctx context.Context, q code.GitQuery) (code.GitAnswer, error) {
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

// A HOME LEAVES ITS SEATS A UNIT: its own git reads — a revision's facts —
// run at most all but one of the node's seat git-work share at once, at least
// one, because each holds a unit of the share a seat's read waits for at most
// two seconds before it is sent to a git node that holds no copy. On a node
// whose share is one unit, two repositories' turns at once — Concurrency is
// two — read ONE AT A TIME, the second turn waiting in the home rather than
// ahead of the seats in the share's own queue; and OwnReads is all but one of
// a wider share.
//
// Mutations: build the loop's workers over the estate as given, its reads
// unbounded — two in flight on a one-unit share, red; OwnReads answering the
// whole share — the table, red.
func TestAHomeLeavesItsSeatsAUnit(t *testing.T) {
	t.Parallel()
	for u, want := range map[int]int{1: 1, 2: 1, 3: 2, 4: 3, 32: 31} {
		if got := home.OwnReads(u); got != want {
			t.Errorf("OwnReads(%d) = %d, want %d: all but one of the share, at least one", u, got, want)
		}
	}

	e := newEstate()
	e.hold(repoA, 1, oid("a"), oid("1"), code.RevisionFactsAnswer{MergeBase: oid("1"), PatchID: oid("e"),
		Touched: []string{"a.go"}})
	e.hold(repoB, 1, oid("b"), oid("2"), code.RevisionFactsAnswer{MergeBase: oid("2"), PatchID: oid("f"),
		Touched: []string{"b.go"}})
	held := newHeldReads(e)
	p := &placement{homes: map[string]string{repoA: "node-a", repoB: "node-a"}}
	loopOver(t, home.Options{Estate: held, Placement: asNode{placement: p, node: "node-a"}, Nudges: newNudges(),
		Cache: unmerged{t: t}, Git: unmerged{t: t}, Chains: unqueued{t: t}, Tasks: unqueued{t: t}, NodeID: "node-a",
		Share: units(1)}, time.Hour)
	select {
	case <-held.began:
	case <-time.After(10 * time.Second):
		t.Fatal("the home read no facts in 10s")
	}
	// BOTH TURNS ARE UNDER WAY once both repositories' due changes are read;
	// the second's facts read, unbounded, would be in flight within moments.
	deadline := time.Now().Add(10 * time.Second)
	for e.countAsked(repoA) == 0 || e.countAsked(repoB) == 0 {
		if time.Now().After(deadline) {
			t.Fatal("the home did not work both repositories at once in 10s")
		}
		time.Sleep(5 * time.Millisecond)
	}
	time.Sleep(100 * time.Millisecond)
	if got := held.peak(); got != 1 {
		t.Errorf("%d of the home's facts reads were in flight at once on a share of one unit, want 1", got)
	}
	close(held.release)
	e.waitDone(t, repoA, 10*time.Second)
	e.waitDone(t, repoB, 10*time.Second)
	if got := held.peak(); got != 1 {
		t.Errorf("%d of the home's facts reads were in flight at once on a share of one unit, want 1", got)
	}
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
