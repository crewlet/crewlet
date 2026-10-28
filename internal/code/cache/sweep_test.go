package cache_test

import (
	"context"
	"maps"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/crewlet/crewlet/internal/code"
	"github.com/crewlet/crewlet/internal/code/cache"
	"github.com/crewlet/crewlet/internal/code/gitproc"
	"github.com/crewlet/crewlet/internal/code/gitproc/gittest"
	"github.com/crewlet/crewlet/internal/statelog/metrics"
)

// A DELTA SWEEPS NOTHING: the steady-state cost of a sync is proportional to
// what moved — between two rounds of one process nothing but this package
// writes the directory, so a delta neither lists nor sweeps it, in either of
// the sweep's halves. A stray pack (the packs' half), and a stray file at the
// incarnation's root and a loose ref (the first half), planted after the first
// round therefore survive a delta, and go with the full first round of the
// next process.
func TestADeltaSweepsNothing(t *testing.T) {
	t.Parallel()
	w := newSyncWorld(t, onMemory)
	repo := w.repo(t, 1)
	tip, _ := w.push(t, repo, "refs/heads/main", nil)
	c := w.open(t)
	dir := w.sync(t, c, repo).Dir
	other := w.client.Init(t)
	strayTip := w.client.Commit(t, other, map[string]string{"x": "stray\n"}, "stray")
	stray := gittest.Pack(t, w.client, dir, other, strayTip)
	strays := []string{
		filepath.Join(dir, "objects", "pack", "pack-"+stray+".idx"),
		filepath.Join(dir, "description"),
		filepath.Join(dir, "refs", "heads", "loose"),
	}
	write(t, strays[1], "a stray at the root\n")
	write(t, strays[2], tip+"\n")

	w.push(t, repo, "refs/heads/main", nil)
	w.sync(t, c, repo)
	for _, path := range strays {
		if !statOK(t, path) {
			t.Errorf("a delta round swept %s", path)
		}
	}

	c = w.reopen(t, c)
	w.sync(t, c, repo)
	for _, path := range strays {
		if statOK(t, path) {
			t.Errorf("the first round of a new process left %s, outside the image", path)
		}
	}
}

// A FULL ROUND SWEEPS A DEAD WRITER'S FILES BEFORE ITS INDEX: a maintenance
// run killed with its node leaves the index's lock and a bitmap's temporary
// file in objects/pack (git removes them only when it is asked to stop), and
// no reader of any ref reads either. Left there until the refs were in, the
// lock refused the round's own index — git writes no index over another
// writer's lock — and the index's failure path then removed it from under
// the sweep that had listed it, so the sweep's move failed and the round with
// it: the first sync of the repository after the crash answered an error. A
// file of objects/pack that is no pack's goes with the round's first half;
// only the pack files wait for the refs.
func TestAFullRoundSweepsADeadWritersFilesBeforeItsIndex(t *testing.T) {
	t.Parallel()
	w := newSyncWorld(t, onMemory)
	repo := w.repo(t, 1)
	for range caseRoundPacks - 1 {
		w.push(t, repo, "refs/heads/main", nil)
	}
	round := cache.RoundPacksAt(caseRoundPacks)
	c := w.open(t, round)
	dir := w.sync(t, c, repo).Dir
	w.push(t, repo, "refs/heads/main", nil)
	packDir := filepath.Join(dir, "objects", "pack")
	if statOK(t, filepath.Join(packDir, "multi-pack-index")) {
		t.Fatal("a round under its threshold wrote an index: the case needs one with none")
	}

	c.Stop(t.Context())
	dead := []string{
		filepath.Join(packDir, "multi-pack-index.lock"),
		filepath.Join(packDir, "tmp_bitmap_Xq3vRk"),
		filepath.Join(dir, "objects", "info", "commit-graph.lock"),
	}
	for _, path := range dead {
		write(t, path, "half written\n")
	}
	c = w.open(t, round)
	s, err := c.Sync(t.Context(), repo, linearizable)
	if err != nil {
		t.Fatalf("the first sync after a crash that left a maintenance run's files: %v", err)
	}
	defer s.Release()
	for _, path := range dead {
		if statOK(t, path) {
			t.Errorf("the full round left %s, a dead writer's", path)
		}
	}
	if !statOK(t, filepath.Join(s.Dir, "objects", "pack", "multi-pack-index")) {
		t.Errorf("the full round of %d packs answered with no index: a dead writer's lock refused it", caseRoundPacks)
	}
	if got := w.logs.lines(t, "code_cache_round_index_skipped"); len(got) != 0 {
		t.Errorf("the round's index was skipped: %v", got)
	}
	if got, want := w.image(t, c, repo), w.rebuilt(t, repo); got != want {
		t.Errorf("the image after the crash:\n%s\nwant a fresh cache's:\n%s", got, want)
	}
}

