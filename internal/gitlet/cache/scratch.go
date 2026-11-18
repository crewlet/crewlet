package cache

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"

	"github.com/crewlet/crewlet/internal/gitlet"
	"github.com/crewlet/crewlet/internal/gitlet/gitproc"
	"github.com/crewlet/crewlet/internal/hostbox"
)

// THE RECEIVE PATH'S REPOSITORIES: receive-pack never runs in a cache but in
// a SCRATCH under scratch/ whose one alternate is the cache's objects, and a
// pushed pack enters a cache only through Adopt or a sync (package doc, "A
// push never runs in a cache"). A scratch holds NO REFS: receive-pack hands
// every command to the proc-receive hook without comparing it with the
// scratch's refs, and checks connectivity against the alternate's, so an
// empty refs/ costs nothing and leaves no second copy of the refs to disagree
// with the rows. A push's ADVERTISEMENT runs in a VIEW, whose packed-refs is
// a hard link of the incarnation's, so a sync's rename of the cache's
// packed-refs leaves the view naming the refs it was made with.

// A MERGE'S REPOSITORIES. A merge of a repository's packs (a compaction's
// build, or a publication's repack; internal/gitlet/compact) writes into a
// scratch leaning NOT on the incarnation but on a store of EXACTLY THE PACKS
// IT MERGES (NewMergeScratch): scratch/merge.<repository>.<id>.git, whose
// objects/pack holds each merged pack's .pack, .idx and .rev as hard links of
// the incarnation's (no byte of its own while the incarnation stands), and
// nothing else: no other pack, no multi-pack-index, no commit-graph.
//
// The store is the merge's, as the scratch is: removed with it (Remove), moved
// aside by a boot with scratch/, and counted by the janitor only for a link
// whose incarnation has gone. What a git process run over it reports is about
// the incarnation the links came from (mergeAt, report.go).

// BECAUSE `pack-objects --stdin-packs` fills in its name hashes by a revision
// walk from every commit its inputs hold, stopped only by an EXCLUDED pack
// (git 2.56.0, builtin/pack-objects.c's read_stdin_packs), and a merge excludes
// none (ADR-0090). Over the incarnation the walk ran down the WHOLE HISTORY;
// over a store of its inputs it ends at their edge (revs.ignore_missing_links)
// and writes the same pack, byte for byte. Eight one-commit packs beside
// 400 032 objects walked 1.39 s against 7 ms; BenchmarkCompact's
// history=source/roll=16 arms build sixteen 20 KB pushes in 22.9 ms at
// 0.95 MiB beside the 100 MB source history and in 21.3 ms at 0.94 MiB beside
// nothing, where over the whole copy a merge of eight took 451 ms and
// 100.8 MiB (git 2.56.0, the in-process store, Go 1.27.2, -benchtime 1x, two
// E-cores of an Intel Core Ultra 7 265U, 2026-10-28).

// Both are one request's: plain writes with no fsync, since a boot moves every
// earlier process's scratch into trash/; and a view's linked packed-refs and an
// adopted pack are counted where their incarnation is (sizeUnder's soleNames).

// Scratch is a repository a push's receive-pack runs in (NewScratch), a
// merge's pack-objects writes its outputs into (NewMergeScratch), an
// advertisement view (AdvertiseView), or a publication writes its layer in
// (NewLayerScratch). It pins the incarnation it leans on or links until
// Remove, so the budget never evicts an alternate from under a push or a
// build, and the cache counts a layer's links where the incarnation is.
type Scratch struct {
	// Dir is the repository: absolute and clean, scratch/<id>.git — a push's
	// id, a merge's or a publication's — or scratch/adv-<uuid>.git.
	Dir string
	// Alternate is the repository its one alternates line names (the
	// incarnation, or a merge's store), which every git process run in it
	// names as its gitproc.Cmd.Alternate, since the pool runs in no
	// repository whose alternate the call did not name. "" for a layer's
	// scratch, which links rather than leans.
	Alternate string

	c    *Cache
	e    *entry
	inc  *incarnation
	view bool
	// store is a merge's store, its Alternate; empty for a push's scratch and
	// a view.
	store string
	// remove is Remove's body, run once.
	remove func()
}

