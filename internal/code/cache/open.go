package cache

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/crewlet/crewlet/internal/code"
	"github.com/crewlet/crewlet/internal/code/codefault"
	"github.com/crewlet/crewlet/internal/code/gitproc"
	"github.com/crewlet/crewlet/internal/hostbox"
	"github.com/crewlet/crewlet/internal/statelog/metrics"
)

// Cache is one node's git cache: the repositories it has been asked to
// serve, each a bare repository under repos/.
type Cache struct {
	// dir is the root, as Prepare resolved it.
	dir      string
	maxBytes int64
	git      *gitproc.Pool
	objects  Objects
	fetch    Fetcher
	fleet    Fleet
	limits   func() code.Limits
	faults   codefault.Injector
	log      *slog.Logger
	metrics  *metrics.Recorder
	now      func() time.Time
	knobs    *knobs

	// image is the grammar of an incarnation, built at Open from the
	// declarations of the derived files (derived.go), and mw the
	// maintainer's view of their writes: read-only once built, and read by
	// every goroutine of the cache.
	image *image
	mw    maintWrites

	// fetchConcurrency is how many downloads one round runs at once
	// (FetchConcurrency), and fetches the node's slots every round's
	// downloads share (NodeFetches), each with its copy buffer: fields, so
	// the suite can narrow both before a sync (export_test.go).
	fetchConcurrency int
	fetches          *fetchSlots
	// full is the node's slots for full reads of a repository's rows
	// against the estate, every round's on the node sharing them
	// (MaxFullRounds): a field, so the suite can narrow it before a sync.
	full *fullRounds
	// unobserve gives the pool's observer back (Open, Stop).
	unobserve func()
	// roundPacks is how many packs a full round leaves before it writes its
	// own multi-pack-index (MidxRoundPacks), and midxPacks how many beside
	// the maintainer's make a repository due again (MidxPacks): fields, so
	// the suite can lower them rather than push dozens of packs a case
	// (knobs.roundPacks, knobs.midxPacks).
	roundPacks, midxPacks int
	// noLookupTable is BenchmarkMidx's arm WITHOUT the bitmap's lookup
	// table (ServeConfig), the figure the table is weighed against — set
	// only by the suite (export_test.go), so its zero value, the table
	// written, is every node's. An atomic, since the maintainer reads the
	// serve configuration on its own goroutine.
	noLookupTable atomic.Bool
	// tmpBytes is what tmp/ holds for the cache's own work: downloads in
	// flight, until each file is renamed into place or removed, and the
	// pushed packs adoptions staged for a round or a listing to install
	// (adopt.go), until each is installed or dropped.
	tmpBytes atomic.Int64
	// heldBytes is what every incarnation holds — current and retiring, from
	// the moment it is made or indexed until it is discarded — and
	// heldRepos the repositories with a current incarnation: RUNNING TOTALS,
	// moved where what they count moves (setSize, newIncarnation, retire,
	// discard), so the gauges a round sets cost two loads, never a walk of
	// every repository on the node under the lock every sync takes.
	heldBytes, heldRepos atomic.Int64
	// chainedRepos is how many repositories' current incarnation serves from
	// an installed published chain (crewlet.code.cache.chained): a running
	// total moved by noteChained, the rest serving in own mode.
	chainedRepos atomic.Int64
	// installer is the installed chain's background installer (layers.go):
	// its queue, its share of the node's fetches and the layers maintainers
	// adopted on this node, run by Start.
	installer *layerInstaller
	// memoBytes is what every incarnation's memo holds, of heldBytes — a
	// RUNNING TOTAL moved where an incarnation's memo record moves
	// (setDerived, discard), which the janitor holds to the memo's share of
	// the budget (MemoShare, memo.go) with no walk of the node's copies.
	// memoStaged is what every read's run has staged for the memo in tmp/
	// and not yet placed or given back (MemoBatch), of tmpBytes: the memo's
	// too, so the share is held to both together (memoExcess) and a
	// repository is never evicted for a page in flight.
	memoBytes, memoStaged atomic.Int64
	// droppingBytes is what every incarnation's dropping view holds, of
	// heldBytes — the pack files an expunge's begin left dropping, which
	// leave with its commit — a RUNNING TOTAL moved where an incarnation's
	// dropping files move (setDropping, discard), and reported as it stands
	// (crewlet.code.cache.dropping.bytes): what an open expunge keeps on the
	// node's disk beside the live packs, with no walk of the node's copies.
	droppingBytes atomic.Int64
	// memoNewest is the node's NEWEST COHORT of the memo (memo.go) — where
	// every answer is placed and every entry a read finds in an older one is
	// moved — read without a lock; memoFill is what has been placed in it
	// since it was opened, past a sixteenth of the share of which
	// (memoCohortsPerShare) the next placement opens another; cohortMu guards
	// memoFill and every move of memoNewest, and is taken after every other
	// lock.
	cohortMu   sync.Mutex
	memoNewest atomic.Uint64
	memoFill   int64
	// scratchBytes is what scratch/ held when the janitor last measured it:
	// a push's scratch is written by its client's receive-pack, so only the
	// disk knows.
	scratchBytes atomic.Int64
	// clock is where the heat's windows and the cache's own are counted
	// from, the instant Open made it (heat.go), each structure reading it
	// under its own lock and never behind the latest slot it acted at
	// (heatNow); heat is what the node served of each repository, which the
	// host counts into; and churn is what the cache spent on its own account
	// — evictions and repairs — over the same windows, for its status.
	clock heatClock
	heat  *Heat
	churn cacheChurn
	// maint is the whole-repository work in flight against the node's
	// memory — the maintenance runs, the round's indexes, Verify's checks —
	// and the budget it is held to, and maintMemory what the runs beside the
	// largest may be charged at together (MaintenanceMemoryPerCore for each
	// core the node's memory backs): a pass admits a run against both
	// (maintain.go). maintMemory a field, so the suite can narrow it
	// (export_test.go).
	maint       maintFlight
	maintMemory int64
	// memReserve is what the node keeps of its memory for everything but its
	// background work, fixed at Open from the pool's lanes (memory.go).
	// Under memMu: memory, the node's memory as last read; memSource, where
	// each maintainer pass reads it again (the pool's ReadMemory, or the
	// suite's figure); memFailing, that the last read failed and said so.
	// refusals is what the maintainer's latest walk found refused for it.
	memReserve int64
	memMu      sync.Mutex
	memory     gitproc.Memory
	memSource  func() (gitproc.Memory, error)
	memFailing bool
	refusals   memoryRefusals
	// mark is the code log as this process has seen it answered, and
	// rereads counts the marks made for whole reads (Cache.reread): one log
	// under every repository, so what one round learns of it reaches all.
	mark    logMark
	rereads atomic.Uint64
	// installHook, when set, is called at each stage of a pack's install
	// (the suite's, export_test.go).
	installHook func(stage, packDir string)
	// hashHook, when set, is called with its bound as a hint's pack is hashed
	// (ConfirmHint; the suite's, export_test.go).
	hashHook func(pack string, bound time.Duration)

	// releaseClaim gives back this process's hold on the directory, taken
	// over from Prepare: once, by Stop when nothing of the cache's is
	// running in the directory, else by the last of that work to end.
	releaseClaim func()

	// life is the cache's own lifetime, never a caller's: what the cache
	// starts runs under it, so a caller that hangs up never cancels work
	// others wait on. Stop ends it.
	life   context.Context
	cancel context.CancelFunc

	// kicks wakes the janitor at once, maintKicks the maintainer, scrubKicks
	// the scrubber and backfillKicks the backfill: buffered one, sent without
	// blocking, so any number of kicks before a pass are one pass.
	kicks, maintKicks, scrubKicks, backfillKicks chan struct{}
	// scrubs is the node's one scrub, given by evidence (scrub.go).
	scrubs *scrubSlot
	// maps is the node's runs of its indexes of pushes mapped into memory,
	// held to a bound under the process's count of mappings (runmaps.go).
	maps *runMaps
	// sched is the backfill's schedule: which copies have a step due, kept
	// as their work changes (schedule.go).
	sched schedule
	// repairers mints the name each reader is handed under (pin;
	// Synced.Repairer): one counter for the cache, since the pool reports to
	// one observer and a name must say which reader of which copy it is.
	repairers atomic.Uint64

	// mu guards entries. Lock order: Cache.mu, then entry.mu.
	mu      sync.Mutex
	entries map[string]*entry

	// lifeMu guards the lifecycle: started, stopped, and running — the
	// cache's work in flight in its directory (a janitor pass, a move into
	// trash/), each registered by enter and ended by leave. Once Stop has
	// begun, enter refuses, because the directory is the next Open's from
	// the moment running is zero; and that is decided under this lock, not
	// raced against the caller's context, so a Stop with nothing running
	// gives the claim back before it returns whatever its context says. A
	// leaf: taken under entry.mu, and nothing is taken under it.
	lifeMu  sync.Mutex
	started bool
	stopped bool
	running int
	// idle is closed by the last leave after Stop, once the claim is back.
	idle chan struct{}
}

