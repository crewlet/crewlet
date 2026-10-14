package cache

import (
	"context"
	"io"
	"os"
	"slices"
	"time"

	"github.com/crewlet/crewlet/internal/code"
	"github.com/crewlet/crewlet/internal/objstore"
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

// BeforeVerifyWaits makes Verify call f with the incarnation's directory as it
// is about to wait for the repository's rounds before listing it.
func BeforeVerifyWaits(o *Options, f func(dir string)) {
	if o.knobs == nil {
		o.knobs = &knobs{}
	}
	o.knobs.verifyWaits = f
}

// WhileVerifyLists makes Verify call f with the incarnation's directory while
// it lists it: with the repository's rounds held off and no maintenance run
// allowed to start on it.
func WhileVerifyLists(o *Options, f func(dir string)) {
	if o.knobs == nil {
		o.knobs = &knobs{}
	}
	o.knobs.verifyListing = f
}

// BeforeVerifyFsck makes Verify call f with the incarnation's directory as its
// fsck is about to ask for a maintenance slot.
func BeforeVerifyFsck(o *Options, f func(dir string)) {
	if o.knobs == nil {
		o.knobs = &knobs{}
	}
	o.knobs.verifyFsck = f
}

// WhileAdopting makes every adoption that takes its share of the repository's
// directory call f with the repository's id before it links.
func WhileAdopting(o *Options, f func(repo string)) {
	if o.knobs == nil {
		o.knobs = &knobs{}
	}
	o.knobs.adopting = f
}

// AdoptionSyncFails makes the directory sync of every adoption's links fail
// with err, after the links are made.
func AdoptionSyncFails(o *Options, err error) {
	if o.knobs == nil {
		o.knobs = &knobs{}
	}
	o.knobs.adoptSync = func(string) error { return err }
}

// MaintainerPass runs one maintainer pass as work of the cache's own (a
// stopped cache runs none), waits for every run it started to end, and
// answers the repositories whose runs ran every step to its end.
func MaintainerPass(c *Cache) []string {
	_, wait := StartMaintainerPass(c)
	return wait()
}

// StartMaintainerPass runs one maintainer pass as work of the cache's own,
// and answers the repositories it started runs on, sorted, as soon as it has
// started them — the runs go on — with a wait that answers, once every one
// has ended, those that ran every step to its end.
func StartMaintainerPass(c *Cache) (started []string, wait func() []string) {
	if !c.enter() {
		return nil, func() []string { return nil }
	}
	runs := c.maintainerPass(context.Background())
	c.leave()
	for _, m := range runs {
		started = append(started, m.e.id)
	}
	slices.Sort(started)
	return started, func() []string {
		var ran []string
		for _, m := range runs {
			<-m.done
			if m.maintained {
				ran = append(ran, m.e.id)
			}
		}
		slices.Sort(ran)
		return ran
	}
}

// SetMaintenanceMemory sets what the maintenance runs beside the largest may
// be estimated at together. Before any pass.
func SetMaintenanceMemory(c *Cache, n int64) { c.maintMemory = n }

// MaintenanceMemory is what the maintenance runs beside the largest may be
// estimated at together.
func MaintenanceMemory(c *Cache) int64 { return c.maintMemory }

// MaintenanceEstimate is the memory a maintenance run of repoID's current
// incarnation is estimated at.
func MaintenanceEstimate(c *Cache, repoID string) int64 {
	e := lookup(c, repoID)
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.current.objects * MaintenanceBytesPerObject
}

// BeforeMaintaining makes every maintenance run call f with its own context
// before each of its steps ("multi-pack-index", then "commit-graph").
func BeforeMaintaining(o *Options, f func(ctx context.Context, step string)) {
	if o.knobs == nil {
		o.knobs = &knobs{}
	}
	o.knobs.maintaining = f
}

// MaintainerTicksFrom makes the maintainer take its ticks from ticks, in
// place of a ticker of MidxDebounce.
func MaintainerTicksFrom(o *Options, ticks <-chan time.Time) {
	if o.knobs == nil {
		o.knobs = &knobs{}
	}
	o.knobs.maintTicks = ticks
}

// MaintainerKicked reports whether a kick is waiting for the maintainer, and
// takes it: a case that answers the kicks itself sees each one once.
func MaintainerKicked(c *Cache) bool {
	select {
	case <-c.maintKicks:
		return true
	default:
		return false
	}
}

// Maintaining reports whether a maintenance run is in flight on repoID.
func Maintaining(c *Cache, repoID string) bool {
	e := lookup(c, repoID)
	if e == nil {
		return false
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.maint != nil
}

// RebuildWindow is how long after the last rebuild attempt the next is
// refused, for an incarnation n attempts in a row stand behind.
func RebuildWindow(n int) time.Duration { return rebuildWindow(n) }

// Total is what the janitor holds the cache to its budget by: every
// incarnation's files, the downloads in flight and scratch/ as last measured.
func Total(c *Cache) int64 { return c.total() }

// SetMaxBytes sets the cache's budget. Between syncs, with nothing running.
func SetMaxBytes(c *Cache, n int64) { c.maxBytes = n }

// BuffersOut is the copy buffers the node's download slots hold now, and the
// most they have held at once.
func BuffersOut(c *Cache) (now, most int64) { return c.fetches.out.Load(), c.fetches.most.Load() }

// SlotWaiters is how many downloads wait for one of the node's slots.
func SlotWaiters(c *Cache) int64 { return c.fetches.waiting.Load() }

// InHand is how many packs the node holds in hand now, and the most it has
// held at once.
func InHand(c *Cache) (now, most int64) { return c.fetches.held.Load(), c.fetches.mostHeld.Load() }

// HandWaiters is how many of the node's downloads wait for a pack in hand.
func HandWaiters(c *Cache) int64 { return c.fetches.handWaiting.Load() }

// FileSyncWith makes the cache Open builds make every downloaded file
// durable through fn in place of (*os.File).Sync.
func FileSyncWith(fn func(f *os.File) error) func(*Options) {
	return func(o *Options) {
		if o.knobs == nil {
			o.knobs = &knobs{}
		}
		o.knobs.fileSync = fn
	}
}

// StagedWith makes the cache Open builds call fn with a bundle's object once
// its round has made the bundle's files, as it hands the pack to its worker.
func StagedWith(fn func(object objstore.Key)) func(*Options) {
	return func(o *Options) {
		if o.knobs == nil {
			o.knobs = &knobs{}
		}
		o.knobs.staged = fn
	}
}

// AskingWith makes the cache Open builds call fn with a bundle's object as
// its worker is about to ask for its GET, the bundle's files made.
func AskingWith(fn func(object objstore.Key)) func(*Options) {
	return func(o *Options) {
		if o.knobs == nil {
			o.knobs = &knobs{}
		}
		o.knobs.asking = fn
	}
}

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
	if e.current == nil {
		e.mu.Unlock()
		return false
	}
	gone := c.retire(e, e.current)
	e.mu.Unlock()
	c.trash(gone)
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
