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
	"github.com/crewlet/crewlet/internal/gitlet"
	"github.com/crewlet/crewlet/internal/queue/topics"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/statelog/metrics"
)

// The backfill: one loop a node (Start), behind the serving, doing for the
// index of pushes (provenance.go) what no round does, one incarnation's step
// at a time, in this order (backfillDue):
//
//  1. CHECK the runs a previous process left, or a rebuild linked unvouched,
//     against their trailers (checkRuns); until then readers go without it.
//  2. MERGE runs (mergeDue), so each push is written O(log n) times.
//  3. Read a page of the GAP a full round passed over, on every holder.
//  4. Read a page of HISTORY, only where Cache.WantHistory marks it wanted:
//     once per fleet, by the repository's home (M3-decisions M3-O20).
//  5. Every HorizonEvery, drop what lies below the rows' oldest push, so the
//     index never outgrows the push history the rows keep (horizonPage).

// Within a kind, the most recently used repository first. The step comes from
// the SCHEDULE (schedule.go), never a walk of every copy under the lock every
// request takes: a pick costs the copies whose work changed since the last,
// and O(log n) in the rest.

// EVERY READ IS AT A FLOOR THE INDEX HAS SEEN: SESSION at the gap's end, or at
// the index's From for history and horizon, never stale. A data node behind
// the log holds a PREFIX of the rows, and a page from it would hold a range's
// pushes in part, which a run then claims to hold whole.

// A PAGE IS CUT WHERE A RECORD ENDS: group commit lands up to
// gitlet.MaxPushesPerRecord pushes at one position, and a run is complete over
// its range, so a page ending inside a record leaves it to the next page, and
// a page of one position is read on until the record ends (introducedRead).

// PACED BY THE FLEET: after each page the loop waits BackfillIdle × ⌈live
// Gitlet nodes ÷ live data nodes⌉ × the page's duration (fleetRatio), the
// counts read again after every page (Options.Fleet), so the fleet's
// backfills hold under half of one reader of each data node. Counts that
// cannot be read are waited on, never guessed.

// A FAILED STEP WAITS ON ITS OWN REPOSITORY: it is taken again once that
// repository's window ends (entry.bfRetryAt, doubling from BackfillRetry to
// BackfillRetryCeiling) while every other repository's steps go on: one
// shared wait let one repository's unservable rows hold every gap open, and a
// failed merge answered as done was taken again for ever. Only failures
// spanning repositories (backfillStreak) make the loop itself wait: the
// estate unreachable, or a disk refusing every write. A run the index names
// that is gone from the disk is LOST, and its check asks the repair round.

// backfillWork is a kind of step, in the order the loop takes them.
type backfillWork uint8

const (
	workCheck backfillWork = iota + 1
	workMerge
	workGap
	workHistory
	workHorizon
)

// backfillJitter spreads each failure's wait a fifth either way, the code
// reconciler's spread (engine's codeReposRetryJitter), so Gitlet nodes whose
// data nodes failed together do not ask them again on the same second.
const backfillJitter = 0.2

// backfillTask is one step the backfill picked: the incarnation, PINNED as no
// use, and the index it was picked from, by generation (gen) and as it stood
// (rec), so a step commits only into the index it read.
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

// backfillDone is what one step did: its kind and repository, whether it did
// anything, a page read's duration (the pace the loop owes), its failure and
// the retry its repository waits; or, with nothing due, when the next step
// falls due by time (zero for none).
type backfillDone struct {
	work  backfillWork
	repo  string
	did   bool
	page  time.Duration
	next  time.Time
	err   error
	retry time.Duration
}

// backfillStreak is the failed steps in a row: how many, the first's
// repository, and how many failed since they spanned two repositories, which
// the loop as a whole waits on.
type backfillStreak struct {
	n      int
	first  string
	spread int
}

// fail counts a failed step of repo and answers how many failures the loop
// itself waits on: none while all are of one repository, which its own
// window holds back.
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

// backfiller is the backfill's loop until ctx ends or the cache stops: a step
// at a time, each page's pace paid before the next, and with nothing due,
// parked, holding nothing of the directory, until a kick or the next step's
// time (a horizon, a retry window's end). A kick never cuts short a pace or a
// wait after failures: new work is no reason to read the estate sooner.
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

// fleetRatio is ⌈live Gitlet nodes ÷ live data nodes⌉ from the fleet's
// placements now, each count at least one: a node with no fleet is both.
func (c *Cache) fleetRatio() (int, error) {
	git, data, err := c.fleet()
	if err != nil {
		return 0, fmt.Errorf("cache: counting the fleet's git and data nodes: %w", err)
	}
	git, data = max(git, 1), max(data, 1)
	return (git + data - 1) / data, nil
}

