package cache

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/crewlet/crewlet/internal/backoff"
	"github.com/crewlet/crewlet/internal/code"
	"github.com/crewlet/crewlet/internal/queue/topics"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/statelog/metrics"
)

// THE BACKFILL: one closed loop a node (Start), behind the serving, that does
// for the indexes of pushes (provenance.go) what no round does, one step at a
// time, each step one incarnation's:
//
//  1. CHECK the runs a previous process left — or a rebuild linked unvouched
//     — against their own trailers, a read slot a piece, before anything maps
//     one: a reader meanwhile is answered without the index. A run that fails
//     goes on the copy's repair mark, whose round drops the index.
//  2. MERGE ProvenanceMergeWidth adjacent runs as one (mergeDue): the
//     cheapest such span that is BALANCED — no run of it holding more than
//     ProvenanceMergeFactor times the others together — so the runs folds of
//     a push or two write merge eight at a time where a pair at a time they
//     merged on nearly every push, runs grow geometrically as they merge, and
//     each push is written O(log n) times over its life, whatever sizes the
//     folds, the gap's pages and the history's write, and at whichever end;
//     or, at MaxProvenanceRuns with no such span, the cheapest pair whatever
//     their sizes, so a round's fold finds room.
//  3. Read one page of the GAP a full round passed over — EVERY holder,
//     bounded by the pushes made while the node was down — ascending, at the
//     SESSION floor of the gap's end, so whichever data node answers holds
//     every push in it.
//  4. Read one page of a repository's HISTORY, down to the start of the push
//     history the rows keep, only where it is WANTED (Cache.WantHistory): read
//     by the repository's effective home alone, once per fleet, and only for a
//     repository a seat asked about (M3-decisions M3-O20).
//  5. Ask the rows for their oldest push once every HorizonEvery — every
//     holder — and drop what the index holds below it, so the index holds the
//     push history the rows do and never grows past it.
//
// Within a kind of step, the most recently used repository first: the ones
// people are asking about. The step is taken from the backfill's SCHEDULE
// (schedule.go), which every change to a copy's work keeps — never from a
// walk of every copy under the lock every request takes, which at 10 000
// copies held it up to 127 ms a step, on every push of every repository: a
// pick costs the copies whose work changed since the last, and O(log n) in
// the rest.
//
// # Every read is at a floor the index has already seen
//
// A page is read at SESSION, its floor a position this cache already read —
// the gap's end for the gap, the index's From for the history and the
// horizon — never at stale: a data node behind the log holds a PREFIX of the
// rows, and a page read from one would hold a range's pushes partly, which a
// run then claims to hold whole — a commit introduced in the missing part
// answered with no push, a landing searched over moves with one missing and
// named for the wrong push. At a floor the index has seen, every push the page
// ranges over is on the answering node.
//
// # A page is cut where a record ends
//
// Group commit lands up to code.MaxPushesPerRecord pushes in one record, at one
// position, and a page — bounded by its rows and its bytes — can end inside
// one. A run is complete over its range, so a page that ends inside a record
// leaves that record's pushes to the next page, whose range starts below it;
// and where every row of a page is at one position, the next page is read on
// from its cursor until the record ends, so no record is ever split across
// two runs — at most MaxPushesPerRecord pushes held.
//
// # Paced by the fleet
//
// After each page the loop waits BackfillIdle × ⌈live git nodes ÷ live data
// nodes⌉ × the page's own duration, the counts read from the fleet's
// placements again after every page (Options.Fleet). Every git node backfills
// from the data nodes, so its share of them is that ratio: one git node a data
// node holds at most a third of one reader of each, and the fleet's backfills
// hold under half of one however many git nodes share them — the serving keeps
// the rest. Counts that cannot be read are waited on and read again, never
// paced on a guess.
//
// # A step that fails waits on its own repository
//
// A step that fails — a page the estate could not serve, a run that could
// not be written or merged — is taken again only once its REPOSITORY's
// retry window ends, the window doubling from BackfillRetry to
// BackfillRetryCeiling with each failure of it in a row
// (entry.bfRetryAt), and the backfill meanwhile takes every other
// repository's steps. Sharing one wait, the repository picked first — the
// one used most recently — was picked again after every wait, so the rows of
// one repository the data nodes could not serve held every other copy's gap
// open; and a failed merge, answered as a step done, was taken again at once,
// for ever. Only failures that SPAN repositories — two or more in a row with
// none succeeding between — make the loop itself wait, doubling the same
// way: the estate unreachable, or a disk refusing every write, never a
// stream of failures as fast as the copies can fail them. A run the index
// names that is gone from the disk is LOST, as a full round finds one lost
// (indexFull): its check asks the repair round that makes the index anew.

