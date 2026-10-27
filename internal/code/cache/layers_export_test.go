package cache

import (
	"slices"
	"time"

	"github.com/crewlet/crewlet/internal/code"
)

// The suite's knobs into the installed published chain (layers.go): what a
// copy holds of it and the rows' account of it, the installer's queue and its
// passes run synchronously, and the one cut both chains share.

// Installed is the published chain repoID's current incarnation holds, root
// first, each layer with the row it was installed from; nil for none.
func Installed(c *Cache, repoID string) []code.LayerRow {
	e := lookup(c, repoID)
	e.mu.Lock()
	defer e.mu.Unlock()
	return slices.Clone(e.current.installed)
}

// ChainOf is the rows' published chain as repoID's current incarnation last
// read it (incarnation.chain), and whether a round of this process read it.
func ChainOf(c *Cache, repoID string) ([]code.LayerRow, bool) {
	e := lookup(c, repoID)
	e.mu.Lock()
	defer e.mu.Unlock()
	return slices.Clone(e.current.chain), e.current.chainRead
}

// Refused is the layers repoID's current incarnation refuses while the rows
// name them, by layer id, with the outcome that refused each.
func Refused(c *Cache, repoID string) map[string]string {
	e := lookup(c, repoID)
	e.mu.Lock()
	defer e.mu.Unlock()
	out := map[string]string{}
	for id, r := range e.current.refused {
		out[id] = string(r.outcome)
	}
	return out
}

// ChainedRepos is the running total crewlet.code.cache.chained reports.
func ChainedRepos(c *Cache) int64 { return c.chainedRepos.Load() }

// InstallQueued is the repositories the installer holds queued: the due in
// order, then the waiting by when each one's wait ends.
func InstallQueued(c *Cache) []string {
	in := c.installer
	in.mu.Lock()
	defer in.mu.Unlock()
	var out []string
	for _, e := range in.due.entries[in.due.head:] {
		out = append(out, e.id)
	}
	waiting := slices.Clone(in.waiting)
	slices.SortStableFunc(waiting, func(a, b *installEntry) int { return a.wait.retryAt.Compare(b.wait.retryAt) })
	for _, e := range waiting {
		out = append(out, e.id)
	}
	return out
}

// The installer's backoff: its first wait after a try that failed for a
// reason that may pass, and the most it waits — how long a stale layer may
// stand in for its replacement.
const (
	InstallRetryBase = installRetryBase
	InstallRetryCap  = installRetryCap
)

// InstallWait is when repoID's next try is due by the installer's backoff,
// and the tries in a row that failed; the zero time where it waits for none.
func InstallWait(c *Cache, repoID string) (time.Time, int) {
	in := c.installer
	in.mu.Lock()
	defer in.mu.Unlock()
	if e := in.repos[repoID]; e != nil {
		return e.wait.retryAt, e.wait.failures
	}
	return time.Time{}, 0
}

// KickInstall queues repoID for the installer as a round of it does; fresh
// as a round that served its chain does, which ends its wait.
func KickInstall(c *Cache, repoID string, fresh bool) { c.kickInstall(repoID, fresh) }

// HoldInstallUntil queues repoID, which the installer does not hold yet,
// behind a wait that ends at at, as a try that failed for a reason that may
// pass leaves it.
func HoldInstallUntil(c *Cache, repoID string, at time.Time) {
	in := c.installer
	in.mu.Lock()
	defer in.mu.Unlock()
	e := &installEntry{id: repoID, index: -1, wait: installWait{failures: 1, retryAt: at}}
	in.repos[repoID] = e
	in.hold(e, c.now())
}

// InstallHandled is how many queued repositories the installer's passes have
// taken in hand so far: what a pass's cost is held to.
func InstallHandled(c *Cache) uint64 {
	in := c.installer
	in.mu.Lock()
	defer in.mu.Unlock()
	return in.handled
}

// InstallPass runs one pass of the installer synchronously, as work of the
// cache's own, and waits for every task it started: the repositories it took
// up, in queue order. A stopped cache runs none.
func InstallPass(c *Cache) []string {
	if !c.enter() {
		return nil
	}
	_, started, tasks := c.installPass(c.life)
	c.leave()
	tasks.Wait()
	return started
}

// ChainLinesWith makes every multi-pack-index chain file c writes list what f
// makes of the ids installed: a chain over the wrong base, reversed.
func ChainLinesWith(f func(ids []string) []string) func(*Options) {
	return func(o *Options) {
		if o.knobs == nil {
			o.knobs = &knobs{}
		}
		o.knobs.chainLines = f
	}
}

