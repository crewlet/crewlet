package cache

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"path/filepath"
	"slices"
	"time"

	"github.com/crewlet/crewlet/internal/gitlet"
	"github.com/crewlet/crewlet/internal/gitlet/gitproc"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/statelog/metrics"
	"github.com/crewlet/crewlet/internal/words"
)

// REQUEST-TIME REPAIR (package doc, "Damage is repaired once, in place where
// nothing is lost"). [DamageReason] reads a failure from what the pool read
// of git's trace2 formats, never git's words, which a request can write; a
// hint decides nothing until [Cache.ConfirmHint] holds its pack to its row.
// A shape and derived files are repaired in place by a whole read (markWhole).
// A pack's or an object's damage takes four steps, a request's (repairRequest,
// then escalate) and a round's own git's (damagedRound, repairDamaged) alike:
// 1. THE GUARD (predates, asked again before 3 and 4); 2. IN PLACE, a forced
// round (bounded by forcedLately); 3. THE SCRUB (scrubFor); 4. THE WHOLE
// REBUILD into the next incarnation beside the one readers hold, seeded
// (seedRebuild), joined by every asker (askRebuild), and refused where it
// cannot help (rebuildRefusal) or while a pack it needs is held off.

// RebuildReason is why a repository was read whole into a cache again: a
// closed set ([RebuildReasons], [RebuildReason.Valid]) and the reason
// attribute of crewlet.code.cache.rebuilds.
type RebuildReason string

const (
	// RebuildCorrupt is gitproc.ErrCorrupt damage not confined to derived files:
	// a pack or an index git could not read. Counted only for the whole rebuild,
	// once neither the in-place round (RebuildStored) nor a scrub found what to
	// fetch again.
	RebuildCorrupt RebuildReason = "corrupt"
	// RebuildDerived is damage git named only in derived files
	// (gitproc.Damage.Derived): the multi-pack-index, its bitmap and reverse
	// index, the commit-graph. Written from the packs the incarnation holds, they
	// are repaired IN PLACE by a whole read that drops the files named
	// (gitproc.Damage.Drops), with no download where a rebuild would GET every
	// pack; damage that survives the drop escalates to RebuildCorrupt
	// (damagedRound).
	RebuildDerived RebuildReason = "derived"
	// RebuildMissing is gitproc.ErrMissingObject: an object a row names that no
	// installed pack holds. Repaired and counted as RebuildCorrupt is.
	RebuildMissing RebuildReason = "missing"
	// RebuildShape is the pool's refusal to start git in the incarnation
	// (gitproc.ErrRepoShape): its config is not gitproc.RepoConfig byte for byte,
	// or it holds a commondir or an alternates file. Every process is refused, so
	// no retry or delta round (which never sweeps) ends it: a full round's sweep
	// repairs it in place (markWhole), counted by that round, and only a shape
	// that sweep did not end is rebuilt.
	RebuildShape RebuildReason = "shape"
	// RebuildStream is a cache whose log was RECREATED under it: its stream
	// identity is not the one the cache read from (ADR-0083; a snapshot restore
	// is not a recreation). Counted by the round that reads it whole.
	RebuildStream RebuildReason = "stream"
	// RebuildRewound is a cache AHEAD of its log on the same stream: a
	// linearizable answer below a position this cache read, which only a
	// log that went back can give — a broker restored from an older copy.
	// Counted by the round that reads it whole.
	RebuildRewound RebuildReason = "rewound"
	// RebuildVerify is an operator's repair ([Verification.Run]): in place when
	// everything the check found is the copy's own directory out of place, by a
	// rebuild otherwise or for what that leaves, each counted once. Never a git
	// process's: Rebuild refuses it.
	RebuildVerify RebuildReason = "verify"
	// RebuildStored is a pack's stored files (.pack, .idx, .rev) installed again
	// IN PLACE, one GET a pack, over files a check or a scrub found failing, a
	// full round found at another size or missing, or a confirmed hint named
	// (install.go): the first repair of a pack's or an object's damage, counted
	// once a round by a round that fetched a pack. Never refused: what drives it
	// is bounded already (a check holds only what is unvouched; a scrub and a
	// forced round run at most once a ScrubCooldown, forcedLately).
	RebuildStored RebuildReason = "stored"
)

// RebuildReasons is every [RebuildReason].
var RebuildReasons = []RebuildReason{RebuildCorrupt, RebuildDerived, RebuildMissing, RebuildShape, RebuildStream,
	RebuildRewound, RebuildVerify, RebuildStored}

// gitReasons are the reasons a git process's failure reaches [askRebuild]
// with, so the ones crewlet.code.cache.rebuild.refusals can carry. Not
// RebuildDerived: always repaired in place, it is never refused, and damage
// surviving its drop is asked as RebuildCorrupt.
var gitReasons = []RebuildReason{RebuildCorrupt, RebuildMissing, RebuildShape}

// rebuildReasons are every reason [Cache.Rebuild] takes: the git reasons plus
// RebuildDerived.
var rebuildReasons = append(append([]RebuildReason{}, gitReasons...), RebuildDerived)

// inPlaceReasons are the damage a whole read IN PLACE repairs, with no new
// incarnation: a shape the sweep puts right and derived files the round drops
// (no download), and a pack's stored files installed again (one GET a pack).
var inPlaceReasons = []RebuildReason{RebuildShape, RebuildDerived, RebuildStored}