// backfillWork is a kind of step, in the order the loop takes them.
type backfillWork uint8

const (
	workCheck backfillWork = iota + 1
	workMerge
	workGap
	workHistory
	workHorizon
)

// backfillJitter spreads each failure's wait across a fifth of itself either
// way — the code reconciler's spread for its retries (engine's
// codeReposRetryJitter) — so the git nodes of a fleet whose data nodes failed
// together do not ask them again on the same second.
const backfillJitter = 0.2

// backfillTask is one step the backfill picked: the incarnation, PINNED as no
// use, and the index it was picked from — by its making (gen) and as it stood
// (rec) — so a step commits only into the index it read.
type backfillTask struct {
	e    *entry
	inc  *incarnation
	gen  uint64
	rec  *provenanceState
	work backfillWork
	// lo and hi are a merge's runs, [lo, hi) by their index in rec.Runs.
	lo, hi int
	// unchecked is a check's runs: those of rec no vouch is held for.
	unchecked []provenanceRun
}

// backfillDone is what one step did: its kind (work), of which repository
// (repo), and anything at all (did); a page read in page — what the loop
// then owes a pace; a step that failed (err), whose repository waits retry
// before the next; or nothing due, and when the next step falls due by time
// (next, zero for none).
type backfillDone struct {
	work  backfillWork
	repo  string
	did   bool
	page  time.Duration
	next  time.Time
	err   error
	retry time.Duration
}

// backfillStreak is the backfill's failed steps in a row since one last
// succeeded: how many, the repository the first was of, and how many have
// failed since they spanned two repositories or more — what the loop as a
// whole waits on.
type backfillStreak struct {
	n      int
	first  string
	spread int
}

// fail counts a failed step of repo and answers how many failures in a row
// the loop itself waits on: none while every one is of one repository, which
// its own window holds back while the others' steps go on.
func (s *backfillStreak) fail(repo string) int {
	switch {
	case s.n == 0:
		s.first = repo
	case s.spread > 0 || repo != s.first:
		s.spread++
	}
	s.n++
	return s.spread
}

// kickBackfill wakes the backfill without waiting for it.
func (c *Cache) kickBackfill() {
	select {
	case c.backfillKicks <- struct{}{}:
	default:
	}
}

// backfiller is the backfill's loop, until ctx ends or the cache stops: a
// step at a time while any is due, the pace a page owes paid before the next
// step, and — with nothing due — parked, holding nothing of the directory,
// until it is kicked or the next step falls due by time: a horizon, or the
// end of a repository's retry window. A wait for a pace or after failures
// that span repositories is not cut short by a kick: new work is no reason
// to read the estate sooner.
func (c *Cache) backfiller(ctx context.Context) {
	var (
		// unpaced is the fleet's counts unreadable in a row.
		unpaced int
		owed    time.Duration
		streak  backfillStreak
	)
	alive := func() bool { return ctx.Err() == nil && c.life.Err() == nil }
	for {
		if owed > 0 {
			ratio, err := c.fleetRatio()
			if err != nil {
				unpaced++
				c.log.Warn("code_cache_backfill_unpaced", "error", err)
				if !c.backfillWait(ctx, c.backfillRetry(unpaced), false) {
					return
				}
				continue
			}
			unpaced = 0
			pace := time.Duration(BackfillIdle*ratio) * owed
			owed = 0
			if !c.backfillWait(ctx, pace, false) {
				return
			}
			continue
		}
		if !c.enter() {
			return
		}
		// A kick sent before this step reads the entries is answered by it.
		select {
		case <-c.backfillKicks:
		default:
		}
		done := c.backfillStep(ctx)
		c.leave()
		var ok bool
		switch {
		case done.err != nil:
			c.log.Warn("code_cache_backfill_failed", "repo", done.repo, "error", done.err, "retry_in", done.retry)
			ok = alive()
			if spread := streak.fail(done.repo); spread > 0 {
				ok = c.backfillWait(ctx, c.backfillRetry(spread), false)
			}
		case done.page > 0:
			streak, owed, ok = backfillStreak{}, done.page, true
		case done.did:
			streak, ok = backfillStreak{}, alive()
		default:
			var until time.Duration
			if !done.next.IsZero() {
				until = max(done.next.Sub(c.now()), time.Nanosecond)
			}
			ok = c.backfillWait(ctx, until, true)
		}
		if !ok {
			return
		}
	}
}

