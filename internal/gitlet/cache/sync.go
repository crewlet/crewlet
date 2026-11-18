package cache

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/crewlet/crewlet/internal/gitlet"
	"github.com/crewlet/crewlet/internal/gitlet/gitproc"
	"github.com/crewlet/crewlet/internal/queue/topics"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/statelog/metrics"
)

// SYNC: the one function that writes a cache, by CONVERGENCE (derive the
// target from rows, make the directory equal to it), and the single flight
// that shares its rounds (package doc, "One sync at a time, shared"). A
// LINEARIZABLE caller joins only a round that starts after it arrived: a
// running round's barrier may predate the write it is about to read.

// round is one convergence a repository's callers share.
type round struct {
	// fresh is the round's merged requirement: what every caller that
	// joined it asked for at once. Fixed once the round runs.
	fresh statelog.Freshness
	// waiters is every caller answered by this round.
	waiters []*waiter
	// rebuild, when set, makes the round a REBUILD of the incarnation of
	// (rebuild.go): a whole read at linearizable into a new incarnation
	// beside it, which replaces it once converged. Fixed once the round runs.
	rebuild RebuildReason
	of      *incarnation
	// whole, on a rebuild, is the packs of `of` its asker has just held to
	// their rows, which the rebuild links in rather than downloads
	// (seedRebuild). Fixed once the round runs.
	whole []string
	// found is what the round repaired in place of what its check, or an
	// earlier check or scrub still on the repair mark, found failing
	// (recordCheck), each sentence once: a check among its callers is handed
	// them (waiter.found), since its copy no longer holds them.
	found []string
	// did is what the round's applies did, summed: what a request's
	// in-place round answers its escalation by (applied.repaired).
	did applied
	// reader is the name the round's own git runs carry in its incarnation
	// (roundReader), minted as it first applies to it.
	reader roundReader
}

// roundReader is the reader a round's own git runs (the peel of the tags it
// sets) name in its incarnation (gitproc.Cmd.Repairer), so what they report is
// HELD for the round, not made a scrub at once (report.go, HELD FOR THE
// REQUEST), then SETTLED with the round's answer (settleReader): consumed by
// the repair the round asked of its own git's failure (repairDamaged), else
// handed to the ordinary rules (handOn). A scrub made due beside that repair
// would read the whole copy and race the repair to its pack.
type roundReader struct {
	inc *incarnation
	by  gitproc.Repairer
}

// settleReader ends rd, a round's reader: what its runs reported is consumed
// by the round's repair (counted reports{outcome=request}) when answered, and
// handed to the ordinary rules otherwise; either way its name is closed. e.mu
// is held.
func (c *Cache) settleReader(e *entry, rd roundReader, answered bool) {
	if rd.inc == nil {
		return
	}
	if answered {
		c.consume(rd.inc, rd.by)
	}
	c.handOn(e, rd.inc, rd.by)
}

// waiter is one caller of a round.
type waiter struct {
	// fresh is what the caller asked for, and rebuild that it is a Rebuild
	// caller: what a failed rebuild needs to hand every other caller on to
	// an ordinary round (Cache.run).
	fresh   statelog.Freshness
	rebuild bool
	// escalate: this round is the in-place repair, for this reason, of the
	// caller's pack or object damage of failedOn, a REQUEST's (Cache.Rebuild)
	// or a sync's own round's git's (damagedRound). Should it repair nothing,
	// a request goes to the scrub and past it the whole rebuild
	// (Cache.escalate), only while the round's incarnation IS failedOn; should
	// its git meet the damage again, the caller goes to the scrub too.
	escalate RebuildReason
	failedOn *incarnation
	// began is when the failing process began (Failure.Began): a repair
	// written into failedOn after it may be what ended the failure, so the
	// caller is answered by the guard (standOrSync), not handed to a rebuild.
	began time.Time
	// request: the caller is a REQUEST's repair (Cache.Rebuild), whose git
	// failed over a Synced it held and runs again over its answer, not a sync,
	// which only a converged round answers. So an in-place round that repaired
	// nothing hands a request to the scrub, and the guard answers it from the
	// copy as it stands; a sync is answered by its in-place round once that
	// converges, and the guard hands it to a round, never an unconverged copy.
	request bool
	// scrubbedOn is the incarnation the repair's SCRUB ran on for this caller
	// and found damaged (Cache.escalate), and whole the packs it found whole.
	// Should the round after it meet the damage again, the caller goes to the
	// whole rebuild, seeded with whole (damagedRound).
	scrubbedOn *incarnation
	whole      []string
	// asIs: the caller is such a request, answered from the copy as it
	// stands once the round that may be repairing it has ended (asItStands):
	// with Kind SyncNone, whatever the round queued behind it read.
	asIs bool
	// checks: the caller is a CHECK of the copy (Verification.Run), answered
	// with the damage its round's git names and the incarnation pinned
	// (damage, got), not the repair: only an operator's Repair repairs what a
	// check names.
	checks bool
	// noUse: the pin the caller is answered with is no use of the copy
	// (pinAs) — the indexer's look (Cache.indexSync).
	noUse  bool
	got    Synced
	err    error
	damage *damaged
	// found is what the round's own check found and repaired, handed to a
	// check (round.found).
	found []string
	// done is closed once the caller has its answer and its pin, taken under
	// e.mu before the close, so nothing can retire what it is handed before
	// it wakes. The caller's own channel, not its round's: a failed rebuild
	// queues it again.
	done chan struct{}
	// gone: the caller's context ended before a round answered it, so no
	// round pins anything for it or writes to it: it read what it had as it
	// left.
	gone bool
}

// newWaiter is a caller asking for fresh.
func newWaiter(fresh statelog.Freshness) *waiter {
	return &waiter{fresh: fresh, done: make(chan struct{})}
}

// Sync hands out repoID's directory, PINNED, holding every row up to a
// position at least as fresh as fresh asks: the one way to a repository's
// directory. Every caller defers the answer's Release.
//
// A caller the cache already meets (stale or consistent-prefix, or a session
// floor reached, of a repository converged in this process) is answered at
// once with no estate call (SyncNone); a linearizable one, or one with a lag
// bound only the estate can measure, never is. Any other joins a round. ctx
// bounds the caller's own wait only: the round runs on for everyone else.
func (c *Cache) Sync(ctx context.Context, repoID string, fresh statelog.Freshness) (Synced, error) {
	s, _, _, err := c.sync(ctx, repoID, fresh, false, true)
	return s, err
}

// indexSync is Sync at STALE for the indexer, its pin NO USE of the copy
// (pinAs): eviction orders by the readers' last use, and the indexes' trim by
// IndexTip's last search, never the indexer's.
func (c *Cache) indexSync(ctx context.Context, repoID string) (Synced, error) {
	s, _, _, err := c.sync(ctx, repoID, statelog.Freshness{Level: statelog.ReadStale}, false, false)
	return s, err
}

