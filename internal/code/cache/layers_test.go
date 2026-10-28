package cache_test

import (
	"context"
	"crypto/sha1" //nolint:gosec // git's hashfile trailer is a SHA-1: a case reseals a file as git would
	"io"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/code"
	"github.com/crewlet/crewlet/internal/code/cache"
	"github.com/crewlet/crewlet/internal/code/gitproc"
	"github.com/crewlet/crewlet/internal/code/gitproc/gittest"
	"github.com/crewlet/crewlet/internal/objstore"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/statelog/metrics"
)

// THE PUBLISHED CHAIN A CASE INSTALLS: real layers over a sync world's own
// pushes, written the way a repository's maintainer writes them — each an
// incremental multi-pack-index with its bitmap over exactly its packs,
// written over a chain file naming exactly its base (--stdin-packs
// --no-write-chain-file --base), and a commit-graph layer over the same packs
// with version-2 changed-path filters (--stdin-packs --split=no-merge
// --changed-paths) — laid out as the rows name them (code.LayLayer), their
// bytes stored in parts (code.StoreLayer) in the world's object store, and
// published on its log in one record (fakeLog.publish).

// pushed is one push a case made: the commit its ref moved to, and its pack's
// row.
type pushed struct {
	oid string
	row code.PackRow
}

// pushN makes n pushes to repo's main branch, one pack each.
func (w *syncWorld) pushN(t testing.TB, repo string, n int) []pushed {
	t.Helper()
	out := make([]pushed, 0, n)
	for range n {
		oid, row := w.push(t, repo, "refs/heads/main", nil)
		out = append(out, pushed{oid: oid, row: row})
	}
	return out
}

// chainBuild is a scratch repository holding a world's packs, where layers
// are written one over another as a maintainer writes them, by the node's own
// git.
type chainBuild struct {
	w    *syncWorld
	git  *gittest.Client
	bare string
	// ids is the multi-pack-index layers written so far, root first: what the
	// scratch's chain file names, each next layer's base.
	ids []string
}

// newChainBuild is a scratch repository holding the packs of every push of
// pushes.
func (w *syncWorld) newChainBuild(t testing.TB, pushes ...[]pushed) *chainBuild {
	t.Helper()
	b := &chainBuild{w: w, git: gittest.NewClientWith(t, gittest.RequireNode(t)), bare: gittest.Empty(t)}
	for _, group := range pushes {
		for _, p := range group {
			writeBundleInto(t, filepath.Join(b.bare, "objects", "pack"), w.bundle(p.row.GitChecksum))
		}
	}
	return b
}

// writeBundleInto writes a bundle's three files into packDir.
func writeBundleInto(t testing.TB, packDir string, bundle gittest.Bundled) {
	t.Helper()
	at := int64(0)
	for _, s := range []struct {
		ext string
		n   int64
	}{{".pack", bundle.PackBytes}, {".idx", bundle.IndexBytes}, {".rev", bundle.ReverseBytes}} {
		write(t, filepath.Join(packDir, "pack-"+bundle.Checksum+s.ext), string(bundle.Bytes[at:at+s.n]))
		at += s.n
	}
}

// layer writes the next layer of the chain over group's packs — its refs at
// the newest commit of the group, which every layer below and this one close
// over — and answers its row, its parts stored, over the layer before it.
// overNone writes its multi-pack-index as a root whatever was written before
// while its row and its commit-graph layer still name the layer below: a layer
// over the wrong base (M16), whose files every check of their own passes.
func (b *chainBuild) layer(t testing.TB, group []pushed, overNone bool) code.LayerRef {
	t.Helper()
	bare := b.bare
	gittest.SetRefs(t, bare, map[string]string{"refs/heads/main": group[len(group)-1].oid})
	var names strings.Builder
	for _, p := range group {
		names.WriteString("pack-" + p.row.GitChecksum + ".idx\n")
	}
	base, baseID := "none", ""
	if len(b.ids) > 0 {
		baseID = b.ids[len(b.ids)-1]
		if !overNone {
			base = baseID
		}
	}
	out, stderr, err := b.git.Try(t, bare, strings.NewReader(names.String()), "-c",
		"pack.writeBitmapLookupTable=true", "multi-pack-index", "write", "--incremental", "--bitmap",
		"--stdin-packs", "--no-write-chain-file", "--base="+base, "--no-progress")
	if err != nil {
		t.Fatalf("git wrote no layer over %d packs: %v\n%s", len(group), err, stderr)
	}
	id := strings.TrimSpace(out)
	b.ids = append(b.ids, id)
	layers := filepath.Join(bare, "objects", "pack", "multi-pack-index.d")
	write(t, filepath.Join(layers, "multi-pack-index-chain"), strings.Join(b.ids, "\n")+"\n")
	if _, stderr, err = b.git.Try(t, bare, strings.NewReader(names.String()), "-c",
		"commitGraph.changedPathsVersion=2", "commit-graph", "write", "--stdin-packs", "--split=no-merge",
		"--changed-paths", "--no-progress"); err != nil {
		t.Fatalf("git wrote no commit-graph layer over %d packs: %v\n%s", len(group), err, stderr)
	}
	graphs := filepath.Join(bare, "objects", "info", "commit-graphs")
	lines := strings.Fields(read(t, filepath.Join(graphs, "commit-graph-chain")))
	graphID := lines[len(lines)-1]
	paths := [3]string{
		filepath.Join(layers, "multi-pack-index-"+id+".midx"),
		filepath.Join(layers, "multi-pack-index-"+id+".bitmap"),
		filepath.Join(graphs, "graph-"+graphID+".graph"),
	}
	// The base's graph layers are the chain file's lines below the layer's
	// own, which its commit-graph layer names as its BASE — the layer over
	// the wrong base included, whose graph layer is written over the chain.
	ref, err := code.LayLayer(t.Context(), paths[0], paths[1], paths[2], lines[:len(lines)-1], code.Pieces{})
	if err != nil {
		t.Fatalf("laying out layer %s: %v", id, err)
	}
	ref.BaseID, ref.Git, ref.TookMs = baseID, b.w.pool.Version().String(), 1
	if ref.Parts, err = code.StoreLayer(t.Context(), b.w.store, paths, ref.Sections(), nil); err != nil {
		t.Fatalf("storing layer %s: %v", id, err)
	}
	if err = ref.Check(); err != nil {
		t.Fatalf("layer %s: %v", id, err)
	}
	return ref
}

// buildChain is a chain of one layer a group, root first, each over the one
// before it.
func (w *syncWorld) buildChain(t testing.TB, groups ...[]pushed) []code.LayerRef {
	t.Helper()
	b := w.newChainBuild(t, groups...)
	refs := make([]code.LayerRef, 0, len(groups))
	for _, g := range groups {
		refs = append(refs, b.layer(t, g, false))
	}
	return refs
}

// layerIDs is the multi-pack-index layer ids of a chain, root first.
func layerIDs[L code.LayerRef | code.LayerRow](chain []L) []string {
	out := make([]string, 0, len(chain))
	for _, l := range chain {
		switch l := any(l).(type) {
		case code.LayerRef:
			out = append(out, l.LayerID)
		case code.LayerRow:
			out = append(out, l.Layer.LayerID)
		}
	}
	return out
}

// graphIDs is the commit-graph layer ids of a chain of refs, root first.
func graphIDs(chain []code.LayerRef) []string {
	out := make([]string, 0, len(chain))
	for _, l := range chain {
		if l.GraphID != "" {
			out = append(out, l.GraphID)
		}
	}
	return out
}

// chainFiles is what the incarnation at dir's two chain files list, a line
// an id: none for a file that is not there.
func chainFiles(t testing.TB, dir string) (midx, graph []string) {
	t.Helper()
	for _, f := range []struct {
		rel string
		out *[]string
	}{
		{"objects/pack/multi-pack-index.d/multi-pack-index-chain", &midx},
		{"objects/info/commit-graphs/commit-graph-chain", &graph},
	} {
		raw, err := os.ReadFile(filepath.Join(dir, filepath.FromSlash(f.rel)))
		if err == nil {
			*f.out = strings.Fields(string(raw))
		} else if !os.IsNotExist(err) {
			t.Fatal(err)
		}
	}
	return midx, graph
}

// partsOf is every stored object a chain's parts name.
func partsOf(chain ...code.LayerRef) []string {
	var out []string
	for _, l := range chain {
		for _, p := range l.Parts {
			out = append(out, p.Object.String())
		}
	}
	return out
}

// gets is how many times the world's store was asked for each of keys.
func (w *syncWorld) gets(keys []string) int {
	opens, _, _ := w.objects.counts()
	n := 0
	for key, count := range opens {
		if slices.Contains(keys, key.String()) {
			n += count
		}
	}
	return n
}

// layerCount is crewlet.code.cache.layers under outcome, as the world's
// recorder holds it.
func (w *syncWorld) layerCount(outcome string) uint64 {
	var n uint64
	for _, s := range w.rec.Read() {
		if s.Name == metrics.CodeCacheLayers && s.Attrs["outcome"] == outcome {
			n += s.Total
		}
	}
	return n
}

// timedStore is the world's store as a case watches it: every object's read
// begun only after a first byte of delay, when each began and ended (its
// reader closed: the object landed), and how many reads of each are in
// flight at once and at most — and, for a key a case holds, every read held
// until the case lets it go.
type timedStore struct {
	inner cache.Objects
	delay time.Duration

	mu             sync.Mutex
	opened, landed map[objstore.Key]time.Time
	inFlight, most map[objstore.Key]int
	hold           map[objstore.Key]chan struct{}
}

func newTimedStore(inner cache.Objects, delay time.Duration) *timedStore {
	return &timedStore{inner: inner, delay: delay, opened: map[objstore.Key]time.Time{},
		landed: map[objstore.Key]time.Time{}, inFlight: map[objstore.Key]int{}, most: map[objstore.Key]int{},
		hold: map[objstore.Key]chan struct{}{}}
}

func (s *timedStore) Open(ctx context.Context, obj objstore.Object) (io.ReadCloser, error) {
	s.mu.Lock()
	if _, ok := s.opened[obj.Key]; !ok {
		s.opened[obj.Key] = time.Now()
	}
	s.inFlight[obj.Key]++
	s.most[obj.Key] = max(s.most[obj.Key], s.inFlight[obj.Key])
	hold := s.hold[obj.Key]
	s.mu.Unlock()
	done := sync.OnceFunc(func() {
		s.mu.Lock()
		defer s.mu.Unlock()
		s.inFlight[obj.Key]--
		s.landed[obj.Key] = time.Now()
	})
	wait := func(ch <-chan struct{}) error {
		select {
		case <-ch:
			return nil
		case <-ctx.Done():
			done()
			return context.Cause(ctx)
		}
	}
	if hold != nil {
		if err := wait(hold); err != nil {
			return nil, err
		}
	}
	if s.delay > 0 {
		timer := make(chan struct{})
		time.AfterFunc(s.delay, func() { close(timer) })
		if err := wait(timer); err != nil {
			return nil, err
		}
	}
	rc, err := s.inner.Open(ctx, obj)
	if err != nil {
		done()
		return nil, err
	}
	return &counted{ReadCloser: rc, done: done}, nil
}