// backfillRetry is the wait after failures failures in a row.
func (c *Cache) backfillRetry(failures int) time.Duration {
	return backoff.Jitter(backoff.Doubling(failures, BackfillRetry, BackfillRetryCeiling), backfillJitter)
}

// fleetRatio is ⌈live git nodes ÷ live data nodes⌉ as the fleet's placements
// count them now, each count at least one: a node with no fleet is both.
func (c *Cache) fleetRatio() (int, error) {
	git, data, err := c.fleet()
	if err != nil {
		return 0, fmt.Errorf("cache: counting the fleet's git and data nodes: %w", err)
	}
	git, data = max(git, 1), max(data, 1)
	return (git + data - 1) / data, nil
}

// backfillWait waits d — none at zero, unless kickable, which then waits for a
// kick alone — or for a kick where kickable, and answers false once ctx ends
// or the cache stops.
func (c *Cache) backfillWait(ctx context.Context, d time.Duration, kickable bool) bool {
	var wake <-chan time.Time
	switch {
	case d <= 0 && !kickable:
		return ctx.Err() == nil && c.life.Err() == nil
	case d <= 0:
	case c.knobs != nil && c.knobs.backfillWake != nil:
		wake = c.knobs.backfillWake(d)
	default:
		t := time.NewTimer(d)
		defer t.Stop()
		wake = t.C
	}
	var kicks <-chan struct{}
	if kickable {
		kicks = c.backfillKicks
	}
	select {
	case <-ctx.Done():
		return false
	case <-c.life.Done():
		return false
	case <-kicks:
	case <-wake:
	}
	return true
}

// backfillStep takes the next step due and does it — under ctx and the
// cache's lifetime, which Stop ends — and answers what it did. A step that
// fails opens or widens its repository's retry window, and one that succeeds
// closes it (entry.bfRetryAt); a step its context ended does neither. And
// whatever it did, its copy is marked once it has (backfillDirty): a step is
// a change to the copy's work.
func (c *Cache) backfillStep(ctx context.Context) (done backfillDone) {
	t, next := c.nextBackfill()
	if t == nil {
		return backfillDone{next: next}
	}
	sctx, cancel := context.WithCancel(ctx)
	defer cancel()
	//nolint:contextcheck // The cache's lifetime ends the step too: Stop must end its reads however long ctx lives.
	stop := context.AfterFunc(c.life, cancel)
	defer stop()
	defer func() {
		now := c.now()
		t.e.mu.Lock()
		gone := c.unpin(t.e, t.inc)
		switch {
		case done.err != nil:
			t.e.bfFailures++
			t.e.bfRetryAt = now.Add(c.backfillRetry(t.e.bfFailures))
			done.retry = t.e.bfRetryAt.Sub(now)
		case done.did && sctx.Err() == nil:
			t.e.bfFailures, t.e.bfRetryAt = 0, time.Time{}
		}
		t.e.mu.Unlock()
		c.trash(gone)
		c.backfillDirty(t.e)
		done.work, done.repo = t.work, t.e.id
	}()
	switch t.work {
	case workCheck:
		c.checkRuns(sctx, t)
		return backfillDone{did: true}
	case workMerge:
		ok, err := c.mergeSpan(sctx, t.e, t.inc, t.gen, t.rec, t.lo, t.hi)
		switch {
		case err != nil && sctx.Err() != nil:
		case err != nil:
			return backfillDone{err: fmt.Errorf("repository %s: merging runs of its index of pushes: %w", t.e.id,
				err)}
		case ok:
			c.backfillStored(sctx, t)
		}
		return backfillDone{did: true}
	}
	began := c.now()
	err := c.backfillPage(sctx, t)
	took := c.now().Sub(began)
	switch {
	case err != nil && sctx.Err() != nil:
		return backfillDone{did: true}
	case err != nil:
		return backfillDone{err: fmt.Errorf("repository %s: %w", t.e.id, err)}
	}
	return backfillDone{did: true, page: took}
}

