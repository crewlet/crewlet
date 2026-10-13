package cache

import (
	"context"
	"io"
	"os"
	"time"

	"github.com/crewlet/crewlet/internal/code"
)

// The suite's knobs into the cache: the background loops' steps run
// synchronously, the moments a case must hold still, and what the cache
// holds in memory read back.

// JanitorPass runs one janitor pass, synchronously, as work of the cache's
// own (a stopped cache runs none).
func JanitorPass(c *Cache) {
	if c.enter() {
		c.janitorPass(context.Background())
		c.leave()
	}
}

// RecordUse runs the janitor's tick step, synchronously: every repository
// used since its last stamp has its last use stamped on the disk.
func RecordUse(c *Cache) {
	if c.enter() {
		c.recordUse(context.Background())
		c.leave()
	}
}

// Running is how much of the cache's work is in flight in its directory.
func Running(c *Cache) int {
	c.lifeMu.Lock()
	defer c.lifeMu.Unlock()
	return c.running
}

// WhileTheHookIsOpen makes Open call f while the hook it writes is still
// open for writing.
func WhileTheHookIsOpen(o *Options, f func(*os.File)) {
	if o.knobs == nil {
		o.knobs = &knobs{}
	}
	o.knobs.hookStaged = f
}

// BeforeRemoving makes the janitor call f before it removes each entry of
// trash/.
func BeforeRemoving(o *Options, f func(path string)) {
	if o.knobs == nil {
		o.knobs = &knobs{}
	}
	o.knobs.removing = f
}

// TicksFrom makes the janitor take its ticks from ticks, in place of a
// ticker of EvictInterval: a tick is delivered by a send, and a channel
// nothing sends on is a janitor that never ticks.
func TicksFrom(o *Options, ticks <-chan time.Time) {
	if o.knobs == nil {
		o.knobs = &knobs{}
	}
	o.knobs.ticks = ticks
}

// BeforePeel makes every round call f with the incarnation's directory as its
// peel is about to read it.
func BeforePeel(o *Options, f func(dir string)) {
	if o.knobs == nil {
		o.knobs = &knobs{}
	}
	o.knobs.peeling = f
}

// BuffersOut is the copy buffers the node's download slots hold now, and the
// most they have held at once.
func BuffersOut(c *Cache) (now, most int64) { return c.fetches.out.Load(), c.fetches.most.Load() }

// SlotWaiters is how many downloads wait for one of the node's slots.
func SlotWaiters(c *Cache) int64 { return c.fetches.waiting.Load() }

// Repos is every repository the cache holds, with its current incarnation's
// number.
func Repos(c *Cache) map[string]int {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make(map[string]int, len(c.entries))
	for id, e := range c.entries {
		e.mu.Lock()
		if e.current != nil {
			out[id] = e.current.n
		}
		e.mu.Unlock()
	}
	return out
}

// HasState reports whether repoID's current incarnation has a state.
func HasState(c *Cache, repoID string) bool {
	e := lookup(c, repoID)
	if e == nil {
		return false
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.current != nil && e.current.state != nil
}

// LastUsed is when repoID's current incarnation was last used, as the cache
// holds it.
func LastUsed(c *Cache, repoID string) time.Time {
	e := lookup(c, repoID)
	if e == nil {
		return time.Time{}
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.current == nil {
		return time.Time{}
	}
	return e.current.lastUsed
}

// Pins is the pins on repoID's current incarnation.
func Pins(c *Cache, repoID string) int {
	e := lookup(c, repoID)
	if e == nil {
		return 0
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.current == nil {
		return 0
	}
	return e.current.pins
}

// Incarnation is the number of the incarnation s was handed out of.
func Incarnation(s Synced) int { return s.inc.n }

// Pin hands out repoID's current incarnation as it stands, the way an answer
// from what the cache holds does.
func Pin(c *Cache, repoID string) (Synced, bool) {
	e := lookup(c, repoID)
	if e == nil {
		return Synced{}, false
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.current == nil {
		return Synced{}, false
	}
	return c.pin(e, e.current, SyncNone), true
}

// Retire retires repoID's current incarnation, as a rebuild that replaced it
// or a purge does.
func Retire(c *Cache, repoID string) bool {
	e := lookup(c, repoID)
	if e == nil {
		return false
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.current == nil {
		return false
	}
	c.retire(e, e.current)
	return true
}

// Persist writes repoID's current incarnation's state from its in-memory
// fields, as a round (round) or the maintainer does.
func Persist(ctx context.Context, c *Cache, repoID string, round bool) error {
	e := lookup(c, repoID)
	e.mu.Lock()
	inc := e.current
	e.mu.Unlock()
	return c.persist(ctx, e, inc, round)
}

// MergeRefs merges moves onto the packed-refs in dir — one this cache wrote,
// whose digest is digest — writing the merged file to w as a delta round's
// refs step does, and answers how many refs it wrote.
func MergeRefs(dir string, moves []code.RefRow, digest string, w io.Writer) (int, error) {
	var out refsFile
	err := mergeDelta(dir, w, moves, nil, digest, &out)
	return out.refs, err
}

// SetFetchConcurrency sets how many downloads one round runs at once. Before
// any sync.
func SetFetchConcurrency(c *Cache, n int) { c.fetchConcurrency = n }

// SetNodeFetches sets how many downloads every round on the node shares.
// Before any sync.
func SetNodeFetches(c *Cache, n int64) { c.fetches = newFetchSlots(n) }

// SetInstallHook makes every install call fn: "fetched" once a bundle is
// split and checked, then "pack", "rev" and "idx" after each rename. Before
// any sync.
func SetInstallHook(c *Cache, fn func(stage, packDir string)) { c.installHook = fn }

// Waiting is how many callers wait on a round of repoID.
func Waiting(c *Cache, repoID string) int {
	e := lookup(c, repoID)
	if e == nil {
		return 0
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	n := 0
	for _, r := range []*round{e.running, e.next} {
		if r == nil {
			continue
		}
		for _, w := range r.waiters {
			if !w.gone {
				n++
			}
		}
	}
	return n
}

// Converged reports whether repoID's current incarnation has had a full
// round in this process.
func Converged(c *Cache, repoID string) bool {
	e := lookup(c, repoID)
	if e == nil {
		return false
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.current != nil && e.current.converged
}

func lookup(c *Cache, repoID string) *entry {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.entries[repoID]
}