// THE SWEEP KEEPS EXACTLY THE DECLARED IMAGE, and Verify admits exactly what
// the sweep keeps: one predicate (image.imaged) over the fixed entries and the
// files the declarations own (derived.go, ADR-0056), so the two never disagree
// about what a cache is. Over a copy the maintainer has written — the
// multi-pack-index with its bitmap, the commit-graph — an undeclared entry is
// planted in every directory of the image, a loose ref among them, and a
// directory named for an index chain or a graph chain no declaration owns,
// beside each declaration's files held only while a write runs, with no write
// running. Verify names every one of them unexpected and nothing else; the
// first round of a new process moves every one of them into trash/ and
// nothing else, keeping the real index and its bitmap; and git reads the copy
// whole after.
//
// EVERY FILE OF THE IMAGE IS A REGULAR FILE (ADR-0056): a symbolic link at
// the commit-graph's name, at config's, at packed-refs', at HEAD's and at the
// state's, each to a file holding exactly what the cache keeps there, is
// outside the image whatever it points at. git reads the graph, the
// configuration or the refs wherever such a link leads, outside the
// incarnation, the budget and what a rebuild replaces; and it refuses the
// whole repository when HEAD is a link whose own text is no ref (setup.c,
// validate_headref) — Verify's fsck says so, every git process in the copy
// failing until the link is gone. Verify names each link unexpected, and
// config, HEAD and packed-refs missing; the round moves each into trash/
// without following it, leaving the file it points at as it was, writes
// config, HEAD, packed-refs and the state afresh as regular files, and git
// reads the copy again; and the incarnation records no graph, so the
// maintainer's next pass writes a regular one in the link's place, after which
// Verify finds nothing.
//
// Mutations: drop the multi-pack-index's owned files from its declaration, and
// the round trashes the real index and its bitmap; make imaged admit an owned
// name whatever its kind, and the directory planted at an owned name is kept;
// make imaged admit a fixed or declared file as anything but a directory, as
// each directory's grammar did before the grammar was one predicate, and every
// link is kept and admitted — HEAD's with it, so the copy's fsck fails after
// the round; give Verify a predicate of its own for objects/info, and the two
// sets differ.
func TestTheSweepKeepsExactlyTheDeclaredImage(t *testing.T) {
	t.Parallel()
	w := newSyncWorld(t, onMemory)
	clk := newClock()
	repo := w.repo(t, 1)
	for range 3 {
		w.push(t, repo, "refs/heads/main", nil)
	}
	c := w.open(t, clk.on)
	dir := w.sync(t, c, repo).Dir
	clk.Add(cache.MidxDebounce)
	if ran := cache.MaintainerPass(c); len(ran) != 1 {
		t.Fatalf("the maintainer ran %v, want the repository", ran)
	}
	packDir := filepath.Join(dir, "objects", "pack")
	derived := []string{filepath.Join(packDir, "multi-pack-index"), bitmapOf(t, packDir),
		filepath.Join(dir, "objects", "info", "commit-graph")}
	for _, path := range derived {
		if !statOK(t, path) {
			t.Fatalf("the maintainer left no %s: the case needs every derived file written", path)
		}
	}

	// Every entry planted outside the image, by its path relative to the
	// incarnation, each with a base name of its own; true for a directory.
	// A directory at an owned file's name is outside it too: a derived file
	// is a file. The installed chain's two directories are the image's — a
	// copy with no chain installed has neither, and an entry in one that no
	// declaration owns is outside it.
	cs := strings.Repeat("ab", 20)
	planted := map[string]bool{
		"description":                                     false,
		"refs/stray-in-refs":                              false,
		"refs/heads/loose":                                false,
		"refs/tags/stray-tag":                             false,
		"objects/stray-in-objects":                        false,
		"objects/info/stray-in-info":                      false,
		"objects/info/commit-graphs/stray-in-graphs":      false,
		"objects/pack/stray-in-pack":                      false,
		"objects/pack/multi-pack-index.d/stray-in-layers": false,
		"objects/pack/multi-pack-index.d/multi-pack-index-" + cs + ".midx": true,
		"objects/pack/multi-pack-index.lock":                               false,
		"objects/pack/tmp_bitmap_Xq3vRk":                                   false,
		"objects/info/commit-graph.lock":                                   false,
		"objects/pack/multi-pack-index-" + cs + ".rev":                     true,
	}
	for rel, isDir := range planted {
		path := filepath.Join(dir, filepath.FromSlash(rel))
		if isDir {
			write(t, filepath.Join(path, "inside"), "a stray directory's file\n")
			continue
		}
		write(t, path, "a stray\n")
	}
	// The links: the graph's, config's, packed-refs', HEAD's and the state's,
	// each to a file outside the incarnation holding exactly what the cache
	// keeps at its name — the graph's own bytes, the configuration the pool
	// requires, the copy's refs, its HEAD and its state — so nothing but the
	// entry's KIND puts any of them outside the image.
	outside := t.TempDir()
	links := map[string]string{
		"objects/info/commit-graph": read(t, derived[2]),
		"config":                    gitproc.RepoConfig,
		"packed-refs":               read(t, filepath.Join(dir, "packed-refs")),
		"HEAD":                      read(t, filepath.Join(dir, "HEAD")),
		"crewlet-state.json":        read(t, filepath.Join(dir, "crewlet-state.json")),
	}
	for rel, content := range links {
		at := filepath.Join(dir, filepath.FromSlash(rel))
		target := filepath.Join(outside, path.Base(rel))
		write(t, target, content)
		if err := os.Remove(at); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(target, at); err != nil {
			t.Fatal(err)
		}
	}
	outsideImage := slices.Concat(slices.Collect(maps.Keys(planted)), slices.Collect(maps.Keys(links)))
	slices.Sort(outsideImage)

	report, err := verify(t.Context(), c, repo, false)
	if err != nil {
		t.Fatalf("Verify of the planted copy: %v", err)
	}
	var unexpected []string
	for _, p := range report.Problems {
		if rel, ok := strings.CutPrefix(p, "unexpected "); ok {
			unexpected = append(unexpected, rel)
		}
	}
	slices.Sort(unexpected)
	if !slices.Equal(unexpected, outsideImage) {
		t.Errorf("Verify named unexpected\n\t%v\nwant exactly what was planted outside the image, the links "+
			"among it\n\t%v\n(all its problems: %q)", unexpected, outsideImage, report.Problems)
	}
	for _, fixed := range []string{"config", "HEAD", "packed-refs"} {
		if !slices.Contains(report.Problems, "missing "+fixed) {
			t.Errorf("Verify of a copy whose %s is a link named %q, want %s missing: a link is no file of the "+
				"image", fixed, report.Problems, fixed)
		}
	}
	// git itself refuses the copy: a HEAD that is a link whose text is no
	// ref is no repository to it, so the connectivity check never ran.
	problemsOf(t, "Verify of a copy whose HEAD is a link", report.Problems, "not a git repository")

	c = w.reopen(t, c, clk.on)
	before := map[string]bool{}
	for _, name := range names(t, w.path("trash")) {
		before[name] = true
	}
	s := w.sync(t, c, repo)
	var moved []string
	for _, name := range names(t, w.path("trash")) {
		if before[name] {
			continue
		}
		// A move into trash/ is named <a UUIDv7>-<its base name>.
		if len(name) <= 37 || name[36] != '-' {
			t.Fatalf("trash/ holds %q, which is no move's name", name)
		}
		moved = append(moved, name[37:])
	}
	slices.Sort(moved)
	var want []string
	for _, rel := range outsideImage {
		want = append(want, path.Base(rel))
	}
	slices.Sort(want)
	if !slices.Equal(moved, want) {
		t.Errorf("the first round of a new process moved into trash/\n\t%v\nwant exactly what Verify named "+
			"unexpected\n\t%v", moved, want)
	}
	at := func(rel string) string { return filepath.Join(s.Dir, filepath.FromSlash(rel)) }
	kept := []string{"objects/pack/multi-pack-index", "objects/pack/" + filepath.Base(derived[1])}
	for _, rel := range kept {
		if info, err := os.Lstat(at(rel)); err != nil || !info.Mode().IsRegular() {
			t.Errorf("%s after the sweep: %v, %v — want the declared derived file kept, a regular file", rel, info, err)
		}
	}
	if _, err := os.Lstat(at("objects/info/commit-graph")); !os.IsNotExist(err) {
		t.Errorf("the sweep left an entry at the commit-graph's name (%v), where it moved the link", err)
	}
	for _, fixed := range []string{"config", "HEAD", "packed-refs", "crewlet-state.json"} {
		if info, err := os.Lstat(at(fixed)); err != nil || !info.Mode().IsRegular() {
			t.Errorf("%s after the sweep: %v, %v — want the regular file the round writes in the link's place",
				fixed, info, err)
		}
	}
	if got := read(t, at("config")); got != gitproc.RepoConfig {
		t.Errorf("config after the sweep reads %q, want gitproc.RepoConfig", got)
	}
	if got, want := read(t, at("HEAD")), links["HEAD"]; got != want {
		t.Errorf("HEAD after the sweep reads %q, want %q", got, want)
	}
	for rel, content := range links {
		if got := read(t, filepath.Join(outside, path.Base(rel))); got != content {
			t.Errorf("the file the %s link pointed at reads %q after the sweep, want it untouched", rel, got)
		}
	}
	if _, err := runGit(t, w.pool, s.Dir, "", "fsck", "--connectivity-only", "--no-progress"); err != nil {
		t.Errorf("fsck after the sweep: %v", err)
	}
	// The incarnation records what the sweep left — no graph — so the
	// maintainer writes one at once (a copy with no graph gets it first).
	if ran := cache.MaintainerPass(c); len(ran) != 1 {
		t.Fatalf("the maintainer ran %v after the sweep, want the repository: it holds no graph", ran)
	}
	if info, err := os.Lstat(at("objects/info/commit-graph")); err != nil || !info.Mode().IsRegular() {
		t.Errorf("the commit-graph after the maintainer's pass: %v, %v — want the regular file it writes", info, err)
	}
	if report, err := verify(t.Context(), c, repo, false); err != nil || len(report.Problems) != 0 {
		t.Errorf("Verify after the round and the maintainer's pass = %q, %v; want a copy exactly its image",
			report.Problems, err)
	}
}

