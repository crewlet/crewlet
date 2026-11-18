package cache_test

import (
	"errors"
	"io/fs"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/gitlet"
	"github.com/crewlet/crewlet/internal/gitlet/cache"
)

// EVERY DERIVED FILE IS DECLARED, and every declaration says all its readers
// need (ADR-0086): the sweep, Verify, the check, the scrub, the budget and the
// maintainer act on exactly the declarations, so a file git writes that none
// owns is trashed under a running reader, and a pattern naming what no writer
// makes is a hole anyone may fill. So the declarations validate; each row of a
// TABLE of every name written in the derived directories matches only its
// owner or holder (or none), and every pattern a row (regular expressions
// can't be compared for overlap); REAL GIT leaves under objects/ only a row's
// pack or a declaration's file, and each declaration whose write ran owns one.
//
// Mutation: narrow midxBitmap to match no bitmap git writes: the table fails
// both ways, and real git's bitmap is no declaration's.
func TestEveryDerivedFileIsDeclared(t *testing.T) {
	t.Parallel()
	decls := cache.DerivedFiles()
	if err := cache.ValidateDerived(decls); err != nil {
		t.Fatalf("the build's declarations: %v", err)
	}
	for _, d := range decls {
		if err := d.Validate(); err != nil {
			t.Errorf("%s: %v", d.Name, err)
		}
	}

	t.Run("known names", func(t *testing.T) {
		t.Parallel()
		cs := strings.Repeat("0123456789", 4)
		type row struct {
			rel           string
			owner, holder cache.DerivedName // "" for none
		}
		rows := []row{
			{rel: "objects/pack/multi-pack-index", owner: cache.DerivedMidx},
			{rel: "objects/pack/multi-pack-index-" + cs + ".bitmap", owner: cache.DerivedMidx},
			{rel: "objects/pack/multi-pack-index-" + cs + ".rev", owner: cache.DerivedMidx},
			{rel: "objects/pack/multi-pack-index.lock", holder: cache.DerivedMidx},
			{rel: "objects/pack/tmp_bitmap_AbC123", holder: cache.DerivedMidx},
			{rel: "objects/info/commit-graph", owner: cache.DerivedGraph},
			{rel: "objects/info/commit-graph.lock", holder: cache.DerivedGraph},
			// The index of pushes' runs, numbered from one in canonical
			// decimal: the bare name, a run numbered zero or spelled with a
			// leading zero, and a run's name under objects/ are none of its.
			{rel: "crewlet/provenance.1", owner: cache.DerivedProvenance},
			{rel: "crewlet/provenance.17", owner: cache.DerivedProvenance},
			{rel: "crewlet/provenance"}, {rel: "crewlet/provenance.0"}, {rel: "crewlet/provenance.017"},
			{rel: "objects/info/provenance.1"},
			// The memo's tree, which it owns whole: an entry inside it is no
			// pattern's — the tree is the declaration's — and a name beside
			// it, or the tree's name under objects/, is none of its.
			{rel: "crewlet/memo", owner: cache.DerivedMemo},
			{rel: "crewlet/memo/" + strings.Repeat("ab", 32)}, {rel: "crewlet/memo.1"},
			// The code index's files, numbered from one, under its directory.
			{rel: "crewlet/codeindex/seg.1", owner: cache.DerivedCodeIndex},
			{rel: "crewlet/codeindex/tip.12", owner: cache.DerivedCodeIndex},
			{rel: "crewlet/codeindex/seg.0"}, {rel: "crewlet/codeindex/tip.01"}, {rel: "crewlet/codeindex"},
			{rel: "crewlet/seg.1"},
			{rel: "objects/info/memo"},
			// The tag index, one file under crewlet/.
			{rel: "crewlet/tags", owner: cache.DerivedTags},
			{rel: "crewlet/tags.1"}, {rel: "crewlet/tags/x"}, {rel: "objects/info/tags"},
			// The installed chain (layers.go): each layer's multi-pack-index
			// and bitmap and the chain file the cache writes, as git 2.56.0
			// names them, and the commit-graph layers with the node's tip over
			// them, whose lock and temporary layer the tip's write holds
			// (maintain.go). Then names no write makes: a layer's reverse index
			// as a file (git writes the RIDX chunk), a publisher's temporary
			// layer (the sweep trashes it), a short checksum, and a temporary
			// layer of another length than git's mkstemp gives.
			{rel: "objects/pack/multi-pack-index.d/multi-pack-index-chain", owner: cache.DerivedLayers},
			{rel: "objects/pack/multi-pack-index.d/multi-pack-index-" + cs + ".midx", owner: cache.DerivedLayers},
			{rel: "objects/pack/multi-pack-index.d/multi-pack-index-" + cs + ".bitmap", owner: cache.DerivedLayers},
			{rel: "objects/info/commit-graphs/commit-graph-chain", owner: cache.DerivedGraphLayers},
			{rel: "objects/info/commit-graphs/graph-" + cs + ".graph", owner: cache.DerivedGraphLayers},
			{rel: "objects/info/commit-graphs/commit-graph-chain.lock", holder: cache.DerivedGraphLayers},
			{rel: "objects/info/commit-graphs/tmp_graph_AbC123", holder: cache.DerivedGraphLayers},
			{rel: "objects/pack/multi-pack-index.d/multi-pack-index-" + cs + ".rev"},
			{rel: "objects/pack/multi-pack-index.d/tmp_midx_AbC123"},
			{rel: "objects/pack/multi-pack-index.d/multi-pack-index-abc.midx"},
			{rel: "objects/info/commit-graphs/graph-abc.graph"},
			{rel: "objects/info/commit-graphs/tmp_graph_AbC1234"},
			// The image's fixed files, a pack's own, and names beside the
			// declared ones that no write of this build makes: an
			// alternates file, a bitmap whose name carries no checksum.
			{rel: "HEAD"}, {rel: "config"}, {rel: "packed-refs"}, {rel: "crewlet-state.json"},
			{rel: "objects/pack/pack-" + cs + ".pack"}, {rel: "objects/pack/pack-" + cs + ".idx"},
			{rel: "objects/pack/pack-" + cs + ".rev"},
			{rel: "objects/info/alternates"},
			{rel: "objects/pack/multi-pack-index-abc.bitmap"},
			{rel: "refs/heads/commit-graph"},
		}
		for _, r := range rows {
			if got, want := cache.OwnersOf(r.rel), oneName(r.owner); !slices.Equal(got, want) {
				t.Errorf("%s is owned by %v, want %v", r.rel, got, want)
			}
			if got, want := cache.HoldersOf(r.rel), oneName(r.holder); !slices.Equal(got, want) {
				t.Errorf("%s is held while a write runs by %v, want %v", r.rel, got, want)
			}
		}
		// The other direction: a pattern no row names is a name nobody
		// checked against git, or one nothing writes.
		matches := func(name cache.DerivedName, p cache.FilePattern, held bool) bool {
			return slices.ContainsFunc(rows, func(r row) bool {
				dir, base := path.Split(r.rel)
				named := r.owner
				if held {
					named = r.holder
				}
				return named == name && path.Clean(dir) == p.Dir && p.Name.MatchString(base)
			})
		}
		for _, d := range decls {
			for _, o := range d.Owns {
				if !matches(d.Name, o.FilePattern, false) {
					t.Errorf("%s owns %s/%s, which no row of the table names", d.Name, o.Dir, o.Name)
				}
			}
			for _, w := range d.Writes {
				for _, p := range w.Holds {
					if !matches(d.Name, p, true) {
						t.Errorf("%s's %s write holds %s/%s while it runs, which no row of the table names",
							d.Name, w.Step, p.Dir, p.Name)
					}
				}
			}
		}
	})

	t.Run("real git", func(t *testing.T) {
		t.Parallel()
		w := newSyncWorld(t, onMemory)
		clk := newClock()
		repo := w.repo(t, 1)
		for range caseRoundPacks {
			w.push(t, repo, "refs/heads/main", nil)
		}
		c := w.open(t, clk.on, cache.RoundPacksAt(caseRoundPacks))
		s := w.sync(t, c, repo)
		roundWrote := derivedOnDisk(t, s.Dir)
		clk.Add(cache.MidxDebounce)
		if ran := cache.MaintainerPass(c); len(ran) != 1 {
			t.Fatalf("the maintainer ran %v, want the repository", ran)
		}
		if _, bitmap, graph := cache.Indexed(c, repo); !bitmap || !graph {
			t.Fatalf("the maintainer left a bitmap (%t) and a graph (%t); the case needs both written", bitmap, graph)
		}
		// And the cache's own: a delta folding a push into the index of
		// pushes writes its run, and a read's run memoizes its answer.
		w.push(t, repo, "refs/heads/main", nil)
		memoize(t, c, w.sync(t, c, repo), memoKeys("declared", 1))
		maintained := derivedOnDisk(t, s.Dir)
		// AND THE INSTALLED CHAIN: a second repository's published layers,
		// as the suite's git writes them in a maintainer's scratch and as the
		// cache places them.
		chained := w.repo(t, 2)
		p := w.pushN(t, chained, 2)
		build := w.newChainBuild(t, p)
		w.log.publish(chained, []gitlet.LayerRef{build.layer(t, p, false)}, nil)
		installed := derivedOnDisk(t, w.sync(t, c, chained).Dir)
		published := derivedOnDisk(t, build.bare)
		// And the node's tip over that chain, written over a push since.
		w.push(t, chained, "refs/heads/main", nil)
		tipped := w.sync(t, c, chained).Dir
		if ran := cache.MaintainerPass(c); !slices.Contains(ran, chained) {
			t.Fatalf("the maintainer ran %v, want %s's tip", ran, chained)
		}
		tip := derivedOnDisk(t, tipped)
		// And the code index of the branch's tip, built over that copy.
		copied := w.sync(t, c, chained)
		if got, err := c.Index(t.Context(), copied); err != nil || got != cache.IndexBuilt {
			t.Fatalf("the code index's build = (%q, %v), want built", got, err)
		}
		indexed := derivedOnDisk(t, copied.Dir)
		t.Logf("the suite's git and the cache wrote, of the derived files: %v after the round, %v after the "+
			"maintainer and a delta, %v of a published layer, %v where the cache installed it, %v with the "+
			"node's tip and %v with its code index", roundWrote, maintained, published, installed, tip, indexed)
		owned := map[cache.DerivedName]int{}
		for _, rel := range slices.Concat(maintained, published, installed, tip, indexed) {
			got := cache.OwnersOf(rel)
			if len(got) != 1 {
				t.Errorf("real git wrote %s, owned by %v: a file under objects/ that is no row's pack is one "+
					"declaration's, or the sweep trashes it under a running reader", rel, got)
				continue
			}
			owned[got[0]]++
		}
		for _, d := range decls {
			if owned[d.Name] == 0 {
				t.Errorf("%s's write ran and it owns no file on disk: its patterns name nothing git writes", d.Name)
			}
		}
		// The round writes its index and the tag index (tags.go).
		if !slices.ContainsFunc(roundWrote, func(rel string) bool {
			return slices.Contains(cache.OwnersOf(rel), cache.DerivedMidx)
		}) || !slices.Contains(roundWrote, "crewlet/tags") {
			t.Errorf("the round's own index left %v: want the multi-pack-index's files and the tag index", roundWrote)
		}
	})
}

