package cache

import (
	"bytes"
	"cmp"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/crewlet/crewlet/internal/gitlet/gitletfault"
	"github.com/crewlet/crewlet/internal/gitlet/gitproc"
)

// THE DERIVED FILES' writers, in one file so the writer of an index or a graph
// is one file: the maintainer, and a full round's own index. Every write each
// runs is a declared one (derived.go's Writes: argument list, lane, deadline,
// memory charge and held files read from its row), and the writers gate holds
// each argument list's literal here to its row in both directions
// (TestNothingButSyncWritesACache).

// THE MAINTAINER writes a multi-pack-index WITH its bitmap, and a commit-graph,
// for a repository whose packs have gathered (package doc, "Background work
// never takes a person's slot", which also says how the node's memory bounds
// it). One goroutine, owned by Start, woken every MidxDebounce, after a full
// round, by every run that ends and by oweWake; a pass starts a run per due
// repository, each in a maintenance-lane slot of its own, waiting for none
// (maintainerPass).

// A repository is DUE (due) when it holds a pack and either no index with a
// bitmap covers its packs, MidxPacks have gathered beside it, or its
// commit-graph is missing (two processes: the second can be refused a slot the
// first had); when its packs have stood still for MidxDebounce or the oldest
// has waited MidxMaxWait, so one pushed without pause is still indexed; and
// when its last run ended max(MidxMinInterval, MidxDutyCycle × that run's
// duration) ago. One pack is enough: an index over one pack is valid, and the
// bitmap and graph pay over a history imported in one push.

// GRAPH FIRST. An incarnation with NO COMMIT-GRAPH (read whole for the first
// time, or its graph swept with a pack) is due for its graph AT ONCE: no
// debounce or interval, ahead of every index in the pass, on the graph's own
// charge, and the graph alone in any slot of the lane, the one kept for a
// person's verify included (startRun says why an index never is). Without a
// graph git walks by date, and a push of a commit dated before its parent walks
// every commit in each of its walks (internal/gitlet/host's scans,
// receive-pack's connectivity check; host's BenchmarkPushSmall step= arms),
// where the graph's write is one walk (BenchmarkMidx's step=graph). A
// graph-first step that failed waits for the ordinary rules (graphFailed), so
// it is not asked again every pass.

// AT ONCE IS THE NEXT PASS, and what wakes one is a full round that leaves such
// a copy, as it lets the repository go (Cache.run), and the last adoption that
// held a pass back from it (oweWake); a round that held a pass back starts the
// run itself as it ends (startOwed). A copy read whole with no pack takes its
// first by a delta or an adoption, neither of which wakes the maintainer: the
// next tick does, at most MidxDebounce later.

// INDEX REPAIR. A multi-pack-index a full round dropped as DAMAGED (a file of it
// failing its trailer, or a pack's .idx it may be built from reinstalled) is due
// AT ONCE (incarnation.indexRepair) and sorted with the graph-first runs, since
// every clone walks without a bitmap until it is written again. Unlike a graph
// it takes only a slot of the maintainer's own: a bitmap holds its slot for
// minutes, and the kept slot is a person's. Cleared by the index written, or by
// its step failing. A drop for a pack's REMOVAL is ordinary: a move of packs
// waits for the debounce.

// A FULL ROUND DOES NOT WRITE THE GRAPH ITSELF, as it writes its own index: the
// graph serves only a push dated before its parent, and written by the round it
// would slow every cold request (a clone, a fetch, a push) to spare that rare
// push a window in which it is slow and never refused (gitproc.HistoryWalk
// bounds every walk).

// EVERY GRAPH CARRIES CHANGED-PATH FILTERS (--changed-paths, version 2 under
// the serve configuration's commitGraph.changedPathsVersion), so `git log --
// <path>` and blame skip every commit that did not touch the path, and a write
// computes EVERY filter its commits lack, with no --max-new-filters: own mode's
// whole graph at its first write (later ones read them back), the tip afresh
// over its tail. A bound would not bound memory (a filter's diffs go through
// git's delta base cache, capped whatever the count: GraphBaseBytes), and
// readers would diff every unfiltered commit for thousands of runs
// (internal/gitlet/query's BenchmarkBlame and BenchmarkLogPath). The cost is
// the write's time, inside its deadline (gitproc.PackWorkDeadline).

// THE TIP. In CHAIN mode (an installed published chain, layers.go) the
// maintainer's one write is the node's TIP commit-graph layer over the TAIL,
// the packs no installed layer names: git's own split write (writeTip). It is
// DUE once the tail holds a pack and is not the tail the last write covered
// (tipWanted; the digest persists, so a restart owes nothing): at once where
// the copy has no tip (GRAPH FIRST), else on the ordinary cadence. It is
// written WHOLE each time over exactly the installed layers, never stacked on
// the old tip, which would grow without bound while the tail waits for its
// publication; a rewrite costs only the tail's commits (BenchmarkMidx's
// step=tip-graph). Charged by Write.charge (TipGraphBytesPerObject of the
// tail). A tip's failure costs speed, never an answer: the tail's commits are
// read from their packs.

// WHAT A DUE RUN WRITES is the plan the node's memory allows (plan): the bitmap
// where the budget holds it; where not, the index WITHOUT one once no index
// covers the packs, MidxPacks have gathered beside an index without a bitmap,
// or MidxRoundPacks beside a bitmap the node wrote while it could. That bitmap
// is KEPT so long because a plain index deletes it (git removes a bitmap not
// matching the index it writes) and it saves a clone most of its walk, while a
// pack outside it costs a request hundredths of a millisecond (MidxPacks). It
// is given up at MidxRoundPacks because a refused repository only grows: past
// it, lookups search more packs than the cache leaves unindexed anywhere else.
// A judgment call between a clone's walk and every request's lookups.

// A RUN HOLDS A PIN, NEVER THE REPOSITORY'S ROUND: a bitmap may take hours, and
// git writes both files by its own lockfile and rename, so readers are
// unaffected. A run starts only on an entry with no round in flight and an
// incarnation this process has read whole (one a previous process left may
// hold strays); a FULL round stops a run before its sweep, which moves a
// writer's lock and temporary files, and an index naming a swept pack fails
// every read. A run stopped so, or by Stop or a retire, is no run: it costs the
// next run, never an interval. A repository whose rounds leave no gap still
// gets its run (startOwed).

// A FULL ROUND THAT LEAVES MidxRoundPacks PACKS INDEXES THEM BEFORE IT ANSWERS
// (indexRound; package doc, "The byte path is shaped for S3").
// Its index has NO BITMAP (a merge of the packs' own indexes, where a bitmap is
// a walk of the whole history no request should wait for), runs in the READ
// lane because a request waits on it, and is held to the node's memory beside
// the maintenance in flight (admitOther): one the budget cannot hold now is one
// the round answers without.