// A DERIVED FILE FOLLOWS ITS DECLARATION ON EVERY EVENT: a pack added keeps
// the multi-pack-index and the commit-graph (git reads a pack the index does
// not cover from the pack's own index, and the graph names commits that are
// still there), refs moved keep both (a bitmap names the commits it selected,
// whichever refs point at them), a compaction's retirement drops the index and
// keeps the graph (X04: a merge drops no commit), and a pack removed drops
// both (an index naming a missing pack fails every read, a graph may name
// commits that left with it) — and what the incarnation records it holds
// follows. The memo's answers, functions of object ids, and the index of
// pushes are kept by every change to the packs and the refs, and dropped with
// the history a log gone back replaces. The explicit expectations of today's
// declarations, each read at the one place its change is made: dropFor in
// place, and a swap's links (swap.go) where a pack leaves a copy a reader
// holds — the reader here holding every copy, each event's checked in the
// copy that answers it. And a published chain PLACED over own mode's files
// (EventLayersMoved) drops the index, its bitmap and the graph — git reads a
// whole index and a whole graph before a chain — and keeps the memo; the
// installed chain's own answer to a change is a CUT to the longest prefix it
// leaves valid (Prefix): a layer naming a pack that leaves goes with every
// layer above it, a pack outside the chain cuts nothing, and the chain moving
// keeps the layers the rows still name in turn — and the node's tip over the
// chain goes with every one of those changes but a retirement that keeps the
// whole chain beneath it (tipGoes): a retirement drops no commit (X04), where
// a pack removed may take commits the tip names and a cut or a chain moved is
// a base it was not written over.
//
// Mutations: declare the multi-pack-index dropped on a pack added, and the
// first delta takes it; declare the graph dropped on a retirement
// (OnPackRetired), and the compaction's swap leaves it behind; let prefixOf
// keep a layer above one a removed pack cuts, and the Prefix table's cut is
// past it; let tipGoes keep the tip over a removed tail pack, or take it on a
// retired one, and the table's tip column says so.
func TestADerivedFileFollowsItsDeclarationOnEveryEvent(t *testing.T) {
	t.Parallel()
	// THE PREFIX TABLE: a three-layer chain — packs a and b under layer 1, c
	// under 2, d under 3 — and e outside it, the tail.
	layer := func(seq int, id string, packs ...string) code.LayerRow {
		return code.LayerRow{Seq: seq, Layer: code.LayerRef{LayerID: strings.Repeat(id, 40), Packs: packs}}
	}
	chain := []code.LayerRow{layer(1, "1", "a", "b"), layer(2, "2", "c"), layer(3, "3", "d")}
	moved := slices.Clone(chain)
	moved[1] = layer(2, "4", "c")
	for _, tc := range []struct {
		name    string
		ev      cache.Event
		packs   []string
		want    []code.LayerRow
		failing []string
		keep    int
		// tip: the node's tip over the chain stays.
		tip bool
	}{
		{"a retired pack in layer 2", cache.EventPackRetired, []string{"c"}, nil, nil, 1, false},
		{"a removed pack in the root", cache.EventPackRemoved, []string{"b"}, nil, nil, 0, false},
		{"a retired tail pack", cache.EventPackRetired, []string{"e"}, nil, nil, 3, true},
		{"a removed tail pack", cache.EventPackRemoved, []string{"e"}, nil, nil, 3, false},
		{"the rows' chain as it is", cache.EventLayersMoved, nil, chain, nil, 3, false},
		{"the rows' chain without its top", cache.EventLayersMoved, nil, chain[:2], nil, 2, false},
		{"the rows' chain moved at layer 2", cache.EventLayersMoved, nil, moved, nil, 1, false},
		{"no chain in the rows", cache.EventLayersMoved, nil, nil, nil, 0, false},
		{"layer 3's files failing their check", cache.EventPackAdded, nil, nil, []string{chain[2].Layer.LayerID}, 2,
			false},
	} {
		got := cache.PrefixOf(chain, tc.ev, tc.packs, tc.want, tc.failing)
		if got != tc.keep {
			t.Errorf("the prefix %s keeps is %d layers, want %d", tc.name, got, tc.keep)
		}
		if stays := !cache.TipGoes(chain, got, tc.ev); stays != tc.tip {
			t.Errorf("the node's tip over the chain stays across %s: %t, want %t", tc.name, stays, tc.tip)
		}
	}

	w := newSyncWorld(t, onMemory)
	clk := newClock()
	repo := w.repo(t, 1)
	var rows []code.PackRow
	var tip string
	for range 3 {
		oid, row := w.push(t, repo, "refs/heads/main", nil)
		rows, tip = append(rows, row), oid
	}
	c := w.open(t, clk.on)
	first := w.sync(t, c, repo)
	clk.Add(cache.MidxDebounce)
	if ran := cache.MaintainerPass(c); len(ran) != 1 {
		t.Fatalf("the maintainer ran %v, want the repository", ran)
	}
	answer := memoKeys("events", 1)
	memoize(t, c, first, answer)
	held := func(event string, s cache.Synced, want map[string]bool) {
		t.Helper()
		packDir := filepath.Join(s.Dir, "objects", "pack")
		bitmaps, err := filepath.Glob(filepath.Join(packDir, "*.bitmap"))
		if err != nil {
			t.Fatal(err)
		}
		for name, got := range map[string]bool{
			"multi-pack-index": statOK(t, filepath.Join(packDir, "multi-pack-index")),
			"bitmap":           len(bitmaps) == 1,
			"commit-graph":     statOK(t, filepath.Join(s.Dir, "objects", "info", "commit-graph")),
			"memo":             cache.MemoEntryFile(s, answer[0]) != "",
		} {
			if got != want[name] {
				t.Errorf("after %s, the copy's %s on disk is %t, want %t", event, name, got, want[name])
			}
		}
	}
	every := map[string]bool{"multi-pack-index": true, "bitmap": true, "commit-graph": true, "memo": true}
	before := w.log.state()

	oid, row := w.push(t, repo, "refs/heads/main", nil)
	rows, tip = append(rows, row), oid
	held("a delta that adds a pack", w.sync(t, c, repo), every)

	w.tag(t, repo, "v1", false)
	held("a delta that only moves refs", w.sync(t, c, repo), every)

	oid, row = w.push(t, repo, "refs/heads/main", nil)
	rows, tip = append(rows, row), oid
	w.sync(t, c, repo)
	// THE INDEX OF PUSHES is built over the push rows, which none of those
	// moved: every delta above folded its push as a run, and every run is
	// still there.
	idx, ok := cache.Index(c, repo)
	if !ok || len(idx.Runs) != 3 {
		t.Fatalf("the index of pushes after three deltas is %+v (kept %t), want a run of each", idx, ok)
	}

	merged := w.compacted(t, repo, rows[0], rows[1])
	retired := w.sync(t, c, repo)
	if retired.Dir == first.Dir {
		t.Fatalf("the compaction answered the copy a reader holds, %s: no swap", retired.Dir)
	}
	held("a delta that retires two packs", retired, map[string]bool{"commit-graph": true, "memo": true})
	if idx, ok := cache.Index(c, repo); !ok || len(idx.Runs) != 3 {
		t.Errorf("the index of pushes after the compaction is %+v (kept %t), want its three runs carried", idx, ok)
	}
	var runs []string
	for _, n := range idx.Runs {
		runs = append(runs, cache.RunRel(n))
	}

	// THE CHAIN MOVING over own mode's files: a publication over every pack
	// the copy holds, placed by the installer — a swap, the copy being held.
	clk.Add(cache.MidxMinInterval)
	if ran := cache.MaintainerPass(c); len(ran) != 1 {
		t.Fatalf("the maintainer ran %v after the compaction, want the repository", ran)
	}
	held("the maintainer's pass after the compaction", w.sync(t, c, repo), every)
	group := []pushed{{row: merged}, {row: rows[2]}, {row: rows[3]}, {oid: tip, row: rows[4]}}
	w.log.publish(repo, w.buildChain(t, group), nil)
	w.sync(t, c, repo).Release()
	cache.InstallPass(c)
	placed := w.sync(t, c, repo)
	held("a published chain placed", placed, map[string]bool{"memo": true})
	if idx, ok := cache.Index(c, repo); !ok || len(idx.Runs) != 3 {
		t.Errorf("the index of pushes after the chain's placement is %+v (kept %t), want its three runs carried",
			idx, ok)
	}

	w.log.recreate(before)
	s := w.sync(t, c, repo)
	held("a full round that removes the packs a log gone back no longer names", s, nil)
	for _, run := range runs {
		if statOK(t, filepath.Join(s.Dir, run)) {
			t.Errorf("after the log went back, %s is on disk: a log gone back drops the index, whose positions "+
				"name other records now", run)
		}
	}
	// What the state records follows: no index over the packs, no bitmap,
	// no graph, and an index of pushes made anew at the round's position,
	// holding nothing yet.
	doc := readStateDoc(t, s.Dir)
	if prov, ok := doc.Derived[string(cache.DerivedProvenance)]; len(doc.Derived) != 1 || !ok || prov.Bytes != 0 {
		t.Errorf("the state after the drop records %+v, want only an empty index of pushes", doc.Derived)
	}
	if idx, ok := cache.Index(c, repo); !ok || len(idx.Runs) != 0 || idx.From != s.Position.Packed() ||
		idx.Through != s.Position.Packed() {
		t.Errorf("the index of pushes after the log went back is %+v (kept %t), want one made anew at %d", idx, ok,
			s.Position.Packed())
	}
	if packs, bitmap, graph := cache.Indexed(c, repo); packs != 0 || bitmap || graph {
		t.Errorf("the incarnation after the drop records an index over %d packs (bitmap %t) and a graph %t, "+
			"want none", packs, bitmap, graph)
	}
}

