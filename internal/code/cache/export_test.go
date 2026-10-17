package cache

import (
	"context"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"time"

	"github.com/crewlet/crewlet/internal/code"
	"github.com/crewlet/crewlet/internal/code/gitproc"
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

// AfterSweep makes every full round call f with the incarnation's directory
// once its sweep has made it the image, before what it left is held to the
// pool's shape check.
func AfterSweep(o *Options, f func(dir string)) {
	if o.knobs == nil {
		o.knobs = &knobs{}
	}
	o.knobs.swept = f
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

// SetCheckBudget makes f the bound of every check pass, over the bytes the
// copy holds, in place of CheckBudget's.
func SetCheckBudget(o *Options, f func(bytes int64) time.Duration) {
	if o.knobs == nil {
		o.knobs = &knobs{}
	}
	o.knobs.checkBudget = f
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

// IndexRound runs a full round's index step (indexRound) on repoID's current
// incarnation over packs — the rows a full round would have read — holding
// the repository's write lock as a round does, and reports whether it wrote
// an index.
func IndexRound(c *Cache, repoID string, packs []code.PackRow) bool {
	e := lookup(c, repoID)
	if err := c.acquireWrite(context.Background(), e); err != nil {
		return false
	}
	defer c.releaseWrite(e, writeExclusive)
	e.mu.Lock()
	inc := e.current
	e.mu.Unlock()
	snap := snapshot{full: true, packs: make(map[packKey]code.PackRow, len(packs))}
	for _, p := range packs {
		snap.packs[packKey{packID: p.PackID, part: p.Part}] = p
	}
	return c.indexRound(context.Background(), e, inc, snap)
}

// ForgetMaintenance takes every maintenance output — the multi-pack-index
// with its bitmap and reverse index, the commit-graph — out of repoID's
// current incarnation, off the disk and out of what the cache holds of it,
// as if no index had been written there and no run had ever ended: the
// maintainer's next pass finds it due, and a full round's index step finds
// no index in its way. With nothing running on the repository.
func ForgetMaintenance(c *Cache, repoID string) error {
	e := lookup(c, repoID)
	e.mu.Lock()
	defer e.mu.Unlock()
	inc := e.current
	packDir := filepath.Join(inc.dir, "objects", "pack")
	list, err := os.ReadDir(packDir)
	if err != nil {
		return err
	}
	paths := []string{filepath.Join(inc.dir, "objects", "info", "commit-graph")}
	for _, d := range list {
		if midxFile.MatchString(d.Name()) {
			paths = append(paths, filepath.Join(packDir, d.Name()))
		}
	}
	for _, p := range paths {
		if err := os.Remove(p); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return err
		}
	}
	inc.midx = midxState{}
	c.setSize(inc, inc.diskBytes, 0)
	return nil
}

// SetBitmapLookupTable sets whether a bitmap the maintainer writes from now
// on carries its lookup table (ServeConfig's pack.writeBitmapLookupTable): on
// in production, off for BenchmarkMidx's arm without it. A bitmap already
// written keeps what it was written with; ForgetMaintenance and a pass write
// it again.
func SetBitmapLookupTable(c *Cache, on bool) { c.noLookupTable.Store(!on) }

// SetMaintenanceMemory sets what the maintenance runs beside the largest may
// be charged at together. Before any pass.
func SetMaintenanceMemory(c *Cache, n int64) { c.maintMemory = n }

// MaintenanceMemory is what the maintenance runs beside the largest may be
// charged at together.
func MaintenanceMemory(c *Cache) int64 { return c.maintMemory }

// SetNodeMemory makes every later read of the node's memory — each
// maintainer pass's first step — answer bytes, from the case rather than the
// pool.
func SetNodeMemory(c *Cache, bytes int64) {
	c.memMu.Lock()
	defer c.memMu.Unlock()
	c.memSource = func() (gitproc.Memory, error) { return gitproc.Memory{Bytes: bytes, From: "the case"}, nil }
}

// NodeMemoryUnreadable makes every later read of the node's memory fail
// with err.
func NodeMemoryUnreadable(c *Cache, err error) {
	c.memMu.Lock()
	defer c.memMu.Unlock()
	c.memSource = func() (gitproc.Memory, error) { return gitproc.Memory{}, err }
}

// SetMaintenanceBudget makes the node's memory what leaves its background
// work a budget of n — what the reserve fixed at Open plus n — from now on:
// what everything in flight is held to together, and what one step's charge
// may never exceed. Between passes.
func SetMaintenanceBudget(c *Cache, n int64) {
	bytes := n + c.memReserve
	SetNodeMemory(c, bytes)
	c.memMu.Lock()
	c.memory = gitproc.Memory{Bytes: bytes, From: "the case"}
	c.memMu.Unlock()
	c.maint.setBudget(n)
}

// MaintenanceBudget is what everything in flight is held to together now.
func MaintenanceBudget(c *Cache) int64 { return c.maint.budgetNow() }

// MemoryReserve is what a node of memory bytes whose git pool admits
// transfers transfers and has cores read slots keeps of its memory for
// everything but its background work.
func MemoryReserve(transfers, cores int, memory int64) int64 {
	return memoryReserve(transfers, cores, memory)
}

// MemoryReserved is what c keeps of the node's memory for everything but its
// background work, as Open fixed it.
func MemoryReserved(c *Cache) int64 { return c.memReserve }

// BitmapCharge, IndexCharge, GraphCharge and FsckCharge are the memory each
// step over repoID's current incarnation is charged at against the node's
// budget: its own figure an object, and room beside it for one transfer of
// the repository.
func BitmapCharge(c *Cache, repoID string) int64 { return chargeOf(c, repoID, BitmapBytesPerObject) }

// IndexCharge is the index without a bitmap's charge (BitmapCharge).
func IndexCharge(c *Cache, repoID string) int64 { return chargeOf(c, repoID, IndexBytesPerObject) }

// GraphCharge is the commit-graph's charge (BitmapCharge).
func GraphCharge(c *Cache, repoID string) int64 { return chargeOf(c, repoID, GraphBytesPerObject) }

// FsckCharge is Verify's connectivity check's charge (BitmapCharge).
func FsckCharge(c *Cache, repoID string) int64 { return chargeOf(c, repoID, FsckBytesPerObject) }

func chargeOf(c *Cache, repoID string, perObject int64) int64 {
	e := lookup(c, repoID)
	e.mu.Lock()
	defer e.mu.Unlock()
	return charge(e.current.objects, perObject)
}

// ObjectsOf is how many objects repoID's current incarnation holds, as its
// rows count them.
func ObjectsOf(c *Cache, repoID string) int64 {
	e := lookup(c, repoID)
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.current.objects
}

// Indexed is what repoID's current incarnation holds of the maintainer's:
// how many packs its multi-pack-index covers (0, none), whether it has its
// bitmap, and whether the commit-graph is there.
func Indexed(c *Cache, repoID string) (packs int, bitmap, graph bool) {
	e := lookup(c, repoID)
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.current.midx.Packs, e.current.midx.Bitmap, e.current.midx.Graph
}

// HoldOther charges mem against the node's budget as a process beside the
// maintenance runs — a round's index, a check — would be, and answers
// whether it was admitted and a function taking it back out of flight.
func HoldOther(c *Cache, mem int64) (bool, func()) {
	if c.maint.admitOther(mem) != admitted {
		return false, func() {}
	}
	return true, func() { c.maint.doneOther(mem) }
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

// StagedWriteFails makes every write into a bundle's staged files, while the
// cache Open builds downloads it, answer fn's error for the bundle's object
// (nil to write).
func StagedWriteFails(fn func(object objstore.Key) error) func(*Options) {
	return func(o *Options) {
		if o.knobs == nil {
			o.knobs = &knobs{}
		}
		o.knobs.stagedWrite = fn
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

// RoundPacksAt makes the cache Open builds write its round's own
// multi-pack-index once a full round leaves n packs — MidxRoundPacks in
// production — so a case exercises the round's index without pushing
// hundreds of packs.
func RoundPacksAt(n int) func(*Options) {
	return func(o *Options) {
		if o.knobs == nil {
			o.knobs = &knobs{}
		}
		o.knobs.roundPacks = n
	}
}

// MidxPacksAt makes the cache Open builds find a repository due again once n
// packs have gathered beside the maintainer's index — MidxPacks in
// production — so a case reaches a run over that index without pushing
// dozens of packs.
func MidxPacksAt(n int) func(*Options) {
	return func(o *Options) {
		if o.knobs == nil {
			o.knobs = &knobs{}
		}
		o.knobs.midxPacks = n
	}
}

// OnRoundIndex makes the cache Open builds call fn with the incarnation's
// directory every time a full round writes its own multi-pack-index there.
func OnRoundIndex(fn func(dir string)) func(*Options) {
	return func(o *Options) {
		if o.knobs == nil {
			o.knobs = &knobs{}
		}
		o.knobs.roundIndexed = fn
	}
}

// SetRoundPacks sets how many packs a full round of c leaves before it writes
// its own multi-pack-index. Between rounds, with nothing running.
func SetRoundPacks(c *Cache, n int) { c.roundPacks = n }

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

// SetHashHook makes every hash of a hint's pack call fn with the pack's
// checksum and the hash's bound, once it holds its first read slot
// (ConfirmHint).
func SetHashHook(c *Cache, fn func(pack string, bound time.Duration)) { c.hashHook = fn }

// WriteLockHeld reports whether something holds repoID's write lock whole —
// a round, or a check's listing — at this instant (entry.write).
func WriteLockHeld(c *Cache, repoID string) bool {
	e := lookup(c, repoID)
	if !e.write.TryAcquire(1) {
		return true
	}
	e.write.Release(1)
	return false
}

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

// HashPacks hashes every pack in the incarnation directory dir as a check
// does — each bundle's .pack, .idx and .rev back to back (hashBundle) —
// answering the bytes it read: the share of a check BenchmarkVerify's
// step=hash times.
func HashPacks(ctx context.Context, dir string) (int64, error) {
	packs, err := filepath.Glob(filepath.Join(dir, "objects", "pack", "pack-*.pack"))
	if err != nil {
		return 0, err
	}
	buf := make([]byte, copyBuffer)
	var read int64
	for _, p := range packs {
		stem := p[:len(p)-len(".pack")]
		if _, err := hashBundle(ctx, stem, buf, nil); err != nil {
			return read, err
		}
		for _, ext := range []string{".pack", ".idx", ".rev"} {
			info, err := os.Stat(stem + ext)
			if err != nil {
				return read, err
			}
			read += info.Size()
		}
	}
	return read, nil
}

// MarkWhole marks repoID's current incarnation to be read whole in place for
// reason (dropping drops, for RebuildDerived), as a request's in-place repair
// does before its round runs (markWhole) — and nothing more: no round is
// asked.
func MarkWhole(c *Cache, repoID string, reason RebuildReason, drops gitproc.Part) {
	e := lookup(c, repoID)
	e.mu.Lock()
	defer e.mu.Unlock()
	markWhole(e.current, reason, drops)
}

// Reread marks every current incarnation to be read whole for reason, as a
// code log recreated or gone back under the node does (Cache.reread) — and
// nothing more: no round is asked.
func Reread(c *Cache, reason RebuildReason) {
	c.reread(reason, func(*incarnation) bool { return true })
}
