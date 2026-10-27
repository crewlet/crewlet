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
// cache has decided to give back, and holds the cache to its budget. Whatever
// is removed is first RENAMED into trash/ — an instant at any size, on one
// filesystem — and removed here, in the background: a request that lets go
// of the last pin on a 20 GiB directory, or a boot that moves a crashed
// process's scratch aside, returns at once rather than waiting on the
// removal.
//
// A CACHE IS EVICTED TO ITS BUDGET AND FOR NO OTHER REASON. Over the budget,
// the repositories used least recently go first, each a directory renamed
// into trash/; under it, nothing goes, however long a repository has been
// idle — and what the memo holds past its share is never a repository's to
// make room for (overBudget): the memo's own trim gives it back. An idle repository costs disk the budget already bounds, while
// evicting it costs ONE GET A PUSH the next time anybody fetches it — nothing
// compacts a repository's packs — so a weekend's idle eviction was every
// repository nobody fetched re-downloaded whole on Monday, on every git node.
// A cache older than the rows' tombstone horizon is brought forward by a
// whole read that reuses every pack it holds, so correctness never needed an
// eviction either. A repository a request holds is never evicted, even over
// the budget: it is served, and goes the first pass after its last reader —
// or the maintenance run that pinned it — lets go, which kicks the janitor
// for exactly that, as a round that grows the cache past its budget does,
// rather than leaving it to the tick.
//
// It is also what makes a repository's LAST USE survive a restart: a boot
// reads it from the disk (the state file's modification), and a state is
// rewritten only when its directory changes, so a repository fetched all day
// and pushed to rarely would otherwise come back from every restart as the
// least recently used entry there is — the first the budget evicts. Every
// tick stamps the moment of last use onto each repository used since the tick
// before — a metadata write, no content and no fsync, so a boot finds it to
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