// THE CODE READS THE DECLARATIONS, never a constant beside them: a cache
// opened over declarations in which the commit-graph is dropped when the refs
// move drops it on a delta that only moves refs, before the new packed-refs
// is in, and records that it holds none — where the build's own declarations
// keep it. So what a declaration says of a change is the declaration's to
// change: a later part that drops a file on an event changes a value, never a
// call site.
//
// Mutations: drop writeRefs' call to dropFor, and the changed declarations'
// graph survives; drop dropFor's removal of each dropped declaration's entry
// (its closing setDerived), and the incarnation still records the graph it
// no longer holds.
func TestTheCodeReadsTheDeclarations(t *testing.T) {
	t.Parallel()
	dropOnRefs := cache.DerivedFiles()
	for i := range dropOnRefs {
		if dropOnRefs[i].Name == cache.DerivedGraph {
			dropOnRefs[i].OnRefsMoved = cache.Drop
		}
	}
	for _, tc := range []struct {
		name  string
		decls []cache.Derived
		kept  bool
	}{
		{"the build's declarations", cache.DerivedFiles(), true},
		{"the graph dropped when the refs move", dropOnRefs, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			w := newSyncWorld(t, onMemory)
			clk := newClock()
			repo := w.repo(t, 1)
			for range 3 {
				w.push(t, repo, "refs/heads/main", nil)
			}
			c := w.open(t, clk.on, cache.WithDerived(tc.decls))
			dir := w.sync(t, c, repo).Dir
			clk.Add(cache.MidxDebounce)
			if ran := cache.MaintainerPass(c); len(ran) != 1 {
				t.Fatalf("the maintainer ran %v, want the repository", ran)
			}
			graph := filepath.Join(dir, "objects", "info", "commit-graph")
			if !statOK(t, graph) {
				t.Fatal("the maintainer wrote no commit-graph: the case needs one")
			}
			w.tag(t, repo, "v1", false)
			w.sync(t, c, repo)
			if got := statOK(t, graph); got != tc.kept {
				t.Errorf("after a delta that only moves refs the commit-graph on disk is %t, want %t", got, tc.kept)
			}
			if _, _, recorded := cache.Indexed(c, repo); recorded != tc.kept {
				t.Errorf("after a delta that only moves refs the incarnation records a graph %t, want %t",
					recorded, tc.kept)
			}
			if !statOK(t, filepath.Join(dir, "objects", "pack", "multi-pack-index")) {
				t.Error("a delta that only moves refs took the multi-pack-index, which no declaration drops then")
			}
		})
	}
}