// NewScratch makes the repository a push's receive-pack runs in: HEAD the
// bytes of s's, the pool's config, refs/heads/ and refs/tags/ EMPTY, and one
// alternate, s's objects. It pins s's incarnation until Remove. id is the
// push's, a canonical lowercase UUID since it is a path component, and names
// one scratch, once. Its receive-pack NEVER runs with [Cache.AdvertiseConfig]:
// the alternate's refs are the only negative tips of its connectivity check.
func (c *Cache) NewScratch(ctx context.Context, s Synced, id string) (*Scratch, error) {
	if err := c.handedOut(s, "a scratch"); err != nil {
		return nil, err
	}
	if !canonicalID(id) {
		return nil, fmt.Errorf("%w: scratch id %q is not a canonical lowercase UUID (it becomes a path component)",
			ErrInvalid, id)
	}
	return c.newScratch(ctx, s, id, false, nil)
}

// NewMergeScratch makes the repository a merge's pack-objects writes its
// outputs into (a compaction's build, or a publication's repack, whose outputs
// [Scratch.Gather] takes into its layer's scratch), shaped as NewScratch's but
// leaning on a store of exactly the packs it merges (A MERGE'S REPOSITORIES,
// above). packs names each by git checksum, each live in the incarnation
// serving s now (newScratch), pinned until Remove, which takes the store too.
// id is the merge's canonical lowercase UUID, naming one scratch, once.
//
// No pack, a pack named twice, or one not live in objects/pack (one only an
// expunge's dropping rows name) is refused ([ErrInvalid]), nothing made. A
// file that could not be linked (one a repair is replacing) is the error, the
// scratch and its store moved aside.
func (c *Cache) NewMergeScratch(ctx context.Context, s Synced, id string, packs []string) (*Scratch, error) {
	if err := c.handedOut(s, "a merge's scratch"); err != nil {
		return nil, err
	}
	switch {
	case !canonicalID(id):
		return nil, fmt.Errorf("%w: scratch id %q is not a canonical lowercase UUID (it becomes a path component)",
			ErrInvalid, id)
	case len(packs) == 0:
		return nil, fmt.Errorf("%w: a merge's scratch names the packs it merges, and this one names none",
			ErrInvalid)
	}
	return c.newScratch(ctx, s, id, false, packs)
}

// NewLayerScratch makes the repository a publication writes the next
// published layer in (internal/gitlet/compact): scratch/<id>.git holding HARD
// LINKS of s's incarnation's pack files and packed-refs, HEAD the bytes of
// s's, the pool's config, and the first base layers' files of the rows' chain,
// with chain files naming EXACTLY those, root first. It pins s's incarnation
// until Remove; id is the publication's canonical lowercase UUID, used once.
// A base past [Chain.Installed] is refused ([ErrInvalid]).
//
// LINKS, NEVER AN ALTERNATE: a multi-pack-index names packs of its own pack
// directory only, and the bitmap's writer selects its commits off the
// repository's own refs. Nothing writes a link in place: every git process in
// the scratch renames new files in. Three links a pack, as a swap pays.
func (c *Cache) NewLayerScratch(ctx context.Context, s Synced, id string, base int) (*Scratch, error) {
	if err := c.handedOut(s, "a layer's scratch"); err != nil {
		return nil, err
	}
	if !canonicalID(id) {
		return nil, fmt.Errorf("%w: scratch id %q is not a canonical lowercase UUID (it becomes a path component)",
			ErrInvalid, id)
	}
	if !c.enter() {
		return nil, ErrStopped
	}
	defer c.leave()
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("cache: making a layer's scratch for %s: %w", s.entry.id, context.Cause(ctx))
	}
	e, inc := s.entry, s.inc
	e.mu.Lock()
	if inc.discarded {
		e.mu.Unlock()
		return nil, fmt.Errorf("%w: a scratch names a Synced still held, and %s was released and removed",
			ErrInvalid, s.Dir)
	}
	// A layer is valid only over its exact base, so the base must be one the
	// copy holds installed ([Chain.Installed]): its files are here to link.
	if held := chainOf(inc).Installed; base < 0 || base > held {
		e.mu.Unlock()
		return nil, fmt.Errorf("%w: a layer over the chain's first %d layers, and %s holds %d of them installed",
			ErrInvalid, base, s.Dir, held)
	}
	under := slices.Clone(inc.chain[:base])
	inc.pins++
	c.used(e, inc)
	e.mu.Unlock()

	dir := filepath.Join(c.dir, scratchDir, id+".git")
	if err := os.Mkdir(dir, hostbox.DirMode); err != nil {
		c.release(e, inc)
		if errors.Is(err, fs.ErrExist) {
			return nil, fmt.Errorf("%w: %s already exists: a publication id names one scratch, once", ErrInvalid, dir)
		}
		return nil, fmt.Errorf("cache: making %s: %w", dir, err)
	}
	sc := &Scratch{Dir: dir, c: c, e: e, inc: inc}
	sc.remove = sync.OnceFunc(sc.discard)
	if err := sc.fillLayer(s.Dir, under); err != nil {
		sc.Remove()
		return nil, fmt.Errorf("cache: making %s for %s: %w", dir, e.id, err)
	}
	return sc, nil
}

