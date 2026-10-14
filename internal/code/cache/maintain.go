package cache

import (
	"bytes"
	"cmp"
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/crewlet/crewlet/internal/code/gitproc"
)

// THE MULTI-PACK-INDEX's writers, in one file so the writer of an index is one
// file (TestNothingButSyncWritesACache's allowance holds them to it): the
// maintainer, and a full round's own index.
//
// THE MAINTAINER writes a multi-pack-index WITH its bitmap, and a
// commit-graph, for a repository whose packs have gathered — so a clone
// counts objects from the bitmap rather than walking history, and a walk
// reads commits from the graph rather than parsing each one. One goroutine,
// owned by Start, woken every MidxDebounce, kicked after a full round and by
// every run that ends; a pass STARTS a run for every due repository, most
// packs left uncovered first, each in a slot of its own in the pool's
// MAINTENANCE lane — half the node's cores, a free slot or none, never
// waiting — in every slot of it but one, which is kept for a person's
// verify, and as many as MaintenanceMemoryPerCore holds beside the largest
// run; it waits for none of them, so one repository's hours-long bitmap
// never holds another's index back. Background work never takes a person's
// slot. And never a step the node's memory cannot hold: every run in flight,
// the largest included, is held to the node's budget together with the
// round's indexes and Verify's checks beside them (memory.go), each step at a
// charge of its own — so a repository whose bitmap the node cannot hold is
// indexed WITHOUT one, and its graph written, rather than not at all, and
// one whose every step is past the budget is served from its packs' own
// indexes, said so once an incarnation, rather than killed by the kernel at
// every duty cycle. A repository the runs in flight leave no room for is the
// one the pass waits for: the first in its order held back for memory alone
// is given the room it needs, and nothing that would take it starts behind
// it, so the largest is never starved by a stream of small ones.
//
// A repository is DUE when it holds a pack and either no index with a bitmap
// covers its packs, MidxPacks of them have gathered beside it, or its
// commit-graph is missing (the two are written by two processes, and the
// second can be refused a slot the first had); when its packs have stood
// still for MidxDebounce — a burst of pushes lands before the index that
// covers it — or the oldest of them has waited MidxMaxWait, so a repository
// pushed to without pause is still indexed; and when its last run ended at
// least max(MidxMinInterval, MidxDutyCycle × that run's duration) ago — a
// tenth of a core's time a repository at most, however fast it is pushed to.
// One pack is enough: the bitmap and the graph are worth having over a
// history imported in one push, and an index over one pack is still a valid
// one.
//
// WHAT A DUE RUN WRITES is the PLAN the node's memory allows (plan): the
// bitmap where the budget holds it; where it does not, the index WITHOUT a
// bitmap, once no index covers the packs or enough have gathered outside one
// — MidxPacks beside an index without a bitmap, the cadence any index is kept
// at, and MidxRoundPacks beside a bitmap the node wrote while it could — and
// the graph after any index, or alone where it is missing. A bitmap written
// while it could be is KEPT that long because an index without one would
// delete it (git removes a bitmap that does not match the index it writes),
// and it saves a clone most of its walk — 14 s against 111 s over a gigabyte
// (MidxRoundPacks) — while each pack outside it costs a request a few
// hundredths of a millisecond (MidxPacks). It is GIVEN UP at MidxRoundPacks
// because a refused repository only grows, so the packs outside its bitmap
// would otherwise gather without bound, and past that count every lookup of
// what was pushed since searches more packs than this cache leaves outside an
// index anywhere else. A judgment call between a clone's walk and every
// request's lookups, which no single measurement settles.
//
// A RUN HOLDS A PIN, NEVER THE REPOSITORY'S ROUND. A bitmap over the largest
// repository the defaults admit is a walk of minutes to hours, and holding the
// round would hold every push and fetch of it for as long. git writes both
// files through its own lockfile and rename, so readers are unaffected. Two
// rules keep a run and a round apart instead: a run starts only on an entry
// with no round in flight, on an incarnation this process has read whole
// (one a previous process left may hold strays its first round will sweep);
// and a FULL round stops a run on its incarnation before its sweep — the
// sweep moves a running writer's lock and temporary files, and an index
// naming a pack the sweep removes fails every read of the repository. A run
// stopped so, or by Stop or a retire, is no run: what it costs the repository
// is the next run, never an interval.
//
// A FULL ROUND THAT LEAVES MANY PACKS INDEXES THEM BEFORE IT ANSWERS. One push
// is one pack and nothing compacts them, so a repository reaches thousands;
// without a multi-pack-index every object lookup binary-searches each pack's
// own .idx in turn, and the request a cold cache was synced for — a clone on a
// node that just joined, the first fetch after a restart — would be served by
// pack-objects searching every pack for every object. MANY is MidxRoundPacks
// (256), where the cold clone alone pays the index back, over 100 MB and over
// a gigabyte. Under that the index saves the cold request less than it costs
// it, so the round answers without one, and the maintainer's index follows
// once the packs have stood still and the runs ahead of it have ended — one
// at a time on four cores, so on a node that just joined, after every
// repository ahead of it in the queue. The round's index has NO BITMAP: it is
// a merge of the packs' own indexes, proportional to their objects, where a
// bitmap is a walk of the whole history no request should wait for. It runs
// in the READ lane, because a request waits on it — and it is held to the
// node's memory like every whole-repository process the cache starts: at
// IndexBytesPerObject an object of the snapshot's packs, with room for a
// transfer of the repository beside it (memory.go), against the budget the
// maintenance in flight shares, so a round over a repository of tens of
// millions of objects never writes an index of gigabytes on a node that
// cannot hold one. It never waits for that room either: an index the budget
// cannot hold beside what is in flight is one the round answers without.

