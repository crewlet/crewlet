package cache

import (
	"cmp"
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/crewlet/crewlet/internal/statelog/metrics"
)

// THE JANITOR: one goroutine, owned by Start, that gives back the disk the
// cache has decided to give back and holds it to its budget (package doc,
// "Kept while there is room"). What is removed is first RENAMED into trash/,
// instant at any size on one filesystem, so a request releasing the last pin
// on a 20 GiB directory, or a boot moving scratch aside, never waits on it.
//
// It also makes a repository's LAST USE survive a restart. A boot reads it
// from the state file's modification time, and a state is rewritten only when
// its directory changes, so a repository fetched all day would come back as
// the least recently used. Every tick stamps last use onto each repository
// used since the tick before (metadata only, no fsync), so a boot finds it to
// within [EvictInterval].

// janitor runs a pass at once (the boot has just moved scratch/ and tmp/
// into trash/), then whenever it is kicked or [EvictInterval] passes, until
// ctx ends or the cache stops. Between passes it is parked, not running: it
// holds nothing of the directory, so a Stop then has nothing to wait for.
func (c *Cache) janitor(ctx context.Context) {
	var ticks <-chan time.Time
	if c.knobs != nil {
		ticks = c.knobs.ticks
	}
	if ticks == nil {
		tick := time.NewTicker(EvictInterval)
		defer tick.Stop()
		ticks = tick.C
	}
	ticked := false
	for {
		if !c.enter() {
			return
		}
		// A kick sent before this pass lists trash/ is answered by it;
		// left pending, it would run a second pass over nothing.
		select {
		case <-c.kicks:
		default:
		}
		c.janitorPass(ctx, ticked)
		c.leave()
		ticked = false
		select {
		case <-ctx.Done():
			return
		case <-c.life.Done():
			return
		case <-ticks:
			ticked = true
		case <-c.kicks:
		}
	}
}

// janitorPass is one pass, in order: trash/ emptied; scratch/ measured;
// unclaimed staged layers expired; unlisted memo trees listed (listMemos);
// the code indexes and the memo trimmed to their shares BEFORE any repository
// goes, since an evicted answer costs one git process and an evicted
// repository a cold sync of every pack it holds; repositories evicted to the
// budget (evictToBudget); trash/ emptied again if anything moved there, so an
// eviction's disk comes back within the pass; on a TICKED pass alone, last
// use stamped and moved memos' states written; then the gauges.
//
// States are written once a tick, never once a pass: each is two syncs
// (persist) beside the rounds' on the same disk, and a busy node kicks
// every few misses. A crash costs a boot's memo count (Layout.Held) a tick.
func (c *Cache) janitorPass(ctx context.Context, ticked bool) {
	c.emptyTrash(ctx)
	c.measureScratch(ctx)
	c.expireStagedLayers()
	c.listMemos(ctx, ticked)
	trimmed := c.trimCodeIndexes(ctx)
	trimmed = c.trimMemo(ctx, ticked) || trimmed
	if c.evictToBudget(ctx) > 0 || trimmed {
		c.emptyTrash(ctx)
	}
	if ticked {
		c.recordUse(ctx)
		c.stateMemos(ctx)
	}
	c.setGauges()
}

// emptyTrash removes everything in trash/. A removal that fails is logged and
// left for the next pass: what is in trash/ is never read again, so a failure
// costs disk and nothing else. An ended ctx, or a cache that is stopping,
// stops the pass between entries.
func (c *Cache) emptyTrash(ctx context.Context) {
	trash := filepath.Join(c.dir, trashDir)
	list, err := os.ReadDir(trash)
	if err != nil {
		c.log.Warn("code_cache_trash_failed", "path", trash, "error", err)
		return
	}
	for _, d := range list {
		if ctx.Err() != nil || c.life.Err() != nil {
			return
		}
		path := filepath.Join(trash, d.Name())
		if c.knobs != nil && c.knobs.removing != nil {
			c.knobs.removing(path)
		}
		if err = os.RemoveAll(path); err != nil {
			c.log.Warn("code_cache_trash_failed", "path", path, "error", err)
		}
	}
}