// fillLayer writes a layer's scratch from the incarnation at from, its chain
// files naming exactly under ([Cache.NewLayerScratch]).
func (s *Scratch) fillLayer(from string, under []gitlet.LayerRow) error {
	for _, d := range []string{"refs/heads", "refs/tags", layersDir, graphLayersDir} {
		if err := os.MkdirAll(filepath.Join(s.Dir, filepath.FromSlash(d)), hostbox.DirMode); err != nil {
			return err
		}
	}
	head, err := os.ReadFile(filepath.Join(from, "HEAD"))
	if err != nil {
		return err
	}
	if err = os.WriteFile(filepath.Join(s.Dir, "HEAD"), head, hostbox.FileMode); err != nil {
		return err
	}
	if err = os.WriteFile(filepath.Join(s.Dir, "config"), []byte(gitproc.RepoConfig), hostbox.FileMode); err != nil {
		return err
	}
	// The refs are linked BEFORE the packs are listed: a round installs a pack
	// before it renames in the packed-refs naming its commits, so the bitmap's
	// writer, which parses every commit a ref names, finds each in a listed
	// pack. A pack still being installed (its .idx renamed in last) is one git
	// does not read.
	if err = os.Link(filepath.Join(from, "packed-refs"), filepath.Join(s.Dir, "packed-refs")); err != nil {
		return err
	}
	packs, err := os.ReadDir(filepath.Join(from, "objects", "pack"))
	if err != nil {
		return err
	}
	for _, d := range packs {
		if !packFileName().MatchString(d.Name()) {
			continue
		}
		rel := filepath.Join("objects", "pack", d.Name())
		if err := os.Link(filepath.Join(from, rel), filepath.Join(s.Dir, rel)); err != nil {
			return err
		}
	}
	for _, row := range under {
		for _, rel := range layerRels(row.Layer) {
			if rel == "" {
				continue
			}
			if err := os.Link(filepath.Join(from, rel), filepath.Join(s.Dir, rel)); err != nil {
				return err
			}
		}
	}
	if len(under) == 0 {
		return nil
	}
	// The chain files name EXACTLY the base: on git 2.56.0 a write with layers
	// above its base still named took 196–200 s against 1.03 s with only the
	// base, and so did `multi-pack-index compact` of two layers with one above.
	midx, graph := chainIDs(under)
	if err := os.WriteFile(filepath.Join(s.Dir, layersChainRel), chainText(midx), hostbox.FileMode); err != nil {
		return err
	}
	if len(graph) == 0 {
		return nil
	}
	return os.WriteFile(filepath.Join(s.Dir, graphChainRel), chainText(graph), hostbox.FileMode)
}

