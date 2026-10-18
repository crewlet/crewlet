package cache

import (
	"errors"
	"path/filepath"
	"time"

	"github.com/crewlet/crewlet/internal/code/gitproc"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/statelog/metrics"
)

// WHAT ANY GIT PROCESS REPORTS DAMAGED IS REPAIRED, WHETHER OR NOT IT FAILED.
// git reads around most damage to a derived file — it warns, and answers from
// the packs' own indexes, walking by date — and a caller that got its answer
// never looks at the warning: a clone over a truncated multi-pack-index
// succeeds, slower, every time, and the maintainer's own write over an index
// whose checksum is wrong says so and exits 0 having kept what it could read
// of it. So the pool hands every run to the cache's OBSERVER
// (gitproc.Pool.Observe, installed by Open), which records the derived parts a
// run over one of the cache's incarnations named on that incarnation's REPAIR
// MARK and queues a round to repair them — with no request waiting, and
// without taking the incarnation out of service: it keeps answering (atOnce)
// while git reads around the damage, where markWhole would hold every request
// for the round.
//
//   - ROUTED BY THE PROCESS'S DIRECTORY (incarnationAt): a process run in an
//     incarnation reports about it; one run in a push's scratch or an
//     advertisement view reports about its ALTERNATE only its derived parts —
//     a pack or object part there may be the client's pack (index-pack's
//     `early EOF`), so it is counted `unrouted` and left to the next process
//     over the incarnation itself. A retired incarnation's successor was read
//     whole and checked: a report about it is dropped.
//   - REPAIRED BY THE NEXT FULL ROUND (repairRound): a linearizable one,
//     queued with no waiter when none is queued — linearizable, because a
//     full round's sweep removes every pack its snapshot does not name, and an
//     adoption installs a pack past the incarnation's own position. Reports
//     coalesce on the mark while one is queued.
//   - A DROP IS STILL GUARDED (dropDamaged): a part is not dropped when this
//     process wrote its current file AFTER the reporting run began
//     (gitproc.Result.Began against incarnation.written, on the monotonic
//     clock) — that run may have read the file the write replaced, and without
//     the guard every long clone that began over a damaged bitmap would drop
//     each rewrite in turn. Counted `stale`.
//   - NOTHING IS REPAIRED UNDER A CHECK. While a check of the repository is
//     admitted (entry.verifying) a report is recorded but no round takes it
//     (`deferred`): a check holds rounds off only for its listing, and a
//     repair between its listing and its hashing would drop a file it
//     admitted. The check's release queues the round.
//   - A PART DAMAGED AGAIN WAITS ITS TURN (D13): a derived part dropped for
//     damage again within RebuildQuiet of its last drop for damage — however
//     many writes of it came between, the repair's and the maintainer's
//     ordinary ones — is dropped but not made due at once: the index waits
//     for the ordinary rules, the graph is graphFailed. Logged
//     code_cache_derived_damaged_again once an incarnation and counted
//     `again`, so a disk that keeps damaging, or a git that rejects its own
//     write, costs one rewrite at once a RebuildQuiet and the ordinary
//     schedule's after it, never a loop. (Cleared by the maintainer's next
//     ordinary write, the mark let every other damage be written again at
//     once: one rewrite at once and one ordinary, alternating for ever.)
//   - A STORED REPORT, AN OBJECT REPORT, A HINT OR A CRASH MAKES THE SCRUB DUE
//     AT ONCE (scrub.go), and drops nothing: a stored file is never dropped on
//     a report. `hash mismatch` names no file — the pack, its index or the
//     multi-pack-index placed the wrong bytes — a hint names a pack in words a
//     request may reach, and a crash names nothing at all (a pack-objects
//     dying of a damaged reverse index or bitmap says only that it died), so
//     what decides is the bytes: the scrub hashes every pack and index file of
//     the copy, vouched or not, and what fails its checksum is reinstalled in
//     place or dropped by the repair round. A crash in a scratch or a view is
//     its alternate's like a derived part — neither ever holds a reverse index
//     or a bitmap of its own — and its pack, object and hint reports may be the
//     client's pack: `unrouted`. BOUNDED by ScrubCooldown: within an hour of a
//     scrub that found nothing, a report makes nothing due (`cooldown`),
//     whatever was written meanwhile — a push resetting it would leave a
//     request that can provoke a crash beside a busy repository bounded by
//     nothing — and within an hour of one whose findings still wait for
//     their repair, the repair is the answer (`covered`).

