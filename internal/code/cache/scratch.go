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

	"github.com/crewlet/crewlet/internal/code"
	"github.com/crewlet/crewlet/internal/code/gitproc"
	"github.com/crewlet/crewlet/internal/hostbox"
)

// THE RECEIVE PATH'S REPOSITORIES. receive-pack NEVER RUNS IN A CACHE: a
// push's runs in a SCRATCH repository under scratch/ whose one alternate is
// the cache's objects, so a thin pack is completed and checked against
// everything the cache holds without a byte of it entering the cache. That
// is what keeps a cache a function of rows: under protocol v2 any object
// present is fetchable by id, so a pack a push left in a cache before its
// row applied — a push the committer refused, one whose node died before
// its record landed — would be served by this node and no other, and a later
// push could connect against it and land a ref no other node can
// materialise. A pushed pack enters a cache only through Adopt, once its row
// has applied, or as every other pack does, through a sync.
//
// A scratch holds NO REFS: receive-pack hands every command to the
// proc-receive hook without comparing it with the scratch's own refs, and
// checks connectivity against the alternate's — so an empty refs/ costs the
// push nothing and leaves no second copy of the refs to disagree with the
// rows. A push's ADVERTISEMENT runs in a VIEW: a scratch whose packed-refs is
// the incarnation's own file under a second name (a hard link), so
// receive-pack lists the cache's refs without running in the cache, and a
// sync that replaces the cache's packed-refs by rename meanwhile leaves the
// view naming the refs it was made with.
//
// A MERGE'S REPOSITORIES. A merge of a repository's packs — a compaction's
// build, or a publication's repack (internal/code/compact) — writes its
// outputs into a scratch too, and
// that scratch leans NOT on the incarnation but on an OBJECT STORE HOLDING
// EXACTLY THE PACKS IT MERGES (NewMergeScratch): a repository of its own under
// scratch/, merge.<repository>.<id>.git, whose objects/pack holds each merged
// pack's .pack, .idx and .rev as hard links of the incarnation's — the very
// inodes, so it holds no byte of its own while the incarnation stands — and
// nothing else: no other pack, no multi-pack-index, no commit-graph.
//
// BECAUSE `pack-objects --stdin-packs` WALKS WHAT IT CAN REACH. To fill in the
// name hash every object it writes is sorted by for its delta search, it runs
// a revision walk from every commit its inputs hold, over every tree and blob
// those reach, and only an object of an EXCLUDED pack stops it (git 2.56.0,
// builtin/pack-objects.c's read_stdin_packs: revs.no_kept_objects over the
// packs kept in core). A merge excludes no pack — every object of its inputs
// is written, an object a pack it does not merge also holds among them
// (ADR-0083) — so over the incarnation the walk went from a roll's newest
// commits down the repository's WHOLE HISTORY, keeping a parsed object for
// every object it met: measured at the review that found it, on the node pin,
// a merge of eight one-commit packs (32 objects) beside a history of 400 032
// objects took 1.39 s in the walk, about 3.5 µs an object of the history, for
// a build charged and deadlined by its roll. Over a store of its inputs alone
// the walk ends at their edge, since git walks with revs.ignore_missing_links
// and a parent no input holds is simply not there: 7 ms, and the same pack,
// byte for byte, its name-hash hints the same. So a merge's cost follows what
// it merges, never the history beside it — BenchmarkCompact's
// history=source/roll=16 arms, a roll of code.CompactMinMerge's pushes:
// sixteen one-commit pushes of 20 KB, 64 objects, built in 22.9 ms at a peak
// of 0.95 MiB beside the 100 MB source history's 295 680 objects and in
// 21.3 ms at 0.94 MiB beside nothing. At the floor of eight before it, eight
// one-commit pushes of 20 KB, 24 objects, built in 19.6 ms at 0.8 MiB beside
// the history and in 14.2 ms at 0.75 MiB beside nothing, where over the whole
// copy, as it built before, the same merge of eight took 451 ms and
// 100.8 MiB (git 2.56.0, the in-process store, Go 1.27.2, -benchtime 1x and
// one count each, two E-cores of an Intel Core Ultra 7 265U, which has SHA
// instructions, 2026-10-28).
//
// The store is the merge's, as the scratch is: removed with it (Remove), moved
// aside by a boot with the rest of scratch/, and counted by the janitor only
// for a link whose incarnation has since gone — a link the incarnation still
// names is on the disk once, counted there. What a git process run over it
// reports is about the incarnation the links came from (mergeAt, report.go).