// AdvertiseView is NewScratch for a push's ADVERTISEMENT: its packed-refs is
// a hard link to s's, so receive-pack --advertise-refs lists the cache's refs
// without running in the cache, and a sync's rename of the cache's
// packed-refs does not change what the view says. Its receive-pack runs with
// [Cache.AdvertiseConfig], so it does not list its alternate's refs twice.
func (c *Cache) AdvertiseView(ctx context.Context, s Synced) (*Scratch, error) {
	if err := c.handedOut(s, "an advertisement view"); err != nil {
		return nil, err
	}
	return c.newScratch(ctx, s, "adv-"+newV7(), true, nil)
}

// mergeStoreName is the name under scratch/ of the store merge id of repoID
// leans on: the repository, so mergeAt finds whose a report is, and the
// merge's id, so no two stores share a name.
func mergeStoreName(repoID, id string) string { return "merge." + repoID + "." + id + ".git" }

// mergeAt is the entry whose merge's store dir is, and the incarnation its
// links came from, or nil: a git process run over the store reads only those
// links, the very inodes that incarnation holds, so what it reports is about
// that copy (report.go). A store whose scratch is removed is none.
func (c *Cache) mergeAt(dir string) (*entry, *incarnation) {
	if dir == "" || filepath.Dir(dir) != filepath.Join(c.dir, scratchDir) {
		return nil, nil
	}
	parts := strings.Split(filepath.Base(dir), ".")
	if len(parts) != 4 || parts[0] != "merge" || parts[3] != "git" {
		return nil, nil
	}
	c.mu.Lock()
	e := c.entries[parts[1]]
	c.mu.Unlock()
	if e == nil {
		return nil, nil
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if inc := e.merges[dir]; inc != nil && !e.dead {
		return e, inc
	}
	return nil, nil
}

// newScratch makes scratch/<name>.git, pinning s's incarnation for it and, for
// a merge, the store of exactly its packs, registered on the entry for the
// observer (mergeAt). A name already there is refused; a scratch not written
// whole is moved into trash/ with its store, its pin given back.
//
// A MERGE'S IS MADE OVER THE COPY SERVING s NOW (serving), as a rewrite's is
// (newRewriteScratch): a swap leaves the incarnation it replaced no live set,
// and other records on the maintainer swap the copy between a merge's plan and
// its scratch; its inputs, claimed from every other build of the repository
// (internal/gitlet/compact), are live in the serving copy. A push's scratch
// and a view stay over s's own: a push is checked against the copy its sync
// handed it.
func (c *Cache) newScratch(ctx context.Context, s Synced, name string, view bool, merge []string) (*Scratch,
	error) {

	if !c.enter() {
		return nil, ErrStopped
	}
	defer c.leave()
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("cache: making a scratch for %s: %w", s.entry.id, context.Cause(ctx))
	}
	e, inc := s.entry, s.inc
	e.mu.Lock()
	if merge != nil {
		inc = serving(inc)
	}
	if inc.discarded {
		e.mu.Unlock()
		return nil, fmt.Errorf("%w: a scratch names a Synced still held, and %s was released and removed",
			ErrInvalid, s.Dir)
	}
	if err := mergedLive(inc, merge); err != nil {
		e.mu.Unlock()
		return nil, fmt.Errorf("%w: a merge's scratch for %s: %w", ErrInvalid, e.id, err)
	}
	inc.pins++
	c.used(e, inc)
	e.mu.Unlock()

	dir := filepath.Join(c.dir, scratchDir, name+".git")
	if err := os.Mkdir(dir, hostbox.DirMode); err != nil {
		c.release(e, inc)
		if errors.Is(err, fs.ErrExist) {
			return nil, fmt.Errorf("%w: %s already exists: an id names one scratch, once", ErrInvalid, dir)
		}
		return nil, fmt.Errorf("cache: making %s: %w", dir, err)
	}
	sc := &Scratch{Dir: dir, Alternate: inc.dir, c: c, e: e, inc: inc, view: view}
	sc.remove = sync.OnceFunc(sc.discard)
	if merge != nil {
		sc.store = filepath.Join(c.dir, scratchDir, mergeStoreName(e.id, name))
		sc.Alternate = sc.store
		e.mu.Lock()
		if e.merges == nil {
			e.merges = map[string]*incarnation{}
		}
		e.merges[sc.store] = inc
		e.mu.Unlock()
	}
	if err := sc.fill(inc.dir, merge); err != nil {
		sc.Remove()
		return nil, fmt.Errorf("cache: making %s for %s: %w", dir, e.id, err)
	}
	return sc, nil
}

