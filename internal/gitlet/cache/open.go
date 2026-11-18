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

	"github.com/crewlet/crewlet/internal/gitlet"
	"github.com/crewlet/crewlet/internal/gitlet/gitletfault"
	"github.com/crewlet/crewlet/internal/gitlet/gitproc"
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
	limits   func() gitlet.Limits
	faults   gitletfault.Injector
	log      *slog.Logger
	metrics  *metrics.Recorder
	now      func() time.Time
	knobs    *knobs

	// image is the grammar of an incarnation, built at Open from the derived
	// files' declarations (derived.go), and mw the maintainer's view of their
	// writes: read-only once built, so every goroutine reads them unlocked.
	image *image
	mw    maintWrites

	// fetchConcurrency is how many downloads one round runs at once
	// (FetchConcurrency), and fetches the node's slots, each with its copy
	// buffer, that every round's downloads share (NodeFetches): fields, so the
	// suite can narrow both before a sync (export_test.go).
	fetchConcurrency int
	fetches          *fetchSlots
	// full is the node's slots for full reads of a repository's rows against the
	// estate, shared by every round (MaxFullRounds): a field, so the suite can
	// narrow it before a sync.
	full *fullRounds
	// unobserve gives the pool's observer back (Open, Stop).
	unobserve func()
	// roundPacks is how many packs a full round leaves before it writes its own
	// multi-pack-index (MidxRoundPacks), and midxPacks how many beside the
	// maintainer's make a repository due again (MidxPacks): fields, so the suite
	// can lower them (knobs.roundPacks, knobs.midxPacks).
	roundPacks, midxPacks int
	// noLookupTable is BenchmarkMidx's arm WITHOUT the bitmap's lookup table
	// (ServeConfig), set only by the suite (export_test.go), so every node writes
	// the table. Atomic: the maintainer reads the serve configuration on its own
	// goroutine.
	noLookupTable atomic.Bool
	// tmpBytes is what tmp/ holds for the cache's own work: downloads in flight
	// and pushed packs staged by adoptions (adopt.go), until each is placed or
	// dropped.
	tmpBytes atomic.Int64
	// heldBytes is what every incarnation, current or retiring, holds from being
	// made or indexed until discarded, and heldRepos the repositories with a
	// current incarnation: RUNNING TOTALS moved with what they count (setSize,
	// newIncarnation, retire, discard), so a round's gauges cost two loads, never
	// a walk of every repository under the lock every sync takes.
	heldBytes, heldRepos atomic.Int64
	// chainedRepos is how many repositories' current incarnation serves from an
	// installed published chain (crewlet.code.cache.chained), the rest in own
	// mode: a running total moved by noteChained.
	chainedRepos atomic.Int64
	// installer is the installed chain's background installer (layers.go): its
	// queue, its share of the node's fetches and the layers maintainers adopted
	// here, run by Start.
	installer *layerInstaller
	// memoBytes is the memos' part of heldBytes, a RUNNING TOTAL moved with an
	// incarnation's memo record (setDerived, discard), which the janitor holds to
	// MemoShare (memo.go) with no walk. memoStaged is what reads have staged for
	// the memo in tmp/ and not yet placed or given back (MemoBatch), of tmpBytes:
	// the share holds both together (memoExcess), so a repository is never
	// evicted for a page in flight.
	memoBytes, memoStaged atomic.Int64
	// codeIndexBytes is the code indexes' part of heldBytes, a running total like
	// memoBytes, which the janitor holds to CodeIndexShare (codeindex.go).
	codeIndexBytes atomic.Int64
	// placement is where this node keeps code indexes (Options.Placement), nil
	// for nowhere. Under indexMu: indexWanted, the repositories a round moved the
	// refs of since the indexer last looked (indexKicks wakes it); indexBack,
	// what the budget took of the repositories this node indexes (the evicted
	// copy, the trimmed index) by bytes, which the indexer waits for room for
	// before filling or building it again (codeindex.go); indexSeen, this node's
	// place among each repository's index holders as last seen, and since when
	// (indexPlace).
	placement   *Placement
	indexMu     sync.Mutex
	indexWanted map[string]bool
	indexBack   map[string]indexTaken
	indexSeen   map[string]indexPlace
	indexKicks  chan struct{}
	// droppingBytes is the dropping views' part of heldBytes (the packs an
	// expunge's begin left dropping until its commit): a RUNNING TOTAL moved with
	// an incarnation's dropping files (setDropping, discard), reported as
	// crewlet.code.cache.dropping.bytes with no walk of the node's copies.
	droppingBytes atomic.Int64
	// memoNewest is the memo's NEWEST COHORT (memo.go), where every answer is
	// placed and every entry a read finds in an older one is moved; read without
	// a lock. memoFill is what was placed in it since it opened: past a sixteenth
	// of the share (memoCohortsPerShare) the next placement opens another.
	// memoReserved is the newest cohort the root says a process may have placed
	// in (cohortsName), which memoNewest never passes (reserveCohort). cohortMu
	// guards memoFill, memoReserved and every move of memoNewest; held across a
	// reservation's write (two syncs), it is taken after every other lock and
	// never with an entry's mu, which every memo lookup takes.
	cohortMu     sync.Mutex
	memoNewest   atomic.Uint64
	memoFill     int64
	memoReserved uint64
	// scratchBytes is what scratch/ held when the janitor last measured it: a
	// push's scratch is written by its client's receive-pack, so only the disk
	// knows.
	scratchBytes atomic.Int64
	// clock counts the heat's windows and the cache's own from Open (heat.go),
	// each structure reading it under its own lock and never behind the latest
	// slot it acted at (heatNow); heat is what the node served of each
	// repository, counted by the host; churn is the cache's own spending
	// (evictions, repairs) over the same windows, for its status.
	clock heatClock
	heat  *Heat
	churn cacheChurn
	// maint is the whole-repository work in flight against the node's memory
	// (maintenance runs, a round's indexes, Verify's checks) and its budget;
	// maintMemory is what the runs beside the largest may be charged together
	// (MaintenanceMemoryPerCore per core the memory backs), a field so the suite
	// can narrow it. A pass admits a run against both (maintain.go).
	maint       maintFlight
	maintMemory int64
	// memReserve is the memory the node keeps for everything but its background
	// work, fixed at Open from the pool's lanes (memory.go). Under memMu: memory,
	// as last read; memSource, where each maintainer pass rereads it (the pool's
	// ReadMemory, or the suite's figure); memFailing, that the last read failed
	// and said so. refusals is what the maintainer's latest walk found refused.
	memReserve int64
	memMu      sync.Mutex
	memory     gitproc.Memory
	memSource  func() (gitproc.Memory, error)
	memFailing bool
	refusals   memoryRefusals
	// mark is the code log as this process has seen it answered, and rereads
	// counts the marks made for whole reads (Cache.reread): one log under every
	// repository, so what one round learns of it reaches all.
	mark    logMark
	rereads atomic.Uint64
	// installHook, when set, is called at each stage of a pack's install
	// (the suite's, export_test.go).
	installHook func(stage, packDir string)
	// hashHook, when set, is called with its bound as a hint's pack is hashed
	// (ConfirmHint; the suite's, export_test.go).
	hashHook func(pack string, bound time.Duration)

	// releaseClaim gives back this process's hold on the directory, taken over
	// from Prepare: once, by Stop when nothing of the cache's runs there, else by
	// the last of that work to end.
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

	// lifeMu guards started, stopped and running, the cache's work in flight in
	// its directory (a janitor pass, a move into trash/), each between enter and
	// leave. Once Stop begins enter refuses, since the directory is the next
	// Open's once running is zero; decided under this lock, not raced against a
	// context, so a Stop with nothing running gives the claim back before it
	// returns. A leaf: taken under entry.mu, and nothing is taken under it.
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

