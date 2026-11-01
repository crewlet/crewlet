package codetest

import (
	"slices"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/code"
	"github.com/crewlet/crewlet/internal/code/codefault"
	"github.com/crewlet/crewlet/internal/rendezvous"
	"github.com/crewlet/crewlet/internal/statelog/metrics"
)

// The reads of a repository by object id through a fleet's routing — asked of
// the repository's home git node (ADR-0059) — as a seat's tool asks them.

// homeWait is how long a case waits for a read through the fleet's routing to
// be answered by the node it expects: a home asked to fill a copy answers
// `syncing` until its download lands, and the walk meanwhile asks the other
// git nodes, so the case asks again until the copy is warm. A minute is far
// past a small repository's cold sync on a loaded runner (a second or two)
// and well inside a case's own deadline.
const homeWait = time.Minute

// readsServed is how many reads node's query server has run its git for: every
// unit of its seat git-work share a read took (metrics.CodeSeatworkWaits),
// which only a read that found its objects takes — never one answered unready
// or refused.
func readsServed(n Node) uint64 {
	var total uint64
	for _, s := range n.Metrics().Read() {
		if s.Name == metrics.CodeSeatworkWaits {
			total += s.Count
		}
	}
	return total
}

// treeRead is the root listing of repo at rev, as a person reads it.
func treeRead(repo, rev string) code.GitQuery {
	return code.GitQuery{Kind: code.GitTree, RepoID: repo, Asker: Reader, Tree: &code.TreeQuery{Rev: rev}}
}

// lists reports whether a read answered a root listing naming path.
func lists(a code.GitAnswer, path string) bool {
	return a.Tree != nil && slices.ContainsFunc(a.Tree.Entries, func(e code.TreeEntry) bool { return e.Path == path })
}

// F26: A READ IS ANSWERED BY THE HOME (ADR-0059). A read of a repository by
// object id through the fleet's routing is asked of the repository's home git
// node — the first of rendezvous.Order(repository, every git node) — and once
// the home's copy is warm it is the node that answers: a fleet of three git
// nodes, a push through the first, and the home whichever node the order puts
// first. Meanwhile the walk may be answered by the node the push filled, which
// holds a copy whatever its place; and the third node — neither the home nor
// the pusher — holds no copy after, since only the walk's effective home is
// ever asked to fill one.
func aReadIsAnsweredByTheHome(t *testing.T, newFleet NewFleet) {
	c := newCase(t, newFleet, Shape{GitNodes: 3}, Settings())
	p := NewPerson(t, c.Fleet, "ceo")
	work := p.Init()
	head := p.Commit(work, map[string]string{"README.md": "home\n"}, "home")
	c.MustPush(p, work, "main")
	repo := c.Rows().Repo.ID
	nodes := c.Fleet.Nodes()
	ids := make([]string, len(nodes))
	byID := map[string]Node{}
	for i, n := range nodes {
		ids[i] = n.ID()
		byID[n.ID()] = n
	}
	home := byID[rendezvous.Order(repo, ids)[0]]
	pusher := nodes[0]

	deadline := time.Now().Add(homeWait)
	for {
		before := readsServed(home)
		a, err := c.Fleet.GitRead(c.Ctx, treeRead(repo, head))
		if err == nil && !lists(a, "README.md") {
			t.Fatalf("a read through the fleet answered %+v, want the root listing README.md", a.Tree)
		}
		if err == nil && readsServed(home) > before {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("no read through the fleet was answered by the repository's home %s within %s "+
				"(the last: %v)", home.ID(), homeWait, err)
		}
		time.Sleep(50 * time.Millisecond)
	}
	for _, n := range nodes {
		if n.ID() != home.ID() && n.ID() != pusher.ID() && n.Cache().Holds(repo) {
			t.Errorf("%s, neither the repository's home %s nor the node its push went through, holds a "+
				"copy: a node a walk did not ask to fill started a download", n.ID(), home.ID())
		}
	}
	assertConverged(t, c.Fleet)
}