// mergedLive holds a merge's packs to inc's live set, each the checksum of a
// pack placed in objects/pack (placeOf), named once; a push's scratch or a
// view names none. e.mu is held. A pack only DROPPING rows name lies in the
// dropping view, where linkPacks finds nothing, and no merge reads one
// ([Cache.Packs]), so it is refused here, nothing made, rather than failing
// its link half way.
func mergedLive(inc *incarnation, packs []string) error {
	if packs == nil {
		return nil
	}
	named := make(map[string]bool, len(packs))
	for _, cs := range packs {
		switch place := placeOf(inc, cs); {
		case named[cs]:
			return fmt.Errorf("pack-%s is named twice", cs)
		case place == placeDropping:
			return fmt.Errorf("pack-%s is dropping in %s (an expunge has begun), and no merge reads one", cs,
				inc.dir)
		case place != placeLive:
			return fmt.Errorf("pack-%s is no pack live in %s", cs, inc.dir)
		}
		named[cs] = true
	}
	return nil
}

// fill writes the scratch's repository over the incarnation at from, leaning
// on it, or for a merge on the store of exactly merge's packs, made first.
func (s *Scratch) fill(from string, merge []string) error {
	head, err := os.ReadFile(filepath.Join(from, "HEAD"))
	if err != nil {
		return err
	}
	if s.store != "" {
		if err := os.Mkdir(s.store, hostbox.DirMode); err != nil {
			return err
		}
		if err := writeRepo(s.store, head, ""); err != nil {
			return err
		}
		if err := linkPacks(from, s.store, merge); err != nil {
			return err
		}
	}
	if err := writeRepo(s.Dir, head, s.Alternate); err != nil {
		return err
	}
	if s.view {
		return os.Link(filepath.Join(from, "packed-refs"), filepath.Join(s.Dir, "packed-refs"))
	}
	return nil
}

// writeRepo writes a repository the pool runs in at dir, which exists: HEAD
// head, the pool's config, refs/heads/ and refs/tags/ EMPTY, objects/pack/,
// and, where alternate is set, the one alternates line naming its objects:
// exactly what the pool holds a call's repository to (gitproc's
// ShapeAlternates).
func writeRepo(dir string, head []byte, alternate string) error {
	for _, d := range []string{"refs/heads", "refs/tags", "objects/pack", "objects/info"} {
		if err := os.MkdirAll(filepath.Join(dir, d), hostbox.DirMode); err != nil {
			return err
		}
	}
	type file struct {
		name string
		body []byte
	}
	files := []file{{"HEAD", head}, {"config", []byte(gitproc.RepoConfig)}}
	if alternate != "" {
		files = append(files, file{filepath.Join("objects", "info", "alternates"),
			[]byte(filepath.Join(alternate, "objects") + "\n")})
	}
	for _, f := range files {
		if err := os.WriteFile(filepath.Join(dir, f.name), f.body, hostbox.FileMode); err != nil {
			return err
		}
	}
	return nil
}

// linkPacks hard-links every file of each of packs — git checksums — from the
// incarnation at from into the store at store: its .pack, its .rev, and its
// .idx LAST (bundleOrder), the very inodes the incarnation holds.
func linkPacks(from, store string, packs []string) error {
	return linkPacksFrom(from, packRel, store, packs)
}

// linkPacksFrom hard-links every file of each of packs from the directory rel
// of the incarnation at from — objects/pack, or its dropping view — into the
// objects/pack of the repository at to, the .idx LAST (bundleOrder).
func linkPacksFrom(from, rel, to string, packs []string) error {
	for _, cs := range packs {
		for _, ext := range bundleOrder {
			name := "pack-" + cs + ext
			if err := os.Link(filepath.Join(from, rel, name), filepath.Join(to, packRel, name)); err != nil {
				return fmt.Errorf("linking pack-%s's %s from %s: %w", cs, ext, rel, err)
			}
		}
	}
	return nil
}