// holdKey holds every read of key until the answer is called.
func (s *timedStore) holdKey(key objstore.Key) func() {
	ch := make(chan struct{})
	s.mu.Lock()
	s.hold[key] = ch
	s.mu.Unlock()
	return sync.OnceFunc(func() {
		s.mu.Lock()
		delete(s.hold, key)
		s.mu.Unlock()
		close(ch)
	})
}

// flight is how many reads of key are in flight now, and the most at once.
func (s *timedStore) flight(key objstore.Key) (now, most int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.inFlight[key], s.most[key]
}

// times is when the read of each of keys first began and last landed: the
// earliest and the latest of either across them.
func (s *timedStore) times(keys []objstore.Key) (firstOpened, lastOpened, firstLanded, lastLanded time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, k := range keys {
		o, l := s.opened[k], s.landed[k]
		if firstOpened.IsZero() || o.Before(firstOpened) {
			firstOpened = o
		}
		if o.After(lastOpened) {
			lastOpened = o
		}
		if firstLanded.IsZero() || l.Before(firstLanded) {
			firstLanded = l
		}
		if l.After(lastLanded) {
			lastLanded = l
		}
	}
	return firstOpened, lastOpened, firstLanded, lastLanded
}

// keysOf is the stored objects of rows and of a chain's parts.
func keysOf(rows []code.PackRow, chain ...code.LayerRef) []objstore.Key {
	var out []objstore.Key
	for _, r := range rows {
		out = append(out, r.Object)
	}
	for _, l := range chain {
		for _, p := range l.Parts {
			out = append(out, p.Object)
		}
	}
	return out
}

// rowsOf is the pack rows of pushes.
func rowsOf(pushes []pushed) []code.PackRow {
	out := make([]code.PackRow, 0, len(pushes))
	for _, p := range pushes {
		out = append(out, p.row)
	}
	return out
}

// readsThroughTheBitmap runs git's bitmapped walk of every object over the
// incarnation at dir with the node's git — what the canary runs — and fails
// the case where git cannot.
func readsThroughTheBitmap(t testing.TB, dir string) {
	t.Helper()
	git := gittest.NewClientWith(t, gittest.RequireNode(t))
	out, stderr, err := git.Try(t, dir, nil, "rev-list", "--objects", "--use-bitmap-index", "--count", "--all")
	if err != nil || strings.Contains(stderr, "corrupt") {
		t.Fatalf("git cannot walk %s through its chain's bitmaps: %v\n%s%s", dir, err, out, stderr)
	}
}

// A FULL ROUND INSTALLS THE CHAIN BEFORE IT ANSWERS: a cold sync of a
// repository whose maintainer published a chain is answered with the chain
// in place — every layer's files, both chain files listing it from the rows,
// the record — so the copy's first clone walks through a bitmap rather than
// every pack's index, and git reads it.
//
// Mutation: hand the full round's layers to the installer instead of
// fetching them (planChain's want cleared and kickInstall) — the answer's copy
// holds no chain.
func TestAFullRoundInstallsTheChainBeforeItAnswers(t *testing.T) {
	t.Parallel()
	w := newSyncWorld(t, onMemory)
	repo := w.repo(t, 1)
	p := w.pushN(t, repo, 4)
	chain := w.buildChain(t, p[:2], p[2:3])
	w.log.publish(repo, chain, nil)

	c := w.open(t)
	s := w.sync(t, c, repo)
	if got := layerIDs(cache.Installed(c, repo)); !slices.Equal(got, layerIDs(chain)) {
		t.Fatalf("the cold sync's answer holds the chain %v, want the published %v", got, layerIDs(chain))
	}
	midx, graph := chainFiles(t, s.Dir)
	if !slices.Equal(midx, layerIDs(chain)) || !slices.Equal(graph, graphIDs(chain)) {
		t.Errorf("the chain files list %v and %v, want %v and %v", midx, graph, layerIDs(chain), graphIDs(chain))
	}
	for _, l := range chain {
		for _, rel := range []string{"objects/pack/multi-pack-index.d/multi-pack-index-" + l.LayerID + ".midx",
			"objects/pack/multi-pack-index.d/multi-pack-index-" + l.LayerID + ".bitmap",
			"objects/info/commit-graphs/graph-" + l.GraphID + ".graph"} {
			if !exists(filepath.Join(s.Dir, filepath.FromSlash(rel))) {
				t.Errorf("the answer's copy holds no %s", rel)
			}
		}
	}
	if exists(filepath.Join(s.Dir, "objects", "pack", "multi-pack-index")) {
		t.Error("a whole multi-pack-index sits beside the chain, which git reads instead of it")
	}
	readsThroughTheBitmap(t, s.Dir)
	if got := w.layerCount("installed"); got != uint64(len(chain)) {
		t.Errorf("crewlet.code.cache.layers{outcome=installed} = %d, want %d", got, len(chain))
	}
	if got := cache.ChainedRepos(c); got != 1 {
		t.Errorf("the chained gauge's total is %d, want the one repository", got)
	}
	if got := len(w.logs.lines(t, "code_layer_installed")); got != len(chain) {
		t.Errorf("code_layer_installed logged %d times, want %d", got, len(chain))
	}
}

// A DELTA NEVER DOWNLOADS A LAYER: a converged copy whose next delta names a
// chain is answered before any GET of its parts, the rows' chain recorded,
// and the installer — not the round — fetches and places it, behind the
// serving, within its pass.
//
// Mutation: let the delta fetch the chain inline (apply's delta calling the
// full round's planChain and placeChain) — the parts are fetched before the
// answer.
func TestADeltaHandsNewLayersToTheInstaller(t *testing.T) {
	t.Parallel()
	w := newSyncWorld(t, onMemory)
	repo := w.repo(t, 1)
	p := w.pushN(t, repo, 3)
	c := w.open(t)
	w.sync(t, c, repo)

	chain := w.buildChain(t, p[:2])
	w.log.publish(repo, chain, nil)
	s := w.sync(t, c, repo)
	if s.Kind != cache.SyncDelta {
		t.Fatalf("the sync after the publication was %s, want a delta", s.Kind)
	}
	if n := w.gets(partsOf(chain...)); n != 0 {
		t.Fatalf("the delta fetched the chain's parts %d times before it answered: a delta never downloads a layer", n)
	}
	if got, read := cache.ChainOf(c, repo); !read || !slices.Equal(layerIDs(got), layerIDs(chain)) {
		t.Fatalf("the delta recorded the rows' chain %v (read %t), want %v", layerIDs(got), read, layerIDs(chain))
	}
	if got := cache.Installed(c, repo); len(got) != 0 {
		t.Fatalf("the delta installed %v itself", layerIDs(got))
	}
	if q := cache.InstallQueued(c); !slices.Equal(q, []string{repo}) {
		t.Fatalf("the installer holds %v queued, want the repository", q)
	}
	s.Release()

	if took := cache.InstallPass(c); !slices.Equal(took, []string{repo}) {
		t.Fatalf("the installer's pass took up %v, want the repository", took)
	}
	if got := layerIDs(cache.Installed(c, repo)); !slices.Equal(got, layerIDs(chain)) {
		t.Fatalf("after the installer's pass the copy holds %v, want %v", got, layerIDs(chain))
	}
	if n := w.gets(partsOf(chain...)); n != len(partsOf(chain...)) {
		t.Errorf("the installer fetched the chain's parts %d times, want each once", n)
	}
	s = w.sync(t, c, repo)
	readsThroughTheBitmap(t, s.Dir)
	if midx, _ := chainFiles(t, s.Dir); !slices.Equal(midx, layerIDs(chain)) {
		t.Errorf("the copy the installer placed lists %v, want %v", midx, layerIDs(chain))
	}
}

// THE CHAIN HOLDS ITS PACKS: a publication's repack retires the packs its own
// new layer replaces, and between the delta that brings it and the
// installer's placement of the replacement, every pack the INSTALLED layer
// names stays in the copy — so the chain, and its bitmap, keeps serving —
// and leaves with the placement, which the next incarnation is built without.
// The placement's swap is logged and counted as one: the packs the old chain
// held alone dropped, graph=chain, since a chained copy holds no whole graph
// and the background indexing writes none beside it, and the layer it
// replaced counted cut.
//
// Mutations: let the delta take a retired pack an installed layer names out
// of the copy (apply's retired loop without the chained check) — its files
// leave with the delta's swap, and the old chain with them; count a chained
// copy's swap by its own-mode graph (swapped's chain case removed) — it reads
// dropped; count only what was leaving (swapped's chain-held loop removed) —
// the old chain's packs drop uncounted; count no layer a swap leaves behind
// (swapOnce's cut count removed) — the replaced layer leaves uncounted.
func TestTheChainHoldsItsPacksUntilItsReplacementIsPlaced(t *testing.T) {
	t.Parallel()
	w := newSyncWorld(t, onMemory)
	repo := w.repo(t, 1)
	p := w.pushN(t, repo, 3)
	old := w.buildChain(t, p[:2])
	w.log.publish(repo, old, nil)
	c := w.open(t)
	w.sync(t, c, repo).Release()
	if got := layerIDs(cache.Installed(c, repo)); !slices.Equal(got, layerIDs(old)) {
		t.Fatalf("the cold sync installed %v, want %v", got, layerIDs(old))
	}

	// A REPACKING PUBLICATION: the two packs merged into one, a root layer
	// over it, both inputs retired in the same record.
	merged := w.merged(t, p[0].row, p[1].row)
	replacement := w.buildChain(t, []pushed{{oid: p[1].oid, row: merged}})
	w.log.publish(repo, replacement, []code.PackRow{merged}, p[0].row.PackID, p[1].row.PackID)
	s := w.sync(t, c, repo)
	if got := layerIDs(cache.Installed(c, repo)); !slices.Equal(got, layerIDs(old)) {
		t.Fatalf("after the publication's delta the copy holds %v, want the old chain %v still", got, layerIDs(old))
	}
	for _, r := range p[:2] {
		for _, ext := range []string{".pack", ".idx", ".rev"} {
			if !exists(filepath.Join(s.Dir, "objects", "pack", "pack-"+r.row.GitChecksum+ext)) {
				t.Errorf("the delta took pack-%s%s out of the copy while the installed chain names it",
					r.row.GitChecksum, ext)
			}
		}
	}
	if leaving := cache.Leaving(c, repo); len(leaving) != 0 {
		t.Errorf("the packs leaving the copy are %v: the chain holds its packs", leaving)
	}
	readsThroughTheBitmap(t, s.Dir)
	held := s.Dir
	s.Release()

	cache.InstallPass(c)
	if got := layerIDs(cache.Installed(c, repo)); !slices.Equal(got, layerIDs(replacement)) {
		t.Fatalf("after the installer's pass the copy holds %v, want the replacement %v", got, layerIDs(replacement))
	}
	s = w.sync(t, c, repo)
	if s.Dir == held {
		t.Fatalf("the placement wrote into %s, the copy the delta answered", held)
	}
	for _, r := range p[:2] {
		if exists(filepath.Join(s.Dir, "objects", "pack", "pack-"+r.row.GitChecksum+".pack")) {
			t.Errorf("pack-%s is still in the copy the replacement was placed in: it left with the old chain",
				r.row.GitChecksum)
		}
	}
	if !exists(filepath.Join(s.Dir, "objects", "pack", "pack-"+merged.GitChecksum+".pack")) {
		t.Error("the copy holds no merged pack")
	}
	readsThroughTheBitmap(t, s.Dir)
	// The placement's swap says so: the two packs the old chain held alone
	// dropped with it, and a copy serving from a chain counted as one — it
	// holds no whole graph, and the background indexing writes none.
	line := swapLine(t, w)
	if line["packs_dropped"] != float64(2) || line["layers"] != float64(len(replacement)) ||
		line["graph"] != "chain" {
		t.Errorf("code_cache_swapped = %v, want the old chain's two packs dropped, the replacement's %d layers "+
			"and graph=chain", line, len(replacement))
	}
	if got := counter(w.rec, metrics.CodeCacheSwaps, "graph", "chain"); got != 1 {
		t.Errorf("%s{graph=chain} = %d, want the one placement", metrics.CodeCacheSwaps, got)
	}
	// And the layer it replaced is counted as it leaves, as one cut in
	// place would be.
	if got := w.layerCount("cut"); got != uint64(len(old)) {
		t.Errorf("crewlet.code.cache.layers{outcome=cut} = %d, want the %d layers the placement replaced", got,
			len(old))
	}
}