// Holds reports whether this cache holds a copy of repoID or is filling one:
// an incarnation (converged here, or found on the disk it booted over) or a
// running round. It reads memory alone and starts nothing.
//
// A Gitlet node asks it before a read it was not asked to FILL a copy for
// (gitlet.GitQuery.Home, ADR-0089): answering false, it starts nothing
// (gitlet.UnreadyNotHeld), so a read of a repository no node holds costs one
// download, by the walk's effective home, not one per node the walk reaches.
// A running round is held: a read joining it adds no download. Either race
// is sound: a copy evicted after a true is filled by the read's own sync, and
// a fill begun after a false is answered by another node meanwhile.
func (c *Cache) Holds(repoID string) bool {
	c.mu.Lock()
	e := c.entries[repoID]
	c.mu.Unlock()
	if e == nil {
		return false
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	return !e.dead && (e.current != nil || e.running != nil)
}

// sync is Sync, and for a CHECK (checks; Verification.Run) also the damage a
// round's own git met in the incarnation it answers pinned (nil if none;
// damagedRound) and what the round's own check repaired in place
// (round.found). use says whether the pin is a use of the copy (pinAs).
func (c *Cache) sync(ctx context.Context, repoID string, fresh statelog.Freshness, checks, use bool) (Synced,
	*damaged, []string, error) {

	if err := c.checkSync(repoID, fresh); err != nil {
		return Synced{}, nil, nil, err
	}
	for {
		e := c.entryFor(repoID)
		e.mu.Lock()
		if e.dead {
			// Purged since it was looked up: the next look makes a new one.
			e.mu.Unlock()
			continue
		}
		if s, ok := c.atOnce(e, fresh, use); ok {
			e.mu.Unlock()
			return s, nil, nil, nil
		}
		w := newWaiter(fresh)
		w.checks, w.noUse = checks, !use
		c.join(e, w)
		e.mu.Unlock()
		s, err := c.await(ctx, e, w)
		// Written before the close await waited on, or, for a caller that
		// left, never (Cache.run, damagedRound): no race with this read.
		return s, w.damage, w.found, err
	}
}

// await is w's answer, or ctx's end, whichever comes first. A caller that
// gives up leaves the round running for everyone else, and gives back the pin
// the round took for it if the answer raced its leaving.
func (c *Cache) await(ctx context.Context, e *entry, w *waiter) (Synced, error) {
	select {
	case <-w.done:
		return w.got, w.err
	case <-ctx.Done():
	}
	e.mu.Lock()
	select {
	case <-w.done:
		got := w.got
		e.mu.Unlock()
		got.Release()
	default:
		w.gone = true
		e.mu.Unlock()
	}
	return Synced{}, fmt.Errorf("cache: waiting for %s's sync: %w", e.id, context.Cause(ctx))
}

// answer hands w its answer and wakes it; e.mu is held. A caller that is
// gone is pinned nothing.
func (c *Cache) answer(e *entry, w *waiter, inc *incarnation, kind SyncKind, err error) {
	switch {
	case w.gone:
	case err != nil:
		w.err = err
	case w.asIs:
		w.got = c.pinAs(e, inc, SyncNone, !w.noUse)
	default:
		w.got = c.pinAs(e, inc, kind, !w.noUse)
	}
	close(w.done)
}

// checkSync refuses a request that is not a sync, before any estate call.
func (c *Cache) checkSync(repoID string, fresh statelog.Freshness) error {
	switch {
	case c.isStopped():
		return ErrStopped
	case !canonicalID(repoID):
		return fmt.Errorf("%w: repository id %q is not a canonical lowercase UUID (it becomes a path component)",
			ErrInvalid, repoID)
	case fresh.Level == "":
		return fmt.Errorf("%w: a sync states its freshness", gitlet.ErrBadLevel)
	case !fresh.Level.Valid():
		return fmt.Errorf("%w: %q is not a read level", gitlet.ErrBadLevel, fresh.Level)
	}
	// A floor on another log first, as the reader it would reach checks it:
	// a repository's rows are on the code log, so it names nothing to wait for.
	if err := fresh.OnlyOn(topics.CodeLogStream); err != nil {
		return fmt.Errorf("%w: a sync's floor is on another log than a repository's rows: %w",
			ErrInvalid, err)
	}
	if fresh.Level == statelog.ReadSession && floorOf(fresh).IsZero() {
		return fmt.Errorf("%w: a session sync names the position it must include", gitlet.ErrBadLevel)
	}
	return nil
}

// floorOf is a sync's floor: the position on the code log it names — the one
// log [Cache.checkSync] lets it name one on — or zero.
func floorOf(fresh statelog.Freshness) statelog.Position {
	return fresh.Floor(topics.CodeLogStream)
}

// entryFor is repoID's entry, made when there is none.
func (c *Cache) entryFor(repoID string) *entry {
	c.mu.Lock()
	defer c.mu.Unlock()
	e := c.entries[repoID]
	if e == nil {
		e = newEntry(repoID)
		c.entries[repoID] = e
	}
	return e
}

// atOnce answers a caller from what e holds, pinned, when that is all it
// needs; e.mu is held. use says whether the pin is a use of the copy (pinAs).
//
// Only from an incarnation CONVERGED IN THIS PROCESS: a log restored from an
// older copy keeps its identity (ADR-0083) and reuses positions for other
// records, so a previous process's state could serve a history the log no
// longer holds. A repository's first caller after a boot pays the full round
// D3 reads anyway.
func (c *Cache) atOnce(e *entry, fresh statelog.Freshness, use bool) (Synced, bool) {
	inc := e.current
	if inc == nil || !inc.converged || fresh.Bounded() {
		return Synced{}, false
	}
	floor := floorOf(fresh)
	switch fresh.Level {
	case statelog.ReadStale, statelog.ReadConsistentPrefix:
		if !floor.IsZero() && !reached(inc.at, floor) {
			return Synced{}, false
		}
	case statelog.ReadSession:
		if !reached(inc.at, floor) {
			return Synced{}, false
		}
	default:
		return Synced{}, false
	}
	return c.pinAs(e, inc, SyncNone, use), true
}

// reached reports whether at is at or past floor (both on the code log).
func reached(at, floor statelog.Position) bool {
	before, err := at.Before(floor)
	return err == nil && !before
}

// join adds w to the round that may answer it, starting one when none runs;
// e.mu is held. A linearizable or bounded caller joins the QUEUED round, as
// the running one's barrier may predate it; a session caller joins the
// running round only when it is a session round with a floor at or past its
// own. A stale or consistent-prefix caller with no floor got here because no
// incarnation is converged in this process, and any round answers it.
func (c *Cache) join(e *entry, w *waiter) {
	if r := e.running; r != nil && answers(r.fresh, w.fresh) {
		r.waiters = append(r.waiters, w)
		return
	}
	c.queue(e, w)
	c.start(e)
}

// queue adds w to the round queued behind the one running, making it when
// there is none; e.mu is held.
func (c *Cache) queue(e *entry, w *waiter) {
	if e.next == nil {
		e.next = &round{fresh: w.fresh}
	} else {
		e.next.fresh = merge(e.next.fresh, w.fresh)
	}
	e.next.waiters = append(e.next.waiters, w)
}

// answers reports whether a round already running at have answers a caller
// that arrived asking for want.
func answers(have, want statelog.Freshness) bool {
	if want.Level == statelog.ReadLinearizable || want.Bounded() {
		return false
	}
	switch want.Level {
	case statelog.ReadSession:
		return have.Level == statelog.ReadSession && reached(floorOf(have), floorOf(want))
	default:
		return floorOf(want).IsZero()
	}
}

// strength orders the read levels as statelog lists them, strongest first:
// a merged round reads at the strongest level any of its callers asked for.
func strength(l statelog.ReadLevel) int {
	return len(statelog.ReadLevels) - slices.Index(statelog.ReadLevels, l)
}

// merge is the requirement of a round two callers share: the stronger level,
// the later floor, and the smaller of each lag bound either set.
func merge(a, b statelog.Freshness) statelog.Freshness {
	out := a
	if strength(b.Level) > strength(a.Level) {
		out.Level = b.Level
	}
	if floor := floorOf(b); !floor.IsZero() && (floorOf(out).IsZero() || !reached(floorOf(out), floor)) {
		out.Floors = []statelog.Position{floor}
	}
	out.MaxLag = smallerSet(a.MaxLag, b.MaxLag)
	out.MaxLagSeq = smallerSet(a.MaxLagSeq, b.MaxLagSeq)
	return out
}

// smallerSet is the smaller of two bounds, a zero being no bound.
func smallerSet[T time.Duration | uint64](a, b T) T {
	switch {
	case a == 0:
		return b
	case b == 0:
		return a
	}
	return min(a, b)
}

// start makes the queued round the running one when none runs; e.mu is held.
func (c *Cache) start(e *entry) {
	if e.running != nil || e.next == nil {
		return
	}
	r := e.next
	e.next, e.running = nil, r
	go c.run(e, r)
}

// run is one round, on a goroutine of the cache's own: converge under the
// entry's write lock (entry.write), answer every caller still waiting, each
// pinned before it is woken, and start the round queued behind it. A FAILED
// REBUILD answers only its own callers: those of a round it was queued over
// (queueRebuild) asked for a sync, so they are queued again on the
// incarnation still serving unless the repository was purged, and the failure
// refuses the next Rebuild of it (failedRebuild). DAMAGE the round's own git
// named in its incarnation is answered by its REPAIR (damagedRound): answered
// with the failure, no caller would hold a Synced to ask one with, and every
// later round would fail the same. A pack named in words (hinted) is hashed
// after the round ends and lets the write lock go (confirmRound; package doc,
// "Damage is repaired once, in place where nothing is lost").
func (c *Cache) run(e *entry, r *round) {
	inc, kind, err := c.converging(e, r)
	if c.knobs != nil && c.knobs.roundEnding != nil {
		c.knobs.roundEnding(e.id)
	}
	var (
		h    *hinted
		held Synced
	)
	if errors.As(err, &h) {
		e.mu.Lock()
		held = c.pin(e, h.inc)
		e.running = nil
		c.start(e)
		e.mu.Unlock()
		err = c.confirmRound(e, h)
	}
	e.mu.Lock()
	var (
		d      *damaged
		repair roundRepair
	)
	// replaced: the round's incarnation stopped being the entry's while its
	// hint was hashed, or its hints confirmed nothing and a REPAIR was
	// written into the copy after its git began (predates), a request's or
	// the scrubber's. Either way its git may have failed on what no longer
	// serves: its callers are synced again.
	replaced := h != nil && !e.dead &&
		(e.current != h.inc || !errors.As(err, new(*damaged)) && predates(h.inc, h.began()))
	if r.rebuild == "" && errors.As(err, &d) && !e.dead && e.current == d.inc {
		repair = c.damagedRound(e, r, d)
	}
	c.settleReader(e, r.reader, repair.answers)
	requeue := r.rebuild != "" && err != nil && !e.dead
	if requeue {
		failedRebuild(r.of, err)
	}
	// An IN-PLACE ROUND (Cache.Rebuild, damagedRound) that converged repaired
	// what its callers asked, or found nothing to. A REQUEST it repaired
	// nothing for goes on to the scrub and past it the whole rebuild
	// (Cache.escalate), not answered from the copy that failed it; a sync is
	// answered either way, a converged round being all it asked for.
	var escalating []*waiter
	requeued := func(w *waiter) bool { return (replaced || requeue && !w.rebuild) && !w.gone }
	repairs := func(w *waiter) bool {
		return !requeued(w) && w.escalate != "" && w.failedOn == inc && err == nil && r.rebuild == ""
	}
	asks := func(w *waiter) bool { return repairs(w) && w.request }
	if r.did.repaired() && r.did.reinstalled == 0 && r.did.restored == 0 &&
		slices.ContainsFunc(r.waiters, repairs) {
		// COUNTED BY WHAT IT DID, not its mark: an in-place round that only
		// dropped is a derived file's repair (one that put a pack in was
		// counted `stored` by its apply), and BEFORE anybody is answered, so
		// an answered caller finds the count.
		c.rebuilt(RebuildDerived)
	}
	for _, w := range r.waiters {
		if requeued(w) {
			c.queue(e, w)
			continue
		}
		if asks(w) && !r.did.repaired() && !w.gone {
			escalating = append(escalating, w)
			continue
		}
		if w.checks && !w.gone {
			// Only to a check still waiting: one that left read what it had
			// outside e.mu, so a write now races it.
			w.found = r.found
		}
		c.answer(e, w, inc, kind, err)
	}
	if e.running == r {
		e.running = nil
	}
	// BETWEEN TWO ROUNDS, the one moment a repository read without a pause is
	// sure to offer: the run a pass found it due for and a round held back.
	c.startOwed(e)
	c.start(e)
	// THE MAINTAINER IS WOKEN ONCE NO ROUND HOLDS THE REPOSITORY: a pass
	// starts no run on one whose round runs (runnable), so a kick from inside
	// the round would leave work due at once to the next tick, MidxDebounce
	// later. A round queued behind this one owes it on.
	wake := c.owedWake(e)
	e.mu.Unlock()
	if wake {
		c.kickMaintainer()
	}
	// Let go only now: every caller holds its own pin or is queued again,
	// so the copy was never evictable between the hash and their answers.
	held.Release()
	if repair.quit != nil {
		// COUNTED AND LOGGED BEFORE IT IS ANSWERED, as Rebuild's refusal is,
		// so a woken caller finds both; outside e.mu, which would hold the
		// entry through a log write.
		c.report(*repair.quit)
		e.mu.Lock()
		for _, w := range repair.refusee {
			c.answer(e, w, nil, "", repair.quit.err())
		}
		e.mu.Unlock()
	}
	if len(escalating) > 0 {
		c.escalate(e, inc, escalating)
	}
	if len(repair.scrubbing) > 0 {
		c.escalate(e, d.inc, repair.scrubbing)
	}
}

// damagedRound answers the callers of r, a round whose own git named damage d
// of the entry's current incarnation, but those Cache.run answers or scrubs
// (roundRepair); e.mu is held.
//   - A CHECK gets the damage and the incarnation pinned: repaired under it, a
//     check would quietly download the repository and report it clean.
//   - The rest wait on the REPAIR. A SHAPE refusal no sweep met, or DERIVED
//     damage with a droppable file this round has not dropped, is read whole
//     IN PLACE, no GET, by the next round, its callers queued for it
//     (markWhole). A PACK'S OR AN OBJECT'S damage, or derived damage that
//     survived its drop, is repaired as a request's is (repairDamaged). A
//     shape refusal past the round's own sweep, which no read in place ends,
//     is a REBUILD (askRebuild), refused or failed as Rebuild's is.
func (c *Cache) damagedRound(e *entry, r *round, d *damaged) roundRepair {
	c.log.Info("code_sync_damaged", "repo", e.id, "reason", string(d.reason), "swept", d.swept,
		"packs", d.packs, "error", d.err)
	var asking []*waiter
	for _, w := range r.waiters {
		switch {
		case w.gone:
			c.answer(e, w, nil, "", d)
		case w.checks:
			w.got, w.damage, w.found = c.pin(e, d.inc), d, r.found
			close(w.done)
		default:
			asking = append(asking, w)
		}
	}
	r.waiters = nil
	switch {
	case len(asking) == 0:
		return roundRepair{}
	case d.reason == RebuildShape && !d.swept,
		d.reason == RebuildDerived &&
			c.image.reported(d.drops)&c.image.holding(d.inc)&c.image.dropping(EventReported)&^d.dropped != 0:
		c.markWhole(e, d.inc, Failure{Reason: d.reason, Drops: d.drops, Began: d.began}, dropRound)
		for _, w := range asking {
			c.queue(e, w)
		}
		return roundRepair{}
	}
	reason := d.reason
	if reason == RebuildDerived {
		// Every droppable file the damage names (OnReported) was dropped by
		// the round that met it, or none is: what git could not read is a
		// pack's.
		reason = RebuildCorrupt
	}
	if reason != RebuildShape {
		return c.repairDamaged(e, d, reason, asking)
	}
	next, rule := c.askRebuild(e, d.inc, reason, true, nil, c.now())
	if rule == "" {
		next.waiters = append(next.waiters, asking...)
		return roundRepair{}
	}
	f := c.refusal(e, d.name, reason, rule, c.now())
	return roundRepair{quit: &f, refusee: asking}
}

// roundRepair is what damagedRound leaves for Cache.run once e.mu is let go:
// a refused rebuild to report (quit) and the callers to answer once it is
// (refusee), the callers the repair's scrub is for (scrubbing,
// Cache.escalate), and whether the repair answers what the round's own git
// reported (answers, settleReader): a pack's or an object's does; a shape's
// or a derived file's, read in place, reads no pack and answers none.
type roundRepair struct {
	quit      *refused
	refusee   []*waiter
	scrubbing []*waiter
	answers   bool
}

// repairDamaged is damagedRound for a PACK'S OR AN OBJECT'S damage d, of
// reason (RebuildCorrupt or RebuildMissing), met by a round's own git: the
// four steps of rebuild.go's REQUEST-TIME REPAIR, each caller taken on from
// the step it has reached; e.mu is held. 1. THE GUARD (predates) answers all
// as Rebuild's does (standOrSync). 2. IN PLACE: a FORCED round (markWhole,
// repairMark.forces) installs every confirmed hint's pack again, skipped when
// none is fetchable and a forced round already said what one would (d.forced,
// forcedLately). 3. THE SCRUB, handed back to Cache.run (Cache.escalate).
// 4. THE WHOLE REBUILD, for a caller whose scrub's round met the damage again
// (waiter.scrubbedOn), never asked while a pack it needs is held off
// (repairMark.heldOffFor): the store's refusal answers it. Each step runs at
// most once a caller per incarnation, so a repair ends whatever the damage.
func (c *Cache) repairDamaged(e *entry, d *damaged, reason RebuildReason, asking []*waiter) roundRepair {
	inc := d.inc
	if predates(inc, d.began) {
		c.countPart(partOf(reason), reportStale)
		for _, w := range asking {
			w.escalate = ""
			c.standOrSync(e, inc, w)
		}
		return roundRepair{answers: true}
	}
	now := c.now()
	skip := len(inc.repair.fetchable(d.packs, now)) == 0 && (d.forced || c.forcedLately(inc))
	var inPlace, scrub, rebuild []*waiter
	for _, w := range asking {
		switch {
		case w.scrubbedOn == inc:
			rebuild = append(rebuild, w)
		case skip, w.escalate != "" && w.failedOn == inc:
			scrub = append(scrub, w)
		default:
			inPlace = append(inPlace, w)
		}
	}
	for _, ws := range [][]*waiter{inPlace, scrub} {
		for _, w := range ws {
			w.escalate, w.failedOn = reason, inc
			// The guard measures from the NEWEST failure: what the caller's
			// in-place round wrote before its git met the damage again ended
			// nothing.
			if d.began.After(w.began) {
				w.began = d.began
			}
		}
	}
	if len(inPlace) > 0 {
		c.markWhole(e, inc, Failure{Reason: RebuildStored}, dropRound)
		inc.repair.forces++
		c.reinstallHinted(inc, d.packs)
		for _, w := range inPlace {
			c.queue(e, w)
		}
	}
	out := roundRepair{scrubbing: scrub, answers: true}
	if len(rebuild) == 0 {
		return out
	}
	if err := inc.repair.heldOffFor(now, rebuild[0].whole); err != nil {
		for _, w := range rebuild {
			w.escalate, w.scrubbedOn, w.whole = "", nil, nil
			c.answer(e, w, nil, "", err)
		}
		return out
	}
	next, rule := c.askRebuild(e, inc, reason, true, rebuild[0].whole, now)
	if rule == "" {
		for _, w := range rebuild {
			w.escalate, w.rebuild, w.scrubbedOn, w.whole = "", true, nil, nil
		}
		next.waiters = append(next.waiters, rebuild...)
		return out
	}
	f := c.refusal(e, d.name, reason, rule, now)
	out.quit, out.refusee = &f, rebuild
	return out
}

// markWhole marks inc to be read WHOLE, IN PLACE, by its next round, a full
// one counting f.Reason as it converges unless inc was already marked for
// another; its entry's mu is held. Nothing is answered from it at once until
// then (atOnce). The mark is counted (marks), so a round begun before it,
// whose sweep may predate the damage, converges nothing. No new incarnation
// and no GET: a shape refusal lost no pack, and a multi-pack-index or a
// commit-graph is derived. RebuildDerived also has the round drop the files
// of f.Drops' declarations that drop on a report (incarnation.drop;
// OnReported), guarded by f.Began (dropDamaged) and tagged with by, what
// asked it.
func (c *Cache) markWhole(e *entry, inc *incarnation, f Failure, by string) {
	inc.converged = false
	c.backfillDirty(e) // no step of the backfill is taken of a copy until its round converges it
	inc.marks++
	if f.Reason == RebuildDerived {
		inc.drop.addSet(c.image, c.image.reported(f.Drops)&c.image.holding(inc)&c.image.dropping(EventReported),
			f.Began, by)
	}
	if inc.reread == "" {
		inc.reread = f.Reason
	}
}

// damaged is a round's own git process, in inc, failing with damage of inc
// (DamageReason): what converge answers so Cache.run can ask for its repair.
// It reads as the git failure it wraps.
type damaged struct {
	inc    *incarnation
	name   string // the repository's name, as the round's snapshot read it
	reason RebuildReason
	// swept: the round was a full one, whose sweep had already made the
	// directory its image and held it to the pool's shape check — so a shape
	// refusal there is one the sweep did not end.
	swept bool
	// drops is the parts a RebuildDerived damage names (Failure.Drops), and
	// dropped the declarations the round that met it had itself dropped
	// (applied.dropped): damage in files the round just dropped survived the
	// drop. began is when the git that met it began.
	drops   gitproc.Part
	dropped derivedSet
	began   time.Time
	// packs is the packs a confirmed hint of the round's git named
	// (confirmRound), each fetched again by the in-place repair
	// (Failure.Packs). forced: the round was itself a forced in-place round
	// (checked.forced) whose findings are on the repair mark, so another
	// forced round would find nothing new.
	packs  []string
	forced bool
	err    error
}

func (d *damaged) Error() string { return d.err.Error() }

// Unwrap is the git failure, so errors.Is sees gitproc's classification.
func (d *damaged) Unwrap() error { return d.err }

// converging is converge holding ALL of the entry's write lock, waited for
// under the cache's lifetime: other holders hold it for a listing or a few
// links, never across a round, so only Stop ends the wait. Every pack an
// adoption handed the round meanwhile is installed before the lock goes
// (releaseWrite), so before the round's callers are answered.
func (c *Cache) converging(e *entry, r *round) (*incarnation, SyncKind, error) {
	if err := c.acquireWrite(c.life, e); err != nil {
		return nil, "", ErrStopped
	}
	defer c.releaseWrite(e, writeExclusive)
	return c.converge(e, r.fresh, r.rebuild, r.of, r.whole, &r.found, &r.did, &r.reader)
}

// roundIsFull reports whether a round begun now over inc, e's current
// incarnation, reads it whole; e.mu is held. See converge's step 1. A REPAIR MARK
// asks a full round too (report.go) — unless a check of the repository is
// admitted, whose release asks it again.
func (c *Cache) roundIsFull(e *entry, inc *incarnation) bool {
	return inc == nil || inc.state == nil || !inc.converged || (inc.repair.due(c.now()) && e.verifying == 0)
}

// converge is one round's body, its checks' repairs added to found and its
// applies to did; each step is named by the field its log and error carry:
//  1. pick the incarnation, or a NEW one once the rows say the repository
//     exists; FULL when new, stateless or not yet full in this process. A
//     REBUILD (rebuild.go) reads whole into a new one, seeded with whole.
//  2. read the snapshot (read.go), not-found or a full answer below the
//     incarnation asked again ONCE at linearizable; not-found there PURGES.
//  3. a linearizable answer below the log mark is the log gone BACK: reread.
//  4. install the packs the directory lacks (install.go); 5. on a full
//     answer, the round's index (maintain.go); 6. the refs (refs.go); 7. on a
//     full answer, the sweep's pack half (sweep.go); 8. the state (state.go);
//  9. the SWAP, where packs left a copy a reader may hold (swap.go).
func (c *Cache) converge(e *entry, fresh statelog.Freshness, rebuild RebuildReason, of *incarnation,
	whole []string, found *[]string, did *applied, reader *roundReader) (_ *incarnation, _ SyncKind, err error) {

	if !c.enter() {
		return nil, "", ErrStopped
	}
	defer c.leave()
	ctx := c.life
	began := time.Now()
	// epoch is the reread mark this round's convergence must still find: a
	// mark made while it runs is about rows it may have read before it.
	epoch := c.rereads.Load()
	// forcedCheck is the round's FORCED check, once an apply has run one
	// (checked.forced), and forcedOn its incarnation: damage the round's git
	// meets after it is met by a forced round (damaged.forced), and the
	// check's VERDICT is recorded as the round ENDS, however it ended
	// (incarnation.forcedAt), so the next request runs no forced round of its
	// own, and none asked while the round installs what it found goes to the
	// scrub beside it. Repairs the round writes after the check end that
	// verdict (forcedLately reads incarnation.repairWrite against the check's
	// end).
	var (
		forcedCheck checked
		forcedOn    *incarnation
	)
	defer func() {
		if forcedOn != nil {
			// On the incarnation serving the checked files now: a swap at
			// the round's end (swap.go) links them into the next one.
			e.mu.Lock()
			on := serving(forcedOn)
			on.forcedAt, on.forcedEnded = forcedCheck.at, forcedCheck.ended
			e.mu.Unlock()
		}
	}()

	e.mu.Lock()
	inc := e.current
	full := c.roundIsFull(e, inc)
	// marks is the in-place marks this round's convergence must still find
	// (markWhole), as epoch is the cache's.
	var marks uint64
	if inc != nil {
		marks = inc.marks
	}
	e.mu.Unlock()
	// A REBUILD reads the repository whole into a new incarnation beside the
	// current one (replaced), which serves until the new one has converged;
	// a rebuild that fails moves what it built into trash/ and leaves the
	// old one serving.
	var replaced, building *incarnation
	if rebuild != "" {
		replaced, inc, full, marks = inc, nil, true, 0
		defer func() {
			if err != nil && building != nil {
				c.abandon(e, building)
			}
		}()
	}

	// escalate reads the round again at linearizable, once, and reports
	// whether it will: an answer only a copy behind the log can give is asked
	// of the log's head first. Not the store's not-found, which a read at the
	// head can predate too (gone).
	escalate := func() bool {
		if fresh.Level == statelog.ReadLinearizable {
			return false
		}
		fresh = statelog.Freshness{Level: statelog.ReadLinearizable}
		return true
	}
	var (
		askedFull, rewound bool
		// gone is the store's not-found for a pack the round's last read
		// named, met by the apply after that read: the round's next read, at
		// the log's head, decides it (THE READ AFTER A NOT-FOUND DECIDES IT).
		gone *ErrPackMissing
		res  applied
	)
	for {
		var at, leaving statelog.Position
		var stream time.Time
		// indexed: the incarnation keeps an index of pushes, which a delta's
		// pushes phase is folded into before the round answers (apply).
		indexed := false
		if inc != nil {
			e.mu.Lock()
			at, leaving, stream, indexed = inc.at, inc.leavingSince, inc.stream, inc.record() != nil
			e.mu.Unlock()
		}
		markStream, markAt := c.mark.read()
		var snap snapshot
		snap, err = c.read(ctx, e.id, at, retiredFloor(at, leaving), stream, full, indexed, fresh)
		switch {
		case errors.Is(err, gitlet.ErrRepoNotFound) && escalate():
			continue
		case errors.Is(err, gitlet.ErrRepoNotFound):
			err = fmt.Errorf("cache: sync %s: read: %w", e.id, err)
			c.purge(e, err)
			return nil, "", err
		case err != nil:
			return nil, "", fmt.Errorf("cache: sync %s: read: %w", e.id, err)
		}
		if c.mark.saw(snap.streamCreated, snap.position) {
			c.log.Info("code_cache_log_recreated", "repo", e.id, "was", markStream, "now", snap.streamCreated)
			epoch = c.reread(RebuildStream, func(i *incarnation) bool {
				return !i.stream.IsZero() && !i.stream.Equal(snap.streamCreated)
			})
		}
		if inc == nil {
			// A FIRST incarnation (no rebuild's) whose round fails is kept
			// with no state, so the next, full, reuses every pack it installed.
			if inc, err = c.newIncarnation(e, rebuild == ""); err != nil {
				return nil, "", fmt.Errorf("cache: sync %s: incarnation: %w", e.id, err)
			}
			if rebuild != "" {
				building = inc
				if replaced != nil && replaced == of {
					if err = c.seedRebuild(ctx, e, replaced, inc, whole); err != nil {
						return nil, "", fmt.Errorf("cache: sync %s: seeding the rebuild: %w", e.id, err)
					}
				}
			}
		}
		linearizable := fresh.Level == statelog.ReadLinearizable
		belowMark := markStream.Equal(snap.streamCreated) && !markAt.IsZero() && !reached(snap.position, markAt)
		switch {
		case linearizable && (snap.behind || belowMark) && !rewound:
			rewound = true
			was := at
			if belowMark && (!snap.behind || reached(markAt, at)) {
				was = markAt
			}
			c.log.Warn("code_cache_log_rewound", "repo", e.id, "was", was.String(), "now", snap.position.String())
			c.mark.set(snap.streamCreated, snap.position)
			epoch = c.reread(RebuildRewound, func(*incarnation) bool { return true })
			if !snap.full {
				full = true
				continue
			}
		case snap.behind && snap.full && escalate():
			continue
		case snap.behind && !linearizable:
			// A copy behind this cache answered a delta: the cache never
			// moves backwards for one. Only a swap the last round could not
			// make is tried again, over the zero snapshot, so no round's
			// index is written.
			inc = c.swapLeaving(ctx, e, inc, snapshot{}, nil)
			c.account(e, inc, snap, applied{}, time.Since(began))
			return inc, snap.kind(), nil
		}
		if snap.restreamed {
			e.mu.Lock()
			if inc.reread == "" {
				inc.reread = RebuildStream
			}
			inc.historyGone = true
			e.mu.Unlock()
		}
		if gone != nil {
			// THE READ AFTER A NOT-FOUND DECIDES IT, made at the log's head
			// after the store's not-found, with nothing installed again.
			// Still naming the pack at the same object, it names an object the
			// store lost while a live row named it: the round fails here, as
			// any refetch would. It names the pack whenever its row is live (a
			// full answer names every live pack, and is asked whenever the
			// naming answer was; a delta from the incarnation's position names
			// every pack above it), so one that does not is the row gone from
			// the log since, and the round goes on with what it holds.
			if p, ok := snap.packs[packKey{gone.Pack.PackID, gone.Pack.Part}]; ok && p.Object == gone.Pack.Object {
				c.log.Warn("code_pack_missing", "repo", e.id, "pack_id", gone.Pack.PackID,
					"object", gone.Pack.Object.String(), "damaged", false)
				return nil, "", gone
			}
			gone = nil
		}
		if reader.inc != inc {
			// The round's own git runs in inc from here (the peel): what they
			// report is held for the round (roundReader).
			e.mu.Lock()
			*reader = roundReader{inc: inc, by: c.reader(inc)}
			e.mu.Unlock()
		}
		res, err = c.apply(ctx, e, inc, snap, epoch, marks, reader.by)
		if err != nil && res.placing != nil {
			// A placement no swap of this round will make: its layers go
			// from tmp/.
			res.placing.discard(c)
			res.placing = nil
		}
		// What the apply changed of the backfill's work, however it ended —
		// its check's vouches and findings for runs, the repairs it settled,
		// its drops of the index, its fold, its convergence — was marked in
		// the hold that changed it (schedule.go).
		if res.checked.forced {
			forcedCheck, forcedOn = res.checked, inc
		}
		// Each finding once: one an apply could not act on waits on the
		// repair mark, so the round's next apply repairs it — and names it —
		// again.
		for _, s := range res.checked.findings(c.image) {
			if !slices.Contains(*found, s) {
				*found = append(*found, s)
			}
		}
		did.add(res)
		var missing *ErrPackMissing
		switch {
		case errors.Is(err, errRefsNotOurs) && !snap.full && !askedFull:
			// A delta onto a packed-refs this cache did not write: read
			// ONCE again, full.
			askedFull, full = true, true
			continue
		case errors.As(err, &missing) && !missing.Damaged:
			// THE STORE'S NOT-FOUND IS ASKED OF THE LOG'S HEAD, AFTER IT,
			// whatever level the round read at: even a linearizable round's
			// barrier came before the store answered, and the row may have
			// left the log since (a purge drops pack rows before the
			// repository; a retired pack's object outlives its row only by
			// the domain's grace, ADR-0081). EVERY not-found is asked so, by
			// the read after its own; the round still ends, as a row gone from
			// the log never returns. A pack a FULL answer named is decided by
			// a full read: a delta from the incarnation's position reaches no
			// pack at or below it, so would take a lost pack for a row gone.
			// A DAMAGED answer is final at once (below): a purge or a
			// retirement never alters an object.
			gone, fresh = missing, statelog.Freshness{Level: statelog.ReadLinearizable}
			full = full || snap.full
			continue
		case errors.As(err, &missing):
			// Final: the bytes the store holds under it are not the row's.
			c.log.Warn("code_pack_missing", "repo", e.id, "pack_id", missing.Pack.PackID,
				"object", missing.Pack.Object.String(), "damaged", missing.Damaged)
			return nil, "", err
		case err != nil:
			f, ok := DamageReason(err)
			hints := hintsOf(err)
			if rebuild == "" && len(hints) > 0 &&
				(!ok || f.Reason == RebuildCorrupt || f.Reason == RebuildMissing) {
				// The round's own git named a pack in words (gitproc.Hint),
				// alone (2.56's bare "%s") or beside a pack's or an object's
				// damage (a row of git's table): Cache.run holds that pack to
				// its row once the round has ended and let the write lock go,
				// as the hash takes minutes, and a pack not its row's is named
				// to the repair, which fetches THAT pack again (the host's
				// confirmed rule). With no classification, only such a pack
				// asks a repair at all.
				h := &hinted{inc: inc, name: snap.repo.Name, full: snap.full, hints: hints, err: err,
					classified: ok, damage: f, forced: forcedOn != nil}
				if snap.full {
					h.rows = &snap
				}
				return nil, "", h
			}
			if ok && rebuild == "" {
				// The round's own git, in inc, named its damage — or, on a
				// full answer, the sweep left a shape the pool refuses:
				// Cache.run makes the repair of it this round's answer.
				return nil, "", &damaged{inc: inc, name: snap.repo.Name, reason: f.Reason, swept: snap.full,
					drops: f.Drops, dropped: res.dropped, began: f.Began, forced: forcedOn != nil, err: err}
			}
			return nil, "", err
		}
		if rebuild != "" {
			// A rebuild's product is a new incarnation nobody held: its
			// strays left it in place (packStrays), and so was its chain
			// placed (placeChain).
			c.promote(e, replaced, inc, rebuild, time.Since(began))
		} else {
			// Packs left a copy a reader may hold, or its chain moves: the
			// round answers from the next incarnation, or, where the swap
			// failed, from this one, a superset of its rows the next round
			// swaps again.
			inc = c.swapLeaving(ctx, e, inc, snap, res.placing)
		}
		// THE INSTALLER takes whatever of the rows' chain the copy does not
		// hold now (layers.go): a delta's new layers, which it never fetches
		// itself, and what a full round could not fetch — its wait ended
		// where the round served the chain, a new try.
		e.mu.Lock()
		due := installDue(inc)
		e.mu.Unlock()
		if due {
			c.kickInstall(e.id, snap.layersServed)
		}
		c.account(e, inc, snap, res, time.Since(began))
		return inc, snap.kind(), nil
	}
}

// reread marks every current incarnation which selects for a WHOLE read: its
// next round reads the repository whole, nothing is answered from it at once
// until then, the reason is counted when that read completes, and that read
// drops its index of pushes first (incarnation.historyGone). It answers
// the epoch a round's convergence must still find to count as one. Rare — a
// log recreated or gone back under the node — so a walk of every entry.
func (c *Cache) reread(reason RebuildReason, which func(*incarnation) bool) uint64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	epoch := c.rereads.Add(1)
	for _, e := range c.entries {
		e.mu.Lock()
		if inc := e.current; inc != nil && which(inc) {
			inc.converged = false
			if inc.reread == "" {
				inc.reread = reason
			}
			// Every position its index of pushes recorded may name another
			// record now: its next full round drops it (EventHistoryGone),
			// and the backfill takes no step of it until then.
			inc.historyGone = true
			c.backfillDirty(e)
		}
		e.mu.Unlock()
	}
	return epoch
}