// indexRound writes a multi-pack-index without a bitmap over exactly the
// snapshot's packs, for a full round, when the snapshot names MidxRoundPacks
// or more packs and the incarnation holds NO multi-pack-index file — one stat,
// never the state's count, which reads zero beside an index on disk when the
// state was unreadable. A round never writes over an index that exists: git
// deletes a bitmap that does not match the index it writes.
//
// IT RUNS BEFORE THE REFS, because the refs' peel reads every tag the round
// sets out of the packs, and without an index each lookup searches every
// pack's .idx in turn — the very cost the index removes: at 5 000 packs of one
// commit and one annotated tag each (BenchmarkPeel: git 2.43, the cat-file the
// peel runs over all 5 000 tags, six counts on four shared vCPUs) the peel
// took 0.65 s without the index and 0.23 s with it, for a 0.19 s write —
// though at a thousand such packs it saves less than the write costs, so the
// peel is no reason for MidxRoundPacks' count. Over exactly the snapshot's
// packs (--stdin-packs), so a stray the sweep is about to remove is never in
// it.
//
// Never a failure the round fails on: a busy lane or a failed write is
// logged, the lock file a killed write leaves removed, and the round answers
// without the index — slower, never wrong. So is an index the node's memory
// cannot hold (admitOther): past the budget alone, refused and said so once
// an incarnation (step index, as the maintainer's own would be); held back
// by the work in flight, said so and left to the maintainer. It reports
// whether it wrote.
func (c *Cache) indexRound(ctx context.Context, e *entry, inc *incarnation, snap snapshot) bool {
	packs := len(snap.packs)
	if packs < c.roundPacks {
		return false
	}
	packDir := filepath.Join(inc.dir, "objects", "pack")
	if _, err := os.Lstat(filepath.Join(packDir, "multi-pack-index")); !errors.Is(err, fs.ErrNotExist) {
		return false
	}
	var names bytes.Buffer
	var indexBytes, objects int64
	for _, p := range snap.packs {
		names.WriteString("pack-" + p.GitChecksum + ".idx\n")
		indexBytes += p.IndexBytes
		objects += p.Objects
	}
	mem := charge(objects, IndexBytesPerObject)
	switch c.maint.admitOther(mem) {
	case pastBudget:
		e.mu.Lock()
		var w refusalWalk
		w.note(e.id, inc, stepPlain, map[maintStep]int64{stepPlain: mem})
		e.mu.Unlock()
		for _, r := range w.fresh {
			c.logRefusal(r)
		}
		return false
	case heldBack:
		c.log.Info("code_cache_round_index_held", "repo", e.id, "packs", packs, "estimate", mem,
			"budget", c.maint.budgetNow())
		return false
	}
	defer c.maint.doneOther(mem)
	_, err := c.git.Run(ctx, gitproc.Cmd{
		Dir: inc.dir, Args: []string{"multi-pack-index", "write", "--stdin-packs", "--no-progress"},
		Config: c.ServeConfig(), Class: gitproc.ClassRead, Stdin: &names,
		Deadline: gitproc.PackWorkDeadline(indexBytes),
	})
	if err != nil {
		c.log.Warn("code_cache_round_index_skipped", "repo", e.id, "packs", packs, "error", err)
		lock := filepath.Join(packDir, "multi-pack-index.lock")
		if rerr := os.Remove(lock); rerr != nil && !errors.Is(rerr, fs.ErrNotExist) {
			c.log.Warn("code_cache_trash_failed", "path", lock, "error", rerr)
		}
		return false
	}
	meta, err := metaBytes(inc.dir)
	e.mu.Lock()
	// LastRun and Took stay the maintainer's: the round's index is no
	// maintenance run, and the bitmap the maintainer adds is due at once.
	inc.midx.Packs, inc.midx.Bitmap = packs, false
	if err == nil {
		c.setSize(inc, inc.diskBytes, meta)
	}
	e.mu.Unlock()
	if c.knobs != nil && c.knobs.roundIndexed != nil {
		c.knobs.roundIndexed(inc.dir)
	}
	return true
}