// Open opens a prepared cache (package doc, "A boot starts from the disk,
// once"), running no git and reading no rows, in steps:
//  1. o is validated ([ErrOptions]); o.Layout must be one [Prepare] returned
//     ([ErrNotACache]) and opens once.
//  2. The cache holds Prepare's claim until [Cache.Stop], any failure giving
//     it back ([ErrCacheInUse] keeps a second engine off one gitlet.cache_dir),
//     and derives its memory budget from what the git pool read (memory.go).
//  3. clearBoot empties scratch/ and tmp/ into trash/.
//  4. writeHook rewrites and settles the hook.
//  5. index acts on Prepare's reading of repos/.
//  6. openMemo opens and reserves the memo's newest cohort: a root that takes
//     no write opens no cache.
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
		backfillKicks: make(chan struct{}, 1), indexKicks: make(chan struct{}, 1), placement: o.Placement,
		scrubs:    newScrubSlot(),
		installer: newLayerInstaller(FetchConcurrency),
		entries:   map[string]*entry{}, idle: make(chan struct{}),
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
	// THE BOUND ON MAPPED RUNS, from the kernel's mapping limit for this process
	// (runmaps.go). An unreadable count is taken as Linux's default, the least a
	// distribution ships, and logged: guessed high, the runs could take the
	// mappings the runtime's heap needs.
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
	trashed, recorded, err := c.index(b)
	if err != nil {
		return nil, err
	}
	if err = c.openMemo(recorded); err != nil {
		return nil, err
	}
	c.log.Info("code_cache_opened", "dir", c.dir, "repos", len(c.entries), "bytes", c.heldBytes.Load(),
		"budget", o.MaxBytes, "trashed", trashed, "memory", c.memory.Bytes, "memory_from", c.memory.From,
		"memory_reserved", c.memReserve, "maintenance_budget", c.maint.budgetNow(), "mapped_runs", c.maps.most)
	c.setGauges()
	// THE OBSERVER, last: every pool run from now on reports what it named to
	// this cache (report.go). One cache a pool, given back by Stop, so a cache
	// opened again over the same pool installs its own.
	unobserve, err := o.Git.Observe(c.observe)
	if err != nil {
		return nil, fmt.Errorf("%w: a git pool reports its runs to one cache, and this one already reports to "+
			"another — stop that cache first: %w", ErrOptions, err)
	}
	c.unobserve = unobserve
	opened = true
	return c, nil
}

