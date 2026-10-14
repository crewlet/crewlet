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
// Both are one request's, and written as such: plain writes with no fsync,
// because nothing outlives the request — a boot moves every scratch of the
// process before it into trash/ — and the files are on the disk once:
// measured by the janitor as what scratch/ holds, a view's linked packed-refs
// and an adopted pack counted where their incarnation is (sizeUnder's
// soleNames).

// Scratch is a repository a push's receive-pack runs in (NewScratch), or an
// advertisement view (AdvertiseView). It pins the incarnation it leans on
// until Remove: its alternate must outlive the push, so the budget never
// evicts it from under one.
type Scratch struct {
	// Dir is the repository: absolute and clean, scratch/<push-id>.git or
	// scratch/adv-<uuid>.git.
	Dir string

	c    *Cache
	e    *entry
	inc  *incarnation
	view bool
	// remove is Remove's body, run once.
	remove func()
}

// NewScratch makes the repository a push's receive-pack runs in: HEAD the
// bytes of s's, the pool's config, refs/heads/ and refs/tags/ EMPTY, and one
// alternate — s's objects — so a thin pack is completed and checked against
// the cache without a byte of it entering the cache. It pins s's incarnation
// until Remove. pushID names it — a canonical lowercase UUID, since it is a
// path component — and a push id names one scratch, once. Its receive-pack
// NEVER runs with [Cache.AdvertiseConfig]: the alternate's refs are the
// only negative tips of its connectivity check.
func (c *Cache) NewScratch(ctx context.Context, s Synced, pushID string) (*Scratch, error) {
	if err := c.handedOut(s, "a scratch"); err != nil {
		return nil, err
	}
	if !canonicalID(pushID) {
		return nil, fmt.Errorf("%w: push id %q is not a canonical lowercase UUID (it becomes a path component)",
			ErrInvalid, pushID)
	}
	return c.newScratch(ctx, s, pushID+".git", false)
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
	return c.newScratch(ctx, s, "adv-"+newV7()+".git", true)
}

// newScratch makes scratch/<name>, pinning s's incarnation for it. A
// directory already there under the name is refused; one that could not be
// written whole is moved into trash/ and its pin given back.
func (c *Cache) newScratch(ctx context.Context, s Synced, name string, view bool) (*Scratch, error) {
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
	inc.pins++
	inc.lastUsed = c.now()
	e.mu.Unlock()

	dir := filepath.Join(c.dir, scratchDir, name)
	if err := os.Mkdir(dir, hostbox.DirMode); err != nil {
		c.release(e, inc)
		if errors.Is(err, fs.ErrExist) {
			return nil, fmt.Errorf("%w: %s already exists: a push id names one scratch, once", ErrInvalid, dir)
		}
		return nil, fmt.Errorf("cache: making %s: %w", dir, err)
	}
	sc := &Scratch{Dir: dir, c: c, e: e, inc: inc, view: view}
	sc.remove = sync.OnceFunc(sc.discard)
	if err := sc.fill(s.Dir); err != nil {
		sc.Remove()
		return nil, fmt.Errorf("cache: making %s for %s: %w", dir, e.id, err)
	}
	return sc, nil
}

// fill writes the scratch's repository, leaning on the incarnation at from.
func (s *Scratch) fill(from string) error {
	for _, d := range []string{"refs/heads", "refs/tags", "objects/pack", "objects/info"} {
		if err := os.MkdirAll(filepath.Join(s.Dir, d), hostbox.DirMode); err != nil {
			return err
		}
	}
	head, err := os.ReadFile(filepath.Join(from, "HEAD"))
	if err != nil {
		return err
	}
	for _, f := range []struct {
		name string
		body []byte
	}{
		{"HEAD", head},
		{"config", []byte(gitproc.RepoConfig)},
		// One absolute, clean line ending in a newline: exactly what the
		// pool's alternates guard admits.
		{filepath.Join("objects", "info", "alternates"), []byte(filepath.Join(from, "objects") + "\n")},
	} {
		if err := os.WriteFile(filepath.Join(s.Dir, f.name), f.body, hostbox.FileMode); err != nil {
			return err
		}
	}
	if s.view {
		return os.Link(filepath.Join(from, "packed-refs"), filepath.Join(s.Dir, "packed-refs"))
	}
	return nil
}

// Pack is the one pack receive-pack wrote into the scratch — the .pack's path
// and its checksum — or ok false when the push carried no objects. A pack is
// present once its .idx is, git's own rule; an .idx without its .pack, or
// more than one pack, is an error naming them: receive-pack keeps every push
// as ONE pack (receive.unpackLimit=1), so either is a scratch something else
// wrote into.
func (s *Scratch) Pack() (path, checksum string, ok bool, err error) {
	packDir := filepath.Join(s.Dir, "objects", "pack")
	list, err := os.ReadDir(packDir)
	if err != nil {
		return "", "", false, fmt.Errorf("cache: listing %s: %w", packDir, err)
	}
	have := map[string]bool{}
	var indexes []string
	for _, d := range list {
		have[d.Name()] = true
		if m := packFileName.FindStringSubmatch(d.Name()); m != nil && m[2] == "idx" {
			indexes = append(indexes, m[1])
		}
	}
	switch len(indexes) {
	case 0:
		return "", "", false, nil
	case 1:
	default:
		slices.Sort(indexes)
		return "", "", false, fmt.Errorf("cache: %s holds %d packs (pack-%s), and a push is one",
			packDir, len(indexes), strings.Join(indexes, ", pack-"))
	}
	cs := indexes[0]
	if !have["pack-"+cs+".pack"] {
		return "", "", false, fmt.Errorf("cache: %s holds pack-%s.idx without its .pack", packDir, cs)
	}
	return filepath.Join(packDir, "pack-"+cs+".pack"), cs, true, nil
}

// Remove moves the scratch into trash/ and gives its pin back: once, however
// often it is called. Once Stop has begun it moves nothing — the directory is
// the next Open's, whose boot moves every scratch aside — and still gives the
// pin back. Called once the receive-pack in it has ended.
func (s *Scratch) Remove() { s.remove() }

// discard is Remove's body.
func (s *Scratch) discard() {
	c := s.c
	if c.enter() {
		if err := toTrash(c.dir, s.Dir, newV7()+"-scratch"); err != nil && !errors.Is(err, fs.ErrNotExist) {
			c.log.Warn("code_cache_scratch_remove_failed", "path", s.Dir, "error", err)
		}
		c.leave()
		c.kick()
	}
	c.release(s.e, s.inc)
}

// Spool is a file a request's body that outgrew memory is written to: a
// fetch's negotiation past the host's in-memory share, which a partial clone's
// checkout reaches by naming every blob it lacks in one request.
//
// IT HAS NO NAME. It is created under scratch/ — on the cache's volume, never
// a TMPDIR that may be memory — and unlinked at once, so it is the OPEN FILE
// and nothing else: the kernel removes it when the last descriptor closes,
// whatever the request's outcome and however the process ends, a crash
// included, so no spool outlives its request and none is left for a boot to
// find; and the janitor, which measures scratch/ by walking its names, never
// counts one against the cache's budget. That is deliberate: a spool draws on
// the host's own allowance (internal/code/host), and counted here, one
// reader's large request would make the budget evict warm repositories that
// every other reader then downloads again. The caller closes it.
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
