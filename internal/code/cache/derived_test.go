package cache_test

import (
	"io/fs"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/code/cache"
)

// EVERY DERIVED FILE IS DECLARED, and every declaration says everything a
// reader of it needs (ADR-0038). A derived file is one the cache writes from
// what an incarnation already holds and no row names; the sweep keeps, Verify
// admits, the check and the scrub hold, the budget counts and the maintainer
// writes exactly what the declarations say, so a file real git writes that no
// declaration owns is trashed under a running reader, and a pattern that
// names what no writer makes is a hole anyone may fill.
//
// Three halves. The declarations pass their own validation. A TABLE of every
// name git and the cache write in an incarnation's derived directories — each
// with the declaration that owns it or holds it while a write runs, or none —
// is held to the declarations in both directions, because regular
// expressions cannot be compared for overlap in general: every row is matched
// by exactly the declaration it names and by no other, and every pattern
// matches a row. And REAL GIT — a round's index and the maintainer's bitmap
// and graph, on the suite's git — leaves no file under objects/ that is not a
// row's pack or a declaration's own, and each declaration whose write ran
// owns a file on disk.
//
// Mutation: narrow the multi-pack-index bitmap's pattern (midxBitmap) so it
// matches no bitmap git writes: the table's bitmap row is undeclared, the
// pattern matches no row, and the bitmap real git wrote is no declaration's.
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
			// The image's fixed files, a pack's own, and names beside the
			// declared ones that no write of this build makes: a split
			// graph's chain, an incremental index's chain, an alternates
			// file, a bitmap whose name carries no checksum.
			{rel: "HEAD"}, {rel: "config"}, {rel: "packed-refs"}, {rel: "crewlet-state.json"},
			{rel: "objects/pack/pack-" + cs + ".pack"}, {rel: "objects/pack/pack-" + cs + ".idx"},
			{rel: "objects/pack/pack-" + cs + ".rev"},
			{rel: "objects/info/alternates"},
			{rel: "objects/pack/multi-pack-index-abc.bitmap"},
			{rel: "objects/info/commit-graphs/commit-graph-chain"},
			{rel: "objects/pack/multi-pack-index.d/multi-pack-index-chain"},
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
		maintained := derivedOnDisk(t, s.Dir)
		t.Logf("the suite's git wrote, of the derived files: %v after the round, %v after the maintainer",
			roundWrote, maintained)
		owned := map[cache.DerivedName]int{}
		for _, rel := range maintained {
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
		if len(roundWrote) == 0 || !slices.Contains(cache.OwnersOf(roundWrote[0]), cache.DerivedMidx) {
			t.Errorf("the round's own index left %v: want the multi-pack-index's files", roundWrote)
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

// derivedOnDisk is every file under the incarnation at dir's objects/ that is
// no pack's own file, by its path relative to dir, slash-separated, sorted.
func derivedOnDisk(t *testing.T, dir string) []string {
	t.Helper()
	pack := regexp.MustCompile(`^pack-[0-9a-f]{40}\.(pack|idx|rev)$`)
	var out []string
	err := filepath.WalkDir(filepath.Join(dir, "objects"), func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || pack.MatchString(d.Name()) {
			return err
		}
		rel, err := filepath.Rel(dir, p)
		out = append(out, filepath.ToSlash(rel))
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	slices.Sort(out)
	return out
}
