package codetest

import (
	"context"
	"fmt"
	"slices"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/code"
	"github.com/crewlet/crewlet/internal/code/cache"
	"github.com/crewlet/crewlet/internal/code/codefault"
	"github.com/crewlet/crewlet/internal/code/compact"
)

// THE LAYERS' FLEET CASES: what only more than one data node can make of a
// publication — git nodes of different sizes, and two maintainers.

// publication is the repository as the publication due listing through node
// n names it, or as its rows stand where the listing does not name it.
func (rc *repoCase) publication(n Node) code.PublicationCandidate {
	rc.t.Helper()
	a, err := n.Read(rc.ctx, code.ReadQuery{Kind: code.ReadPublicationDue, Fresh: Linearizable})
	if err != nil {
		rc.t.Fatalf("the publication due listing through %s: %v", n.ID(), err)
	}
	for _, cand := range a.Publish {
		if cand.Name == rc.name {
			return cand
		}
	}
	rows := rc.rows()
	cand := code.PublicationCandidate{ID: rc.id, Name: rc.name, LivePacks: int64(len(rows.Packs)),
		Version: rows.Position}
	for _, p := range rows.Packs {
		cand.LiveObjects += p.Objects
	}
	return cand
}

// maintainer is the running member whose compactor takes itself for the
// repository's maintainer, asked by its maintenance's charge.
func (rc *repoCase) maintainer() Node {
	rc.t.Helper()
	var out []Node
	for _, n := range rc.c.Nodes() {
		yes, err := maintains(n, rc.publication(n))
		if err != nil {
			rc.t.Fatalf("whether %s maintains %s: %v", n.ID(), rc.name, err)
		}
		if yes {
			out = append(out, n)
		}
	}
	if len(out) != 1 {
		rc.t.Fatalf("%d members take themselves for %s's maintainer, want one", len(out), rc.name)
	}
	return out[0]
}