// applied is what one round did to its incarnation.
type applied struct {
	packsAdded int
	// reinstalled is the packs the round installed again over their files,
	// and restored those it installed that the copy had held and lost
	// (installs.restored) — of packsAdded, the ones that repair something.
	reinstalled, restored int
	trashed               bool
	// dropped is the declarations whose files the round dropped for a mark
	// (incarnation.drop) or for its check, or before it placed a pack over
	// files whose index may have changed (install), taken before anything
	// else the round did.
	dropped derivedSet
	// checked is what the round's check found (check.go); zero for a delta.
	checked checked
	// placing is the published chain a full round places in the copy a
	// reader may hold, as its swap's layer step (layers.go) — nil where it
	// placed it in place or had nothing to place — which the round's swap
	// takes, or which goes from tmp/ with a round that fails.
	placing *layerStep
}

// add sums what another apply of the same round did into a.
func (a *applied) add(b applied) {
	a.packsAdded += b.packsAdded
	a.reinstalled += b.reinstalled
	a.restored += b.restored
	a.dropped |= b.dropped
	a.trashed = a.trashed || b.trashed
}

// repaired reports whether the round did anything a failed request may have
// met the want of: installed a pack the copy had held and lost, installed one
// again over its files, or dropped a derived file. A pack of a push the copy
// had not yet read repairs nothing — its install is a write into the copy,
// which the escalation's guard reads instead (Cache.escalate).
func (a applied) repaired() bool { return a.restored > 0 || a.reinstalled > 0 || a.dropped != 0 }