// Both are one request's, and written as such: plain writes with no fsync,
// because nothing outlives the request — a boot moves every scratch of the
// process before it into trash/ — and the files are on the disk once:
// measured by the janitor as what scratch/ holds, a view's linked packed-refs
// and an adopted pack counted where their incarnation is (sizeUnder's
// soleNames).

// Scratch is a repository a push's receive-pack runs in (NewScratch), or a
// merge's pack-objects — a compaction's, or a publication's repack — writes
// its outputs into (NewMergeScratch), or an
// advertisement view (AdvertiseView), or a publication writes its layer in
// (NewLayerScratch). It pins the incarnation it leans on — directly, or
// through the store a merge's links of it make — until Remove: its alternate
// must outlive the push or the build, so the budget never evicts it from under
// one — and a layer's scratch, which links rather than leans, pins it all the
// same, so the cache counts its links' bytes where the incarnation is.
type Scratch struct {
	// Dir is the repository: absolute and clean, scratch/<id>.git — a push's
	// id, a merge's or a publication's — or scratch/adv-<uuid>.git.
	Dir string
	// Alternate is the repository it leans on, whose objects its one
	// alternates line names — the incarnation, or a merge's store of exactly
	// the packs it merges — what every git process run in it names as its
	// gitproc.Cmd.Alternate, since the pool runs in no repository whose
	// alternate the call did not name. "" for a layer's scratch, which holds
	// links of the incarnation's packs and names no alternate.
	Alternate string

	c    *Cache
	e    *entry
	inc  *incarnation
	view bool
	// store is a merge's store, scratch/merge.<repository>.<id>.git — its
	// Alternate — and empty for a push's scratch and a view.
	store string
	// remove is Remove's body, run once.
	remove func()
}

// NewScratch makes the repository a push's receive-pack runs in: HEAD the
// bytes of s's, the pool's config, refs/heads/ and refs/tags/ EMPTY, and one
// alternate — s's objects — so a thin pack is completed and checked against
// the cache without a byte of it entering the cache. It pins s's incarnation
// until Remove. id names it — the push's id: a canonical lowercase UUID, since
// it is a path component — and an id names one scratch, once. Its
// receive-pack NEVER runs with [Cache.AdvertiseConfig]: the alternate's refs
// are the only negative tips of its connectivity check.
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

// NewMergeScratch makes the repository a MERGE's pack-objects writes its
// outputs into — a compaction's build, or a publication's repack
// (internal/code/compact), whose outputs are then gathered into its layer's
// scratch ([Scratch.Gather]) — shaped as
// NewScratch's, but leaning on a STORE OF EXACTLY THE PACKS IT MERGES rather
// than on s's incarnation: packs names each by its git checksum, every one a
// pack live in the incarnation s pins, and its .pack, .idx and .rev are
// hard-linked from there into scratch/merge.<repository>.<id>.git, the
// scratch's one alternate (A MERGE'S REPOSITORIES, above). So a
// `pack-objects --stdin-packs` run in it reads every pack it merges through
// its alternate, writes into the scratch alone, and walks for its name hashes
// nothing its inputs do not hold. It pins s's incarnation until Remove, which
// takes the store with the scratch. id names it — the merge's: a canonical
// lowercase UUID, since it is a path component — and an id names one scratch,
// once.
//
// A merge of no pack, a pack named twice and a pack the incarnation does not
// hold live in objects/pack — one only rows an expunge's begin left dropping
// name lies in its dropping view — are refused ([ErrInvalid]), with nothing
// made: the store holds what the merge reads, the incarnation's live files
// alone. A file that could not be linked — one a repair is replacing in place
// this moment — is the error, the scratch and its store moved aside.
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