// hookProbe is the argument hooks/proc-receive answers by exiting at once.
// git runs proc-receive with no arguments, so the probe never fires for git.
const hookProbe = "--crewlet-probe"

// Open opens a prepared cache: takes over the claim [Prepare] took, derives its
// memory budget, clears what the previous process left in flight, rewrites
// the hook (and waits until it can be exec'd) and indexes every repository
// Prepare found. It runs no git, reads no rows, and reads nothing of repos/
// that Prepare already read.
//
//  1. o is validated ([ErrOptions]); o.Layout must be one Prepare returned
//     ([ErrNotACache] otherwise), and is opened once.
//  2. The claim passes from the Layout to the cache, which holds it until
//     [Cache.Stop]: two engines pointed at one git.cache_dir would empty
//     each other's tmp/ and scratch/ at boot and race on every repository's
//     files, so the second is refused at its Prepare ([ErrCacheInUse]).
//     Any failure from here gives the claim back. What its background work
//     may hold of the node's memory is derived from the memory its git pool
//     read (memory.go): the pool refuses a node whose memory cannot be read,
//     so a cache is never opened on a guessed budget.
//  3. scratch/ and tmp/ are each renamed into trash/ and made again empty —
//     an instant at any size. What they held belonged to requests of the
//     process that made it, so every one of them is an orphan now; trash/
//     is the janitor's, so a boot never waits on a large removal.
//  4. hooks/proc-receive is written again — the binary's path changes
//     across an upgrade — and SETTLED (hostbox.Settle): a script just
//     written is refused by the kernel while any process holds it open for
//     writing, which every fork during the write does until it execs, and
//     the first push's receive-pack execs it, so the wait is this call's
//     rather than a push's.
//  5. Prepare's reading of repos/ is acted on: every entry it found that is
//     not a repository's kept incarnation is moved into trash/ and logged —
//     a lower incarnation an interrupted rebuild left (the HIGHEST holds the
//     newest downloads), a stray, an incarnation whose state is another
//     build's format — and every kept incarnation is indexed from its state,
//     or with none when its state was missing or undecodable.
func Open(ctx context.Context, o Options) (*Cache, error) {
	if err := o.validate(); err != nil {
		return nil, err
	}
	b := o.Layout.boot
	if !b.taken.CompareAndSwap(false, true) {
		return nil, fmt.Errorf("%w: the Layout of %s was already opened or released — "+
			"run cache.Prepare on the directory again", ErrOptions, b.dir)
	}
	c := &Cache{
		dir: b.dir, maxBytes: o.MaxBytes, git: o.Git, objects: o.Objects, fetch: o.Fetch, fleet: o.Fleet,
		limits: o.Limits, faults: o.Faults, log: o.Logger, metrics: o.Metrics, now: o.Now, knobs: o.knobs,
		fetchConcurrency: FetchConcurrency, fetches: newFetchSlots(NodeFetches), roundPacks: MidxRoundPacks,
		full:         newFullRounds(MaxFullRounds, o.Metrics),
		midxPacks:    MidxPacks,
		releaseClaim: sync.OnceFunc(b.claim.Release),
		kicks:        make(chan struct{}, 1), maintKicks: make(chan struct{}, 1), scrubKicks: make(chan struct{}, 1),
		backfillKicks: make(chan struct{}, 1),
		scrubs:        newScrubSlot(),
		installer:     newLayerInstaller(FetchConcurrency),
		entries:       map[string]*entry{}, idle: make(chan struct{}),
	}
	if o.knobs != nil && o.knobs.roundPacks > 0 {
		c.roundPacks = o.knobs.roundPacks
	}
	if o.knobs != nil && o.knobs.midxPacks > 0 {
		c.midxPacks = o.knobs.midxPacks
	}
	if c.log == nil {
		c.log = log
	}
	// THE BOUND ON MAPPED RUNS, from the count of mappings the kernel lets
	// this process hold (runmaps.go). A count that cannot be read is
	// Linux's default, the least a distribution ships, and said so: guessed
	// high, the bound would let the runs take the mappings the runtime's own
	// heap needs.
	mapCount, err := readMapCount()
	if err != nil {
		c.log.Warn("code_cache_map_count_unread", "error", err, "assumed", DefaultMapCount)
		mapCount = DefaultMapCount
	}
	c.maps = newRunMaps(mappedRunsOf(mapCount))
	if o.knobs != nil && o.knobs.mappedRuns > 0 {
		c.maps = newRunMaps(o.knobs.mappedRuns)
	}
	c.clock = newHeatClock(c.now)
	c.heat = newHeat(c.clock)
	c.churn.now = heatNow{clock: c.clock}
	// Under the boot's values (a trace, the logger's attributes), never
	// its cancellation: a boot's context ends with the boot.
	c.life, c.cancel = context.WithCancel(context.WithoutCancel(ctx))
	opened := false
	defer func() {
		if !opened {
			c.cancel()
			c.releaseClaim()
		}
	}()

	// The derived files, declared (derived.go): a build whose declarations
	// leave anything unsaid opens no cache, rather than one that would
	// sweep a file it writes or keep one nothing tracks.
	decls := DerivedFiles()
	if o.knobs != nil && o.knobs.derived != nil {
		decls = o.knobs.derived
	}
	im, err := newImage(decls)
	if err != nil {
		return nil, fmt.Errorf("%w: the derived files this build declares: %w", ErrOptions, err)
	}
	if c.mw, err = newMaintWrites(im); err != nil {
		return nil, fmt.Errorf("%w: the derived files this build declares: %w", ErrOptions, err)
	}
	if o.knobs != nil {
		im.stating = o.knobs.stating
	}
	c.image = im

	c.initMemory(o.Git)
	if err = c.clearBoot(); err != nil {
		return nil, err
	}
	if err = c.writeHook(ctx, o.HookCommand); err != nil {
		return nil, err
	}
	trashed, err := c.index(b)
	if err != nil {
		return nil, err
	}
	c.log.Info("code_cache_opened", "dir", c.dir, "repos", len(c.entries), "bytes", c.heldBytes.Load(),
		"budget", o.MaxBytes, "trashed", trashed, "memory", c.memory.Bytes, "memory_from", c.memory.From,
		"memory_reserved", c.memReserve, "maintenance_budget", c.maint.budgetNow(), "mapped_runs", c.maps.most)
	c.setGauges()
	// THE OBSERVER, last: every run of the pool's from now on reports what
	// it named to this cache (report.go). One cache a pool, and given back
	// by Stop, so a cache opened again over the same pool installs its own.
	unobserve, err := o.Git.Observe(c.observe)
	if err != nil {
		return nil, fmt.Errorf("%w: a git pool reports its runs to one cache, and this one already reports to "+
			"another — stop that cache first: %w", ErrOptions, err)
	}
	c.unobserve = unobserve
	opened = true
	return c, nil
}

