package gitlettest

import (
	"slices"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/gitlet"
	"github.com/crewlet/crewlet/internal/gitlet/gitletfault"
	"github.com/crewlet/crewlet/internal/rendezvous"
	"github.com/crewlet/crewlet/internal/statelog/metrics"
)

// The reads of a repository by object id through a fleet's routing — asked of
// the repository's home Gitlet node (ADR-0089) — as a seat's tool asks them.

// homeWait bounds a case's wait for the node it expects to answer a read: a
// home filling its copy answers `syncing` and the walk asks the others, so the
// case asks again until the copy is warm. A minute is far past a small
// repository's cold sync on a loaded runner (a second or two) and well inside
// a case's own deadline.
const homeWait = time.Minute

// readsServed is how many reads n's query server ran git for: its granted
// seat git-work units of work read (metrics.CodeSeatworkWaits), which only a
// read that found its objects takes — not one answered unready or refused,
// whose waits the share times too, nor an engine write's build.
func readsServed(n Node) uint64 {
	var total uint64
	for _, s := range n.Metrics().Read() {
		if s.Name == metrics.CodeSeatworkWaits && s.Attrs["outcome"] == "granted" &&
			s.Attrs["work"] == string(gitlet.SeatWorkRead) {
			total += s.Count
		}
	}
	return total
}

// treeRead is the root listing of repo at rev, as a person reads it.
func treeRead(repo, rev string) gitlet.GitQuery {
	return gitlet.GitQuery{Kind: gitlet.GitTree, RepoID: repo, Asker: Reader, Tree: &gitlet.TreeQuery{Rev: rev}}
}

// lists reports whether a read answered a root listing naming path.
func lists(a gitlet.GitAnswer, path string) bool {
	return a.Tree != nil && slices.ContainsFunc(a.Tree.Entries, func(e gitlet.TreeEntry) bool { return e.Path == path })
}

// F26: A READ IS ANSWERED BY THE HOME (ADR-0089). A read by object id through
// the fleet's routing is asked of the repository's home, the first of
// rendezvous.Order over the Gitlet nodes, which answers once its copy is warm.
// Meanwhile the node the push filled may answer; the third node, neither home
// nor pusher, holds no copy after, since only the walk's effective home is
// ever asked to fill one.
func aReadIsAnsweredByTheHome(t *testing.T, newFleet NewFleet) {
	c := newCase(t, newFleet, Shape{GitletNodes: 3}, Settings())
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
		if time.Now().After(deadline) { //fixedwait:measure a poll's own deadline, whose firing only fails the case
			t.Fatalf("no read through the fleet was answered by the repository's home %s within %s "+
				"(the last: %v)", home.ID(), homeWait, err)
		}
		time.Sleep(50 * time.Millisecond) //fixedwait:liveness
	}
	for _, n := range nodes {
		if n.ID() != home.ID() && n.ID() != pusher.ID() && n.Cache().Holds(repo) {
			t.Errorf("%s, neither the repository's home %s nor the node its push went through, holds a "+
				"copy: a node a walk did not ask to fill started a download", n.ID(), home.ID())
		}
	}
	assertConverged(t, c.Fleet)
}

// F27: A HOME'S DEATH FILLS ONE COPY (ADR-0089). A repository only its home
// holds is read, the home KILLED, through each other member until one serves
// it. Walks meeting the dead home ask the runner-up and the third member
// unstamped, and neither downloads for a read it was not asked to fill; once a
// router asks the silent home last, the runner-up is first, stamped, and
// fills. So the runner-up alone holds a copy after, not every member reached.
//
// THE DOWNLOADS ARE HELD while the runner-up fills, so it answers `syncing`
// and walks go on to the third member: a small repository's cold sync beats a
// read's two-second wait, so unheld the third would never be asked a read it
// could wrongly fill.
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
	// RELEASED BEFORE THE RESTART (deferred calls run last-in first-out) and
	// on every way out: a held download holds its round for the cache's
	// lifetime.
	held := make(chan struct{})
	c.Faults().Arm(gitletfault.SyncDownload, gitletfault.Action{Block: held})
	released := false
	release := func() {
		if !released {
			released = true
			c.Faults().Disarm(gitletfault.SyncDownload)
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
		if time.Now().After(deadline) { //fixedwait:measure a poll's own deadline, whose firing only fails the case
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
		if !served && time.Now().After(deadline) { //fixedwait:measure a poll's own deadline, whose firing only fails the case
			t.Fatalf("no read through %s or %s was served within %s of the home %s's death", runnerUp, third,
				homeWait, home)
		}
		if !served {
			time.Sleep(100 * time.Millisecond) //fixedwait:liveness
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