// A REWRITE'S REPOSITORIES (expunge.go): an expunge's build and its rescue
// each write into a scratch/<name>.git whose OWN objects/pack holds hard links
// of the packs the rewrite sorts (the build's inputs, the rescue's dropping
// packs), beside a hard link of the incarnation's packed-refs, so a later
// round's rename leaves it the refs it was made with. What it must never
// pack lies in its ONE ALTERNATE, which `pack-objects --local` reads and never
// writes: for the build, a merge's store of EVERY OTHER LIVE PACK, registered
// as a merge's is (mergeAt); for the rescue, the incarnation's objects/, never
// its dropping view, which only a serving process names
// (gitproc.Cmd.Dropping).

// MADE UNDER THE ENTRY'S WRITE LOCK, WHOLE: a round installs a pack's files,
// renames its packed-refs in, and only then records the pack live (sync.go's
// apply), so a scratch made in between could link refs reaching a pack the
// live set does not list yet, and its build die missing an object. pick reads
// the live set under e.mu and refuses what no rewrite may sort.

// rewritePick is what a rewrite's scratch links: its own packs, by checksum,
// from where the live set places them (place), and — where the scratch leans
// on a store rather than the incarnation (leanStore) — the store's packs, from
// objects/pack.
type rewritePick struct {
	local     []string
	place     packPlace
	store     []string
	leanStore bool
}

// newRewriteScratch makes scratch/<name>.git as pick says over the copy
// SERVING s now, pinning it for the scratch and any store it leans on until
// Remove. A name already there is refused; a scratch not written whole is
// moved into trash/ with its store, its pin given back.
//
// Never over s's own where a swap replaced it (serving): a swap leaves the
// replaced copy no live set (swapFields), so pick would refuse, and the
// repository's merges swap the copy beside every rewrite on its maintainer.
// The serving copy holds every pack s's did but the merged ones, whose
// objects their outputs hold, and a rewrite is valid over any copy at or past
// its scan (an expunge's inputs are held from every merge; a rescue is valid
// at or after its begin).
func (c *Cache) newRewriteScratch(ctx context.Context, s Synced, name string,
	pick func(*incarnation) (rewritePick, error)) (*Scratch, error) {

	if !c.enter() {
		return nil, ErrStopped
	}
	defer c.leave()
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("cache: making %s's scratch for %s: %w", name, s.entry.id, context.Cause(ctx))
	}
	e := s.entry
	if err := c.acquireWrite(ctx, e); err != nil {
		return nil, fmt.Errorf("cache: making %s's scratch for %s: %w", name, e.id, err)
	}
	defer c.releaseWrite(e, writeExclusive)
	e.mu.Lock()
	inc := serving(s.inc)
	if inc.discarded {
		e.mu.Unlock()
		return nil, fmt.Errorf("%w: a scratch names a Synced still held, and %s was released and removed",
			ErrInvalid, s.Dir)
	}
	p, err := pick(inc)
	if err != nil {
		e.mu.Unlock()
		return nil, fmt.Errorf("%w: %s's scratch for %s: %w", ErrInvalid, name, e.id, err)
	}
	inc.pins++
	c.used(e, inc)
	e.mu.Unlock()

	dir := filepath.Join(c.dir, scratchDir, name+".git")
	if err := os.Mkdir(dir, hostbox.DirMode); err != nil {
		c.release(e, inc)
		if errors.Is(err, fs.ErrExist) {
			return nil, fmt.Errorf("%w: %s already exists: an id names one scratch, once", ErrInvalid, dir)
		}
		return nil, fmt.Errorf("cache: making %s: %w", dir, err)
	}
	sc := &Scratch{Dir: dir, Alternate: inc.dir, c: c, e: e, inc: inc}
	sc.remove = sync.OnceFunc(sc.discard)
	if p.leanStore {
		sc.store = filepath.Join(c.dir, scratchDir, mergeStoreName(e.id, name))
		sc.Alternate = sc.store
		e.mu.Lock()
		if e.merges == nil {
			e.merges = map[string]*incarnation{}
		}
		e.merges[sc.store] = inc
		e.mu.Unlock()
	}
	if err := sc.fillRewrite(inc.dir, p); err != nil {
		sc.Remove()
		return nil, fmt.Errorf("cache: making %s for %s: %w", dir, e.id, err)
	}
	return sc, nil
}