// inPlace reports whether r is repaired by a whole read in place rather than a
// new incarnation.
func inPlace(r RebuildReason) bool { return slices.Contains(inPlaceReasons, r) }

// Valid reports whether r is one of [RebuildReasons].
func (r RebuildReason) Valid() bool { return slices.Contains(RebuildReasons, r) }

// Failure is what a git process over a cache failed with, as the cache
// repairs it ([DamageReason], [ShapeReason]; [Cache.Rebuild]).
type Failure struct {
	// Reason is the repair the failure asks.
	Reason RebuildReason
	// Drops is the derived files a [RebuildDerived] repair drops
	// (gitproc.Damage.Drops), and none for any other reason.
	Drops gitproc.Part
	// Began is when the failing process began (gitproc.Damage.Began; zero when
	// unknown): a drop spares a file written after it, and a pack's or an
	// object's repair answers from the copy a failure that predates a repair.
	Began time.Time
	// Packs is the packs a confirmed hint named ([Cache.ConfirmHint]), by
	// checksum: a [RebuildCorrupt] or [RebuildMissing] repair installs each
	// again in place, one GET a pack, and none for any other reason.
	Packs []string
}

// DamageReason reads the failure of a git process run IN one of this cache's
// incarnations as the [Failure] the cache repairs ([Cache.Rebuild]), if any:
// [RebuildShape] for gitproc.ErrRepoShape, [RebuildDerived] for an ErrCorrupt
// gitproc.Damage whose parts are all derived, [RebuildCorrupt] for any other
// gitproc.ErrCorrupt, [RebuildMissing] for gitproc.ErrMissingObject (read
// with errors.Is, so a bare sentinel such as the peel's reads too). Never
// gitproc.ErrNotOurRef, which a want never served and a lost pack print alike
// (the next linearizable sync fetches a lost pack again), nor a refusal of
// the call itself (gitproc.ErrConfig), which no directory causes. A process
// in a push's scratch or an advertisement view reads the incarnation only as
// its alternate, and its ErrCorrupt may be the client's pack: its caller asks
// [ShapeReason].
func DamageReason(err error) (Failure, bool) {
	var dmg *gitproc.Damage
	var began time.Time
	if errors.As(err, &dmg) {
		began = dmg.Began
	}
	switch {
	case errors.Is(err, gitproc.ErrRepoShape):
		return Failure{Reason: RebuildShape}, true
	case dmg != nil && errors.Is(dmg.Kind, gitproc.ErrCorrupt) && dmg.Derived():
		return Failure{Reason: RebuildDerived, Drops: dmg.Drops(), Began: began}, true
	case errors.Is(err, gitproc.ErrCorrupt):
		return Failure{Reason: RebuildCorrupt, Began: began}, true
	case errors.Is(err, gitproc.ErrMissingObject):
		return Failure{Reason: RebuildMissing, Began: began}, true
	}
	return Failure{}, false
}

// ShapeReason is [DamageReason] for a git process in a scratch or an
// advertisement view of an incarnation: only the pool's refusal of THE
// ALTERNATE's shape (gitproc.ShapeError.InAlternate) is the cache's to
// repair; a refusal of the scratch's own shape is one request's directory,
// which the next makes again.
func ShapeReason(err error) (Failure, bool) {
	var shape *gitproc.ShapeError
	if errors.As(err, &shape) && shape.InAlternate {
		return Failure{Reason: RebuildShape}, true
	}
	return Failure{}, false
}

// Rebuild repairs the incarnation failed was handed out of for f, what the
// host's git process over failed.Dir failed with ([DamageReason],
// [ShapeReason]), and answers the repaired directory pinned. [RebuildShape]
// and [RebuildDerived] are read whole IN PLACE: what the failing process
// reported for failed's reader ([Synced.Repairer]) is no part they answer, and
// is CARRIED to the repaired copy's reader (awaitInPlace). [RebuildCorrupt]
// and [RebuildMissing] take the four steps above, which consume it, their
// rebuild joined if queued or running, or refused ([ErrRebuildRefused]). A
// failed since rebuilt or purged is answered by a linearizable [Cache.Sync];
// one a SWAP replaced, from the copy serving now as it stands. failed stays
// the caller's to release; ctx bounds the caller's wait only.
func (c *Cache) Rebuild(ctx context.Context, failed Synced, f Failure) (Synced, error) {
	reason, drops := f.Reason, f.Drops
	if !slices.Contains(rebuildReasons, reason) {
		return Synced{}, fmt.Errorf("%w: a rebuild's reason is what the host's git process over the cache "+
			"failed with, one of %s, and %q is not", ErrInvalid, words.Or(rebuildReasons), reason)
	}
	switch {
	case reason == RebuildDerived && (drops == 0 || drops&^c.image.parts(c.image.all()) != 0):
		return Synced{}, fmt.Errorf("%w: a derived repair drops the files of a part a declaration of them "+
			"names (%v, gitproc.Damage.Drops), not %v", ErrInvalid, c.image.parts(c.image.all()), drops)
	case reason != RebuildDerived && drops != 0:
		return Synced{}, fmt.Errorf("%w: only a derived repair drops files, and %q is not one", ErrInvalid, reason)
	case len(f.Packs) > 0 && reason != RebuildCorrupt && reason != RebuildMissing:
		return Synced{}, fmt.Errorf("%w: only a pack's or an object's repair names packs (Failure.Packs), "+
			"and %q is not one", ErrInvalid, reason)
	}
	for _, cs := range f.Packs {
		if !packChecksum().MatchString(cs) {
			return Synced{}, fmt.Errorf("%w: %q is no pack checksum (forty lowercase hex digits)", ErrInvalid, cs)
		}
	}
	if err := c.handedOut(failed, "a rebuild"); err != nil {
		return Synced{}, err
	}
	if c.isStopped() {
		return Synced{}, ErrStopped
	}
	failed.entry.mu.Lock()
	holding := c.image.holding(failed.inc)
	failed.entry.mu.Unlock()
	if reason == RebuildDerived && c.image.reported(drops)&holding&c.image.dropping(EventReported) == 0 {
		// DAMAGE NO DROP REPAIRS: every declaration it names of those the copy
		// holds files of keeps its files on a report (OnReported), the installed
		// chain's among them, so it is repaired as a pack's damage is
		// (damagedRound): a whole read dropping nothing would answer the request
		// from the copy that failed it.
		f = Failure{Reason: RebuildCorrupt, Began: f.Began}
		reason = f.Reason
	}
	if reason == RebuildCorrupt || reason == RebuildMissing {
		return c.repairRequest(ctx, failed, f)
	}
	return c.rebuild(ctx, failed, f, true, nil)
}

