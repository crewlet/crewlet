package codetest

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/crewlet/crewlet/internal/code"
	"github.com/crewlet/crewlet/internal/code/codefault"
	"github.com/crewlet/crewlet/internal/code/compact"
	"github.com/crewlet/crewlet/internal/code/gitproc/gittest"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/statelog/metrics"
)

// THE LAYER CASES (LY1–LY7): a repository's bitmapped index published ONCE,
// by its maintainer, as a layer of a chain every git node installs only over
// its own packs, matching its row and in the rows' order — and a node that
// cannot install one serving from the layers below it, never an error
// (ADR-0061) — through the production compactor every git node of a topology
// is built with, never started, so each case drives its publications itself.

// otherThan is the first running git node that is not n.
func otherThan(c *Case, n Node) Node {
	c.T.Helper()
	for _, m := range c.Fleet.Nodes() {
		if m.ID() != n.ID() {
			return m
		}
	}
	c.T.Fatalf("no git node but %s", n.ID())
	return nil
}

// urlOf is RepoName's URL through node n.
func urlOf(n Node) string { return n.URL() + "/git/" + RepoName }

// bitmapLoads fails the case unless git loads the bitmap of the chain node n's
// copy holds and finds commit in it — `rev-list --test-bitmap`, which dies
// where no bitmap loads or the commit has none — run with the node's own git
// over the copy, as the cache's canary runs one.
func bitmapLoads(c *Case, n Node, commit string) {
	c.T.Helper()
	s, err := n.Cache().Sync(c.Ctx, c.Rows().Repo.ID, Linearizable)
	if err != nil {
		c.T.Fatalf("%s's sync of %s: %v", n.ID(), RepoName, err)
	}
	defer s.Release()
	git := gittest.NewClientWith(c.T, gittest.RequireNode(c.T))
	out, stderr, err := git.Try(c.T, s.Dir, nil, "rev-list", "--test-bitmap", commit)
	if err != nil || !strings.Contains(stderr, "OK!") {
		c.T.Errorf("git does not read %s's chain's bitmap for %s: %v\n%s%s\nthe copy's indexes:\n%s", n.ID(),
			commit, err, out, stderr, indexFiles(s.Dir))
	}
}

// indexFiles is every multi-pack-index, bitmap and commit-graph file of the
// copy at dir, and what its two chain files list: what a failed bitmapped
// read is judged by.
func indexFiles(dir string) string {
	var b strings.Builder
	for _, sub := range []string{packFilesDir, layersDir, filepath.Join("objects", "info"), graphsDir} {
		entries, err := os.ReadDir(filepath.Join(dir, sub))
		if err != nil {
			fmt.Fprintf(&b, "%s: %v\n", sub, err)
			continue
		}
		for _, e := range entries {
			if name := e.Name(); !strings.HasPrefix(name, "pack-") && !e.IsDir() {
				fmt.Fprintf(&b, "%s\n", filepath.Join(sub, name))
			}
		}
	}
	for _, chain := range []string{layersChain, graphsChain} {
		ids, err := chainFile(filepath.Join(dir, chain))
		fmt.Fprintf(&b, "%s lists %v (%v)\n", chain, ids, err)
	}
	return b.String()
}

// cloneHead fails the case unless a clone through node n checks out head.
func cloneHead(c *Case, p *Person, n Node, head string) {
	c.T.Helper()
	if got := p.Head(p.Clone(urlOf(n))); got != head {
		c.T.Errorf("a clone through %s is at %s, want %s", n.ID(), got, head)
	}
}