// fillRewrite writes a rewrite's scratch over the incarnation at from: its
// store first, where it leans on one; then the scratch, leaning on that store
// or on the incarnation, its packed-refs linked from the incarnation and its
// own packs linked from where the live set places them.
func (s *Scratch) fillRewrite(from string, p rewritePick) error {
	head, err := os.ReadFile(filepath.Join(from, "HEAD"))
	if err != nil {
		return err
	}
	if s.store != "" {
		if err := os.Mkdir(s.store, hostbox.DirMode); err != nil {
			return err
		}
		if err := writeRepo(s.store, head, ""); err != nil {
			return err
		}
		if err := linkPacks(from, s.store, p.store); err != nil {
			return err
		}
	}
	if err := writeRepo(s.Dir, head, s.Alternate); err != nil {
		return err
	}
	if err := os.Link(filepath.Join(from, "packed-refs"), filepath.Join(s.Dir, "packed-refs")); err != nil {
		return err
	}
	return linkPacksFrom(from, p.place.rel(), s.Dir, p.local)
}

// Gather hard-links into s, a publication's scratch ([Cache.NewLayerScratch]),
// the packs from wrote (its repack's outputs, built in a merge's scratch,
// [Cache.NewMergeScratch]), by git checksum: each .pack, .rev and .idx, the
// .idx LAST (bundleOrder). So s's multi-pack-index, which names only packs of
// its own directory, can name them, the adoption of what s built finds them,
// and from can be removed at once.
//
// THE REPACK IS NEVER BUILT IN s: s links every pack of the copy, so its
// name-hash walk would run down the whole history (A MERGE'S REPOSITORIES,
// above). Both scratches are of one repository of this cache; a pack s
// already holds, or one from lacks, is an error, the files linked before it
// left for s's Remove.
func (s *Scratch) Gather(from *Scratch, packs []string) error {
	if from == nil || from.c != s.c || from.e != s.e {
		return fmt.Errorf("%w: %s gathers packs only from a scratch of its own repository", ErrInvalid, s.Dir)
	}
	for _, cs := range packs {
		for _, ext := range bundleOrder {
			rel := filepath.Join("objects", "pack", "pack-"+cs+ext)
			if err := os.Link(filepath.Join(from.Dir, rel), filepath.Join(s.Dir, rel)); err != nil {
				return fmt.Errorf("cache: gathering pack-%s%s into %s: %w", cs, ext, s.Dir, err)
			}
		}
	}
	return nil
}

// ScratchPack is one pack a scratch holds: its .pack's path and its checksum,
// the name git gave it.
type ScratchPack struct {
	Path, Checksum string
}

// Packs is every pack the scratch holds, sorted by checksum — what a
// compaction's pack-objects wrote, one pack an output. A pack is present once
// its .idx is, git's own rule; an .idx without its .pack is an error naming
// it, since the writer that left it is not one that finished.
func (s *Scratch) Packs() ([]ScratchPack, error) {
	packDir := filepath.Join(s.Dir, "objects", "pack")
	list, err := os.ReadDir(packDir)
	if err != nil {
		return nil, fmt.Errorf("cache: listing %s: %w", packDir, err)
	}
	have := map[string]bool{}
	var indexes []string
	for _, d := range list {
		have[d.Name()] = true
		if m := packFileName().FindStringSubmatch(d.Name()); m != nil && m[2] == "idx" {
			indexes = append(indexes, m[1])
		}
	}
	slices.Sort(indexes)
	out := make([]ScratchPack, 0, len(indexes))
	for _, cs := range indexes {
		if !have["pack-"+cs+".pack"] {
			return nil, fmt.Errorf("cache: %s holds pack-%s.idx without its .pack", packDir, cs)
		}
		out = append(out, ScratchPack{Path: filepath.Join(packDir, "pack-"+cs+".pack"), Checksum: cs})
	}
	return out, nil
}