// packChecksum is a pack's checksum as its file names spell it, compiled at
// its first use as every file-name pattern of this package is (lazyPattern).
var packChecksum = lazyPattern(`^[0-9a-f]{40}$`)

// repairRequest is Rebuild for a request's pack or object damage: steps 1
// and 2 above; Cache.run hands a round that repaired nothing to escalate
// (steps 3 and 4).
func (c *Cache) repairRequest(ctx context.Context, failed Synced, f Failure) (Synced, error) {
	e, inc := failed.entry, failed.inc
	linearizable := statelog.Freshness{Level: statelog.ReadLinearizable}
	e.mu.Lock()
	if swappedOut(e, inc) {
		// SWAPPED UNDER THE REQUEST: counted stale, as the guard below counts
		// a failure a repair may have ended (swappedAnswer).
		c.countPart(partOf(f.Reason), reportStale)
		return c.swappedAnswer(ctx, e)
	}
	if e.dead || e.current != inc {
		// Somebody rebuilt it, or a purge removed it, since this request
		// pinned it: the current incarnation answers, or the purge does.
		e.mu.Unlock()
		return c.Sync(ctx, e.id, linearizable)
	}
	w := newWaiter(linearizable)
	w.escalate, w.failedOn, w.began, w.request = f.Reason, inc, f.Began, true
	if predates(inc, f.Began) {
		// THE GUARD: the request may have failed on what this process
		// repaired since its process began.
		c.countPart(partOf(f.Reason), reportStale)
		w.escalate = ""
		c.asItStands(e, inc, w)
		e.mu.Unlock()
		return c.awaitRequest(ctx, e, w, failed)
	}
	if len(inc.repair.fetchable(f.Packs, c.now())) == 0 && c.forcedLately(inc) {
		// STEP 2 IS BOUNDED (forcedLately): another forced round would hash the
		// same files the same, and each reads the rows whole and holds every
		// reader while it hashes about a quarter of the copy's pack bytes (4–5 s
		// warm, some 39 s cold for the largest repository the defaults admit), for
		// every request of a client polling a broken repository. A hinted pack
		// whose download is held off is no pack here: the round could not fetch it.
		e.mu.Unlock()
		go c.escalate(e, inc, []*waiter{w})
		return c.awaitRequest(ctx, e, w, failed)
	}
	c.markWhole(e, inc, Failure{Reason: RebuildStored}, dropRequest)
	inc.repair.forces++
	c.reinstallHinted(inc, f.Packs)
	c.join(e, w)
	e.mu.Unlock()
	return c.awaitRequest(ctx, e, w, failed)
}

// swappedAnswer answers a request whose copy a SWAP replaced under it
// (swappedOut) from the copy serving its files now, AS IT STANDS — Kind
// SyncNone, which the host reads as no repair — once no round holds it
// (asItStands). The failing run read the old directory: damage in a file
// that left is gone, and damage in a shared file fails again, begun after
// the swap, for that copy's own repair. Answering by a sync instead would
// spend the request's repair (host's maxRepairs) on a copy no repair
// touched, on a repository compacted every few minutes. What failed's reader
// holds stays on the old copy for its release to hand on (handOn). e.mu is
// held, and let go.
func (c *Cache) swappedAnswer(ctx context.Context, e *entry) (Synced, error) {
	w := newWaiter(statelog.Freshness{Level: statelog.ReadLinearizable})
	w.request = true
	c.asItStands(e, e.current, w)
	e.mu.Unlock()
	return c.await(ctx, e, w)
}

// reinstallHinted asks inc's next full round to install again every pack of
// packs (Failure.Packs) at the identity its .pack has now, so a round that
// finds it already replaced fetches nothing; a pack the copy lacks the round
// installs anyway. e.mu is held.
func (c *Cache) reinstallHinted(inc *incarnation, packs []string) {
	for _, cs := range packs {
		rel := packFileRel(placeOf(inc, cs), cs, ".pack")
		id := lstatID(filepath.Join(inc.dir, rel))
		if !id.present {
			continue
		}
		if inc.repair.reinstall == nil {
			inc.repair.reinstall = map[string]checkedFile{}
		}
		inc.repair.reinstall[cs] = checkedFile{kind: filePack, rel: rel, id: id, trigger: triggerRequest,
			hinted: true}
	}
}