// InstallWakesWith makes the installer wait on f's channel, told when its
// earliest wait ends, in place of a timer.
func InstallWakesWith(o *Options, f func(at time.Time) <-chan time.Time) {
	if o.knobs == nil {
		o.knobs = &knobs{}
	}
	o.knobs.installWake = f
}

// InstallReadingWith makes the installer tell f each time it takes a share of
// the node's read lane (held true) and gives it back (held false): its
// canaries' where canary says so, else its checks'.
func InstallReadingWith(f func(canary, held bool)) func(*Options) {
	return func(o *Options) {
		if o.knobs == nil {
			o.knobs = &knobs{}
		}
		o.knobs.installReading = f
	}
}

// InstallSettlingWith makes every installer's task call f with its repository
// once its try has ended, before the try is settled.
func InstallSettlingWith(f func(id string)) func(*Options) {
	return func(o *Options) {
		if o.knobs == nil {
			o.knobs = &knobs{}
		}
		o.knobs.installSettling = f
	}
}

// PrefixOf is how many layers of installed the cut a change ev makes keeps
// (prefixOf): packs the checksums it takes out, want the rows' chain for
// EventLayersMoved, failing the layers whose files failed their check.
func PrefixOf(installed []code.LayerRow, ev Event, packs []string, want []code.LayerRow, failing []string) int {
	d := eventDetail{packs: map[string]bool{}, want: want, failing: map[string]bool{}}
	for _, cs := range packs {
		d.packs[cs] = true
	}
	for _, id := range failing {
		d.failing[id] = true
	}
	return prefixOf(installed, ev, d)
}

// TipGoes reports whether a change ev the installed chain answers by keeping
// k of installed's layers takes the node's tip over it (tipGoes).
func TipGoes(installed []code.LayerRow, k int, ev Event) bool { return tipGoes(installed, k, ev) }

// PlanSteps is what the maintainer would plan for repoID's current
// incarnation now on an unbounded budget, due or not: whether an index (of
// either kind), own mode's graph, and the node's tip over an installed chain.
func PlanSteps(c *Cache, repoID string) (index, graph, tip bool) {
	e := lookup(c, repoID)
	e.mu.Lock()
	defer e.mu.Unlock()
	p, _, _ := c.plan(e.current, 1<<62, true)
	return p.index != 0, p.graph, p.tip
}

// Tip is the node's tip commit-graph layer repoID's current incarnation
// holds (incarnation.tip), "" for none, and whether its tail has moved since
// the tip — or the write that found the tail held no commit — was written
// over it (tipWanted).
func Tip(c *Cache, repoID string) (string, bool) {
	e := lookup(c, repoID)
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.current.tip.id, tipWanted(e.current)
}

// TailPacks is how many packs repoID's current incarnation's tail holds, as
// it counts them (incarnation.tail).
func TailPacks(c *Cache, repoID string) int {
	e := lookup(c, repoID)
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.current.tail.packs
}

// TipCharge is what the node's tip over repoID's current incarnation's tail
// is admitted at (chargeOver): TipGraphBytesPerObject an object of the tail,
// and room for a clone of the whole repository.
func TipCharge(c *Cache, repoID string) int64 {
	e := lookup(c, repoID)
	e.mu.Lock()
	defer e.mu.Unlock()
	return chargeOver(e.current.tail.objects, TipGraphBytesPerObject, e.current.objects)
}

// WantsRun reports whether the maintainer wants a run of repoID's current
// incarnation for what it holds (wantsRun).
func WantsRun(c *Cache, repoID string) bool {
	e := lookup(c, repoID)
	e.mu.Lock()
	defer e.mu.Unlock()
	return c.wantsRun(e)
}

// InstallerShare is the installer's parts in flight bound and its tasks.
func InstallerShare(c *Cache) int { return c.installer.tasks }

// StagedLayers is the layers staged on c for a placement that has not taken
// them yet — maintainers' adoptions, and the installer's own fetches a moved
// placement held — by repository.
func StagedLayers(c *Cache) map[string][]string {
	in := c.installer
	in.mu.Lock()
	defer in.mu.Unlock()
	out := map[string][]string{}
	for repo, layers := range in.staged {
		for id := range layers {
			out[repo] = append(out[repo], id)
		}
		slices.Sort(out[repo])
	}
	return out
}

// ExpireStagedLayers runs the janitor's discard of the staged layers no
// placement took within objstore.RecordWithin, on c's clock.
func ExpireStagedLayers(c *Cache) { c.expireStagedLayers() }