// metaBytes is the size of the maintainer's files in the incarnation at dir:
// the multi-pack-index's files and the commit-graph.
func metaBytes(dir string) (int64, error) {
	packDir := filepath.Join(dir, "objects", "pack")
	list, err := os.ReadDir(packDir)
	if err != nil {
		return 0, err
	}
	var total int64
	for _, d := range list {
		if !midxFile.MatchString(d.Name()) {
			continue
		}
		info, err := d.Info()
		if err != nil {
			return 0, err
		}
		total += info.Size()
	}
	if info, err := os.Lstat(filepath.Join(dir, "objects", "info", "commit-graph")); err == nil {
		total += info.Size()
	} else if !errors.Is(err, fs.ErrNotExist) {
		return 0, err
	}
	return total, nil
}

// maintRun is one maintenance run in flight on an incarnation: what stops it,
// and what says it has stopped — closed once its outcome is recorded, its
// lock files are gone and its pin is released.
type maintRun struct {
	e      *entry
	inc    *incarnation
	cancel context.CancelFunc
	done   chan struct{}
	// maintained: every step the run set out to take ran to its end. Read
	// once done is closed.
	maintained bool
}

// runPlan is what a run was started to do, fixed when it is started under
// the entry's mu (plan): the index it writes — with its bitmap, without one,
// or none — and whether it writes the commit-graph, the packs that index will
// cover, the bytes its deadline is sized by, the memory it was admitted at
// (maintFlight), and when it started on the cache's clock.
type runPlan struct {
	index     maintStep // stepBitmap, stepPlain or 0
	graph     bool
	covered   int
	disk      int64
	memory    int64
	startedAt time.Time
}

// steps reports whether the plan takes any step at all.
func (p runPlan) steps() bool { return p.index != 0 || p.graph }

// maintainer runs a pass at every tick and every kick until ctx ends or the
// cache stops. Between passes it is parked, holding nothing of the directory;
// the runs a pass starts go on without it. No pass runs at Start: nothing is
// maintained until a round of this process has read it whole, and that
// round's kick wakes it.
func (c *Cache) maintainer(ctx context.Context) {
	var ticks <-chan time.Time
	if c.knobs != nil {
		ticks = c.knobs.maintTicks
	}
	if ticks == nil {
		tick := time.NewTicker(MidxDebounce)
		defer tick.Stop()
		ticks = tick.C
	}
	for {
		select {
		case <-ctx.Done():
			return
		case <-c.life.Done():
			return
		case <-ticks:
		case <-c.maintKicks:
		}
		if !c.enter() {
			return
		}
		// A kick sent before this pass reads the entries is answered by it;
		// left pending, it would run a second pass over the same entries.
		select {
		case <-c.maintKicks:
		default:
		}
		c.maintainerPass(ctx)
		c.leave()
	}
}

// kickMaintainer wakes the maintainer without waiting for it.
func (c *Cache) kickMaintainer() {
	select {
	case c.maintKicks <- struct{}{}:
	default:
	}
}