// indexed reports whether inc holds an index of EITHER SHAPE on disk: the
// maintainer's whole multi-pack-index (DerivedMidx's main file), or an
// installed chain's chain file (DerivedLayers') — one Lstat each. git reads a
// whole index before a chain, and a whole write deletes every layer and the
// chain file, so a round writes no index beside either.
func (c *Cache) indexed(inc *incarnation) bool {
	for _, name := range []DerivedName{DerivedMidx, DerivedLayers} {
		rel := c.image.mainRel(name)
		if rel == "" {
			continue
		}
		if _, err := os.Lstat(filepath.Join(inc.dir, rel)); !errors.Is(err, fs.ErrNotExist) {
			return true
		}
	}
	return false
}

// indexRound writes a multi-pack-index without a bitmap over exactly the
// snapshot's packs (--stdin-packs: never a stray the sweep will remove), when
// they number MidxRoundPacks or more and inc holds no index of either shape
// (indexed: a stat each, never the state's count, which reads zero when the
// state was unreadable). It runs BEFORE THE REFS, whose peel reads every tag
// out of the packs (package doc, "Measured"). What it runs is DerivedMidx's
// round write (derived.go); when is this file's.
//
// Never a failure the round fails on: a busy lane, a failed write or an index
// the memory cannot hold (admitOther; past the budget alone, refused once an
// incarnation as the maintainer's plain index is) leaves the round answering
// without it, slower, never wrong. It reports whether it wrote.
func (c *Cache) indexRound(ctx context.Context, e *entry, inc *incarnation, snap snapshot) bool {
	// Over objects/pack's packs: an index never covers the dropping view,
	// which git reads beside it in a serving process alone.
	figures := snap.packFigures()
	packs := len(figures)
	if packs < c.roundPacks || c.indexed(inc) {
		return false
	}
	w := c.mw.round
	var names bytes.Buffer
	covers := Covers{Packs: packs}
	for _, cs := range slices.Sorted(maps.Keys(figures)) {
		p := figures[cs]
		names.WriteString("pack-" + cs + ".idx\n")
		covers.IndexBytes += p.indexBytes
		covers.Objects += p.objects
	}
	mem := w.charge(covers.Objects, covers.Objects)
	switch c.maint.admitOther(mem) {
	case pastBudget:
		// Refused as the maintainer's plain index would be: one line and one
		// count an incarnation.
		e.mu.Lock()
		var walk refusalWalk
		walk.note(&c.mw, e.id, inc, c.mw.plain, map[maintStep]int64{c.mw.plain: mem})
		e.mu.Unlock()
		for _, r := range walk.fresh {
			c.logRefusal(r)
		}
		return false
	case heldBack:
		c.log.Info("code_cache_round_index_held", "repo", e.id, "packs", packs, "estimate", mem,
			"budget", c.maint.budgetNow())
		return false
	}
	defer c.maint.doneOther(mem)
	before := c.image.ids(inc.dir)
	res, err := c.git.Run(ctx, gitproc.Cmd{
		Dir: inc.dir, Args: w.Args, Config: c.ServeConfig(), Class: w.Lane, Stdin: &names,
		Deadline: w.Deadline(covers),
	})
	if err != nil {
		c.log.Warn("code_cache_round_index_skipped", "repo", e.id, "packs", packs, "error", err)
		if !res.Began.IsZero() {
			c.clearWriting(inc, w)
		}
		return false
	}
	c.vouchWritten(e, inc, before)
	m, err := c.image.derivedSizes(inc.dir)
	e.mu.Lock()
	// The maintained clock stays the maintainer's: the round's index is no
	// maintenance run, and the bitmap the maintainer adds is due at once.
	c.made(inc, DerivedMidx, packs, w.Step)
	if err == nil {
		c.recordDerived(inc, m)
	}
	e.mu.Unlock()
	if c.knobs != nil && c.knobs.roundIndexed != nil {
		c.knobs.roundIndexed(inc.dir)
	}
	return true
}

// maintRun is one maintenance run in flight on an incarnation: what stops it,
// and what says it has stopped — closed once its outcome is recorded, its
// lock files are gone and its pin is released.
type maintRun struct {
	e      *entry
	inc    *incarnation
	cancel context.CancelFunc
	done   chan struct{}
	// writes is the declarations the run's steps write: what a drop of one
	// of them stops it for (stopWriting).
	writes derivedSet
	// maintained: every step the run set out to take ran to its end. Read
	// once done is closed.
	maintained bool
	// began (a time.Now() reading) and duty (runPlan.duty) are what a swap
	// that stops it records as the last run (swap.go's stopRun), since a run
	// ended from outside records none of its own.
	began time.Time
	duty  bool
}

// runPlan is what a run was started to do, fixed under the entry's mu (plan):
// its index write (with a bitmap, without, or none) and the commit-graph, or
// in chain mode the node's tip alone; what it covers (the packs, objects and
// bytes its charges and deadlines are sized by; the tail's for the tip); the
// memory it was admitted at, the graph's own charge, and its start on the
// cache's clock.
type runPlan struct {
	index maintStep // the maintainer's bitmap or plain write (maintWrites), or 0
	graph bool
	// tip: the node's tip commit-graph over the tail (THE TIP above), the
	// one step a run in chain mode takes; tipOver is the digest of the tail
	// it is written over (incarnation.tail).
	tip     bool
	tipOver tailDigest
	// first: the incarnation has no commit-graph — no tip, in chain mode —
	// so the graph is the run's first step, due at once (GRAPH FIRST above).
	first       bool
	covers      Covers
	memory      int64
	graphCharge int64
	startedAt   time.Time
}

// steps reports whether the plan takes any step at all.
func (p runPlan) steps() bool { return p.index != 0 || p.graph || p.tip }

// graphOnly is a graph-first plan without its index: what a run takes when no
// slot of the maintainer's own is free for the whole plan (the graph alone may
// take the one kept for a person), or only the graph's charge fits beside what
// is in flight (startRun).
func (p runPlan) graphOnly() runPlan {
	p.index, p.memory = 0, p.graphCharge
	return p
}

// duty reports whether the duty cycle counts the run (its end and length push
// the next run back). A graph-first run writing the graph alone is not: one
// walk of the commits, once an incarnation, which counted would hold the
// bitmap a clone counts from back by MidxMinInterval.
func (p runPlan) duty() bool { return !p.first || p.index != 0 }

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