// reportPart is what a report names: the `part` attribute of
// crewlet.code.cache.reports.
type reportPart string

const (
	reportGraph  reportPart = "commit-graph"
	reportMidx   reportPart = "multi-pack-index"
	reportChunk  reportPart = "chunk"
	reportPack   reportPart = "pack"
	reportObject reportPart = "object"
	// reportCrash: a process of the run's tree died of a signal the pool
	// did not send (gitproc.Result.Crashed).
	reportCrash reportPart = "crash"
	// reportHint: a pack a run named in words alone (gitproc.Hint).
	reportHint reportPart = "hint"
)

// reportOutcome is what became of a report: the `outcome` attribute of
// crewlet.code.cache.reports.
type reportOutcome string

const (
	// reportQueued: a repair asked — a derived part recorded on the
	// incarnation's repair mark with a round queued to drop it; any other
	// part, a hint or a crash, the incarnation's scrub made due, or answered
	// by the one already due or running (scrub.go).
	reportQueued reportOutcome = "queued"
	// reportDeferred: recorded while a check of the repository is
	// admitted; the check's release queues the round.
	reportDeferred reportOutcome = "deferred"
	// reportStale: not dropped — this process wrote the part's current file
	// after the reporting run began.
	reportStale reportOutcome = "stale"
	// reportUnrouted: a pack or object part, or a hint, from a process in a
	// scratch or a view, which may be the client's pack; nothing recorded.
	reportUnrouted reportOutcome = "unrouted"
	// reportAgain: a part dropped for damage again within RebuildQuiet of
	// its last drop for damage: dropped, not due at once.
	reportAgain reportOutcome = "again"
	// reportCooldown: a stored, object, hint or crash report within
	// ScrubCooldown of a scrub that found nothing: no scrub made due.
	reportCooldown reportOutcome = "cooldown"
	// reportCovered: a stored, object, hint or crash report within
	// ScrubCooldown of a scrub whose findings still wait for their repair:
	// the repair is the answer, and no scrub is made due.
	reportCovered reportOutcome = "covered"
)

// dropAsk is one part's drop as asked: the latest Began of a run that asked
// it (gitproc.Result.Began; zero when nothing says), and what asked
// (dropRequest, dropRound, dropReport).
type dropAsk struct {
	began time.Time
	by    string
}

// What asked a derived part's drop: the `trigger` of
// code_cache_derived_repaired.
const (
	dropRequest = "request" // a request's git failed on it (Cache.Rebuild)
	dropRound   = "round"   // a round's own git failed on it (damagedRound)
	dropReport  = "report"  // a run reported it, failing or not (observe)
	dropCheck   = "check"   // the round's check found it failing its trailer
	dropScrub   = "scrub"   // a scrub found it failing its trailer
)

// partDrops is the derived parts to drop, each with what asked it: one entry
// a part, PartMultiPackIndex or PartCommitGraph.
type partDrops map[gitproc.Part]dropAsk

// add records parts asked by a run begun at began, keeping each part's
// latest.
func (d *partDrops) add(parts gitproc.Part, began time.Time, by string) {
	for _, p := range []gitproc.Part{gitproc.PartMultiPackIndex, gitproc.PartCommitGraph} {
		if parts&p == 0 {
			continue
		}
		if *d == nil {
			*d = partDrops{}
		}
		if have, ok := (*d)[p]; !ok || have.began.Before(began) {
			(*d)[p] = dropAsk{began: began, by: by}
		}
	}
}

// merge adds every entry of o.
func (d *partDrops) merge(o partDrops) {
	for p, a := range o {
		d.add(p, a.began, a.by)
	}
}