// NewLayerScratch makes the repository a publication writes the repository's
// next published layer in (internal/code/compact): scratch/<id>.git holding,
// by HARD LINKS of s's incarnation, every pack file the copy holds and its
// packed-refs; HEAD the bytes of s's and the pool's config; and the files of
// the first base layers of the rows' chain — each layer's multi-pack-index,
// its bitmap and its commit-graph layer — with a multi-pack-index-chain and a
// commit-graph-chain naming EXACTLY those base layers, root first. It pins s's
// incarnation until Remove. id names it — the publication's own id, a
// canonical lowercase UUID, since it is a path component — once.
//
// LINKS, NEVER AN ALTERNATE, for two reasons git gives: a multi-pack-index
// names packs of ITS OWN pack directory, which an alternate's are not, and the
// bitmap's writer selects its commits off the repository's own refs. A link
// is the same inode, and nothing here writes one: every git process in the
// scratch writes new files by renaming them in, as the incarnation's own
// writers do. Three links a pack, as a swap pays.
//
// THE CHAIN FILES NAME EXACTLY THE BASE, never the whole chain: measured on
// git 2.56.0, a write over a base with layers above it still named in the
// chain file took 196–200 s against 1.03 s for the same write with only its
// base named, and so did `multi-pack-index compact` of two layers with one
// above them. And a layer is written over a base the copy HOLDS: a base past
// the installed prefix ([Chain.Installed]) is refused with ErrInvalid, since a
// layer is valid only over its exact base and the files of one the copy does
// not hold are not there to link.
//
// THE REFS ARE LINKED BEFORE THE PACKS ARE LISTED: a round over the copy
// installs a pack before it renames the packed-refs naming its commits in, so
// every commit the linked refs name is in a pack the listing finds, and the
// bitmap's writer, which parses every commit a ref names, never meets one the
// scratch lacks. A pack a round is still installing meanwhile — its index the
// last of its files renamed in — is one git does not read, and no plan named.
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
		c.release(e, inc, noReader)
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
func (s *Scratch) fillLayer(from string, under []code.LayerRow) error {
	for _, d := range []string{"refs/heads", "refs/tags", layersDir, graphLayersDir} {
		if err := os.MkdirAll(filepath.Join(s.Dir, filepath.FromSlash(d)), hostbox.DirMode); err != nil {
			return err
		}
	}
	head, err := os.ReadFile(filepath.Join(from, "HEAD"))
	if err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(s.Dir, "HEAD"), head, hostbox.FileMode); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(s.Dir, "config"), []byte(gitproc.RepoConfig), hostbox.FileMode); err != nil {
		return err
	}
	if err := os.Link(filepath.Join(from, "packed-refs"), filepath.Join(s.Dir, "packed-refs")); err != nil {
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
// without running in the cache, and a sync that replaces the cache's
// packed-refs meanwhile — by rename, never in place — does not change what
// the view says. Its receive-pack runs with [Cache.AdvertiseConfig] beside
// the receive configuration, so it does not list its alternate's refs a
// second time.
func (c *Cache) AdvertiseView(ctx context.Context, s Synced) (*Scratch, error) {
	if err := c.handedOut(s, "an advertisement view"); err != nil {
		return nil, err
	}
	return c.newScratch(ctx, s, "adv-"+newV7(), true, nil)
}

// mergeStoreName is the name under scratch/ of the store the merge named id of
// repository repoID leans on: merge.<repoID>.<id>.git — the repository, so a
// report of a git process run over it finds whose it is (mergeAt), and the
// merge's own id, so no two merges' stores share a name.
func mergeStoreName(repoID, id string) string { return "merge." + repoID + "." + id + ".git" }

// mergeAt is the entry whose merge's store dir is, and the incarnation the
// store's links came from — or nil: a git process run over the store, a
// merge's build, reads nothing but those links, the very inodes that
// incarnation holds, so what it reports is about that copy (report.go). A
// store whose scratch is removed is none.
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

// newScratch makes scratch/<name>.git, pinning s's incarnation for it — and,
// for a merge of packs, the store of exactly them it leans on, registered on
// the entry for the observer (mergeAt). A directory already there under the
// name is refused; one that could not be written whole is moved into trash/,
// with its store, and its pin given back.
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
		c.release(e, inc, noReader)
		if errors.Is(err, fs.ErrExist) {
			return nil, fmt.Errorf("%w: %s already exists: an id names one scratch, once", ErrInvalid, dir)
		}
		return nil, fmt.Errorf("cache: making %s: %w", dir, err)
	}
	sc := &Scratch{Dir: dir, Alternate: s.Dir, c: c, e: e, inc: inc, view: view}
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
	if err := sc.fill(s.Dir, merge); err != nil {
		sc.Remove()
		return nil, fmt.Errorf("cache: making %s for %s: %w", dir, e.id, err)
	}
	return sc, nil
}