// apply makes inc equal to snap — steps 4 to 8 of converge. epoch is the
// reread mark the round began under, and marks the incarnation's in-place
// marks then (markWhole): a full answer converges the incarnation only while
// no mark of either was made since. by is the round's reader of inc
// (roundReader), which its own git runs name.
func (c *Cache) apply(ctx context.Context, e *entry, inc *incarnation, snap snapshot, epoch, marks uint64,
	by gitproc.Repairer) (applied, error) {

	var (
		res       applied
		found     []checkedFile
		reinstall []checkedFile
		// history: the log under the incarnation is no longer the history it
		// was read from — recreated or gone back — so a pack file no row
		// names is no compaction's retirement, whatever a retired phase says
		// (packStrays).
		history bool
		// chain is a full round's plan of the published chain (layers.go):
		// what the copy keeps of it and the layers its install fetches.
		chain chainPlan
	)
	if snap.full {
		// The sweep moves whatever lies outside the image, a running
		// maintenance's lock and temporary files among it, and may remove a
		// pack its index would name: so a full round stops maintenance on its
		// incarnation first, and none starts while a round runs (maintain.go).
		c.stopMaintenance(e, inc)
		// A LOG RECREATED OR GONE BACK under the incarnation drops what its
		// positions name — the index of pushes (EventHistoryGone) — before
		// the sweep, and before any git process sees the round's image.
		e.mu.Lock()
		reread := inc.reread
		e.mu.Unlock()
		gone := c.historyGone(e, inc, snap)
		history = gone || reread == RebuildStream || reread == RebuildRewound
		if gone {
			if _, err := c.dropFor(e, inc, EventHistoryGone, eventDetail{}); err != nil {
				return res, fmt.Errorf("cache: sync %s: drop derived files: %w", e.id, err)
			}
		}
		trashed, err := c.tidy(inc, snap)
		if err != nil {
			return res, fmt.Errorf("cache: sync %s: sweep: %w", e.id, err)
		}
		res.trashed = trashed
		// THE CHECK (check.go), before anything is answered from what a
		// previous process left: every index file the snapshot names and every
		// derived file, held to its trailer unless this process wrote or
		// checked it as it is now. A failing stored file is reinstalled below,
		// a derived one dropped; with the write lock held and the maintainer
		// stopped, what it hashes is what it repairs. A pack's or an object's
		// REPAIR IN PLACE (Cache.Rebuild, repairDamaged) forces every index
		// file whatever its vouch and reinstalls the packs a confirmed hint
		// named, never under an admitted check, whose release asks again.
		forces, asked := c.requested(e, inc)
		ck, err := c.checkIndexes(ctx, e, inc, snap, forces > 0)
		if err != nil {
			return res, fmt.Errorf("cache: sync %s: check: %w", e.id, err)
		}
		// THE CHECK HAS RUN: its forces are spent and its findings are the
		// repair mark's (recordCheck), whatever the round meets past here. The
		// round repairs them with what earlier checks and scrubs left, each
		// only while still the failing file (checked.still), and takes each
		// off once acted on (settleFound, below).
		res.checked, reinstall = ck, asked
		found = c.recordCheck(e, inc, forces, ck.damaged)
		res.checked.damaged = slices.Concat(found, reinstall)
		// THE PUBLISHED CHAIN'S PLAN (layers.go), once the check has vouched
		// or named every file: the prefix of the rows' chain the copy keeps,
		// each file held to its Check by this process, and the layers above it
		// for the install to fetch behind the packs.
		chain = c.planChain(e, inc, snap, found)
	}
	// A pack the check or a scrub found damaged, or a full round found at the
	// wrong size, is installed again by renaming over its files (install.go),
	// and the derived files that may have been built from it go first.
	ins, err := c.install(ctx, e, inc, snap, res.checked, chain)
	defer func() {
		// The layers the install staged that no placement took go from
		// tmp/, however the round ends — but those it hands its swap
		// (res.placing), which the swap places or discards.
		if res.placing == nil {
			for _, f := range ins.layers {
				if f.staged != nil {
					f.staged.remove(c)
				}
			}
		}
	}()
	// COUNTED BY THE ROUND THAT DID IT (RebuildStored), once a round and
	// before anybody is answered, never by who waited: a request's forces are
	// read in this apply (requested), so a round already running when it
	// asked may be the one that repairs it.
	if ins.replaced+ins.restored > 0 {
		c.rebuilt(RebuildStored)
	}
	if err != nil {
		return res, err
	}
	res.packsAdded, res.reinstalled, res.restored = ins.added, ins.replaced, ins.restored
	if ins.dropped != 0 {
		res.trashed, res.dropped = true, ins.dropped
	}
	// A pack whose download to install it again failed is left as it stands,
	// its findings on the mark and its next attempt held off (install.go):
	// neither it nor a pack still held off is settled as repaired.
	now := c.now()
	e.mu.Lock()
	unrepaired := inc.repair.heldOff(now)
	for cs, ferr := range ins.failed {
		inc.repair.fetchFailedAt(cs, now, ferr)
		unrepaired[cs] = true
	}
	e.mu.Unlock()

	indexed := false
	var (
		strays []string
		// leaving is what a full round's strays leave by the swap — every
		// pack file no row places where it lies, in an incarnation a reader
		// may hold — and nil where they leave in place or there are none
		// (packStrays).
		leaving map[leaveKey]Event
		// placedHere: the round placed the published chain in place, or
		// wrote its chain files again (placeChain).
		placedHere bool
	)
	if snap.full {
		// A DERIVED-DAMAGE REPAIR drops the files the damage named before the
		// round's index decides: the round (at MidxRoundPacks packs) or the
		// maintainer writes sound ones over the packs already held, with no
		// download. By PART: the multi-pack-index's files, the commit-graph,
		// or both for a chunk-format message, which names neither. Each read
		// off its own mark (dropDamaged): a request's or the round's git
		// (incarnation.drop), a run's report (incarnation.repair), the check.
		if err = c.dropDamaged(e, inc, &res); err != nil {
			return res, fmt.Errorf("cache: sync %s: drop derived files: %w", e.id, err)
		}
		// SETTLED ONCE ACTED ON, not once the round converges: a later
		// failure (the refs' peel) undoes none of it. A round that failed
		// before here leaves its findings on the mark for the next; its
		// check's forces are spent already (recordCheck).
		c.settleFound(e, inc, found, unrepaired)
		c.settleRequested(e, inc, reinstall, unrepaired)
		// THE CHAIN'S PLACEMENT (layers.go), before the sweep's pack half
		// decides (a chain's packs are no strays; those a placement cuts away
		// are) and before the round's index, which no round writes beside a
		// chain: in place on a copy nobody holds, the canary reading it first,
		// else as the round's swap's layer step.
		if res.placing, placedHere, err = c.placeChain(ctx, e, inc, chain, ins.layers); err != nil {
			return res, fmt.Errorf("cache: sync %s: place the published chain: %w", e.id, err)
		}
		// The sweep's pack half is DECIDED here and done once the refs are in:
		// an index that may name a pack it removes goes now, with whatever in
		// objects/pack is no pack's file, so the round's index over exactly
		// the snapshot's packs is written before the peel, with no dead
		// writer's lock to refuse it. Strays in a copy a reader may hold leave
		// by the SWAP instead (swap.go), whose next incarnation takes the index.
		var moved bool
		if strays, leaving, moved, err = c.packStrays(e, inc, snap, history); err != nil {
			return res, fmt.Errorf("cache: sync %s: sweep: %w", e.id, err)
		}
		res.trashed = res.trashed || moved
		if leaving == nil && res.placing == nil {
			// Where the round swaps a chain in, its index is the swap's to
			// write over the next incarnation — none, beside a chain.
			indexed = c.indexRound(ctx, e, inc, snap)
		}
	}

	refsWritten, tagsWritten, err := c.writeRefs(ctx, e, inc, snap, by)
	if err != nil {
		return res, err
	}

	// THE INDEX OF PUSHES (provenance.go), after the packs and the refs and
	// before the state: a delta's pushes folded as one run before anybody is
	// answered, a full round's position its index's — or its gap's — upper
	// bound. What either retires waits for a state that no longer names it
	// (persist); and neither fails the round.
	provenanceWrote := false
	if snap.full {
		trashed, err := c.sweepPacks(e, inc, strays)
		if err != nil {
			return res, fmt.Errorf("cache: sync %s: sweep: %w", e.id, err)
		}
		res.trashed = res.trashed || trashed
		// Every pack the copy holds is the snapshot's now, so a memo its
		// history's drop shut answers again (memo.go). NOT WHERE STRAYS LEAVE
		// BY THE SWAP: this copy holds them until retired, and a batch begun
		// over it meanwhile could read the old history and be carried into the
		// next incarnation with the memo (carryMemo), where it opens instead
		// (swap.go's memoShut).
		if leaving == nil {
			c.reopenMemo(e, inc)
		}
		c.indexFull(e, inc, snap)
	} else {
		provenanceWrote = c.foldPushes(ctx, e, inc, snap)
	}

	// The in-memory fields are what the directory now reflects. When it
	// CHANGED they are also written down — persist writes them whole —
	// and otherwise only the position moves: nothing between the state's
	// position and this one changed the directory, so a restart's next
	// delta from the state re-reads rows that moved nothing here.
	e.mu.Lock()
	// wrote: the round wrote into the directory — packs in or out, refs, the
	// index, the published chain, a file swept out of it.
	wrote := ins.added+ins.replaced > 0 || refsWritten || indexed || placedHere || res.trashed
	// A run of the index of pushes is written down with the state that
	// names it, and one retired goes only once a state no longer names it
	// (persist) — but neither is a file git reads, so no change a rebuild
	// weighs. Nor is the tag index, whose record the state carries.
	changed := snap.full || wrote || provenanceWrote || tagsWritten || len(inc.retiredRuns) > 0 ||
		!snap.streamCreated.Equal(inc.stream) || !sameRepo(inc.repo, snap.repo)
	prevAt, prevStream, prevRepo, prevLeaving := inc.at, inc.stream, inc.repo, inc.leavingSince
	inc.at, inc.stream, inc.repo = snap.position, snap.streamCreated, snap.repo
	// THE FLOOR OF WHAT IS LEAVING is written down with the state, so a boot
	// after a crash before the swap reads the retirements of what it finds
	// (state.LeavingSince). Too low a floor only serves a full answer more
	// retirements, never a wrong one.
	switch {
	case snap.full && !leavingRetired(leaving):
		inc.leavingSince = statelog.Position{}
	case snap.full:
		inc.leavingSince = lowest(inc.leavingSince, snap.retiredFrom)
	case len(snap.retired) > 0:
		inc.leavingSince = lowest(inc.leavingSince, snap.since)
	}
	if snap.full {
		// The directory holds exactly the snapshot's packs now, and the packs
		// its installed chain holds beyond them: their rows' count, whatever
		// the incarnation counted before the round.
		inc.objects = 0
		figures := snap.packFigures()
		for _, p := range figures {
			inc.objects += p.objects
		}
		for cs, p := range inc.installedPacks {
			if _, live := figures[cs]; !live {
				inc.objects += p.objects
			}
		}
	}
	if wrote {
		// No longer exactly what the last rebuild attempt read: a rebuild
		// may make something else of it (rebuild.go).
		inc.unmoved = false
	}
	e.mu.Unlock()
	if changed {
		if err := c.persist(ctx, e, inc, true); err != nil {
			// The directory is ahead of its state file and of the position
			// put back here: the next round's delta re-applies those rows,
			// which changes nothing twice.
			e.mu.Lock()
			inc.at, inc.stream, inc.repo, inc.leavingSince = prevAt, prevStream, prevRepo, prevLeaving
			e.mu.Unlock()
			return res, fmt.Errorf("cache: sync %s: state: %w", e.id, err)
		}
	}
	// THE LIVE SET (swap.go) follows the rows only once the round applied
	// them whole: a failed round leaves it, and what is leaving, for the next
	// to read against. A delta adds its rows first, then takes out its
	// retired ones, so a pack named again in the same delta stays.
	e.mu.Lock()
	if snap.full {
		liveWhole(inc, snap.packs)
		inc.leaving = leaving
	} else {
		applyDelta(inc, snap)
	}
	if len(inc.leaving) == 0 {
		inc.leaving, inc.leavingSince = nil, statelog.Position{}
	}
	if len(inc.leaving) == 0 && liveChain(inc, inc.installed) {
		// THE EPOCH OBJECTS/ HOLDS (Synced.DropEpoch) is the rows' only once
		// the copy holds every pack file where they place it and none in
		// objects/ for a row not live; until a begin's inputs and a retired
		// chain leave by the swap, it is the epoch before.
		inc.dropEpoch = snap.repo.DropEpoch
	}
	e.mu.Unlock()
	if snap.full {
		e.mu.Lock()
		inc.converged = c.rereads.Load() == epoch && inc.marks == marks
		// No step of the backfill is taken of a copy until a round converges
		// it: marked in this hold, converged or not (schedule.go).
		c.backfillDirty(e)
		var reason RebuildReason
		if inc.converged {
			reason, inc.reread = inc.reread, ""
			// Its bytes held to their checksums: an inherited copy's first
			// scrub due at once, a copy this process built its interval
			// begun (scrub.go).
			c.firstScrub(inc)
		}
		e.mu.Unlock()
		// A pack's in-place read (RebuildStored), a request's or a round's
		// own, is counted by what its round did (Cache.run), not by its mark.
		if reason != "" && reason != RebuildStored {
			c.rebuilt(reason)
		}
		if reason == RebuildShape {
			// A log recreated or gone back is logged once, where it is
			// seen, for every repository it marks; a shape repaired is
			// one directory something outside the engine wrote into.
			c.log.Warn("code_cache_reshaped", "repo", e.id, "incarnation", inc.n, "moved", res.trashed)
		}
	}
	return res, nil
}