// backfillStored writes down what a step committed into t's index — the
// incarnation's state, which names its runs — and the runs the index no
// longer names go with it (persist). A state that could not be written leaves
// them where they are, named by the state on the disk, which a restart reads,
// and waiting for the next state any writer writes; a retired incarnation's
// state is not written at all, its directory leaving with its last pin.
func (c *Cache) backfillStored(ctx context.Context, t *backfillTask) {
	t.e.mu.Lock()
	retiredInc := t.inc.retired || t.inc.discarded
	t.e.mu.Unlock()
	if retiredInc {
		return
	}
	if err := c.persist(ctx, t.e, t.inc, false); err != nil {
		c.log.Warn("code_cache_provenance_unpersisted", "repo", t.e.id, "incarnation", t.inc.n, "error", err)
		return
	}
	c.setGauges()
}

// nextBackfill picks the next step due — the first kind of step any copy has
// due, and among those the most recently used, from the schedule's queues
// (schedule.go) once every copy marked since the last pick is placed again —
// pinned as no use; or nil, and when the next step falls due by time alone
// (zero for none). The copy the queues put first is looked at once more
// under its own lock, and that look, never the queue, is what it pins.
func (c *Cache) nextBackfill() (*backfillTask, time.Time) {
	s := &c.sched
	s.pick.Lock()
	defer s.pick.Unlock()
	now := c.now()
	c.placeDue(now)
	for {
		s.mu.Lock()
		e, work, next := s.top()
		s.mu.Unlock()
		if e == nil {
			return nil, next
		}
		e.mu.Lock()
		d := c.backfillDue(e, now)
		if d.work == work {
			t := d.task(e)
			t.inc.pins++
			e.mu.Unlock()
			return t, time.Time{}
		}
		// Its work moved since it was placed, and a mark of it is on its
		// way or already here: placed as this look says, or left to the
		// next pick's look at the mark.
		s.mu.Lock()
		s.placeLooked(e, d)
		s.mu.Unlock()
		e.mu.Unlock()
	}
}

// dueWork is a look at one copy's backfill (backfillDue): the first kind of
// step it has due, or none and when its next falls due by time alone (zero
// for never), with its last use, whether it counts as behind, and what a step
// of it takes.
type dueWork struct {
	work   backfillWork
	when   time.Time
	used   time.Time
	behind bool
	inc    *incarnation
	gen    uint64
	rec    *provenanceState
	// lo and hi are a merge's runs, [lo, hi) by their index in rec.Runs;
	// unchecked a check's runs, one bit each by their index there.
	lo, hi    int
	unchecked uint64
}

// The look marks a record's runs in one machine word each, so a record holds
// at most 64: a compile error past it.
const _ uint64 = 1 << (MaxProvenanceRuns - 1)

// backfillDue looks at e's backfill: the first step its current index has due
// — a check of the runs no vouch is held for, a merge, a page of its gap, a
// page of its history where it is wanted, its horizon — or none and when its
// next falls due: its horizon, or the end of its retry window, before which
// it has none (entry.bfRetryAt); nothing at all for a copy the backfill does
// not work on (dead, with no converged current incarnation, keeping no
// index). e.mu is held. It reads no disk and allocates nothing: a run is
// unchecked when no vouch is held for its name — a vouch held for another
// identity than the file has is forgotten by the reader that meets it
// (mapRun), which marks the copy.
func (c *Cache) backfillDue(e *entry, now time.Time) dueWork {
	inc := e.current
	if e.dead || inc == nil || inc.retired || !inc.converged || inc.record() == nil {
		return dueWork{}
	}
	rec := inc.record()
	d := dueWork{used: inc.lastUsed, behind: rec.gapped() || e.wantHistory && !rec.Complete, inc: inc,
		gen: inc.provGen, rec: rec}
	if now.Before(e.bfRetryAt) {
		d.when = e.bfRetryAt
		return d
	}
	vouched, unchecked := runMarks(inc, rec)
	horizon := rec.OldestAt.Add(HorizonEvery)
	switch lo, hi := mergeDue(rec, vouched); {
	case unchecked != 0:
		d.work, d.unchecked = workCheck, unchecked
	case hi > 0:
		d.work, d.lo, d.hi = workMerge, lo, hi
	case rec.gapped():
		d.work = workGap
	case e.wantHistory && !rec.Complete:
		d.work = workHistory
	case rec.OldestAt.IsZero() || !now.Before(horizon):
		d.work = workHorizon
	default:
		d.when = horizon
	}
	return d
}