// awaitRequest is await for a request's repair of pack or object damage:
// once it has answered, whatever the answer, every report HELD for the
// request's reader (failed.Repairer) is that repair's and consumed
// (reports{outcome=request}), since its forced round, scrub and rebuild
// answer what they name. A request that LEFT first consumes nothing: its
// reports go to the ordinary rules with its pin's release (handOn).
func (c *Cache) awaitRequest(ctx context.Context, e *entry, w *waiter, failed Synced) (Synced, error) {
	s, err := c.await(ctx, e, w)
	e.mu.Lock()
	if !w.gone {
		c.consume(failed.inc, failed.Repairer)
	}
	e.mu.Unlock()
	return s, err
}

// awaitInPlace is await for a request's IN-PLACE repair of a shape or a
// derived file: that repair read no pack, so every report held for failed's
// reader is CARRIED to the reader it answers (carry, report.go). A failed
// answer or a caller that left carries nothing: failed's release hands its
// reports on.
func (c *Cache) awaitInPlace(ctx context.Context, e *entry, w *waiter, failed Synced) (Synced, error) {
	s, err := c.await(ctx, e, w)
	if err != nil {
		return s, err
	}
	e.mu.Lock()
	c.carry(failed, s)
	e.mu.Unlock()
	return s, nil
}

// forcedLately reports whether inc's last forced check ran within
// ScrubCooldown with no repair written since it ended, its round's own
// included (incarnation.forcedAt): its findings wait on the repair mark, and
// another would find the same. e.mu is held.
func (c *Cache) forcedLately(inc *incarnation) bool {
	return !inc.forcedAt.IsZero() && c.now().Sub(inc.forcedAt) < ScrubCooldown &&
		!inc.repairWrite.After(inc.forcedEnded)
}

// asItStands answers w, a request the guard says may have failed on what a
// repair since replaced, from inc as it stands (Kind SyncNone, which the host
// reads as no repair) once no round holds inc: a running round may be
// mid-repair (derived files dropped, the pack not yet renamed over), and a
// copy pinned half way fails the retry as it failed the request. So w joins
// the round queued behind it, or is answered at once when none runs. e.mu is
// held.
func (c *Cache) asItStands(e *entry, inc *incarnation, w *waiter) {
	if e.running == nil {
		c.answer(e, w, inc, SyncNone, nil)
		return
	}
	w.asIs = true
	c.queue(e, w)
}

// requested is what in-place repairs of pack or object damage asked inc's
// next full round to do (repairMark.forces and reinstall), or nothing while a
// check of the repository is admitted, whose release asks the round again.
// Copied, not taken: the check spends the forces (recordCheck) and the
// install takes off the packs (settleRequested), so a round that fails
// before either leaves them for the next. e.mu is not held.
func (c *Cache) requested(e *entry, inc *incarnation) (int, []checkedFile) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.verifying > 0 {
		return 0, nil
	}
	reinstall := make([]checkedFile, 0, len(inc.repair.reinstall))
	for _, f := range inc.repair.reinstall {
		reinstall = append(reinstall, f)
	}
	slices.SortFunc(reinstall, func(a, b checkedFile) int { return cmp.Compare(a.rel, b.rel) })
	return inc.repair.forces, reinstall
}

// settleRequested takes off inc's repair mark the packs a full round's install
// acted on, each only at the identity it was asked at (a repair asked since
// keeps its own); a pack in unrepaired, its download failed or held off,
// stays asked. The forces are the check's to spend (recordCheck).
func (c *Cache) settleRequested(e *entry, inc *incarnation, reinstall []checkedFile,
	unrepaired map[string]bool) {

	e.mu.Lock()
	defer e.mu.Unlock()
	for _, f := range reinstall {
		cs, _ := packOfRel(f.rel)
		if unrepaired[cs] {
			continue
		}
		if have, ok := inc.repair.reinstall[cs]; ok && have.id == f.id {
			delete(inc.repair.reinstall, cs)
			delete(inc.repair.fetchFailed, cs)
		}
	}
}