// mergedLive holds a merge's packs to inc's live set — each the checksum of a
// pack the set places in objects/pack (placeOf), named once — and a push's
// scratch or a view, which names none, to nothing; e.mu is held. The live set
// holds DROPPING rows too, a pack only they name lying in the copy's dropping
// view (packPlace), where the store's links (linkPacks) find nothing: no
// merge reads one — a compaction never takes one as an input ([Cache.Packs])
// — so one named is refused here, with nothing made, rather than failing its
// link with the scratch half written.
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

// fill writes the scratch's repository over the incarnation at from: leaning
// on it, for a push's scratch or a view; for a merge, leaning on the store of
// exactly the packs merge names, written and linked from it first.
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
// and — where alternate is set — the one alternates line naming its objects:
// exactly what the pool holds a repository to for a call whose Alternate is
// alternate, and no such file where it is not (gitproc's ShapeAlternates).
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
// each write into a scratch of their own, scratch/<name>.git, whose OWN
// objects/pack holds the packs whose objects the rewrite sorts — the build's
// inputs, the rescue's dropping packs — as hard links of the incarnation's,
// with the incarnation's packed-refs linked beside them: a hard link, so a
// round that renames a new packed-refs in afterwards leaves the scratch the
// refs it was made with. What the rewrite must never pack lies in its ONE
// ALTERNATE, which `pack-objects --local` reads through and never writes: the
// build's, a merge's store of EVERY OTHER LIVE PACK, linked the same way and
// registered for the observer as a merge's is (mergeAt); the rescue's, the
// incarnation's objects/ itself — its live packs alone, never its dropping
// view, which only a serving process names (gitproc.Cmd.Dropping).
//
// MADE UNDER THE ENTRY'S WRITE LOCK, WHOLE: a round installs a pack's files,
// renames its packed-refs in, and only then records the pack in its live set
// (sync.go's apply), so a scratch that linked the refs a round had just
// renamed in and listed the live set before the round recorded the pack would
// lean on no file holding what those refs reach — a build that dies missing
// an object, at every pass of a repository pushed to every second. With no
// round between them, every object the linked refs reach is in a pack the live
// set lists. pick reads the live set under the entry's mu — the packs the
// scratch links and, for a build, its store's — and refuses what no rewrite
// may sort.

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