// task is the step d looked at, of e.
func (d dueWork) task(e *entry) *backfillTask {
	t := &backfillTask{e: e, inc: d.inc, gen: d.gen, rec: d.rec, work: d.work, lo: d.lo, hi: d.hi}
	for i, r := range d.rec.Runs {
		if d.unchecked&(1<<i) != 0 {
			t.unchecked = append(t.unchecked, r)
		}
	}
	return t
}

// runMarks is which of rec's runs inc holds a vouch for, and which none and
// no failed check either — those a check reads (a run whose check failed
// waits for the repair round that drops the index, and is not checked again
// meanwhile) — one bit each by their index in rec.Runs; e.mu is held. Each
// run's name is spelled into a buffer of the stack, so the look allocates
// nothing.
func runMarks(inc *incarnation, rec *provenanceState) (vouched, unchecked uint64) {
	var buf [runRelBytes]byte
	for i, r := range rec.Runs {
		rel := appendRunRel(buf[:0], r.N)
		if _, ok := inc.vouched.derived[string(rel)]; ok {
			vouched |= 1 << i
			continue
		}
		if _, failed := inc.repair.found[string(rel)]; !failed {
			unchecked |= 1 << i
		}
	}
	return vouched, unchecked
}

// mergeDue is the runs of rec, [lo, hi) by their index, the backfill merges
// next — adjacent, on one side of its gap, each one vouched holds a bit for —
// or hi zero for none:
//
//  1. the cheapest span of ProvenanceMergeWidth runs that is BALANCED: none
//     holding more than ProvenanceMergeFactor times the others together;
//  2. with rec at MaxProvenanceRuns and no such span, the cheapest adjacent
//     pair whatever their sizes, so a fold finds room.
//
// A merge is never of fewer runs than the width, so the runs folds of a push
// or two write merge once eight of them stand side by side, not on nearly
// every push; and never one a run dominates, so no run is rewritten for less
// than half its own pushes again. Balance asks nothing of where a run lies or
// how its neighbours compare one to one: a pair rule — two neighbours within
// a factor of each other — met runs of every size a burst of pushes folds,
// each beside one it is not within a factor of, and the index filled to its
// bound, where every fold's merge rewrote a run whatever its size.
func mergeDue(rec *provenanceState, vouched uint64) (int, int) {
	best, cost := -1, int64(0)
spans:
	for lo := 0; lo+ProvenanceMergeWidth <= len(rec.Runs); lo++ {
		var sum, most int64
		for i := lo; i < lo+ProvenanceMergeWidth; i++ {
			if vouched&(1<<i) == 0 || i > lo && !rec.adjacent(i-1) {
				continue spans
			}
			sum += rec.Runs[i].Pushes
			most = max(most, rec.Runs[i].Pushes)
		}
		if most <= ProvenanceMergeFactor*(sum-most) && (best < 0 || sum < cost) {
			best, cost = lo, sum
		}
	}
	switch {
	case best >= 0:
		return best, best + ProvenanceMergeWidth
	case len(rec.Runs) >= MaxProvenanceRuns:
		if i := cheapestPair(rec, func(i int) bool { return vouched&(1<<i) != 0 }); i >= 0 {
			return i, i + 2
		}
	}
	return 0, 0
}