// escalate is steps 3 and 4 for ws, callers of an in-place repair of inc's
// pack or object damage that did not end it (Cache.run, damagedRound,
// repairDamaged): the scrub, run for them now (scrubFor), whose findings the
// round it asks repairs; with none, the whole rebuild seeded with what it
// found whole, asked as Rebuild asks one (askRebuild), unless a pack it would
// download is held off (repairMark.heldOffFor), whose refusal answers them.
// An incarnation replaced meanwhile answers them by an ordinary round. THE
// GUARD IS ASKED AGAIN before the scrub and before the rebuild
// (staleAnswered): the copy is written into meanwhile (a push the in-place
// round installs, the scrubber's own repair of the same damage beside this
// scrub), and a caller whose failure began before such a write may no longer
// face damage.
func (c *Cache) escalate(e *entry, inc *incarnation, ws []*waiter) {
	if !c.enter() {
		e.mu.Lock()
		for _, w := range ws {
			c.answer(e, w, nil, "", ErrStopped)
		}
		e.mu.Unlock()
		return
	}
	defer c.leave()
	e.mu.Lock()
	if !e.dead && e.current == inc {
		ws = c.staleAnswered(e, inc, ws)
	}
	e.mu.Unlock()
	if len(ws) == 0 {
		return
	}
	out, err := c.scrubFor(e, inc, latestBegan(ws))
	now := c.now()
	e.mu.Lock()
	switch {
	case e.dead:
		for _, w := range ws {
			c.answer(e, w, nil, "", fmt.Errorf("cache: %s was purged while its repair ran: %w", e.id,
				gitlet.ErrRepoNotFound))
		}
	case err != nil:
		for _, w := range ws {
			c.answer(e, w, nil, "", err)
		}
	case e.current != inc || len(out.damaged) > 0:
		// Replaced meanwhile, or the scrub's findings are on the repair mark with a
		// round asked: the next round answers. If its own git meets the damage again
		// on the copy the scrub read, the caller goes on to the whole rebuild,
		// seeded with what the scrub found whole (damagedRound, waiter.scrubbedOn).
		for _, w := range ws {
			w.escalate = ""
			if e.current == inc {
				w.scrubbedOn, w.whole = inc, out.whole
			}
			c.queue(e, w)
		}
		c.start(e)
	default:
		if ws = c.staleAnswered(e, inc, ws); len(ws) == 0 {
			break
		}
		if herr := inc.repair.heldOffFor(now, out.whole); herr != nil {
			// A rebuild would ask the store for a pack whose download it
			// has just refused (heldOffFor): the callers are answered with
			// that refusal until the pack's backoff passes.
			for _, w := range ws {
				w.escalate = ""
				c.answer(e, w, nil, "", herr)
			}
			break
		}
		reason := ws[0].escalate
		r, rule := c.askRebuild(e, inc, reason, true, out.whole, now)
		if rule == "" {
			for _, w := range ws {
				w.escalate, w.rebuild = "", true
			}
			r.waiters = append(r.waiters, ws...)
			break
		}
		f := c.refusal(e, inc.repo.Name, reason, rule, now)
		e.mu.Unlock()
		c.report(f)
		e.mu.Lock()
		for _, w := range ws {
			c.answer(e, w, nil, "", f.err())
		}
	}
	e.mu.Unlock()
}

// predates reports whether a failure whose process began at began (a
// time.Now() reading, zero when unknown) began before this process last
// wrote a repair into inc (incarnation.repairWrite), and so may have been of
// what that repair replaced; e.mu is held.
func predates(inc *incarnation, began time.Time) bool {
	return !began.IsZero() && inc.repairWrite.After(began)
}

// latestBegan is when the last of ws's failing processes began, or zero when
// any of them says nothing of it: a scrub that began after it read every
// byte any of them failed on (scrubFor).
func latestBegan(ws []*waiter) time.Time {
	var latest time.Time
	for _, w := range ws {
		if w.began.IsZero() {
			return time.Time{}
		}
		if w.began.After(latest) {
			latest = w.began
		}
	}
	return latest
}

// partOf is the report part a pack's or an object's repair reason counts
// under, a request's or a round's own.
func partOf(reason RebuildReason) reportPart {
	if reason == RebuildMissing {
		return reportObject
	}
	return reportPack
}

// staleAnswered answers as the guard does (standOrSync, counted
// reports{outcome=stale}) every caller in ws whose failure predates a repair
// written into inc, the current incarnation, and returns the others; e.mu is
// held.
func (c *Cache) staleAnswered(e *entry, inc *incarnation, ws []*waiter) []*waiter {
	rest := ws[:0]
	for _, w := range ws {
		if !predates(inc, w.began) {
			rest = append(rest, w)
			continue
		}
		c.countPart(partOf(w.escalate), reportStale)
		w.escalate = ""
		c.standOrSync(e, inc, w)
	}
	return rest
}

// standOrSync answers w, whose failure predates a repair written into inc,
// as the guard does; e.mu is held. A REQUEST is answered from inc as it
// stands (asItStands), and its git's retry tells. A SYNC is answered only by
// a round that converged, so it is queued for a round of inc, which meets
// the damage again and repairs it from step 1, or answers it.
func (c *Cache) standOrSync(e *entry, inc *incarnation, w *waiter) {
	if w.request {
		c.asItStands(e, inc, w)
		return
	}
	c.queue(e, w)
	c.start(e)
}

// rebuild is Rebuild's body for a reason already judged: refuse says whether
// rebuildRefusal applies. Verify passes false (an operator asked, and the
// check has just named the damage) yet still joins a queued or running
// rebuild and counts as an attempt of the line. whole is the packs the caller
// has just held to their rows (seedRebuild): Verify's, nil for a git process.
func (c *Cache) rebuild(ctx context.Context, failed Synced, f Failure, refuse bool, whole []string) (Synced,
	error) {

	reason := f.Reason
	e := failed.entry
	now := c.now()
	e.mu.Lock()
	linearizable := statelog.Freshness{Level: statelog.ReadLinearizable}
	if swappedOut(e, failed.inc) {
		if reason != RebuildVerify {
			return c.swappedAnswer(ctx, e)
		}
		// AN OPERATOR'S REPAIR OF A COPY A SWAP REPLACED since its check: the
		// copy serving holds every file it kept as the same inode, the damaged
		// ones among them, so it is the copy rebuilt, seeded with what the check
		// found whole.
		failed.inc = e.current
	}
	if e.dead || e.current != failed.inc {
		// Somebody rebuilt it, or a purge removed it, since this request
		// pinned it: the current incarnation answers, or the purge does.
		e.mu.Unlock()
		return c.Sync(ctx, e.id, linearizable)
	}
	if inPlace(reason) {
		// IN PLACE: a linearizable caller joins only the round queued behind the
		// running one, which starts after the mark and so reads the incarnation
		// whole, dropping damaged derived files (apply), with no download.
		c.markWhole(e, failed.inc, f, dropRequest)
		w := newWaiter(linearizable)
		c.join(e, w)
		e.mu.Unlock()
		return c.awaitInPlace(ctx, e, w, failed)
	}
	r, rule := c.askRebuild(e, failed.inc, reason, refuse, whole, now)
	if rule != "" {
		f := c.refusal(e, failed.Repo.Name, reason, rule, now)
		e.mu.Unlock()
		c.report(f)
		return Synced{}, f.err()
	}
	w := newWaiter(linearizable)
	w.rebuild = true
	r.waiters = append(r.waiters, w)
	e.mu.Unlock()
	return c.await(ctx, e, w)
}