// LY1: ten pushes, then ONE publication by the repository's maintainer: every
// git node installs the chain — the maintainer by linking the files it wrote,
// with no GET of its own layer, every other git node by its installer — a
// clone through each checks out the head, and git loads the chain's bitmap
// over each copy. A git node that joins after the publication installs it in
// its first, full, round, before that round answers.
func aLayerIsInstalled(t *testing.T, newFleet NewFleet) {
	c := newCase(t, newFleet, Shape{GitNodes: 2}, Settings())
	p := NewPerson(t, c.Fleet, "ceo")
	work := p.Init()
	head := pushEach(c, p, work, 10, 2<<10)
	maintainer := mustPublish(c)
	for _, n := range c.Fleet.Nodes() {
		if ch := chainOn(c, n); len(ch.Rows) != 1 || ch.Installed != 1 {
			t.Fatalf("%s holds %d of a chain of %d layers installed after one publication, want its one layer",
				n.ID(), ch.Installed, len(ch.Rows))
		}
		cloneHead(c, p, n, head)
		bitmapLoads(c, n, head)
	}
	if n := counterOf(maintainer, metrics.CodeCacheLayers, "outcome", "adopted"); n != 1 {
		t.Errorf("the maintainer %s adopted %d layers, want its own one linked from its build, never downloaded",
			maintainer.ID(), n)
	}
	if n := counterOf(maintainer, metrics.CodeCacheLayers, "outcome", "installed"); n != 0 {
		t.Errorf("the maintainer %s installed %d layers from the store, want none: it built the one there is",
			maintainer.ID(), n)
	}

	// A NODE THAT JOINS AFTERWARDS: its first round of the repository is a
	// full one, which installs the chain before it answers.
	fresh := freshNode(c, otherThan(c, maintainer))
	s, err := fresh.Cache().Sync(c.Ctx, c.Rows().Repo.ID, Linearizable)
	if err != nil {
		t.Fatalf("%s's first sync of %s: %v", fresh.ID(), RepoName, err)
	}
	ch := fresh.Cache().Chain(s)
	s.Release()
	if ch.Installed != len(ch.Rows) || len(ch.Rows) != 1 {
		t.Errorf("a git node that joined after the publication answered its first sync holding %d of %d layers, "+
			"want the chain installed before it answers", ch.Installed, len(ch.Rows))
	}
	bitmapLoads(c, fresh, head)
	assertConverged(t, c.Fleet)
}

// LY2: a part of the published layer the object store lost. A git node that
// joins afterwards refuses the layer `missing` — once a read of the log's head
// made after the store's answer still names it — and serves from its own
// index, a clone through it correct; the audit reports nothing missing, since
// a layer is no data. The maintainer's next publication holds the base it
// builds on to the store, finds the part gone, and replaces the lost layer —
// which the joined node then installs.
func aLayerTheStoreLostFallsBack(t *testing.T, newFleet NewFleet) {
	c := newCase(t, newFleet, Shape{GitNodes: 2}, Settings())
	p := NewPerson(t, c.Fleet, "ceo")
	work := p.Init()
	head := pushEach(c, p, work, 3, 1<<10)
	maintainer := mustPublish(c)
	lost := loseLayerPart(c, maintainer, 0)

	fresh := freshNode(c, otherThan(c, maintainer))
	ch := chainOn(c, fresh)
	if ch.Installed != 0 || ch.Refused[lost.LayerID] != "missing" {
		t.Fatalf("a git node that joined after the store lost layer %s holds %d of its chain installed, the layer "+
			"refused %q; want it refused missing and nothing installed", lost.LayerID, ch.Installed,
			ch.Refused[lost.LayerID])
	}
	cloneHead(c, p, fresh, head)
	if report := c.Fleet.Audit(t); report.Missing != 0 || report.Damaged != 0 {
		t.Errorf("the audit after a layer's part was lost reports %d missing and %d damaged, want none: a "+
			"published layer is derived, no data", report.Missing, report.Damaged)
	}

	// THE REPAIR, at the next push's publication.
	head = pushEach(c, p, work, 1, 1<<10)
	mustPublish(c)
	rows := chainRows(c, fresh)
	if len(rows) == 0 || slices.Contains(rowLayerIDs(rows), lost.LayerID) {
		t.Fatalf("the publication after the store lost layer %s left the chain %v, want the lost layer replaced",
			lost.LayerID, rowLayerIDs(rows))
	}
	if ch := chainOn(c, fresh); ch.Installed != len(ch.Rows) {
		t.Errorf("%s holds %d of the repaired chain of %d layers installed, want all of it", fresh.ID(),
			ch.Installed, len(ch.Rows))
	}
	cloneHead(c, p, fresh, head)
	bitmapLoads(c, fresh, head)
	assertConverged(t, c.Fleet)
}