// applyDelta applies a delta's rows to inc's live set (added rows first, then
// retired ones out) and marks LEAVING every pack's files inc holds where the
// rows no longer place them (placeOf): a pack no row names, or one MOVED
// between objects/pack and the dropping view; by EventPackRetired, but
// EventPackExpunged for a dropping pack no row names (an expunge's commit).
// e.mu is held. A pack an installed layer names stays (layers.go) until its
// layer's replacement is placed, since a merge keeps every object it
// retires. NOT PAST A BEGIN: once the rows' drop epoch passes the copy's
// (Synced.DropEpoch), a layer would keep the expunge's dropping objects in
// objects/pack, where a push relies on them, while it is open; so they leave
// with this round's swap, cutting the chain at the lowest layer naming one
// (swap.go's chainOf, gitlet.ExpungeBegin.DropLayers).
func applyDelta(inc *incarnation, snap snapshot) {
	was := map[string]packPlace{}
	note := func(cs string) {
		if _, ok := was[cs]; !ok {
			was[cs] = placeOf(inc, cs)
		}
	}
	for _, p := range snap.packs {
		note(p.GitChecksum)
	}
	for _, r := range snap.retired {
		if p, ok := inc.live[r.PackID]; ok && r.Part == "pack" {
			note(p.checksum)
		}
	}
	for _, p := range snap.packs {
		setLive(inc, p.PackID, liveRow(p))
	}
	for _, r := range snap.retired {
		if r.Part != "pack" {
			// A part this build installs none of: no live row is it.
			continue
		}
		deleteLive(inc, r.PackID)
	}
	leave := func(key leaveKey, ev Event) {
		if inc.leaving == nil {
			inc.leaving = map[leaveKey]Event{}
		}
		inc.leaving[key] = ev
	}
	begun := snap.repo.DropEpoch > inc.dropEpoch
	purged := crossedACommit(inc.dropEpoch, snap.repo.DropEpoch, inc.droppingPacks > 0)
	for cs, from := range was {
		now := placeOf(inc, cs)
		if from == placeNone || from == now {
			continue
		}
		if _, chained := inc.installedPacks[cs]; chained && from == placeLive && !begun {
			continue
		}
		// A RETIREMENT keeps every object that leaves (a compaction's or a
		// begin's inputs, a pack only moving between objects/pack and the
		// view). A dropping pack no row names is AN EXPUNGE'S COMMIT
		// (EventPackExpunged), and so is every pack leaving a round that
		// crossed a commit whose dropping pack the copy never placed
		// (crossedACommit).
		ev := EventPackRetired
		if now == placeNone && (from == placeDropping || purged) {
			ev = EventPackExpunged
		}
		leave(leaveKey{from, cs}, ev)
	}
	if begun {
		ev := EventPackRetired
		if purged {
			ev = EventPackExpunged
		}
		for cs := range inc.installedPacks {
			if placeOf(inc, cs) != placeLive {
				leave(leaveKey{placeLive, cs}, ev)
			}
		}
	}
	// A DELTA NEVER DOWNLOADS A LAYER: the chain it served is recorded, and
	// what of it the copy does not hold is the installer's (converge kicks it
	// once the round has answered).
	if snap.layersServed {
		setChain(inc, snap.layers)
	}
}