// maintainerPass rereads the node's memory, then STARTS a run for every due
// repository (graph-first ones, then most packs uncovered first) that the runs
// in flight leave room for (maintFlight.admit), waiting for none: each holds a
// slot of its own and kicks the maintainer as it ends, so an hours-long bitmap
// holds nothing else back. Refusals are noted every pass, room or not
// (noteRefusals), so the gauge and the alarm follow the repositories.
//
// The first repository HELD BACK, for memory or by a round (whose end starts
// it, startOwed), keeps its place: nothing starts behind it that would take its
// charge or slot (reservation, standing until the next pass), or a stream of
// small repositories would starve it. A run refused a slot by another process
// never started; the next tick asks again.
func (c *Cache) maintainerPass(ctx context.Context) []*maintRun {
	c.rereadMemory()
	budget := c.maint.budgetNow()
	lane := c.git.Slots(gitproc.ClassMaintenance)
	most := maintenanceRuns(lane)
	now := c.now()
	type candidate struct {
		e         *entry
		first     bool
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
			p, refused, charges := c.plan(inc, budget, c.due(e, now))
			// A PUBLICATION THE NODE'S MEMORY REFUSED, judged again on
			// this pass's budget: a node given the memory in place clears
			// it here, before its maintainer next asks.
			if inc.publishRefused > budget {
				refused |= stepPublish
				charges[stepPublish] = inc.publishRefused
			}
			walk.note(&c.mw, e.id, inc, refused, charges)
			if p.steps() && !c.runnableButForARound(e) {
				c.oweWake(e)
			}
			if p.steps() && c.runnableButForARound(e) {
				uncovered := inc.packs
				switch {
				case len(inc.installed) > 0:
					// A chained copy's packs outside every index are its tail.
					uncovered = inc.tail.packs
				case inc.bitmapped():
					uncovered -= inc.covered()
				}
				// An index repair is sorted with the graph-first runs: the
				// copy it repairs has lost what it served clones with.
				first := p.first || (inc.indexRepair && p.index != 0)
				candidates = append(candidates, candidate{e: e, first: first, uncovered: uncovered})
			}
		}
		e.mu.Unlock()
	}
	c.mu.Unlock()
	c.noteRefusals(walk)
	if c.maint.count() >= lane {
		return nil
	}
	// GRAPH FIRST: every incarnation with no commit-graph ahead of every
	// index and bitmap, then most packs left uncovered first.
	slices.SortFunc(candidates, func(a, b candidate) int {
		if a.first != b.first {
			if a.first {
				return -1
			}
			return 1
		}
		return cmp.Or(cmp.Compare(b.uncovered, a.uncovered), strings.Compare(a.e.id, b.e.id))
	})
	var (
		started []*maintRun
		held    reservation
	)
	// THE ROOM KEPT STANDS UNTIL THE NEXT PASS: a compaction, the one run
	// admitted between passes, is admitted beside it (maintFlight.standing),
	// and a pass that held nothing back leaves none standing.
	defer func() { c.maint.setStanding(held) }()
	for _, cand := range candidates {
		if ctx.Err() != nil || c.life.Err() != nil {
			break
		}
		m, verdict, mem := c.startMaintenance(ctx, cand.e, most, lane, budget, held)
		switch verdict {
		case admitted:
			started = append(started, m)
		case noSlot:
			// The slots this candidate may take are taken, and every
			// candidate after it may take no more: a graph-first one any
			// slot of the lane for its graph alone, the rest the
			// maintainer's own.
			return started
		case heldBack:
			if !held.on {
				held = reservation{on: true, memory: mem, of: cand.e}
			}
		}
	}
	return started
}

// maintenanceRuns is how many runs the maintainer keeps in flight at most in a
// maintenance lane of slots: EVERY SLOT BUT ONE, kept for a person's `crewlet
// gitlet verify` (Cache.AdmitVerify), which is refused rather than queued when
// no slot is free and would otherwise be answered "busy" for hours. A lane of
// ONE slot is the maintainer's whole: holding it back would maintain nothing.
func maintenanceRuns(slots int) int {
	if slots > 1 {
		return slots - 1
	}
	return slots
}