// A CHAINED COPY'S SWAP IS ONE SYNC A DIRECTORY, as an own copy's is
// (swap.go's syncStaged): the layer step links the layers the copy keeps,
// renames in those a placement fetched and writes both chain files from the
// rows into the staging, and syncs nothing at the step — nothing reads the
// staging before its rename — so each directory it wrote in is synced once
// with the rest of the staging: objects/pack, the two chain directories,
// crewlet/ for the index of pushes, the staged root by its state's write,
// and repos/ after the rename. A swap that carries the chain whole (packs of
// the tail leaving) and the installer's placement of a replacement, whose
// every layer is renamed in rather than linked, alike.
//
// Mutations: sync a chain file's directory at the layer step as an
// incarnation's are in place (placeLayers' writeChains given the cache's
// durableDir) — the two chain directories synced three times each; leave
// what the layer step wrote out of the staging's sync (syncStaged's wrote
// loop removed) — the placement's chain directories, holding no file it
// linked, never synced.
func TestAChainedCopysSwapSyncsEachDirectoryOnce(t *testing.T) {
	t.Parallel()
	// indexed makes the copy keep an index of pushes, whose runs the swap
	// links into crewlet/ — two pushes, each read by a delta that folds it in
	// before it answers — and answers the syncs every case's swap makes.
	indexed := func(t *testing.T, w *syncWorld, c *cache.Cache, repo string) map[string]int {
		t.Helper()
		for range 2 {
			w.push(t, repo, "refs/heads/main", nil)
			w.sync(t, c, repo)
		}
		if idx, ok := cache.Index(c, repo); !ok || len(idx.Runs) == 0 {
			t.Fatalf("the index of pushes is %+v (kept %t): the case needs runs to carry", idx, ok)
		}
		return map[string]int{".": 1, "objects/pack": 1, "objects/pack/multi-pack-index.d": 1,
			"objects/info/commit-graphs": 1, "crewlet": 1, "repos/": 1}
	}
	t.Run("carried", func(t *testing.T) {
		t.Parallel()
		w := newSyncWorld(t, onMemory)
		repo := w.repo(t, 1)
		p := w.pushN(t, repo, 4)
		chain := w.buildChain(t, p[:2])
		w.log.publish(repo, chain, nil)
		var syncs syncLog
		c := w.open(t, cache.SyncingWith(syncs.add))
		held := w.sync(t, c, repo)
		want := indexed(t, w, c, repo)
		// The tail's two first packs merged: they leave, and the chain,
		// naming neither, crosses the swap whole.
		w.compacted(t, repo, p[2].row, p[3].row)
		syncs.reset()

		s := w.sync(t, c, repo)
		if s.Dir == held.Dir {
			t.Fatalf("the round answered the incarnation a reader holds, %s: no swap", s.Dir)
		}
		if got := layerIDs(cache.Installed(c, repo)); !slices.Equal(got, layerIDs(chain)) {
			t.Fatalf("the next incarnation holds the chain %v, want %v carried", got, layerIDs(chain))
		}
		if got := syncs.swap(w); !maps.Equal(got, want) {
			t.Errorf("the swap synced %v, want each directory it wrote once — the two chain directories "+
				"among them — and repos/ after the rename: %v", got, want)
		}
		if midx, graph := chainFiles(t, s.Dir); !slices.Equal(midx, layerIDs(chain)) ||
			!slices.Equal(graph, graphIDs(chain)) {
			t.Errorf("the next incarnation's chain files list %v and %v, want %v and %v", midx, graph,
				layerIDs(chain), graphIDs(chain))
		}
		readsThroughTheBitmap(t, s.Dir)
	})
	t.Run("placed", func(t *testing.T) {
		t.Parallel()
		w := newSyncWorld(t, onMemory)
		repo := w.repo(t, 1)
		p := w.pushN(t, repo, 3)
		w.log.publish(repo, w.buildChain(t, p[:2]), nil)
		var syncs syncLog
		c := w.open(t, cache.SyncingWith(syncs.add))
		w.sync(t, c, repo)
		want := indexed(t, w, c, repo)
		// A repacking publication: the chain's two packs merged under one
		// root layer, which the installer fetches and places by a swap.
		merged := w.merged(t, p[0].row, p[1].row)
		replacement := w.buildChain(t, []pushed{{oid: p[1].oid, row: merged}})
		w.log.publish(repo, replacement, []code.PackRow{merged}, p[0].row.PackID, p[1].row.PackID)
		held := w.sync(t, c, repo)
		syncs.reset()

		cache.InstallPass(c)
		synced := syncs.swap(w)
		if got := layerIDs(cache.Installed(c, repo)); !slices.Equal(got, layerIDs(replacement)) {
			t.Fatalf("after the installer's pass the copy holds %v, want the replacement %v", got,
				layerIDs(replacement))
		}
		s := w.sync(t, c, repo)
		if s.Dir == held.Dir {
			t.Fatalf("the placement wrote into %s, the copy the delta answered", held.Dir)
		}
		if !maps.Equal(synced, want) {
			t.Errorf("the placement's swap synced %v, want each directory it wrote once — the two chain "+
				"directories its fetched layer was renamed into among them — and repos/ after the rename: %v",
				synced, want)
		}
		readsThroughTheBitmap(t, s.Dir)
	})
}

// A PLACEMENT SWAPS A COPY A READER MAY HOLD, and writes in place one nobody
// does: a full round on a copy a reader holds places its chain in the next
// incarnation — the reader's copy left whole, chain-free as it was handed to
// it, its packs there for the reads it has yet to make — and a copy's first
// round, which nobody can hold, places it in its own directory.
//
// Mutation: place in place on a copy a reader holds (placeChain's held case
// removed) — the chain lands in the reader's directory.
func TestAPlacementSwapsAHeldIncarnationAndWritesInPlaceOtherwise(t *testing.T) {
	t.Parallel()
	t.Run("held", func(t *testing.T) {
		t.Parallel()
		w := newSyncWorld(t, onMemory)
		repo := w.repo(t, 1)
		p := w.pushN(t, repo, 3)
		c := w.open(t)
		reader := w.sync(t, c, repo)
		chain := w.buildChain(t, p[:2])
		w.log.publish(repo, chain, nil)
		// The next round full — a copy marked to be read whole — while the
		// reader still holds the copy it was handed.
		cache.MarkWhole(c, repo, cache.RebuildShape, 0)
		s := w.sync(t, c, repo)
		if s.Kind != cache.SyncFull {
			t.Fatalf("the round was %s, want a full one", s.Kind)
		}
		if s.Dir == reader.Dir {
			t.Fatalf("the placement wrote into %s, the directory a reader holds", reader.Dir)
		}
		if midx, _ := chainFiles(t, s.Dir); !slices.Equal(midx, layerIDs(chain)) {
			t.Errorf("the next incarnation lists %v, want %v", midx, layerIDs(chain))
		}
		if midx, graph := chainFiles(t, reader.Dir); len(midx)+len(graph) != 0 {
			t.Errorf("the reader's copy lists the chain %v and %v: it was placed under the reader", midx, graph)
		}
		for _, r := range p {
			if !exists(filepath.Join(reader.Dir, "objects", "pack", "pack-"+r.row.GitChecksum+".pack")) {
				t.Errorf("the reader's copy lost pack-%s", r.row.GitChecksum)
			}
		}
		readsThroughTheBitmap(t, s.Dir)
	})
	t.Run("nobody holds it", func(t *testing.T) {
		t.Parallel()
		w := newSyncWorld(t, onMemory)
		repo := w.repo(t, 1)
		p := w.pushN(t, repo, 3)
		chain := w.buildChain(t, p[:2])
		w.log.publish(repo, chain, nil)
		c := w.open(t)
		s := w.sync(t, c, repo)
		if n := cache.Incarnation(s); n != 1 {
			t.Errorf("the cold sync answered incarnation %d, want its first: placed in place", n)
		}
		if midx, _ := chainFiles(t, s.Dir); !slices.Equal(midx, layerIDs(chain)) {
			t.Errorf("the copy lists %v, want %v", midx, layerIDs(chain))
		}
		if n := len(w.logs.lines(t, "code_cache_swapped")); n != 0 {
			t.Errorf("a copy nobody held was swapped %d times to place its chain", n)
		}
	})
}

