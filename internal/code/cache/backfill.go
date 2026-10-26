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
//  2. MERGE two adjacent runs: the cheapest pair whose pushes are within
//     ProvenanceMergeFactor of each other — git's own geometric rule, so runs
//     double as they merge and stay O(log n), each push written O(log n)
//     times over its life — or, at MaxProvenanceRuns, the cheapest pair
//     whatever their sizes, so a round's fold finds room.
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
// people are asking about.
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
// paced on a guess; a page the estate could not serve is waited on and asked
// again, the wait doubling from BackfillRetry to BackfillRetryCeiling.

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
	// pair is a merge's first run, by its index in rec.Runs.
	pair int
	// unchecked is a check's runs: those of rec no vouch is held for.
	unchecked []provenanceRun
}

// backfillDone is what one step did: anything (did); a page read in page —
// what the loop then owes a pace; a read that failed (err), which the loop
// waits on; or nothing due, and when the next horizon falls due (next, zero
// for none).
type backfillDone struct {
	did  bool
	page time.Duration
	next time.Time
	err  error
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
// until it is kicked or the next horizon falls due. A wait for a pace or after
// a failure is not cut short by a kick: new work is no reason to read the
// estate sooner.
func (c *Cache) backfiller(ctx context.Context) {
	var (
		failures int
		owed     time.Duration
	)
	for {
		if owed > 0 {
			ratio, err := c.fleetRatio()
			if err != nil {
				failures++
				c.log.Warn("code_cache_backfill_unpaced", "error", err)
				if !c.backfillWait(ctx, c.backfillRetry(failures), false) {
					return
				}
				continue
			}
			failures = 0
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
			failures++
			c.log.Warn("code_cache_backfill_failed", "error", done.err)
			ok = c.backfillWait(ctx, c.backfillRetry(failures), false)
		case done.page > 0:
			failures, owed, ok = 0, done.page, true
		case done.did:
			failures, ok = 0, ctx.Err() == nil && c.life.Err() == nil
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
// cache's lifetime, which Stop ends — and answers what it did.
func (c *Cache) backfillStep(ctx context.Context) backfillDone {
	t, next, moved := c.nextBackfill()
	switch {
	case moved:
		// What the walk picked moved before it was pinned: walk again.
		return backfillDone{did: true}
	case t == nil:
		return backfillDone{next: next}
	}
	sctx, cancel := context.WithCancel(ctx)
	defer cancel()
	//nolint:contextcheck // The cache's lifetime ends the step too: Stop must end its reads however long ctx lives.
	stop := context.AfterFunc(c.life, cancel)
	defer stop()
	defer func() {
		t.e.mu.Lock()
		gone := c.unpin(t.e, t.inc)
		t.e.mu.Unlock()
		c.trash(gone)
	}()
	switch t.work {
	case workCheck:
		c.checkRuns(sctx, t)
		return backfillDone{did: true}
	case workMerge:
		retired, ok, err := c.mergePair(sctx, t.e, t.inc, t.gen, t.rec, t.pair)
		switch {
		case err != nil:
			c.log.Warn("code_cache_provenance_merge_failed", "repo", t.e.id, "incarnation", t.inc.n, "error", err)
		case ok:
			c.backfillStored(sctx, t, retired)
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
// incarnation's state, which names its runs — and then moves what the index
// no longer names into trash/. A state that could not be written leaves the
// retired runs where they are, named by the state on disk, which a restart
// reads; a retired incarnation's state is not written at all, its directory
// leaving with its last pin.
func (c *Cache) backfillStored(ctx context.Context, t *backfillTask, retired []string) {
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
	c.trashRuns(t.e, t.inc, retired)
	c.setGauges()
}

// nextBackfill picks the next step due — the first kind of step any current
// incarnation has due, and among those the most recently used — pinned as no
// use; or nil, and when the next horizon falls due (zero for none) — or moved,
// where what it picked changed before it could pin it. It sets
// crewlet.code.cache.provenance.behind from the same walk.
func (c *Cache) nextBackfill() (*backfillTask, time.Time, bool) {
	now := c.now()
	var (
		best   *backfillTask
		bestAt time.Time
		next   time.Time
		behind int
	)
	c.mu.Lock()
	for _, e := range c.entries {
		e.mu.Lock()
		inc := e.current
		if e.dead || inc == nil || inc.retired || !inc.converged || inc.record() == nil {
			e.mu.Unlock()
			continue
		}
		rec := inc.record()
		if rec.gapped() || (e.wantHistory && !rec.Complete) {
			behind++
		}
		t, due := c.backfillDue(e, inc, rec, now)
		// The copy's last use is read before its lock goes, as the janitor's
		// eviction reads it: every reader's pin and release writes it under
		// e.mu (used), so a read after the unlock races each request.
		used := inc.lastUsed
		e.mu.Unlock()
		switch {
		case t == nil:
			if !due.IsZero() && (next.IsZero() || due.Before(next)) {
				next = due
			}
		case best == nil || t.work < best.work ||
			t.work == best.work && (used.After(bestAt) ||
				used.Equal(bestAt) && strings.Compare(e.id, best.e.id) < 0):
			best, bestAt = t, used
		}
	}
	c.mu.Unlock()
	if c.metrics != nil {
		c.metrics.Set(metrics.CodeCacheProvenanceBehind, float64(behind), nil)
	}
	if best == nil {
		return nil, next, false
	}
	e := best.e
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.dead || e.current != best.inc || best.inc.retired || best.inc.record() != best.rec ||
		best.inc.provGen != best.gen {
		return nil, time.Time{}, true
	}
	best.inc.pins++
	return best, time.Time{}, false
}

// backfillDue is the step of inc's index rec the backfill takes first, or
// nil and when its horizon falls due; e.mu is held. Nothing here reads the
// disk: a run is unchecked when no vouch is held for its name — a vouch held
// for another identity than the file has is forgotten by the reader that meets
// it (mapRun), which makes it unchecked here.
func (c *Cache) backfillDue(e *entry, inc *incarnation, rec *provenanceState, now time.Time) (*backfillTask,
	time.Time) {

	t := &backfillTask{e: e, inc: inc, gen: inc.provGen, rec: rec}
	vouched := func(r provenanceRun) bool {
		_, ok := inc.vouched.derived[runRel(r.N)]
		return ok
	}
	for _, r := range rec.Runs {
		// A run its check found failing waits for the repair round that
		// drops the index, and is not checked again meanwhile.
		if _, failed := inc.repair.found[runRel(r.N)]; !vouched(r) && !failed {
			t.unchecked = append(t.unchecked, r)
		}
	}
	horizon := rec.OldestAt.Add(HorizonEvery)
	switch {
	case len(t.unchecked) > 0:
		t.work = workCheck
	case mergeDue(rec, vouched) >= 0:
		t.work, t.pair = workMerge, mergeDue(rec, vouched)
	case rec.gapped():
		t.work = workGap
	case e.wantHistory && !rec.Complete:
		t.work = workHistory
	case rec.OldestAt.IsZero() || !now.Before(horizon):
		t.work = workHorizon
	default:
		return nil, horizon
	}
	return t, time.Time{}
}

// mergeDue is the pair of rec's runs, by the first's index, the backfill
// merges next — each run ok to read: the cheapest adjacent pair whose pushes
// are within ProvenanceMergeFactor of each other, or, with rec at
// MaxProvenanceRuns, the cheapest adjacent pair whatever their sizes — or -1.
func mergeDue(rec *provenanceState, ok func(provenanceRun) bool) int {
	if len(rec.Runs) >= MaxProvenanceRuns {
		return cheapestPair(rec, ok, nil)
	}
	return cheapestPair(rec, ok, func(i int) bool {
		a, b := rec.Runs[i].Pushes, rec.Runs[i+1].Pushes
		return max(a, b) <= ProvenanceMergeFactor*min(a, b)
	})
}

// checkRuns holds every run of t no vouch is held for to its own trailer
// (CheckRunTrailer), one piece at a time under a read slot that waits its turn
// however long, as the scrub's does (scrubHold) — nobody waits on it. A run
// that passes is vouched for; one that fails, or whose read fails, goes on the
// copy's repair mark at the identity it failed at, with a repair round asked,
// which drops the index (dropDamaged: eventFailedCheck) — never under a check
// of the repository, whose release asks the round again. A run replaced while
// it was read decides nothing, and the next pass reads its successor.
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
		case err == nil && v == hashNone:
			c.countCheck(kind, checkReplaced, triggerFirst)
			continue
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
			"trigger", string(triggerFirst), "action", c.image.repairAction(rel), "error", err)
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
	retired, ok := c.commitIndex(t.e, t.inc, t.gen, func(next *provenanceState) bool {
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
		c.backfillStored(ctx, t, retired)
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
		if retired, ok := c.commitIndex(t.e, t.inc, t.gen, change); ok {
			c.backfillStored(ctx, t, retired)
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
	retired, ok, err := c.storeRun(t.e, t.inc, t.gen, s, func(next *provenanceState, run provenanceRun) bool {
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
		c.backfillStored(ctx, t, retired)
	}
	return nil
}

// backfillGone drops t's index: the rows no longer hold its repository,
// asked at a floor that held it — purged, which the repository's next round
// reads at the log's head and takes the copy out of the cache for.
func (c *Cache) backfillGone(ctx context.Context, t *backfillTask, why error) {
	c.log.Info("code_cache_provenance_dropped", "repo", t.e.id, "incarnation", t.inc.n, "why", why.Error())
	if retired := c.dropIndex(t.e, t.inc, t.gen); retired != nil {
		c.backfillStored(ctx, t, retired)
	}
}
