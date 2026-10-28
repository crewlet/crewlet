package cache

import (
	"context"
	"crypto/sha1"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/code"
	"github.com/crewlet/crewlet/internal/code/gitproc"
	"github.com/crewlet/crewlet/internal/objstore"
	"github.com/crewlet/crewlet/internal/queue/topics"
	"github.com/crewlet/crewlet/internal/statelog"
)

// The suite's knobs into the cache: the background loops' steps run
// synchronously, the moments a case must hold still, and what the cache
// holds in memory read back.

// JanitorPass runs one janitor pass between its ticks — a kicked one —
// synchronously, as work of the cache's own (a stopped cache runs none).
func JanitorPass(c *Cache) { JanitorPassUnder(context.Background(), c) }

// JanitorPassUnder is JanitorPass under ctx, as the janitor runs its passes
// under the context Start was given: a case ends ctx part way through a pass,
// as the engine stopping does.
func JanitorPassUnder(ctx context.Context, c *Cache) {
	if c.enter() {
		c.janitorPass(ctx, false)
		c.leave()
	}
}

// JanitorTick runs one of the janitor's TICKED passes, synchronously: the pass
// JanitorPass runs, every repository's last use stamped, and the state of
// every copy whose memo moved since it was last written written again.
func JanitorTick(c *Cache) {
	if c.enter() {
		c.janitorPass(context.Background(), true)
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

// BeforeMeasuring makes the janitor's measures call f as each is about to
// list a directory: scratch/ itself, then each directory a listing under it
// named; and each memo cohort a copy's listing measures.
func BeforeMeasuring(o *Options, f func(dir string)) {
	if o.knobs == nil {
		o.knobs = &knobs{}
	}
	o.knobs.measuring = f
}

// BeforeStatingMemo makes a copy's memo listing call f with each entry of a
// cohort it measures as it is about to stat it, the cohort's listing having
// named it.
func BeforeStatingMemo(o *Options, f func(path string)) {
	if o.knobs == nil {
		o.knobs = &knobs{}
	}
	o.knobs.statingMemo = f
}

// BeforeTrimming makes the memo's trim call f as it is about to move the
// cohort at dir out of a copy it found holding it.
func BeforeTrimming(o *Options, f func(dir string)) {
	if o.knobs == nil {
		o.knobs = &knobs{}
	}
	o.knobs.trimming = f
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

// BeforeRoundEnds makes every round call f with its repository's id once it
// has converged and before it lets the repository go (Cache.run).
func BeforeRoundEnds(o *Options, f func(repo string)) {
	if o.knobs == nil {
		o.knobs = &knobs{}
	}
	o.knobs.roundEnding = f
}

// KickPending reports whether a kick is waiting for the maintainer, without
// taking it.
func KickPending(c *Cache) bool { return len(c.maintKicks) > 0 }

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

// AdoptionSyncsCounted makes every adoption's directory sync count itself in
// *n, under mu, before it syncs the directory as the cache would.
func AdoptionSyncsCounted(o *Options, mu *sync.Mutex, n *int) {
	if o.knobs == nil {
		o.knobs = &knobs{}
	}
	o.knobs.adoptSync = func(dir string) error {
		mu.Lock()
		*n++
		mu.Unlock()
		return syncDir(dir)
	}
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
	files, err := c.image.present(inc.dir)
	if err != nil {
		return err
	}
	for _, f := range files {
		if err := os.Remove(filepath.Join(inc.dir, f.rel)); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return err
		}
	}
	c.image.each(c.image.all(), func(_ int, d *Derived) { c.setDerived(inc, d.Name, derivedState{}, false) })
	inc.maintained = maintainedState{}
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
	return chargeOver(e.current.objects, perObject, e.current.objects)
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
	return e.current.covered(), e.current.bitmapped(), e.current.graphed()
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

// RebuildWhole is the last step of a request's repair alone (rebuild.go): the
// whole rebuild of the incarnation failed was handed out of — joined,
// refused by the same rules and counted as an ask of the incarnation's line,
// as Cache.escalate asks it — seeded with no pack, so every pack is
// downloaded. What the cases of the whole rebuild's own rules drive: a
// request reaches it only once its in-place round and its scrub repaired
// nothing, and then seeded with what the scrub found whole.
func RebuildWhole(ctx context.Context, c *Cache, failed Synced, f Failure) (Synced, error) {
	if err := c.handedOut(failed, "a rebuild"); err != nil {
		return Synced{}, err
	}
	return c.rebuild(ctx, failed, f, true, nil)
}

// Total is what the janitor holds the cache to its budget by: every
// incarnation's files, the downloads in flight and scratch/ as last measured.
func Total(c *Cache) int64 { return c.total() }

// SetMaxBytes sets the cache's budget. Between syncs, with nothing running.
func SetMaxBytes(c *Cache, n int64) { c.maxBytes = n }

// NewHeat is a Heat counting on now from its first reading, as Open builds the
// cache's own.
func NewHeat(now func() time.Time) *Heat { return newHeat(newHeatClock(now)) }

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

// SetFetchConcurrency sets how many downloads one round runs at once — and
// the installer's share of the node's, which is a round's (layers.go). Before
// any sync.
func SetFetchConcurrency(c *Cache, n int) {
	c.fetchConcurrency = n
	c.installer = newLayerInstaller(n)
}

// SetNodeFetches sets how many downloads every round on the node shares.
// Before any sync.
func SetNodeFetches(c *Cache, n int64) { c.fetches = newFetchSlots(n) }

// SetMaxFullRounds sets how many full reads of a repository's rows the node
// has against the estate at once. Before any sync.
func SetMaxFullRounds(c *Cache, n int64) { c.full = newFullRounds(n, c.metrics) }

// FullRounds is how many full reads hold one of the node's slots now, the
// most that have at once, and how many wait for one.
func FullRounds(c *Cache) (running, most, waiting int64) {
	c.full.mu.Lock()
	defer c.full.mu.Unlock()
	return c.full.running, c.full.most, c.full.waiting
}

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
	c.markWhole(e, e.current, Failure{Reason: reason, Drops: drops}, dropRequest)
}

// Reread marks every current incarnation to be read whole for reason, as a
// code log recreated or gone back under the node does (Cache.reread) — and
// nothing more: no round is asked.
func Reread(c *Cache, reason RebuildReason) {
	c.reread(reason, func(*incarnation) bool { return true })
}

// WhileHashing makes every check call f with the path of each file as it is
// about to hash it (check.go).
func WhileHashing(o *Options, f func(path string)) {
	if o.knobs == nil {
		o.knobs = &knobs{}
	}
	o.knobs.hashing = f
}

// IndexRepair reports whether repoID's current incarnation's
// multi-pack-index is due at once as a repair (incarnation.indexRepair).
func IndexRepair(c *Cache, repoID string) bool {
	e := lookup(c, repoID)
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.current != nil && e.current.indexRepair
}

// Vouched is the paths, relative to the incarnation, of every file of
// repoID's current incarnation this process vouches for at the identity it
// has now (check.go), sorted.
func Vouched(c *Cache, repoID string) []string {
	e := lookup(c, repoID)
	e.mu.Lock()
	defer e.mu.Unlock()
	var out []string
	for _, rel := range vouchedRels(e.current) {
		if vouchedAt(e.current, rel, lstatID(filepath.Join(e.current.dir, rel))) {
			out = append(out, rel)
		}
	}
	return out
}

// VouchMemory is what holding the vouches of n packs — each pack's .idx and
// .rev — costs the heap, as an incarnation holds them.
func VouchMemory(n int) (perPack float64) {
	inc := &incarnation{}
	var key [hashTrailer]byte
	id := fileID{present: true, dev: 1, ino: 1, size: 1, modified: 1}
	runtime.GC()
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	for i := range n {
		binary.BigEndian.PutUint64(key[:], uint64(i))
		cs := hex.EncodeToString(key[:])
		vouch(inc, packFileRel(cs, ".idx"), id)
		vouch(inc, packFileRel(cs, ".rev"), id)
	}
	runtime.GC()
	runtime.ReadMemStats(&after)
	runtime.KeepAlive(inc)
	return float64(after.HeapAlloc-before.HeapAlloc) / float64(n)
}

// CheckPiecesOf makes every hash read a file n bytes a slot, in place of
// CheckPieceBytes, and tell f each piece's path once it is read, its slot still
// held (held), and again once the slot is given back.
func CheckPiecesOf(o *Options, n int64, f func(path string, held bool)) {
	if o.knobs == nil {
		o.knobs = &knobs{}
	}
	o.knobs.checkPiece, o.knobs.pieceRead = n, f
}

// WantIndex makes repoID's current incarnation want the maintainer's
// bitmapped index again over the packs it holds — as if its index had been
// written without a bitmap — and its last run long ago, so the next pass runs
// the index step over an index that has not changed.
func WantIndex(c *Cache, repoID string) {
	e := lookup(c, repoID)
	e.mu.Lock()
	defer e.mu.Unlock()
	inc := e.current
	if inc.bitmapped() {
		c.made(inc, DerivedMidx, inc.covered(), StepIndex)
	}
	inc.maintained = maintainedState{}
}

// testImage is the image of the build's declarations (DerivedFiles), for a
// helper that reads a directory with no cache open.
func testImage() *image {
	im, err := newImage(DerivedFiles())
	if err != nil {
		panic(err)
	}
	return im
}

// recordedChains is what the state at dir records each chain file of an
// installed chain holds (chainWants), for a helper with no cache open: the
// ids its two chains' records list, which is what a boot's first round holds
// the chain files to.
func recordedChains(dir string) map[string][]byte {
	inc := &incarnation{}
	if raw, err := os.ReadFile(filepath.Join(dir, stateName)); err == nil {
		var st state
		if json.Unmarshal(raw, &st) == nil {
			inc.derived = st.Derived
		}
	}
	return chainWants(inc)
}

// Unsound is every file of the incarnation at dir a scrub holds to its
// checksum — each pack's .idx, .pack and .rev, and every derived file
// (scrub.go), a chain file to what its state records — that fails it, by its
// path relative to dir.
func Unsound(dir string) ([]string, error) {
	p := pieces{buf: make([]byte, copyBuffer), size: CheckPieceBytes}
	files, wants, err := scrubFiles(testImage(), dir, recordedChains(dir))
	if err != nil {
		return nil, err
	}
	var out []string
	for _, f := range files {
		v, _, _, err := trailerSound(context.Background(), filepath.Join(dir, f.rel), p, wants[f.rel])
		if err != nil {
			return nil, err
		}
		if v == hashDamaged {
			out = append(out, f.rel)
		}
	}
	return out, nil
}

// DeclareCheck makes c's declaration name declare check, after Open held
// the declarations to Validate: a Check no reader of this build knows, which
// Validate refuses — what a case sets to show that every reader of a derived
// file's bytes reads its declaration's Check rather than holding every file
// to a trailer. Nothing of c may be running.
func DeclareCheck(c *Cache, name DerivedName, check Check) {
	for i := range c.image.decls {
		if c.image.decls[i].Name == name {
			c.image.decls[i].Check = check
		}
	}
}

// ScrubListing is what a scrub of repoID's current incarnation would hold to
// its bytes (scrubFiles) over c's own declarations, by path relative to the
// incarnation — or the error the scrub would stop at before reading a byte.
func ScrubListing(c *Cache, repoID string) ([]string, error) {
	e := lookup(c, repoID)
	e.mu.Lock()
	dir, chains := e.current.dir, chainWants(e.current)
	e.mu.Unlock()
	files, _, err := scrubFiles(c.image, dir, chains)
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, len(files))
	for _, f := range files {
		out = append(out, filepath.ToSlash(f.rel))
	}
	return out, nil
}

// ScrubbedFiles is the bytes and the pieces of every file a scrub of the
// incarnation at dir hashes (scrubFiles), as it holds them now.
func ScrubbedFiles(dir string) (bytes int64, pieces int, err error) {
	files, _, err := scrubFiles(testImage(), dir, recordedChains(dir))
	if err != nil {
		return 0, 0, err
	}
	for _, f := range files {
		info, err := os.Lstat(filepath.Join(dir, f.rel))
		if err != nil {
			return 0, 0, err
		}
		bytes += info.Size()
		pieces += int((info.Size() + CheckPieceBytes - 1) / CheckPieceBytes)
	}
	return bytes, pieces, nil
}

// Report hands the cache's observer one run, as the pool does after every
// run (Cache.observe): what a case uses to report a run at the moment it
// chooses.
func Report(c *Cache, ran gitproc.Ran) { c.observe(ran) }

// Settled reports whether repoID has no round running or queued, and its
// current incarnation no report waiting for one (incarnation.repair).
func Settled(c *Cache, repoID string) bool {
	e := lookup(c, repoID)
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.running == nil && e.next == nil && (e.current == nil || !e.current.repair.pending())
}

// HeldOff reports whether repoID has no round running or queued, and its
// current incarnation's repair mark asks nothing a round can do now: every
// finding left is a pack whose download is held off (repairMark.fetchFailed).
func HeldOff(c *Cache, repoID string) bool {
	e := lookup(c, repoID)
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.running == nil && e.next == nil && e.current != nil && e.current.repair.pending() &&
		!e.current.repair.due(c.now())
}

// RepairPending reports whether repoID's current incarnation holds a report
// no round has taken (incarnation.repair).
func RepairPending(c *Cache, repoID string) bool {
	e := lookup(c, repoID)
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.current != nil && e.current.repair.pending()
}

// GraphFailed reports whether repoID's current incarnation's commit-graph
// waits for the ordinary rules rather than being written first
// (incarnation.graphFailed).
func GraphFailed(c *Cache, repoID string) bool {
	e := lookup(c, repoID)
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.current != nil && e.current.graphFailed
}

// ScrubPass runs every scrub due now, one after another, as the scrubber's
// pass does, and answers the repositories it scrubbed in order: what a case
// runs instead of starting the scrubber, whose wake it would otherwise wait
// for.
func ScrubPass(c *Cache) []string {
	if !c.enter() {
		return nil
	}
	defer c.leave()
	done, _ := c.scrubPass(c.life)
	return done
}

// ScrubDue is why repoID's current incarnation's scrub is due at once —
// "first", "report", or "" when only its interval can make it due — and
// whether one is running.
func ScrubDue(c *Cache, repoID string) (why string, running bool) {
	e := lookup(c, repoID)
	if e == nil {
		return "", false
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.current == nil {
		return "", false
	}
	if w := e.current.scrubDue; w != 0 {
		why = string(w.trigger())
	}
	return why, e.current.scrubbing
}

// Scrubbed is when repoID's current incarnation's last scrub ended and
// whether it found anything (zero and false before its first), and when its
// bytes were last held to their checksums, which its interval runs from.
func Scrubbed(c *Cache, repoID string) (at time.Time, found bool, base time.Time) {
	e := lookup(c, repoID)
	if e == nil {
		return time.Time{}, false, time.Time{}
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.current == nil {
		return time.Time{}, false, time.Time{}
	}
	return e.current.scrubbedAt, e.current.scrubFound, e.current.scrubBase
}

// Findings is the files a round's check or a scrub found failing that
// repoID's repair mark still holds (repairMark.found), by their paths
// relative to the incarnation, sorted.
func Findings(c *Cache, repoID string) []string {
	e := lookup(c, repoID)
	if e == nil {
		return nil
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.current == nil {
		return nil
	}
	var out []string
	for rel := range e.current.repair.found {
		out = append(out, rel)
	}
	slices.Sort(out)
	return out
}

// ScrubFor is the scrub a repair's step 3 asks of repoID's current
// incarnation (scrubFor, as Cache.escalate asks it for callers whose failures
// began at an unknown instant): what it answers failing, by path relative to
// the incarnation, and the packs it answers whole — what a rebuild past it
// links rather than downloads — both sorted.
func ScrubFor(c *Cache, repoID string) (damaged, whole []string, err error) {
	e := lookup(c, repoID)
	if e == nil {
		return nil, nil, errors.New("cache: no such repository in the cache")
	}
	e.mu.Lock()
	inc := e.current
	e.mu.Unlock()
	if inc == nil {
		return nil, nil, errors.New("cache: the repository has no incarnation")
	}
	out, err := c.scrubFor(e, inc, time.Time{})
	for _, f := range out.damaged {
		damaged = append(damaged, f.rel)
	}
	slices.Sort(damaged)
	whole = slices.Sorted(slices.Values(out.whole))
	return damaged, whole, err
}

// ScrubWakesWith makes the scrubber ask f for its wake at the next interval's
// end — next, on the cache's clock — in place of a timer: a case on a clock of
// its own sees when the scrubber means to wake, and wakes it.
func ScrubWakesWith(o *Options, f func(next time.Time) <-chan time.Time) {
	if o.knobs == nil {
		o.knobs = &knobs{}
	}
	o.knobs.scrubWake = f
}

// ReadFails makes every hash a check or a scrub makes ask f before each piece
// it reads, an error f answers being that read's: a disk failing under one
// file.
func ReadFails(o *Options, f func(path string) error) {
	if o.knobs == nil {
		o.knobs = &knobs{}
	}
	o.knobs.readFault = f
}

// ScrubAsking is how many requests' scrubs wait for the node's one scrub.
func ScrubAsking(c *Cache) int {
	c.scrubs.mu.Lock()
	defer c.scrubs.mu.Unlock()
	return c.scrubs.asking
}

// Quiet reports whether repoID has no round running or queued, whatever its
// repair mark still holds.
func Quiet(c *Cache, repoID string) bool {
	e := lookup(c, repoID)
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.running == nil && e.next == nil
}

// ValidateDerived is the check Open holds a build's declarations to
// (validateAll): every declaration's Validate, and what holds only across
// them.
func ValidateDerived(decls []Derived) error { return validateAll(decls) }

// OwnersOf is every declaration of the build's whose Owns names rel — a path
// relative to an incarnation, slash-separated — in declaration order: one
// for a derived file, none for any other.
func OwnersOf(rel string) []DerivedName {
	dir, name := filepath.Split(filepath.FromSlash(rel))
	dir = filepath.Clean(dir)
	var out []DerivedName
	for _, d := range DerivedFiles() {
		if slices.ContainsFunc(d.Owns, func(o Owned) bool {
			return filepath.FromSlash(o.Dir) == dir && o.Name.MatchString(name)
		}) {
			out = append(out, d.Name)
		}
	}
	return out
}

// HoldersOf is every declaration of the build's a write of which holds rel
// only while it runs (Write.Holds), in declaration order.
func HoldersOf(rel string) []DerivedName {
	dir, name := filepath.Split(filepath.FromSlash(rel))
	dir = filepath.Clean(dir)
	var out []DerivedName
	for _, d := range DerivedFiles() {
		if slices.ContainsFunc(d.holds(), func(p FilePattern) bool {
			return filepath.FromSlash(p.Dir) == dir && p.Name.MatchString(name)
		}) {
			out = append(out, d.Name)
		}
	}
	return out
}

// WithDerived opens a cache over decls in place of DerivedFiles(): a case
// proves the code reads the declarations by changing one.
func WithDerived(decls []Derived) func(*Options) {
	return func(o *Options) {
		if o.knobs == nil {
			o.knobs = &knobs{}
		}
		o.knobs.derived = decls
	}
}

// StatingWith tells f the path, relative to its incarnation and
// slash-separated, of every directory entry the cache stats to count its
// bytes (image.sizes).
func StatingWith(f func(rel string)) func(*Options) {
	return func(o *Options) {
		if o.knobs == nil {
			o.knobs = &knobs{}
		}
		o.knobs.stating = func(rel string) { f(filepath.ToSlash(rel)) }
	}
}

// WriteCharge is what w is admitted at over repoID's current incarnation:
// its own memory an object of what it covers — the repository, or the tail
// for the node's tip — and the room for one transfer of the repository
// beside it.
func WriteCharge(c *Cache, repoID string, w Write) int64 {
	if w.Step == StepTipGraph {
		return TipCharge(c, repoID)
	}
	return chargeOf(c, repoID, w.BytesPerObject)
}

// BackfillStep takes the backfill's next step due and does it, synchronously
// and owing no pace, as work of the cache's own: "check", "merge", "gap",
// "history" or "horizon" for the step it took, "" when none was due — and the
// error of a page the estate did not serve.
func BackfillStep(c *Cache) (string, error) {
	if !c.enter() {
		return "", ErrStopped
	}
	defer c.leave()
	done := c.backfillStep(c.life)
	return workNames[done.work], done.err
}

// workNames is each kind of step as BackfillStep answers it; "" for none.
var workNames = map[backfillWork]string{workCheck: "check", workMerge: "merge", workGap: "gap",
	workHistory: "history", workHorizon: "horizon"}

// BackfillPick is the backfill's choice of its next step — the repository it
// would take, "" for none — made and let go with the step not taken: the pick
// alone, which a case runs beside the cache's readers so the two meet.
func BackfillPick(c *Cache) (string, error) {
	if !c.enter() {
		return "", ErrStopped
	}
	defer c.leave()
	t, _ := c.nextBackfill()
	if t == nil {
		return "", nil
	}
	t.e.mu.Lock()
	gone := c.unpin(t.e, t.inc)
	t.e.mu.Unlock()
	c.trash(gone)
	return t.e.id, nil
}

// SyntheticCopies adds n copies to c that hold no file — each a converged
// incarnation whose index holds five vouched runs too far apart in size to
// merge and whose next horizon is a day off, the first due of them with a gap
// for the backfill to read — and marks each for the backfill's schedule, the
// copy used most recently first: what BenchmarkBackfillPick picks among. It
// answers their ids.
func SyntheticCopies(c *Cache, n, due int) []string {
	now := c.now()
	base := statelog.Position{Stream: topics.CodeLogStream, Generation: 1, Seq: 1}.Packed()
	ids := make([]string, n)
	for i := range n {
		id := fmt.Sprintf("synthetic-%06d", i)
		e := newEntry(id)
		rec := &provenanceState{From: base, Through: base + 2_500, OldestAt: now, Next: 6}
		inc := &incarnation{n: 1, dir: filepath.Join(c.dir, reposDir, id+".1.git"), converged: true,
			lastUsed: now.Add(-time.Duration(i) * time.Second),
			vouched:  vouches{derived: map[string]fileStamp{}}}
		if i < due {
			rec.GapFrom, rec.GapThrough, rec.Through = base+2_500, base+2_510, base+2_510
		}
		for k, pushes := range []int64{500, 200, 80, 30, 10} {
			r := provenanceRun{N: uint64(k + 1), Bytes: 64 * pushes, From: base + int64(k)*500,
				Through: base + int64(k)*500 + pushes, Pushes: pushes}
			rec.Runs = append(rec.Runs, r)
			inc.vouched.derived[runRel(r.N)] = fileStamp{dev: 1, ino: r.N, size: r.Bytes}
		}
		inc.derived = map[DerivedName]derivedState{DerivedProvenance: {Step: StepProvenance, Provenance: rec}}
		e.current = inc
		c.mu.Lock()
		c.entries[id] = e
		c.mu.Unlock()
		c.backfillDirty(e)
		ids[i] = id
	}
	return ids
}

// PickAndSettle is one pick of c's backfill among synthetic copies
// (SyntheticCopies) and its step taken as done: the copy's gap closed — or
// left open where keep, so the copy is due again, as a busy repository's
// folds keep it — and the copy marked, as a step's end marks it. It answers
// the copy picked, "" for none.
func PickAndSettle(c *Cache, keep bool) string {
	t, _ := c.nextBackfill()
	if t == nil {
		return ""
	}
	t.e.mu.Lock()
	t.inc.pins--
	if !keep {
		next := t.inc.record().clone()
		next.GapFrom, next.GapThrough = 0, 0
		t.inc.derived[DerivedProvenance] = derivedState{Step: StepProvenance, Provenance: next}
	}
	t.e.mu.Unlock()
	c.backfillDirty(t.e)
	return t.e.id
}

// BackfillAudit is every copy c's backfill schedule holds otherwise than a
// look at it now says (backfillDue): in another queue than its first step
// due, under another key, or counted otherwise as behind — leaving out the
// copies marked since the last pick and those whose time in the later queue
// has come, which the next pick looks at again. None for a schedule every
// change to a copy's work has reached: a change no mark follows is one the
// backfill never takes up, or takes up in the wrong order.
func BackfillAudit(c *Cache) []string {
	s := &c.sched
	s.pick.Lock()
	defer s.pick.Unlock()
	now := c.now()
	c.mu.Lock()
	entries := slices.Collect(maps.Values(c.entries))
	c.mu.Unlock()
	var problems []string
	held := map[*entry]bool{}
	for _, e := range entries {
		held[e] = true
	}
	// Every copy a queue holds is one the cache holds, or one marked since
	// it left — evicted or purged — which the next pick takes out; and the
	// copies counted behind are those.
	behind := 0
	s.mu.Lock()
	pending := map[*entry]bool{}
	for _, e := range s.dirty {
		if !pending[e] && !held[e] && e.bf.behind {
			behind++
		}
		pending[e] = true
	}
	queued := slices.Clone(s.later)
	for k := range s.due {
		queued = append(queued, s.due[k]...)
	}
	s.mu.Unlock()
	for _, e := range queued {
		if !held[e] && !pending[e] {
			problems = append(problems, fmt.Sprintf("%s is queued, and the cache holds it no more", e.id))
		}
	}
	for _, e := range entries {
		e.mu.Lock()
		d := c.backfillDue(e, now)
		e.mu.Unlock()
		s.mu.Lock()
		got := e.bf
		queued := e.bfQueued.Load()
		s.mu.Unlock()
		if got.behind {
			behind++
		}
		switch {
		case got.dirty, got.kind == 0 && !got.when.IsZero() && !got.when.After(now):
		case got.kind != d.work:
			problems = append(problems, fmt.Sprintf("%s is queued for %q, and a look says %q is due", e.id,
				workNames[got.kind], workNames[d.work]))
		case d.work != 0 && !got.used.Equal(d.used):
			problems = append(problems, fmt.Sprintf("%s is queued as used at %s, and was last used at %s", e.id,
				got.used, d.used))
		case d.work == 0 && !got.when.Equal(d.when):
			problems = append(problems, fmt.Sprintf("%s waits for %s, and a look says its next step is due at %s",
				e.id, got.when, d.when))
		case got.behind != d.behind:
			problems = append(problems, fmt.Sprintf("%s counts as behind %t, and a look says %t", e.id, got.behind,
				d.behind))
		case queued != (got.kind != 0):
			problems = append(problems, fmt.Sprintf("%s is queued %t and says %t", e.id, got.kind != 0, queued))
		}
	}
	s.mu.Lock()
	if s.behind != behind {
		problems = append(problems, fmt.Sprintf("the schedule counts %d copies behind, and its copies %d",
			s.behind, behind))
	}
	s.mu.Unlock()
	slices.Sort(problems)
	return problems
}

// BackfillPass takes backfill steps until none is due, and answers every step
// it took, in order: what a case runs in place of starting the backfill,
// whose pace it would otherwise wait out. A step that fails is answered among
// them, its error joined to the pass's, and the pass goes on to the other
// repositories' steps, as the backfill does while the failing one waits out
// its window — the pass, waiting nothing out, ends there.
func BackfillPass(c *Cache) ([]string, error) {
	var (
		steps []string
		errs  []error
	)
	for range 10_000 {
		step, err := BackfillStep(c)
		if err != nil {
			errs = append(errs, err)
		}
		if step == "" {
			return steps, errors.Join(errs...)
		}
		steps = append(steps, step)
	}
	return steps, errors.Join(append(errs, errors.New("cache: the backfill never ran out of steps"))...)
}

// BackfillWakesWith makes the backfill ask f for each wait — after a page,
// after a failure, or until the next horizon falls due — in place of a timer
// of d: a case sees how long the backfill means to wait, and wakes it.
func BackfillWakesWith(o *Options, f func(d time.Duration) <-chan time.Time) {
	if o.knobs == nil {
		o.knobs = &knobs{}
	}
	o.knobs.backfillWake = f
}

// CountDecodes counts every move a view of the index of pushes decodes
// (MoveView.At) into n.
func CountDecodes(o *Options, n *atomic.Int64) {
	if o.knobs == nil {
		o.knobs = &knobs{}
	}
	o.knobs.decodes = n
}

// IndexState is repoID's current incarnation's index of pushes as its record
// holds it — every position packed — with the bytes its entry counts.
type IndexState struct {
	From, Through, GapFrom, GapThrough, Oldest int64
	Complete                                   bool
	// Runs is every run's number, in position order, and RunRanges each
	// one's (From, Through].
	Runs      []uint64
	RunRanges [][2]int64
	Bytes     int64
}

// Index is repoID's current incarnation's index of pushes, and whether it
// keeps one.
func Index(c *Cache, repoID string) (IndexState, bool) {
	e := lookup(c, repoID)
	if e == nil {
		return IndexState{}, false
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.current == nil || e.current.record() == nil {
		return IndexState{}, false
	}
	rec := e.current.record()
	s := IndexState{From: rec.From, Through: rec.Through, GapFrom: rec.GapFrom, GapThrough: rec.GapThrough,
		Oldest: rec.Oldest, Complete: rec.Complete, Bytes: e.current.derived[DerivedProvenance].Bytes}
	for _, r := range rec.Runs {
		s.Runs = append(s.Runs, r.N)
		s.RunRanges = append(s.RunRanges, [2]int64{r.From, r.Through})
	}
	return s, true
}

// RunRel is run n's path relative to its incarnation.
func RunRel(n uint64) string { return runRel(n) }

// WantsHistory reports whether repoID's history is wanted in this process.
func WantsHistory(c *Cache, repoID string) bool {
	e := lookup(c, repoID)
	if e == nil {
		return false
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.wantHistory
}

// RunViaCodes and RunKindCodes are the codes a run's push record names a
// push's way and its principal's kind by.
func RunViaCodes() map[code.PushVia]uint8 { return maps.Clone(runViaCodes) }

// RunKindCodes: see RunViaCodes.
func RunKindCodes() map[code.PrincipalKind]uint8 { return maps.Clone(runKindCodes) }

// MergesWith tells f how many runs each merge of an index of pushes the cache
// Open builds stores merged.
func MergesWith(o *Options, f func(runs int)) {
	if o.knobs == nil {
		o.knobs = &knobs{}
	}
	o.knobs.merged = f
}

// StateWriteFails makes every write of a repository's state by the cache Open
// builds ask f first — round says whether a round writes it — and fail with
// what it answers (nil to write).
func StateWriteFails(o *Options, f func(repo string, round bool) error) {
	if o.knobs == nil {
		o.knobs = &knobs{}
	}
	o.knobs.stateWrite = f
}

// MappedRunsAt bounds the runs the cache Open builds holds mapped at once at
// n, in place of the process's map count's share.
func MappedRunsAt(n int) func(*Options) {
	return func(o *Options) {
		if o.knobs == nil {
			o.knobs = &knobs{}
		}
		o.knobs.mappedRuns = n
	}
}

// MappedRuns is how many runs c holds mapped now, and the most it may.
func MappedRuns(c *Cache) (now, most int) { return c.maps.count(), c.maps.most }

// MappedRunsOf is the bound on mapped runs a process whose map count is count
// holds itself to.
func MappedRunsOf(count int) int { return mappedRunsOf(count) }

// FoldTimes tells f what each round's fold of its delta's pushes into the
// index of pushes took (foldPushes).
func FoldTimes(o *Options, f func(took time.Duration)) {
	if o.knobs == nil {
		o.knobs = &knobs{}
	}
	o.knobs.folded = f
}

// SyntheticIndex is an index of pushes holding pushes synthetic pushes in
// runs runs of as many each — every push moving one branch and introducing
// perPush commits of ids drawn as git's are, uniformly — each run built,
// written and read back in memory as a git node's are: what
// BenchmarkProvenance's lookup arm searches. It answers the index and the
// bytes its runs hold.
func SyntheticIndex(tb testing.TB, runs, pushes, perPush int) (Provenance, int64) {
	tb.Helper()
	base := statelog.Position{Stream: topics.CodeLogStream, Generation: 1, Seq: 1}.Packed()
	var files []runFile
	var size int64
	for k := range runs {
		lo, hi := k*pushes/runs, (k+1)*pushes/runs
		rows := make([]code.IntroducedRow, 0, hi-lo)
		for i := lo; i < hi; i++ {
			row := synthetic(base, i, "refs/heads/main")
			row.Introduced, row.Count = make([]string, perPush), perPush
			for j := range perPush {
				row.Introduced[j] = DrawnOID(fmt.Sprintf("commit-%d-%d", i, j))
			}
			rows = append(rows, row)
		}
		f := runOf(tb, base+int64(lo)-1, base+int64(hi)-1, rows)
		files = append(files, f)
		size += int64(len(f.data))
	}
	return Provenance{runs: files, from: base - 1, indexed: true}, size
}

// SyntheticMerge is two adjacent runs of pushes synthetic pushes each — every
// push moving one branch, or with eachRef a ref of its own, and introducing
// one commit — built, written and read back in memory as a git node's are,
// and a merge of the two into w: what BenchmarkProvenance's merge arm times.
func SyntheticMerge(tb testing.TB, pushes int, eachRef bool) func(w io.Writer) error {
	tb.Helper()
	base := statelog.Position{Stream: topics.CodeLogStream, Generation: 1, Seq: 1}.Packed()
	var runs []runFile
	for k := range 2 {
		rows := make([]code.IntroducedRow, 0, pushes)
		for i := k * pushes; i < (k+1)*pushes; i++ {
			ref := "refs/heads/main"
			if eachRef {
				ref = fmt.Sprintf("refs/heads/seat/%08d", i)
			}
			rows = append(rows, synthetic(base, i, ref))
		}
		runs = append(runs, runOf(tb, base+int64(k*pushes)-1, base+int64((k+1)*pushes)-1, rows))
	}
	return func(w io.Writer) error {
		_, err := mergeRuns(w, runs, base-1)
		return err
	}
}

// DrawnOID is an object id drawn from name as git's are, uniformly: its
// SHA-1.
func DrawnOID(name string) string {
	sum := sha1.Sum([]byte(name))
	return hex.EncodeToString(sum[:])
}

// MemoBytes is what the memo holds node-wide: the running total the janitor
// holds to its share (MemoShare).
func MemoBytes(c *Cache) int64 { return c.memoBytes.Load() }

// MemoStaged is what every read's run has staged for the memo and not yet
// placed or given back: counted against its share with MemoBytes.
func MemoStaged(c *Cache) int64 { return c.memoStaged.Load() }

// EvictToBudget runs the janitor's eviction step alone, synchronously — no
// trim of the memo before it — and answers how many repositories it evicted.
func EvictToBudget(c *Cache) int {
	if !c.enter() {
		return 0
	}
	defer c.leave()
	return c.evictToBudget(context.Background())
}

// MemoState is what repoID's current incarnation records of its memo.
type MemoState struct {
	// Bytes is its record's, and Recorded whether it records one at all.
	Bytes    int64
	Recorded bool
	// Cohorts is each cohort its record holds, oldest first, and Held what
	// each holds.
	Cohorts []uint64
	Held    []int64
	// Listed is whether every cohort on the disk is in the record, and Stated
	// whether the state last written records the record as it stands.
	Listed, Stated bool
}

// MemoOf is repoID's current incarnation's memo, as MemoState says, and false
// where the cache holds no copy of it.
func MemoOf(c *Cache, repoID string) (MemoState, bool) {
	e := lookup(c, repoID)
	if e == nil {
		return MemoState{}, false
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	inc := e.current
	if inc == nil {
		return MemoState{}, false
	}
	d, recorded := inc.derived[DerivedMemo]
	s := MemoState{Bytes: d.Bytes, Recorded: recorded, Listed: inc.memoListed,
		Stated: inc.memoStated == inc.memoVersion}
	for _, ch := range d.Cohorts {
		s.Cohorts, s.Held = append(s.Cohorts, ch.N), append(s.Held, ch.Bytes)
	}
	return s, true
}

// MemoTree is the memo's tree in the incarnation at dir.
func MemoTree(dir string) string { return filepath.Join(dir, memoRel) }

// MemoCohortDir is cohort n's directory in the memo of the incarnation at dir.
func MemoCohortDir(dir string, n uint64) string { return cohortDir(dir, n) }

// MemoEntryFile is the file entry k of the copy s holds is in — in whichever
// of its cohorts holds it, newest first — and "" where none does.
func MemoEntryFile(s Synced, k MemoKey) string {
	s.entry.mu.Lock()
	cs := s.inc.derived[DerivedMemo].Cohorts
	s.entry.mu.Unlock()
	for i := len(cs) - 1; i >= 0; i-- {
		if path := memoPath(s.inc.dir, cs[i].N, k); exists(path) {
			return path
		}
	}
	return ""
}

// exists reports whether path names anything.
func exists(path string) bool {
	_, err := os.Lstat(path)
	return err == nil
}

// MemoNewest is the node's newest cohort: where the next answer is placed.
func MemoNewest(c *Cache) uint64 { return c.memoNewest.Load() }

// MemoCohortsPerShare is how many cohorts a memo at its share holds.
const MemoCohortsPerShare = memoCohortsPerShare

// MaxMemoCohorts bounds the cohorts the node keeps.
const MaxMemoCohorts = maxMemoCohorts

// MemoMaxTail is the most a run may append to a page it staged, by Extend and
// by its tail together.
const MemoMaxTail = memoMaxTail

// MemoEntryBytes is the volume an entry whose body and tail are n bytes takes,
// as the memo counts it.
func MemoEntryBytes(n int) int64 { return memoBytesOf(int64(memoHeader + n)) }

// MemoStagedBytes is what a page of a body of n bytes holds staged, before its
// tail: its file's length, as the memo's share counts it.
func MemoStagedBytes(n int) int64 { return int64(memoHeader + n) }

// LinkWith makes every hard link a swap makes (swap.go) go through f in place
// of os.Link (knobs.link): a case counts them, or makes one fail.
func LinkWith(f func(oldname, newname string) error) func(*Options) {
	return func(o *Options) {
		if o.knobs == nil {
			o.knobs = &knobs{}
		}
		o.knobs.link = f
	}
}

// SyncingWith makes the cache Open builds tell fn every directory a swap or
// a state's write is about to sync (knobs.syncing): a case counts what a
// swap's durability costs.
func SyncingWith(fn func(dir string)) func(*Options) {
	return func(o *Options) {
		if o.knobs == nil {
			o.knobs = &knobs{}
		}
		o.knobs.syncing = fn
	}
}

// Leaving is what is leaving repoID's current incarnation
// (incarnation.leaving): each pack file no live row names any more, by its
// checksum — or by its name, where it carries none — with the event that took
// it out; nil when nothing is, or the cache holds no copy of repoID.
func Leaving(c *Cache, repoID string) map[string]Event {
	e := lookup(c, repoID)
	if e == nil {
		return nil
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.current == nil {
		return nil
	}
	return maps.Clone(e.current.leaving)
}

// Maintained is when the last run of the maintainer the duty cycle counts
// ended on repoID's current incarnation, and how long it took
// (incarnation.maintained): zero before its first, or where the cache holds
// no copy of repoID.
func Maintained(c *Cache, repoID string) (time.Time, time.Duration) {
	e := lookup(c, repoID)
	if e == nil {
		return time.Time{}, 0
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.current == nil {
		return time.Time{}, 0
	}
	return e.current.maintained.LastRun, e.current.maintained.Took
}