// maintainerPass reads the node's memory again, then STARTS a run for every
// due repository, most packs left uncovered first, that the runs in flight
// leave room for (maintFlight.admit: every slot of the maintenance lane but
// one, the memory of the runs beside the largest within the allowance, and
// everything in flight within the node's budget), and answers the runs it
// started — waiting for none of them. A bitmap over the largest repository
// the defaults admit is a walk of hours (PackWorkDeadline), and a pass that
// waited for it would hold every other repository's index and graph back for
// as long, with the rest of the lane idle; so each run takes a slot of its
// own and kicks the maintainer as it ends, and the next due repository starts
// then.
//
// What each run takes is its PLAN (plan): every step the repository is due
// for whose charge the budget holds alone. A step past it is refused, every
// pass, and the pass's walk of the entries is what says so (noteRefusals) —
// every pass, whether or not the lane has room, so the gauge and the alarm
// follow the repositories rather than the lane.
//
// A repository whose plan fits the budget alone but not beside what is in
// flight is HELD, and the first one held in the pass's order is given the
// room it needs: nothing starts behind it that would leave less than its
// charge, or take the slot it needs, once what is in flight has ended
// (reservation). Without that, a stream of small repositories falling due
// would each fit beside the runs in flight and refill the room as it
// drained, and the repository first in the order — the one with the most
// packs uncovered, often the largest — would wait for as long as they came.
//
// The lane never waits: a run refused a slot at its first step — some other
// process holds it — never started, records nothing and kicks nothing, and
// the next tick asks again. The cache's own runs never refuse one another:
// there are fewer of them than the lane has slots, and each holds at most
// one at a time.
func (c *Cache) maintainerPass(ctx context.Context) []*maintRun {
	c.rereadMemory()
	budget := c.maint.budgetNow()
	most := maintenanceRuns(c.git.Slots(gitproc.ClassMaintenance))
	now := c.now()
	type candidate struct {
		e         *entry
		uncovered int
	}
	var (
		candidates []candidate
		walk       refusalWalk
	)
	c.mu.Lock()
	for _, e := range c.entries {
		e.mu.Lock()
		if c.holdsWork(e) {
			inc := e.current
			p, refused, charges := c.plan(inc, budget, now)
			walk.note(e.id, inc, refused, charges)
			if p.steps() && c.maintainable(e, now) {
				uncovered := inc.packs
				if inc.midx.Bitmap {
					uncovered -= inc.midx.Packs
				}
				candidates = append(candidates, candidate{e: e, uncovered: uncovered})
			}
		}
		e.mu.Unlock()
	}
	c.mu.Unlock()
	c.noteRefusals(walk)
	if c.maint.count() >= most {
		return nil
	}
	slices.SortFunc(candidates, func(a, b candidate) int {
		return cmp.Or(cmp.Compare(b.uncovered, a.uncovered), strings.Compare(a.e.id, b.e.id))
	})
	var (
		started []*maintRun
		held    reservation
	)
	for _, cand := range candidates {
		if ctx.Err() != nil || c.life.Err() != nil {
			break
		}
		m, verdict, mem := c.startMaintenance(ctx, cand.e, most, budget, held)
		switch verdict {
		case admitted:
			started = append(started, m)
		case noSlot:
			return started
		case heldBack:
			if !held.on {
				held = reservation{on: true, memory: mem}
			}
		}
	}
	return started
}

// maintenanceRuns is how many runs the maintainer keeps in flight at most in
// a maintenance lane of slots: EVERY SLOT BUT ONE. The one left is a
// person's: `crewlet code verify`, which every refused rebuild tells an
// operator to run, holds a slot of this lane from its admission to its fsck
// (Cache.AdmitVerify), and like every maintenance-class process it is refused
// rather than queued when no slot is free — so with every slot the
// maintainer's it would be answered "busy, run it again" for as long as the
// runs last, hours for the largest repositories. A lane of ONE slot (a node of one or two cores) is the
// maintainer's whole: holding it back would maintain nothing, and a verify
// there is refused while a run is in flight and answered between them.
func maintenanceRuns(slots int) int {
	if slots > 1 {
		return slots - 1
	}
	return slots
}

// maintFlight is the cache's whole-repository work in flight against the
// node's memory — the maintenance runs, each with its charge and a slot of
// the lane, and the round's indexes and Verify's checks beside them, which
// hold no slot of the maintainer's — and the budget all of it is held to
// together (memory.go). Its lock is a leaf: taken under an entry's and the
// memory's, never the other way round.
type maintFlight struct {
	mu     sync.Mutex
	budget int64
	runs   []int64
	others int64
}

// verdict is what admitting a charge decided.
type verdict uint8

const (
	// admitted: it is in flight now.
	admitted verdict = iota + 1
	// noSlot: the maintainer's runs hold every slot they may.
	noSlot
	// heldBack: it fits the budget alone, but not beside what is in flight.
	heldBack
	// pastBudget: it does not fit the budget even alone.
	pastBudget
	// notDue: nothing to run (startMaintenance's alone).
	notDue
)

// reservation is the room one pass keeps for the first repository it held
// back for memory: its charge, and a slot.
type reservation struct {
	on     bool
	memory int64
}

// setBudget sets what everything in flight is held to.
func (f *maintFlight) setBudget(n int64) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.budget = n
}

// budgetNow is what everything in flight is held to now.
func (f *maintFlight) budgetNow() int64 {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.budget
}