// maintFlight is the cache's whole-repository work in flight against the
// node's memory — the maintenance runs, each with its charge and a slot of
// the lane (a compaction's build among them, Cache.AdmitCompaction), and the
// round's indexes and Verify's checks beside them, which hold no slot of the
// maintainer's — and the budget all of it is held to together (memory.go).
// Its lock is a leaf: taken under an entry's and the memory's, never the other
// way round.
type maintFlight struct {
	mu     sync.Mutex
	budget int64
	runs   []int64
	others int64
	// standing is the room the LATEST pass kept for the first repository it
	// held back (reservation), until the next pass sets its own: a compaction,
	// the one run admitted between passes, is admitted beside it, or it would
	// take the room promised for as long as compactions kept coming.
	standing reservation
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
// back, for memory or for a round: its charge, and a slot. of is that
// repository, whose own run is admitted in the room rather than beside it
// (maintFlight.standingBeside).
type reservation struct {
	on     bool
	memory int64
	of     *entry
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
//   - noSlot when most runs are in flight, a reservation's slot among them;
//   - pastBudget when mem alone is past the budget, which the kernel would
//     kill (plan never plans one, so the budget was lowered since);
//   - heldBack when it fits alone but not beside everything in flight and the
//     reservation, or the runs BESIDE THE LARGEST would exceed allowance (the
//     largest is exempt from the allowance, MaintenanceMemoryPerCore says why,
//     never from the budget).
func (f *maintFlight) admit(mem int64, most int, allowance int64, held reservation) verdict {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.admitLocked(mem, most, allowance, held)
}

// admitStanding is admit beside the room the latest pass kept standing
// (maintFlight.standing), read under the same lock that admits: a compaction's
// admission, which no pass makes.
func (f *maintFlight) admitStanding(mem int64, most int, allowance int64) verdict {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.admitLocked(mem, most, allowance, f.standing)
}

// setStanding leaves held standing until the next pass sets its own.
func (f *maintFlight) setStanding(held reservation) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.standing = held
}

// standingBeside is the room the latest pass kept standing, as a run of e is
// admitted between passes (startOwed): none where it was kept for e, which
// would otherwise be refused for its own room. That room goes here whatever
// the answer; the next pass keeps it again if e still needs it.
func (f *maintFlight) standingBeside(e *entry) reservation {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.standing.of == e {
		f.standing = reservation{}
	}
	return f.standing
}

// admitLocked is admit's rule; f.mu is held.
func (f *maintFlight) admitLocked(mem int64, most int, allowance int64, held reservation) verdict {
	runs := len(f.runs)
	if held.on {
		runs++
	}
	if runs >= most {
		return noSlot
	}
	if v := f.fitsLocked(mem, allowance, held); v != admitted {
		return v
	}
	f.runs = append(f.runs, mem)
	return admitted
}

// fitsLocked is admit's memory rule — pastBudget where mem is past the budget
// alone, heldBack where it does not fit beside everything in flight and held,
// admitted where it does — and nothing of its slots; f.mu is held. ONE RULE
// for a run admitted and a run's charge moved (recharge).
func (f *maintFlight) fitsLocked(mem int64, allowance int64, held reservation) verdict {
	if mem > f.budget {
		return pastBudget
	}
	runs := len(f.runs)
	if held.on {
		runs++
	}
	sum, largest := mem+held.memory, max(mem, held.memory)
	for _, r := range f.runs {
		sum += r
		largest = max(largest, r)
	}
	if sum+f.others > f.budget || (runs > 0 && sum-largest > allowance) {
		return heldBack
	}
	return admitted
}

// recharge holds the run in flight at *mem to next instead, keeping its place
// and slot (NO SLOT IS COUNTED): at once where next is no larger, else where
// fitsLocked holds next beside everything else in flight and the standing
// room. *mem is next once admitted; refused, the run keeps its charge. notDue
// answers a run no longer in flight.
func (f *maintFlight) recharge(mem *int64, next int64, allowance int64) verdict {
	f.mu.Lock()
	defer f.mu.Unlock()
	i := slices.Index(f.runs, *mem)
	if i < 0 {
		return notDue
	}
	f.runs = slices.Delete(f.runs, i, i+1)
	if next > *mem {
		if v := f.fitsLocked(next, allowance, f.standing); v != admitted {
			f.runs = append(f.runs, *mem)
			return v
		}
	}
	f.runs = append(f.runs, next)
	*mem = next
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

// admitOther records a process holding no slot of the maintainer's (a round's
// index, Verify's check) charged at mem as in flight when the budget holds it
// beside everything in flight: admitted, pastBudget when mem alone is past it,
// heldBack when what is in flight leaves no room. It never waits: a request
// waits on a round's index, and a person's check is refused, never queued.
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

// runnable reports whether a run may start on e's current incarnation now; e.mu
// is held. Never beside a round, another run or an admitted check (its listing
// reads once whether a run is in flight, and a run started under it leaves
// files the listing reports as damage: holdMaintainer), and only on what
// wantsRun accepts. WHEN is due's to say; WHAT, plan's and admit's.
func (c *Cache) runnable(e *entry) bool {
	return e.running == nil && c.runnableButForARound(e)
}

// runnableButForARound reports whether all that may keep a run from starting
// on e now is a round in flight: what makes it a repository a pass holds a place
// for in its order (maintainerPass); e.mu is held.
func (c *Cache) runnableButForARound(e *entry) bool {
	return c.wantsRun(e) && e.adopting == 0 && e.maint == nil && e.verifying == 0
}

// oweWake records that a pass was held back from e, due, if an adoption is what
// holds it: the last to let go wakes the maintainer (owedWake), so a run due at
// once — a graph, an index repair — is not left to the next tick; e.mu is held.
// A run in flight owes nothing here (its end kicks), and neither does a check
// (its release does). A round's debt is the run itself (startOwed).
func (c *Cache) oweWake(e *entry) {
	if e.adopting > 0 {
		e.maintainerOwed = true
	}
}

// owedWake reports whether the maintainer is owed a wake on e now — a pass
// was held back from it by an adoption (oweWake), or a full round converged it,
// and neither a round nor an adoption holds it any more — and takes the debt;
// e.mu is held.
func (c *Cache) owedWake(e *entry) bool {
	if !e.maintainerOwed || e.running != nil || e.adopting > 0 {
		return false
	}
	e.maintainerOwed = false
	return true
}

// startOwed starts the run a pass found e due for and a round held back
// (entry.runOwed), as that round ends and before the round queued behind it
// starts; e.mu is held. NEITHER A TICK NOR A WAKE IS ENOUGH: every
// linearizable read is a round, so a repository read faster than its rounds
// take has no gap, and one read nearly that fast has gaps a woken pass is too
// slow for; its graph and index would wait out its traffic. It costs the
// callers nothing (arithmetic under e.mu; the run holds a pin, so the queued
// round goes on beside it; one debt a pass, under the duty cycle), except where
// the queued round would stop it (queuedRoundStops): the debt then waits for
// that round's end. ONE TRY A DEBT: refused a slot or room here, it owes
// nothing further, as what holds them kicks the maintainer.
func (c *Cache) startOwed(e *entry) {
	if !e.runOwed || e.running != nil || c.queuedRoundStops(e) {
		return
	}
	e.runOwed = false
	lane := c.git.Slots(gitproc.ClassMaintenance)
	c.startRun(c.life, e, maintenanceRuns(lane), lane, c.maint.budgetNow(), c.maint.standingBeside(e))
}

// queuedRoundStops reports whether the round queued on e would end a run
// started now, its callers waiting for a git just begun: a rebuild, which
// replaces the copy, or one the copy's marks make full, whose sweep stops
// every run (apply). A full answer no mark asked for is the round's own to
// find, and stops the run as it stops any; e.mu is held.
func (c *Cache) queuedRoundStops(e *entry) bool {
	r := e.next
	return r != nil && (r.rebuild != "" || c.roundIsFull(e, e.current))
}

// due reports whether e's current incarnation's ORDINARY run is due now (DUE
// above), or its index is being repaired (INDEX REPAIR above); e.mu is held. A
// graph-first run is due whatever this says (plan).
func (c *Cache) due(e *entry, now time.Time) bool {
	inc := e.current
	if inc.indexRepair && len(inc.installed) == 0 {
		// Own mode's alone (dropped by sync.go's apply): a chained copy
		// writes no index.
		return true
	}
	settled := now.Sub(e.lastAdded) >= MidxDebounce ||
		(!e.uncoveredSince.IsZero() && now.Sub(e.uncoveredSince) >= MidxMaxWait)
	return settled && now.Sub(inc.maintained.LastRun) >= max(MidxMinInterval, MidxDutyCycle*inc.maintained.Took)
}

// holdsWork reports whether e's current incarnation is one the maintainer
// works on at all, whatever it holds of the maintainer's: read whole by this
// process, in service, and holding a pack. e.mu is held.
func (c *Cache) holdsWork(e *entry) bool {
	inc := e.current
	return !e.dead && inc != nil && !inc.retired && inc.converged && inc.packs >= 1
}

// wantsRun reports whether e's current incarnation wants a maintenance run for
// what it HOLDS: in OWN mode (holdsWork), the maintainer's bitmapped index or
// its commit-graph; in CHAIN mode, only its TIP over a moved tail (tipWanted),
// since a whole index or graph written beside the chain deletes it, and its
// index is its maintainer's to publish. runnable and due add WHEN, plan WHAT.
// e.mu is held.
func (c *Cache) wantsRun(e *entry) bool {
	if !c.holdsWork(e) {
		return false
	}
	if inc := e.current; len(inc.installed) > 0 {
		return tipWanted(inc)
	}
	return c.indexDue(e.current) || !e.current.graphed()
}

// tipWanted reports whether a chained inc wants its tip written: its tail
// holds a pack and is not the tail the last successful write covered, so a
// quiet repository is written nothing at any pass, round or restart. A digest
// compared (incarnation.tail), never a walk of the packs, since a pass reads
// every entry under the cache's lock. e.mu is held.
func tipWanted(inc *incarnation) bool {
	return inc.tail.packs > 0 && (!inc.tipWritten || inc.tipOver != inc.tail.digest)
}

// indexDue reports whether inc wants the maintainer's BITMAPPED index
// written: none on disk, one without its bitmap (a full round's own, or the
// maintainer's where the bitmap was refused), or MidxPacks packs gathered
// beside it. One that is current and has its bitmap is due for its
// commit-graph alone, if that is what it lacks.
func (c *Cache) indexDue(inc *incarnation) bool {
	return inc.covered() == 0 || !inc.bitmapped() || inc.packs-inc.covered() >= c.midxPacks
}

// plan decides what a run over inc would take on budget, answering it with the
// steps the budget refused and every step's charge (Write.charge over what it
// covers, the tail for the tip, with room for one transfer of the repository);
// e.mu is held. OWN mode (WHAT A DUE RUN WRITES above): the bitmap where it is
// due (indexDue) and fits, else the plain index where the packs call for it,
// then the graph, or the graph alone where it is missing. Those are the
// ORDINARY run's, taken only when due; a copy with no graph is planned a
// graph-first run whatever due says, unless graphFailed (GRAPH FIRST above).
// CHAIN mode plans no own-mode step: the tip alone (THE TIP above), at once
// where there is none unless tipFailed. A step whose charge alone is past the
// budget is refused, judged every pass, due or not; the plan's charge is its
// largest step's, as they run in turn.
func (c *Cache) plan(inc *incarnation, budget int64, due bool) (p runPlan, refused maintStep,
	charges map[maintStep]int64) {

	mw := &c.mw
	charges = make(map[maintStep]int64, len(mw.all))
	for i, w := range mw.all {
		s, over := maintStep(1)<<i, inc.objects
		if s == mw.tipGraph {
			// The tip covers the tail; the clone beside it, the repository.
			over = inc.tail.objects
		}
		charges[s] = w.charge(over, inc.objects)
	}
	p = runPlan{covers: Covers{Packs: inc.packs, Objects: inc.objects, DiskBytes: inc.diskBytes}, startedAt: c.now(),
		graphCharge: charges[mw.graph]}
	fits := func(s maintStep) bool {
		if charges[s] > budget {
			refused |= s
			return false
		}
		return true
	}
	if len(inc.installed) > 0 {
		p.covers = inc.tail.covers()
		if !tipWanted(inc) || !fits(mw.tipGraph) {
			return p, refused, charges
		}
		first := inc.tip.id == "" && !inc.tipFailed
		if due || first {
			p.tip, p.first, p.tipOver = true, first, inc.tail.digest
			p.memory = charges[mw.tipGraph]
		}
		return p, refused, charges
	}
	var index maintStep
	if c.indexDue(inc) && fits(mw.bitmap) {
		index = mw.bitmap
	}
	if refused&mw.bitmap != 0 {
		outside := inc.packs - inc.covered()
		plainDue := inc.covered() == 0 ||
			(inc.bitmapped() && outside >= c.roundPacks) ||
			(!inc.bitmapped() && outside >= c.midxPacks)
		if plainDue && fits(mw.plain) {
			index = mw.plain
		}
	}
	graph := (index != 0 || !inc.graphed()) && fits(mw.graph)
	p.first = graph && !inc.graphed() && !inc.graphFailed
	if due {
		p.index = index
	}
	p.graph = graph && (due || p.first)
	if p.index != 0 {
		p.memory = charges[p.index]
	}
	if p.graph {
		p.memory = max(p.memory, charges[mw.graph])
	}
	return p, refused, charges
}

// startMaintenance starts one run of e's current incarnation on a goroutine of
// its own when it is still runnable, its plan takes a step, and the runs in
// flight leave it room (maintFlight.admit: at most most runs, or lane for a
// graph alone, within budget, beside the pass's reservation), answering the
// run, or nil and why not, with the plan's charge, which a pass reserves when
// the answer is heldBack.
//
// The run pins the incarnation and registers on the entry (e.maint) and as the
// cache's work (enter), so Stop waits for it and the pin keeps its directory;
// it runs holding no entry lock, in a context the pass's ctx, the cache's
// lifetime, a full round's sweep (stopMaintenance) and a retire can each end.
func (c *Cache) startMaintenance(ctx context.Context, e *entry, most, lane int, budget int64,
	held reservation) (*maintRun, verdict, int64) {

	e.mu.Lock()
	defer e.mu.Unlock()
	return c.startRun(ctx, e, most, lane, budget, held)
}

// startRun is startMaintenance with e.mu held, for a pass and for the end of a
// round (startOwed), which holds it already.
//
// A REPOSITORY A ROUND HOLDS is answered heldBack with its plan's charge, the
// run owed to that round's end (entry.runOwed): the pass that asked keeps its
// place (maintainerPass).
func (c *Cache) startRun(ctx context.Context, e *entry, most, lane int, budget int64,
	held reservation) (*maintRun, verdict, int64) {

	if !c.runnableButForARound(e) {
		// HELD BACK SINCE THE PASS READ IT RUNNABLE, by an adoption that began
		// after its walk: the same debt as one the walk finds.
		c.oweWake(e)
		return nil, notDue, 0
	}
	inc := e.current
	plan, _, _ := c.plan(inc, budget, c.due(e, c.now()))
	if !plan.steps() {
		return nil, notDue, 0
	}
	if !c.runnable(e) {
		e.runOwed = true
		return nil, heldBack, plan.memory
	}
	// THE SLOT KEPT FOR A PERSON'S VERIFY is lent to a GRAPH ALONE (seconds,
	// where a bitmap is hours), never to a run going on to an index: a run
	// takes a slot per step in turn, so it would hold the person's slot
	// through its bitmap. A graph-first plan carrying an index is admitted at
	// the maintainer's limit; refused a slot or room there, its graph runs
	// alone at the lane's, and the index follows in a run of the maintainer's.
	alone := plan.first && plan.index == 0
	limit := most
	if alone {
		limit = lane
	}
	v := c.maint.admit(plan.memory, limit, c.maintMemory, held)
	if (v == noSlot || v == heldBack) && plan.first && !alone {
		plan = plan.graphOnly()
		v = c.maint.admit(plan.memory, lane, c.maintMemory, held)
	}
	if v != admitted {
		return nil, v, plan.memory
	}
	if !c.enter() {
		c.maint.done(plan.memory)
		return nil, notDue, 0
	}
	runCtx, cancel := context.WithCancel(ctx)
	//nolint:contextcheck // The cache's lifetime ends the run too: Stop must end the git it started however long the pass's ctx lives.
	stop := context.AfterFunc(c.life, cancel)
	m := &maintRun{e: e, inc: inc, cancel: cancel, done: make(chan struct{}),
		writes: c.mw.written(c.image, c.mw.steps(plan)), began: time.Now(), duty: plan.duty()}
	e.maint = m
	inc.pins++
	go func() {
		defer c.leave()
		defer stop()
		defer cancel()
		c.maintain(runCtx, m, plan)
	}()
	return m, admitted, plan.memory
}

// maintain is one run's body, and records how it ended. REFUSED A SLOT AT ITS
// FIRST STEP, it never started: nothing is recorded or kicked, and the next
// tick asks again. ENDED FROM OUTSIDE (Stop, a full round's sweep, a retire),
// it is NO RUN: a finished step is recorded, its file being on disk, but not
// the run's end or length, which would push the next run back for work that
// wrote nothing; nothing is persisted, as the stopper writes the state next (a
// SWAP's stop does count, swap.go's stopRun, or a repository compacted faster
// than its index runs would have every run stopped). Any other end is a run,
// recorded with its end and length for the duty cycle and persisted; a step
// refused a slot is no failure. An index is recorded as written, with or
// without its bitmap, so a refused bitmap stays wanted (indexDue).
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
	steps := c.mw.steps(p)
	out := c.runMaintenance(ctx, e, inc, p, steps)
	took := time.Since(began)
	refused := errors.Is(out.err, gitproc.ErrBusy)
	if refused && out.step == c.mw.all[c.mw.index(steps[0])].Writer.Subcommand {
		return
	}
	kick = true
	stopped := out.err != nil && ctx.Err() != nil
	if out.began {
		// A step whose git process began and did not succeed: clear what every
		// write of the plan holds, since only this run writes them here. A
		// step that never began wrote nothing, and the steps before it took
		// their locks with them as they succeeded.
		writes := make([]Write, 0, len(steps))
		for _, s := range steps {
			writes = append(writes, c.mw.all[c.mw.index(s)])
		}
		c.clearWriting(inc, writes...)
	}
	switch {
	case stopped:
		c.log.Debug("code_cache_maintenance_cancelled", "repo", e.id, "step", out.step)
	case out.err != nil && !refused:
		c.log.Warn("code_cache_maintenance_failed", "repo", e.id, "step", out.step, "error", out.err)
	}
	measured, merr := c.image.derivedSizes(inc.dir)
	e.mu.Lock()
	switch {
	case out.indexed:
		inc.indexRepair = false
	case inc.indexRepair && out.step == procIndex && out.err != nil && !refused && !stopped:
		// AN INDEX REPAIR THAT FAILED waits for the ordinary rules, as a
		// graph-first run that failed does: a write that keeps failing is
		// not asked again every pass.
		inc.indexRepair = false
	}
	if out.indexed {
		c.made(inc, DerivedMidx, p.covers.Packs, Step(c.mw.name(p.index)))
		// Every pack a delta added while the index was written arrived
		// after the run began: the oldest one the index leaves out is
		// no older than that. An incarnation replaced meanwhile says
		// nothing of the packs of the one that replaced it.
		switch {
		case e.current != inc:
		case inc.packs > p.covers.Packs:
			e.uncoveredSince = p.startedAt
		default:
			e.uncoveredSince = time.Time{}
		}
	}
	switch {
	case out.graphed:
		c.made(inc, DerivedGraph, p.covers.Packs, Step(c.mw.name(c.mw.graph)))
		inc.graphFailed = false
	case p.first && p.graph && out.step == procGraph && out.err != nil && !refused && !stopped:
		// A GRAPH-FIRST RUN THAT FAILED waits for the ordinary rules
		// (plan), so a step that keeps failing is not asked again every
		// pass.
		inc.graphFailed = true
	}
	switch {
	case out.tipped:
		// The tip is recorded by its write (writeTip). The tail it covers is
		// the one planned: a pack a delta added while it was written arrived
		// after the run began, and is the oldest the next tip covers.
		inc.tipFailed = false
		switch {
		case e.current != inc:
		case inc.tail.digest != p.tipOver:
			e.uncoveredSince = p.startedAt
		default:
			e.uncoveredSince = time.Time{}
		}
	case p.first && p.tip && out.err != nil && !refused && !stopped:
		// A TIP-FIRST RUN THAT FAILED waits for the ordinary rules, as a
		// graph-first one does.
		inc.tipFailed = true
	}
	if out.indexed || out.graphed || out.tipped {
		// A file no rebuild wrote: the incarnation no longer holds exactly
		// what the last rebuild attempt read, and one may make something
		// else of it — a bitmap the run wrote damaged among it (rebuild.go).
		inc.unmoved = false
	}
	if !stopped && (p.duty() || out.err != nil) {
		inc.maintained = maintainedState{LastRun: c.now(), Took: took}
	}
	if merr == nil {
		c.recordDerived(inc, measured)
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
	c.log.Debug("code_cache_maintained", "repo", e.id, "packs", p.covers.Packs, "index", c.mw.name(p.index),
		"graph", p.graph, "tip", p.tip, "first", p.first, "took", took)
}

// maintOutcome is how a run's steps ended: which it wrote, and the step it
// stopped at — its writer's subcommand — with that step's error ("" and nil
// when every step ran), and whether that step's git process began (what the
// run's writes hold may then be left, clearWriting).
type maintOutcome struct {
	indexed, graphed, tipped bool
	step                     string
	err                      error
	began                    bool
}

// The maintainer's two processes, by the name a knob a case holds a run with
// (knobs.maintaining) is told: each declared write's Writer.Subcommand, and
// the first of its argument list.
const (
	procIndex = "multi-pack-index"
	procGraph = "commit-graph"
)

// maintainFile is this file as the declarations name their writer
// (Writer.File): the writers gate holds every write declared here to this
// file's allowance rows.
const maintainFile = "internal/gitlet/cache/maintain.go"

// The declared writes' argument lists (derived.go), spelled as literals in the
// file that runs them, which is how TestNothingButSyncWritesACache reads which
// git writer a file runs. Both graphs compute every filter their commits lack
// (EVERY GRAPH CARRIES CHANGED-PATH FILTERS above;
// TestEveryGraphWriteComputesEveryFilterAndIsChargedItsTrees). The tip's split
// write never merges (--split=no-merge): merging would write the published
// layers again into a layer of the node's own.
var (
	roundIndexArgs = []string{"multi-pack-index", "write", "--stdin-packs", "--no-progress"}
	bitmapArgs     = []string{"multi-pack-index", "write", "--bitmap", "--no-progress"}
	plainArgs      = []string{"multi-pack-index", "write", "--no-progress"}
	graphArgs      = []string{"commit-graph", "write", "--reachable", "--changed-paths", "--no-progress"}
	tipGraphArgs   = []string{"commit-graph", "write", "--reachable", "--split=no-merge", "--changed-paths",
		"--no-progress"}
)

// maintWrites is the maintainer's declared writes (every Write whose Trigger
// is TriggerMaintainer, in declaration order, a maintStep's bit i being
// all[i]), and the five the plan and a full round name by what they are. No
// step's name, charge, lane, deadline or argument list is spelled outside
// derived.go; this file says when each runs (plan).
type maintWrites struct {
	all []Write
	// of is the declaration each of all is a write of.
	of []DerivedName
	// bitmap and plain are DerivedMidx's maintainer writes of StepBitmap and
	// StepIndex, graph DerivedGraph's of StepGraph, and tipGraph
	// DerivedGraphLayers' of StepTipGraph, as their bits.
	bitmap, plain, graph, tipGraph maintStep
	// round is DerivedMidx's round write (indexRound).
	round Write
}

// maxMaintWrites is how many maintainer writes a maintStep holds.
const maxMaintWrites = 8

// newMaintWrites reads the maintainer's writes out of the image's
// declarations, and refuses declarations that lack one the plan takes.
func newMaintWrites(im *image) (maintWrites, error) {
	var mw maintWrites
	roundFound := false
	for _, d := range im.decls {
		for _, w := range d.Writes {
			switch {
			case w.Trigger == TriggerMaintainer:
				mw.all = append(mw.all, w)
				mw.of = append(mw.of, d.Name)
			case d.Name == DerivedMidx && w.Trigger == TriggerRound && w.Step == StepIndex:
				mw.round, roundFound = w, true
			}
		}
	}
	if len(mw.all) > maxMaintWrites {
		return mw, fmt.Errorf("%d maintainer writes are declared, and a plan's set of them holds %d", len(mw.all),
			maxMaintWrites)
	}
	bit := func(name DerivedName, step Step) maintStep {
		for i, w := range mw.all {
			if mw.of[i] == name && w.Step == step {
				return maintStep(1) << i
			}
		}
		return 0
	}
	mw.bitmap, mw.plain, mw.graph = bit(DerivedMidx, StepBitmap), bit(DerivedMidx, StepIndex), bit(DerivedGraph,
		StepGraph)
	mw.tipGraph = bit(DerivedGraphLayers, StepTipGraph)
	if mw.bitmap == 0 || mw.plain == 0 || mw.graph == 0 || mw.tipGraph == 0 || !roundFound {
		return mw, fmt.Errorf("the maintainer's plan takes %s's maintainer writes of steps %s and %s, %s's of step "+
			"%s, %s's of step %s and %s's round write of step %s, and the declarations lack one of them",
			DerivedMidx, StepBitmap, StepIndex, DerivedGraph, StepGraph, DerivedGraphLayers, StepTipGraph,
			DerivedMidx, StepIndex)
	}
	return mw, nil
}

// index is the position in all of the one write s names, or -1.
func (mw *maintWrites) index(s maintStep) int {
	for i := range mw.all {
		if s == maintStep(1)<<i {
			return i
		}
	}
	return -1
}

// name is the Step of the one write s names, "" for none: what a line and a
// count's `step` attribute say.
func (mw *maintWrites) name(s maintStep) string {
	if i := mw.index(s); i >= 0 {
		return string(mw.all[i].Step)
	}
	return ""
}

// steps is the writes a run of p takes, in order, as their bits: the graph
// first on a graph-first run, else after the index; and the tip, the one step
// of a run in chain mode.
func (mw *maintWrites) steps(p runPlan) []maintStep {
	var steps []maintStep
	if p.graph && p.first {
		steps = append(steps, mw.graph)
	}
	if p.index != 0 {
		steps = append(steps, p.index)
	}
	if p.graph && !p.first {
		steps = append(steps, mw.graph)
	}
	if p.tip {
		steps = append(steps, mw.tipGraph)
	}
	return steps
}

// written is the declarations steps are writes of, in im.
func (mw *maintWrites) written(im *image, steps []maintStep) derivedSet {
	var s derivedSet
	for _, step := range steps {
		if i := mw.index(step); i >= 0 {
			s |= im.set(mw.of[i])
		}
	}
	return s
}

// runMaintenance runs the steps of p in inc, in order, each a declared write:
// its argument list in its lane with the serve configuration, under its
// deadline over what the run covers — the node's tip by its own steps
// (writeTip). What a step that succeeds writes is vouched (check.go): this
// process wrote it, so no check of this process hashes it again.
func (c *Cache) runMaintenance(ctx context.Context, e *entry, inc *incarnation, p runPlan,
	steps []maintStep) maintOutcome {

	var out maintOutcome
	for _, s := range steps {
		i := c.mw.index(s)
		w := c.mw.all[i]
		name := w.Writer.Subcommand
		if c.knobs != nil && c.knobs.maintaining != nil {
			c.knobs.maintaining(ctx, name)
		}
		if c.faults != nil {
			if err := c.faults.Hit(ctx, gitletfault.MaintainBeforeStep, 0); err != nil {
				out.step, out.err = name, err
				return out
			}
		}
		if err := ctx.Err(); err != nil {
			out.step, out.err = name, context.Cause(ctx)
			return out
		}
		if s == c.mw.tipGraph {
			began, err := c.writeTip(ctx, e, inc, w, p)
			if err != nil {
				out.step, out.err, out.began = name, err, began
				return out
			}
			out.tipped = true
			continue
		}
		before := c.image.ids(inc.dir)
		res, err := c.git.Run(ctx, gitproc.Cmd{
			Dir: inc.dir, Args: w.Args, Config: c.ServeConfig(), Class: w.Lane, Deadline: w.Deadline(p.covers),
		})
		if err != nil {
			out.step, out.err, out.began = name, err, !res.Began.IsZero()
			return out
		}
		c.vouchWritten(e, inc, before)
		switch c.mw.of[i] {
		case DerivedMidx:
			out.indexed = true
		case DerivedGraph:
			out.graphed = true
		}
	}
	return out
}

// writeTip is the node's TIP write over a chained inc and the tail p planned
// (THE TIP above), answering whether its git began and how it failed. It
// RESERVES a slot first, so a full lane refuses it before the copy moves; then
// writes commit-graph-chain naming EXACTLY the installed graph layers, so
// git's split write makes a layer of every commit they lack rather than one
// stacked on the old tip, whose file git expires (a reader then told "unable
// to find all commit-graph files" reads the layers below: no damage, gitproc's
// format table says); runs the write; reads the chain back (readTip); and
// records it (recordChains) whatever git did, the tail as covered (tipOver)
// only on success. Marked in flight (chainWrite), so a check or scrub across
// it gives the chain file no verdict. No entry lock is held: a cut or a
// placement stops the run first (dropFor, stopRun, stopMaintenance).
func (c *Cache) writeTip(ctx context.Context, e *entry, inc *incarnation, w Write, p runPlan) (bool, error) {
	slot, err := c.git.Reserve()
	if err != nil {
		return false, err
	}
	defer slot.Release()
	e.mu.Lock()
	_, layers := chainIDs(inc.installed)
	ended := chainWrite(inc)
	e.mu.Unlock()
	before := c.image.ids(inc.dir)
	// Written unsynced: it names nothing new — every layer it lists is a file
	// of the installed chain, durable since its placement — and a crash that
	// loses it leaves a chain file the boot's round holds to the record and
	// writes again (planChain).
	err = c.writeChain(inc.dir, graphChainRel, layers, unsynced)
	var res gitproc.Result
	if err == nil {
		res, err = slot.Run(ctx, gitproc.Cmd{
			Dir: inc.dir, Args: w.Args, Config: c.ServeConfig(), Class: w.Lane, Deadline: w.Deadline(p.covers),
		})
	}
	tip := readTip(inc.dir, layers)
	if err == nil {
		c.vouchWritten(e, inc, before)
	}
	e.mu.Lock()
	inc.tip = tip
	inc.tipOver, inc.tipWritten = p.tipOver, err == nil
	c.recordChains(inc)
	ended()
	e.mu.Unlock()
	return !res.Began.IsZero(), err
}

// readTip is the tip the commit-graph-chain of the incarnation at dir names
// above layers, the installed commit-graph layers it was written over: its one
// line past them, with that layer's bytes — or none, where it lists no more
// than them, lists them otherwise, or names a file that is not there.
func readTip(dir string, layers []string) tipLayer {
	raw, err := os.ReadFile(filepath.Join(dir, graphChainRel))
	if err != nil {
		return tipLayer{}
	}
	lines := strings.Fields(string(raw))
	if len(lines) != len(layers)+1 || !slices.Equal(lines[:len(layers)], layers) {
		return tipLayer{}
	}
	id := lines[len(layers)]
	info, err := os.Lstat(filepath.Join(dir, tipRel(id)))
	if err != nil || !info.Mode().IsRegular() || !graphLayerName().MatchString(filepath.Base(tipRel(id))) {
		return tipLayer{}
	}
	return tipLayer{id: id, bytes: info.Size()}
}

// clearWriting removes what a run of writes whose git process began and did
// not succeed may have left in inc: every file each holds while it runs
// (Write.Holds), a lock git leaves when killed and a bitmap's temporary file it
// leaves either way. No round runs a git writer beside a run, so anything of
// theirs still there is the run's own. A literal name (a lock) is removed BY
// NAME, and a directory is listed at most once, for the patterns only:
// objects/pack holds three files a pack, and a full round stopping the run
// waits on the listing. A failed removal is logged; the next full round sweeps
// whatever is outside the image.
func (c *Cache) clearWriting(inc *incarnation, writes ...Write) {
	named, listed := clearPlan(writes)
	paths := make([]string, 0, len(named))
	for _, rel := range named {
		paths = append(paths, filepath.Join(inc.dir, rel))
	}
	for rel, patterns := range listed {
		// A directory that cannot be read holds nothing of the run's this
		// can remove; the next full round's sweep can.
		list, _ := os.ReadDir(filepath.Join(inc.dir, rel))
		for _, d := range list {
			if slices.ContainsFunc(patterns, func(re *regexp.Regexp) bool { return re.MatchString(d.Name()) }) {
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

// clearPlan is how clearWriting clears what writes hold: the held names that
// are whole literals, by their path relative to the incarnation, each once —
// removed by name — and, by directory, the patterns that are not, each
// directory listed once for all of them.
func clearPlan(writes []Write) (named []string, listed map[string][]*regexp.Regexp) {
	listed = map[string][]*regexp.Regexp{}
	for _, w := range writes {
		for _, p := range w.Holds {
			rel := filepath.FromSlash(p.Dir)
			if name, whole := p.Name.LiteralPrefix(); whole {
				if path := filepath.Join(rel, name); !slices.Contains(named, path) {
					named = append(named, path)
				}
				continue
			}
			if !slices.ContainsFunc(listed[rel], func(re *regexp.Regexp) bool { return re.String() == p.Name.String() }) {
				listed[rel] = append(listed[rel], p.Name)
			}
		}
	}
	return named, listed
}

// stopMaintenance stops a maintenance run on inc, if one is in flight, and
// waits for it to end — bounded: the pool ends a cancelled git within its kill
// grace and reaps it, and the run then records its end and lets go. Holding no
// lock of the entry, since the run takes it to let go. A run on another
// incarnation of the entry is not inc's to stop.
func (c *Cache) stopMaintenance(e *entry, inc *incarnation) {
	c.stopWriting(e, inc, c.image.all())
}

// stopWriting is stopMaintenance for a run on inc that writes a declaration
// of s, and none other: a drop of s answers a change of the files those
// declarations are built over, and only a run writing one of them would
// record a file built over the inputs before the change as current after it.
func (c *Cache) stopWriting(e *entry, inc *incarnation, s derivedSet) {
	e.mu.Lock()
	m := e.maint
	e.mu.Unlock()
	if m == nil || m.inc != inc || m.writes&s == 0 {
		return
	}
	m.cancel()
	<-m.done
}