// checkRuns holds every run of t no vouch is held for to its own trailer
// (CheckRunTrailer), one piece at a time under a read slot that waits its turn
// however long, as the scrub's does (scrubHold) — nobody waits on it. A run
// that passes is vouched for; one that fails, or whose read fails, goes on the
// copy's repair mark at the identity it failed at, with a repair round asked,
// which drops the index (dropDamaged: eventFailedCheck) — never under a check
// of the repository, whose release asks the round again. A run gone from the
// disk while its index still names it is LOST, and goes on the mark the same
// way, as a full round's indexFull makes an index anew over a run it finds
// gone: answered "replaced", its check found it unchecked again at once, and
// took it again, for ever. One its index no longer names — retired since the
// check was picked — decides nothing.
func (c *Cache) checkRuns(ctx context.Context, t *backfillTask) {
	p := c.pieces(c.scrubHold)
	began := time.Now()
	var read int64
	defer func() {
		if c.metrics != nil && read > 0 {
			c.metrics.Observe(metrics.CodeCacheCheckDuration, time.Since(began),
				metrics.Attrs{"trigger": string(triggerFirst)})
		}
	}()
	for _, r := range t.unchecked {
		rel := runRel(r.N)
		path := filepath.Join(t.inc.dir, rel)
		// Counted as what its declaration names a check of its files by
		// (Owned.File), as every derived file's is.
		kind := fileKind("")
		if _, o := c.image.owner(rel); o != nil {
			kind = fileKind(o.File)
		}
		if c.knobs != nil && c.knobs.hashing != nil {
			c.knobs.hashing(path)
		}
		v, id, n, err := trailerSound(ctx, path, p, expect{run: true})
		read += n
		switch {
		case err != nil && ctx.Err() != nil:
			return
		case err == nil && v == hashNone && !c.namesRun(t, r):
			c.countCheck(kind, checkReplaced, triggerFirst)
			continue
		case err == nil && v == hashNone:
			c.countCheck(kind, checkDamaged, triggerFirst)
			err = errRunGone
		case err == nil && v == hashSound:
			c.countCheck(kind, checkSound, triggerFirst)
			t.e.mu.Lock()
			vouch(t.inc, rel, id)
			t.e.mu.Unlock()
			continue
		case err != nil:
			c.countCheck(kind, checkUnreadable, triggerFirst)
		default:
			c.countCheck(kind, checkDamaged, triggerFirst)
		}
		c.log.Warn("code_cache_check_damaged", "repo", t.e.id, "incarnation", t.inc.n, "file", rel,
			"trigger", string(triggerFirst), "action", c.image.repairAction(rel, ""), "error", err)
		t.e.mu.Lock()
		if t.inc.repair.found == nil {
			t.inc.repair.found = map[string]checkedFile{}
		}
		t.inc.repair.found[rel] = checkedFile{kind: kind, rel: rel, id: id, trigger: triggerFirst}
		if t.e.current == t.inc {
			c.repairRound(t.e)
		}
		t.e.mu.Unlock()
		return
	}
}

// namesRun reports whether t's incarnation's index names run r now: a run
// number is never taken twice in an incarnation (allocRun), so a run named is
// the file the check read. e.mu is not held.
func (c *Cache) namesRun(t *backfillTask, r provenanceRun) bool {
	t.e.mu.Lock()
	defer t.e.mu.Unlock()
	rec := t.inc.record()
	return rec != nil && rec.named(r.N)
}

// backfillPage reads and commits one page of t's work — the gap, the history
// or the horizon — and answers the read's failure.
func (c *Cache) backfillPage(ctx context.Context, t *backfillTask) error {
	switch t.work {
	case workGap:
		return c.gapPage(ctx, t)
	case workHistory:
		return c.historyPage(ctx, t)
	}
	return c.horizonPage(ctx, t)
}

// pos is p as a position on the code log.
func pos(p int64) statelog.Position { return statelog.Unpack(topics.CodeLogStream, p) }

// introducedRead is the backfill's read of q's pushes, at SESSION with floor
// as its floor: pages read on from their cursors until the rows hold more than
// one position or the read ends — so the pushes of every position but the
// last are whole — and answered with whether the read goes on past them. A
// repository the rows no longer hold, read at a floor that held it, is
// purged: code.ErrRepoNotFound.
func (c *Cache) introducedRead(ctx context.Context, q code.IntroducedQuery, floor int64) ([]code.IntroducedRow,
	bool, error) {

	read := code.ReadQuery{Kind: code.ReadIntroduced, Introduced: &q,
		Fresh: statelog.FloorAt(statelog.ReadSession, pos(floor))}
	var rows []code.IntroducedRow
	for {
		page, err := c.fetch.Read(ctx, read)
		if err != nil {
			return nil, false, err
		}
		for _, row := range page.Introduced {
			if err := backfillRow(row, q); err != nil {
				return nil, false, fmt.Errorf("%w: %w", ErrBadSnapshot, err)
			}
			if n := len(rows); n > 0 && !inOrder(rows[n-1], row, q.Descending) {
				return nil, false, fmt.Errorf("%w: push %s at %s is served after push %s at %s", ErrBadSnapshot,
					row.PushID, row.Position, rows[n-1].PushID, rows[n-1].Position)
			}
			rows = append(rows, row)
		}
		switch {
		case page.Next == "":
			return rows, false, nil
		case len(page.Introduced) == 0:
			return nil, false, fmt.Errorf("%w: a page of pushes served no row and a cursor past it", ErrBadSnapshot)
		case rows[0].Position != rows[len(rows)-1].Position:
			return rows, true, nil
		}
		read.Cursor = page.Next
	}
}