// lyingPublisher answers a git node's compactions with every publication's
// layer row claiming one object more than its files hold, while armed: a
// publisher whose row is not what it wrote, which every node's check refuses.
type lyingPublisher struct {
	mu    sync.Mutex
	armed bool
}

func (l *lyingPublisher) wrap(next Compactions) Compactions {
	return func(ctx context.Context, p code.CompactionProposal) (code.CompactResult, error) {
		l.mu.Lock()
		lie := l.armed && p.Compaction.Layer != nil
		l.mu.Unlock()
		if lie {
			layer := *p.Compaction.Layer
			layer.Objects++
			p.Compaction.Layer = &layer
		}
		return next(ctx, p)
	}
}

func (l *lyingPublisher) arm(on bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.armed = on
}

// LY3: a layer every node refuses `corrupt`, in two arms — one byte of a part
// spoiled in the store, which its hash check refuses, and a LYING PUBLISHER
// whose row claims an object its files do not hold, which code.CheckLayer
// refuses. In both a git node that joins afterwards serves from the layers
// below — its own index here, the chain having one layer — and a publication
// by a maintainer whose own copy refused the layer too REPAIRS it, replacing
// it whether or not anything was pushed since, which every node installs.
func aCorruptLayerFallsBack(t *testing.T, newFleet NewFleet) {
	for _, arm := range []string{"spoiled", "lying"} {
		t.Run(arm, func(t *testing.T) {
			liar := &lyingPublisher{}
			fleet := func(t *testing.T, s Shape) Fleet {
				s.WrapCompactions = liar.wrap
				return newFleet(t, s)
			}
			c := newCase(t, fleet, Shape{GitNodes: 2}, Settings())
			p := NewPerson(t, c.Fleet, "ceo")
			work := p.Init()
			head := pushEach(c, p, work, 3, 1<<10)
			liar.arm(arm == "lying")
			maintainer, out := publishAtMaintainer(c)
			liar.arm(false)
			if out != compact.OutcomeApplied {
				t.Fatalf("%s's publication: %s, want applied", maintainer.ID(), out)
			}
			badRow := chainRows(c, maintainer)[0]
			bad := badRow.Layer
			if arm == "spoiled" {
				spoilLayerPart(c, maintainer, 0)
			}
			// EVERY COPY READS THE LAYER FROM THE STORE: a fresh maintainer's
			// copy as well, so its own refusal is the REPAIR its next
			// publication makes.
			maintainer = freshNode(c, maintainer)
			fresh := freshNode(c, otherThan(c, maintainer))
			maintainer = nodeByID(c, maintainer.ID())
			for _, n := range []Node{maintainer, fresh} {
				ch := chainOn(c, n)
				if ch.Installed != 0 || ch.Refused[bad.LayerID] != "corrupt" {
					t.Fatalf("%s holds %d of the chain installed and refused layer %s %q, want it refused "+
						"corrupt and nothing installed", n.ID(), ch.Installed, bad.LayerID, ch.Refused[bad.LayerID])
				}
				cloneHead(c, p, n, head)
			}
			if got := maintainer.Cache().Damaged(); len(got) != 1 || got[0].Name != RepoName {
				t.Fatalf("the maintainer's cache lists %v damaged, want %s", got, RepoName)
			}
			// THE REPAIR: nothing pushed since.
			mustPublish(c)
			// The same packs write the same layer, so the repair may carry
			// the refused layer's id: what it replaced is the RECORD, at a
			// position of its own, with parts of its own.
			rows := chainRows(c, fresh)
			if len(rows) != 1 {
				t.Fatalf("the repair left the chain %v, want one layer", rowLayerIDs(rows))
			}
			if rows[0].Added == badRow.Added {
				t.Fatalf("the repair left the layer added at %v, the refused one; want it replaced", rows[0].Added)
			}
			for _, n := range c.Fleet.Nodes() {
				if ch := chainOn(c, n); ch.Installed != 1 {
					t.Errorf("%s holds %d of the repaired chain installed, want its one layer", n.ID(), ch.Installed)
				}
				bitmapLoads(c, n, head)
			}
			assertConverged(t, c.Fleet)
		})
	}
}