// clearBoot is Open's step 3, after the fixed directories are made sure of
// (an operator's rm of one is not a reason to refuse a boot).
func (c *Cache) clearBoot() error {
	for _, name := range []string{reposDir, trashDir, hooksDir} {
		if err := os.MkdirAll(filepath.Join(c.dir, name), hostbox.DirMode); err != nil {
			return fmt.Errorf("cache: making %s: %w", filepath.Join(c.dir, name), err)
		}
	}
	for _, name := range []string{scratchDir, tmpDir} {
		path := filepath.Join(c.dir, name)
		if err := toTrash(c.dir, path, bootLabel(name)); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("cache: moving %s aside: %w", path, err)
		}
		if err := os.Mkdir(path, hostbox.DirMode); err != nil {
			return fmt.Errorf("cache: making %s: %w", path, err)
		}
	}
	c.kick()
	return nil
}

// writeHook is Open's step 4.
func (c *Cache) writeHook(ctx context.Context, argv []string) error {
	path := filepath.Join(c.Hooks(), hookName)
	staged, err := c.stage(0o500, func(f *os.File) error {
		if _, err := f.WriteString(hookScript(argv)); err != nil {
			return err
		}
		if c.knobs != nil && c.knobs.hookStaged != nil {
			c.knobs.hookStaged(f)
		}
		return nil
	})
	if err != nil {
		return err
	}
	if err = c.place(staged, path); err != nil {
		return err
	}
	if err = syncDir(c.Hooks()); err != nil {
		return err
	}
	if err = hostbox.Settle(ctx, path, hookProbe); err != nil {
		return fmt.Errorf("cache: the hook %s: %w", path, err)
	}
	return nil
}