// askRebuild asks a rebuild of inc, e's current incarnation, for reason now;
// e.mu is held. It answers the rebuild round to wait on (the one queued or
// running for inc, or a new one), or, when refuse is set, the rule refusing
// it (rebuildRefusal). Every ask, refused or not, restarts the line's quiet
// (RebuildQuiet), since a line ends when the asking stops; only an attempt
// starts the window (lastRebuild). A SHAPE arrives only past the sweep of the
// round that met it (Cache.run).
func (c *Cache) askRebuild(e *entry, inc *incarnation, reason RebuildReason, refuse bool, whole []string,
	now time.Time) (*round, refusalRule) {

	quiet := !e.lastAsked.IsZero() && now.Sub(e.lastAsked) >= RebuildQuiet
	e.lastAsked = now
	if r := rebuilding(e, inc); r != nil {
		return r, ""
	}
	if quiet {
		inc.rebuilds, inc.unmoved = 0, false
	}
	if reason == RebuildShape {
		// A SHAPE REFUSAL PROVES SOMETHING WROTE INTO THE INCARNATION: no round
		// or rebuild makes a directory the pool refuses, so the unchanged rule is
		// false of it. The cooldown still holds: a rebuild downloads the whole
		// repository, not to be repeated on every request for a directory
		// something keeps editing.
		inc.unmoved = false
	}
	if rule := rebuildRefusal(e, inc, now); refuse && rule != "" {
		return nil, rule
	}
	e.lastRebuild = now
	return c.queueRebuild(e, inc, reason, whole), ""
}

// refusalRule is a rule a rebuild is refused by: the values
// crewlet.code.cache.rebuild.refusals counts under its rule attribute, and
// code_cache_rebuild_refused names. A type of its own so the reference text
// is held to every one declared (the cache's reasons test).
type refusalRule string

const (
	// refusedUnchanged: the incarnation holds exactly what a rebuild that
	// did not end the failure read, less than RebuildBackoffCap ago.
	refusedUnchanged refusalRule = "unchanged"
	// refusedCooldown: the last attempt began less than rebuildWindow ago.
	refusedCooldown refusalRule = "cooldown"
)

// rebuildRefusal is the rule refusing a rebuild of inc now, or ""; e.mu is
// held, and a line gone quiet is already ended (askRebuild). A rebuild downloads the whole repository on every node a failing
// request reaches, so it runs only where it can still help:
//
//   - the first attempt at the damage (inc.rebuilds zero) runs at once;
//   - one holding EXACTLY what the last attempt read (inc.unmoved) waits
//     RebuildBackoffCap: it would make the same directory, the damage being
//     the store's or the rows' (`crewlet gitlet verify`). Not for ever: the
//     store can come to hold what it lacked, and any write into it ends this;
//   - otherwise rebuildWindow(inc.rebuilds): nothing a push brings repairs a
//     stored pack or a missing object, so each failed attempt doubles the wait.
func rebuildRefusal(e *entry, inc *incarnation, now time.Time) refusalRule {
	since := now.Sub(e.lastRebuild)
	switch {
	case inc.rebuilds == 0:
		return ""
	case inc.unmoved && since < RebuildBackoffCap:
		return refusedUnchanged
	case since < rebuildWindow(inc.rebuilds):
		return refusedCooldown
	}
	return ""
}

// rebuildWindow is how long after the last attempt the next is refused, for
// an incarnation n attempts in a row stand behind: RebuildCooldown after the
// first, doubled for every attempt after it, never past RebuildBackoffCap.
func rebuildWindow(n int) time.Duration {
	w := RebuildCooldown
	for i := 1; i < n && w < RebuildBackoffCap; i++ {
		w *= 2
	}
	return min(w, RebuildBackoffCap)
}

// refused is a rebuild refused by its rule, decided under e.mu (refusal) and
// reported once it is let go (report): what the refusal is counted under,
// whether this one is logged, and what its asker is answered (err).
type refused struct {
	repo, name string
	rule       refusalRule
	reason     RebuildReason
	since      time.Duration
	logIt      bool
}

// refusal is a rebuild of e's repository, named name, refused by rule now;
// e.mu is held. Logged at most once a RebuildCooldown a repository: every
// failing request asks again, and a line a request would bury the one that
// says what to do.
func (c *Cache) refusal(e *entry, name string, reason RebuildReason, rule refusalRule, now time.Time) refused {
	f := refused{repo: e.id, name: name, rule: rule, reason: reason, since: now.Sub(e.lastRebuild).Round(time.Second)}
	if f.logIt = e.refusedLogged.IsZero() || now.Sub(e.refusedLogged) >= RebuildCooldown; f.logIt {
		e.refusedLogged = now
	}
	return f
}

