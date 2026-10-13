package cache

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"time"

	"github.com/crewlet/crewlet/internal/statelog/metrics"
)

// THE JANITOR: one goroutine, owned by Start, that gives back the disk the
// cache has decided to give back. Whatever is removed is first RENAMED into
// trash/ — an instant at any size, on one filesystem — and removed here, in
// the background: a request that lets go of the last pin on a 20 GiB
// directory, or a boot that moves a crashed process's scratch aside, returns
// at once rather than waiting on the removal.
//
// It is also what makes a repository's LAST USE survive a restart: a boot
// reads it from the disk (the state file's modification), and a state is
// rewritten only when its directory changes, so a repository fetched all day
// and pushed to rarely would otherwise come back from every restart as the
// least recently used entry there is. Every tick stamps the moment of last
// use onto each repository used since the tick before — a metadata write,
// no content and no fsync, so a boot finds it to within [EvictInterval].

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
		c.janitorPass(ctx)
		if ticked {
			c.recordUse(ctx)
		}
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

// janitorPass removes everything in trash/. A removal that fails is logged
// and left for the next pass: what is in trash/ is never read again, so a
// failure costs disk and nothing else. An ended ctx, or a cache that is
// stopping, stops the pass between entries.
func (c *Cache) janitorPass(ctx context.Context) {
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

// setGauges sets crewlet.code.cache.bytes — what every incarnation holds,
// current and retiring, plus the downloads in flight in tmp/ — and
// crewlet.code.cache.repos, the repositories with a directory on this node:
// both from the running totals, so a round that sets them after it ends pays
// two loads, however many repositories the node holds.
func (c *Cache) setGauges() {
	if c.metrics == nil {
		return
	}
	c.metrics.Set(metrics.CodeCacheBytes, float64(c.heldBytes.Load()+c.tmpBytes.Load()), nil)
	c.metrics.Set(metrics.CodeCacheRepos, float64(c.heldRepos.Load()), nil)
}

// kick wakes the janitor without waiting for it: a kick already pending
// covers this one, because a pass removes everything in trash/.
func (c *Cache) kick() {
	select {
	case c.kicks <- struct{}{}:
	default:
	}
}