// measureScratch sets what scratch/ holds (a push's scratch repository, an
// advertisement view) from the disk, the only thing that knows: what a
// receive-pack writes there is the client's to decide. An unreadable entry is
// logged and left out; one removed under the walk is nothing to count or log
// (sizeUnder); an ended ctx keeps the last measure.
//
// Only a file whose ONLY NAME is in scratch/ counts. One with a second name is
// the cache's own (a view's packed-refs, an adopted push's pack linked into
// its incarnation), already in the total through that incarnation; counted
// again, every adopted push would evict as many bytes more, each a cold sync
// later. The other name is never in trash/: the pass empties it first.
func (c *Cache) measureScratch(ctx context.Context) {
	var listing func(dir string)
	if c.knobs != nil {
		listing = c.knobs.measuring
	}
	n, err := sizeUnder(ctx, filepath.Join(c.dir, scratchDir), soleNames, func(path string, err error) {
		c.log.Warn("code_cache_scratch_unmeasured", "path", path, "error", err)
	}, listing)
	if err == nil {
		c.scratchBytes.Store(n)
	}
}

// total is what the cache holds against its budget: every incarnation's
// files, current and retiring, the downloads, staged adoptions and staged
// memo pages in tmp/, and scratch/ as the janitor last measured it — a
// scratch file a staging links is counted in tmp/ alone, since it has two
// names.
func (c *Cache) total() int64 {
	return c.heldBytes.Load() + c.tmpBytes.Load() + c.scratchBytes.Load()
}

// memoShare is the most the memo may hold of the budget: the budget divided
// by MemoShare.
func (c *Cache) memoShare() int64 { return c.maxBytes / MemoShare }

// memoExcess is what the memo holds past its share — the pages it keeps
// (memoBytes) and those reads' runs have staged for it (memoStaged)
// together — or zero: what the memo's own trim gives back (trimMemo).
func (c *Cache) memoExcess() int64 {
	return max(c.memoBytes.Load()+c.memoStaged.Load()-c.memoShare(), 0)
}

// overBudget is what the cache holds past its budget that a REPOSITORY'S
// eviction is for, positive when one is due: its total less the memo's and
// the code indexes' excess over their shares (memoExcess, codeIndexExcess).
// That excess is their own trims' to give back: a repository evicted for it
// costs a cold sync of every pack it holds, a GET each, for bytes the memo
// was never entitled to, and the trim takes them anyway.
func (c *Cache) overBudget() int64 {
	return c.total() - c.memoExcess() - c.codeIndexExcess() - c.maxBytes
}

// evictToBudget evicts repositories, least recently used first, while an
// eviction is due (overBudget), and answers how many. A candidate that stops
// being evictable before its turn is passed over.
func (c *Cache) evictToBudget(ctx context.Context) int {
	if c.overBudget() <= 0 {
		return 0
	}
	type candidate struct {
		e        *entry
		lastUsed time.Time
	}
	var candidates []candidate
	c.mu.Lock()
	for _, e := range c.entries {
		e.mu.Lock()
		if evictable(e) {
			candidates = append(candidates, candidate{e: e, lastUsed: e.current.lastUsed})
		}
		e.mu.Unlock()
	}
	c.mu.Unlock()
	slices.SortFunc(candidates, func(a, b candidate) int {
		return cmp.Or(a.lastUsed.Compare(b.lastUsed), strings.Compare(a.e.id, b.e.id))
	})
	evicted := 0
	for _, cand := range candidates {
		if c.overBudget() <= 0 || ctx.Err() != nil || c.life.Err() != nil {
			break
		}
		if c.evict(cand.e) {
			evicted++
		}
	}
	return evicted
}

// evictable reports whether e may be evicted now; e.mu is held. Its current
// incarnation unpinned (by a request or the maintainer), no round running or
// queued, no maintenance running, and no RETIRING incarnation: a reader still
// on one a rebuild replaced keeps that directory in repos/ and is served from
// it whatever becomes of the entry.
func evictable(e *entry) bool {
	return !e.dead && e.current != nil && e.current.pins == 0 && e.running == nil && e.next == nil &&
		e.maint == nil && len(e.retiring) == 0
}