// admit records a maintenance run charged at mem as in flight when it may
// start, and says so — or why not:
//
//   - noSlot when most runs are in flight already, the slot held for a
//     reservation counted among them;
//   - pastBudget when mem alone is past the budget: a step the node's memory
//     cannot hold is killed at every duty cycle, and the kernel may kill the
//     engine in its place (plan never plans one, so this is a budget lowered
//     since);
//   - heldBack when it fits alone but not beside what is in flight — every
//     run, the round's indexes and Verify's checks, and a reservation, the
//     largest run included — or when the runs BESIDE THE LARGEST would come to
//     more than allowance together. The largest run is not charged against
//     the allowance (MaintenanceMemoryPerCore says why), but it is against
//     the budget, like everything.
func (f *maintFlight) admit(mem int64, most int, allowance int64, held reservation) verdict {
	f.mu.Lock()
	defer f.mu.Unlock()
	runs := len(f.runs)
	if held.on {
		runs++
	}
	if runs >= most {
		return noSlot
	}
	if mem > f.budget {
		return pastBudget
	}
	sum, largest := mem+held.memory, max(mem, held.memory)
	for _, r := range f.runs {
		sum += r
		largest = max(largest, r)
	}
	if sum+f.others > f.budget || (runs > 0 && sum-largest > allowance) {
		return heldBack
	}
	f.runs = append(f.runs, mem)
	return admitted
}

// done takes a run charged at mem out of flight.
func (f *maintFlight) done(mem int64) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if i := slices.Index(f.runs, mem); i >= 0 {
		f.runs = slices.Delete(f.runs, i, i+1)
	}
}

// admitOther records a process that holds no slot of the maintainer's — a
// round's index, Verify's check — charged at mem as in flight when the budget
// holds it beside everything in flight, and says so: admitted, pastBudget
// when mem alone is past the budget, heldBack when only what is in flight
// leaves no room. It never waits: a round's index is one a request waits on,
// and a check is a person's, refused rather than queued like every
// maintenance-class process.
func (f *maintFlight) admitOther(mem int64) verdict {
	f.mu.Lock()
	defer f.mu.Unlock()
	if mem > f.budget {
		return pastBudget
	}
	sum := mem + f.others
	for _, r := range f.runs {
		sum += r
	}
	if sum > f.budget {
		return heldBack
	}
	f.others += mem
	return admitted
}

// doneOther takes a process admitOther admitted at mem out of flight.
func (f *maintFlight) doneOther(mem int64) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.others -= mem
}

// count is how many runs are in flight.
func (f *maintFlight) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.runs)
}

// maintainable reports whether e's current incarnation is due for a run now;
// e.mu is held. See DUE above: never while a round is in flight or another run
// is, never while a check of the repository is admitted (its listing reads
// once whether a run is in flight, and a run started under it would leave
// files the listing reports as damage — holdMaintainer), never on an
// incarnation this process has not read whole, and
// never on one leaving the cache (wantsRun). WHAT the run takes, and whether
// the node's memory holds it, is plan's and admit's to say, not this.
func (c *Cache) maintainable(e *entry, now time.Time) bool {
	if !c.wantsRun(e) || e.running != nil || e.maint != nil || e.verifying > 0 {
		return false
	}
	inc := e.current
	settled := now.Sub(e.lastAdded) >= MidxDebounce ||
		(!e.uncoveredSince.IsZero() && now.Sub(e.uncoveredSince) >= MidxMaxWait)
	return settled && now.Sub(inc.midx.LastRun) >= max(MidxMinInterval, MidxDutyCycle*inc.midx.Took)
}

// holdsWork reports whether e's current incarnation is one the maintainer
// works on at all, whatever it holds of the maintainer's: read whole by this
// process, in service, and holding a pack. e.mu is held.
func (c *Cache) holdsWork(e *entry) bool {
	inc := e.current
	return !e.dead && inc != nil && !inc.retired && inc.converged && inc.packs >= 1
}

// wantsRun reports whether e's current incarnation wants a maintenance run
// for what it HOLDS, whenever one may start: one the maintainer works on
// (holdsWork) lacking the maintainer's bitmapped index or its commit-graph.
// maintainable adds WHEN — no round, run or verify in flight, the debounce
// and the interval passed — and plan WHAT, on the node's memory. e.mu is
// held.
func (c *Cache) wantsRun(e *entry) bool {
	return c.holdsWork(e) && (c.indexDue(e.current) || !e.current.midx.Graph)
}

// indexDue reports whether inc wants the maintainer's BITMAPPED index
// written: none on disk, one without its bitmap (a full round's own, or the
// maintainer's where the bitmap was refused), or MidxPacks packs gathered
// beside it. One that is current and has its bitmap is due for its
// commit-graph alone, if that is what it lacks.
func (c *Cache) indexDue(inc *incarnation) bool {
	return inc.midx.Packs == 0 || !inc.midx.Bitmap || inc.packs-inc.midx.Packs >= c.midxPacks
}

