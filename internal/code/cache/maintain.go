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

	"github.com/crewlet/crewlet/internal/code/gitproc"
)

// THE DERIVED FILES' writers, in one file so the writer of an index or a graph
// is one file: the maintainer, and a full round's own index. Every write each
// runs is a declared one (derived.go's Writes) — its argument list, lane,
// deadline and memory charge read from the declaration's row, its files
// owned and held by the declaration — and the writers gate holds each
// argument list's literal here to its row in both directions
// (TestNothingButSyncWritesACache).
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
// GRAPH FIRST. An incarnation with NO COMMIT-GRAPH — read whole for the first
// time (a cold sync, a rebuild, a node that just joined), or its graph swept
// with a pack — is due for its graph AT ONCE: no debounce and no interval,
// ahead of every index and bitmap in the pass, admitted on the graph's own
// charge (GraphBytesPerObject) beside the runs in flight, and — for the graph
// ALONE — in any slot of the lane, the one kept for a person's verify
// included. Without a graph every walk git makes over the repository goes by
// date, and a push of a commit dated before its parent — a person's skewed
// clock — walks every commit in each of its walks (internal/code/host's scans,
// receive-pack's connectivity check): 5.0–5.5 µs a commit, 2.3–2.5 s a walk
// over a gigabyte's history (internal/code/host's BenchmarkPushSmall step=
// arms). That window was the maintainer's queue, which on a node that just
// joined is every other repository's bitmap — hours. It is now the graph's own
// write, one walk of the commits: 3.4 s over a gigabyte of source history
// (BenchmarkMidx's step=graph, 2.97 million objects). ON A BUSY LANE it is no
// longer: the maintainer keeps to every slot but one, and a graph alone may
// take that one, so a graph waits only for other graphs, seconds each, or —
// where a person's verify holds the last slot beside a run in every other, or
// on a lane of one slot — for the sooner of those to end. Where the ordinary
// run is due as well (its packs stood still before the run started — every
// graph-less copy after a restart, whose first full round adds no packs), the
// run writes the graph and then the index, but only in a slot of the
// maintainer's own: a run takes a slot for each of its steps in turn, and lent
// the kept slot it would hold it through the bitmap after the graph — hours on
// a large repository, every verify refused and every later graph-less copy
// waiting behind it. Where no slot of the maintainer's is free, or the index's
// charge leaves no room beside what is in flight, the run writes the graph
// alone (runPlan.graphOnly), in any slot, and the index follows in a run of
// the maintainer's own (startMaintenance). A graph-only run is no run the duty
// cycle counts (runPlan.duty), so the bitmap a clone counts from is not held
// back by it. One whose graph step fails waits for the ordinary rules
// (graphFailed), so a step that keeps failing is not asked again every pass.
//
// INDEX REPAIR. A multi-pack-index a full round dropped as DAMAGED — its
// check found a file of it failing its trailer, or one the index may have
// been built from (a pack's .idx) reinstalled — is due AT ONCE as a graph
// is: no debounce and no interval (incarnation.indexRepair), sorted with the
// graph-first runs, since every clone of the copy walks without a bitmap
// until it is written again, and nothing but a push would otherwise make it
// due before the interval. Unlike a graph it is admitted only in a slot of
// the maintainer's own: a bitmap holds its slot for minutes, and the kept
// slot is a person's. Cleared by the index written; one whose index step
// fails waits for the ordinary rules, as a graph-first run's does. A drop
// for a pack's REMOVAL is ordinary and asks nothing at once: the packs moved,
// and the ordinary debounce is what a move of packs waits for.
//
// A FULL ROUND DOES NOT WRITE THE GRAPH ITSELF, as it writes its own index
// past MidxRoundPacks: the round's index is what the request it was synced
// for is served from, and pays that request back; the graph serves only a
// push dated before its parent, and written by the round it would put 3.4 s
// a gigabyte on every cold request — a clone, a fetch, a push — to spare the
// rare skewed push a window of the same length, during which that push is
// slow and never refused (gitproc.HistoryWalk bounds every walk by the
// history as well as the push).
//
// EVERY GRAPH CARRIES CHANGED-PATH FILTERS (--changed-paths, version 2 under
// the serve configuration's commitGraph.changedPathsVersion): a `git log --
// <path>` or a blame then skips every commit whose filter says the path did
// not change rather than diffing it — 0.044 s with them against 0.399 s with
// no commit-graph, over a synthetic history of 40 000 commits on git 2.56.0 —
// and a write computes at most MaxNewFilters new ones (--max-new-filters), so
// what one write spends on filters is bounded whatever the history it walks:
// own mode's whole graph keeps every filter the graph it replaces holds, and
// the next write computes more; the tip computes its own afresh at each
// write (constants.go says what that leaves of a long tail).
//
// THE TIP. In CHAIN mode — an installed published chain (layers.go) — the
// maintainer writes no own-mode file; its one write is the node's TIP
// commit-graph layer over the TAIL, the packs pushed since the last
// publication, which no installed layer names and whose commits no
// installed commit-graph layer holds: git's own split write (`commit-graph
// write --reachable --split=no-merge`), which reads every layer beneath it
// and writes a layer of just the commits they lack. It is DUE once the tail
// holds a pack and is no longer the tail the last write covered
// (incarnation.tipOver, the tail's digest, persisted with the tip, so a
// restart or a full round over the same packs owes it nothing): at once
// where the copy has no tip — GRAPH FIRST, in any slot of the lane — and
// otherwise by the ordinary cadence above. It is written WHOLE each time,
// over the whole tail and exactly the installed layers: the chain file is
// written first naming exactly those (writeTip), so the old tip is off the
// chain git writes over and its file is expired by git's own write, rather
// than a new tip stacked on the old one — a stack would grow without bound
// while the tail waits for its publication — and a whole rewrite costs only
// the tail's commits (0.29 s over 5 000 with every filter, 0.04 s at 100, on
// git 2.56.0), charged at TipGraphBytesPerObject an object of the tail with
// room for a clone of the whole repository beside it (chargeOver). A tip's
// failure costs speed and never an answer: the tail's commits are read from
// their packs, as they are before the tip is written.
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
// is one pack until the repository's maintainer merges them
// (internal/code/compact), so a repository may reach thousands
// (code.MaxPacksPerRepo) between merges; without a multi-pack-index every
// object lookup binary-searches each pack's own .idx in turn, and the request
// a cold cache was synced for — a clone on a node that just joined, the first
// fetch after a restart — would be served by pack-objects searching every
// pack for every object. MANY is MidxRoundPacks
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