// THE CANARY REFUSES A CHAIN OVER THE WRONG BASE (M16): a layer file does not
// name its base — the chain file's order does — so a chain file listing the
// layers in another order passes every check of the layers' own files and
// fails the first read through the bitmap ("invalid MIDX object position, MIDX
// is likely corrupt"). A placement reads through the chain once before any
// reader is handed it, so a placement whose chain file is reversed is
// abandoned, every layer it added refused corrupt, and the copy keeps serving
// — in place on a copy nobody holds and by a swap on one somebody may.
//
// Mutation: skip the canary (placeInPlace's and the swap's) — the reversed
// chain is installed.
func TestTheCanaryRefusesAChainOverTheWrongBase(t *testing.T) {
	t.Parallel()
	reversed := cache.ChainLinesWith(func(ids []string) []string {
		out := slices.Clone(ids)
		slices.Reverse(out)
		return out
	})
	check := func(t *testing.T, w *syncWorld, c *cache.Cache, repo string, chain []code.LayerRef) {
		t.Helper()
		if got := cache.Installed(c, repo); len(got) != 0 {
			t.Fatalf("the copy holds %v: a chain over the wrong base was placed", layerIDs(got))
		}
		for _, l := range chain {
			if got := cache.Refused(c, repo)[l.LayerID]; got != "corrupt" {
				t.Errorf("layer %s is refused %q, want corrupt", l.LayerID, got)
			}
		}
		if got := w.layerCount("corrupt"); got != uint64(len(chain)) {
			t.Errorf("crewlet.code.cache.layers{outcome=corrupt} = %d, want %d", got, len(chain))
		}
		if got := len(w.logs.lines(t, "code_layer_refused")); got != len(chain) {
			t.Errorf("code_layer_refused logged %d times, want once a layer", got)
		}
		s := w.sync(t, c, repo)
		if midx, graph := chainFiles(t, s.Dir); len(midx)+len(graph) != 0 {
			t.Errorf("the copy holds the chain files %v and %v after the placement was abandoned", midx, graph)
		}
		readsThroughTheBitmap(t, s.Dir)
	}
	t.Run("in place", func(t *testing.T) {
		t.Parallel()
		w := newSyncWorld(t, onMemory)
		repo := w.repo(t, 1)
		p := w.pushN(t, repo, 3)
		chain := w.buildChain(t, p[:1], p[1:2])
		w.log.publish(repo, chain, nil)
		c := w.open(t, reversed)
		w.sync(t, c, repo).Release()
		check(t, w, c, repo, chain)
	})
	t.Run("by a swap", func(t *testing.T) {
		t.Parallel()
		w := newSyncWorld(t, onMemory)
		repo := w.repo(t, 1)
		p := w.pushN(t, repo, 3)
		c := w.open(t, reversed)
		w.sync(t, c, repo).Release()
		chain := w.buildChain(t, p[:1], p[1:2])
		w.log.publish(repo, chain, nil)
		w.sync(t, c, repo).Release()
		cache.InstallPass(c)
		check(t, w, c, repo, chain)
		if n := len(w.logs.lines(t, "code_cache_swapped")); n != 0 {
			t.Errorf("the abandoned placement made %d swaps: nothing else of it was to swap", n)
		}
	})
}

// A CANARY THAT COULD NOT READ THE CHAIN REFUSES NOTHING: the canary tells a
// layer over the wrong base from a sound one only once git has read the chain
// through its bitmap — exited with a status that is not its answer, or named
// damage. One the read lane did not admit within SlotWait, every read slot
// held by the requests the lane serves, read nothing: the layers it was to
// judge are not placed, and are the installer's to try again — never refused
// corrupt while the rows name them, which kept them off the node until the
// maintainer's next publication or a restart, and counted a busy node's
// layers as damage.
//
//   - "in place": a cold copy's first full round places its chain in place;
//     its canary refused a slot, the round answers from the layers below the
//     chain's first (none here), and the installer places the chain once the
//     lane has room.
//   - "by a swap": the installer's swap places a delta's chain; its canary
//     refused a slot, the try waits installRetryBase as one that may pass
//     does, and the next places the chain.
//
// Mutation: take any failure of the canary for its verdict (canary answering
// errChainUnsound whatever its git's error) — every layer is refused corrupt.
func TestACanaryThatCouldNotReadTheChainRefusesNothing(t *testing.T) {
	t.Parallel()
	// busy holds every slot of w's read lane while the first canary waits
	// for one, and lets them go once its git has ended: refused ErrBusy
	// after SlotWait, as on a node whose requests fill the lane.
	busy := func(t *testing.T, w *syncWorld) func(*cache.Options) {
		var once atomic.Bool
		return cache.CanaryRunningWith(func(string) func() {
			if !once.CompareAndSwap(false, true) {
				return func() {}
			}
			var holds []func()
			for range w.pool.Slots(gitproc.ClassRead) {
				release, err := w.pool.HoldRead(context.Background())
				if err != nil {
					t.Errorf("holding a read slot for the canary to wait behind: %v", err)
					break
				}
				holds = append(holds, release)
			}
			return func() {
				for _, release := range holds {
					release()
				}
			}
		})
	}
	nothingRefused := func(t *testing.T, w *syncWorld, c *cache.Cache, repo string) {
		t.Helper()
		if got := cache.Refused(c, repo); len(got) != 0 {
			t.Errorf("the layers its canary could not read are refused %v, want none", got)
		}
		if got := w.layerCount("corrupt"); got != 0 {
			t.Errorf("crewlet.code.cache.layers{outcome=corrupt} = %d, want none", got)
		}
		if got := cache.Installed(c, repo); len(got) != 0 {
			t.Errorf("the copy holds %v, which no canary read", layerIDs(got))
		}
	}
	placed := func(t *testing.T, w *syncWorld, c *cache.Cache, repo string, chain []code.LayerRef) {
		t.Helper()
		if got := layerIDs(cache.Installed(c, repo)); !slices.Equal(got, layerIDs(chain)) {
			t.Fatalf("the installer's next try left the copy holding %v, want %v", got, layerIDs(chain))
		}
		readsThroughTheBitmap(t, w.sync(t, c, repo).Dir)
	}

	t.Run("in place", func(t *testing.T) {
		t.Parallel()
		w := newSyncWorld(t, onMemory)
		repo := w.repo(t, 1)
		p := w.pushN(t, repo, 2)
		chain := w.buildChain(t, p)
		w.log.publish(repo, chain, nil)
		c := w.open(t, busy(t, w))
		w.sync(t, c, repo).Release()
		nothingRefused(t, w, c, repo)
		if got := w.layerCount("failed"); got != uint64(len(chain)) {
			t.Errorf("crewlet.code.cache.layers{outcome=failed} = %d, want %d: a layer a try failed to place",
				got, len(chain))
		}
		if got := len(w.logs.lines(t, "code_layer_install_failed")); got != len(chain) {
			t.Errorf("code_layer_install_failed logged %d times, want once a layer", got)
		}
		cache.InstallPass(c)
		placed(t, w, c, repo, chain)
	})
	t.Run("by a swap", func(t *testing.T) {
		t.Parallel()
		w := newSyncWorld(t, onMemory)
		repo := w.repo(t, 1)
		p := w.pushN(t, repo, 2)
		clk := newClock()
		c := w.open(t, clk.on, busy(t, w))
		w.sync(t, c, repo).Release()
		chain := w.buildChain(t, p)
		w.log.publish(repo, chain, nil)
		w.sync(t, c, repo).Release()
		cache.InstallPass(c)
		nothingRefused(t, w, c, repo)
		if got := w.layerCount("failed"); got != uint64(len(chain)) {
			t.Errorf("crewlet.code.cache.layers{outcome=failed} = %d, want %d: a layer a try failed to place",
				got, len(chain))
		}
		at, failures := cache.InstallWait(c, repo)
		if want := clk.Now().Add(cache.InstallRetryBase); !at.Equal(want) || failures != 1 {
			t.Fatalf("the try is due again at %v after %d failures, want %v after one: a placement whose canary "+
				"could not run may pass", at, failures, want)
		}
		clk.Set(at)
		cache.InstallPass(c)
		placed(t, w, c, repo, chain)
	})
}

// A LAYER MISSING FROM THE STORE IS ASKED ABOUT ONCE AT THE LOG'S HEAD: a part
// the object store holds nothing for may be one a publication replaced since
// the rows were read, its parts collected — so the installer reads the rows
// once at the head (a linearizable sync), refuses the layer missing only
// while they still name it, and the copy serves in own mode; the next pass
// asks nothing again.
//
// Mutation: refuse at once, no read at the head (refuseFailed's Sync dropped)
// — no linearizable read follows the miss.
func TestAMissingLayerIsAskedAboutOnceAtTheHead(t *testing.T) {
	t.Parallel()
	w := newSyncWorld(t, onMemory)
	repo := w.repo(t, 1)
	p := w.pushN(t, repo, 2)
	c := w.open(t)
	w.sync(t, c, repo).Release()
	chain := w.buildChain(t, p)
	lost := chain[0].Parts[0].Object
	w.objects.mu.Lock()
	w.objects.fail = func(k objstore.Key) error {
		if k == lost {
			return objstore.ErrNotFound
		}
		return nil
	}
	w.objects.mu.Unlock()
	w.log.publish(repo, chain, nil)
	w.sync(t, c, repo).Release()
	heads := func() int {
		n := 0
		for _, call := range w.log.firstPages(repo) {
			if call.Level == statelog.ReadLinearizable {
				n++
			}
		}
		return n
	}
	before := heads()
	cache.InstallPass(c)
	if got := heads() - before; got != 1 {
		t.Errorf("the installer read the log's head %d times after the part went missing, want once", got)
	}
	if got := cache.Refused(c, repo)[chain[0].LayerID]; got != "missing" {
		t.Errorf("the layer the store lost is refused %q, want missing", got)
	}
	if got := cache.Installed(c, repo); len(got) != 0 {
		t.Errorf("the copy installed %v", layerIDs(got))
	}
	if !cache.WantsRun(c, repo) {
		t.Error("a copy with no chain installed wants no maintenance run: it serves in own mode, its index the " +
			"maintainer's to write")
	}
	if got := len(w.logs.lines(t, "code_layer_refused")); got != 1 {
		t.Errorf("code_layer_refused logged %d times, want once", got)
	}
	gets := w.gets(partsOf(chain...))
	cache.InstallPass(c)
	if got := heads() - before; got != 1 {
		t.Errorf("a pass after the refusal read the head again (%d reads in all)", got)
	}
	if got := w.gets(partsOf(chain...)); got != gets {
		t.Errorf("a pass after the refusal asked the store for the layer again (%d asks, then %d)", gets, got)
	}
}

// A LAYER IS INSTALLED ONLY OVER ITS OWN PACKS: one whose row names a pack the
// copy does not hold is not installed — and not fetched, its parts never
// asked of the store: a layer is valid only over every pack it names — and
// once the pack is there, the installer installs it.
//
// Mutation: drop installable's packs check — the layer is fetched over a
// pack the copy lacks (and refused by the canary, a GET spent).
func TestALayerIsInstalledOnlyOverItsOwnPacks(t *testing.T) {
	t.Parallel()
	w := newSyncWorld(t, onMemory)
	repo := w.repo(t, 1)
	p := w.pushN(t, repo, 3)
	chain := w.buildChain(t, p[:2])
	w.log.publish(repo, chain, nil)
	w.log.dropPack(repo, p[1].row.PackID)
	c := w.open(t)
	w.sync(t, c, repo).Release()
	if n := w.gets(partsOf(chain...)); n != 0 {
		t.Fatalf("the round asked the store for the layer %d times over a pack the copy lacks", n)
	}
	if got := cache.Installed(c, repo); len(got) != 0 {
		t.Fatalf("the copy installed %v over a pack it lacks", layerIDs(got))
	}
	if got := cache.Refused(c, repo); len(got) != 0 {
		t.Errorf("a layer over a pack the copy lacks is refused %v: it is only not yet installable", got)
	}

	w.log.write(repo, []code.PackRow{p[1].row}, nil)
	w.sync(t, c, repo).Release()
	cache.InstallPass(c)
	if got := layerIDs(cache.Installed(c, repo)); !slices.Equal(got, layerIDs(chain)) {
		t.Fatalf("with the pack there the copy holds %v, want %v", got, layerIDs(chain))
	}
}