// publish is node n's publication of the repository, asked again while it
// answers busy or waiting, for a minute at the most.
func (rc *repoCase) publish(ctx context.Context, n Node) (compact.Outcome, error) {
	cand := rc.publication(n)
	deadline := time.Now().Add(time.Minute)
	for {
		out, err := n.Compactor().Publish(ctx, cand)
		if (out != compact.OutcomeBusy && out != compact.OutcomeWaiting) || time.Now().After(deadline) {
			return out, err
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// chain is the repository's published chain as node n's copy holds it, once
// its installer has placed or refused every layer the rows name.
func (rc *repoCase) chain(n Node) cache.Chain {
	rc.t.Helper()
	s, ch, err := settledCopy(rc.ctx, n, rc.id)
	if err != nil {
		rc.t.Fatalf("%s's copy of %s: %v", n.ID(), rc.name, err)
	}
	defer s.Release()
	return ch
}

// LY8: a repository's publication is placed on the git node whose advertised
// background budget holds its maintenance, whatever its home: with two
// members advertising exactly the small repository's charge and one 64 GiB,
// every publication of the large repository, whose charge passes theirs, is
// the large member's — which every other member installs — while the small
// repository, whose charge every member holds, stays with its home, the
// maintainer it had while every member advertised alike.
func aLargeNodePublishesForEveryNode(t *testing.T, c Cluster) {
	members := c.Members()
	for _, m := range members {
		c.Advertise(t, m, 64<<30)
	}
	large := newRepoCase(t, c, "fleet-ly8-large")
	small := newRepoCase(t, c, "fleet-ly8-small")
	p := NewPerson(t, c, "ceo")
	via := c.DataNodes()[0]
	work := p.Init()
	files := map[string]string{}
	for i := range 100 {
		files[fmt.Sprintf("src/f%03d", i)] = randomFile(64)
	}
	p.Commit(work, files, "a hundred files")
	large.mustPush(p, work, via, "main")
	other := p.Init()
	p.Commit(other, map[string]string{"README.md": "one\n"}, "one")
	small.mustPush(p, other, via, "main")
	n := memberNode(t, c, via)
	smallCharge := n.Compactor().PlacedBy(small.publication(n).LiveObjects)
	if charge := n.Compactor().PlacedBy(large.publication(n).LiveObjects); charge <= smallCharge {
		t.Fatalf("the large repository's maintenance is charged %d, which the small members' %d would hold: "+
			"the case needs it past them", charge, smallCharge)
	}
	homeSmall := small.maintainer().ID()
	// THE LARGE MEMBER: one that is NOT the large repository's home, so
	// what moves it is the budgets alone.
	homeLarge := large.maintainer().ID()
	big := ""
	for _, m := range members {
		if m != homeLarge {
			big = m
			break
		}
	}
	for _, m := range members {
		if m != big {
			c.Advertise(t, m, smallCharge)
		}
	}
	if got := large.maintainer().ID(); got != big {
		t.Fatalf("the large repository's maintainer is %s, want %s, the one member whose budget holds its "+
			"publication — its home is %s", got, big, homeLarge)
	}
	if got := small.maintainer().ID(); got != homeSmall {
		t.Errorf("the small repository's maintainer is %s, want its home %s: every member holds its publication",
			got, homeSmall)
	}
	if out, err := large.publish(large.ctx, memberNode(t, c, big)); out != compact.OutcomeApplied {
		t.Fatalf("%s's publication of the large repository: %s, %v; want applied", big, out, err)
	}
	for _, n := range c.Nodes() {
		if ch := large.chain(n); len(ch.Rows) != 1 || ch.Installed != 1 {
			t.Errorf("%s holds %d of the large repository's chain of %d installed, want the one layer the large "+
				"member published", n.ID(), ch.Installed, len(ch.Rows))
		}
	}
	large.converged()
	small.converged()
}

// LY9: two members that each take themselves for a repository's maintainer —
// a presence view split for a moment, which no lease decides — both build a
// publication from the same rows, and the committer lands one: the other is
// refused stale, the chain it built over having moved, and the parts it stored,
// named by no row, are the collector's once their grace has passed.
func twoMaintainersLandOnePublication(t *testing.T, c Cluster) {
	rc := newRepoCase(t, c, "fleet-ly9")
	committer := rc.committer()
	a := rc.other(committer)
	b := rc.other(committer, a)
	c.SplitMaintainers(t, rc.id, a, b)
	p := NewPerson(t, c, "ceo")
	work := p.Init()
	rc.pushN(p, work, a, 3)
	na, nb := memberNode(t, c, a), memberNode(t, c, b)
	for _, n := range []Node{na, nb} {
		if yes, err := maintains(n, rc.publication(n)); !yes || err != nil {
			t.Fatalf("%s does not take itself for the maintainer: %v, %v", n.ID(), yes, err)
		}
	}
	storedBefore := map[string]bool{}
	for _, n := range objectNames(t, c.Objects()) {
		storedBefore[n] = true
	}

	// a's publication held with its parts stored and nothing proposed, while
	// b's lands.
	held, reached := make(chan struct{}), make(chan struct{})
	c.Faults().Arm(codefault.PublishAfterUpload, codefault.Action{Once: true, Block: held, Reached: reached})
	aOut := make(chan compact.Outcome, 1)
	aCand := rc.publication(na)
	go func() {
		out, _ := tryPublishOn(context.WithoutCancel(rc.ctx), na, aCand)
		aOut <- out
	}()
	select {
	case <-reached:
	case out := <-aOut:
		close(held)
		t.Fatalf("%s's publication ended %s before its uploads did", a, out)
	case <-time.After(time.Minute):
		close(held)
		t.Fatalf("%s's publication reached no upload in a minute", a)
	}
	if out, err := rc.publish(rc.ctx, nb); out != compact.OutcomeApplied {
		close(held)
		t.Fatalf("%s's publication: %s, %v; want applied", b, out, err)
	}
	close(held)
	if out := <-aOut; out != compact.OutcomeStale {
		t.Errorf("%s's publication, built over the chain %s's moved, was answered %s, want stale", a, b, out)
	}
	rows := rc.chain(nb).Rows
	if len(rows) != 1 {
		t.Fatalf("two maintainers left a chain of %d layers, want one publication's", len(rows))
	}
	named := map[string]bool{}
	for _, part := range rows[0].Layer.Parts {
		named[part.Object.Name()] = true
	}
	var losers []string
	for _, n := range objectNames(t, c.Objects()) {
		if !storedBefore[n] && !named[n] {
			losers = append(losers, n)
		}
	}
	if len(losers) == 0 {
		t.Errorf("%s's stale publication stored no part: the case did not build two", a)
	}
	c.CollectAt(t, time.Now().Add(25*time.Hour))
	stored := map[string]bool{}
	for _, n := range objectNames(t, c.Objects()) {
		stored[n] = true
	}
	for _, n := range losers {
		if stored[n] {
			t.Errorf("the stale publication's part %s, named by no row, outlived the collection", n)
		}
	}
	for n := range named {
		if !stored[n] {
			t.Errorf("the collection deleted the applied layer's part %s", n)
		}
	}
	for _, m := range c.Nodes() {
		if ch := rc.chain(m); ch.Installed != 1 || !slices.Equal(rowLayerIDs(ch.Rows), rowLayerIDs(rows)) {
			t.Errorf("%s holds %d of the chain %v installed, want the applied publication's layer", m.ID(),
				ch.Installed, rowLayerIDs(ch.Rows))
		}
	}
	rc.converged()
}