// report counts a refusal every time, and logs the ones refusal chose; e.mu
// is not held.
func (c *Cache) report(f refused) {
	if c.metrics != nil {
		c.metrics.Add(metrics.CodeCacheRebuildRefusals, 1,
			metrics.Attrs{"reason": string(f.reason), "rule": string(f.rule)})
	}
	if f.logIt {
		c.log.Warn("code_cache_rebuild_refused", "repo", f.repo, "name", f.name, "reason", string(f.reason),
			"rule", string(f.rule), "since", f.since)
	}
}

// err is what the asker of a refused rebuild is answered: ErrRebuildRefused,
// naming the repository, the rule's reason and `crewlet gitlet verify`.
func (f refused) err() error {
	if f.rule == refusedUnchanged {
		return fmt.Errorf("%w: %s (%s) holds exactly what a rebuild %v ago read, and that rebuild did not end the "+
			"failure: another reads the same rows and objects and makes the same directory — `crewlet gitlet verify %s` "+
			"names what is damaged", ErrRebuildRefused, f.name, f.repo, f.since, f.name)
	}
	return fmt.Errorf("%w: %s (%s) was rebuilt %v ago, and a rebuild that did not end the failure will not end it a "+
		"second time — `crewlet gitlet verify %s` names what is damaged", ErrRebuildRefused, f.name, f.repo, f.since,
		f.name)
}

// rebuilding is the rebuild of inc queued or running on e, or nil; e.mu is
// held.
func rebuilding(e *entry, inc *incarnation) *round {
	for _, r := range []*round{e.running, e.next} {
		if r != nil && r.rebuild != "" && r.of == inc {
			return r
		}
	}
	return nil
}

// queueRebuild makes the round queued on e a rebuild of inc, seeded with
// whole (seedRebuild), starting it when nothing runs; e.mu is held. A queued
// ordinary round is UPGRADED, not left behind: a whole read at the log's head
// answers whatever its callers asked, and should the rebuild fail each is
// queued again for an ordinary round (Cache.run), never handed its failure.
func (c *Cache) queueRebuild(e *entry, inc *incarnation, reason RebuildReason, whole []string) *round {
	linearizable := statelog.Freshness{Level: statelog.ReadLinearizable}
	if e.next == nil {
		e.next = &round{fresh: linearizable}
	} else {
		e.next.fresh = merge(e.next.fresh, linearizable)
	}
	e.next.rebuild, e.next.of, e.next.whole = reason, inc, whole
	r := e.next
	c.start(e)
	return r
}

// seedRebuild links into inc, a rebuild's new incarnation, every pack of
// replaced in whole: held WHOLE against its row by Verify, or against its
// trailers by the failing caller's scrub (step 3). On one filesystem with
// read-only packs the link IS the bytes hashed, and the rebuild's round holds
// each to its row as it would a download. A scrub's REUSED verdict may be an
// hour old: a pack damaged in place since is linked as it is, and the new
// copy's scrub, due at the next report, fetches it; hashing again here would
// cost what the reuse saves. A pack gone is passed over, a failed link left
// to the download; directories are synced before any ref names the packs.
// Of derived files only SeedLink's are linked (seedIndex) and the memo moves
// at promote (carryMemo); the rest are rewritten, since the replaced copy's
// may be what failed. The installed chain is seeded by seedLayers.
func (c *Cache) seedRebuild(ctx context.Context, e *entry, replaced, inc *incarnation, whole []string) error {
	c.seedIndex(e, replaced, inc)
	c.seedCodeIndex(e, replaced, inc)
	// Each pack where the replaced copy kept it (a dropping pack into the new
	// dropping view); the rebuild's own round holds each to where its rows
	// place it and downloads one they moved.
	e.mu.Lock()
	places := make(map[string]packPlace, len(whole))
	for _, cs := range whole {
		places[cs] = placeOf(replaced, cs)
	}
	e.mu.Unlock()
	seeded := 0
	linked := map[string]bool{}
	synced := map[string]bool{}
	for _, cs := range whole {
		place := places[cs]
		into := filepath.Join(inc.dir, place.rel())
		if place == placeDropping && !synced[into] {
			if err := c.ensureView(inc.dir); err != nil {
				return err
			}
		}
		ok, err := linkBundle(nil, filepath.Join(replaced.dir, place.rel(), "pack-"+cs), into, cs, nil)
		switch {
		case ok:
			// Found whole, bundle and all: vouched in the new incarnation as in
			// the old.
			c.vouchPack(e, inc, place, cs)
			if place != placeDropping {
				// What a layer may name (seedLayers): no layer names a
				// dropping pack, publication being refused while one is.
				linked[cs] = true
			}
			synced[into] = true
			seeded++
		case err != nil && !errors.Is(err, fs.ErrNotExist):
			c.log.Warn("code_cache_rebuild_seed_failed", "repo", e.id, "pack", "pack-"+cs, "error", err)
		}
	}
	if seeded == 0 {
		return nil
	}
	c.log.Info("code_cache_rebuild_seeded", "repo", e.id, "incarnation", inc.n, "packs", seeded,
		"whole", len(whole))
	for _, dir := range slices.Sorted(maps.Keys(synced)) {
		if err := syncDir(dir); err != nil {
			return err
		}
	}
	return c.seedLayers(ctx, e, replaced, inc, linked)
}

