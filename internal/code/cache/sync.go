package cache

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/crewlet/crewlet/internal/code"
	"github.com/crewlet/crewlet/internal/code/gitproc"
	"github.com/crewlet/crewlet/internal/queue/topics"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/statelog/metrics"
)

// SYNC: the one function that writes a cache, by CONVERGENCE — derive the
// target from rows, make the directory equal to it — and the single flight
// that shares its rounds.
//
// A repository has one ROUND in flight on a node and at most one queued
// behind it. A caller that needs only what the cache holds is answered at
// once; any other joins a round, and a LINEARIZABLE caller joins only a round
// that starts after it arrived — the statelog.ReadIndex rule one level up:
// a round already running took its barrier before this caller asked, and may
// predate the write the caller is about to read. So a burst of CI clones of
// one repository costs one barrier and one sync, and none of them is answered
// by a barrier taken before it asked. Rounds run under the cache's own
// lifetime, never a caller's: a client that hangs up never cancels the round
// the others are waiting on.

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
	// found is what the round's check found failing and repaired in place
	// (check.go), as sentences: a check among its callers names them first
	// (waiter.found), since the copy it is handed no longer holds them.
	found []string
	// did is what the round's applies did, summed: what a request's
	// in-place round answers its escalation by (applied.repaired).
	did applied
	// reader is the name the round's own git runs carry in its incarnation
	// (roundReader), minted as it first applies to it.
	reader roundReader
}

// roundReader is the reader a round's own git runs name in the incarnation
// it applies to (gitproc.Cmd.Repairer: the peel of the tags it sets), so what
// they report is HELD for the round rather than made the copy's scrub at once
// (report.go, HELD FOR THE REQUEST) — and SETTLED once the round's answer is
// known (settleReader): CONSUMED by the repair of a pack's or an object's
// damage the round asked of its own git's failure (repairDamaged), which
// answers exactly what such a report names, and otherwise handed to the
// ordinary rules (handOn), as a request's reader's release hands them. Made
// due beside that repair, the report's scrub read the whole copy — 1.13 times
// its pack bytes — while the repair fetched one pack again, and raced it to
// that pack: the cost a request's failure is spared by the same hold, which a
// round's own failure repaired in place would pay just the same.
type roundReader struct {
	inc *incarnation
	by  gitproc.Repairer
}

// settleReader ends rd, a round's reader: what its runs reported is the
// round's repair's — consumed, counted reports{outcome=request} — when
// answered says the round asked one that answers it, and handed to the
// ordinary rules otherwise; either way its name is closed. e.mu is held.
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
	// escalate: the caller's pack or object damage of the incarnation
	// failedOn is being repaired in place — a REQUEST's (Cache.Rebuild), or
	// the damage a sync's own round's git met (damagedRound) — for this
	// reason, and this round is the in-place round that repairs it: should
	// the round repair nothing, a request is not answered by it but handed to
	// the scrub and, past it, to the whole rebuild (Cache.escalate) — only
	// while the round's incarnation IS failedOn, since one a rebuild made
	// since is not the copy that failed. Should the round's own git meet the
	// damage again, the caller goes on to the scrub too (damagedRound).
	escalate RebuildReason
	failedOn *incarnation
	// began is when the failing process began (Failure.Began), the
	// escalation's guard: a repair written into failedOn after it — while the
	// repair ran — may be what ended the failure, so the caller is answered
	// as the guard answers it (standOrSync) rather than handed on to a
	// rebuild.
	began time.Time
	// request: the caller is a REQUEST's repair (Cache.Rebuild) — its own git
	// failed over a Synced it held, and it runs that git again over whatever
	// it is answered — rather than a sync, which is answered only by a round
	// that converged. So a request's in-place round that repaired nothing
	// hands it on to the scrub, and the guard answers it from the copy as it
	// stands; a sync whose own round's git met the damage is answered by its
	// in-place round once that converges, whatever it repaired — that
	// round's git met nothing — and the guard hands it to a round of the copy
	// as it stands, never answers it from a copy no round converged.
	request bool
	// scrubbedOn is the incarnation the repair's SCRUB ran on for this
	// caller (Cache.escalate) and found damaged, its findings handed to the
	// round the caller then waits on, and whole is the packs that scrub
	// found whole. Should that round's own git meet the damage again, no
	// repair in place is left to try: the caller goes to the whole rebuild,
	// seeded with whole (damagedRound).
	scrubbedOn *incarnation
	whole      []string
	// asIs: the caller is such a request, answered from the copy as it
	// stands once the round that may be repairing it has ended (asItStands):
	// with Kind SyncNone, whatever the round queued behind it read.
	asIs bool
	// checks: the caller is a CHECK of the copy (Verification.Run), which a
	// round whose own git names damage of its incarnation answers with that
	// damage and the incarnation pinned (damage, got) rather than with the
	// repair every other caller waits on: a check names damage, and only an
	// operator's Repair repairs it.
	checks bool
	got    Synced
	err    error
	damage *damaged
	// found is what the round's own check found and repaired, handed to a
	// check (round.found).
	found []string
	// done is closed once the caller has its answer — and its pin, taken
	// under the entry's mu before the close, so nothing between the round's
	// end and the caller's wake can retire what it is handed. The caller's
	// own channel rather than its round's, because a caller can outlive the
	// round it joined: a failed rebuild queues it again.
	done chan struct{}
	// gone: the caller's own context ended before a round answered it, so
	// no round pins anything for it or writes anything to it — it read what
	// it had as it left.
	gone bool
}

