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
	limits   func() code.Limits
	faults   codefault.Injector
	log      *slog.Logger
	metrics  *metrics.Recorder
	now      func() time.Time
	knobs    *knobs

	// fetchConcurrency is how many downloads one round runs at once
	// (FetchConcurrency), and fetches the node's slots every round's
	// downloads share (NodeFetches), each with its copy buffer: fields, so
	// the suite can narrow both before a sync (export_test.go).
	fetchConcurrency int
	fetches          *fetchSlots
	// tmpBytes is what downloads in flight hold in tmp/, until each file
	// is renamed into place or removed.
	tmpBytes atomic.Int64
	// heldBytes is what every incarnation holds — current and retiring, from
	// the moment it is made or indexed until it is discarded — and
	// heldRepos the repositories with a current incarnation: RUNNING TOTALS,
	// moved where what they count moves (setSize, newIncarnation, retire,
	// discard), so the gauges a round sets cost two loads, never a walk of
	// every repository on the node under the lock every sync takes.
	heldBytes, heldRepos atomic.Int64
	// mark is the code log as this process has seen it answered, and
	// rereads counts the marks made for whole reads (Cache.reread): one log
	// under every repository, so what one round learns of it reaches all.
	mark    logMark
	rereads atomic.Uint64
	// installHook, when set, is called at each stage of a pack's install
	// (the suite's, export_test.go).
	installHook func(stage, packDir string)

	// releaseClaim gives back this process's hold on the directory, taken
	// over from Prepare: once, by Stop when nothing of the cache's is
	// running in the directory, else by the last of that work to end.
	releaseClaim func()

	// life is the cache's own lifetime, never a caller's: what the cache
	// starts runs under it, so a caller that hangs up never cancels work
	// others wait on. Stop ends it.
	life   context.Context
	cancel context.CancelFunc

	// kicks wakes the janitor at once: buffered one, sent without
	// blocking, so any number of kicks before a pass are one pass.
	kicks chan struct{}

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

// Open opens a prepared cache: takes over the claim [Prepare] took, clears
// what the previous process left in flight, rewrites the hook (and waits
// until it can be exec'd) and indexes every repository Prepare found. It runs
// no git, reads no rows, and reads nothing of repos/ that Prepare already
// read.
//
//  1. o is validated ([ErrOptions]); o.Layout must be one Prepare returned
//     ([ErrNotACache] otherwise), and is opened once.
//  2. The claim passes from the Layout to the cache, which holds it until
//     [Cache.Stop]: two engines pointed at one git.cache_dir would empty
//     each other's tmp/ and scratch/ at boot and race on every repository's
//     files, so the second is refused at its Prepare ([ErrCacheInUse]).
//     Any failure from here gives the claim back.
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
		dir: b.dir, maxBytes: o.MaxBytes, git: o.Git, objects: o.Objects, fetch: o.Fetch, limits: o.Limits,
		faults: o.Faults, log: o.Logger, metrics: o.Metrics, now: o.Now, knobs: o.knobs,
		fetchConcurrency: FetchConcurrency, fetches: newFetchSlots(NodeFetches),
		releaseClaim: sync.OnceFunc(b.claim.Release),
		kicks:        make(chan struct{}, 1), entries: map[string]*entry{}, idle: make(chan struct{}),
	}
	if c.log == nil {
		c.log = log
	}
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

	if err := c.clearBoot(); err != nil {
		return nil, err
	}
	if err := c.writeHook(ctx, o.HookCommand); err != nil {
		return nil, err
	}
	trashed, err := c.index(b)
	if err != nil {
		return nil, err
	}
	c.log.Info("code_cache_opened", "dir", c.dir, "repos", len(c.entries), "bytes", c.heldBytes.Load(),
		"budget", o.MaxBytes, "trashed", trashed)
	c.setGauges()
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
	for _, a := range b.aside {
		path := filepath.Join(repos, a.name)
		if err := toTrash(c.dir, path, bootLabel(a.name)); err != nil {
			return trashed, fmt.Errorf("cache: moving %s aside (%s): %w", path, a.why, err)
		}
		trashed++
		c.log.Warn("code_cache_trashed", "path", path, "why", a.why)
	}
	for _, k := range b.keep {
		inc := &incarnation{n: k.n, dir: filepath.Join(repos, k.name), lastUsed: k.lastUsed, stamped: k.lastUsed}
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
		c.heldRepos.Add(1)
		c.entries[k.id] = &entry{id: k.id, current: inc}
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

// Start starts the cache's background work — the janitor — under ctx: it
// ends with ctx or with [Cache.Stop], whichever comes first. A second call,
// or a call after Stop, does nothing. A cache that is never started still
// opens and pins; nothing is removed from trash/ until it is.
func (c *Cache) Start(ctx context.Context) {
	c.lifeMu.Lock()
	defer c.lifeMu.Unlock()
	if c.started || c.stopped {
		return
	}
	c.started = true
	go c.janitor(ctx)
}

// Stop ends the cache: it marks it stopped — nothing of the cache's starts
// in the directory again — and ends its lifetime, so what runs under it
// ends with it. The claim on the directory goes back the moment nothing of
// the cache's is running in it: at once, before Stop returns, when nothing
// is (whatever ctx says — that is decided under the cache's own lock, never
// raced against a context that may already have ended); otherwise Stop
// waits for it, bounded by ctx, and a wait ctx cuts short leaves the claim
// held until the last of that work has ended (logged
// code_cache_stop_unfinished), so no other process can open the directory
// while something of this one is still writing in it. Pins still held keep
// their directories until the next Open, which needs nothing from them: a
// release after Stop moves nothing. Safe to call more than once; a second
// call waits again, bounded by its own ctx.
func (c *Cache) Stop(ctx context.Context) {
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
