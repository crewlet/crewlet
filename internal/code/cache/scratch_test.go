package cache_test

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/code/cache"
	"github.com/crewlet/crewlet/internal/code/gitproc"
)

// A SCRATCH IS A REPOSITORY THE POOL ACCEPTS, and a push's receive-pack runs
// there rather than in the cache: HEAD the incarnation's, the engine's own
// config, NO refs, and one alternate — the incarnation's objects — so a thin
// pack is completed and checked against the cache without a byte of it
// entering the cache. Its one pack is what Pack names; it pins the
// incarnation it leans on, so the budget never evicts the alternate from
// under a push; and a push id names one scratch, once.
func TestAScratchIsARepositoryThePoolAccepts(t *testing.T) {
	t.Parallel()
	w := newSyncWorld(t, onMemory)
	repo := w.repo(t, 1)
	tip, _ := w.push(t, repo, "refs/heads/main", nil)
	c := w.open(t)
	s := w.sync(t, c, repo)
	pushID := uuid.Must(uuid.NewV7()).String()

	sc, err := c.NewScratch(t.Context(), s, pushID)
	if err != nil {
		t.Fatalf("NewScratch: %v", err)
	}
	if want := w.path("scratch", pushID+".git"); sc.Dir != want {
		t.Errorf("the scratch is at %s, want %s", sc.Dir, want)
	}
	if got, want := read(t, filepath.Join(sc.Dir, "HEAD")), read(t, filepath.Join(s.Dir, "HEAD")); got != want {
		t.Errorf("the scratch's HEAD is %q, want the incarnation's %q", got, want)
	}
	if got := read(t, filepath.Join(sc.Dir, "config")); got != gitproc.RepoConfig {
		t.Errorf("the scratch's config is %q, want gitproc.RepoConfig", got)
	}
	if got := names(t, filepath.Join(sc.Dir, "refs")); !slices.Equal(got, []string{"heads", "tags"}) {
		t.Errorf("refs/ holds %v, want heads and tags", got)
	}
	for _, d := range []string{"refs/heads", "refs/tags"} {
		if got := names(t, filepath.Join(sc.Dir, d)); len(got) != 0 {
			t.Errorf("%s holds %v: a scratch holds no refs", d, got)
		}
	}
	if exists(filepath.Join(sc.Dir, "packed-refs")) {
		t.Error("a scratch holds a packed-refs: its refs are empty")
	}
	if got, want := read(t, filepath.Join(sc.Dir, "objects", "info", "alternates")), s.Dir+"/objects\n"; got != want {
		t.Errorf("the scratch's alternates is %q, want %q", got, want)
	}
	if _, err := runGitOver(t, w.pool, sc, "rev-parse", "--git-dir"); err != nil {
		t.Errorf("the pool refused to run git in the scratch: %v", err)
	}
	if _, err := runGitOver(t, w.pool, sc, "cat-file", "-e", tip); err != nil {
		t.Errorf("the cache's tip is not readable through the scratch's alternate: %v", err)
	}

	if path, cs, ok, err := sc.Pack(); err != nil || ok || path != "" || cs != "" {
		t.Errorf("Pack of an empty scratch = %q, %q, %t, %v; want nothing", path, cs, ok, err)
	}
	_, first := w.packInto(t, repo, sc.Dir, nil)
	path, cs, ok, err := sc.Pack()
	want := filepath.Join(sc.Dir, "objects", "pack", "pack-"+first+".pack")
	if err != nil || !ok || path != want || cs != first {
		t.Errorf("Pack = %q, %q, %t, %v; want %s and %s", path, cs, ok, err, want, first)
	}
	_, second := w.packInto(t, repo, sc.Dir, nil)
	if _, _, _, err := sc.Pack(); err == nil || !strings.Contains(err.Error(), first) || !strings.Contains(err.Error(), second) {
		t.Errorf("Pack of a scratch holding two packs = %v, want an error naming %s and %s", err, first, second)
	}
	// A COMPACTION'S SCRATCH holds a pack an output: Packs answers every one,
	// by checksum.
	wantPacks := []cache.ScratchPack{
		{Path: filepath.Join(sc.Dir, "objects", "pack", "pack-"+first+".pack"), Checksum: first},
		{Path: filepath.Join(sc.Dir, "objects", "pack", "pack-"+second+".pack"), Checksum: second},
	}
	slices.SortFunc(wantPacks, func(a, b cache.ScratchPack) int { return strings.Compare(a.Checksum, b.Checksum) })
	if got, err := sc.Packs(); err != nil || !slices.Equal(got, wantPacks) {
		t.Errorf("Packs of a scratch holding two packs = %v, %v; want %v", got, err, wantPacks)
	}
	packDir := filepath.Join(sc.Dir, "objects", "pack")
	for _, ext := range []string{".pack", ".idx", ".rev"} {
		if err := os.Remove(filepath.Join(packDir, "pack-"+second+ext)); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Remove(filepath.Join(packDir, "pack-"+first+".pack")); err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := sc.Pack(); err == nil || !strings.Contains(err.Error(), "pack-"+first+".idx") {
		t.Errorf("Pack of a scratch holding an .idx without its .pack = %v, want an error naming it", err)
	}
	if _, err := sc.Packs(); err == nil || !strings.Contains(err.Error(), "pack-"+first+".idx") {
		t.Errorf("Packs of a scratch holding an .idx without its .pack = %v, want an error naming it", err)
	}

	if _, err := c.NewScratch(t.Context(), s, pushID); !errors.Is(err, cache.ErrInvalid) {
		t.Errorf("a second scratch under one push id = %v, want ErrInvalid", err)
	}
	s.Release()
	cache.SetMaxBytes(c, 1)
	cache.JanitorPass(c)
	if _, held := cache.Repos(c)[repo]; !held {
		t.Fatal("the budget evicted the incarnation a push's scratch leans on")
	}

	sc.Remove()
	sc.Remove()
	if exists(sc.Dir) {
		t.Errorf("%s is still there after Remove", sc.Dir)
	}
	cache.JanitorPass(c)
	if _, held := cache.Repos(c)[repo]; held {
		t.Error("with its scratch removed, the repository over the budget was not evicted")
	}
	if got := names(t, w.path("trash")); len(got) != 0 {
		t.Errorf("trash/ holds %v after a pass", got)
	}
}

// A MERGE'S SCRATCH LEANS ON EXACTLY THE PACKS IT MERGES: its one alternate is
// a store of its own under scratch/, merge.<repository>.<id>.git, holding each
// pack it names as the incarnation's very .pack, .idx and .rev, linked, and
// nothing else — no pack it does not merge, no multi-pack-index, no
// commit-graph, no alternate of its own — a repository the pool accepts. So a
// git process run in it reads every object of the merged packs and none of
// the rest: what `pack-objects --stdin-packs` walks to fill in its name hashes
// ends at the merge's edge, never in the history beside it. None at all, a
// pack named twice and a pack the incarnation does not hold live are refused,
// with nothing made; Remove takes the store with the scratch and gives the pin
// back.
//
// Mutation: lean a merge's scratch on the incarnation (its alternate the
// copy's own directory, as a compaction's build's was), and the third push's
// commit, in no pack the merge names, is read through it.
func TestAMergesScratchLeansOnExactlyThePacksItMerges(t *testing.T) {
	t.Parallel()
	w := newSyncWorld(t, onMemory)
	repo := w.repo(t, 1)
	first, p1 := w.push(t, repo, "refs/heads/main", nil)
	second, p2 := w.push(t, repo, "refs/heads/main", nil)
	third, p3 := w.push(t, repo, "refs/heads/main", nil)
	c := w.open(t)
	s := w.sync(t, c, repo)
	merged := []string{p1.GitChecksum, p2.GitChecksum}
	id := uuid.Must(uuid.NewV7()).String()

	sc, err := c.NewMergeScratch(t.Context(), s, id, merged)
	if err != nil {
		t.Fatalf("NewMergeScratch: %v", err)
	}
	store := w.path("scratch", "merge."+repo+"."+id+".git")
	switch {
	case sc.Dir != w.path("scratch", id+".git"):
		t.Errorf("the merge's scratch is at %s, want %s", sc.Dir, w.path("scratch", id+".git"))
	case sc.Alternate != store:
		t.Errorf("the merge's scratch leans on %s, want its store %s", sc.Alternate, store)
	}
	if got, want := read(t, filepath.Join(sc.Dir, "objects", "info", "alternates")), store+"/objects\n"; got != want {
		t.Errorf("the scratch's alternates is %q, want %q", got, want)
	}
	if err := gitproc.CheckShape(sc.Dir, sc.Alternate); err != nil {
		t.Errorf("the pool would refuse to run in the merge's scratch: %v", err)
	}
	if got := names(t, filepath.Join(sc.Dir, "objects", "pack")); len(got) != 0 {
		t.Errorf("the scratch holds %v before any build: a merge reads its packs through its store", got)
	}
	var want []string
	for _, cs := range merged {
		for _, ext := range []string{".idx", ".pack", ".rev"} {
			want = append(want, "pack-"+cs+ext)
		}
	}
	slices.Sort(want)
	if got := names(t, filepath.Join(store, "objects", "pack")); !slices.Equal(got, want) {
		t.Errorf("the store holds %v, want exactly the merged packs' files %v", got, want)
	}
	for _, name := range want {
		linked := stat(t, filepath.Join(store, "objects", "pack", name))
		if held := stat(t, filepath.Join(s.Dir, "objects", "pack", name)); !os.SameFile(linked, held) {
			t.Errorf("the store's %s is not the incarnation's own file", name)
		}
	}
	for _, rel := range []string{"objects/info/alternates", "objects/info/commit-graph", "objects/info/commit-graphs",
		"objects/pack/multi-pack-index"} {
		if exists(filepath.Join(store, filepath.FromSlash(rel))) {
			t.Errorf("the store holds %s: it holds the merged packs and nothing else", rel)
		}
	}
	for _, oid := range []string{first, second} {
		if _, err := runGitOver(t, w.pool, sc, "cat-file", "-e", oid); err != nil {
			t.Errorf("%s, in a merged pack, is not read through the store: %v", oid, err)
		}
	}
	if _, err := runGitOver(t, w.pool, sc, "cat-file", "-e", third); err == nil {
		t.Errorf("%s, in pack-%s which the merge does not name, is read through its scratch", third, p3.GitChecksum)
	}

	for name, packs := range map[string][]string{
		"no pack":                        {},
		"a pack named twice":             {p1.GitChecksum, p1.GitChecksum},
		"a pack the copy does not hold":  {strings.Repeat("ab", 20)},
		"one held and one not held live": {p1.GitChecksum, strings.Repeat("cd", 20)},
	} {
		if _, err := c.NewMergeScratch(t.Context(), s, uuid.Must(uuid.NewV7()).String(), packs); !errors.Is(err,
			cache.ErrInvalid) {
			t.Errorf("NewMergeScratch of %s = %v, want ErrInvalid", name, err)
		}
	}
	if _, err := c.NewMergeScratch(t.Context(), s, id, merged); !errors.Is(err, cache.ErrInvalid) {
		t.Errorf("a second merge under one id = %v, want ErrInvalid", err)
	}
	if got, want := names(t, w.path("scratch")), []string{filepath.Base(sc.Dir), filepath.Base(store)}; !slices.Equal(
		got, slices.Sorted(slices.Values(want))) {
		t.Errorf("scratch/ holds %v after the refused merges, want only the one merge's %v", got, want)
	}
	if got := cache.Pins(c, repo); got != 2 {
		t.Errorf("the incarnation has %d pins, want the sync's and the merge's", got)
	}

	sc.Remove()
	if exists(sc.Dir) || exists(store) {
		t.Errorf("Remove left the scratch (%t) or its store (%t)", exists(sc.Dir), exists(store))
	}
	if got := cache.Pins(c, repo); got != 1 {
		t.Errorf("the incarnation has %d pins after the merge's scratch was removed, want the sync's one", got)
	}
	if !exists(filepath.Join(s.Dir, "objects", "pack", "pack-"+p1.GitChecksum+".pack")) {
		t.Error("removing the merge's store removed the incarnation's own pack")
	}
}

// A SCRATCH IS NAMED BY A SYNC OF THIS CACHE: its alternate is an
// incarnation the scratch pins, and only the Synced this cache handed out
// says which one — so a zero Synced, another cache's, a push id that is not
// a canonical UUID (it becomes a path component) and a stopped cache are each
// refused before anything is written under scratch/.
func TestAScratchIsRefusedWhatIsNotOneOfThisCachesSyncs(t *testing.T) {
	t.Parallel()
	w := newSyncWorld(t, onMemory)
	repo := w.repo(t, 1)
	w.push(t, repo, "refs/heads/main", nil)
	c := w.open(t)
	s := w.sync(t, c, repo)

	other := newSyncWorld(t, onMemory)
	otherRepo := other.repo(t, 1)
	other.push(t, otherRepo, "refs/heads/main", nil)
	oc := other.open(t)
	foreign := other.sync(t, oc, otherRepo)

	pushID := uuid.Must(uuid.NewV7()).String()
	for name, try := range map[string]func() (*cache.Scratch, error){
		"a zero Synced":        func() (*cache.Scratch, error) { return c.NewScratch(t.Context(), cache.Synced{}, pushID) },
		"another cache's sync": func() (*cache.Scratch, error) { return c.NewScratch(t.Context(), foreign, pushID) },
		"an upper-case id": func() (*cache.Scratch, error) {
			return c.NewScratch(t.Context(), s, strings.ToUpper(pushID))
		},
		"an id that is no UUID": func() (*cache.Scratch, error) { return c.NewScratch(t.Context(), s, "../x") },
		"a view of a zero Synced": func() (*cache.Scratch, error) {
			return c.AdvertiseView(t.Context(), cache.Synced{})
		},
		"a view of another cache's sync": func() (*cache.Scratch, error) {
			return c.AdvertiseView(t.Context(), foreign)
		},
	} {
		if _, err := try(); !errors.Is(err, cache.ErrInvalid) {
			t.Errorf("NewScratch of %s = %v, want ErrInvalid", name, err)
		}
	}
	if got := names(t, w.path("scratch")); len(got) != 0 {
		t.Errorf("refused scratches left %v under scratch/", got)
	}
	if got := cache.Pins(c, repo); got != 1 {
		t.Errorf("the incarnation has %d pins after refused scratches, want the sync's one", got)
	}

	c.Stop(context.Background())
	if _, err := c.NewScratch(t.Context(), s, pushID); !errors.Is(err, cache.ErrStopped) {
		t.Errorf("NewScratch after Stop = %v, want ErrStopped", err)
	}
	if _, err := c.AdvertiseView(t.Context(), s); !errors.Is(err, cache.ErrStopped) {
		t.Errorf("AdvertiseView after Stop = %v, want ErrStopped", err)
	}
}

// A SCRATCH OUTLIVING ITS CACHE IS SAFE TO REMOVE: once Stop has begun the
// directory is the next Open's, so a Remove after it moves nothing — the
// next boot moves every scratch of this process aside — and still gives the
// pin back.
func TestAScratchOutlivingItsCacheIsSafeToRemove(t *testing.T) {
	t.Parallel()
	w := newSyncWorld(t, onMemory)
	repo := w.repo(t, 1)
	w.push(t, repo, "refs/heads/main", nil)
	c := w.open(t)
	s := w.sync(t, c, repo)
	sc, err := c.NewScratch(t.Context(), s, uuid.Must(uuid.NewV7()).String())
	if err != nil {
		t.Fatal(err)
	}
	s.Release()
	c.Stop(context.Background())
	sc.Remove()
	if !exists(sc.Dir) {
		t.Error("a Remove after Stop moved the scratch: the directory is the next Open's")
	}
	if got := cache.Pins(c, repo); got != 0 {
		t.Errorf("the incarnation has %d pins after its last scratch was removed, want 0", got)
	}
	w.open(t)
	if got := names(t, w.path("scratch")); len(got) != 0 {
		t.Errorf("the next boot left %v under scratch/", got)
	}
}

// AN ADVERTISEMENT VIEW IS A SNAPSHOT OF THE REFS: its packed-refs is the
// incarnation's own file under a second name, so receive-pack lists the
// cache's refs without running in the cache — and a sync that replaces the
// cache's packed-refs by rename meanwhile leaves the view naming what it
// named, because a rename gives the old name a new file and leaves the old
// file where the view's link points.
func TestAnAdvertisementViewIsASnapshotOfTheRefs(t *testing.T) {
	t.Parallel()
	w := newSyncWorld(t, onMemory)
	repo := w.repo(t, 1)
	old, _ := w.push(t, repo, "refs/heads/main", nil)
	c := w.open(t)
	s := w.sync(t, c, repo)
	view, err := c.AdvertiseView(t.Context(), s)
	if err != nil {
		t.Fatalf("AdvertiseView: %v", err)
	}
	defer view.Remove()
	if !strings.HasPrefix(filepath.Base(view.Dir), "adv-") || filepath.Dir(view.Dir) != w.path("scratch") {
		t.Errorf("the view is at %s, want scratch/adv-<uuid>.git", view.Dir)
	}
	vi, err := os.Stat(filepath.Join(view.Dir, "packed-refs"))
	if err != nil {
		t.Fatal(err)
	}
	ci, err := os.Stat(filepath.Join(s.Dir, "packed-refs"))
	if err != nil {
		t.Fatal(err)
	}
	if !os.SameFile(vi, ci) {
		t.Error("the view's packed-refs is not the incarnation's own file")
	}

	tip, _ := w.push(t, repo, "refs/heads/main", nil)
	w.sync(t, c, repo)
	if got := read(t, filepath.Join(s.Dir, "packed-refs")); !strings.Contains(got, tip+" refs/heads/main") {
		t.Fatalf("the sync did not move main in the cache:\n%s", got)
	}
	if got := read(t, filepath.Join(view.Dir, "packed-refs")); !strings.Contains(got, old+" refs/heads/main") {
		t.Errorf("the view's packed-refs moved with the cache's:\n%s", got)
	}
	res, err := w.pool.Run(t.Context(), gitproc.Cmd{
		Dir: view.Dir, Alternate: view.Alternate, Args: []string{"receive-pack", "--stateless-rpc", "--advertise-refs", view.Dir},
		Class: gitproc.ClassTransfer, Share: suiteShare, Deadline: time.Minute,
	})
	if err != nil {
		t.Fatalf("receive-pack's advertisement in the view: %v", err)
	}
	advert := string(res.Stdout)
	if !strings.Contains(advert, old+" refs/heads/main") || strings.Contains(advert, tip+" refs/heads/main") {
		t.Errorf("the view's advertisement does not list main at the old tip %s:\n%s", old, advert)
	}
}

// A SPOOL HAS NO NAME: it is the open file and nothing else, so the kernel
// removes it when it is closed whatever became of the request — a crash
// included — and the janitor, which measures scratch/ by its names, never
// counts a request's spool against the cache's budget.
func TestASpoolHasNoName(t *testing.T) {
	t.Parallel()
	w := newSyncWorld(t, onMemory)
	c := w.open(t)
	f, err := c.Spool()
	if err != nil {
		t.Fatalf("Spool: %v", err)
	}
	if got := names(t, w.path("scratch")); len(got) != 0 {
		t.Errorf("a spool left %v under scratch/, where the janitor would count it", got)
	}
	if _, err := f.WriteString("0032want " + strings.Repeat("ab", 20) + "\n"); err != nil {
		t.Fatalf("writing the spool: %v", err)
	}
	back, err := io.ReadAll(io.NewSectionReader(f, 0, 1<<20))
	if err != nil || !strings.HasPrefix(string(back), "0032want ") {
		t.Fatalf("reading the spool back = %q, %v", back, err)
	}
	if err := f.Close(); err != nil {
		t.Fatalf("closing the spool: %v", err)
	}
	c.Stop(context.Background())
	if _, err := c.Spool(); !errors.Is(err, cache.ErrStopped) {
		t.Errorf("Spool after Stop = %v, want ErrStopped", err)
	}
}