// A FULL ROUND FETCHES ITS LAYERS BESIDE ITS PACKS: every part of every layer
// joins the round's one download queue behind its packs, so a layer's GET
// begins the moment a slot the packs no longer need frees — before the
// round's last pack has landed — and no layer waits for the one below it:
// at a first byte of 200 ms, a chain of three costs the round no first byte
// in series.
//
// Mutation: fetch the layers after the packs, one after another (install's
// layer loop moved past workers.Wait, each layer's parts waited for in turn)
// — the first layer's GET begins after the last pack landed, and no two
// layers' GETs overlap.
func TestAFullRoundFetchesItsLayersBesideItsPacks(t *testing.T) {
	t.Parallel()
	w := newSyncWorld(t, onMemory)
	repo := w.repo(t, 1)
	// Twenty packs: a round's share of sixteen GETs, then four more beside
	// the three layers' — so the layers' begin before the packs' last ends.
	p := w.pushN(t, repo, 20)
	chain := w.buildChain(t, p[:6], p[6:12], p[12:18])
	w.log.publish(repo, chain, nil)
	store := newTimedStore(w.objects, 200*time.Millisecond)
	c := w.open(t, func(o *cache.Options) { o.Objects = store })
	w.sync(t, c, repo).Release()
	if got := layerIDs(cache.Installed(c, repo)); !slices.Equal(got, layerIDs(chain)) {
		t.Fatalf("the round's answer holds %v, want %v", got, layerIDs(chain))
	}
	_, _, _, lastPack := store.times(keysOf(rowsOf(p)))
	firstLayer, lastLayer, firstLayerLanded, _ := store.times(keysOf(nil, chain...))
	if !firstLayer.Before(lastPack) {
		t.Errorf("the first layer's GET began %v after the round's last pack landed: a layer waits for no pack's "+
			"download, only for a slot", firstLayer.Sub(lastPack))
	}
	if !lastLayer.Before(firstLayerLanded) {
		t.Errorf("the last layer's GET began %v after the first layer landed: the layers' GETs overlap, none "+
			"waiting for the one below it", lastLayer.Sub(firstLayerLanded))
	}
}

// THE INSTALLER KEEPS HALF THE NODE'S FETCHES, ACROSS REPOSITORIES: with
// sixty-four repositories each a chain to install, it has a round's share of
// parts in flight from its first pass — never one repository at a time, and
// never more than the share — and a round started meanwhile is granted its
// GETs from the node's other half; and one repository's chain is placed by
// one task, once.
//
// Mutation: install one repository at a time (the installer's tasks bound at
// one) — the parts in flight stay at one.
func TestTheInstallerKeepsHalfTheNodesFetchesAcrossRepositories(t *testing.T) {
	t.Parallel()
	w := newSyncWorld(t, onMemory)
	first := w.repo(t, 1)
	p := w.pushN(t, first, 3)
	chain := w.buildChain(t, p[:1], p[1:2])
	store := newTimedStore(w.objects, 0)
	c := w.open(t, func(o *cache.Options) { o.Objects = store })
	const repos = 64
	ids := []string{first}
	for n := 2; n <= repos; n++ {
		id := w.repo(t, n)
		// The same packs under rows of its own, so one published chain
		// serves every repository.
		var rows []code.PackRow
		for _, r := range rowsOf(p) {
			rows = append(rows, w.storeBundle(t, w.bundle(r.GitChecksum), ""))
		}
		w.log.write(id, rows, map[string]string{"refs/heads/main": p[len(p)-1].oid})
		ids = append(ids, id)
	}
	for _, id := range ids {
		w.sync(t, c, id).Release()
		w.log.publish(id, chain, nil)
		w.sync(t, c, id).Release()
	}
	if q := cache.InstallQueued(c); len(q) != repos {
		t.Fatalf("the installer holds %d repositories queued, want %d", len(q), repos)
	}
	share := cache.FetchConcurrency
	if got := cache.InstallerShare(c); got != share {
		t.Fatalf("the installer's share is %d, want a round's, %d", got, share)
	}

	root := chain[0].Parts[0].Object
	release := store.holdKey(root)
	pass := make(chan []string, 1)
	go func() { pass <- cache.InstallPass(c) }()
	deadline := time.Now().Add(30 * time.Second)
	for {
		now, _ := store.flight(root)
		if now == share {
			break
		}
		if time.Now().After(deadline) {
			release()
			t.Fatalf("the installer has %d parts in flight after 30 s, want its share, %d, at once", now, share)
		}
		time.Sleep(5 * time.Millisecond)
	}
	// A ROUND MEANWHILE: a cold copy's packs, from the node's other half.
	other := w.repo(t, repos+1)
	w.pushN(t, other, 2)
	w.sync(t, c, other).Release()
	time.Sleep(50 * time.Millisecond)
	if now, most := store.flight(root); now != share || most != share {
		t.Errorf("the installer has %d parts in flight (at most %d), want its share, %d, and never more", now,
			most, share)
	}
	release()
	took := <-pass
	for len(cache.InstallQueued(c)) > 0 {
		took = append(took, cache.InstallPass(c)...)
	}
	slices.Sort(took)
	want := slices.Sorted(slices.Values(ids))
	if !slices.Equal(took, want) {
		t.Errorf("the installer took up %d repositories, want each of the %d once", len(took), len(want))
	}
	for _, id := range ids {
		if got := layerIDs(cache.Installed(c, id)); !slices.Equal(got, layerIDs(chain)) {
			t.Errorf("%s holds %v, want %v", id, got, layerIDs(chain))
		}
	}
	if _, most := store.flight(root); most > share {
		t.Errorf("the installer had %d of one part in flight at once, past its share, %d", most, share)
	}
	swaps := map[string]int{}
	for _, line := range w.logs.lines(t, "code_cache_swapped") {
		swaps[line["repo"].(string)]++
	}
	for _, id := range ids {
		if swaps[id] != 1 {
			t.Errorf("%s's chain was placed by %d swaps, want one", id, swaps[id])
		}
	}
}

// A ROUND WRITES NO INDEX BESIDE A CHAIN (incidental (b)): a full round past
// MidxRoundPacks writes its own whole multi-pack-index only where the copy
// holds no index of EITHER shape — a whole write deletes every layer of an
// installed chain and its chain file — so a copy's cold round and a boot's
// first round over a chain both leave it as it is.
//
// Mutation: guard the round's index on the whole multi-pack-index alone
// (indexed's DerivedLayers dropped) — the round deletes the chain.
func TestARoundWritesNoIndexBesideAChain(t *testing.T) {
	t.Parallel()
	w := newSyncWorld(t, onMemory)
	repo := w.repo(t, 1)
	p := w.pushN(t, repo, caseRoundPacks+1)
	chain := w.buildChain(t, p[:2])
	w.log.publish(repo, chain, nil)
	held := func(when string, s cache.Synced) {
		t.Helper()
		if midx, graph := chainFiles(t, s.Dir); !slices.Equal(midx, layerIDs(chain)) ||
			!slices.Equal(graph, graphIDs(chain)) {
			t.Errorf("%s the copy lists %v and %v, want the chain %v and %v", when, midx, graph, layerIDs(chain),
				graphIDs(chain))
		}
		if exists(filepath.Join(s.Dir, "objects", "pack", "multi-pack-index")) {
			t.Errorf("%s the round wrote a whole multi-pack-index beside the chain", when)
		}
		readsThroughTheBitmap(t, s.Dir)
	}
	c := w.open(t, cache.RoundPacksAt(caseRoundPacks))
	held("after the cold round", w.sync(t, c, repo))
	c = w.reopen(t, c, cache.RoundPacksAt(caseRoundPacks))
	held("after a boot's first round", w.sync(t, c, repo))
}

// A CHAINED COPY RUNS NO OWN-MODE WRITE (incidental (c)): the maintainer plans
// neither its whole index nor its whole graph on a copy that serves from an
// installed chain, however many packs gather beside it — either would delete
// the chain — and its plan is the node's tip over the tail alone, which once
// written over a tail that does not move is wanted no more.
//
// Mutation: let the plan ignore the mode (plan's chained case dropped) — the
// whole index is planned.
func TestAChainedIncarnationRunsNoOwnModeWrite(t *testing.T) {
	t.Parallel()
	w := newSyncWorld(t, onMemory)
	clk := newClock()
	repo := w.repo(t, 1)
	p := w.pushN(t, repo, 3)
	chain := w.buildChain(t, p[:2])
	w.log.publish(repo, chain, nil)
	c := w.open(t, clk.on, cache.MidxPacksAt(2))
	w.sync(t, c, repo).Release()
	for range 3 {
		w.push(t, repo, "refs/heads/main", nil)
	}
	w.sync(t, c, repo).Release()
	clk.Add(cache.MidxDebounce)
	index, graph, tip := cache.PlanSteps(c, repo)
	if index || graph {
		t.Errorf("the plan for a chained copy writes an index (%t) and a graph (%t): no own-mode write", index,
			graph)
	}
	if !tip {
		t.Error("the plan for a chained copy with a tail is not its tip: the tip alone, and nothing else")
	}
	if ran := cache.MaintainerPass(c); !slices.Equal(ran, []string{repo}) {
		t.Errorf("the maintainer ran %v, want the repository's tip", ran)
	}
	s := w.sync(t, c, repo)
	if midx, _ := chainFiles(t, s.Dir); !slices.Equal(midx, layerIDs(chain)) {
		t.Errorf("the copy lists %v, want the chain %v", midx, layerIDs(chain))
	}
	for _, own := range []string{"objects/pack/multi-pack-index", "objects/info/commit-graph"} {
		if exists(filepath.Join(s.Dir, filepath.FromSlash(own))) {
			t.Errorf("the chained copy holds own mode's %s", own)
		}
	}
	// Its tip written, a tail that has not moved wants nothing more.
	if cache.WantsRun(c, repo) {
		t.Error("the maintainer wants another run of a chained copy whose tip covers its tail")
	}
	if ran := cache.MaintainerPass(c); len(ran) != 0 {
		t.Errorf("the maintainer ran %v over a tail its tip covers", ran)
	}
}