// LY4: a push that lands between a publication's plan and its decide stays in
// the tail: the publication lands — the decide holds the layer complete only
// over the packs live at its plan's position — the pushed pack is outside the
// chain on every node, and a clone of its commit through each succeeds.
func aPushRacingAPublicationStaysInTheTail(t *testing.T, newFleet NewFleet) {
	c := newCase(t, newFleet, Shape{GitNodes: 2}, Settings())
	p := NewPerson(t, c.Fleet, "ceo")
	work := p.Init()
	pushEach(c, p, work, 3, 1<<10)
	maintainer := maintainerOf(c)
	reached, release := make(chan struct{}), make(chan struct{})
	c.Fleet.Faults().Arm(codefault.PublishAfterUpload, codefault.Action{Once: true, Reached: reached,
		Block: release})
	done := make(chan compact.Outcome, 1)
	cand := publicationOf(c, maintainer)
	go func() {
		out, _ := tryPublishOn(context.WithoutCancel(c.Ctx), maintainer, cand)
		done <- out
	}()
	select {
	case <-reached:
	case out := <-done:
		t.Fatalf("the publication answered %s before its uploads ended", out)
	}
	head := pushEach(c, p, work, 1, 1<<10)
	raced := c.Rows().Packs
	close(release)
	if out := <-done; out != compact.OutcomeApplied {
		t.Fatalf("the publication a push raced: %s, want applied", out)
	}
	rows := chainRows(c, maintainer)
	if len(rows) != 1 {
		t.Fatalf("the chain holds %d layers, want the one publication's", len(rows))
	}
	var tail []string
	for _, pk := range raced {
		if !slices.Contains(rows[0].Layer.Packs, pk.GitChecksum) {
			tail = append(tail, pk.GitChecksum)
		}
	}
	if len(tail) != 1 {
		t.Fatalf("the layer leaves %v of the live packs outside it, want exactly the pack pushed meanwhile", tail)
	}
	for _, n := range c.Fleet.Nodes() {
		if ch := chainOn(c, n); ch.Installed != 1 {
			t.Errorf("%s holds %d of the chain installed, want its one layer", n.ID(), ch.Installed)
		}
		cloneHead(c, p, n, head)
	}
	assertConverged(t, c.Fleet)
}