// indexed reports whether inc holds an index of EITHER SHAPE on disk: the
// maintainer's whole multi-pack-index (DerivedMidx's main file), or an
// installed chain's chain file (DerivedLayers') — one Lstat each. git reads a
// whole index before a chain, and a whole write deletes every layer and the
// chain file, so a round writes no index beside either: the guard on the
// whole index alone let a full round past MidxRoundPacks delete a chain it
// had just installed.
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
// snapshot's packs, for a full round, when the snapshot names MidxRoundPacks
// or more packs and the incarnation holds NO index of either shape (indexed) —
// a stat each, never the state's count, which reads zero beside an index on
// disk when the state was unreadable. A round never writes over an index that
// exists: git deletes a bitmap that does not match the index it writes, and a
// whole write deletes every layer of an installed chain and its chain file
// (layers.go). What it runs, in which lane, under which deadline and at which
// charge is DerivedMidx's round write (derived.go); when is this file's.
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
// logged, the files a killed write leaves removed (clearWriting, only when
// its process began: a refused slot wrote nothing), and the
// round answers without the index — slower, never wrong. So is an index the
// node's memory cannot hold (admitOther): past the budget alone, refused and
// said so once an incarnation (step index, as the maintainer's own would be);
// held back by the work in flight, said so and left to the maintainer. It
// reports whether it wrote.
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
	mem := chargeOver(covers.Objects, w.BytesPerObject, covers.Objects)
	switch c.maint.admitOther(mem) {
	case pastBudget:
		// Refused as the maintainer's own index without a bitmap would be:
		// one step, one line, one count an incarnation.
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
	// began is when it started, a time.Now() reading, and duty whether the
	// duty cycle counts it (runPlan.duty): what a swap that stops it records
	// as the repository's last run (swap.go's stopRun), since the run ended
	// from outside records none of its own.
	began time.Time
	duty  bool
}