// tipOf is the node's tip over chain in the incarnation at dir — the last
// line of its commit-graph-chain, listed after exactly chain's commit-graph
// layers — and what its file says of itself, failing t where the chain file
// lists anything else or the file is no commit-graph layer.
func tipOf(t testing.TB, dir string, chain []code.LayerRef) (string, code.GraphInfo) {
	t.Helper()
	_, lines := chainFiles(t, dir)
	layers := graphIDs(chain)
	if len(lines) != len(layers)+1 || !slices.Equal(lines[:len(layers)], layers) {
		t.Fatalf("commit-graph-chain lists %v, want the installed layers %v and one tip above them", lines, layers)
	}
	tip := lines[len(layers)]
	path := filepath.Join(dir, "objects", "info", "commit-graphs", "graph-"+tip+".graph")
	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("the tip the chain lists: %v", err)
	}
	defer func() { _ = f.Close() }()
	fi, err := f.Stat()
	if err != nil {
		t.Fatal(err)
	}
	info, err := code.ParseGraph(t.Context(), f, fi.Size(), code.Pieces{})
	if err != nil {
		t.Fatalf("the tip %s is no commit-graph layer: %v", tip, err)
	}
	return tip, info
}

// THE NODE'S TIP IS WRITTEN OVER THE TAIL ALONE (maintain.go's THE TIP): a
// chained copy's maintainer writes git's split commit-graph layer of exactly
// the commits the installed layers lack — the tail's, every push since the
// publication — over exactly those layers, its BASE their ids, with version-2
// changed-path filters; at once where the copy holds no tip, its packs not yet
// standing still (GRAPH FIRST); and WHOLE again once the tail moves, by the
// ordinary cadence, over every commit of the tail and the installed layers
// alone, the old tip off the chain and its file gone, never a tip stacked on a
// tip. The installed layer's file and line stay as they were, and the state
// records the tip.
//
// Mutations: write the tip without --split=no-merge (tipGraphArgs) — git
// merges the installed layer into a layer of its own, its id leaves the chain;
// write it over the chain as it stands (writeTip's reset of the chain file
// dropped) — the second tip is stacked on the first.
func TestTheTipIsWrittenOverTheTailAlone(t *testing.T) {
	t.Parallel()
	w := newSyncWorld(t, onMemory)
	clk := newClock()
	repo := w.repo(t, 1)
	p := w.pushN(t, repo, 4)
	chain := w.buildChain(t, p[:2])
	w.log.publish(repo, chain, nil)
	c := w.open(t, clk.on)
	s := w.sync(t, c, repo)
	layer := filepath.Join(s.Dir, "objects", "info", "commit-graphs", "graph-"+chain[0].GraphID+".graph")
	before := stat(t, layer)

	// GRAPH FIRST: no tip, so due with the clock never moved.
	if tip, wanted := cache.Tip(c, repo); tip != "" || !wanted {
		t.Fatalf("before any pass the copy holds tip %q, wanted %t: want none, wanted", tip, wanted)
	}
	if ran := cache.MaintainerPass(c); !slices.Equal(ran, []string{repo}) {
		t.Fatalf("the first pass ran %v, want the repository's tip at once", ran)
	}
	first, info := tipOf(t, s.Dir, chain)
	if !slices.Equal(info.Bases, graphIDs(chain)) || info.Commits != 2 || info.BloomVersion != 2 {
		t.Errorf("the tip is over %v with %d commits and filters of version %d; want over %v, the tail's 2 "+
			"commits, version 2", info.Bases, info.Commits, info.BloomVersion, graphIDs(chain))
	}
	if got, _ := cache.Tip(c, repo); got != first {
		t.Errorf("the copy records tip %q, and its chain file lists %s", got, first)
	}
	if doc := readStateDoc(t, s.Dir).Derived[string(cache.DerivedGraphLayers)]; doc.Tip != first ||
		!slices.Equal(doc.Layers, graphIDs(chain)) || doc.Step != string(cache.StepLayer) || doc.TipOver == "" {
		t.Errorf("the state records the graph chain as %+v, want the layer %v, the tip %s and the tail it "+
			"covered", doc, graphIDs(chain), first)
	}
	if ran := cache.MaintainerPass(c); len(ran) != 0 {
		t.Errorf("a second pass over a tail that has not moved ran %v", ran)
	}

	// THE TAIL MOVES: the ordinary cadence, then the tip whole again.
	w.push(t, repo, "refs/heads/main", nil)
	w.sync(t, c, repo).Release()
	if ran := cache.MaintainerPass(c); len(ran) != 0 {
		t.Errorf("a tip already held was written again before its packs stood still: %v", ran)
	}
	clk.Add(cache.MidxDebounce)
	if ran := cache.MaintainerPass(c); !slices.Equal(ran, []string{repo}) {
		t.Fatalf("once the tail stood still the pass ran %v, want the repository's tip", ran)
	}
	second, info := tipOf(t, s.Dir, chain)
	if !slices.Equal(info.Bases, graphIDs(chain)) || info.Commits != 3 {
		t.Errorf("the tip written again is over %v with %d commits; want over %v alone with every commit of the "+
			"tail, 3", info.Bases, info.Commits, graphIDs(chain))
	}
	if second == first || exists(filepath.Join(s.Dir, "objects", "info", "commit-graphs", "graph-"+first+".graph")) {
		t.Errorf("the first tip %s is still on disk beside the second %s: git expires a layer its chain no "+
			"longer names", first, second)
	}
	if after := stat(t, layer); !os.SameFile(before, after) {
		t.Errorf("the installed layer's file moved under the tip's writes")
	}
	readsThroughTheBitmap(t, s.Dir)
}

// A RETIREMENT KEEPS THE NODE'S TIP, AND EVERY OTHER CHANGE TO THE CHAIN TAKES
// IT (tipGoes): a compaction of the tail retires packs whose commits its
// output holds (X04), so the copy's next incarnation links the very tip, its
// chain file listing it; a log gone back removes a pack no retirement accounts
// for, which may take commits the tip names, so the next copy holds none; and
// a publication's placement moves the chain beneath any tip, so the copy it
// places holds none either — and each copy without one has it written again
// at once.
//
// Mutations: let the swap carry the tip whatever leaves (chainOf's tipGoes
// dropped) — the copy after the removal lists the old tip; let a swap carry no
// tip at all — the compaction's copy holds none.
func TestARetirementKeepsTheTipAndEveryOtherChangeTakesIt(t *testing.T) {
	t.Parallel()
	w := newSyncWorld(t, onMemory)
	clk := newClock()
	repo := w.repo(t, 1)
	p := w.pushN(t, repo, 4)
	chain := w.buildChain(t, p[:2])
	w.log.publish(repo, chain, nil)
	c := w.open(t, clk.on)
	held := w.sync(t, c, repo)
	if ran := cache.MaintainerPass(c); !slices.Equal(ran, []string{repo}) {
		t.Fatalf("the maintainer ran %v, want the repository's tip", ran)
	}
	tip, _ := tipOf(t, held.Dir, chain)
	tipFile := func(dir string) string {
		return filepath.Join(dir, "objects", "info", "commit-graphs", "graph-"+tip+".graph")
	}
	before := stat(t, tipFile(held.Dir))
	state := w.log.state()

	// A COMPACTION OF THE TAIL: the copy a reader holds is swapped.
	w.compacted(t, repo, p[2].row, p[3].row)
	retired := w.sync(t, c, repo)
	if retired.Dir == held.Dir {
		t.Fatalf("the compaction answered the copy a reader holds, %s: no swap", retired.Dir)
	}
	if got, _ := tipOf(t, retired.Dir, chain); got != tip {
		t.Errorf("after the retirement the copy lists tip %s, want the one it carried, %s", got, tip)
	}
	if after, err := os.Lstat(tipFile(retired.Dir)); err != nil || !os.SameFile(before, after) {
		t.Errorf("the retirement's copy holds the tip as %v (%v), want the very file it linked", after, err)
	}
	// Wanted again all the same: the compaction's output is a pack added to
	// the tail, which nothing the copy counts tells from a push's — written
	// once too often for a merge, never once too few for a push.
	if got, wanted := cache.Tip(c, repo); got != tip || !wanted {
		t.Errorf("the retirement's copy records tip %q, wanted %t: want %s, wanted", got, wanted, tip)
	}

	// A LOG GONE BACK: the compaction's output is no row's any more, a pack
	// removed with no retirement — and the tip goes with it.
	w.log.recreate(state)
	removed := w.sync(t, c, repo)
	if got, wanted := cache.Tip(c, repo); got != "" || !wanted {
		t.Errorf("after a pack was removed the copy records tip %q, wanted %t: want none, wanted", got, wanted)
	}
	if _, lines := chainFiles(t, removed.Dir); !slices.Equal(lines, graphIDs(chain)) {
		t.Errorf("after a pack was removed commit-graph-chain lists %v, want the installed layers alone %v", lines,
			graphIDs(chain))
	}
	if ran := cache.MaintainerPass(c); !slices.Equal(ran, []string{repo}) {
		t.Fatalf("after the removal the maintainer ran %v, want the tip again at once", ran)
	}
	tipOf(t, removed.Dir, chain)

	// A PUBLICATION over the whole tail, placed by the installer's swap: the
	// chain beneath the tip moved.
	published := w.newChainBuild(t, p)
	moved := []code.LayerRef{published.layer(t, p[:2], false), published.layer(t, p[2:], false)}
	w.log.publish(repo, moved, nil)
	w.sync(t, c, repo).Release()
	cache.InstallPass(c)
	placed := w.sync(t, c, repo)
	if got := layerIDs(cache.Installed(c, repo)); !slices.Equal(got, layerIDs(moved)) {
		t.Fatalf("the placement left the chain %v, want %v", got, layerIDs(moved))
	}
	if got, _ := cache.Tip(c, repo); got != "" {
		t.Errorf("the placed copy records tip %q over a chain it was not written over", got)
	}
	if _, lines := chainFiles(t, placed.Dir); !slices.Equal(lines, graphIDs(moved)) {
		t.Errorf("the placed copy's commit-graph-chain lists %v, want the new chain's %v", lines, graphIDs(moved))
	}
}