// crossedACommit reports whether a round taking a copy whose objects/ hold
// drop epoch held (Synced.DropEpoch) to rows at epoch rows crossed an
// expunge's COMMIT as well as its begin; open is whether the rows still leave
// a pack dropping. One expunge is open at a time per repository, its dropping
// rows live until its commit, so every crossed epoch but the open one has
// committed: what it left dropping never reached this copy, and the begin's
// inputs leaving took those objects with them. Unlike a retirement, which
// keeps every object (X04), they leave as the commit's (EventPackExpunged),
// with the commit-graph and memo naming what was purged.
func crossedACommit(held, rows int64, open bool) bool {
	crossed := rows - held
	if open {
		crossed--
	}
	return crossed > 0
}

// sameRepo reports whether two reads of a repository's row found it the same,
// whatever position each was read at: EVERY OTHER FIELD, the redirect too,
// which TestARoundComparesEveryFieldOfARepositorysRow holds it to as the row
// grows. A field left out is a change no round writes a state for, so a
// restart reads the row as it was before.
func sameRepo(a, b gitlet.RepoRef) bool {
	return a.ID == b.ID && a.Name == b.Name && a.Status == b.Status && a.ManagedBy == b.ManagedBy &&
		a.Kind == b.Kind && a.Owner == b.Owner && a.Visibility == b.Visibility && a.Search == b.Search &&
		a.Mirrored == b.Mirrored &&
		a.DefaultBranch == b.DefaultBranch && a.Description == b.Description &&
		slices.Equal(a.Protections, b.Protections) && a.DeletedAt.Equal(b.DeletedAt) &&
		a.LastActivityAt.Equal(b.LastActivityAt) && a.Refs == b.Refs && a.LivePacks == b.LivePacks &&
		a.LiveBytes == b.LiveBytes && a.LiveObjects == b.LiveObjects && a.DropEpoch == b.DropEpoch &&
		a.Redirect == b.Redirect && a.RedirectUntil.Equal(b.RedirectUntil)
}