// hookScript is the text of hooks/proc-receive: answer the probe, else exec
// argv, every argument single-quoted so the shell reads it as one word
// whatever it holds.
func hookScript(argv []string) string {
	var b strings.Builder
	b.WriteString("#!/bin/sh\n[ \"${1-}\" = " + hookProbe + " ] && exit 0\nexec")
	for _, arg := range argv {
		b.WriteString(" '" + strings.ReplaceAll(arg, "'", `'\''`) + "'")
	}
	b.WriteString("\n")
	return b.String()
}

// Hooks is the hooks directory every push's receive-pack runs with
// (gitproc.Cmd.Hooks).
func (c *Cache) Hooks() string { return filepath.Join(c.dir, hooksDir) }

// index is Open's step 5: it acts on Prepare's reading and answers how many
// entries of repos/ it moved into trash/. A move that fails fails the boot:
// a cache directory the engine cannot rename within is broken, and a node
// that serves from it half-cleared is worse than one that says so.
func (c *Cache) index(b *boot) (int, error) {
	repos := filepath.Join(c.dir, reposDir)
	trashed := 0
	var newest uint64
	defer func() { c.memoNewest.Store(newest + 1) }()
	for _, a := range b.aside {
		path := filepath.Join(repos, a.name)
		if err := toTrash(c.dir, path, bootLabel(a.name)); err != nil {
			return trashed, fmt.Errorf("cache: moving %s aside (%s): %w", path, a.why, err)
		}
		trashed++
		c.log.Warn("code_cache_trashed", "path", path, "why", a.why)
	}
	for _, k := range b.keep {
		inc := &incarnation{n: k.n, dir: filepath.Join(repos, k.name), lastUsed: k.lastUsed, stamped: k.lastUsed,
			inherited: true}
		if k.stateErr != nil {
			c.log.Warn("code_cache_state_unreadable", "repo", k.id, "error", k.stateErr)
		}
		if k.state != nil {
			inc.fromState(k.state)
		} else {
			inc.diskBytes = k.measured
		}
		if inc.lastUsed.IsZero() {
			inc.lastUsed = c.now()
		}
		c.heldBytes.Add(inc.diskBytes + inc.metaBytes)
		c.droppingBytes.Add(inc.droppingBytes)
		// The memo's record as the state holds it — what the janitor writes
		// again once it moves (Cache.stateMemos) — and its bytes in the
		// memo's running total; the newest cohort is opened past every one
		// recorded, so what a previous process left is older than anything
		// placed or read from here on.
		memo := inc.derived[DerivedMemo]
		c.memoBytes.Add(memo.Bytes)
		if cs := memo.Cohorts; len(cs) > 0 {
			newest = max(newest, cs[len(cs)-1].N)
		}
		c.heldRepos.Add(1)
		e := newEntry(k.id)
		e.current = inc
		c.entries[k.id] = e
	}
	return trashed, nil
}