// evict takes e out of the cache: the entry dead and gone from the map, so
// the next sync starts cold with a new entry, and its incarnation's directory
// renamed into trash/. The DECISION is made under the cache's mu then the
// entry's, evictable checked again, so nothing pinned or started since it was
// chosen loses its directory. The rename and the log come after both are let
// go: every sync on the node takes the cache's mu to find its entry, and a
// tight budget evicts after nearly every cold sync. A sync in between makes a
// new entry, whose first incarnation takes the next free number beside the
// directory still on its way out.
func (c *Cache) evict(e *entry) bool {
	c.mu.Lock()
	e.mu.Lock()
	if !evictable(e) {
		e.mu.Unlock()
		c.mu.Unlock()
		return false
	}
	inc := e.current
	bytes, idle := inc.diskBytes+inc.metaBytes, c.now().Sub(inc.lastUsed)
	indexed := inc.codeIndexRecord() != nil
	e.dead = true
	if c.entries[e.id] == e {
		delete(c.entries, e.id)
	}
	gone := c.retire(e, inc)
	e.mu.Unlock()
	c.mu.Unlock()
	if indexed {
		// A copy the indexer keeps an index in: filled again only into room
		// for what it held (indexTaken, codeindex.go).
		c.tookBack(e.id, bytes, 0)
	}
	c.trash(gone)
	c.churned(churned{evictions: 1})
	c.log.Info("code_cache_evicted", "repo", e.id, "bytes", bytes, "reason", "budget", "idle_for", idle)
	return true
}

// recordUse stamps, for every incarnation used since its last stamp, its last
// use (a reader's last take or release) onto the disk for the next boot
// (stampUse). One metadata write per
// repository used, never a rewrite of the state, so a repository only ever
// read costs nothing more. A failed stamp is logged and tried next tick; an
// incarnation retired meanwhile has moved and is passed over.
func (c *Cache) recordUse(ctx context.Context) {
	type due struct {
		e   *entry
		inc *incarnation
		at  time.Time
	}
	var dues []due
	c.mu.Lock()
	for _, e := range c.entries {
		e.mu.Lock()
		if inc := e.current; inc != nil && inc.lastUsed.After(inc.stamped) {
			dues = append(dues, due{e: e, inc: inc, at: inc.lastUsed})
		}
		e.mu.Unlock()
	}
	c.mu.Unlock()
	for _, d := range dues {
		if ctx.Err() != nil || c.life.Err() != nil {
			return
		}
		if err := stampUse(d.inc.dir, d.at); err != nil {
			c.log.Warn("code_cache_use_unrecorded", "path", d.inc.dir, "error", err)
			continue
		}
		d.e.mu.Lock()
		if d.at.After(d.inc.stamped) {
			d.inc.stamped = d.at
		}
		d.e.mu.Unlock()
	}
}

// stampUse sets the modification time a boot reads an incarnation's last use
// from (lastUsedOn): its state file's, else its directory's. An incarnation
// retired into trash/ since it was chosen is no failure.
func stampUse(dir string, at time.Time) error {
	err := os.Chtimes(filepath.Join(dir, stateName), time.Time{}, at)
	if errors.Is(err, fs.ErrNotExist) {
		err = os.Chtimes(dir, time.Time{}, at)
	}
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	return err
}

// setGauges sets the cache's gauges (crewlet.code.cache.bytes is total, what
// the budget is held to) from running totals, so a round that sets them as it
// ends pays a few loads however many repositories the node holds.
func (c *Cache) setGauges() {
	if c.metrics == nil {
		return
	}
	c.metrics.Set(metrics.CodeCacheBytes, float64(c.total()), nil)
	c.metrics.Set(metrics.CodeCacheRepos, float64(c.heldRepos.Load()), nil)
	c.metrics.Set(metrics.CodeCacheMemoBytes, float64(c.memoBytes.Load()), nil)
	c.metrics.Set(metrics.CodeIndexBytes, float64(c.codeIndexBytes.Load()), nil)
	c.metrics.Set(metrics.CodeCacheDroppingBytes, float64(c.droppingBytes.Load()), nil)
	c.metrics.Set(metrics.CodeCacheChained, float64(c.chainedRepos.Load()), nil)
}

// kick wakes the janitor without waiting for it: a kick already pending
// covers this one, because a pass removes everything in trash/.
func (c *Cache) kick() {
	select {
	case c.kicks <- struct{}{}:
	default:
	}
}