// account is a completed round's record: its duration by kind, its log line,
// a repository alone over the budget, the gauges, and what it tells the
// background work:
//   - Packs added restart the maintainer's debounce (lastAdded), so a burst
//     lands before its index; the first no index covers starts the ceiling
//     (uncoveredSince), so unbroken pushes are indexed within MidxMaxWait.
//   - An eviction due (overBudget) kicks the janitor at once, not at its
//     tick, so a burst of cold syncs does not overshoot the budget.
//   - A FULL round owes the maintainer a wake (run): it may have made a copy
//     the maintainer works on, or written an index without a bitmap. A delta's
//     packs, and a first pack, wait for the tick (maintain.go, AT ONCE IS THE
//     NEXT PASS).
func (c *Cache) account(e *entry, inc *incarnation, snap snapshot, res applied, took time.Duration) {
	kind := snap.kind()
	e.mu.Lock()
	bytes, refs := inc.diskBytes+inc.metaBytes, inc.refs
	if res.packsAdded > 0 {
		now := c.now()
		e.lastAdded = now
		if e.uncoveredSince.IsZero() {
			e.uncoveredSince = now
		}
	}
	e.mu.Unlock()
	if c.metrics != nil {
		c.metrics.Observe(metrics.CodeCacheSyncDuration, took, metrics.Attrs{"kind": string(kind)})
	}
	c.log.Debug("code_cache_synced", "repo", e.id, "kind", kind, "position", snap.position.String(),
		"packs_added", res.packsAdded, "refs", refs, "bytes", bytes, "took", took)
	if bytes > c.maxBytes {
		// Served whatever its size: a request is waiting for it.
		c.log.Warn("code_cache_over_budget", "repo", e.id, "bytes", bytes, "budget", c.maxBytes)
	}
	if res.trashed || c.overBudget() > 0 {
		c.kick()
	}
	if snap.full {
		// Woken once the round has let the repository go (run).
		e.mu.Lock()
		e.maintainerOwed = true
		e.mu.Unlock()
	}
	c.setGauges()
}

// purge takes a repository the rows no longer hold out of the cache: every
// incarnation retired (into trash/ with its last reader, the unpinned current
// one once both locks are let go), the entry gone, and every caller queued
// behind the round told why. Only a LINEARIZABLE not-found purges (converge),
// so it answers callers that arrived after the round began too: a
// repository's id is minted once and never comes back.
func (c *Cache) purge(e *entry, cause error) {
	c.mu.Lock()
	e.mu.Lock()
	e.dead = true
	if c.entries[e.id] == e {
		delete(c.entries, e.id)
	}
	var gone string
	if e.current != nil {
		gone = c.retire(e, e.current)
	}
	if r := e.next; r != nil {
		e.next = nil
		for _, w := range r.waiters {
			c.answer(e, w, nil, "", cause)
		}
	}
	e.mu.Unlock()
	c.mu.Unlock()
	// The move into trash/ is made holding no lock, as an eviction's is.
	c.trash(gone)
	c.setGauges()
}