// enter registers work the cache is about to do in its directory, and
// answers false once Stop has begun: the work is then not done, because the
// directory may already be the next Open's. Every enter that answers true is
// ended by one leave.
func (c *Cache) enter() bool {
	c.lifeMu.Lock()
	defer c.lifeMu.Unlock()
	if c.stopped {
		return false
	}
	c.running++
	return true
}

// leave ends work enter registered. The last to leave after Stop gives the
// claim back, then wakes a Stop still waiting for it.
func (c *Cache) leave() {
	c.lifeMu.Lock()
	c.running--
	last := c.stopped && c.running == 0
	c.lifeMu.Unlock()
	if last {
		c.releaseClaim()
		close(c.idle)
	}
}

// Start starts the cache's background work — the janitor, the maintainer, the
// scrubber, the backfill of its indexes of pushes and the installer of
// published layers (layers.go) — under ctx: each ends with ctx or with
// [Cache.Stop], whichever comes first. A second call, or a call after Stop,
// does nothing. A cache that is never started still opens, syncs, pins,
// folds every delta's pushes into its indexes and installs a full round's
// published chain as it answers; nothing is removed from trash/, evicted,
// maintained, scrubbed, backfilled or installed behind the serving until it
// is.
func (c *Cache) Start(ctx context.Context) {
	c.lifeMu.Lock()
	defer c.lifeMu.Unlock()
	if c.started || c.stopped {
		return
	}
	c.started = true
	go c.janitor(ctx)
	go c.maintainer(ctx)
	go c.scrubber(ctx)
	go c.backfiller(ctx)
	go c.runInstaller(ctx)
}