// plan decides what a run over inc would take on a budget of budget, and
// answers it with the steps the budget refused and every step's charge
// (charge: its own figure an object of the repository, and room beside it
// for one transfer of it). e.mu is held.
//
//   - THE INDEX. Where the bitmapped index is due (indexDue) and its charge
//     fits the budget, the bitmap. Where it is due and does not fit, the
//     bitmap is REFUSED, and the index WITHOUT one is written in its place
//     when its own charge fits and the packs call for it: none covered by an
//     index; MidxPacks gathered beside an index without a bitmap, the
//     cadence any index is kept at; or MidxRoundPacks beside a bitmap the
//     node wrote when it could — which a plain index would delete (git
//     removes a bitmap that does not match the index it writes), and which
//     saves a clone most of its walk, so it is kept until the packs outside
//     it cost lookups more than the cache serves unindexed anywhere else.
//   - THE GRAPH, after any index the plan writes, or alone where it is
//     missing, when its charge fits.
//
// A step due whose charge alone is past the budget is refused: never planned,
// on this node, at this memory. The plan's own charge is the largest of its
// steps', since they run one after the other.
func (c *Cache) plan(inc *incarnation, budget int64, now time.Time) (p runPlan, refused maintStep,
	charges map[maintStep]int64) {

	charges = map[maintStep]int64{
		stepBitmap: charge(inc.objects, BitmapBytesPerObject),
		stepPlain:  charge(inc.objects, IndexBytesPerObject),
		stepGraph:  charge(inc.objects, GraphBytesPerObject),
	}
	p = runPlan{covered: inc.packs, disk: inc.diskBytes, startedAt: now}
	take := func(s maintStep) bool {
		if charges[s] > budget {
			refused |= s
			return false
		}
		p.memory = max(p.memory, charges[s])
		return true
	}
	if c.indexDue(inc) && take(stepBitmap) {
		p.index = stepBitmap
	}
	if refused&stepBitmap != 0 {
		outside := inc.packs - inc.midx.Packs
		plainDue := inc.midx.Packs == 0 ||
			(inc.midx.Bitmap && outside >= c.roundPacks) ||
			(!inc.midx.Bitmap && outside >= c.midxPacks)
		if plainDue && take(stepPlain) {
			p.index = stepPlain
		}
	}
	if (p.index != 0 || !inc.midx.Graph) && take(stepGraph) {
		p.graph = true
	}
	return p, refused, charges
}

// startMaintenance starts one run of e's current incarnation on a goroutine
// of its own when it is still due, its plan takes a step, and the runs in
// flight leave it room (maintFlight.admit: at most most of them, held to
// budget, the pass's reservation held for) — answering the run, or nil and
// why not, with the plan's charge, which a pass holds a reservation of when
// the answer is heldBack.
//
// The run pins the incarnation and registers itself on the entry (e.maint)
// and as the cache's work in its directory (enter), so Stop waits for it and
// a pin keeps its directory; then it runs holding no lock of the entry, in a
// context the pass's ctx, the cache's lifetime, a full round's sweep
// (stopMaintenance) and a retire can each end.
func (c *Cache) startMaintenance(ctx context.Context, e *entry, most int, budget int64,
	held reservation) (*maintRun, verdict, int64) {

	if !c.enter() {
		return nil, notDue, 0
	}
	e.mu.Lock()
	now := c.now()
	if !c.maintainable(e, now) {
		e.mu.Unlock()
		c.leave()
		return nil, notDue, 0
	}
	inc := e.current
	plan, _, _ := c.plan(inc, budget, now)
	if !plan.steps() {
		e.mu.Unlock()
		c.leave()
		return nil, notDue, 0
	}
	if v := c.maint.admit(plan.memory, most, c.maintMemory, held); v != admitted {
		e.mu.Unlock()
		c.leave()
		return nil, v, plan.memory
	}
	runCtx, cancel := context.WithCancel(ctx)
	//nolint:contextcheck // The cache's lifetime ends the run too: Stop must end the git it started however long the pass's ctx lives.
	stop := context.AfterFunc(c.life, cancel)
	m := &maintRun{e: e, inc: inc, cancel: cancel, done: make(chan struct{})}
	e.maint = m
	inc.pins++
	e.mu.Unlock()
	go func() {
		defer c.leave()
		defer stop()
		defer cancel()
		c.maintain(runCtx, m, plan)
	}()
	return m, admitted, plan.memory
}