// repairMark is what reports and scrubs asked an incarnation's next full
// round to do.
type repairMark struct {
	// drops is the derived parts reports named.
	drops partDrops
	// found is every file a scrub found failing its checksum, by its path
	// relative to the incarnation, at the identity it failed at: the round
	// reinstalls a pack, or drops a derived file, only while it still has
	// that identity (checked.still).
	found map[string]checkedFile
	// fetchFailed is the packs, by checksum, whose last download to install
	// them again failed (install.go), each held off until its backoff has
	// passed: RebuildCooldown after the first failure, twice as long after
	// each one after it, up to RebuildBackoffCap — the store being down, or
	// no longer holding the bundle, is not ended by asking it again every
	// round. Taken off with the pack's findings once it is installed.
	fetchFailed map[string]fetchBackoff
}

// fetchBackoff is a pack's download failures in a row, and when the next
// attempt may be made (Options.Now's clock).
type fetchBackoff struct {
	failures int
	until    time.Time
}

// pending reports whether the mark asks anything.
func (m repairMark) pending() bool { return len(m.drops) > 0 || len(m.found) > 0 }

// due reports whether the mark asks a full round anything it can do at now:
// a drop, or a finding that is a derived file's or a pack's not held off. A
// mark whose every finding is a pack held off asks nothing, so the copy's
// rounds stay deltas and keep answering until the backoff passes.
func (m repairMark) due(now time.Time) bool {
	if len(m.drops) > 0 {
		return true
	}
	held := m.heldOff(now)
	for _, f := range m.found {
		if cs, ok := packOfRel(f.rel); !ok || !held[cs] {
			return true
		}
	}
	return false
}

// heldOff is the packs whose download is held off at now, by checksum.
func (m repairMark) heldOff(now time.Time) map[string]bool {
	held := map[string]bool{}
	for cs, b := range m.fetchFailed {
		if now.Before(b.until) {
			held[cs] = true
		}
	}
	return held
}

// fetchFailedAt records that a download of pack cs to install it again failed
// at now, and holds the next one off.
func (m *repairMark) fetchFailedAt(cs string, now time.Time) {
	if m.fetchFailed == nil {
		m.fetchFailed = map[string]fetchBackoff{}
	}
	b := m.fetchFailed[cs]
	b.failures++
	b.until = now.Add(rebuildWindow(b.failures))
	m.fetchFailed[cs] = b
}

// packOfRel is the checksum of the pack whose file rel is, or false.
func packOfRel(rel string) (string, bool) {
	m := packFileName.FindStringSubmatch(filepath.Base(rel))
	if m == nil {
		return "", false
	}
	return m[1], true
}