// seedLayers seeds replaced's installed chain into inc, root first, while each
// layer names only packs linked (a layer is valid only over its packs) and its
// linked files pass gitlet.CheckLayer against its row: placed with both chain
// files, vouched and recorded installed, so the rebuild's round fetches only
// the rest. The first layer that fails ends the seed, since nothing above it
// is valid; a hash with no verdict (no read slot, ctx ended) ends it unlogged.
// That one read of each layer's files a rare rebuild can afford.
func (c *Cache) seedLayers(ctx context.Context, e *entry, replaced, inc *incarnation, linked map[string]bool) error {
	// As the declarations say (SeedLink): both chains are one install, each
	// layer's commit-graph layer placed with it, so a rebuild seeds the two
	// together or neither.
	for _, name := range []DerivedName{DerivedLayers, DerivedGraphLayers} {
		if d := c.image.decl(name); d == nil || d.Seed != SeedLink {
			return nil
		}
	}
	e.mu.Lock()
	installed, held := slices.Clone(replaced.installed), heldPacks(replaced)
	e.mu.Unlock()
	var seeded []gitlet.LayerRow
	var rels []string
	p := c.pieces(c.git.HoldRead).layer()
	for i, row := range installed {
		if slices.ContainsFunc(row.Layer.Packs, func(cs string) bool { return !linked[cs] }) {
			break
		}
		got, err := c.linkLayerIn(row, replaced.dir, inc.dir)
		if err == nil {
			err = checkLayerAt(ctx, inc.dir, row, baseOf(installed, i), p)
		}
		if err != nil {
			c.trashRels(inc.dir, got)
			if !errors.Is(err, fs.ErrNotExist) && !noVerdict(ctx, err) {
				c.log.Warn("code_cache_rebuild_seed_failed", "repo", e.id, "layer", row.Layer.LayerID,
					"error", err)
			}
			break
		}
		seeded = append(seeded, row)
		rels = append(rels, got...)
	}
	if len(seeded) == 0 {
		return nil
	}
	if err := c.writeChains(inc.dir, seeded, "", syncDir); err != nil {
		c.trashRels(inc.dir, rels)
		return err
	}
	rels = append(rels, layersChainRel)
	if _, graph := chainIDs(seeded); len(graph) > 0 {
		rels = append(rels, graphChainRel)
	}
	c.vouchFiles(e, inc, rels...)
	e.mu.Lock()
	c.recordInstalled(inc, seeded, held)
	e.mu.Unlock()
	c.countLayers(layerKept, len(seeded))
	c.log.Info("code_cache_rebuild_seeded", "repo", e.id, "incarnation", inc.n, "layers", len(seeded),
		"installed", len(installed))
	return nil
}

// promote makes inc, a rebuild's incarnation, current in place of old, which
// is retired (trashed with its last reader, so a git process running there
// finishes on the files it opened), and records the attempt: inc stands one
// further along old's line, holding exactly what it read. The memo moves
// first, in one rename (carryMemo), so old serves it until here.
func (c *Cache) promote(e *entry, old, inc *incarnation, reason RebuildReason, took time.Duration) {
	if old != nil {
		c.carryMemo(e, old, inc)
	}
	e.mu.Lock()
	var gone string
	if old != nil {
		inc.rebuilds = old.rebuilds + 1
		if !old.retired {
			gone = c.retire(e, old)
		}
	} else {
		inc.rebuilds = 1
	}
	inc.unmoved = true
	if e.current == nil {
		c.heldRepos.Add(1)
	}
	e.current = inc
	c.noteChained(e)
	// The backfill's work is the new copy's now (schedule.go).
	c.backfillDirty(e)
	e.mu.Unlock()
	c.trash(gone)
	c.rebuilt(reason)
	c.log.Warn("code_cache_rebuilt", "repo", e.id, "reason", string(reason), "incarnation", inc.n, "took", took)
	c.setGauges()
}

// rebuilt counts one repair of a copy for reason: crewlet.code.cache.rebuilds
// and the status's windows ([Cache.Status]). THE ONE PLACE A REPAIR IS
// COUNTED (a whole rebuild at promote, an in-place round by what it did), so
// the counter and the windows never disagree, and a node with no recorder
// still counts what its status reports.
func (c *Cache) rebuilt(reason RebuildReason) {
	c.churned(churned{rebuilds: 1})
	if c.metrics != nil {
		c.metrics.Add(metrics.CodeCacheRebuilds, 1, metrics.Attrs{"reason": string(reason)})
	}
}

// failedRebuild records a rebuild of inc that failed with err; e.mu is held.
// inc stays current, one attempt further along its line, and unmoved when the
// failure was the rows' or the stored bytes' own answer, which asking again
// reads the same; one that may not recur (store or estate unreachable, the
// cache stopping) can be retried once the window passes.
func failedRebuild(inc *incarnation, err error) {
	inc.rebuilds++
	var missing *ErrPackMissing
	var corrupt *ErrCorruptPack
	if errors.As(err, &missing) || errors.As(err, &corrupt) || errors.Is(err, ErrBadSnapshot) {
		inc.unmoved = true
	}
}

// abandon takes a rebuild's incarnation that failed out of the cache: nobody
// was ever handed it, so it goes into trash/ at once — once the entry is let
// go.
func (c *Cache) abandon(e *entry, inc *incarnation) {
	e.mu.Lock()
	inc.retired = true
	gone := c.discard(inc)
	e.mu.Unlock()
	c.trash(gone)
}