// maintain is one run's body, and records how it ended:
//
//   - REFUSED A SLOT AT ITS FIRST STEP, it never started: nothing is
//     recorded, and it kicks nothing — another process holds the lane, and
//     the next tick asks again.
//   - ENDED FROM OUTSIDE — the cache's lifetime (a deploy's Stop), a full
//     round's sweep, a retire — it is NO RUN: a step that finished is
//     recorded, since its file is on the disk, but neither when the run ended
//     nor how long it took, which would push the repository's next run back
//     by MidxDutyCycle times work that wrote nothing; and nothing is
//     persisted, because whoever stopped it writes the state next (a full
//     round), throws the incarnation away (a retire), or leaves the
//     directory to the next boot (Stop), whose first round re-measures it.
//   - Any other end is a run, whether it succeeded, failed or hit its
//     deadline: recorded with when it ended and how long it took, which the
//     duty cycle reads, and persisted. One whose commit-graph was refused a
//     slot after its index was written records the index and no graph,
//     which leaves the repository due for its graph alone once the interval
//     has passed — a refusal, not a failure. One that failed first removes
//     what a killed git leaves behind (clearMaintenanceDebris).
//
// An index written is recorded as what it is: with its bitmap, or without
// one — the plain index a run writes where the node's memory refused the
// bitmap, which leaves the repository wanting its bitmap (indexDue) and so
// still refused it, never quietly maintained.
//
// Whatever the end, the pin goes back and the run leaves the entry; a run
// that got a slot kicks the maintainer as it ends, so the repository due
// next takes the slot it frees.
func (c *Cache) maintain(ctx context.Context, m *maintRun, p runPlan) {
	e, inc := m.e, m.inc
	kick := false
	defer func() {
		e.mu.Lock()
		e.maint = nil
		gone := c.unpin(e, inc)
		e.mu.Unlock()
		c.trash(gone)
		c.maint.done(p.memory)
		close(m.done)
		if kick {
			c.kickMaintainer()
		}
	}()

	began := time.Now()
	out := c.runMaintenance(ctx, inc, p)
	took := time.Since(began)
	first := procGraph
	if p.index != 0 {
		first = procIndex
	}
	refused := errors.Is(out.err, gitproc.ErrBusy)
	if refused && out.step == first {
		return
	}
	kick = true
	stopped := out.err != nil && ctx.Err() != nil
	switch {
	case stopped:
		c.clearMaintenanceDebris(inc)
		c.log.Debug("code_cache_maintenance_cancelled", "repo", e.id, "step", out.step)
	case out.err != nil && !refused:
		c.clearMaintenanceDebris(inc)
		c.log.Warn("code_cache_maintenance_failed", "repo", e.id, "step", out.step, "error", out.err)
	}
	meta, merr := metaBytes(inc.dir)
	e.mu.Lock()
	if out.indexed {
		inc.midx.Packs, inc.midx.Bitmap = p.covered, p.index == stepBitmap
		// Every pack a delta added while the index was written arrived
		// after the run began: the oldest one the index leaves out is
		// no older than that. An incarnation replaced meanwhile says
		// nothing of the packs of the one that replaced it.
		switch {
		case e.current != inc:
		case inc.packs > p.covered:
			e.uncoveredSince = p.startedAt
		default:
			e.uncoveredSince = time.Time{}
		}
	}
	if out.graphed {
		inc.midx.Graph = true
	}
	if out.indexed || out.graphed {
		// A file no rebuild wrote: the incarnation no longer holds exactly
		// what the last rebuild attempt read, and one may make something
		// else of it — a bitmap the run wrote damaged among it (rebuild.go).
		inc.unmoved = false
	}
	if !stopped {
		inc.midx.LastRun, inc.midx.Took = c.now(), took
	}
	if merr == nil {
		c.setSize(inc, inc.diskBytes, meta)
	}
	retired := inc.retired
	e.mu.Unlock()
	if !stopped && !retired {
		// Without the round's fault point: the state is a handful of scalars
		// whichever writer wrote it last.
		if perr := c.persist(context.WithoutCancel(ctx), e, inc, false); perr != nil {
			c.log.Warn("code_cache_maintenance_failed", "repo", e.id, "step", "state", "error", perr)
		}
	}
	c.setGauges()
	if out.err != nil {
		return
	}
	m.maintained = true
	c.log.Debug("code_cache_maintained", "repo", e.id, "packs", p.covered, "index", p.index.String(),
		"graph", p.graph, "took", took)
}

// maintOutcome is how a run's steps ended: which it wrote, and the step it
// stopped at with that step's error ("" and nil when every step ran).
type maintOutcome struct {
	indexed, graphed bool
	step             string
	err              error
}

// The maintainer's two processes, by the name a knob a case holds a run with
// (knobs.maintaining) is told: each argument list's first.
const (
	procIndex = "multi-pack-index"
	procGraph = "commit-graph"
)