// A FAILING TIP IS DROPPED AND WRITTEN AGAIN AT ONCE: the node's tip a previous
// process left is held to its check by the boot's first round like every file
// it left, and kept where it passes — the very file, its chain line, its
// record, and the tail it covered (TipOver), so a restart over the same tail
// owes it no write; where it fails, it is no file to serve: the round writes
// the chain files again without it and takes its file out, and the
// maintainer writes it again at once (GRAPH FIRST) — the same bytes, git's
// split write being a function of the commits and the layers beneath it.
//
// Mutations: keep a tip whatever the round's check found of it (planChain's
// dropTip dropped) — the damaged tip stays listed; say a failing tip is
// fetched again as a published layer's file is (repairAction's tip case
// dropped) — the line says refetched; restore no TipOver at the boot
// (planChain's parse dropped) — the restart's pass writes the tip again.
func TestAFailingTipIsDroppedAndWrittenAgain(t *testing.T) {
	t.Parallel()
	w := newSyncWorld(t, onMemory)
	clk := newClock()
	repo := w.repo(t, 1)
	p := w.pushN(t, repo, 3)
	chain := w.buildChain(t, p[:2])
	w.log.publish(repo, chain, nil)
	c := w.open(t, clk.on)
	dir := w.sync(t, c, repo).Dir
	if ran := cache.MaintainerPass(c); !slices.Equal(ran, []string{repo}) {
		t.Fatalf("the maintainer ran %v, want the repository's tip", ran)
	}
	tip, _ := tipOf(t, dir, chain)
	tipFile := filepath.Join(dir, "objects", "info", "commit-graphs", "graph-"+tip+".graph")

	// SOUND, across a restart: kept as it is — and, its tail unmoved, owing
	// no write, the ordinary cadence passed included.
	before := stat(t, tipFile)
	c = w.reopen(t, c, clk.on)
	if s := w.sync(t, c, repo); s.Dir != dir {
		t.Fatalf("the restart's first round answered %s, want the copy at %s", s.Dir, dir)
	}
	if got, wanted := cache.Tip(c, repo); got != tip || wanted {
		t.Errorf("after a restart the copy records tip %q, wanted %t: want the sound one it held, %s, owing "+
			"nothing over the tail it covered", got, wanted, tip)
	}
	clk.Add(cache.MidxDebounce + cache.MidxMinInterval)
	if ran := cache.MaintainerPass(c); len(ran) != 0 {
		t.Errorf("after a restart the maintainer ran %v over a tail its tip still covers", ran)
	}
	if after := stat(t, tipFile); !os.SameFile(before, after) {
		t.Error("after a restart the sound tip's file was written again")
	}

	// FAILING its check: dropped, and written again at once.
	flipAt(t, tipFile, 12)
	c = w.reopen(t, c, clk.on)
	s := w.sync(t, c, repo)
	if got, wanted := cache.Tip(c, repo); got != "" || !wanted {
		t.Errorf("after its tip failed its check the copy records tip %q, wanted %t: want none, wanted", got, wanted)
	}
	if _, lines := chainFiles(t, s.Dir); !slices.Equal(lines, graphIDs(chain)) {
		t.Errorf("after its tip failed commit-graph-chain lists %v, want the installed layer alone %v", lines,
			graphIDs(chain))
	}
	if exists(tipFile) {
		t.Error("the failing tip's file is still in the copy")
	}
	// Logged as what was done about it: dropped, as a file the node writes
	// itself is — never refetched, which no row of the log could answer.
	var actions []any
	for _, l := range w.logs.lines(t, "code_cache_check_damaged") {
		if l["file"] == filepath.Join("objects", "info", "commit-graphs", "graph-"+tip+".graph") {
			actions = append(actions, l["action"])
		}
	}
	if !slices.Equal(actions, []any{"dropped"}) {
		t.Errorf("code_cache_check_damaged named the failing tip with actions %v, want [dropped]", actions)
	}
	if ran := cache.MaintainerPass(c); !slices.Equal(ran, []string{repo}) {
		t.Fatalf("after the failing tip was dropped the maintainer ran %v, want its tip again at once", ran)
	}
	if again, _ := tipOf(t, s.Dir, chain); again != tip {
		t.Errorf("the tip written again is %s, want the same bytes, %s", again, tip)
	}
	readsThroughTheBitmap(t, s.Dir)
}

// A SCRUB ACROSS THE TIP'S WRITE GIVES THE CHAIN FILE NO VERDICT (chainVerdict):
// a scrub holds commit-graph-chain to what the copy's record names of it, read
// before its first hash, while the maintainer's tip run rewrites that file IN
// PLACE — a reader may hold the copy, and neither takes a lock the other
// holds — and records the new tip once git has renamed the file in. A hash
// begun across the run holds a file of one moment to a record of another,
// which is neither sound nor damaged: the file is no finding, the repair owes
// nothing, and the next scrub finds it sound.
//
// Mutation: drop chainVerdict from the scrub — the rewritten chain file is a
// finding, and its repair is pending.
func TestAScrubAcrossTheTipsWriteGivesTheChainFileNoVerdict(t *testing.T) {
	t.Parallel()
	w := newSyncWorld(t, onMemory)
	clk := newClock()
	hold := newHeldHash()
	repo := w.repo(t, 1)
	p := w.pushN(t, repo, 3)
	chain := w.buildChain(t, p[:2])
	w.log.publish(repo, chain, nil)
	c := w.open(t, clk.on, func(o *cache.Options) { cache.WhileHashing(o, hold.at) })
	dir := w.sync(t, c, repo).Dir
	if ran := cache.MaintainerPass(c); !slices.Equal(ran, []string{repo}) {
		t.Fatalf("the maintainer ran %v, want the repository's tip", ran)
	}
	first, _ := tipOf(t, dir, chain)

	// The scrub due by its interval, held as it is about to hash
	// commit-graph-chain: the record it holds the file to is read.
	clk.Add(cache.ScrubInterval)
	w.sync(t, c, repo)
	hold.armed.Store("commit-graph-chain")
	scrubbed := make(chan []string, 1)
	go func() { scrubbed <- cache.ScrubPass(c) }()
	select {
	case <-hold.reached:
	case <-time.After(hangDetector):
		t.Fatal("the scrub never hashed the copy's commit-graph-chain")
	}
	// Beside it, the tail moves and the tip is written again in place.
	w.push(t, repo, "refs/heads/main", nil)
	if s := w.sync(t, c, repo); s.Dir != dir {
		t.Fatalf("the push was answered in %s, want the copy at %s in place", s.Dir, dir)
	}
	clk.Add(cache.MidxDebounce + cache.MidxMinInterval)
	if ran := cache.MaintainerPass(c); !slices.Equal(ran, []string{repo}) {
		t.Fatalf("beside the held scrub the maintainer ran %v, want the repository's tip", ran)
	}
	if second, _ := tipOf(t, dir, chain); second == first {
		t.Fatalf("the tip written over the moved tail is %s, the one the scrub's record names", second)
	}
	close(hold.release)
	select {
	case <-scrubbed:
	case <-time.After(hangDetector):
		t.Fatal("the scrub never ended once released")
	}

	if cache.RepairPending(c, repo) {
		t.Error("the scrub across the tip's write left a repair pending")
	}
	for _, l := range w.logs.lines(t, "code_cache_check_damaged") {
		t.Errorf("the scrub across the tip's write named %v damaged (%v)", l["file"], l["action"])
	}
}

// A REPORT ON A CHAINED COPY DROPS NOTHING (M3-O67): git names a damaged
// multi-pack-index or commit-graph by one part whether the copy holds a whole
// index or a chain, and a report reaches only the declarations whose files
// the copy holds — on a chained copy the installed chain's, which keep their
// files on a report. So each report makes the copy's scrub due, which holds
// every file of the chain to its check, and asks no repair round: nothing is
// dropped and the chain stays installed, its files the very inodes they were.
//
// Mutation: let a report reach every declaration of its part (the observer's
// image.holding intersection removed) — the own-mode index's and graph's
// Drop on a report queues a repair round.
func TestAReportOnAChainedCopyMakesItsScrubDue(t *testing.T) {
	t.Parallel()
	w := newSyncWorld(t, onMemory)
	repo := w.repo(t, 1)
	p := w.pushN(t, repo, 3)
	chain := w.buildChain(t, p[:2])
	w.log.publish(repo, chain, nil)
	var sweeps atomic.Int64
	c := w.open(t, func(o *cache.Options) { cache.AfterSweep(o, func(string) { sweeps.Add(1) }) })
	s := w.sync(t, c, repo)
	defer s.Release()
	files := []string{
		filepath.Join(s.Dir, "objects", "pack", "multi-pack-index.d", "multi-pack-index-chain"),
		filepath.Join(s.Dir, "objects", "pack", "multi-pack-index.d", "multi-pack-index-"+chain[0].LayerID+".midx"),
		filepath.Join(s.Dir, "objects", "info", "commit-graphs", "commit-graph-chain"),
		filepath.Join(s.Dir, "objects", "info", "commit-graphs", "graph-"+chain[0].GraphID+".graph"),
	}
	before := make([]os.FileInfo, len(files))
	for i, f := range files {
		before[i] = stat(t, f)
	}
	swept := sweeps.Load()

	for _, part := range []gitproc.Part{gitproc.PartMultiPackIndex, gitproc.PartCommitGraph} {
		cache.Report(c, derivedReport(s.Dir, part, time.Now()))
	}
	eventually(t, "every round the reports asked", func() bool { return cache.Settled(c, repo) })
	if n := sweeps.Load() - swept; n != 0 {
		t.Errorf("the reports swept the chained copy in %d full rounds, want none", n)
	}
	for _, part := range []string{"multi-pack-index", "commit-graph"} {
		if got := reports(w, part, "queued"); got != 1 {
			t.Errorf("reports{part=%s,outcome=queued} = %d, want the report counted once, as the scrub it asked",
				part, got)
		}
	}
	if lines := w.logs.lines(t, "code_cache_derived_repaired"); len(lines) != 0 {
		t.Errorf("code_cache_derived_repaired = %v, want nothing dropped", lines)
	}
	if why, _ := cache.ScrubDue(c, repo); why != "report" {
		t.Errorf("after the reports the copy's scrub is due for %q, want %q", why, "report")
	}
	for i, f := range files {
		if after, err := os.Lstat(f); err != nil || !os.SameFile(before[i], after) {
			t.Errorf("%s after the reports: %v, want the very file the chain was placed with", f, err)
		}
	}
	if got := layerIDs(cache.Installed(c, repo)); !slices.Equal(got, layerIDs(chain)) {
		t.Errorf("after the reports the copy holds %v, want the chain %v", got, layerIDs(chain))
	}
}

// A LAYER'S GIT IS READ AS IT IS WRITTEN: the release a published row names
// the git that wrote it by (code.LayerRef.Git, held to its spelling by the
// domain, which reads no git) is read back on a git node by
// gitproc.ParseRelease, the exact inverse of gitproc.Version.String — so the
// node's own release and both floors read back as themselves, every spelling
// the row's check admits parses, and every one it refuses is refused.
//
// Mutation: let ParseRelease read a version line (delegate to ParseVersion) —
// "2.56.0" is refused.
func TestALayersGitIsReadAsItIsWritten(t *testing.T) {
	t.Parallel()
	w := newSyncWorld(t, onMemory)
	repo := w.repo(t, 1)
	p := w.pushN(t, repo, 1)
	row := w.buildChain(t, p)[0]
	for _, v := range []gitproc.Version{w.pool.Version(), gitproc.NodeMinVersion, gitproc.ClientMinVersion} {
		got, err := gitproc.ParseRelease(v.String())
		if err != nil || got != v {
			t.Errorf("ParseRelease(%q) = %v, %v: want %v back", v.String(), got, err, v)
		}
	}
	for _, s := range []string{"2.56.0", "0.0.0", "10.20.30", "999999999.999999999.999999999"} {
		row.Git = s
		if err := row.Check(); err != nil {
			t.Fatalf("a row naming git %q: %v", s, err)
		}
		if _, err := gitproc.ParseRelease(s); err != nil {
			t.Errorf("a row's check admits git %q, and a git node cannot read it: %v", s, err)
		}
	}
	for _, s := range []string{"2.56", "v2.56.0", "02.56.0", "2.56.0.1", "2.56.0 ", "2.-1.0", "1000000000.0.0",
		"git version 2.56.0", ""} {
		row.Git = s
		if row.Check() == nil {
			t.Errorf("a row's check admits git %q", s)
		}
		if v, err := gitproc.ParseRelease(s); err == nil {
			t.Errorf("ParseRelease(%q) = %v: a spelling no row carries is no release", s, v)
		}
	}
}