// backfillWait waits d, or a kick where kickable (a kick alone at d zero), and
// answers false once ctx ends or the cache stops.
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

// backfillStep does the next step due, under ctx and the cache's lifetime
// (Stop ends it), and answers what it did. A failure widens its repository's
// retry window and a success closes it (entry.bfRetryAt); a step its context
// ended does neither. Either way its copy is marked (backfillDirty) in the
// hold that moves the window: a step changes the copy's work.
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
		c.backfillDirty(t.e)
		t.e.mu.Unlock()
		c.trash(gone)
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

// backfillStored persists the incarnation's state, which names its runs, after
// a step committed into t's index, and the runs it no longer names go with it.
// A state that cannot be written leaves them named by the state on the disk
// until any writer writes the next; a retired incarnation's is not written,
// its directory leaving with its last pin.
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

// nextBackfill picks the next step due, pinned as no use: the first kind any
// copy has due, most recently used first, from the schedule's queues
// (schedule.go) once every copy marked since the last pick is placed again;
// or nil, and when the next falls due by time (zero for none). The queues'
// first copy is looked at again under its own lock, and that look, never the
// queue, is what it pins.
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
		// Its work moved since it was placed, so a mark of it is pending:
		// placed as this look says, or left to the next pick's look.
		s.mu.Lock()
		s.placeLooked(e, d)
		s.mu.Unlock()
		e.mu.Unlock()
	}
}

// dueWork is a look at one copy's backfill (backfillDue): the first kind of
// step due, or none and when the next falls due by time (zero for never), its
// last use, whether it counts as behind, and what a step of it takes.
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

// The look marks a record's runs in one machine word, so a record holds at
// most 64: a compile error past it.
const _ uint64 = 1 << (MaxProvenanceRuns - 1)

// backfillDue looks at e's backfill: the first step its current index has
// due, in the backfill's order, or none and when the next falls due (its
// horizon, or its retry window's end, before which it has none); nothing for
// a copy the backfill does not work on (dead, no converged current
// incarnation, no index). e.mu is held. It reads no disk and allocates
// nothing: a run is unchecked when no vouch is held for its name, and a vouch
// for another identity than the file's is forgotten by the reader that meets
// it (mapRun), which marks the copy.
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

// runMarks is which of rec's runs inc holds a vouch for, and which neither a
// vouch nor a failed check (those a check reads: a failed one waits for the
// repair round that drops the index), a bit each by index in rec.Runs; e.mu is
// held. Names are spelled into a stack buffer, so it allocates nothing.
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

// mergeDue is the runs of rec, [lo, hi) by index, the backfill merges next
// (adjacent, on one side of its gap, each vouched), or hi zero for none:
//
//  1. the cheapest span of ProvenanceMergeWidth runs that is BALANCED: none
//     holding more than ProvenanceMergeFactor times the others together;
//  2. at MaxProvenanceRuns with no such span, the cheapest adjacent pair
//     whatever their sizes, so a fold finds room.
//
// The width keeps the small runs folds write from merging on nearly every
// push; balance keeps a run from being rewritten for under half its own pushes
// again. Not a pair rule (neighbours within a factor): a burst's runs of every
// size met none, and the index filled to its bound.
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

// checkRuns holds each unvouched run of t to its trailer (CheckRunTrailer), a
// piece at a time under a read slot that waits its turn however long, as the
// scrub's does (scrubHold): nobody waits on it. A run that passes is vouched;
// one that fails or cannot be read goes on the copy's repair mark at the
// identity it failed at, and a repair round is asked, which drops the index
// (dropDamaged: eventFailedCheck), never under a check of the repository,
// whose release asks the round again. A run gone while its index names it is
// LOST and goes on the mark too, as indexFull treats one: answered "replaced",
// it would be found unchecked and taken again for ever. One its index no
// longer names decides nothing.
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
		// Counted under its declaration's file kind (Owned.File), as every
		// derived file's check is.
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
			c.backfillDirty(t.e)
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
		c.backfillDirty(t.e)
		if t.e.current == t.inc {
			c.repairRound(t.e)
		}
		t.e.mu.Unlock()
		return
	}
}

// namesRun reports whether t's incarnation's index names run r now; a run
// number is never reused in an incarnation (allocRun), so a run named is the
// file the check read. e.mu is not held.
func (c *Cache) namesRun(t *backfillTask, r provenanceRun) bool {
	t.e.mu.Lock()
	defer t.e.mu.Unlock()
	rec := t.inc.record()
	return rec != nil && rec.named(r.N)
}