// janitorPass is one pass, in order: trash/ emptied; scratch/ measured; every
// copy's memo tree this process has not yet listed listed, its cohorts' names
// alone (memo.go); the memo trimmed to its share of the budget (MemoShare) a
// cohort at a time, the oldest first, the pages runs have staged for it
// counted with those it keeps — and on a ticked pass held to maxMemoCohorts
// too — BEFORE any repository goes — an answer
// evicted costs one git process to compute again, a repository a cold sync of
// every pack it holds; the cache evicted to its budget for what the
// repositories hold past their room (overBudget), least recently used first —
// and trash/ emptied again when anything was moved there, so an eviction gives
// its disk back within the pass; on a TICKED pass alone, every repository's
// last use stamped and the states whose memo moved written again; then the
// gauges.
//
// THE STATES ONCE A TICK, never once a pass: a state written is two syncs —
// the file and its directory (persist) — on the janitor's goroutine, beside
// the durability syncs of rounds and adoptions on the same disk, and passes
// follow kicks, which on a busy node come after every few misses (a placement
// past the share, a release with an eviction due). Written at most once an
// EvictInterval a copy, a boot still counts the memo within a tick of its
// writes (Layout.Held), which is all the state is there for: a memo is a
// cache of a cache, and the bytes a crash costs the count are a tick's.
func (c *Cache) janitorPass(ctx context.Context, ticked bool) {
	c.emptyTrash(ctx)
	c.measureScratch(ctx)
	// A layer staged for a placement that took none of it within
	// objstore.RecordWithin: an adoption whose record never applied
	// (AdoptLayer), or a fetch a moved placement held (holdFetched) that
	// every plan of its copy since has passed by.
	c.expireStagedLayers()
	c.listMemos(ctx)
	trimmed := c.trimMemo(ctx, ticked)
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

// measureScratch sets what scratch/ holds — a push's scratch repository, an
// advertisement view — from the disk, the only thing that knows: what a
// receive-pack writes there is the client's to decide. An entry there that
// cannot be read is logged and left out, while one the host removes from
// under the walk is nothing to count and nothing to log (sizeUnder); an
// ended ctx keeps the last measure.
//
// Only a file whose ONLY NAME is in scratch/ counts. A file there with a
// second name is one of the cache's own — a view's packed-refs linked from
// its incarnation, an adopted push's pack linked into the incarnation before
// its scratch goes — whose bytes are on the disk once and already in the
// total through that incarnation. Counted again, every adopted push would
// evict as many bytes more of the repositories used least recently, each a
// cold sync later. The other name is never in trash/: the pass empties trash/
// before it measures.
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
// eviction is for: its total less the memo's excess over its share
// (memoExcess), positive when an eviction is due. The memo's excess is its own
// trim's to give back, never a repository's: one evicted for a page a read is
// still cutting, or for pages the memo holds past its share, costs a cold sync
// of every pack it holds — a GET each from the object store — for bytes the
// memo was never entitled to, and the trim then takes the pages anyway.
func (c *Cache) overBudget() int64 { return c.total() - c.memoExcess() - c.maxBytes }

// evictToBudget evicts repositories, least recently used first, while an
// eviction is due (overBudget), and answers how many. Only an EVICTABLE
// entry is a candidate (evictable), and one that stops being one before its
// turn is passed over; none left with an eviction due, and the pass stops
// there.
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
// incarnation unpinned — a request is reading it, or the maintainer is — with
// no round running or queued, no maintenance running, and no RETIRING
// incarnation: a reader still finishing on one a rebuild replaced keeps that
// directory in repos/, and is served from it whatever becomes of the entry.
func evictable(e *entry) bool {
	return !e.dead && e.current != nil && e.current.pins == 0 && e.running == nil && e.next == nil &&
		e.maint == nil && len(e.retiring) == 0
}

// evict takes e out of the cache: the entry dead and gone from the cache's
// map — the next sync of the repository starts cold, with a new entry — and
// its incarnation's directory renamed into trash/. The DECISION is made under
// the cache's mu and then the entry's, the conditions checked again, so
// nothing that pinned or started on it since it was chosen loses its
// directory; the rename and the log line come after both are let go, because
// every sync of every repository on the node takes the cache's mu to find its
// entry, and a budget held tight evicts after nearly every cold sync. A sync
// of the repository in between makes a new entry, whose first incarnation
// takes the next free number beside the directory still on its way out.
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
	e.dead = true
	if c.entries[e.id] == e {
		delete(c.entries, e.id)
	}
	gone := c.retire(e, inc)
	e.mu.Unlock()
	c.mu.Unlock()
	c.trash(gone)
	c.churned(churned{evictions: 1})
	c.log.Info("code_cache_evicted", "repo", e.id, "bytes", bytes, "reason", "budget", "idle_for", idle)
	return true
}

// recordUse stamps each repository's last use onto the disk, where the next
// boot reads it: the modification time of its current incarnation's state
// file — its directory's, when it has none — set to the moment a reader last
// took or released it, for every incarnation used since its last stamp. One
// metadata write per repository used since the last tick, never a rewrite of
// the state, so a repository that is only ever read costs nothing more than
// that. A stamp that fails is logged and tried at the next tick; an
// incarnation retired meanwhile has moved, and is passed over.
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
// that is gone — retired and moved into trash/ since it was chosen — is no
// failure.
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

// setGauges sets crewlet.code.cache.bytes — the total the budget is held to
// (total) — crewlet.code.cache.repos, the repositories with a directory on
// this node, and crewlet.code.cache.memo.bytes, what the memo holds of the
// total: all from the running totals, so a round that sets them after it ends
// pays a few loads, however many repositories the node holds.
func (c *Cache) setGauges() {
	if c.metrics == nil {
		return
	}
	c.metrics.Set(metrics.CodeCacheBytes, float64(c.total()), nil)
	c.metrics.Set(metrics.CodeCacheRepos, float64(c.heldRepos.Load()), nil)
	c.metrics.Set(metrics.CodeCacheMemoBytes, float64(c.memoBytes.Load()), nil)
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