// F27: A HOME'S DEATH FILLS ONE COPY (ADR-0059). A repository only its home
// holds — pushed through that member alone — is read, once the home is
// KILLED, through each other member until one serves it. The walks that meet
// the dead home ask its runner-up and the third member with their reads not
// stamped home, and neither starts a download for a read it was not asked to
// fill; once a router asks the silent home last, the runner-up is its walk's
// first node, stamped, and fills. So exactly one other member holds the
// repository after — its runner-up — where every member a walk reached once
// started a whole download of it, for a read one node answers.
//
// THE DOWNLOADS ARE HELD while the runner-up fills, so its copy answers
// `syncing` and every walk goes on past it to the third member: a small
// repository's cold sync is faster than a read's two-second wait, so unheld,
// the runner-up would answer every walk itself and the third would never be
// asked a read it could wrongly fill.
func aHomesDeathFillsOneCopy(t *testing.T, c Cluster) {
	rc := newRepoCase(t, c, "home-death")
	order := rendezvous.Order(rc.id, c.DataNodes())
	home, runnerUp, third := order[0], order[1], order[2]
	p := NewPerson(t, c, "ceo")
	work := p.Init()
	head := p.Commit(work, map[string]string{"README.md": "home\n"}, "home")
	if res := p.Push(rc.ctx, work, rc.url(home), "main"); res.Err != nil {
		t.Fatalf("the push through the home %s: %s", home, res)
	}
	node := func(id string) Node {
		for _, n := range c.Nodes() {
			if n.ID() == id {
				return n
			}
		}
		t.Fatalf("member %s serves no git", id)
		return nil
	}
	for _, id := range []string{runnerUp, third} {
		if node(id).Cache().Holds(rc.id) {
			t.Fatalf("the premise: %s holds a copy of a repository pushed through %s alone", id, home)
		}
	}

	c.Kill(t, home)
	defer c.Restart(t, home)
	// RELEASED BEFORE THE RESTART, which the deferred calls run after, and
	// on every way out: a held download holds its round for the cache's
	// lifetime.
	held := make(chan struct{})
	c.Faults().Arm(codefault.SyncDownload, codefault.Action{Block: held})
	released := false
	release := func() {
		if !released {
			released = true
			c.Faults().Disarm(codefault.SyncDownload)
			close(held)
		}
	}
	defer release()
	read := func(via string) error {
		a, err := c.GitReadThrough(rc.ctx, via, treeRead(rc.id, head))
		if err == nil && !lists(a, "README.md") {
			t.Fatalf("the read through %s answered %+v, want the root listing README.md", via, a.Tree)
		}
		return err
	}

	// UNTIL THE RUNNER-UP IS FILLING: the walks that first meet the dead
	// home fill nothing, and the next asks the runner-up first, stamped.
	deadline := time.Now().Add(homeWait)
	for !node(runnerUp).Cache().Holds(rc.id) {
		for _, via := range []string{runnerUp, third} {
			if err := read(via); err == nil {
				t.Fatalf("a read through %s was served while every download was held", via)
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("the home's runner-up %s began no copy within %s of the home %s's death", runnerUp,
				homeWait, home)
		}
	}
	// A WALK THROUGH EACH MEMBER NOW, the runner-up's copy answering
	// `syncing`, so each goes on to the third member — which fills nothing.
	for _, via := range []string{third, runnerUp} {
		if err := read(via); err == nil {
			t.Fatalf("a read through %s was served while every download was held", via)
		}
	}
	if node(third).Cache().Holds(rc.id) {
		t.Errorf("%s, behind the filling runner-up %s, began a copy for a read it was never asked to fill",
			third, runnerUp)
	}

	release()
	for served := false; !served; {
		for _, via := range []string{runnerUp, third} {
			if read(via) == nil {
				served = true
				break
			}
		}
		if !served && time.Now().After(deadline) {
			t.Fatalf("no read through %s or %s was served within %s of the home %s's death", runnerUp, third,
				homeWait, home)
		}
		if !served {
			time.Sleep(100 * time.Millisecond)
		}
	}
	if !node(runnerUp).Cache().Holds(rc.id) {
		t.Errorf("the home's runner-up %s holds no copy after a read was served with the home dead", runnerUp)
	}
	if node(third).Cache().Holds(rc.id) {
		t.Errorf("%s, behind the runner-up %s, holds a copy: it downloaded the repository for a read it was "+
			"never asked to fill", third, runnerUp)
	}
}