// oneName is name as a list: none for "".
func oneName(name cache.DerivedName) []cache.DerivedName {
	if name == "" {
		return nil
	}
	return []cache.DerivedName{name}
}

// derivedOnDisk lists, relative to dir, slash-separated and sorted, every file
// under the incarnation's objects/ that is no pack's own and every file under
// its crewlet/ (the roots declarations own files in); a directory a
// declaration owns whole (the memo's tree) is one entry. A copy with no
// crewlet/ yet has none there.
func derivedOnDisk(t *testing.T, dir string) []string {
	t.Helper()
	pack := regexp.MustCompile(`^pack-[0-9a-f]{40}\.(pack|idx|rev)$`)
	var out []string
	for _, root := range []string{"objects", "crewlet"} {
		err := filepath.WalkDir(filepath.Join(dir, root), func(p string, d fs.DirEntry, err error) error {
			if errors.Is(err, fs.ErrNotExist) && p == filepath.Join(dir, root) {
				return fs.SkipDir
			}
			if err != nil {
				return err
			}
			rel, err := filepath.Rel(dir, p)
			if err != nil {
				return err
			}
			rel = filepath.ToSlash(rel)
			if d.IsDir() {
				if len(cache.OwnersOf(rel)) > 0 {
					out = append(out, rel)
					return fs.SkipDir
				}
				return nil
			}
			if !pack.MatchString(d.Name()) {
				out = append(out, rel)
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	slices.Sort(out)
	return out
}