// EVERY PACK ADDED READS ITS DECLARATION, whichever path adds it: a round's
// download, an adoption that takes the repository's write lock itself, and an
// adoption HANDED to a round that holds it (adopt.go), which the round links
// in in place of the pack's download. Over declarations in which the
// multi-pack-index is dropped when a pack is added, each path takes the index
// and its bitmap before its pack is in, and the incarnation records that it
// holds none — so what a declaration says of a pack added is read at every
// place one is added, never at most of them.
//
// Mutation: drop the round's addOnce before a handed pack's links
// (install.go), and the handed path keeps the index over the pack it added.
func TestEveryPackAddedReadsItsDeclaration(t *testing.T) {
	t.Parallel()
	dropOnAdd := cache.DerivedFiles()
	for i := range dropOnAdd {
		if dropOnAdd[i].Name == cache.DerivedMidx {
			dropOnAdd[i].OnPackAdded = cache.Drop
		}
	}
	for _, tc := range []struct {
		name string
		add  func(t *testing.T, w *syncWorld, c *cache.Cache, s cache.Synced, repo string)
	}{
		{"a round's download", func(t *testing.T, w *syncWorld, c *cache.Cache, _ cache.Synced, repo string) {
			w.push(t, repo, "refs/heads/main", nil)
			w.sync(t, c, repo)
		}},
		{"an adoption", func(t *testing.T, w *syncWorld, c *cache.Cache, s cache.Synced, repo string) {
			a := w.receiveOver(t, c, s, repo)
			if err := c.Adopt(t.Context(), repo, a.sc, a.row.Ref()); err != nil {
				t.Fatalf("Adopt: %v", err)
			}
		}},
		{"an adoption handed to a running round", func(t *testing.T, w *syncWorld, c *cache.Cache, s cache.Synced,
			repo string) {
			a := w.receiveOver(t, c, s, repo)
			reached, release := make(chan struct{}), make(chan struct{})
			var once sync.Once
			w.log.set(func(f *fakeLog) {
				f.hook = func(call fakeCall) error {
					if call.Repo == repo && call.Page == 1 {
						once.Do(func() {
							close(reached)
							<-release
						})
					}
					return nil
				}
			})
			done := make(chan error, 1)
			go func() {
				held, err := c.Sync(context.Background(), repo, linearizable)
				if err == nil {
					held.Release()
				}
				done <- err
			}()
			<-reached
			if err := c.Adopt(t.Context(), repo, a.sc, a.row.Ref()); err != nil {
				t.Fatalf("Adopt beside a running round: %v", err)
			}
			close(release)
			if err := <-done; err != nil {
				t.Fatal(err)
			}
			if got := counter(w.rec, metrics.CodeCacheAdoptions, "outcome", "handed"); got != 1 {
				t.Fatalf("%s{outcome=handed} = %d, want 1: the case holds no handed pack",
					metrics.CodeCacheAdoptions, got)
			}
			if opens, _, _ := w.objects.counts(); opens[a.row.Object] != 0 {
				t.Fatalf("the round downloaded the pack it was handed %d times: the case holds no handed pack",
					opens[a.row.Object])
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			w := newSyncWorld(t, onMemory)
			clk := newClock()
			repo := w.repo(t, 1)
			w.push(t, repo, "refs/heads/main", nil)
			c := w.open(t, clk.on, cache.WithDerived(dropOnAdd))
			s := w.sync(t, c, repo)
			clk.Add(cache.MidxDebounce)
			if ran := cache.MaintainerPass(c); len(ran) != 1 {
				t.Fatalf("the maintainer ran %v, want the repository", ran)
			}
			packDir := filepath.Join(s.Dir, "objects", "pack")
			index := []string{filepath.Join(packDir, "multi-pack-index"), bitmapOf(t, packDir)}
			for _, path := range index {
				if !statOK(t, path) {
					t.Fatalf("the maintainer wrote no %s: the case needs the index on disk", filepath.Base(path))
				}
			}
			packs := len(packFiles(t, s.Dir))

			tc.add(t, w, c, s, repo)
			if got := len(packFiles(t, s.Dir)); got <= packs {
				t.Fatalf("the copy holds %d pack files after the path, as before it: it added no pack", got)
			}
			for _, path := range index {
				if statOK(t, path) {
					t.Errorf("%s survived a pack added through %s, which its declaration drops then",
						filepath.Base(path), tc.name)
				}
			}
			if covered, bitmap, _ := cache.Indexed(c, repo); covered != 0 || bitmap {
				t.Errorf("the incarnation records an index over %d packs (bitmap %t) after it was dropped",
					covered, bitmap)
			}
		})
	}
}