// newWaiter is a caller asking for fresh.
func newWaiter(fresh statelog.Freshness) *waiter {
	return &waiter{fresh: fresh, done: make(chan struct{})}
}

// Sync hands out repoID's directory, PINNED, holding every row up to a
// position at least as fresh as fresh asks — the one way to a repository's
// directory. Every caller defers the answer's Release.
//
// A caller whose need the cache already meets — a stale or consistent-prefix
// read, or a session read whose floor the cache has reached, of a repository
// this process has converged — is answered at once with no estate call
// (SyncNone). Any
// other joins a round (see the round rules above); a linearizable caller,
// and one with a lag bound (which only the estate can measure), is never
// answered at once. ctx bounds the caller's own wait and nothing else: the
// round runs on for everyone else.
func (c *Cache) Sync(ctx context.Context, repoID string, fresh statelog.Freshness) (Synced, error) {
	s, _, _, err := c.sync(ctx, repoID, fresh, false)
	return s, err
}

// sync is Sync, and for a CHECK of the copy (checks; Verification.Run) also
// the damage a round's own git met in the incarnation it answers pinned
// (waiter.checks, damagedRound) — nil when it met none — and what the
// round's own check found failing and repaired in place (round.found).
func (c *Cache) sync(ctx context.Context, repoID string, fresh statelog.Freshness, checks bool) (Synced, *damaged,
	[]string, error) {

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
		if s, ok := c.atOnce(e, fresh); ok {
			e.mu.Unlock()
			return s, nil, nil, nil
		}
		w := newWaiter(fresh)
		w.checks = checks
		c.join(e, w)
		e.mu.Unlock()
		s, err := c.await(ctx, e, w)
		// Written before the answer's close, which await waited for, or
		// never: a caller that left is handed nothing (Cache.run,
		// damagedRound), since it reads these as it leaves.
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
		// Answered while the caller gave up: the pin the round took for it
		// goes back.
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
		w.got = c.pin(e, inc, SyncNone)
	default:
		w.got = c.pin(e, inc, kind)
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
		return fmt.Errorf("%w: a sync states its freshness", code.ErrBadLevel)
	case !fresh.Level.Valid():
		return fmt.Errorf("%w: %q is not a read level", code.ErrBadLevel, fresh.Level)
	case fresh.Level == statelog.ReadSession && fresh.MinPosition.IsZero():
		return fmt.Errorf("%w: a session sync names the position it must include", code.ErrBadLevel)
	case !fresh.MinPosition.IsZero() && fresh.MinPosition.Stream != topics.CodeLogStream:
		return fmt.Errorf("%w: a sync's floor is on %s, and a repository's rows are on %s",
			ErrInvalid, fresh.MinPosition.Stream, topics.CodeLogStream)
	}
	return nil
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
// needs; e.mu is held.
//
// Only from an incarnation CONVERGED IN THIS PROCESS — never from a state a
// previous one left: the state's position names a record of the log it was
// read from, and a log restored from an older copy while the node was down
// keeps its identity (ADR-0053) and reuses those positions for other
// records, so a stale or session answer from the state would serve a
// history the log no longer holds. A repository's first caller after a boot
// pays the full round D3 reads anyway; every one after it is answered here.
func (c *Cache) atOnce(e *entry, fresh statelog.Freshness) (Synced, bool) {
	inc := e.current
	if inc == nil || !inc.converged || fresh.Bounded() {
		return Synced{}, false
	}
	switch fresh.Level {
	case statelog.ReadStale, statelog.ReadConsistentPrefix:
		if !fresh.MinPosition.IsZero() && !reached(inc.at, fresh.MinPosition) {
			return Synced{}, false
		}
	case statelog.ReadSession:
		if !reached(inc.at, fresh.MinPosition) {
			return Synced{}, false
		}
	default:
		return Synced{}, false
	}
	return c.pin(e, inc, SyncNone), true
}

// reached reports whether at is at or past floor (both on the code log).
func reached(at, floor statelog.Position) bool {
	before, err := at.Before(floor)
	return err == nil && !before
}