// VERIFY NAMES A DAMAGED LAYER: every layer the copy installed is held to the
// row it was installed from, beside the trailers — so a bitmap whose header
// names another multi-pack-index, its own trailer made whole over it, is a
// finding naming the layer, which no trailer can see. And it reads each of a
// layer's files ONCE: the layer pass holds every file to its trailer as it
// holds it to its row, so the pass of the other derived files leaves an
// installed layer's files to it — read twice, the root layer of the largest
// repository the defaults admit is some 2.6 GB more read and hashed while the
// check holds its maintenance slot.
//
// Mutations: skip the layer pass (check's checkLayers) — nothing is found;
// hash an installed layer's files in the derived files' pass too
// (checkDerived's skip dropped) — each is hashed twice.
func TestVerifyNamesADamagedLayer(t *testing.T) {
	t.Parallel()
	w := newSyncWorld(t, onMemory)
	repo := w.repo(t, 1)
	p := w.pushN(t, repo, 3)
	chain := w.buildChain(t, p[:1], p[1:2])
	w.log.publish(repo, chain, nil)
	var mu sync.Mutex
	hashed := map[string]int{}
	c := w.open(t, func(o *cache.Options) {
		cache.WhileHashing(o, func(path string) {
			mu.Lock()
			hashed[path]++
			mu.Unlock()
		})
	})
	s := w.sync(t, c, repo)
	bitmap := filepath.Join(s.Dir, "objects", "pack", "multi-pack-index.d",
		"multi-pack-index-"+chain[1].LayerID+".bitmap")
	s.Release()
	// A chained copy as installed is exactly its rows: the chain's
	// directories and files are the image's, and every layer its row.
	mu.Lock()
	clear(hashed)
	mu.Unlock()
	if report, err := verify(t.Context(), c, repo, false); err != nil || len(report.Problems) != 0 {
		t.Fatalf("Verify of the copy as installed found %q (%v), want nothing", report.Problems, err)
	}
	mu.Lock()
	for _, l := range chain {
		for _, rel := range []string{"objects/pack/multi-pack-index.d/multi-pack-index-" + l.LayerID + ".midx",
			"objects/pack/multi-pack-index.d/multi-pack-index-" + l.LayerID + ".bitmap",
			"objects/info/commit-graphs/graph-" + l.GraphID + ".graph"} {
			if got := hashed[filepath.Join(s.Dir, filepath.FromSlash(rel))]; got != 1 {
				t.Errorf("Verify hashed %s %d times, want once", rel, got)
			}
		}
	}
	mu.Unlock()
	// The bitmap's header names its multi-pack-index by checksum, the twenty
	// bytes after its signature, version, flags and count (git's
	// bitmap_disk_header): one of them flipped, and the trailer resealed.
	flipAt(t, bitmap, 12)
	reseal(t, bitmap)
	report, err := verify(t.Context(), c, repo, false)
	if err != nil {
		t.Fatal(err)
	}
	problemsOf(t, "Verify's problems", report.Problems, "layer "+chain[1].LayerID+": ")
	for _, problem := range report.Problems {
		if strings.Contains(problem, "fails its checksum") {
			t.Errorf("Verify named %q: the bitmap's trailer is sound, and only its row says otherwise", problem)
		}
	}
}

// A REBUILD SEEDS EVERY LAYER IT CHECKED: a copy rebuilt after a damaged pack
// links into its next incarnation every installed layer whose packs the
// rebuild seeded and whose files pass code.CheckLayer there and then — no
// download of it — and fetches the rest, a spoiled layer among them.
//
// Mutation: seed a layer without holding it to its row (seedLayers'
// checkLayerAt dropped) — the spoiled layer is seeded, and never fetched.
func TestARebuildSeedsEveryLayerItChecked(t *testing.T) {
	t.Parallel()
	w := newSyncWorld(t, onMemory)
	repo := w.repo(t, 1)
	p := w.pushN(t, repo, 3)
	chain := w.buildChain(t, p[:1], p[1:2])
	w.log.publish(repo, chain, nil)
	c := w.open(t)
	s := w.sync(t, c, repo)
	damage(t, filepath.Join(s.Dir, "objects", "pack", "pack-"+p[2].row.GitChecksum+".pack"), 40, []byte("damage"))
	flipAt(t, filepath.Join(s.Dir, "objects", "pack", "multi-pack-index.d",
		"multi-pack-index-"+chain[1].LayerID+".bitmap"), 12)
	s.Release()
	report, err := verify(t.Context(), c, repo, true)
	if err != nil {
		t.Fatal(err)
	}
	if !report.Rebuilt || len(report.Problems) != 0 {
		t.Fatalf("Verify rebuilt %t, leaving %q: want a rebuild that left nothing", report.Rebuilt, report.Problems)
	}
	if got := w.gets(partsOf(chain[0])); got != 1 {
		t.Errorf("the sound layer's part was fetched %d times, want once: the rebuild links it", got)
	}
	if got := w.gets(partsOf(chain[1])); got != 2 {
		t.Errorf("the spoiled layer's part was fetched %d times, want twice: the rebuild fetches it again", got)
	}
	if got := layerIDs(cache.Installed(c, repo)); !slices.Equal(got, layerIDs(chain)) {
		t.Errorf("the rebuilt copy holds %v, want %v", got, layerIDs(chain))
	}
	readsThroughTheBitmap(t, w.sync(t, c, repo).Dir)
}

// A MAINTAINER'S OWN LAYER IS INSTALLED WITH NO DOWNLOAD (Cache.AdoptLayer):
// the files its node wrote in a scratch are staged, held to the row and
// handed to the installer, which places them once a round names the layer —
// counted adopted, no GET; and an adoption no round's chain names within
// objstore.RecordWithin is discarded.
//
// Mutation: let the installer skip the adoptions (fetchForInstall's claim
// dropped) — the layer is downloaded.
func TestAMaintainersOwnLayerIsInstalledWithNoDownload(t *testing.T) {
	t.Parallel()
	w := newSyncWorld(t, onMemory)
	clk := newClock()
	repo := w.repo(t, 1)
	p := w.pushN(t, repo, 2)
	c := w.open(t, clk.on)
	s := w.sync(t, c, repo)
	b := w.newChainBuild(t, p)
	chain := []code.LayerRef{b.layer(t, p, false)}
	sc, err := c.NewScratch(t.Context(), s, "0198a4f4-0000-7000-8000-000000000001")
	if err != nil {
		t.Fatal(err)
	}
	defer sc.Remove()
	b.into(t, sc.Dir)
	if err := c.AdoptLayer(t.Context(), repo, sc, chain[0]); err != nil {
		t.Fatalf("AdoptLayer: %v", err)
	}
	s.Release()
	w.log.publish(repo, chain, nil)
	w.sync(t, c, repo).Release()
	cache.InstallPass(c)
	if got := layerIDs(cache.Installed(c, repo)); !slices.Equal(got, layerIDs(chain)) {
		t.Fatalf("the copy holds %v, want %v", got, layerIDs(chain))
	}
	if got := w.gets(partsOf(chain...)); got != 0 {
		t.Errorf("the adopted layer was downloaded %d times", got)
	}
	if got := w.layerCount("adopted"); got != 1 {
		t.Errorf("crewlet.code.cache.layers{outcome=adopted} = %d, want 1", got)
	}

	// An adoption whose record never applied: discarded once RecordWithin
	// has passed.
	other := w.newChainBuild(t, p)
	stray := other.layer(t, p[:1], false)
	s = w.sync(t, c, repo)
	sc2, err := c.NewScratch(t.Context(), s, "0198a4f4-0000-7000-8000-000000000002")
	if err != nil {
		t.Fatal(err)
	}
	defer sc2.Remove()
	other.into(t, sc2.Dir)
	if err := c.AdoptLayer(t.Context(), repo, sc2, stray); err != nil {
		t.Fatalf("AdoptLayer: %v", err)
	}
	s.Release()
	if got := cache.StagedLayers(c)[repo]; !slices.Equal(got, []string{stray.LayerID}) {
		t.Fatalf("the adoptions held are %v, want the stray one", got)
	}
	clk.Add(objstore.RecordWithin)
	cache.ExpireStagedLayers(c)
	if got := cache.StagedLayers(c); len(got) != 0 {
		t.Errorf("after RecordWithin the adoptions held are %v, want none", got)
	}
}

// into copies the build's layer files, under the names git gives them, and
// its commit-graph-chain into the repository at dir: a maintainer's scratch
// once its build has run.
func (b *chainBuild) into(t testing.TB, dir string) {
	t.Helper()
	for _, rel := range []string{"objects/pack/multi-pack-index.d", "objects/info/commit-graphs"} {
		list, err := os.ReadDir(filepath.Join(b.bare, rel))
		if err != nil {
			t.Fatal(err)
		}
		for _, d := range list {
			write(t, filepath.Join(dir, rel, d.Name()), read(t, filepath.Join(b.bare, rel, d.Name())))
		}
	}
}

// flipAt inverts the byte at offset of the file at path, through its own
// inode.
func flipAt(t testing.TB, path string, offset int64) {
	t.Helper()
	if err := os.Chmod(path, 0o600); err != nil {
		t.Fatal(err)
	}
	f, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	b := make([]byte, 1)
	if _, err := f.ReadAt(b, offset); err != nil {
		t.Fatal(err)
	}
	b[0] ^= 0xff
	if _, err := f.WriteAt(b, offset); err != nil {
		t.Fatal(err)
	}
}

// reseal makes the hashfile at path whole again over what it holds: its last
// twenty bytes the SHA-1 of every byte before them, as git's
// finalize_hashfile writes them.
func reseal(t testing.TB, path string) {
	t.Helper()
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha1.Sum(body[:len(body)-sha1.Size]) //nolint:gosec // git's hashfile trailer is a SHA-1
	copy(body[len(body)-sha1.Size:], sum[:])
	if err := os.Chmod(path, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, body, 0o600); err != nil {
		t.Fatal(err)
	}
}