// clearBoot makes sure of the fixed directories (an operator's rm of one is no
// reason to refuse a boot), then renames scratch/ and tmp/ into trash/ and
// makes them anew: what they held was the previous process's requests', all
// orphans now, and the rename is instant at any size, trash/ being the
// janitor's to empty.
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

// writeHook writes hooks/proc-receive again, since the binary's path changes
// across an upgrade, and SETTLES it (hostbox.Settle): the kernel refuses to
// exec a script any process holds open for writing, as every fork during the
// write does until it execs, so the wait is Open's, not the first push's.
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

// index acts on Prepare's reading of repos/: every entry that is not a kept
// incarnation moves into trash/ and is logged, and every kept one is indexed
// from its state, or without one when it was missing or undecodable. It
// answers how many entries it trashed and the newest memo cohort any kept
// state records (0 for none), which openMemo opens past. A failed move fails
// the boot: a node serving from a half-cleared directory it cannot rename
// within is worse than one that says so.
func (c *Cache) index(b *boot) (int, uint64, error) {
	repos := filepath.Join(c.dir, reposDir)
	trashed := 0
	var newest uint64
	for _, a := range b.aside {
		path := filepath.Join(repos, a.name)
		if err := toTrash(c.dir, path, bootLabel(a.name)); err != nil {
			return trashed, 0, fmt.Errorf("cache: moving %s aside (%s): %w", path, a.why, err)
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
		// The memo's record as the state holds it, which the janitor writes again
		// once it moves (Cache.stateMemos), and its bytes in the running total.
		memo := inc.derived[DerivedMemo]
		c.memoBytes.Add(memo.Bytes)
		c.codeIndexBytes.Add(inc.derived[DerivedCodeIndex].Bytes)
		if cs := memo.Cohorts; len(cs) > 0 {
			newest = max(newest, cs[len(cs)-1].N)
		}
		c.heldRepos.Add(1)
		e := newEntry(k.id)
		e.current = inc
		c.entries[k.id] = e
	}
	return trashed, newest, nil
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

// Start starts the cache's background work under ctx: the janitor, the
// maintainer, the scrubber, the backfill of its indexes of pushes, the
// installer of published layers (layers.go) and the code indexer
// (codeindex.go), each ending with ctx or [Cache.Stop]. A second call, or one
// after Stop, does nothing. An unstarted cache still opens, syncs, pins,
// folds every delta's pushes into its indexes and installs a full round's
// published chain; only nothing is emptied from trash/, evicted, maintained,
// scrubbed, backfilled or installed in the background.
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
	go c.indexer(ctx)
}

// isStopped reports whether Stop has begun.
func (c *Cache) isStopped() bool {
	c.lifeMu.Lock()
	defer c.lifeMu.Unlock()
	return c.stopped
}

// Stop marks the cache stopped, so nothing of it starts in the directory
// again, and ends its lifetime, ending what runs under it. The claim on the
// directory goes back once nothing of the cache's runs there: before Stop
// returns when nothing does, whatever ctx says (decided under lifeMu, never
// raced against a context); otherwise Stop waits, bounded by ctx, and a wait
// cut short leaves the claim held until that work ends (logged
// code_cache_stop_unfinished), so no other process opens a directory this one
// still writes in. Pins still held keep their directories for the next Open
// to clear: a release after Stop moves nothing. Safe to call more than once;
// each call waits bounded by its own ctx.
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