// inOrder reports whether b may follow a in a read of pushes in log order:
// ascending (position, push id), or the reverse.
func inOrder(a, b code.IntroducedRow, descending bool) bool {
	c := cmp.Or(cmp.Compare(a.Position.Packed(), b.Position.Packed()), strings.Compare(a.PushID, b.PushID))
	if descending {
		return c > 0
	}
	return c < 0
}

// backfillRow holds one row a read of q answered to what a run encodes and
// q's range — a position on the code log inside it, moves naming refs and
// object ids, commits that are object ids — before anything is written of it.
func backfillRow(row code.IntroducedRow, q code.IntroducedQuery) error {
	p := row.Position
	switch {
	case p.Stream != topics.CodeLogStream || p.Valid() != nil:
		return fmt.Errorf("push %s is at %s, which is no position on %s", row.PushID, p, topics.CodeLogStream)
	case q.Descending && p.Packed() >= q.Before.Packed():
		return fmt.Errorf("push %s is at %s, not below %s", row.PushID, p, q.Before)
	case !q.Descending && (p.Packed() <= q.After.Packed() || !q.Through.IsZero() && p.Packed() > q.Through.Packed()):
		return fmt.Errorf("push %s is at %s, outside (%s, %s]", row.PushID, p, q.After, q.Through)
	}
	return validPush(row)
}

// gapPage reads one page of t's gap, (GapFrom, GapThrough] ascending, and
// commits what it read whole: a run over (GapFrom, the last position read
// whole], the gap moved up past it — closed where the read ended.
func (c *Cache) gapPage(ctx context.Context, t *backfillTask) error {
	from, through := t.rec.GapFrom, t.rec.GapThrough
	rows, more, err := c.introducedRead(ctx, code.IntroducedQuery{RepoID: t.e.id, After: pos(from),
		Through: pos(through)}, through)
	if errors.Is(err, code.ErrRepoNotFound) {
		c.backfillGone(ctx, t, err)
		return nil
	}
	if err != nil {
		return err
	}
	upTo := through
	if more {
		// The last position may go on past the page: left to the next.
		last := rows[len(rows)-1].Position.Packed()
		upTo = last - 1
		rows = slices.DeleteFunc(rows, func(r code.IntroducedRow) bool { return r.Position.Packed() == last })
	}
	moveGap := func(next *provenanceState) bool {
		if !next.gapped() || next.GapFrom != from || upTo > next.GapThrough {
			return false
		}
		next.GapFrom = upTo
		if next.GapFrom >= next.GapThrough {
			next.GapFrom, next.GapThrough = 0, 0
		}
		return true
	}
	return c.commitPage(ctx, t, from, upTo, rows, moveGap)
}

// historyPage reads one page of t's history — every push at or below From,
// newest first — and commits what it read whole: a run over (the lowest
// position read whole less one, From], From moved down to it; where the read
// ended, Complete, with the oldest push the rows hold.
func (c *Cache) historyPage(ctx context.Context, t *backfillTask) error {
	from := t.rec.From
	rows, more, err := c.introducedRead(ctx, code.IntroducedQuery{RepoID: t.e.id, Before: pos(from + 1),
		Descending: true}, from)
	if errors.Is(err, code.ErrRepoNotFound) {
		c.backfillGone(ctx, t, err)
		return nil
	}
	if err != nil {
		return err
	}
	newFrom, oldest := from, from
	switch {
	case more:
		low := rows[len(rows)-1].Position.Packed()
		newFrom = low
		rows = slices.DeleteFunc(rows, func(r code.IntroducedRow) bool { return r.Position.Packed() == low })
	case len(rows) > 0:
		oldest = rows[len(rows)-1].Position.Packed()
		newFrom = oldest - 1
	}
	slices.Reverse(rows)
	moveFrom := func(next *provenanceState) bool {
		if next.From != from {
			return false
		}
		next.From = newFrom
		if !more {
			next.Complete, next.Oldest = true, oldest
		}
		return true
	}
	return c.commitPage(ctx, t, newFrom, from, rows, moveFrom)
}