// LY5: a publication that absorbs both layers of a chain and REPACKS the many
// small packs it gathers lands, and every node keeps serving through a bitmap
// at every step: between the round that retires the repacked packs and the
// installer's placement of their replacement — held there at
// InstallAfterFetch — the old chain still serves, since the chain holds its
// packs; once the replacement is placed, the packs it retired are gone.
func aRepackingPublicationKeepsTheBitmap(t *testing.T, newFleet NewFleet) {
	c := newCase(t, newFleet, Shape{GitNodes: 2}, Settings())
	p := NewPerson(t, c.Fleet, "ceo")
	work := p.Init()
	// A ROOT LAYER OVER MANY OBJECTS, then a small one above it: the second
	// is under half the first, so the chain keeps both.
	files := map[string]string{}
	for i := range 120 {
		files[fmt.Sprintf("root/f%03d", i)] = randomFile(64)
	}
	p.Commit(work, files, "the root's history")
	c.MustPush(p, work, "main")
	maintainer := mustPublish(c)
	first := pushEach(c, p, work, 1, 1<<10)
	mustPublish(c)
	if rows := chainRows(c, maintainer); len(rows) != 2 {
		t.Fatalf("the chain after a large and a small publication holds %d layers, want 2", len(rows))
	}
	other := otherThan(c, maintainer)
	for _, n := range c.Fleet.Nodes() {
		// Once its installer has placed the second layer: a delta hands it
		// over and answers at once, so a node other than the maintainer may
		// still serve the root alone, which holds no bitmap of first.
		if ch := chainOn(c, n); ch.Installed != 2 {
			t.Fatalf("%s holds %d of the chain of %d installed, want both layers", n.ID(), ch.Installed,
				len(ch.Rows))
		}
		bitmapLoads(c, n, first)
	}
	// TEN PACKS OF MANY OBJECTS: the next layer absorbs both above and
	// repacks every pack it gathers, more than compact.LayerRepackPacks of
	// them not full.
	var head string
	for i := range compact.LayerRepackPacks + 2 {
		batch := map[string]string{}
		for j := range 30 {
			batch[fmt.Sprintf("batch%d/f%02d", i, j)] = randomFile(64)
		}
		head = p.Commit(work, batch, fmt.Sprintf("batch %d", i))
		c.MustPush(p, work, "main")
	}
	before := c.Rows().Packs
	reached, release := make(chan struct{}), make(chan struct{})
	c.Fleet.Faults().Arm(codefault.InstallAfterFetch, codefault.Action{Once: true, Reached: reached,
		Block: release})
	released := false
	defer func() {
		if !released {
			close(release)
		}
	}()
	mustPublish(c)
	rows := chainRows(c, maintainer)
	if len(rows) != 1 || rows[0].Layer.BaseID != "" {
		t.Fatalf("the repacking publication left the chain %v, want one root layer replacing both", rowLayerIDs(rows))
	}
	var retired []string
	for _, pk := range before {
		if !slices.Contains(rows[0].Layer.Packs, pk.GitChecksum) {
			retired = append(retired, pk.GitChecksum)
		}
	}
	if len(retired) <= compact.LayerRepackPacks {
		t.Fatalf("the publication repacked %d packs, want more than %d", len(retired), compact.LayerRepackPacks)
	}
	// THE OTHER NODE'S DELTA: the repacked packs retired, the new layer at
	// its installer, held — and its old chain still serving.
	s, err := other.Cache().Sync(c.Ctx, c.Rows().Repo.ID, Linearizable)
	if err != nil {
		t.Fatalf("%s's sync after the repacking publication: %v", other.ID(), err)
	}
	s.Release()
	<-reached
	bitmapLoads(c, other, first)
	released = true
	close(release)
	if ch := chainOn(c, other); ch.Installed != 1 {
		t.Fatalf("%s holds %d of the new chain installed once its installer was let go, want its one layer",
			other.ID(), ch.Installed)
	}
	for _, n := range c.Fleet.Nodes() {
		bitmapLoads(c, n, head)
		s, err := n.Cache().Sync(c.Ctx, c.Rows().Repo.ID, Linearizable)
		if err != nil {
			t.Fatalf("%s's sync: %v", n.ID(), err)
		}
		held, err := packsIn(filepath.Join(s.Dir, "objects", "pack"))
		s.Release()
		if err != nil {
			t.Fatalf("%s's packs: %v", n.ID(), err)
		}
		for _, cs := range retired {
			if slices.Contains(held, cs) {
				t.Errorf("%s still holds pack-%s, which the publication repacked, once its replacement is placed",
					n.ID(), cs)
			}
		}
	}
	assertConverged(t, c.Fleet)
}