// runPlan is what a run was started to do, fixed when it is started under
// the entry's mu (plan): the index it writes — the declared write with its
// bitmap, the one without, or none — and whether it writes the commit-graph,
// or, in chain mode, the node's tip and nothing else; and FIRST, before the
// index, what the run covers — the packs that index will cover, the objects
// its charges are of and the bytes its deadlines are sized by, the tail's
// for the tip — the memory it was admitted at (maintFlight), the graph's own
// charge, and when it started on the cache's clock.
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

// graphOnly is a graph-first plan without its index: what a run takes when
// no slot of the maintainer's own is free for the whole plan — the graph
// alone may take the one kept for a person, the index may not — or when the
// index's charge leaves it no room beside what is in flight and the graph's
// own does (startMaintenance).
func (p runPlan) graphOnly() runPlan {
	p.index, p.memory = 0, p.graphCharge
	return p
}

// duty reports whether the run is one the duty cycle counts — when it ended
// and how long it took push the repository's next run back. A graph-first
// run that writes the graph alone is not: it is one walk of the commits,
// once an incarnation, and counted it would hold the bitmap a clone counts
// from back by MidxMinInterval.
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
// The room STANDS until the next pass ends (maintFlight.standing), because a
// compaction's build is a run admitted between passes (Cache.AdmitCompaction)
// and would otherwise refill it the same way.
//
// The lane never waits: a run refused a slot at its first step — some other
// process holds it — never started, records nothing and kicks nothing, and
// the next tick asks again. The cache's own runs never refuse one another:
// there are fewer of them than the lane has slots, and each holds at most
// one at a time.
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
			if p.steps() && !c.runnable(e) && e.adopting > 0 {
				// Held back by an adoption's links alone, milliseconds:
				// the last adoption to let go wakes the maintainer, so a
				// run due at once — a graph, an index repair — is not
				// left to the next tick.
				e.maintainerOwed = true
			}
			if p.steps() && c.runnable(e) {
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
	// held back for memory (reservation), left standing until the next pass
	// ends and sets its own: what a compaction, admitted between passes and
	// not by one, is admitted beside. The pass's own runs it held back are
	// started only by passes, so for them the pass's local reservation is the
	// whole rule; a compaction is the one run started between passes, and
	// admitted with no reservation it would take the room a pass promised
	// the repository it held back, which the next pass would then hold back
	// again — for as long as compactions kept coming.
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

// recharge holds the run in flight at *mem to next instead, keeping its
// place among the runs and the slot it holds: at once where next is no
// larger, and otherwise where admit's memory rule (fitsLocked) holds next
// beside everything else in flight — the run's own charge out of it — and the
// room the latest pass kept standing; *mem is next once it is in flight, and
// on a refusal the run keeps its charge. notDue answers a run no longer in
// flight. NO SLOT IS COUNTED: the run holds its own, which a charge moving
// changes nothing of.
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

// runnable reports whether a run may start on e's current incarnation now; e.mu
// is held: never while a round is in flight or another run is, never while a
// check of the repository is admitted (its listing reads once whether a run
// is in flight, and a run started under it would leave files the listing
// reports as damage — holdMaintainer), never on an incarnation this process
// has not read whole, and never on one leaving the cache (wantsRun). WHEN the
// ordinary run is due is due's to say, and WHAT a run takes, and whether the
// node's memory holds it, plan's and admit's.
func (c *Cache) runnable(e *entry) bool {
	return c.wantsRun(e) && e.running == nil && e.adopting == 0 && e.maint == nil && e.verifying == 0
}

// owedWake reports whether the maintainer is owed a wake on e now — a pass
// was held back from it (maintainerOwed) by a round or an adoption, and
// neither holds it any more — and takes the debt; e.mu is held.
func (c *Cache) owedWake(e *entry) bool {
	if !e.maintainerOwed || e.running != nil || e.adopting > 0 {
		return false
	}
	e.maintainerOwed = false
	return true
}

// due reports whether e's current incarnation's ORDINARY run is due now; e.mu
// is held. See DUE above: its packs have stood still for MidxDebounce, or the
// oldest of them has waited MidxMaxWait, and its last run ended at least
// max(MidxMinInterval, MidxDutyCycle × that run's duration) ago — or its
// index is being repaired (INDEX REPAIR above), which is due at once. A
// graph-first run is due whatever this says (plan).
func (c *Cache) due(e *entry, now time.Time) bool {
	inc := e.current
	if inc.indexRepair && len(inc.installed) == 0 {
		// A multi-pack-index dropped as damaged (sync.go apply): every
		// clone of the copy walks without a bitmap until it is written
		// again, so no debounce and no interval hold it back. Own mode's
		// alone: a chained copy writes no index, and its tip waits for
		// nothing a repair of an index owed.
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

// wantsRun reports whether e's current incarnation wants a maintenance run
// for what it HOLDS, whenever one may start: one the maintainer works on
// (holdsWork) in OWN mode lacking the maintainer's bitmapped index or its
// commit-graph. One in CHAIN mode — an installed published chain (layers.go)
// — wants no own-mode write: a whole index written beside the chain deletes
// every layer of it and the chain file, a whole graph the graph chain, and
// its index is its maintainer's to publish; it wants its TIP over a tail that
// moved since the tip was written (tipWanted). runnable and due add WHEN —
// no round, run or verify in flight, the debounce and the interval passed —
// and plan WHAT, on the node's memory. e.mu is held.
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
// holds a pack, and is not the tail the last write that succeeded covered —
// writing the tip it holds, or finding the tail held no commit the layers
// lack. A tail of the same packs holds the same commits, so its tip stands
// until the tail's packs change, and a quiet repository is written nothing
// at every pass, every full round and every restart. A digest compared,
// under the cache's lock as a pass reads every entry, rather than a walk of
// the packs (incarnation.tail). e.mu is held.
func tipWanted(inc *incarnation) bool {
	return inc.tail.packs > 0 && !(inc.tipWritten && inc.tipOver == inc.tail.digest)
}

// indexDue reports whether inc wants the maintainer's BITMAPPED index
// written: none on disk, one without its bitmap (a full round's own, or the
// maintainer's where the bitmap was refused), or MidxPacks packs gathered
// beside it. One that is current and has its bitmap is due for its
// commit-graph alone, if that is what it lacks.
func (c *Cache) indexDue(inc *incarnation) bool {
	return inc.covered() == 0 || !inc.bitmapped() || inc.packs-inc.covered() >= c.midxPacks
}

// plan decides what a run over inc would take on a budget of budget, and
// answers it with the steps the budget refused and every step's charge
// (chargeOver: its own figure an object of what it covers — the repository,
// or the tail for the node's tip — and room beside it for one transfer of the
// whole repository). e.mu is held.
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
// Those are the ORDINARY run's, taken only when due says it is due. An
// incarnation with NO COMMIT-GRAPH — read whole and never graphed, or its
// graph swept with a pack — is planned a graph-first run whatever due says:
// the graph alone, or the graph and then the index where the ordinary run is
// due too (GRAPH FIRST above). One whose graph-first run failed (graphFailed)
// waits for the ordinary rules, so a step that keeps failing is not asked
// again every pass.
//
// A step due whose charge alone is past the budget is refused: never planned,
// on this node, at this memory — judged every pass, due or not, since the
// gauge and the alarm follow the repositories rather than the runs. The plan's
// own charge is the largest of its steps', since they run one after the
// other.
//
// IN CHAIN MODE — an installed published chain (layers.go) — NO OWN-MODE STEP
// IS EVER PLANNED, nor refused: the two modes never coexist (wantsRun). The
// one step is the node's TIP over the tail (THE TIP above), wanted while the
// tail is not the one its last write covered (tipWanted) and refused where
// its charge alone is past the budget: due at once where the copy holds no
// tip — unless its first write failed (tipFailed), which waits for the
// ordinary rules — and otherwise when due says the ordinary run is.
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
		charges[s] = chargeOver(over, w.BytesPerObject, inc.objects)
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

// startMaintenance starts one run of e's current incarnation on a goroutine
// of its own when it is still runnable, its plan takes a step, and the runs in
// flight leave it room (maintFlight.admit: at most most of them — a
// graph-first run writing the graph alone every slot of the lane, lane — held
// to budget, the pass's reservation held for) — answering the run, or nil and
// why not, with the
// plan's charge, which a pass holds a reservation of when the answer is
// heldBack.
//
// The run pins the incarnation and registers itself on the entry (e.maint)
// and as the cache's work in its directory (enter), so Stop waits for it and
// a pin keeps its directory; then it runs holding no lock of the entry, in a
// context the pass's ctx, the cache's lifetime, a full round's sweep
// (stopMaintenance) and a retire can each end.
func (c *Cache) startMaintenance(ctx context.Context, e *entry, most, lane int, budget int64,
	held reservation) (*maintRun, verdict, int64) {

	if !c.enter() {
		return nil, notDue, 0
	}
	e.mu.Lock()
	if !c.runnable(e) {
		e.mu.Unlock()
		c.leave()
		return nil, notDue, 0
	}
	inc := e.current
	plan, _, _ := c.plan(inc, budget, c.due(e, c.now()))
	if !plan.steps() {
		e.mu.Unlock()
		c.leave()
		return nil, notDue, 0
	}
	// THE SLOT KEPT FOR A PERSON'S VERIFY is lent to a GRAPH ALONE — a walk
	// of the commits, seconds where a bitmap is hours, so a check waits at
	// most that long — and never to a run that goes on to an index: a run
	// takes a slot for each of its steps in turn, so a graph-first run
	// carrying the index it is due for as well (its packs stood still before
	// the run started: every graph-less copy after a restart) would hold the
	// person's slot through its bitmap. So a graph-first plan carrying an
	// index is admitted at the maintainer's own limit, and where that leaves
	// it no slot, or the index's charge no room beside what is in flight, the
	// graph runs alone, at the lane's; the index follows in a run of the
	// maintainer's own once the graph is written. A graph-first plan with no
	// index is that graph alone from the start.
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
		e.mu.Unlock()
		c.leave()
		return nil, v, plan.memory
	}
	runCtx, cancel := context.WithCancel(ctx)
	//nolint:contextcheck // The cache's lifetime ends the run too: Stop must end the git it started however long the pass's ctx lives.
	stop := context.AfterFunc(c.life, cancel)
	m := &maintRun{e: e, inc: inc, cancel: cancel, done: make(chan struct{}),
		writes: c.mw.written(c.image, c.mw.steps(plan)), began: time.Now(), duty: plan.duty()}
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
//     The one stop that counts it is a SWAP's, made before the swap links
//     the files the run writes: the stop is recorded as the repository's
//     last run and what the run had spent as its length (swap.go's stopRun),
//     and the next incarnation carries it, since a repository compacted more
//     often than its index run lasts would otherwise have every run stopped
//     and every next one due at once.
//   - Any other end is a run, whether it succeeded, failed or hit its
//     deadline: recorded with when it ended and how long it took, which the
//     duty cycle reads, and persisted. One whose commit-graph was refused a
//     slot after its index was written records the index and no graph,
//     which leaves the repository due for its graph alone once the interval
//     has passed — a refusal, not a failure. One that failed first removes
//     what a killed git leaves behind (clearWriting).
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
		// A step whose git process ran and did not succeed: what the run's
		// writes hold while they run is cleared — the failed step's own, and
		// any other write's of the plan, since nothing but this run writes
		// them in the incarnation (clearWriting). A step stopped or refused
		// before its process began wrote nothing, and the steps before it
		// succeeded, git removing their locks as they did: nothing to clear,
		// and no listing for it.
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
const maintainFile = "internal/code/cache/maintain.go"

// The argument lists of the declared writes (derived.go) — a full round's
// index, the maintainer's index with its bitmap and without one, own mode's
// graph and the node's tip over an installed chain — spelled as literals in
// the file that runs them, which is how TestNothingButSyncWritesACache reads
// which git writer a file runs. Both graphs carry changed-path filters, at
// most MaxNewFilters new ones a write: its flag is spelled whole for the same
// reason, and TestTheFilterFlagIsTheConstant holds it to the constant. The
// tip is a SPLIT write that never merges (--split=no-merge): one layer of the
// commits the installed layers lack, over exactly those layers — merging, it
// would write the published layers again into a layer of the node's own.
var (
	roundIndexArgs = []string{"multi-pack-index", "write", "--stdin-packs", "--no-progress"}
	bitmapArgs     = []string{"multi-pack-index", "write", "--bitmap", "--no-progress"}
	plainArgs      = []string{"multi-pack-index", "write", "--no-progress"}
	graphArgs      = []string{"commit-graph", "write", "--reachable", "--changed-paths", "--max-new-filters=1024",
		"--no-progress"}
	tipGraphArgs = []string{"commit-graph", "write", "--reachable", "--split=no-merge", "--changed-paths",
		"--max-new-filters=1024", "--no-progress"}
)

// maintWrites is the maintainer's declared writes — every Write of the
// cache's declarations whose Trigger is TriggerMaintainer, in declaration
// order, a maintStep's bit i being all[i] — and the five the plan and a full
// round name by what they ARE: the multi-pack-index with its bitmap and
// without one, the commit-graph, the node's tip over an installed chain, and
// the round's own index. The declarations say what each step is — its
// argument list, lane, deadline and charge — and this file when each runs
// (plan): no step's name, charge, lane, deadline or argument list is spelled
// outside derived.go.
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

// writeTip is the node's TIP write over a chained inc (THE TIP above), the
// tail p planned it over, and answers whether its git process began and how
// it failed. In a slot of the maintenance lane it RESERVES FIRST, so a lane
// with no slot free refuses it before anything of the copy moves — and then:
//
//  1. commit-graph-chain written in place naming EXACTLY the installed
//     layers' commit-graph layers, the old tip off it: git writes its split
//     layer over the chain file it finds, so the new tip is a layer of every
//     commit those layers lack rather than one stacked on the old tip, whose
//     file git's own write expires (every .graph its chain does not name, as
//     old as the write). A reader that read the old chain and opens the old
//     tip once it is gone is told "unable to find all commit-graph files" and
//     goes on with the layers below it — git's own split writes do the same
//     to their readers, and that sentence is no damage (gitproc's format
//     table refuses it as a row: a healthy repository prints it).
//  2. git's split write (tipGraphArgs), under the declaration's deadline over
//     the tail.
//  3. commit-graph-chain read back: a line past the layers is the new tip,
//     and none is a tail that held no commit the layers lack, which leaves no
//     tip and the tail recorded as written over, so it is not due again until
//     it moves.
//  4. recorded (recordChains), whatever git did — the chain file is what the
//     record must name — and only a write that succeeded records the tail it
//     covered (tipOver); one that failed records none, whatever tip it left,
//     so the tip is wanted again.
//
// The chain files are written IN PLACE while readers hold the copy, so the
// whole write is marked in flight (chainWrite): a check or a scrub that read
// what the record named before it, or hashes the chain file across it, has no
// verdict of the file. Holding no lock of the entry; the run's pin keeps the
// directory, and nothing else writes a chain file of a copy a run is on: a
// cut and a placement stop the run first (dropFor, a swap's stopRun, a full
// round's stopMaintenance).
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
// not succeed may have left in inc — every file each holds while it runs
// (Write.Holds): a lock, which git removes when it is asked to stop and
// leaves when it is killed, and a bitmap's temporary file, which it leaves
// either way. The run was the only writer of any of them in the incarnation,
// since no round runs a git writer beside a maintenance run and a round's
// index runs with none in flight, so anything of theirs still there is its
// own.
//
// A held name that is a whole literal (a lock) is removed BY NAME, and a
// directory is listed at most once, only for the patterns that are not — the
// bitmap's temporary file — since objects/pack holds three files a pack, and
// its listing is what a full round stopping the run, and the request that
// round answers, would otherwise wait for. A removal that fails is logged;
// the incarnation's next full round sweeps whatever is outside its image.
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