// The argument lists of a run's steps — the index with its bitmap and
// without one, and the graph — spelled as literals, which is how
// TestNothingButSyncWritesACache reads which git writer a file runs.
var (
	bitmapArgs = []string{"multi-pack-index", "write", "--bitmap", "--no-progress"}
	plainArgs  = []string{"multi-pack-index", "write", "--no-progress"}
	graphArgs  = []string{"commit-graph", "write", "--reachable", "--no-progress"}
)

// runMaintenance runs the plan's steps in inc — the index it writes, with its
// bitmap or without, and then the commit-graph, each when the plan takes it —
// each in the MAINTENANCE lane with the serve configuration and a deadline
// proportional to the repository.
func (c *Cache) runMaintenance(ctx context.Context, inc *incarnation, p runPlan) maintOutcome {
	var out maintOutcome
	var steps [][]string
	switch p.index {
	case stepBitmap:
		steps = append(steps, bitmapArgs)
	case stepPlain:
		steps = append(steps, plainArgs)
	}
	if p.graph {
		steps = append(steps, graphArgs)
	}
	for _, args := range steps {
		name := args[0]
		if c.knobs != nil && c.knobs.maintaining != nil {
			c.knobs.maintaining(ctx, name)
		}
		if err := ctx.Err(); err != nil {
			out.step, out.err = name, context.Cause(ctx)
			return out
		}
		if _, err := c.git.Run(ctx, gitproc.Cmd{
			Dir: inc.dir, Args: args, Config: c.ServeConfig(), Class: gitproc.ClassMaintenance,
			Deadline: gitproc.PackWorkDeadline(p.disk),
		}); err != nil {
			out.step, out.err = name, err
			return out
		}
		switch name {
		case procIndex:
			out.indexed = true
		case procGraph:
			out.graphed = true
		}
	}
	return out
}

// tmpBitmap matches the temporary file git writes a bitmap into
// (objects/pack/tmp_bitmap_ and six random characters, git 2.43) before
// renaming it into place. git makes it with mkstemp and registers no cleanup
// for it, so a writer killed while it writes the bitmap — the end of a run,
// where most of its time is — leaves it behind.
var tmpBitmap = regexp.MustCompile(`^tmp_bitmap_[A-Za-z0-9]+$`)

// The directories of an incarnation a maintenance run writes in, relative to
// it.
var (
	packRel = filepath.Join("objects", "pack")
	infoRel = filepath.Join("objects", "info")
)

// maintenanceWriting reports whether name, an entry of the incarnation's
// directory rel (packRel or infoRel), is a file a maintenance run holds only
// WHILE IT RUNS: the multi-pack-index's lock and the commit-graph's — each
// written whole and renamed over its file as the step ends — and a bitmap's
// temporary file. ONE LIST for the two readers that must agree on it: Verify,
// which admits them in an incarnation a run is writing (a writer's own files
// are no damage), and clearMaintenanceDebris, which removes them after a run
// that did not succeed (a killed git leaves them). Nothing else writes them,
// so outside a run each is damage the sweep removes.
func maintenanceWriting(rel, name string) bool {
	switch rel {
	case packRel:
		return name == "multi-pack-index.lock" || tmpBitmap.MatchString(name)
	case infoRel:
		return name == "commit-graph.lock"
	}
	return false
}

// clearMaintenanceDebris removes what a maintenance run that did not succeed
// may have left in inc (maintenanceWriting): the index's and the graph's
// lock files — git removes its own when it is asked to stop, and leaves them
// when it is killed — and a bitmap's temporary file. The run was the only
// writer of either in the incarnation, since no round runs a git writer
// beside it, so anything of theirs still there is its own. A removal that
// fails is logged; the incarnation's next full round sweeps whatever is
// outside its image.
func (c *Cache) clearMaintenanceDebris(inc *incarnation) {
	var paths []string
	for _, rel := range []string{packRel, infoRel} {
		list, err := os.ReadDir(filepath.Join(inc.dir, rel))
		if err != nil {
			continue
		}
		for _, d := range list {
			if maintenanceWriting(rel, d.Name()) {
				paths = append(paths, filepath.Join(inc.dir, rel, d.Name()))
			}
		}
	}
	for _, path := range paths {
		if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
			c.log.Warn("code_cache_trash_failed", "path", path, "error", err)
		}
	}
}

// stopMaintenance stops a maintenance run on inc, if one is in flight, and
// waits for it to end — bounded: the pool ends a cancelled git within its kill
// grace and reaps it, and the run then records its end and lets go. Holding no
// lock of the entry, since the run takes it to let go. A run on another
// incarnation of the entry is not inc's to stop.
func (c *Cache) stopMaintenance(e *entry, inc *incarnation) {
	e.mu.Lock()
	m := e.maint
	e.mu.Unlock()
	if m == nil || m.inc != inc {
		return
	}
	m.cancel()
	<-m.done
}