// backfillPage reads and commits one page of t's gap, history or horizon,
// and answers the read's failure.
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

// introducedRead reads q's pushes at SESSION over floor, page after page
// until the rows hold more than one position or the read ends, so every
// position's pushes but the last are whole, and answers whether the read goes
// on. gitlet.ErrRepoNotFound at a floor that held the repository means it was
// purged.
func (c *Cache) introducedRead(ctx context.Context, q gitlet.IntroducedQuery, floor int64) ([]gitlet.IntroducedRow,
	bool, error) {

	read := gitlet.ReadQuery{Kind: gitlet.ReadIntroduced, Introduced: &q,
		Fresh: statelog.FloorAt(statelog.ReadSession, pos(floor))}
	var rows []gitlet.IntroducedRow
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
func inOrder(a, b gitlet.IntroducedRow, descending bool) bool {
	c := cmp.Or(cmp.Compare(a.Position.Packed(), b.Position.Packed()), strings.Compare(a.PushID, b.PushID))
	if descending {
		return c > 0
	}
	return c < 0
}

// backfillRow holds a row of q's read to what a run encodes and to q's range
// (a code-log position inside it, moves of refs to object ids, commits that
// are object ids) before anything of it is written.
func backfillRow(row gitlet.IntroducedRow, q gitlet.IntroducedQuery) error {
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
// commits a run over (GapFrom, the last position read whole], moving the gap
// up past it, closed where the read ended.
func (c *Cache) gapPage(ctx context.Context, t *backfillTask) error {
	from, through := t.rec.GapFrom, t.rec.GapThrough
	rows, more, err := c.introducedRead(ctx, gitlet.IntroducedQuery{RepoID: t.e.id, After: pos(from),
		Through: pos(through)}, through)
	if errors.Is(err, gitlet.ErrRepoNotFound) {
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
		rows = slices.DeleteFunc(rows, func(r gitlet.IntroducedRow) bool { return r.Position.Packed() == last })
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

// historyPage reads one page of t's history (pushes at or below From, newest
// first) and commits a run over (the lowest position read whole less one,
// From], moving From down to it; where the read ended, Complete, with the
// oldest push the rows hold.
func (c *Cache) historyPage(ctx context.Context, t *backfillTask) error {
	from := t.rec.From
	rows, more, err := c.introducedRead(ctx, gitlet.IntroducedQuery{RepoID: t.e.id, Before: pos(from + 1),
		Descending: true}, from)
	if errors.Is(err, gitlet.ErrRepoNotFound) {
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
		rows = slices.DeleteFunc(rows, func(r gitlet.IntroducedRow) bool { return r.Position.Packed() == low })
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

// horizonPage asks the rows for their oldest push (one row of an ascending
// read) and commits it as t's index's horizon: every run wholly at or below it
// retired, and From raised to just below it (the next merge drops the records
// below From), so From IS the start of what the rows keep and the index is
// Complete.
func (c *Cache) horizonPage(ctx context.Context, t *backfillTask) error {
	read := gitlet.ReadQuery{Kind: gitlet.ReadIntroduced, Limit: 1,
		Introduced: &gitlet.IntroducedQuery{RepoID: t.e.id},
		Fresh:      statelog.FloorAt(statelog.ReadSession, pos(t.rec.From))}
	page, err := c.fetch.Read(ctx, read)
	if errors.Is(err, gitlet.ErrRepoNotFound) {
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
		// Never past Through, which only a round moves: a horizon above it
		// leaves an empty index, still level with its incarnation.
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

// commitPage writes rows (a page's pushes read whole over (from, through], in
// log order) as one run and commits it into t's index with change, which moves
// the index past the read; with no rows, change alone. A change that no longer
// applies, the index having moved under the read, is thrown away with its run,
// and the next step reads again.
func (c *Cache) commitPage(ctx context.Context, t *backfillTask, from, through int64, rows []gitlet.IntroducedRow,
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

// backfillGone drops t's index of a repository purged (absent at a floor that
// held it); its next round, read at the log's head, takes the copy out.
func (c *Cache) backfillGone(ctx context.Context, t *backfillTask, why error) {
	c.log.Info("code_cache_provenance_dropped", "repo", t.e.id, "incarnation", t.inc.n, "why", why.Error())
	if c.dropIndex(t.e, t.inc, t.gen) {
		c.backfillStored(ctx, t)
	}
}