// LY6: a compaction of a published repository's TAIL keeps its chain whole on
// every node: the compaction merges only packs no layer names, every node's
// copy swaps to a directory of links carrying every layer — and the node's
// tip — and no layer is fetched again.
func aCompactionKeepsTheChainWhole(t *testing.T, newFleet NewFleet) {
	c := newCase(t, newFleet, Shape{GitNodes: 2}, Settings())
	p := NewPerson(t, c.Fleet, "ceo")
	work := p.Init()
	first := pushEach(c, p, work, 2, 1<<10)
	mustPublish(c)
	published := chainRows(c, c.Fleet.Nodes()[0])[0].Layer
	head := pushEach(c, p, work, code.CompactMinMerge, 1<<10)
	for _, n := range c.Fleet.Nodes() {
		chainOn(c, n)
	}
	installed := map[string]uint64{}
	for _, n := range c.Fleet.Nodes() {
		installed[n.ID()] = counterOf(n, metrics.CodeCacheLayers, "outcome", "installed")
	}
	compactAtMaintainer(c)
	retired := retiredSince(c, c.Rows().Repo.ID, statelog.Position{})
	for _, r := range retired {
		if slices.Contains(published.Packs, r.GitChecksum) {
			t.Errorf("the compaction retired pack-%s, which the published layer names", r.GitChecksum)
		}
	}
	for _, n := range c.Fleet.Nodes() {
		ch := chainOn(c, n)
		if ch.Installed != 1 || ch.Rows[0].Layer.LayerID != published.LayerID {
			t.Errorf("%s holds %d of the chain %v installed after a compaction of the tail, want the published "+
				"layer kept", n.ID(), ch.Installed, rowLayerIDs(ch.Rows))
		}
		if got := counterOf(n, metrics.CodeCacheLayers, "outcome", "installed"); got != installed[n.ID()] {
			t.Errorf("%s installed %d layers from the store across a compaction of the tail, want none",
				n.ID(), got-installed[n.ID()])
		}
		bitmapLoads(c, n, first)
		cloneHead(c, p, n, head)
	}
	assertConverged(t, c.Fleet)
}

// newerGit answers a git node's compactions with every publication's layer
// row stamped by a git release no node runs.
func newerGit(next Compactions) Compactions {
	return func(ctx context.Context, p code.CompactionProposal) (code.CompactResult, error) {
		if p.Compaction.Layer != nil {
			layer := *p.Compaction.Layer
			layer.Git = "9.0.0"
			p.Compaction.Layer = &layer
		}
		return next(ctx, p)
	}
}

// LY7: a layer a newer git wrote than a node's own is not installed: refused
// `newer_git` with NO GET of its parts, the copy serving in its own mode.
func aLayerFromANewerGitIsNotInstalled(t *testing.T, newFleet NewFleet) {
	fleet := func(t *testing.T, s Shape) Fleet {
		s.WrapCompactions = newerGit
		return newFleet(t, s)
	}
	c := newCase(t, fleet, Shape{GitNodes: 2}, Settings())
	p := NewPerson(t, c.Fleet, "ceo")
	work := p.Init()
	head := pushEach(c, p, work, 3, 1<<10)
	maintainer := mustPublish(c)
	layer := chainRows(c, maintainer)[0].Layer
	if layer.Git != "9.0.0" {
		t.Fatalf("the published layer is stamped %q, want the newer git the case stamped", layer.Git)
	}
	fresh := freshNode(c, otherThan(c, maintainer))
	ch := chainOn(c, fresh)
	if ch.Installed != 0 || ch.Refused[layer.LayerID] != "newer_git" {
		t.Fatalf("%s holds %d of the chain installed and refused it %q, want it refused newer_git", fresh.ID(),
			ch.Installed, ch.Refused[layer.LayerID])
	}
	// NO GET: an install fetches a layer's parts and is timed from its
	// staging, whatever became of it, and a layer of a newer git is never
	// staged.
	for _, s := range fresh.Metrics().Read() {
		if s.Name == metrics.CodeCacheLayerInstall && s.Count > 0 {
			t.Errorf("%s began installing a layer a newer git wrote %d times (%v), want never: its parts are "+
				"never fetched", fresh.ID(), s.Count, s.Attrs)
		}
	}
	cloneHead(c, p, fresh, head)
	assertConverged(t, c.Fleet)
}