// join adds w to the round its freshness may be answered by, starting one
// when none runs; e.mu is held.
//
// A linearizable or a bounded caller joins the round QUEUED behind the one
// running, whose barrier may predate it. A session caller joins the running
// round when that round is a session round whose floor is at or past its
// own — it then answers at a position the caller needs — and otherwise the
// queued one. A stale or consistent-prefix caller with no floor reached here
// because no incarnation of the repository is converged in this process, and
// any round answers it.
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
		return have.Level == statelog.ReadSession && reached(have.MinPosition, want.MinPosition)
	default:
		return want.MinPosition.IsZero()
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
	if !b.MinPosition.IsZero() && (out.MinPosition.IsZero() || !reached(out.MinPosition, b.MinPosition)) {
		out.MinPosition = b.MinPosition
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

// run is one round, on a goroutine of the cache's own: converge, then answer
// every caller still waiting — each with its own pin, taken before it is
// woken — and start the round queued behind it.
//
// A REBUILD THAT FAILED ANSWERS ONLY ITS OWN CALLERS. The callers of an
// ordinary round it was queued over (queueRebuild) asked for a sync, not a
// repair: the incarnation the rebuild would have replaced is still current
// and still serving, so each of them is queued again for an ordinary round
// on it rather than told of a repair somebody else asked for — unless the
// round found the repository purged, which answers everybody. The failure is
// also recorded on that incarnation (failedRebuild), which is what the next
// Rebuild of it is refused by.
//
// A ROUND WHOSE OWN GIT NAMED DAMAGE OF ITS INCARNATION IS ANSWERED BY THE
// REPAIR OF IT (damagedRound). The round's git processes before its answer —
// the peel of the tags it sets, a full round's index — run in the incarnation,
// and a full round holds what its sweep left to the pool's own shape check, so
// a failure classified as damage (DamageReason) is the cache's to repair
// exactly as a host's process's is, under the same rules — a pack's or an
// object's damage IN PLACE FIRST, one GET a damaged pack, as a request's is
// (rebuild.go), since a peel over every tag a full round sets meets a damaged
// tag object as readily as any request does, and read whole into a new
// incarnation for it a repository of the largest size the defaults admit is
// thousands of GETs where one would do. Answered with the failure instead, no
// caller would ever hold a Synced to ask for the repair with, and every round
// after it would meet the same directory: a cache refused for its shape, with
// a tag pushed since, failed every sync until a restart.
//
// The body runs holding the entry's write lock: nothing changes what the
// directory holds beside a round (entry.write).
//
// A ROUND WHOSE OWN GIT NAMED A PACK IN WORDS ENDS BEFORE IT HASHES IT
// (hinted, confirmRound) — through 2.56's bare message alone, or beside a row
// of git's table naming the damage, as 2.55 always and 2.56 mostly report it.
// The hash is minutes of a slow disk for the largest
// pack a push stores, and writes nothing; so the round lets the write lock go
// AND stops being the repository's running round first — its incarnation
// pinned until its callers are answered, so no eviction takes the directory
// the hash reads — and the round queued behind it starts at once. Kept
// running, it held every LINEARIZABLE caller of the repository behind the
// hash, since such a caller never rides a round that began before it asked
// (join): every answer that names a ref is linearizable, so every fetch's
// and every push's advertisement on the node waited for a verdict none of
// them had asked for. Only the hinted round's own callers wait for it, and
// once it is reached they are answered as any round's are: by the repair of
// the damage — the pack it confirms named to it, one GET — if the incarnation
// is still the entry's, or with git's own failure when nothing but a sound
// pack's hint named any. An incarnation a rebuild replaced while the pack was
// hashed serves nobody, so its callers are queued again for a round on the
// current one; a pack that rounds queued meanwhile meet the same hint about
// is hashed once, every one of them waiting for the one verdict (packCheck).
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
		held = c.pin(e, h.inc, SyncNone)
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
	// hint was hashed, so what its git met is no copy that still serves — or
	// its hints confirmed nothing to repair and a REPAIR was written into the
	// copy after its git began (predates): a request's repair of the same
	// pack, whose reinstall moved the files under the hash so it decided
	// nothing, or the scrubber's of the damage the hint stood for. Its git's
	// failure may be of what that repair replaced, and a sync told it would
	// fail on a copy that no longer fails: its callers are synced again.
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
	// AN IN-PLACE ROUND of a pack's or an object's damage (Cache.Rebuild,
	// damagedRound) that converged repaired what its callers' failures asked
	// of it, or found nothing to. A REQUEST it repaired nothing for is handed
	// on rather than answered from the copy that failed it: to the scrub, and
	// past it to the whole rebuild (Cache.escalate). A sync whose own round
	// met the damage (no waiter.request) is answered by it either way: the
	// round's own git met nothing this time, and a converged round is all a
	// sync asked for.
	var escalating []*waiter
	requeued := func(w *waiter) bool { return (replaced || requeue && !w.rebuild) && !w.gone }
	repairs := func(w *waiter) bool {
		return !requeued(w) && w.escalate != "" && w.failedOn == inc && err == nil && r.rebuild == ""
	}
	asks := func(w *waiter) bool { return repairs(w) && w.request }
	if r.did.repaired() && r.did.reinstalled == 0 && r.did.restored == 0 && c.metrics != nil &&
		slices.ContainsFunc(r.waiters, repairs) {
		// COUNTED BY WHAT IT DID, never by its mark: an in-place round that
		// only dropped is a derived file's repair. One that put a pack in —
		// over its files, or one the copy had lost — was counted `stored` by
		// its apply, whoever waited on it. And counted BEFORE anybody is
		// answered, so a caller that reads the count once it has its answer
		// finds it there.
		c.metrics.Add(metrics.CodeCacheRebuilds, 1, metrics.Attrs{"reason": string(RebuildDerived)})
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
			// HANDED ONLY TO A CHECK STILL WAITING, as answer pins nothing
			// for one that left: that caller read what it had as it left,
			// outside e.mu, so a write now races it.
			w.found = r.found
		}
		c.answer(e, w, inc, kind, err)
	}
	if e.running == r {
		e.running = nil
	}
	c.start(e)
	// THE MAINTAINER IS WOKEN ONCE NO ROUND HOLDS THE REPOSITORY. A pass
	// starts no run on a repository whose round runs (runnable), so the kick
	// a full round once sent from inside itself met that round still running
	// and left the repository to the next tick, MidxDebounce later: a
	// graph-first run, an index repair and a copy's first index, each due
	// at once, waited for it. A round queued behind this one owes it on.
	wake := c.owedWake(e)
	e.mu.Unlock()
	if wake {
		c.kickMaintainer()
	}
	// Let go only now: every caller holds its own pin or is queued again,
	// so the copy was never evictable between the hash and their answers.
	held.Release()
	if repair.quit != nil {
		// COUNTED AND LOGGED BEFORE IT IS ANSWERED, as Rebuild's own
		// refusal is: a caller woken by the refusal reads a counter and a
		// log that already hold it. Reported under e.mu it would hold the
		// entry through a log write; answered first, the caller could read
		// both before they did.
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
// of the entry's current incarnation; e.mu is held. Every caller of r is
// answered or handed on here but those a refused rebuild answers and those
// the repair's scrub is for, which it leaves to Cache.run (roundRepair).
//
//   - A CHECK (waiter.checks) is answered with the damage and the incarnation
//     pinned: it names the damage, and an operator's Repair is what repairs
//     it — repaired under it instead, a plain check would quietly download the
//     repository and report the repaired copy clean.
//   - Every other caller waits on the REPAIR, asked for only when one waits.
//     A SHAPE refusal met by a round that did not sweep — a delta's peel — is
//     repaired IN PLACE (markWhole): the incarnation is read whole by the
//     round queued behind this one, whose sweep puts every shape the pool
//     refuses right and reuses every pack it holds, so it costs no GET; the
//     callers are queued for that round. DAMAGED DERIVED FILES (the peel's
//     cat-file reads the multi-pack-index) are repaired IN PLACE the same
//     way, the next round dropping the files the damage named — unless THIS
//     round had already dropped every one of them a drop can repair (their
//     declarations drop on a report, OnReported), in which case the damage
//     survived its drop, or no drop ends it, and is a pack's
//     (RebuildCorrupt), below.
//     A PACK'S OR AN OBJECT'S DAMAGE (RebuildCorrupt, RebuildMissing) is
//     repaired as a request's is, IN PLACE FIRST, one GET a damaged pack
//     (repairDamaged).
//     A shape refusal past the sweep of the very round that met it is a
//     REBUILD, asked for as Rebuild asks for one (askRebuild: joined if one
//     is queued, refused by the same rules, counted as an ask of the
//     incarnation's line): no read in place ends it, and no scrub finds it.
//     A refused rebuild answers its callers as it answers Rebuild
//     (ErrRebuildRefused); one that fails answers them as it answers the
//     callers of any round it was queued over — queued again for an ordinary
//     round, which meets the damage again and is refused by the window the
//     failed attempt opened.
func (c *Cache) damagedRound(e *entry, r *round, d *damaged) roundRepair {
	c.log.Info("code_sync_damaged", "repo", e.id, "reason", string(d.reason), "swept", d.swept,
		"packs", d.packs, "error", d.err)
	var asking []*waiter
	for _, w := range r.waiters {
		switch {
		case w.gone:
			c.answer(e, w, nil, "", d)
		case w.checks:
			w.got, w.damage, w.found = c.pin(e, d.inc, SyncNone), d, r.found
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
		d.reason == RebuildDerived && c.image.reported(d.drops)&c.image.dropping(EventReported)&^d.dropped != 0:
		c.markWhole(d.inc, Failure{Reason: d.reason, Drops: d.drops, Began: d.began}, dropRound)
		for _, w := range asking {
			c.queue(e, w)
		}
		return roundRepair{}
	}
	reason := d.reason
	if reason == RebuildDerived {
		// Every file the damage names that a drop can repair (its
		// declaration drops on a report) was dropped by the round that met
		// it — or none can be, a declaration that keeps its files on a
		// report being repaired by no drop: what git could not read is no
		// derived file a drop of the copy ends, but a pack's.
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
// a refused rebuild to report (quit) and the callers to answer with it once
// it is reported (refusee), the callers the repair's scrub is for
// (scrubbing, Cache.escalate) — and whether the repair it asked answers what
// the round's own git reported (answers, settleReader): a pack's or an
// object's repair does, every step of it; a shape's or a derived file's, read
// in place, reads no pack and answers none of a stored report, a hint or a
// crash.
type roundRepair struct {
	quit      *refused
	refusee   []*waiter
	scrubbing []*waiter
	answers   bool
}

// repairDamaged is damagedRound for a PACK'S OR AN OBJECT'S damage d, of
// reason (RebuildCorrupt or RebuildMissing), that a round's own git met: the
// four steps a request's repair takes (Cache.Rebuild, rebuild.go), each
// caller taken on from the step it has reached; e.mu is held, and its answers
// are damagedRound's, every one of them answering what the round's own git
// reported (roundRepair.answers).
//
//  1. THE GUARD. Damage met by a process that began before this process last
//     wrote a repair into the copy may be of what the repair replaced — a
//     request's repair of the same pack landing while the round's hint was
//     hashed — so every caller is answered as Rebuild's guard answers it: a
//     request from the copy as it stands, a sync by a round of it
//     (standOrSync), which meets the damage again or does not.
//  2. IN PLACE, for a caller that has taken no step yet: the copy marked to
//     be read whole by a FORCED round (markWhole, repairMark.forces) — every
//     index file held to its checksum whatever its vouch — that installs
//     again over its files every pack a confirmed hint named (d.packs), one
//     GET a pack, and the caller queued for it (waiter.escalate): it answers
//     a sync once it converges, and hands a request it repaired nothing for
//     to the scrub (Cache.run). Skipped, as a request's is, when no pack it
//     could fetch is named — none, or every one held off after a failed GET
//     (repairMark.fetchable), which install leaves as it stands — and a
//     forced round has already said what one would: the round that met d was
//     itself forced (d.forced), or a forced round of the copy found nothing
//     within ScrubCooldown with no repair written since (forcedLately).
//  3. THE SCRUB, for a caller whose in-place round met the damage again, or
//     that skipped it: handed back to Cache.run, which runs Cache.escalate for
//     them once e.mu is let go — the scrub finds a pack body no index shows,
//     and past it the whole rebuild is seeded with every pack it found whole.
//  4. THE WHOLE REBUILD, for a caller whose scrub's findings were put to its
//     round and whose round still met the damage (waiter.scrubbedOn): asked
//     as escalate asks it, seeded with what that scrub found whole, refused
//     by the same rules, its failure the caller's answer — and NOT ASKED
//     while a pack it would download is held off (repairMark.heldOffFor): it
//     would ask the store for that very pack again within the backoff, so
//     the caller is answered with the store's own refusal of it instead.
//
// Each caller passes each step at most once on one incarnation, so a repair
// ends whatever the damage: a pack the object store will not hand over goes
// from the forced round, whose one GET of it fails and holds it off, to the
// scrub, and from the scrub's round to that failure — no rebuild asks the
// store for it again, and no forced round is run that could fetch nothing,
// until its backoff passes and step 2 asks once more.
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
		c.markWhole(inc, Failure{Reason: RebuildStored}, dropRound)
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

// markWhole marks inc to be read WHOLE, IN PLACE, by its next round, for
// reason; its entry's mu is held. Nothing is answered from it at once until
// that round has run (atOnce), the round is a full one — its sweep makes the
// directory its image, reusing every pack it holds — and it counts reason as
// it converges, unless inc was already marked for another. The mark is
// counted on the incarnation (marks), so a round that began before it, whose
// sweep may predate the damage it answers, converges nothing. No new
// incarnation, and no GET for a pack the incarnation holds: a directory the
// pool refuses for its shape lost none of its packs, and a multi-pack-index
// or a commit-graph is a derived file — RebuildDerived also has the round
// drop the files of the declarations f.Drops names that drop on a report
// (incarnation.drop; OnReported — one that keeps its files is asked no drop
// that would move nothing), whatever reason the read is counted under, each
// with f.Began, which the round's drop is guarded by (dropDamaged), and by,
// what asked it.
func (c *Cache) markWhole(inc *incarnation, f Failure, by string) {
	inc.converged = false
	inc.marks++
	if f.Reason == RebuildDerived {
		inc.drop.addSet(c.image, c.image.reported(f.Drops)&c.image.dropping(EventReported), f.Began, by)
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
	// (confirmRound): the in-place repair fetches each again, one GET a pack
	// (Failure.Packs). forced: the round that met the damage was itself a
	// forced in-place round, which held every index file to its checksum
	// (checked.forced) — so another finds nothing a pack's damage named by no
	// hint needs.
	packs  []string
	forced bool
	err    error
}

func (d *damaged) Error() string { return d.err.Error() }

// Unwrap is the git failure, so errors.Is sees gitproc's classification.
func (d *damaged) Unwrap() error { return d.err }

// converging is converge holding ALL of the entry's write lock, waited for
// under the cache's lifetime: whatever else holds the lock holds it for a
// listing or a few links, never for anything that waits on a round, so only
// Stop ends the wait — and a round whose wait Stop ended is one converge would
// refuse. Every pack an adoption handed the round meanwhile is installed
// before the lock goes (releaseWrite), and so before the round's callers are
// answered.
func (c *Cache) converging(e *entry, r *round) (*incarnation, SyncKind, error) {
	if err := c.acquireWrite(c.life, e); err != nil {
		return nil, "", ErrStopped
	}
	defer c.releaseWrite(e, writeExclusive)
	return c.converge(e, r.fresh, r.rebuild, r.of, r.whole, &r.found, &r.did, &r.reader)
}

// converge is one round's body, in order — every step's name is the field
// its log line and its error carry:
//
//  1. pick the incarnation: the entry's, or — once the rows say the
//     repository exists — a NEW one when it has none. The round is FULL when
//     the incarnation is new, has no state, or has not had a full round in
//     this process — so a cache is exact from any starting point (a crash, a
//     full disk, an operator's rm, a log restored behind it while the node
//     was down) at the price of one full read per repository per boot, and
//     every later round is a delta, because nothing but this process has
//     touched the directory since. A REBUILD round (rebuild.go) makes a new
//     incarnation BESIDE the current one whatever it holds, reads it whole
//     at linearizable, and replaces the current one only once it has
//     converged; failing, it throws the new one away.
//  2. read the snapshot (read.go). Two answers are not yet answers, and are
//     read again ONCE at linearizable before anything is concluded from
//     them — a copy behind the log may give either: no such repository (a
//     copy that has not applied its create), and a FULL answer below the
//     incarnation (a copy behind the cache, or the log gone back: only a
//     read at the log's head can say which). A linearizable not-found is the
//     repository PURGED — its id is minted once and never comes back, so
//     every incarnation is retired, the entry dies, and every caller is
//     told; a repository no row holds costs no directory.
//  3. a LINEARIZABLE answer below a position this cache read on the same
//     stream — its own, or any repository's (the log mark) — is the log gone
//     BACK under the cache, which a barrier at the log's head cannot answer
//     otherwise: every incarnation is marked to be read whole, and this one
//     is, now, and converges to the rows the log holds. Any other answer
//     below the incarnation is a copy behind it, and changes nothing — the
//     cache never moves backwards for a stale copy.
//  4. install every pack the directory does not hold (install.go); a pack
//     the store does not have reads the snapshot again at linearizable —
//     whatever level the round read at, and in full whenever the answer that
//     named the pack was — and that read decides it: the round's read came
//     before the store answered, and the row it named may have left the log
//     since (a purge's, or a retirement whose grace the round outlived). A
//     read that still names the pack makes it missing, final, with nothing
//     installed again; one that does not is applied in turn, and a pack the
//     store does not have from it is asked the same way, each not-found by
//     the read after its own.
//  5. on a full answer, the round's index (maintain.go), before the refs'
//     peel reads the packs.
//  6. the refs (refs.go): packed-refs and HEAD, after every pack they need.
//     A delta onto a packed-refs this cache did not write reads ONCE again,
//     full.
//  7. on a full answer, the sweep's pack half (sweep.go).
//  8. the state, when the directory changed (state.go), renamed in last.
//
// A FIRST incarnation that fails is kept, with no state, so the next round —
// full, because it has none — reuses every pack it installed.
//
// A rebuild of `of` is SEEDED with whole, the packs of it its asker has just
// held to their rows (seedRebuild), before its first install.
//
// What each check it runs finds failing and repairs is added to found
// (round.found), and what each apply did to did (round.did).
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

	e.mu.Lock()
	inc := e.current
	// A REPAIR MARK asks a full round too (report.go) — unless a check of
	// the repository is admitted, whose release asks it again.
	full := inc == nil || inc.state == nil || !inc.converged || (inc.repair.due(c.now()) && e.verifying == 0)
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
	// whether it will: an answer only a copy behind the log can have given
	// is asked of the log's head before anything is concluded from it. The
	// store's not-found is no such answer — a read at the head can predate
	// it too — and is decided by the head's read made after it, whatever the
	// level (gone).
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
		var at statelog.Position
		var stream time.Time
		if inc != nil {
			e.mu.Lock()
			at, stream = inc.at, inc.stream
			e.mu.Unlock()
		}
		markStream, markAt := c.mark.read()
		var snap snapshot
		snap, err = c.read(ctx, e.id, at, stream, full, fresh)
		switch {
		case errors.Is(err, code.ErrRepoNotFound) && escalate():
			continue
		case errors.Is(err, code.ErrRepoNotFound):
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
			if inc, err = c.newIncarnation(e, rebuild == ""); err != nil {
				return nil, "", fmt.Errorf("cache: sync %s: incarnation: %w", e.id, err)
			}
			if rebuild != "" {
				building = inc
				if replaced != nil && replaced == of {
					if err = c.seedRebuild(e, replaced, inc, whole); err != nil {
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
			// A copy behind this cache answered a delta: nothing to do,
			// and the cache never moves backwards for one.
			c.account(e, inc, snap, applied{}, time.Since(began))
			return inc, snap.kind(), nil
		}
		if snap.restreamed {
			e.mu.Lock()
			if inc.reread == "" {
				inc.reread = RebuildStream
			}
			e.mu.Unlock()
		}
		if gone != nil {
			// THE READ AFTER A NOT-FOUND DECIDES IT, and nothing is installed
			// again to decide it. This read was made at the log's head after
			// the store said the pack's object is not there. Still naming the
			// pack at the same object, it names an object the store lost
			// while a live row named it — the loss itself — and the round
			// fails here. Applied again instead, it staged and fetched again
			// every pack the not-found had cancelled in flight, asked the
			// store once more for the pack it had just been told was not
			// there and, on a forced round, held every index file to its
			// checksum again — all to fail the same way, since no read makes
			// the store hold an object. This read names the pack whenever its
			// row is live: a full answer names every live pack, and this read
			// is asked in full whenever the answer that named the pack was
			// (below), while a delta from the incarnation's own position names
			// every pack added above it, as one a delta named was. So a read
			// that does not name it is the row gone from the log since —
			// purged or retired, its object collected meanwhile — and the
			// round goes on to what this read holds.
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
		*found = append(*found, res.checked.findings(c.image)...)
		did.add(res)
		var missing *ErrPackMissing
		switch {
		case errors.Is(err, errRefsNotOurs) && !snap.full && !askedFull:
			askedFull, full = true, true
			continue
		case errors.As(err, &missing) && !missing.Damaged:
			// THE STORE'S NOT-FOUND IS ASKED OF THE LOG'S HEAD, AFTER IT,
			// WHATEVER LEVEL THE ROUND READ AT. Asking only a round below
			// linearizable took a linearizable read for the head's answer,
			// but its barrier came before the store answered, and the row
			// it named may have left the log since: a purge removes a
			// repository's pack rows part by part before the repository
			// itself, and a retired pack's object outlives its live row only
			// by its domain's grace (ADR-0051), which a long enough round
			// outlives. The collector deletes an object once nothing names
			// it, so such a round failed every caller for a pack no row
			// names, and logged code_pack_missing for it. Only a read made
			// after the store's answer can say, and the next read decides
			// it (THE READ AFTER A NOT-FOUND DECIDES IT, above).
			//
			// EVERY NOT-FOUND IS ASKED SO, each by the read after its own.
			// Asked once a round, a second pack the store failed — one the
			// first not-found had cancelled or the round had not reached, met
			// from the head's answer — was final on a read made BEFORE its
			// not-found: a pack whose row left the log after that read, its
			// object collected meanwhile, failed the round as missing though
			// no row names it, the very failure the asking is for. Asked so,
			// the round still ends: a read that names the pack ends it there,
			// and one that does not goes on without it — and a row gone from a
			// log that does not go back never returns, so every read more
			// follows another pack's row leaving the log under the round (a
			// log gone back is read whole, rewound, and asked the same way).
			//
			// A PACK A FULL ANSWER NAMED IS DECIDED BY A FULL READ. The log
			// may answer a delta in full (page 1 decides: where the delta
			// would outweigh it, or its floor is below the swept tombstones or
			// on another generation), and a full answer names packs at or
			// below the incarnation's position — one whose files the copy lost
			// is installed again from it — which no delta from that position
			// reaches: asked as one, the decision would take a lost pack for a
			// row gone, and converge the copy without a pack its rows name.
			//
			// A DAMAGED answer is final at once, below: a purge or a
			// retirement removes an object and never alters one, so bytes
			// that are not the row's are the store's fault whatever the log
			// holds now.
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
				// The round's own git named a pack in words (a hint,
				// gitproc.Hint) — damage it named no other way (2.56's bare
				// "%s"), or a pack's or an object's damage beside it (a row of
				// git's table: 2.55's of the same sentence, or another report
				// of the run's): the pack it nominates is held to its row —
				// by Cache.run, once the write lock is let go and the round
				// has ended, since a hash is minutes of a slow disk and nobody
				// but the round's own callers waits on it — and a pack that is
				// not the row's is named to the repair, which then fetches
				// THAT pack again rather than look for it: the host's rule for
				// a request's git (host's confirmed). With no classification,
				// only a pack that is not its row's asks a repair at all.
				h := &hinted{inc: inc, name: snap.repo.Name, full: snap.full, hints: hints, err: err,
					classified: ok, damage: f, forced: res.checked.forced}
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
					drops: f.Drops, dropped: res.dropped, began: f.Began, forced: res.checked.forced, err: err}
			}
			return nil, "", err
		}
		if rebuild != "" {
			c.promote(e, replaced, inc, rebuild, time.Since(began))
		}
		c.account(e, inc, snap, res, time.Since(began))
		return inc, snap.kind(), nil
	}
}

// reread marks every current incarnation which selects for a WHOLE read: its
// next round reads the repository whole, nothing is answered from it at once
// until then, and the reason is counted when that read completes. It answers
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
		forces    int
		reinstall []checkedFile
	)
	if snap.full {
		// The sweep lists the directory and moves whatever lies outside the
		// image — a running maintenance's lock and temporary files among
		// it — and may remove a pack the run's index would name, which
		// fails every read of the repository. So a full round stops a
		// maintenance on its incarnation first, and none starts while a
		// round runs (maintain.go).
		c.stopMaintenance(e, inc)
		trashed, err := c.tidy(inc, snap)
		if err != nil {
			return res, fmt.Errorf("cache: sync %s: sweep: %w", e.id, err)
		}
		res.trashed = trashed
		// THE CHECK (check.go), before anything is answered from what a
		// previous process left: every index file the snapshot names and
		// every derived file present, held to its trailer unless this
		// process wrote or checked it at the identity it has now. A stored
		// file that fails is reinstalled by the install below, renamed over
		// in place; a derived one is dropped below. With nothing writing
		// beside it — the write lock held, the maintainer stopped — what it
		// hashes is what it repairs.
		// A PACK'S OR AN OBJECT'S REPAIR IN PLACE — a failed request's
		// (Cache.Rebuild) or the damage a round's own git met
		// (repairDamaged): every index file held to its trailer whatever its
		// vouch, and the packs a confirmed hint named installed again — none
		// of it under a check of the repository, whose release asks the round
		// again.
		forces, reinstall = c.requested(e, inc)
		if res.checked, err = c.checkIndexes(ctx, e, inc, snap, forces > 0); err != nil {
			return res, fmt.Errorf("cache: sync %s: check: %w", e.id, err)
		}
		res.checked.damaged = append(res.checked.damaged, reinstall...)
		// What a scrub found failing (scrub.go) is repaired with what the
		// check found — a pack installed again, a derived file dropped — each
		// only while it is still the file that failed (checked.still), and
		// taken off the repair mark once the round has converged.
		found = c.foundByScrub(e, inc)
		res.checked.damaged = append(res.checked.damaged, found...)
	}
	// A pack the check or a scrub found damaged, or a full round found at the
	// wrong size, is installed again by renaming over its files (install.go),
	// and the derived files that may have been built from it go first.
	ins, err := c.install(ctx, e, inc, snap, res.checked)
	// COUNTED BY THE ROUND THAT DID IT, once a round, and here — before
	// anybody is answered: a pack's stored files repaired in place, or a pack
	// the copy had lost put back, one GET a pack (RebuildStored). Never by
	// who waited on the round: a request's forces are read in this apply
	// (requested), so a full round already running when the request asked —
	// a report's repair round — may be the one that repairs it, and the
	// round the request waits on finds nothing to do.
	if ins.replaced+ins.restored > 0 && c.metrics != nil {
		c.metrics.Add(metrics.CodeCacheRebuilds, 1, metrics.Attrs{"reason": string(RebuildStored)})
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
	var strays []string
	if snap.full {
		// A DERIVED-DAMAGE REPAIR drops the files the damage named here,
		// before the round's own index decides: the damage is confined to
		// derived files git only mmaps, which the maintainer rewrites from
		// the packs this incarnation already holds, so the round (an index,
		// at MidxRoundPacks packs) or the maintainer (at once: an index
		// repair, or the graph first) writes sound ones over the same packs
		// — no download, where a whole-repository rebuild would GET every
		// pack again. By PART: the multi-pack-index's files, the
		// commit-graph, or both for a chunk-format message, which names
		// neither file. What a failed request or the round's own git asked
		// (incarnation.drop), what any run reported (incarnation.repair) and
		// what the round's check found failing its trailer, each read off its
		// own mark, whatever the read is counted under (dropDamaged).
		if err = c.dropDamaged(e, inc, &res); err != nil {
			return res, fmt.Errorf("cache: sync %s: drop derived files: %w", e.id, err)
		}
		// SETTLED ONCE ACTED ON, never only once the round converges: the
		// check has held what it was forced to, each finding is installed
		// again, dropped, held off or already replaced, and what the round
		// fails at past here — its refs' peel meeting damage nothing above
		// could fetch — undoes none of it. Left on the mark by a round that
		// failed at the peel, a request's forces kept every later round of
		// the copy full and FORCED — every index file hashed again, about a
		// quarter of the copy's pack bytes, with nothing it could fetch —
		// and never recorded the verdict that bounds the next (forcedAt).
		c.settleFound(e, inc, found, unrepaired)
		c.settleRequested(e, inc, forces, reinstall, unrepaired)
		if forces > 0 {
			// A FORCED round that repaired nothing bounds the next one
			// (forcedLately): its verdict holds for ScrubCooldown while no
			// repair is written.
			e.mu.Lock()
			inc.forcedAt, inc.forcedEnded = time.Time{}, time.Time{}
			if !res.repaired() {
				inc.forcedAt, inc.forcedEnded = c.now(), time.Now()
			}
			e.mu.Unlock()
		}
		// The sweep's pack half is DECIDED here and done once the refs are
		// in; an index that may name a pack it will remove goes now, and so
		// does whatever in objects/pack is no pack's file, so the round's
		// index — over exactly the snapshot's packs — is written before the
		// refs' peel reads them, with no dead writer's lock to refuse it.
		var moved bool
		if strays, moved, err = c.packStrays(e, inc, snap); err != nil {
			return res, fmt.Errorf("cache: sync %s: sweep: %w", e.id, err)
		}
		res.trashed = res.trashed || moved
		indexed = c.indexRound(ctx, e, inc, snap)
	}

	refsWritten, err := c.writeRefs(ctx, e, inc, snap, by)
	if err != nil {
		return res, err
	}

	if snap.full {
		trashed, err := c.sweepPacks(e, inc, strays)
		if err != nil {
			return res, fmt.Errorf("cache: sync %s: sweep: %w", e.id, err)
		}
		res.trashed = res.trashed || trashed
	}

	// The in-memory fields are what the directory now reflects. When it
	// CHANGED they are also written down — persist writes them whole —
	// and otherwise only the position moves: nothing between the state's
	// position and this one changed the directory, so a restart's next
	// delta from the state re-reads rows that moved nothing here.
	e.mu.Lock()
	// wrote: the round wrote into the directory — packs in or out, refs, the
	// index, a file swept out of it.
	wrote := ins.added+ins.replaced > 0 || refsWritten || indexed || res.trashed
	changed := snap.full || wrote || !snap.streamCreated.Equal(inc.stream) || !sameRepo(inc.repo, snap.repo)
	prevAt, prevStream, prevRepo := inc.at, inc.stream, inc.repo
	inc.at, inc.stream, inc.repo = snap.position, snap.streamCreated, snap.repo
	if snap.full {
		// The directory holds exactly the snapshot's packs now: their rows'
		// count, whatever the incarnation counted before the round.
		inc.objects = 0
		for _, p := range snap.packs {
			inc.objects += p.Objects
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
			inc.at, inc.stream, inc.repo = prevAt, prevStream, prevRepo
			e.mu.Unlock()
			return res, fmt.Errorf("cache: sync %s: state: %w", e.id, err)
		}
	}
	if snap.full {
		e.mu.Lock()
		inc.converged = c.rereads.Load() == epoch && inc.marks == marks
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
		if reason != "" && reason != RebuildStored && c.metrics != nil {
			c.metrics.Add(metrics.CodeCacheRebuilds, 1, metrics.Attrs{"reason": string(reason)})
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

// sameRepo reports whether two reads of a repository's row found it the same,
// whatever position each was read at.
func sameRepo(a, b code.RepoRef) bool {
	a.Position, b.Position = statelog.Position{}, statelog.Position{}
	return a.ID == b.ID && a.Name == b.Name && a.Status == b.Status && a.ManagedBy == b.ManagedBy &&
		a.Owner == b.Owner && a.DefaultBranch == b.DefaultBranch && a.Description == b.Description &&
		slices.Equal(a.Protections, b.Protections) && a.DeletedAt.Equal(b.DeletedAt) &&
		a.Refs == b.Refs && a.LivePacks == b.LivePacks && a.LiveBytes == b.LiveBytes
}

// account is a completed round's record: its duration by kind, its log line,
// a repository alone over the budget, and the gauges — and what it tells the
// background work.
//
//   - Packs added restart the maintainer's debounce (lastAdded): a burst of
//     pushes lands before the index that covers it is written. The first of
//     them no index covers starts its ceiling (uncoveredSince): pushes that
//     never stop for a debounce still have their packs indexed within
//     MidxMaxWait.
//   - The cache over its budget kicks the janitor at once, rather than at its
//     tick: a burst of cold syncs would otherwise overshoot the budget by
//     everything that arrived in a minute.
//   - A FULL round kicks the maintainer: it may have made the incarnation one
//     the maintainer works on (converged in this process) or written the
//     round's index without a bitmap, either of them due at once once its
//     packs have stood still. A delta adding packs needs no kick — they are
//     due only after the debounce, which the maintainer's tick is.
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
	if res.trashed || c.total() > c.maxBytes {
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
// incarnation retired (each goes into trash/ with its last reader, the
// unpinned current one once both locks are let go), the entry gone, and
// every caller queued behind the round told why. Only a
// LINEARIZABLE not-found purges (converge), and it answers the callers that
// arrived after the round began as well: a repository's id is minted once
// and never comes back, so no read that began after them could find it.
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
