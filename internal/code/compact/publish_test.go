package compact_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/code"
	"github.com/crewlet/crewlet/internal/code/cache"
	"github.com/crewlet/crewlet/internal/code/codefault"
	"github.com/crewlet/crewlet/internal/code/codetest"
	"github.com/crewlet/crewlet/internal/code/compact"
	"github.com/crewlet/crewlet/internal/code/gitproc"
	"github.com/crewlet/crewlet/internal/objstore"
	"github.com/crewlet/crewlet/internal/objstore/collect"
	"github.com/crewlet/crewlet/internal/statelog/metrics"
)

// publicationCandidate is the repository as the publication due listing names
// it.
func (x *fixture) publicationCandidate() code.PublicationCandidate {
	x.t.Helper()
	a, err := x.fleet.Data().Served().Read(x.c.Ctx, code.ReadQuery{Kind: code.ReadPublicationDue,
		Fresh: codetest.Linearizable})
	if err != nil {
		x.t.Fatalf("the publication due listing: %v", err)
	}
	for _, cand := range a.Publish {
		if cand.Name == codetest.RepoName {
			return cand
		}
	}
	x.t.Fatalf("the publication due listing names no %s: %+v", codetest.RepoName, a.Publish)
	return code.PublicationCandidate{}
}

// publishWhileBusy is c's publication of cand, asked again while it answers
// busy — a maintenance slot the maintainer holds a moment — or waiting — a
// layer the maintainer adopted that its installer has not yet placed — for a
// minute at the most.
func publishWhileBusy(t testing.TB, c *compact.Compactor, cand code.PublicationCandidate) (compact.Outcome, error) {
	t.Helper()
	deadline := time.Now().Add(time.Minute)
	for {
		out, err := c.Publish(t.Context(), cand)
		if (out != compact.OutcomeBusy && out != compact.OutcomeWaiting) || time.Now().After(deadline) {
			return out, err
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// chain is the repository's published chain as x's git node's copy holds it,
// synced linearizable.
func (x *fixture) chain() cache.Chain {
	x.t.Helper()
	s, err := x.node().Cache().Sync(x.c.Ctx, x.rows().Repo.ID, codetest.Linearizable)
	if err != nil {
		x.t.Fatalf("the sync of %s: %v", codetest.RepoName, err)
	}
	defer s.Release()
	return x.node().Cache().Chain(s)
}

// awaitInstalled waits for x's git node's copy to hold the rows' whole chain
// of n layers installed — its installer places what its maintainer adopted in
// the background — a minute at most, and answers it.
func (x *fixture) awaitInstalled(n int) cache.Chain {
	x.t.Helper()
	deadline := time.Now().Add(time.Minute)
	for {
		ch := x.chain()
		if len(ch.Rows) == n && ch.Installed == n {
			return ch
		}
		if time.Now().After(deadline) {
			x.t.Fatalf("the copy holds %d of a chain of %d layers installed a minute on, want all %d: %+v",
				ch.Installed, len(ch.Rows), n, ch.Refused)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// liveChecksums is every live pack's git checksum, sorted and distinct.
func liveChecksums(rows codetest.Rows) []string {
	var out []string
	for _, p := range rows.Packs {
		out = append(out, p.GitChecksum)
	}
	slices.Sort(out)
	return slices.Compact(out)
}

// watched is a publication's cache as a case watches it: every admission and
// every scratch counted, and its chain, its packs and its admissions answered
// by the case's own where set.
type watched struct {
	*counted
	mu        sync.Mutex
	admits    int
	scratches [][]string
	chainOf   func(cache.Chain) cache.Chain
	packsOf   func([]code.PackRow) []code.PackRow
}

func watchedOver(c *cache.Cache) *watched { return &watched{counted: countedOver(c)} }

func (w *watched) Chain(s cache.Synced) cache.Chain {
	ch := w.counted.Chain(s)
	w.mu.Lock()
	edit := w.chainOf
	w.mu.Unlock()
	if edit != nil {
		return edit(ch)
	}
	return ch
}

func (w *watched) Packs(s cache.Synced) []code.PackRow {
	packs := w.counted.Packs(s)
	w.mu.Lock()
	edit := w.packsOf
	w.mu.Unlock()
	if edit != nil {
		return edit(packs)
	}
	return packs
}

func (w *watched) AdmitPublication(s cache.Synced, layerObjects, repackObjects int64, limits code.Limits) (
	*cache.Admission, error) {

	w.mu.Lock()
	w.admits++
	w.mu.Unlock()
	return w.counted.AdmitPublication(s, layerObjects, repackObjects, limits)
}

// NewLayerScratch records the chain file of every scratch it makes, as the
// scratch holds it before any git process runs in it.
func (w *watched) NewLayerScratch(ctx context.Context, s cache.Synced, id string, base int) (*cache.Scratch, error) {
	sc, err := w.counted.NewLayerScratch(ctx, s, id, base)
	if err != nil {
		return nil, err
	}
	text, err := os.ReadFile(filepath.Join(sc.Dir, "objects", "pack", "multi-pack-index.d", "multi-pack-index-chain"))
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		sc.Remove()
		return nil, err
	}
	w.mu.Lock()
	w.scratches = append(w.scratches, strings.Fields(string(text)))
	w.mu.Unlock()
	return sc, nil
}

// seen is how many publications were admitted, and every scratch's chain
// file.
func (w *watched) seen() (admits int, scratches [][]string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.admits, slices.Clone(w.scratches)
}

// A PUBLICATION IS WRITTEN OVER EVERY LIVE PACK AND ADOPTED BY ITS MAINTAINER:
// a repository never published gets a root layer covering every pack it
// holds, its bitmap with it, named by the git that wrote it and decided by its
// committer; the node that built it installs it from its own files, with no
// GET of what it just stored; and a second publication with nothing outside
// the chain finds nothing to publish.
//
// Mutations: cover only the plan's packs less one, and the decide refuses the
// layer stale (rule 6d); adopt nothing, and the node fetches its own layer.
func TestAPublicationIsWrittenOverEveryLivePackAndAdoptedByItsMaintainer(t *testing.T) {
	t.Parallel()
	x := newFixture(t)
	x.pushN(3, 4<<10)
	c, rec, _ := x.compactorOf(nil)
	if out, err := publishWhileBusy(t, c, x.publicationCandidate()); out != compact.OutcomeApplied {
		t.Fatalf("the publication of three pushes: %s, %v; want applied", out, err)
	}
	ch := x.awaitInstalled(1)
	layer := ch.Rows[0].Layer
	if want := liveChecksums(x.rows()); !slices.Equal(layer.Packs, want) {
		t.Errorf("the root layer covers %v, want every live pack %v", layer.Packs, want)
	}
	if want := x.node().Pool().Version().String(); layer.Git != want || layer.BaseID != "" || len(layer.Parts) == 0 {
		t.Errorf("the root layer names git %q over base %q in %d parts, want git %s's, no base and a part at the "+
			"least", layer.Git, layer.BaseID, len(layer.Parts), want)
	}
	m := x.node().Metrics()
	if adopted, fetched := counter(m, metrics.CodeCacheLayers, "outcome", "adopted"),
		counter(m, metrics.CodeCacheLayers, "outcome", "installed"); adopted != 1 || fetched != 0 {
		t.Errorf("the maintainer installed its own layer %d times from its files and %d from the store, want "+
			"once from its files and never from the store", adopted, fetched)
	}
	if n := counter(rec, metrics.CodePublishRuns, "outcome", string(compact.OutcomeApplied)); n != 1 {
		t.Errorf("%s{outcome=applied} = %d, want 1", metrics.CodePublishRuns, n)
	}
	cand := code.PublicationCandidate{ID: x.rows().Repo.ID, Name: codetest.RepoName, Version: x.rows().Position}
	if out, err := publishWhileBusy(t, c, cand); out != compact.OutcomeNotDue {
		t.Errorf("a publication with the whole chain installed and nothing outside it: %s, %v; want not_due",
			out, err)
	}
}

// A PUBLICATION IS BUILT IN A SCRATCH NAMING EXACTLY ITS BASE: a layer is
// written over a chain file listing the layers it builds on and none above
// them — measured on git 2.56.0, the same write with the layers it replaces
// still named took minutes where it takes a second — so a publication that
// absorbs the chain's top layer is written in a scratch naming only the layers
// below it, and every scratch names exactly its plan's base.
//
// Mutation: link the whole installed chain into the scratch, and the third
// publication's scratch names the layer it replaces.
func TestAPublicationIsBuiltInAScratchNamingExactlyItsBase(t *testing.T) {
	t.Parallel()
	x := newFixture(t)
	w := watchedOver(x.node().Cache())
	c, _, _ := x.compactorOf(func(o *compact.Options) { o.Cache = w })
	publish := func(what string) {
		t.Helper()
		if out, err := publishWhileBusy(t, c, x.publicationCandidate()); out != compact.OutcomeApplied {
			t.Fatalf("%s: %s, %v; want applied", what, out, err)
		}
	}
	// A ROOT over ten pushes, a SMALL LAYER over one more above it, and then
	// a tail of two more pushes, which the split absorbs the small layer into.
	x.pushN(10, 4<<10)
	publish("the root over ten pushes")
	root := x.awaitInstalled(1).Rows[0].Layer.LayerID
	x.push(4 << 10)
	publish("a layer over one push")
	small := x.awaitInstalled(2).Rows[1].Layer.LayerID
	x.pushN(2, 4<<10)
	publish("a layer over two more pushes")
	ch := x.awaitInstalled(2)
	if ch.Rows[0].Layer.LayerID != root || slices.Contains(ids(ch.Rows...), small) {
		t.Fatalf("the chain after the third publication is %v, want the root %s kept and the small layer %s "+
			"absorbed", ids(ch.Rows...), root, small)
	}
	_, scratches := w.seen()
	want := [][]string{nil, {root}, {root}}
	if len(scratches) != len(want) {
		t.Fatalf("three publications made %d scratches: %v", len(scratches), scratches)
	}
	for i := range want {
		if !slices.Equal(scratches[i], want[i]) {
			t.Errorf("publication %d's scratch names the chain %v, want exactly its base %v", i+1, scratches[i],
				want[i])
		}
	}
}

// A PUBLICATION IS NEVER BUILT OVER A BASE THE STORE LOST: the maintainer
// installed its own layer from the files it wrote, so its copy holds a layer
// whose part the store has since lost — and every node that fetches the chain
// refuses it `missing`, and every layer above it. So before it builds over a
// base, the maintainer asks the store for every part of it, and a base the
// store lost is replaced with the new layer's tail in one root: the next
// publication after the loss repairs it.
//
// Mutation: build over the base the plan names without asking the store, and
// the new layer lands over the lost root.
func TestAPublicationIsNeverBuiltOverABaseTheStoreLost(t *testing.T) {
	t.Parallel()
	x := newFixture(t)
	x.pushN(10, 4<<10)
	c, _, l := x.compactorOf(nil)
	if out, err := publishWhileBusy(t, c, x.publicationCandidate()); out != compact.OutcomeApplied {
		t.Fatalf("the root's publication: %s, %v; want applied", out, err)
	}
	root := x.awaitInstalled(1).Rows[0].Layer
	if err := x.store.Delete(t.Context(), root.Parts[0].Object); err != nil {
		t.Fatal(err)
	}
	x.push(4 << 10)
	if out, err := publishWhileBusy(t, c, x.publicationCandidate()); out != compact.OutcomeApplied {
		t.Fatalf("the publication after the store lost the root: %s, %v; want applied", out, err)
	}
	ch := x.awaitInstalled(1)
	if got := ch.Rows[0].Layer; got.LayerID == root.LayerID || !slices.Equal(got.Packs, liveChecksums(x.rows())) {
		t.Errorf("the chain after the store lost its root is %v over %v, want one new root over every live pack",
			ids(ch.Rows...), got.Packs)
	}
	if n := l.count("code_publication_base_lost", `"level":"WARN"`); n != 1 {
		t.Errorf("code_publication_base_lost logged %d times, want once:\n%s", n, l)
	}
}

// A MAINTAINER WAITS FOR ITS INSTALLER: a layer of the rows' chain this node
// has neither installed nor refused is still at its installer, so the
// publication answers waiting — nothing admitted, no scratch made — rather
// than replace a layer a moment from being installed; once it is installed, the
// next publication plans over it.
//
// Mutation: treat a layer neither installed nor refused as refused, and the
// publication replaces it.
func TestAMaintainerWaitsForItsInstaller(t *testing.T) {
	t.Parallel()
	x := newFixture(t)
	// A ROOT the one push after it is too small to absorb, so the second
	// publication is a layer of its own above it.
	x.pushN(10, 4<<10)
	w := watchedOver(x.node().Cache())
	c, _, _ := x.compactorOf(func(o *compact.Options) { o.Cache = w })
	if out, err := publishWhileBusy(t, c, x.publicationCandidate()); out != compact.OutcomeApplied {
		t.Fatalf("the first publication: %s, %v; want applied", out, err)
	}
	x.awaitInstalled(1)
	x.push(4 << 10)
	w.mu.Lock()
	w.chainOf = func(ch cache.Chain) cache.Chain {
		ch.Installed = 0
		return ch
	}
	w.mu.Unlock()
	admitted, _ := w.seen()
	if out, err := c.Publish(t.Context(), x.publicationCandidate()); out != compact.OutcomeWaiting {
		t.Fatalf("a publication over a layer still at the installer: %s, %v; want waiting", out, err)
	}
	if admits, scratches := w.seen(); admits != admitted || len(scratches) != 1 {
		t.Errorf("a waiting publication was admitted %d times and made %d scratches, want none of either",
			admits-admitted, len(scratches)-1)
	}
	w.mu.Lock()
	w.chainOf = nil
	w.mu.Unlock()
	if out, err := publishWhileBusy(t, c, x.publicationCandidate()); out != compact.OutcomeApplied {
		t.Fatalf("the publication once the layer is installed: %s, %v; want applied", out, err)
	}
	if ch := x.awaitInstalled(2); len(ch.Rows) != 2 {
		t.Errorf("the chain is %v, want the new layer over the installed one", ids(ch.Rows...))
	}
}

// A LAYER PAST ITS PARTS IS REFUSED BEFORE ITS BUILD: a repository whose
// layer's estimate passes what code.MaxLayerParts parts of
// code.LayerPartBytes hold is refused `layer_too_large` with no slot reserved
// and no scratch made, and remembered at its version, so the next pass syncs
// nothing.
//
// Mutation: compare the estimate with one part rather than every part, and a
// layer of a few parts is refused.
func TestALayerPastItsPartsIsRefusedBeforeItsBuild(t *testing.T) {
	t.Parallel()
	x := newFixture(t)
	x.pushN(2, 4<<10)
	w := watchedOver(x.node().Cache())
	w.packsOf = func(packs []code.PackRow) []code.PackRow {
		packs = slices.Clone(packs)
		packs[0].Objects = code.MaxLayerParts * code.LayerPartBytes / 40
		return packs
	}
	c, rec, l := x.compactorOf(func(o *compact.Options) { o.Cache = w })
	report, err := c.PublishPass(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if report.Refused != 1 {
		t.Fatalf("the pass over a repository whose layer passes its parts reported %+v, want it refused", report)
	}
	if n := counter(rec, metrics.CodePublishRefusals, "reason", "layer_too_large"); n != 1 {
		t.Errorf("%s{reason=layer_too_large} = %d, want 1", metrics.CodePublishRefusals, n)
	}
	if n := l.count("code_publication_refused", `"refusal":"layer_too_large"`); n != 1 {
		t.Errorf("code_publication_refused layer_too_large logged %d times, want once:\n%s", n, l)
	}
	if admits, scratches := w.seen(); admits != 0 || len(scratches) != 0 {
		t.Errorf("the refused publication was admitted %d times and made %d scratches, want none", admits,
			len(scratches))
	}
	syncs, _ := w.counts()
	if _, err := c.PublishPass(t.Context()); err != nil {
		t.Fatal(err)
	}
	if again, _ := w.counts(); again != syncs {
		t.Errorf("a pass over the refused repository, unmoved, synced it again")
	}

	// AND A LAYER OF A FEW PARTS IS NOT: the estimate is held to every part.
	few := code.LayerPartBytes * 3 / 40
	got := compact.PlanLayerOf(cache.Chain{}, []code.PackRow{row("few", few, few*100)}, planCap, planCut, 0)
	if got.Verdict != compact.LayerDue {
		t.Errorf("a layer estimated at %d bytes, three parts' worth, planned %s, want a layer",
			compact.LayerEstimate(few), got.Verdict)
	}
}

// A PUBLICATION UPLOADS ITS PARTS AND OUTPUTS AT ONCE: every part of the layer
// and every output of its repack has begun its upload before any of them
// returns — a key is minted as its upload begins, and uploads one after
// another would age the first past what a record may name.
//
// Mutation: upload the layer after the outputs, and the first upload waits
// for one that has not begun.
func TestAPublicationUploadsItsPartsAndOutputsAtOnce(t *testing.T) {
	t.Parallel()
	x := newFixture(t)
	// NINE PACKS NOT FULL, one more than a layer may hold before its
	// publication repacks them: a repack output and the layer's part.
	x.pushN(compact.LayerRepackPacks+1, 4<<10)
	var (
		mu      sync.Mutex
		entered int
		all     = make(chan struct{})
	)
	rs := &recordingStore{Store: x.store}
	rs.hold = func(ctx context.Context) error {
		mu.Lock()
		entered++
		if entered == 2 {
			close(all)
		}
		mu.Unlock()
		select {
		case <-all:
			return nil
		case <-ctx.Done():
			return errHeld
		case <-time.After(30 * time.Second):
			return errors.New("an upload waited thirty seconds for the publication's other upload to begin")
		}
	}
	c, _, _ := x.compactorOf(func(o *compact.Options) { o.Objects = rs })
	before := x.rows()
	if out, err := publishWhileBusy(t, c, x.publicationCandidate()); out != compact.OutcomeApplied {
		t.Fatalf("the publication of nine packs: %s, %v; want applied", out, err)
	}
	if outs := outputs(before, x.rows()); len(outs) != 1 {
		t.Errorf("the publication's repack wrote %d outputs, want the nine packs merged into one: %+v", len(outs),
			outs)
	}
	if puts, _ := rs.stored(); len(puts) < 2 {
		t.Errorf("the publication stored %d objects, want its layer's part and its output", len(puts))
	}
}

// THE PUBLICATION'S ADMISSION IS RELEASED BEFORE ITS UPLOADS: once its build's
// last git process has ended its maintenance slot and its charge — a bitmap
// over the whole history — are given back, so an upload the store paces never
// idles the node's maintenance for every other repository. While its upload
// is held, another publication is admitted.
//
// Mutation: release the admission once the publication is answered, and the
// second admission is refused busy while the upload is held.
func TestThePublicationsAdmissionIsReleasedBeforeItsUploads(t *testing.T) {
	t.Parallel()
	x := newFixture(t)
	x.pushN(2, 16<<10)
	entered, release := make(chan struct{}, code.MaxLayerParts), make(chan struct{})
	rs := &recordingStore{Store: x.store}
	rs.hold = func(ctx context.Context) error {
		entered <- struct{}{}
		select {
		case <-release:
			return nil
		case <-ctx.Done():
			return errHeld
		}
	}
	c, _, _ := x.compactorOf(func(o *compact.Options) { o.Objects = rs })
	cand := x.publicationCandidate()
	done := make(chan compact.Outcome, 1)
	go func() {
		out, _ := publishWhileBusy(t, c, cand)
		done <- out
	}()
	select {
	case <-entered:
	case out := <-done:
		t.Fatalf("the publication ended %s before its upload", out)
	case <-time.After(time.Minute):
		t.Fatal("the publication reached no upload in a minute")
	}
	s, err := x.node().Cache().Sync(t.Context(), cand.ID, codetest.Linearizable)
	if err != nil {
		t.Fatal(err)
	}
	var adm *cache.Admission
	for deadline := time.Now().Add(10 * time.Second); ; {
		adm, err = x.node().Cache().AdmitPublication(s, 1, 0, code.DefaultLimits())
		if err == nil || !errors.Is(err, gitproc.ErrBusy) || time.Now().After(deadline) {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if err != nil {
		t.Errorf("a second admission while the first publication's upload is held = %v, want it granted", err)
	} else {
		adm.Release()
	}
	s.Release()
	close(release)
	if out := <-done; out != compact.OutcomeApplied {
		t.Errorf("the publication once its upload was let go: %s, want applied", out)
	}
}

// A PUBLICATION THE NODE CANNOT HOLD IS PUBLISHED WHEN THE NODE GROWS: an
// admission the node's memory refuses is counted `memory` and NEVER
// remembered (M3-decisions M3-O26) — the node's budget is read again at every
// pass, and the next pass asks again who maintains the repository — so the
// next pass, with no push between, plans it again and, once the memory holds
// it, publishes it.
//
// Mutation: remember the refusal as a plan with nothing to publish, and the
// second pass skips the repository.
func TestAPublicationTheNodeCannotHoldIsPublishedWhenTheNodeGrows(t *testing.T) {
	t.Parallel()
	x := newFixture(t)
	x.pushN(2, 4<<10)
	w := watchedOver(x.node().Cache())
	w.answerPublications(noMemoryToPublish)
	c, rec, _ := x.compactorOf(func(o *compact.Options) { o.Cache = w })
	report, err := c.PublishPass(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if report.Refused != 1 {
		t.Fatalf("the pass over a repository the node's memory cannot publish reported %+v, want it refused",
			report)
	}
	if n := counter(rec, metrics.CodePublishRefusals, "reason", "memory"); n != 1 {
		t.Errorf("%s{reason=memory} = %d, want 1", metrics.CodePublishRefusals, n)
	}
	w.answerPublications(nil)
	deadline := time.Now().Add(time.Minute)
	for {
		report, err = c.PublishPass(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		if report.Published == 1 || time.Now().After(deadline) {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if report.Published != 1 {
		t.Errorf("the pass once the node holds the publication reported %+v, want it published", report)
	}
}

// A PASS PUBLISHES ONLY WHAT ITS NODE MAINTAINS, asked by the publication's
// own charge: a node that is not a repository's maintainer syncs nothing of
// it, and the question is asked by cache.Cache.PublishCharge of the listing's
// live objects — the figure the maintainer is placed by.
//
// Mutation: publish what another node maintains, and the pass syncs it; ask by
// the layer's own objects, and the charge differs.
func TestAPassPublishesOnlyWhatItsNodeMaintains(t *testing.T) {
	t.Parallel()
	x := newFixture(t)
	x.pushN(2, 4<<10)
	w := watchedOver(x.node().Cache())
	var asked []int64
	c, _, _ := x.compactorOf(func(o *compact.Options) {
		o.Cache, o.Maintainers = w, maintainers{asked: &asked}
	})
	report, err := c.PublishPass(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	cand := x.publicationCandidate()
	if syncs, _ := w.counts(); syncs != 0 || report.Maintained != 0 || report.Listed != 1 {
		t.Errorf("a pass over a repository another node maintains synced it %d times and reported %+v, want it "+
			"listed and nothing more", syncs, report)
	}
	if want := x.node().Cache().PublishCharge(cand.LiveObjects); !slices.Equal(asked, []int64{want}) {
		t.Errorf("the pass asked who maintains the repository by %v, want the publication's charge %d", asked, want)
	}
}

// A COMPACTION LEAVES EVERY PUBLISHED PACK ALONE: a node's chain loads only
// over every pack it names, so the compaction's plan leaves every pack a
// published layer names out — a tail one push short of a roll, listed beside
// the pack a root layer covers, is no compaction at all, where without the
// layer the two are one roll — and nothing the committer would refuse stale is
// ever built.
//
// Mutation: plan over every live pack, and the compaction is built and refused
// stale.
func TestACompactionLeavesEveryPublishedPackAlone(t *testing.T) {
	t.Parallel()
	x := newFixture(t)
	x.pushN(code.CompactMinMerge, 4<<10)
	c, rec, _ := x.compactorOf(nil)
	if out, err := publishWhileBusy(t, c, x.publicationCandidate()); out != compact.OutcomeApplied {
		t.Fatalf("the publication of a roll's pushes: %s, %v; want applied", out, err)
	}
	x.awaitInstalled(1)
	// THE TAIL: as many pushes again as make a roll with the packs the root
	// layer covers — its repack merged the first pushes into one — and one
	// short of a roll on their own, so only a plan over a published pack is
	// due.
	x.pushN(code.CompactMinMerge-len(x.rows().Packs), 4<<10)
	before := x.rows()
	if len(before.Packs) != code.CompactMinMerge {
		t.Fatalf("the premise: the repository holds %d live packs, want a roll's %d listed", len(before.Packs),
			code.CompactMinMerge)
	}
	report := pollWhileBusy(t, c, rec)
	if report.Listed != 1 || report.Due != 0 || report.Compacted != 0 {
		t.Errorf("a pass over a tail one push short of a roll beside the packs a root layer covers reported %+v, "+
			"want it listed and nothing due", report)
	}
	if n := counter(rec, metrics.CodeCompactRefusals, "reason", string(code.RefusalStale)); n != 0 {
		t.Errorf("%s{reason=stale} = %d, want no compaction built to be refused", metrics.CodeCompactRefusals, n)
	}
	if after := x.rows(); len(after.Packs) != len(before.Packs) {
		t.Errorf("the rows hold %d live packs after the pass, and %d before", len(after.Packs), len(before.Packs))
	}
}

// A REPOSITORY IS NEVER COMPACTED AND PUBLISHED AT ONCE on one node: each
// would retire packs the other planned over, so a compaction asked while a
// publication of its repository is building answers busy, having synced
// nothing — and is planned again at the next pass.
//
// Mutation: let both build at once, and the compaction syncs and builds
// beside the publication.
func TestARepositoryIsNeverCompactedAndPublishedAtOnce(t *testing.T) {
	t.Parallel()
	x := newFixture(t)
	x.pushN(code.CompactMinMerge, 4<<10)
	faults := &codefault.Script{}
	held, reached := make(chan struct{}), make(chan struct{})
	faults.Arm(codefault.PublishAfterBuild, codefault.Action{Once: true, Block: held, Reached: reached})
	cc := countedOver(x.node().Cache())
	c, _, _ := x.compactorOf(func(o *compact.Options) { o.Cache, o.Faults = cc, faults })
	done := make(chan compact.Outcome, 1)
	go func() {
		out, _ := publishWhileBusy(t, c, x.publicationCandidate())
		done <- out
	}()
	select {
	case <-reached:
	case out := <-done:
		t.Fatalf("the publication ended %s before its build did", out)
	case <-time.After(time.Minute):
		t.Fatal("the publication built nothing in a minute")
	}
	syncs, _ := cc.counts()
	if out, err := c.Compact(t.Context(), x.candidate()); out != compact.OutcomeBusy {
		t.Errorf("a compaction while its repository's publication builds: %s, %v; want busy", out, err)
	}
	if again, _ := cc.counts(); again != syncs {
		t.Errorf("the compaction refused busy synced the repository anyway")
	}
	close(held)
	if out := <-done; out != compact.OutcomeApplied {
		t.Errorf("the publication once let go: %s, want applied", out)
	}
}

// A FULL STORE HOLDS BOTH LOOPS UNTIL IT HAS ROOM, never as a blip to retry:
// a compaction whose output, and a publication whose part, the object store
// refused for room (objstore.ErrFull) are each refused `store_full`, counted
// so, and their repository planned again by that loop only once the store
// shows the room — on a store that holds a reservation, once a pass's read of
// it holds what the build would store; on a provider's bucket, which says
// nothing, once the collector's interval has passed since the loop's refusal —
// and never remembered at its version, which no room coming back moves.
//
// Mutations: answer ErrFull `failed`, and the loops build and upload again at
// the next pass; hold the repository at its version, and neither loop runs
// again once the room is back.
func TestAFullStoreHoldsBothLoopsUntilItHasRoom(t *testing.T) {
	t.Parallel()
	full := &objstore.Full{Remedy: "the operator raises the bucket's reservation", Err: errors.New("no room")}
	for _, k := range []struct {
		name     string
		reserves bool
	}{{"a reserved store", true}, {"a provider's bucket", false}} {
		t.Run(k.name, func(t *testing.T) {
			t.Parallel()
			x := newFixture(t)
			x.pushN(code.CompactMinMerge, 4<<10)
			var (
				mu sync.Mutex
				// A debounce on, so the publication pass takes the pushes for
				// a burst that has settled.
				now = time.Now().Add(compact.PublishDebounce)
			)
			clock := func() time.Time {
				mu.Lock()
				defer mu.Unlock()
				return now
			}
			advance := func(d time.Duration) {
				mu.Lock()
				defer mu.Unlock()
				now = now.Add(d)
			}
			room := int64(0)
			rs := &recordingStore{Store: x.store}
			rs.refuse = func() error { return full }
			rs.usage = func() (objstore.Usage, bool, error) {
				mu.Lock()
				defer mu.Unlock()
				return objstore.Usage{Bytes: 1 << 30, MaxBytes: 1<<30 + room}, k.reserves, nil
			}
			cc := countedOver(x.node().Cache())
			c, rec, l := x.compactorOf(func(o *compact.Options) { o.Cache, o.Objects, o.Now = cc, rs, clock })

			if report := pollWhileBusy(t, c, rec); report.Refused != 1 {
				t.Fatalf("the compaction pass whose output the store refused for room reported %+v, want it "+
					"refused", report)
			}
			if report := publicationPassWhileBusy(t, c, rec); report.Refused != 1 {
				t.Fatalf("the publication pass whose part the store refused for room reported %+v, want it "+
					"refused", report)
			}
			if n := counter(rec, metrics.CodeCompactRefusals, "reason", string(code.RefusalStoreFull)); n != 1 {
				t.Errorf("%s{reason=store_full} = %d, want 1", metrics.CodeCompactRefusals, n)
			}
			if n := counter(rec, metrics.CodePublishRefusals, "reason", string(code.RefusalStoreFull)); n != 1 {
				t.Errorf("%s{reason=store_full} = %d, want 1", metrics.CodePublishRefusals, n)
			}
			if n := l.count("code_publication_refused", `"refusal":"store_full"`); n != 1 {
				t.Errorf("code_publication_refused store_full logged %d times, want once:\n%s", n, l)
			}
			synced, _ := cc.counts()
			_, handed := rs.stored()
			put := len(handed)

			// Still no room — and, on a provider's bucket, the collector's
			// interval not yet passed.
			advance(collect.CollectInterval / 2)
			if _, err := c.Poll(t.Context()); err != nil {
				t.Fatal(err)
			}
			if _, err := c.PublishPass(t.Context()); err != nil {
				t.Fatal(err)
			}
			if syncs, _ := cc.counts(); syncs != synced {
				t.Errorf("a pass of either loop while the store has no room synced the repository held back")
			}
			if _, sizes := rs.stored(); len(sizes) != put {
				t.Errorf("a pass while the store has no room handed it %d more uploads", len(sizes)-put)
			}

			rs.set(func(r *recordingStore) { r.refuse = nil })
			mu.Lock()
			room = 1 << 30
			mu.Unlock()
			advance(collect.CollectInterval / 2)
			if report := pollWhileBusy(t, c, rec); report.Compacted != 1 {
				t.Errorf("the compaction pass once the store has room reported %+v, want the repository compacted",
					report)
			}
			if report := publicationPassWhileBusy(t, c, rec); report.Published != 1 {
				t.Errorf("the publication pass once the store has room reported %+v, want the repository "+
					"published", report)
			}
		})
	}
}

// A REPOSITORY'S STORED PUBLICATION ANSWERS ITS OWN HOLD, as a compaction's
// outputs do (TestAStoredCompactionReleasesItsOwnHold), on a store that holds a
// reservation as on any: a repository whose first publication a full store
// refused is published again, once that one landed, as soon as the room holds
// its NEXT publication — a small tail's layer over the first — though the room
// is below what the first would have stored. The refusal it was held back for
// is answered by its own uploads, stored; a refusal of a later publication
// would set an estimate of its own.
//
// Mutation: keep the publication loop's own hold once its uploads are stored
// (Compactor.stored), and the next publication is skipped at every pass — held
// to the first one's estimate, which the room no longer reaches.
func TestAStoredPublicationReleasesItsOwnHold(t *testing.T) {
	t.Parallel()
	full := &objstore.Full{Remedy: "the operator raises the bucket's reservation", Err: errors.New("no room")}
	x := newFixture(t)
	// THE FIRST PUBLICATION: one push of two hundred files, its layer's
	// estimate many times the next one's.
	history := make([]int, 200)
	for i := range history {
		history[i] = 64
	}
	x.push(history...)
	published := x.rows().Packs
	var objects int64
	for _, p := range published {
		objects += p.Objects
	}
	first := compact.LayerEstimate(objects)

	var (
		mu sync.Mutex
		// A debounce on, so a pass takes the pushes for a burst that has
		// settled.
		now  = time.Now().Add(compact.PublishDebounce)
		room int64
	)
	clock := func() time.Time {
		mu.Lock()
		defer mu.Unlock()
		return now
	}
	setRoom := func(n int64) {
		mu.Lock()
		defer mu.Unlock()
		room = n
	}
	rs := &recordingStore{Store: x.store}
	rs.refuse = func() error { return full }
	rs.usage = func() (objstore.Usage, bool, error) {
		mu.Lock()
		defer mu.Unlock()
		return objstore.Usage{Bytes: 1 << 30, MaxBytes: 1<<30 + room}, true, nil
	}
	c, rec, _ := x.compactorOf(func(o *compact.Options) { o.Objects, o.Now = rs, clock })

	if report := publicationPassWhileBusy(t, c, rec); report.Refused != 1 {
		t.Fatalf("the pass whose part the store refused for room reported %+v, want it refused", report)
	}
	rs.set(func(r *recordingStore) { r.refuse = nil })
	setRoom(first)
	if report := publicationPassWhileBusy(t, c, rec); report.Published != 1 {
		t.Fatalf("the pass once the store had room for the first layer reported %+v, want it published", report)
	}
	x.awaitInstalled(1)

	// THE NEXT PUBLICATION: a tail of four small pushes — past a sixteenth of
	// the repository's objects, so it is due — whose layer, over the first,
	// weighs far less. The duty cycle's interval passed since the first.
	x.pushN(4, 1<<10)
	var tail int64
	for _, p := range x.rows().Packs {
		if !slices.ContainsFunc(published, func(q code.PackRow) bool { return q.PackID == p.PackID }) {
			tail += p.Objects
		}
	}
	second := compact.LayerEstimate(tail)
	if second >= first-1 {
		t.Fatalf("the premise: the next layer's estimate %d is under the first's %d", second, first)
	}
	setRoom(first - 1)
	mu.Lock()
	now = now.Add(compact.PublishMinInterval)
	mu.Unlock()
	if report := publicationPassWhileBusy(t, c, rec); report.Published != 1 {
		t.Errorf("the pass over the next publication, an estimate of %d bytes, with room for it and not for the "+
			"first's %d reported %+v, want it published: the repository's own uploads, stored, answered its hold",
			second, first, report)
	}
}

// publicationPassWhileBusy is c's publication pass, made again while it found
// its one publication busy or waiting (publishWhileBusy's reason).
func publicationPassWhileBusy(t testing.TB, c *compact.Compactor, rec *metrics.Recorder) compact.PublishReport {
	t.Helper()
	deadline := time.Now().Add(time.Minute)
	for {
		held := counter(rec, metrics.CodePublishRuns, "outcome", string(compact.OutcomeBusy)) +
			counter(rec, metrics.CodePublishRuns, "outcome", string(compact.OutcomeWaiting))
		report, err := c.PublishPass(t.Context())
		if err != nil {
			t.Fatalf("a publication pass: %v", err)
		}
		again := counter(rec, metrics.CodePublishRuns, "outcome", string(compact.OutcomeBusy)) +
			counter(rec, metrics.CodePublishRuns, "outcome", string(compact.OutcomeWaiting))
		if again == held || time.Now().After(deadline) {
			return report
		}
		time.Sleep(20 * time.Millisecond)
	}
}