// LY10: a crash of the maintainer at every step of a publication leaves a
// truthful state, as C6 holds a compaction's: before the committer's answer
// nothing applied — the parts it stored named by no row, which the collector
// deletes and nothing else — and the next publication lands; after it, the
// layer is applied and un-adopted, so the restarted maintainer's installer
// fetches it. Either way every node's chain is a prefix of the rows' that git
// loads.
func aMaintainerKilledAtEveryPublishPoint(t *testing.T, newFleet NewFleet) {
	for _, point := range codefault.PublishPath() {
		t.Run(string(point), func(t *testing.T) { publishCrashAt(t, newFleet, point) })
	}
}

// publishCrashAt is LY10 at one point.
func publishCrashAt(t *testing.T, newFleet NewFleet, point codefault.Point) {
	c := newCase(t, newFleet, Shape{GitNodes: 2}, Settings())
	p := NewPerson(t, c.Fleet, "ceo")
	work := p.Init()
	head := pushEach(c, p, work, 3, 1<<10)
	before := objectNames(t, c.Fleet.Objects())
	maintainer := maintainerOf(c)
	cand := publicationOf(c, maintainer)
	reached := make(chan struct{})
	c.Fleet.Faults().Arm(point, codefault.Action{Once: true, Crash: true, Reached: reached})
	done := make(chan compact.Outcome, 1)
	go func() {
		out, _ := tryPublishOn(context.WithoutCancel(c.Ctx), maintainer, cand)
		done <- out
	}()
	select {
	case <-reached:
	case out := <-done:
		t.Fatalf("the publication ended %s before it reached %s", out, point)
	}
	c.Fleet.Kill(t, maintainer.ID())
	c.Fleet.Restart(t, maintainer.ID())
	maintainer = maintainerOf(c)

	rows := chainRows(c, maintainer)
	if point == codefault.PublishAfterPropose {
		// THE COMMITTER ANSWERED before the crash: applied, and nothing
		// adopted, so the restarted maintainer fetched its own layer.
		if len(rows) != 1 {
			t.Fatalf("after a crash at %s the chain holds %d layers, want the one the committer applied", point,
				len(rows))
		}
		if n := counterOf(maintainer, metrics.CodeCacheLayers, "outcome", "installed"); n != 1 {
			t.Errorf("the restarted maintainer installed %d layers from the store, want its un-adopted one", n)
		}
	} else {
		if len(rows) != 0 {
			t.Fatalf("after a crash at %s the chain holds %d layers, want nothing applied", point, len(rows))
		}
		mustPublish(c)
		rows = chainRows(c, maintainer)
	}
	for _, n := range c.Fleet.Nodes() {
		if ch := chainOn(c, n); ch.Installed != len(ch.Rows) || len(ch.Rows) != 1 {
			t.Errorf("%s holds %d of the chain of %d installed after a crash at %s, want it whole", n.ID(),
				ch.Installed, len(ch.Rows), point)
		}
		bitmapLoads(c, n, head)
	}
	// THE COLLECTOR DELETES WHAT NO ROW NAMES AND NOTHING ELSE: the parts of a
	// publication that never applied, and none of the applied layer's or of
	// a pack's.
	named := map[string]bool{}
	for _, pk := range c.Rows().Packs {
		named[pk.Object.Name()] = true
	}
	for _, part := range rows[0].Layer.Parts {
		named[part.Object.Name()] = true
	}
	for _, n := range before {
		named[n] = true
	}
	c.Fleet.Collect(t)
	stored := map[string]bool{}
	for _, n := range objectNames(t, c.Fleet.Objects()) {
		stored[n] = true
		if !named[n] {
			t.Errorf("%s is named by no row and the collector left it", n)
		}
	}
	for _, part := range rows[0].Layer.Parts {
		if !stored[part.Object.Name()] {
			t.Errorf("the collector deleted the applied layer's part %s", part.Object.Name())
		}
	}
	assertConverged(t, c.Fleet)
}