// isStopped reports whether Stop has begun.
func (c *Cache) isStopped() bool {
	c.lifeMu.Lock()
	defer c.lifeMu.Unlock()
	return c.stopped
}

// Stop ends the cache: it marks it stopped — nothing of the cache's starts
// in the directory again — and ends its lifetime, so what runs under it
// ends with it: a round's downloads, a maintenance run's git. The claim on
// the directory goes back the moment nothing of the cache's is running in
// it: at once, before Stop returns, when nothing is (whatever ctx says —
// that is decided under the cache's own lock, never raced against a context
// that may already have ended); otherwise Stop waits for it, bounded by
// ctx, and a wait ctx cuts short leaves the claim held until the last of
// that work has ended (logged code_cache_stop_unfinished), so no other
// process can open the directory while something of this one is still
// writing in it. Pins still held keep their directories until the next
// Open, which needs nothing from them: a release after Stop moves nothing.
// Safe to call more than once; a second call waits again, bounded by its
// own ctx.
func (c *Cache) Stop(ctx context.Context) {
	// The pool goes back last, whatever the wait: a run in flight past the
	// stop reports to nobody (observe), and another cache over the same pool
	// may install its own observer.
	defer c.unobserve()
	c.lifeMu.Lock()
	c.stopped = true
	idle := c.running == 0
	c.lifeMu.Unlock()
	c.cancel()
	if idle {
		c.releaseClaim()
		return
	}
	select {
	case <-c.idle:
		return
	default:
	}
	select {
	case <-c.idle:
	case <-ctx.Done():
		c.log.Warn("code_cache_stop_unfinished", "dir", c.dir, "error", context.Cause(ctx))
	}
}