// observe is the pool's observer (gitproc.Pool.Observe): called after every
// git run with where it ran and how it ended, on the run's own goroutine. It
// reads the parts the run's damage names, its hints and whether it crashed,
// finds the incarnation they are about by the process's directory, records a
// derived part on its repair mark and queues a round, and makes its scrub due
// for the rest — under the entry's mu, never its write lock, and doing no I/O,
// so it never holds its run.
func (c *Cache) observe(ran gitproc.Ran) {
	if c.isStopped() {
		return
	}
	var dmg *gitproc.Damage
	var derived, stored gitproc.Part
	if errors.As(ran.Result.Damage, &dmg) {
		dmg = dmg.Narrow(ran.Loads)
		derived = derivedOf(dmg)
		stored = dmg.Parts & (gitproc.PartPack | gitproc.PartObject)
	}
	hinted, crashed := len(ran.Result.Trace.Hints) > 0, ran.Result.Crashed
	if derived == 0 && stored == 0 && !hinted && !crashed {
		return
	}
	e, inc := c.incarnationAt(ran.Dir)
	if e == nil {
		// A scratch or a view: only its alternate's derived parts and a
		// crash are the cache's; its pack and object parts, and the packs
		// its hints name, may be the client's.
		if e, inc = c.incarnationAt(ran.Alternate); e == nil {
			return
		}
		if stored != 0 {
			c.countReports(stored, reportUnrouted)
		}
		if hinted {
			c.countPart(reportHint, reportUnrouted)
		}
		stored, hinted = 0, false
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.current != inc || e.dead {
		return
	}
	if stored != 0 || hinted || crashed {
		outcome := c.scrubAsked(inc)
		c.countReports(stored, outcome)
		if hinted {
			c.countPart(reportHint, outcome)
		}
		if crashed {
			c.countPart(reportCrash, outcome)
		}
	}
	if derived == 0 {
		return
	}
	inc.repair.drops.add(dmg.Drops(), ran.Result.Began, dropReport)
	if e.verifying > 0 {
		c.countReports(derived, reportDeferred)
		return
	}
	c.countReports(derived, reportQueued)
	c.repairRound(e)
}

// derivedOf is the derived parts a damage names — the index, the graph, a
// chunk-format file of either — for the reports' counts.
func derivedOf(d *gitproc.Damage) gitproc.Part {
	return d.Parts & (gitproc.PartMultiPackIndex | gitproc.PartCommitGraph | gitproc.PartChunk)
}

// countPart counts one report of part with outcome.
func (c *Cache) countPart(part reportPart, outcome reportOutcome) {
	if c.metrics != nil {
		c.metrics.Add(metrics.CodeCacheReports, 1, metrics.Attrs{"part": string(part), "outcome": string(outcome)})
	}
}

// countReports counts one report of each part of parts with outcome.
func (c *Cache) countReports(parts gitproc.Part, outcome reportOutcome) {
	if c.metrics == nil {
		return
	}
	for _, p := range []struct {
		bit  gitproc.Part
		name reportPart
	}{
		{gitproc.PartCommitGraph, reportGraph}, {gitproc.PartMultiPackIndex, reportMidx},
		{gitproc.PartChunk, reportChunk}, {gitproc.PartPack, reportPack}, {gitproc.PartObject, reportObject},
	} {
		if parts&p.bit != 0 {
			c.countPart(p.name, outcome)
		}
	}
}

// incarnationAt is the entry and its CURRENT incarnation when dir is that
// incarnation's directory (repos/<id>.<n>.git), or nil: the one parse of an
// incarnation's name, beside newIncarnation's. A retiring incarnation is
// none — its successor was read whole and checked.
func (c *Cache) incarnationAt(dir string) (*entry, *incarnation) {
	if dir == "" || filepath.Dir(dir) != filepath.Join(c.dir, reposDir) {
		return nil, nil
	}
	id, n, ok := parseIncarnation(filepath.Base(dir))
	if !ok {
		return nil, nil
	}
	c.mu.Lock()
	e := c.entries[id]
	c.mu.Unlock()
	if e == nil {
		return nil, nil
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if inc := e.current; inc != nil && inc.n == n && !e.dead {
		return e, inc
	}
	return nil, nil
}

// repairRound queues a LINEARIZABLE round with no waiter on e when none is
// queued — or makes the queued one linearizable — and starts it when none
// runs; e.mu is held. Not while a check of e is admitted: its release calls
// it. converge reads the round FULL (incarnation.repair).
func (c *Cache) repairRound(e *entry) {
	if e.verifying > 0 || e.dead || c.isStopped() {
		return
	}
	linearizable := statelog.Freshness{Level: statelog.ReadLinearizable}
	if e.next == nil {
		e.next = &round{fresh: linearizable}
	} else {
		e.next.fresh = merge(e.next.fresh, linearizable)
	}
	c.start(e)
}

// dropDamaged is a full round's drop of damaged derived files, before its own
// index decides: what a failed request or the round's own git asked
// (incarnation.drop), what reports asked (incarnation.repair, unless a check
// of the repository is admitted), and what the round's check or a scrub found
// (ck.still). Each asked part is TAKEN before the drop, so a run that names
// it while the round runs marks it again for the next; a drop that fails
// puts it back.
//
// THE GUARD: an asked part whose current file this process wrote after the
// asking run began is spared (`stale`). A part dropped for damage again
// within RebuildQuiet of its last drop for damage is not made due at once
// (`again`, D13); any other part dropped for damage is: the index as an
// indexRepair, the graph by the maintainer's graph-first run. The check's own
// drops are unguarded — the bytes decided.
func (c *Cache) dropDamaged(e *entry, inc *incarnation, res *applied) error {
	e.mu.Lock()
	asked := inc.drop
	inc.drop = nil
	var reported partDrops
	if e.verifying == 0 {
		reported, inc.repair.drops = inc.repair.drops, nil
	}
	all := partDrops{}
	all.merge(asked)
	all.merge(reported)
	var drop, stale, again gitproc.Part
	byTrigger := map[string]gitproc.Part{}
	for p, a := range all {
		if w, ok := inc.written[p]; ok && !a.began.IsZero() && !w.Before(a.began) {
			stale |= p
			continue
		}
		drop |= p
		byTrigger[a.by] |= p
	}
	e.mu.Unlock()
	if stale != 0 {
		c.countReports(stale, reportStale)
	}

	_, checks := res.checked.still(inc.dir)
	var found gitproc.Part
	for p, a := range checks {
		if p&(drop|res.dropped) == 0 {
			found |= p
			byTrigger[a.by] |= p
		}
	}
	if all := drop | found; all&^res.dropped != 0 {
		if err := c.dropDerived(e, inc, all&^res.dropped); err != nil {
			e.mu.Lock()
			inc.drop.merge(asked)
			inc.repair.drops.merge(reported)
			e.mu.Unlock()
			return err
		}
	}
	res.trashed = res.trashed || drop|found != 0
	res.dropped |= drop | found
	for by, parts := range byTrigger {
		// Derived files git could not read, or that failed their trailer,
		// dropped in place: they were damaged, not the repository, and the
		// round or the maintainer writes sound ones over the same packs.
		c.log.Warn("code_cache_derived_repaired", "repo", e.id, "incarnation", inc.n,
			"files", derivedFiles(parts), "trigger", by)
	}

	now := c.now()
	e.mu.Lock()
	defer e.mu.Unlock()
	for _, p := range []gitproc.Part{gitproc.PartMultiPackIndex, gitproc.PartCommitGraph} {
		if (drop|found)&p != 0 {
			// DAMAGED AGAIN: dropped for damage within RebuildQuiet of the
			// last drop of it for damage, whatever was written between —
			// the rewrite the first drop made due, and any ordinary one
			// after it, which a disk that keeps damaging damages too.
			if at, ok := inc.damagedAt[p]; ok && now.Sub(at) < RebuildQuiet {
				again |= p
			}
			if inc.damagedAt == nil {
				inc.damagedAt = map[gitproc.Part]time.Time{}
			}
			inc.damagedAt[p] = now
		}
		switch {
		case again&p != 0:
			if inc.againLogged&p == 0 {
				inc.againLogged |= p
				c.log.Warn("code_cache_derived_damaged_again", "repo", e.id, "incarnation", inc.n,
					"part", derivedFiles(p))
			}
			c.countReports(p, reportAgain)
			if p == gitproc.PartCommitGraph {
				inc.graphFailed = true
			}
		case p == gitproc.PartMultiPackIndex && (drop|found|res.dropped)&p != 0:
			// A multi-pack-index dropped as damaged is due at once: every
			// clone of the copy goes without its bitmap until it is written
			// again, and nothing but a push would otherwise make it due. A
			// graph dropped so needs nothing: the maintainer writes a
			// missing graph first, at once.
			inc.indexRepair = true
		}
	}
	return nil
}

// wrote stamps that this process has just changed the files of parts in inc
// — a write that moved a file's identity, or a drop — as a time.Now()
// reading, compared on the monotonic clock with a report's Began (D4); e.mu
// is held. Stamped as the write's step RETURNS, after its rename, so a run
// that began between the rename and the stamp is presumed to have read the
// file replaced: it spares a file, and never drops a fresh one.
func wrote(inc *incarnation, parts gitproc.Part) {
	now := time.Now()
	for _, p := range []gitproc.Part{gitproc.PartMultiPackIndex, gitproc.PartCommitGraph} {
		if parts&p != 0 {
			if inc.written == nil {
				inc.written = map[gitproc.Part]time.Time{}
			}
			inc.written[p] = now
		}
	}
}