// newRewriteScratch makes scratch/<name>.git as pick says, pinning s's
// incarnation for it — and for the store it leans on, where pick names one —
// until Remove. A name already there is refused; a scratch that could not be
// written whole is moved into trash/ with its store, and its pin given back.
func (c *Cache) newRewriteScratch(ctx context.Context, s Synced, name string,
	pick func(*incarnation) (rewritePick, error)) (*Scratch, error) {

	if !c.enter() {
		return nil, ErrStopped
	}
	defer c.leave()
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("cache: making %s's scratch for %s: %w", name, s.entry.id, context.Cause(ctx))
	}
	e, inc := s.entry, s.inc
	if err := c.acquireWrite(ctx, e); err != nil {
		return nil, fmt.Errorf("cache: making %s's scratch for %s: %w", name, e.id, err)
	}
	defer c.releaseWrite(e, writeExclusive)
	e.mu.Lock()
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
		c.release(e, inc, noReader)
		if errors.Is(err, fs.ErrExist) {
			return nil, fmt.Errorf("%w: %s already exists: an id names one scratch, once", ErrInvalid, dir)
		}
		return nil, fmt.Errorf("cache: making %s: %w", dir, err)
	}
	sc := &Scratch{Dir: dir, Alternate: s.Dir, c: c, e: e, inc: inc}
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
	if err := sc.fillRewrite(s.Dir, p); err != nil {
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

// Gather links into s, a publication's scratch of links ([Cache.NewLayerScratch]),
// the packs from wrote — its repack's outputs, built in a merge's scratch of
// their own ([Cache.NewMergeScratch]) — by their git checksums: each one's
// .pack, .rev and .idx, the .idx LAST (bundleOrder), hard-linked into s's
// objects/pack, the very inodes from holds. So s's multi-pack-index can name
// them, a multi-pack-index naming only packs of its own directory, and the
// adoption of what s built finds them there; from can be removed at once.
//
// THE REPACK IS NEVER BUILT IN s: s links every pack of the copy, so
// `pack-objects --stdin-packs` there, filling in its name hashes, walks from
// the inputs' commits down the repository's whole history (A MERGE'S
// REPOSITORIES, above) — what a merge's store of exactly its inputs exists to
// end. Both scratches are of one repository of this cache; a pack s already
// holds, or one from does not, is an error, the files linked before it left
// in s, which its Remove takes.
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
// sorted — an expunge's rest, which its dropping build excludes by name so
// its walk stops at their objects (internal/code/compact) — and none for a
// scratch leaning on its incarnation or on nothing. A pack is present once its
// .idx is, as [Scratch.Packs] reads one.
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

// Pack is the one pack receive-pack wrote into the scratch — the .pack's path
// and its checksum — or ok false when the push carried no objects: [Scratch.Packs]
// held to a push's one. More than one pack is an error naming them:
// receive-pack keeps every push as ONE pack (receive.unpackLimit=1), so a push's
// scratch holding two is one something else wrote into.
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

// Remove moves the scratch into trash/ and gives its pin back: once, however
// often it is called. Once Stop has begun it moves nothing — the directory is
// the next Open's, whose boot moves every scratch aside — and still gives the
// pin back. Called once the receive-pack in it has ended.
func (s *Scratch) Remove() { s.remove() }

// discard is Remove's body: the scratch and, for a merge, its store moved
// aside — the store's registration taken back first, so no report of a run
// over it is routed to a store being removed — and the pin given back.
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
	c.release(s.e, s.inc, noReader)
}

// Spool is a file a request's body that outgrew memory is written to: a
// fetch's negotiation past the host's in-memory share, which a partial clone's
// checkout reaches by naming every blob it lacks in one request. And the file
// a revision's facts write a change's patch into, for `git patch-id` to read
// back as the next process of the read lane (internal/code/query).
//
// IT HAS NO NAME. It is created under scratch/ — on the cache's volume, never
// a TMPDIR that may be memory — and unlinked at once, so it is the OPEN FILE
// and nothing else: the kernel removes it when the last descriptor closes,
// whatever the request's outcome and however the process ends, a crash
// included, so no spool outlives its request and none is left for a boot to
// find; and the janitor, which measures scratch/ by walking its names, never
// counts one against the cache's budget. That is deliberate: a spool draws on
// its caller's own bound — a fetch's on the host's allowance
// (internal/code/host), a patch on the bound its patch-id is taken under
// (query.MaxPatchIDDiffBytes) — and counted here, one reader's large request
// would make the budget evict warm repositories that every other reader then
// downloads again. The caller closes it.
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