// StorePacks is every pack the store the scratch leans on holds, by checksum,
// sorted: an expunge's rest, which its dropping build excludes by name so its
// walk stops at their objects (internal/gitlet/compact). None for a scratch
// leaning on its incarnation or on nothing. A pack is present once its .idx
// is, as [Scratch.Packs] reads one.
func (s *Scratch) StorePacks() ([]string, error) {
	if s.store == "" {
		return nil, nil
	}
	packDir := filepath.Join(s.store, packRel)
	list, err := os.ReadDir(packDir)
	if err != nil {
		return nil, fmt.Errorf("cache: listing %s: %w", packDir, err)
	}
	var out []string
	for _, d := range list {
		if m := packFileName().FindStringSubmatch(d.Name()); m != nil && m[2] == "idx" {
			out = append(out, m[1])
		}
	}
	slices.Sort(out)
	return out, nil
}

// Pack is the one pack receive-pack wrote into the scratch, its .pack's path
// and checksum, or ok false when the push carried no objects. More than one
// is an error naming them: receive-pack keeps every push as ONE pack
// (receive.unpackLimit=1), so a second is one something else wrote.
func (s *Scratch) Pack() (path, checksum string, ok bool, err error) {
	packs, err := s.Packs()
	switch {
	case err != nil:
		return "", "", false, err
	case len(packs) == 0:
		return "", "", false, nil
	case len(packs) > 1:
		names := make([]string, len(packs))
		for i, p := range packs {
			names[i] = p.Checksum
		}
		return "", "", false, fmt.Errorf("cache: %s holds %d packs (pack-%s), and a push is one",
			filepath.Join(s.Dir, "objects", "pack"), len(packs), strings.Join(names, ", pack-"))
	}
	return packs[0].Path, packs[0].Checksum, true, nil
}

// Remove moves the scratch into trash/ and gives its pin back, once however
// often it is called; called once the git process in it has ended. Once Stop
// has begun it moves nothing (the next Open's boot moves every scratch aside)
// and still gives the pin back.
func (s *Scratch) Remove() { s.remove() }

// discard is Remove's body: the store's registration taken back first, so no
// report of a run over it is routed to a store being removed; the scratch and
// its store moved aside; the pin given back.
func (s *Scratch) discard() {
	c := s.c
	if s.store != "" {
		s.e.mu.Lock()
		delete(s.e.merges, s.store)
		s.e.mu.Unlock()
	}
	if c.enter() {
		for _, dir := range []string{s.Dir, s.store} {
			if dir == "" {
				continue
			}
			if err := toTrash(c.dir, dir, newV7()+"-scratch"); err != nil && !errors.Is(err, fs.ErrNotExist) {
				c.log.Warn("code_cache_scratch_remove_failed", "path", dir, "error", err)
			}
		}
		c.leave()
		c.kick()
	}
	c.release(s.e, s.inc)
}

// Spool is a file for a request's data that outgrew memory: a fetch's
// negotiation past the host's in-memory share, a change's patch for
// `git patch-id` (internal/gitlet/query), or a coding box's push's commit ids
// (internal/gitlet/host's packCommits). The caller closes it.
//
// IT HAS NO NAME: created on the cache's volume (never a TMPDIR that may be
// memory) and unlinked at once, so the kernel removes it when its last
// descriptor closes, a crash included, and no boot finds one. The janitor,
// walking scratch/ by name, never counts it, deliberately: a spool draws on
// its caller's bound (the host's allowance, query.PatchSpoolAllowance,
// gitlet.MaxPackBytes), and counted here one large request would evict warm
// repositories that every other reader then downloads again.
func (c *Cache) Spool() (*os.File, error) {
	if c.isStopped() {
		return nil, ErrStopped
	}
	f, err := os.CreateTemp(filepath.Join(c.dir, scratchDir), "spool-*")
	if err != nil {
		return nil, fmt.Errorf("cache: make a request's spool: %w", err)
	}
	if err := os.Remove(f.Name()); err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("cache: unlink a request's spool %s: %w", f.Name(), err)
	}
	return f, nil
}