// horizonPage asks the rows for the oldest push they keep — one row of an
// ascending read from the log's start — and commits it as t's index's horizon:
// every run wholly at or below it retired at once, and From raised to just
// below it, which the next merge drops the records of; From then IS the start
// of what the rows keep, so the index is Complete.
func (c *Cache) horizonPage(ctx context.Context, t *backfillTask) error {
	read := code.ReadQuery{Kind: code.ReadIntroduced, Limit: 1,
		Introduced: &code.IntroducedQuery{RepoID: t.e.id},
		Fresh:      statelog.FloorAt(statelog.ReadSession, pos(t.rec.From))}
	page, err := c.fetch.Read(ctx, read)
	if errors.Is(err, code.ErrRepoNotFound) {
		c.backfillGone(ctx, t, err)
		return nil
	}
	if err != nil {
		return err
	}
	now := c.now()
	oldest := int64(0)
	if len(page.Introduced) > 0 {
		row := page.Introduced[0]
		if err := backfillRow(row, *read.Introduced); err != nil {
			return fmt.Errorf("%w: %w", ErrBadSnapshot, err)
		}
		oldest = row.Position.Packed()
	}
	ok := c.commitIndex(t.e, t.inc, t.gen, func(next *provenanceState) bool {
		next.OldestAt = now
		if oldest == 0 {
			return true
		}
		next.Oldest = oldest
		// Never past Through, which only a round moves: a horizon above it —
		// every push the index could hold expired — leaves an index of
		// nothing, still level with its incarnation.
		if floor := min(oldest-1, next.Through); floor > next.From {
			next.From, next.Complete = floor, true
			next.Runs = slices.DeleteFunc(next.Runs, func(r provenanceRun) bool { return r.Through <= floor })
			if next.gapped() && next.GapFrom < floor {
				next.GapFrom = floor
				if next.GapFrom >= next.GapThrough {
					next.GapFrom, next.GapThrough = 0, 0
				}
			}
		}
		return true
	})
	if ok {
		c.backfillStored(ctx, t)
	}
	return nil
}

// commitPage writes rows — a page's pushes read whole over (from, through], in
// log order — as one run and commits it into t's index with change, which
// also says where the read leaves the index; with no rows, change alone. A
// change that no longer applies — the index moved under the read in a way the
// read did not see — is thrown away with its run, and the next step reads
// again from the index as it stands.
func (c *Cache) commitPage(ctx context.Context, t *backfillTask, from, through int64, rows []code.IntroducedRow,
	change func(next *provenanceState) bool) error {

	if len(rows) == 0 {
		if c.commitIndex(t.e, t.inc, t.gen, change) {
			c.backfillStored(ctx, t)
		}
		return nil
	}
	built, err := buildRun(from, through, rows)
	if err != nil {
		return fmt.Errorf("%w: %w", ErrBadSnapshot, err)
	}
	s, err := c.stageRun(built.write)
	if err != nil {
		return err
	}
	ok, err := c.storeRun(t.e, t.inc, t.gen, s, func(next *provenanceState, run provenanceRun) bool {
		if !change(next) {
			return false
		}
		next.Runs = withRun(next.Runs, run)
		return true
	})
	if err != nil {
		return err
	}
	if ok {
		if c.metrics != nil {
			c.metrics.Add(metrics.CodeCacheProvenanceBackfilled, uint64(len(rows)), nil)
		}
		c.backfillStored(ctx, t)
	}
	return nil
}

// backfillGone drops t's index: the rows no longer hold its repository,
// asked at a floor that held it — purged, which the repository's next round
// reads at the log's head and takes the copy out of the cache for.
func (c *Cache) backfillGone(ctx context.Context, t *backfillTask, why error) {
	c.log.Info("code_cache_provenance_dropped", "repo", t.e.id, "incarnation", t.inc.n, "why", why.Error())
	if c.dropIndex(t.e, t.inc, t.gen) {
		c.backfillStored(ctx, t)
	}
}
