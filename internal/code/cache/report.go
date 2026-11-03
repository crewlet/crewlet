package cache

import (
	"errors"
	"fmt"
	"maps"
	"path/filepath"
	"slices"
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
//     over the incarnation itself. One run over a MERGE'S STORE — a
//     compaction's build, in its scratch — reports about the incarnation the
//     store's links came from (mergeAt): its pack, object and hint parts and
//     a crash, since the store holds nothing but that copy's packs and the
//     build reads nothing else; a derived part, of which the store holds
//     none, is `unrouted`. A retired incarnation's successor was read whole
//     and checked: a report about it is dropped.
//   - REPAIRED BY THE NEXT FULL ROUND (repairRound): a linearizable one,
//     queued with no waiter when none is queued — linearizable, because a
//     full round's sweep removes every pack its snapshot does not name, and an
//     adoption installs a pack past the incarnation's own position. Reports
//     coalesce on the mark while one is queued. Only a part whose declaration
//     drops on a report (OnReported) is recorded and queued: one that keeps
//     its files is damage no drop repairs, and makes the scrub due like a
//     stored report (below) — queued, it asked a round that moved nothing for
//     every git process that reported it.
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
//     a report, and neither is a derived file whose declaration keeps its
//     files on one. `hash mismatch` names no file — the pack, its index or the
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
//   - HELD FOR THE REQUEST: a stored report, an object report, a hint or a
//     crash — or a derived part no drop repairs — from a run IN the
//     incarnation whose Cmd names one of its readers (gitproc.Cmd.Repairer,
//     Synced.Repairer) is held for that reader (incarnation.held) and makes
//     nothing due: the host names its reader on every process whose failure
//     before its first byte it repairs, and that repair — a forced round in
//     place, then the scrub for its own callers, then the rebuild
//     (Cache.Rebuild) — is the answer to exactly what such a report names.
//     Made due as well, the report's scrub ran beside the request's own
//     repair of the same damage: a read of every byte of the copy, about
//     1.13 times its pack bytes (some 24 GB for the largest repository the
//     defaults admit), beside a repair of one pack, and a race between the
//     two over which repaired it. The reader's repair of a pack's or an
//     object's damage CONSUMES what is held for it (consume, counted
//     `request`) once it has answered the reader. Its repair of a shape or a
//     derived file IN PLACE reads no pack, so it answers none of a crash's
//     or a stored part's and consumes nothing: it CARRIES what is held to
//     the reader it hands the repaired copy to (carry) — the same request,
//     which lets go of the pin it failed over before it runs again — so the
//     hold is the request's across its repairs, never one pin's: handed on
//     with that first pin, a crash beside a damaged graph went to the scrub
//     while the request ran on, and on into a pack's repair when its retry
//     met the pack beneath the graph, the escalation the host's repair limit
//     is for. The reader's RELEASE hands what is left to the ordinary rules
//     as though it had just arrived (handOn) — the report of a request that
//     failed past its first byte, failed in a way the host does not repair,
//     never failed at all, left before its repair answered, or was repaired
//     for a shape or a derived file alone and then succeeded — so the bound
//     on a hold is the request's own lifetime, which always ends, and no
//     timer or grace stands beside it. A derived part a drop repairs is
//     never held: its round drops it in place, races nothing and is the
//     repair's own first step; nor is a report from a scratch or a view,
//     whose process the host repairs only for its alternate's shape, which
//     no process reports. A ROUND'S OWN GIT is held the same way: the peel
//     names a reader the round mints (roundReader, sync.go), and what it
//     reports is consumed by the round's repair of a pack's or an object's
//     damage, or handed on as the round settles otherwise (settleReader).

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
	// reportHint: a pack a run named as holding a corrupt object
	// (gitproc.Hint) — 2.56's bare message, or the pack 2.55's report of
	// the same damage names, beside its pack part.
	reportHint reportPart = "hint"
)

// reportOutcome is what became of a report: the `outcome` attribute of
// crewlet.code.cache.reports.
type reportOutcome string

const (
	// reportQueued: a repair asked — a derived part whose declaration drops
	// on a report recorded on the incarnation's repair mark with a round
	// queued to drop it; any other part, a hint or a crash, the
	// incarnation's scrub made due, or answered by the one already due or
	// running (scrub.go).
	reportQueued reportOutcome = "queued"
	// reportDeferred: recorded while a check of the repository is
	// admitted; the check's release queues the round.
	reportDeferred reportOutcome = "deferred"
	// reportStale: not dropped — this process wrote the part's current file
	// after the reporting run began.
	reportStale reportOutcome = "stale"
	// reportUnrouted: a pack or object part, or a hint, from a process in a
	// push's scratch or a view, which may be the client's pack — or a derived
	// part from a merge's build, whose store holds no derived file; nothing
	// recorded.
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
	// reportRequest: a report HELD FOR THE REQUEST whose own run made it —
	// or for the round whose own git did (roundReader) — and answered by the
	// repair that request or that round asked (Cache.Rebuild, damagedRound):
	// no scrub is made due beside it.
	reportRequest reportOutcome = "request"
)

// heldReport is what one run reported that the copy's scrub answers — its
// stored parts (a pack's, an object's), the derived parts whose every
// declaration keeps its files on a report, a hint, a crash — as the observer
// takes it; HELD (incarnation.held) while the request whose run it was may
// still repair it.
type heldReport struct {
	stored, kept    gitproc.Part
	hinted, crashed bool
}

// scrubs reports whether r names anything the copy's scrub answers.
func (r heldReport) scrubs() bool { return r.stored|r.kept != 0 || r.hinted || r.crashed }

// countHeld counts every part r names with outcome.
func (c *Cache) countHeld(r heldReport, outcome reportOutcome) {
	c.countReports(r.stored|r.kept, outcome)
	if r.hinted {
		c.countPart(reportHint, outcome)
	}
	if r.crashed {
		c.countPart(reportCrash, outcome)
	}
}

// toScrub hands r to inc's scrub (scrubAsked) and counts it with what became
// of it — the ordinary rule for a report the scrub answers; e.mu is held.
func (c *Cache) toScrub(inc *incarnation, r heldReport) {
	c.countHeld(r, c.scrubAsked(inc))
}

// handOn takes every report held for reader by of inc and hands each to the
// ordinary rules (toScrub) as though it had just arrived — while inc, or the
// copy a swap made current in its place (serving), is the entry's copy in
// service and the cache runs; a report about a copy a rebuild or a purge has
// since replaced is dropped, as one arriving about it would be (observe).
// Called as the reader lets inc go, so a request that ended without asking
// its repair — it failed past its first byte, failed in a way the host does
// not repair, or never failed — leaves nothing held. e.mu is held.
func (c *Cache) handOn(e *entry, inc *incarnation, by gitproc.Repairer) {
	held, ok := inc.held[by]
	if !ok {
		return
	}
	delete(inc.held, by)
	at := serving(inc)
	if e.dead || e.current != at || c.isStopped() {
		return
	}
	for _, r := range held {
		c.toScrub(at, r)
	}
}

// consume takes every report held for reader by of inc as answered by the
// repair that reader asked, counting each `request`; the reader keeps its
// pin, and what its later runs report is held again. e.mu is held.
func (c *Cache) consume(inc *incarnation, by gitproc.Repairer) {
	held, ok := inc.held[by]
	if !ok || len(held) == 0 {
		return
	}
	inc.held[by] = nil
	for _, r := range held {
		c.countHeld(r, reportRequest)
	}
}

// carry moves every report held for from's reader onto to's: to is the copy
// from's run failed over, repaired IN PLACE (a shape, a derived file;
// Cache.Rebuild) and handed to the same request under a reader of its own.
// The host lets go of from before it runs again over to (its repair loop), so
// a report left under from would be handed on (handOn) while the request it
// belongs to was still running — and still repairing, when its retry meets a
// pack's damage beneath the derived file. Carried, it is consumed by that
// pack's repair under to (awaitRequest) or handed on with to's release. A to
// on another incarnation — the copy replaced by the round that answered — and
// a to already let go take nothing: what from holds about a copy since
// replaced is dropped with from's release, as handOn drops any. e.mu is held.
func (c *Cache) carry(from, to Synced) {
	inc := from.inc
	if to.inc != inc {
		return
	}
	held := inc.held[from.Repairer]
	if _, live := inc.held[to.Repairer]; !live || len(held) == 0 {
		return
	}
	inc.held[from.Repairer] = nil
	inc.held[to.Repairer] = append(inc.held[to.Repairer], held...)
}

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

// derivedDrops is the derived files to drop, by declaration, each with what
// asked it.
type derivedDrops map[DerivedName]dropAsk

// add records that a run begun at began asked name dropped, keeping each
// declaration's latest ask.
func (d *derivedDrops) add(name DerivedName, began time.Time, by string) {
	if *d == nil {
		*d = derivedDrops{}
	}
	if have, ok := (*d)[name]; !ok || have.began.Before(began) {
		(*d)[name] = dropAsk{began: began, by: by}
	}
}

// addSet records that a run begun at began asked every declaration of s
// dropped.
func (d *derivedDrops) addSet(im *image, s derivedSet, began time.Time, by string) {
	im.each(s, func(_ int, decl *Derived) { d.add(decl.Name, began, by) })
}

// merge adds every entry of o.
func (d *derivedDrops) merge(o derivedDrops) {
	for name, a := range o {
		d.add(name, a.began, a.by)
	}
}

// repairMark is what reports, checks and scrubs asked an incarnation's next
// full round to do.
type repairMark struct {
	// drops is the derived parts reports named.
	drops derivedDrops
	// found is every file a round's check or a scrub found failing its
	// checksum that waits for its repair, by its path relative to the
	// incarnation, at the identity it failed at: the round reinstalls a pack,
	// or drops a derived file, only while it still has that identity
	// (checked.still), and takes it off once it has (settleFound). A check's
	// go on as it ends (recordCheck), so a round that cannot act on them —
	// its install failing on a pack it adds, a finding's own download
	// failing — leaves them to the next, as a scrub's. checkedFile.scrubbed
	// tells the two apart: only a SCRUB's findings stand for a scrub's answer
	// (scrubWaits) — a check reads no pack body. A FINDING FOUND AGAIN STAYS
	// AS IT WAS FOUND: a check that finds a file failing at the identity its
	// finding here names leaves that finding be — the same damage, a scrub's
	// staying a scrub's — and replaces only one at another identity, damage
	// newer than it (recordCheck); a scrub's replaces whatever is here for
	// its file (runScrub).
	found map[string]checkedFile
	// forces is how many in-place repairs of a pack's or an object's damage
	// — a failed request's ([Cache.Rebuild]) or a round's own git's
	// (repairDamaged) — asked the next full round to hold EVERY index file to
	// its trailer, vouched or not; that round's check spends them once it has
	// run, whatever the round meets after (recordCheck), what it found going
	// on found.
	forces int
	// reinstall is the packs a confirmed hint named ([Failure].Packs), by
	// checksum, each at the identity of its .pack when it was asked: the
	// round installs each again, one GET, while it still has that identity.
	reinstall map[string]checkedFile
	// fetchFailed is the packs, by checksum, whose last download to install
	// them again failed (install.go), each held off until its backoff has
	// passed: RebuildCooldown after the first failure, twice as long after
	// each one after it, up to RebuildBackoffCap — the store being down, or
	// no longer holding the bundle, is not ended by asking it again every
	// round. Taken off with the pack's findings once it is installed.
	fetchFailed map[string]fetchBackoff
}

// fetchBackoff is a pack's download failures in a row, when the next attempt
// may be made (Options.Now's clock), and what the last one failed with — the
// store's own answer (ErrStoreRead, ErrPackMissing, ErrCorruptPack), which is
// what a caller whose repair waits on the pack is answered (heldOffFor).
type fetchBackoff struct {
	failures int
	until    time.Time
	err      error
}

// pending reports whether the mark asks anything at all.
func (m repairMark) pending() bool {
	return len(m.drops) > 0 || len(m.found) > 0 || m.forces > 0 || len(m.reinstall) > 0
}

// scrubWaits reports whether a finding a SCRUB made still waits for its
// repair: what answers a repair's step 3 in that scrub's place (scrubFor) and
// covers a report within ScrubCooldown of it (scrubAsked). A round's check's
// finding is neither — a check reads index files alone, and the scrub is
// asked for the pack bodies it never reads.
func (m repairMark) scrubWaits() bool {
	for _, f := range m.found {
		if f.scrubbed {
			return true
		}
	}
	return false
}

// due reports whether the mark asks a full round anything it can do at now:
// a drop, a forced check, or a finding that is a derived file's or a pack's
// not held off. A mark whose every finding is a pack held off asks nothing,
// so the copy's rounds stay deltas and keep answering until the backoff
// passes.
func (m repairMark) due(now time.Time) bool {
	if len(m.drops) > 0 || m.forces > 0 {
		return true
	}
	held := m.heldOff(now)
	for _, f := range m.found {
		if cs, ok := packOfRel(f.rel); !ok || !held[cs] {
			return true
		}
	}
	for cs := range m.reinstall {
		if !held[cs] {
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

// fetchable is the packs of packs whose download is not held off at now: what
// a forced round asked to install them again (reinstallHinted) can fetch. A
// held-off pack is no pack to that round — install leaves it as it stands —
// so a repair whose named packs are all held off is decided as one that names
// none (Cache.Rebuild's and repairDamaged's step 2).
func (m repairMark) fetchable(packs []string, now time.Time) []string {
	held := m.heldOff(now)
	var out []string
	for _, cs := range packs {
		if !held[cs] {
			out = append(out, cs)
		}
	}
	return out
}

// fetchFailedAt records that a download of pack cs to install it again failed
// at now with err, and holds the next one off.
func (m *repairMark) fetchFailedAt(cs string, now time.Time, err error) {
	if m.fetchFailed == nil {
		m.fetchFailed = map[string]fetchBackoff{}
	}
	b := m.fetchFailed[cs]
	b.failures++
	b.until, b.err = now.Add(rebuildWindow(b.failures)), err
	m.fetchFailed[cs] = b
}

// heldOffFor is what a WHOLE REBUILD seeded with seed (seedRebuild) would
// meet first, answered without asking it: the last download failure of a pack
// whose download is held off at now and that seed does not link — or nil when
// there is none. A rebuild downloads every pack it does not link, so it would
// ask the store for that very pack again, under the same row, within the
// backoff that holds its download off (fetchFailed): a GET the backoff exists
// to spare, from a store that refused it moments ago, which ends nothing the
// in-place reinstall the backoff allows next would not end — and every one of
// them a new incarnation's directory made, its whole rows read and its line's
// window grown, for a rebuild that cannot complete. Every such pack is one the
// rows still name: they drop a pack only with the repository's purge, which
// ends the entry. The answer names the earliest pack by checksum and how long
// its backoff has left, and wraps the store's own failure, so a host answers
// it as the store's (cache.ErrStoreRead's 503, ErrPackMissing's,
// ErrCorruptPack's).
func (m repairMark) heldOffFor(now time.Time, seed []string) error {
	var held []string
	for cs := range m.heldOff(now) {
		if !slices.Contains(seed, cs) {
			held = append(held, cs)
		}
	}
	if len(held) == 0 {
		return nil
	}
	slices.Sort(held)
	b := m.fetchFailed[held[0]]
	more := ""
	if len(held) > 1 {
		more = fmt.Sprintf(" (and %d more)", len(held)-1)
	}
	return fmt.Errorf("cache: the copy's pack-%s%s is damaged and the object store did not hand it over to install "+
		"again; a rebuild would ask it for the same pack, so none is asked until its next download, in %v: %w",
		held[0], more, b.until.Sub(now).Round(time.Second), b.err)
}

// packOfRel is the checksum of the pack whose file rel is, or false.
func packOfRel(rel string) (string, bool) {
	m := packFileName().FindStringSubmatch(filepath.Base(rel))
	if m == nil {
		return "", false
	}
	return m[1], true
}

// observe is the pool's observer (gitproc.Pool.Observe): called after every
// git run with where it ran and how it ended, on the run's own goroutine. It
// reads the parts the run's damage names, its hints and whether it crashed,
// finds the incarnation they are about by the process's directory, records a
// derived part a drop repairs on its repair mark and queues a round, and
// makes its scrub due for the rest — or HOLDS the rest for the reader the run
// names (Ran.Repairer), whose own repair answers it (HELD FOR THE REQUEST) —
// under the entry's mu, never its write lock, and doing no I/O, so it never
// holds its run.
func (c *Cache) observe(ran gitproc.Ran) {
	if c.isStopped() {
		return
	}
	var dmg *gitproc.Damage
	var derived gitproc.Part
	var r heldReport
	if errors.As(ran.Result.Damage, &dmg) {
		dmg = dmg.Narrow(ran.Loads)
		derived = derivedOf(dmg)
		r.stored = dmg.Parts & (gitproc.PartPack | gitproc.PartObject)
	}
	r.hinted, r.crashed = len(ran.Result.Trace.Hints) > 0, ran.Result.Crashed
	if derived == 0 && !r.scrubs() {
		return
	}
	e, inc := c.incarnationAt(ran.Dir)
	inIncarnation, staged := e != nil, false
	if !inIncarnation {
		e, inc = c.swapAt(ran.Dir)
		inIncarnation, staged = e != nil, e != nil
	}
	if !inIncarnation {
		if e, inc = c.mergeAt(ran.Alternate); e != nil {
			// A MERGE'S BUILD reads nothing but its store's links of the
			// incarnation's own packs — the very inodes — and writes into its
			// scratch, which nothing reads back: its pack, object and hint
			// reports and its crash are that copy's, for its scrub. The store
			// holds no derived file, so no derived part it names is the
			// copy's.
			if derived != 0 {
				c.countReports(derived, reportUnrouted)
				derived = 0
			}
			if !r.scrubs() {
				return
			}
		} else {
			// A scratch or a view: only its alternate's derived parts and a
			// crash are the cache's; its pack and object parts, and the
			// packs its hints name, may be the client's.
			if e, inc = c.incarnationAt(ran.Alternate); e == nil {
				return
			}
			if r.stored != 0 {
				c.countReports(r.stored, reportUnrouted)
			}
			if r.hinted {
				c.countPart(reportHint, reportUnrouted)
			}
			r.stored, r.hinted = 0, false
		}
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	// at is the copy serving the files the run read: inc, or — where a swap
	// replaced inc under a reader still running — the incarnation the swap
	// made current, which holds every file it kept as the same inode.
	at := serving(inc)
	if e.current != at || e.dead {
		return
	}
	// A REPORT ABOUT FILES A SWAP CARRIES (swap.go) — a run in a copy a swap
	// replaced, or the round's own index in the staged next one — is the
	// SCRUB'S EVIDENCE about the copy serving them, every part of it: the
	// file it names may be one the swap left behind (the multi-pack-index
	// never crosses one), which a drop on the copy serving would answer with
	// a whole read of it for nothing, and the scrub holds every file that
	// copy does hold to its checksum, dropping a derived one that fails.
	// Dropped as a report about a replaced copy is, a damaged pack both copies
	// share was found only by the next scrub a day later, though git had named
	// it — the swap of a repository compacted every few minutes replacing the
	// copy under nearly every long clone.
	evidence := staged || at != inc
	var named, drops derivedSet
	if derived != 0 {
		// Of the declarations whose files the copy holds (image.holding): in
		// CHAIN mode a part reaches the installed chain alone, and in either
		// mode a part of no file the copy holds — gone since the run read it,
		// dropped or cut — reaches nothing, counted stale: a drop of what is
		// not there would read the copy whole for nothing.
		named = c.image.reported(dmg.Drops()) & c.image.holding(inc)
		if named == 0 {
			c.countReports(derived, reportStale)
		}
		if !evidence {
			drops = named & c.image.dropping(EventReported)
		}
		// A DERIVED PART NO DROP REPAIRS — every declaration it names keeps
		// its files on a report (OnReported) — goes where all damage a drop
		// does not end goes: the copy's scrub, whose bytes decide, bounded
		// by its cooldown. Recorded as a drop it queued a full round that
		// could move nothing, for every git process that reported it.
		r.kept = c.image.naming(derived, named&^drops)
	}
	if r.scrubs() {
		// Held for its reader only where the run was IN the incarnation:
		// the host repairs nothing a process in a scratch or a view meets
		// but the alternate's shape, which no process started to report.
		// Held on the incarnation the reader holds, a replaced one's too:
		// its release hands them on to the copy serving (handOn).
		if held, ok := inc.held[ran.Repairer]; ok && inIncarnation && ran.Repairer != noReader {
			inc.held[ran.Repairer] = append(held, r)
		} else {
			c.toScrub(at, r)
		}
	}
	if drops == 0 {
		return
	}
	inc.repair.drops.addSet(c.image, drops, ran.Result.Began, dropReport)
	queued := c.image.naming(derived, drops)
	if e.verifying > 0 {
		c.countReports(queued, reportDeferred)
		return
	}
	c.countReports(queued, reportQueued)
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

// incarnationAt is the entry and its incarnation whose directory dir is
// (repos/<id>.<n>.git) — its CURRENT one, or a retiring one a SWAP replaced,
// whose files the current one serves as the very inodes it kept (swappedOut)
// — or nil: the one parse of an incarnation's name, beside newIncarnation's.
// A retiring incarnation a REBUILD replaced is none: its successor was read
// whole and checked, and holds none of its files unchecked.
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
	if e.dead {
		return nil, nil
	}
	if inc := e.current; inc != nil && inc.n == n {
		return e, inc
	}
	for _, inc := range e.retiring {
		if inc.n == n && swappedOut(e, inc) {
			return e, inc
		}
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
// (incarnation.drop) and what reports asked (incarnation.repair, unless a
// check of the repository is admitted) — EventReported for the declarations
// they name, each of which drops on a report (no source asks it of one that
// keeps its files, and dropFor reads the declaration again) — and what the
// round's check or a scrub found failing its Check (ck.still), dropped
// whatever its declaration says of a report (eventFailedCheck: the bytes
// decided). Only what dropFor dropped is recorded as dropped. Each asked
// declaration is TAKEN before the drop, so a run that names it while the
// round runs marks it again for the next; a drop that fails puts it back.
//
// THE GUARD: an asked declaration whose current file this process wrote
// after the asking run began is spared (`stale`). One dropped for damage again
// within RebuildQuiet of its last drop for damage is not made due at once
// (`again`, D13); any other dropped for damage is: the index as an
// indexRepair, the graph by the maintainer's graph-first run. The check's own
// drops are unguarded — the bytes decided.
func (c *Cache) dropDamaged(e *entry, inc *incarnation, res *applied) error {
	im := c.image
	e.mu.Lock()
	asked := inc.drop
	inc.drop = nil
	var reported derivedDrops
	if e.verifying == 0 {
		reported, inc.repair.drops = inc.repair.drops, nil
	}
	all := derivedDrops{}
	all.merge(asked)
	all.merge(reported)
	var drop, stale, again derivedSet
	byTrigger := map[string]derivedSet{}
	for name, a := range all {
		s := im.set(name)
		if w, ok := inc.written[name]; ok && !a.began.IsZero() && !w.Before(a.began) {
			stale |= s
			continue
		}
		drop |= s
		byTrigger[a.by] |= s
	}
	e.mu.Unlock()
	if stale != 0 {
		c.countReports(im.parts(stale), reportStale)
	}

	_, checks := res.checked.still(im, inc.dir)
	var found derivedSet
	for name, a := range checks {
		if s := im.set(name); s&(drop|res.dropped) == 0 {
			found |= s
			byTrigger[a.by] |= s
		}
	}
	if named := (drop | found) &^ res.dropped; named != 0 {
		// Two drops, by what found the damage: git's word, which a
		// declaration answers (OnReported); and the bytes', which none does
		// — a file failing its check is never kept and served, and kept, a
		// declaration that keeps its files on a report would be found
		// failing by every scrub its reports asked, for ever.
		dropped, err := c.dropFor(e, inc, EventReported, eventDetail{named: drop & named})
		if err != nil {
			e.mu.Lock()
			inc.drop.merge(asked)
			inc.repair.drops.merge(reported)
			e.mu.Unlock()
			return err
		}
		failed, err := c.dropFor(e, inc, eventFailedCheck, eventDetail{named: found & named})
		if err != nil {
			return err
		}
		dropped |= failed
		// WHAT FOLLOWS RECORDS ONLY WHAT WAS DROPPED. A declaration dropFor
		// kept was dropped by nothing, so nothing below may log it repaired,
		// count it dropped, stamp its damage or make its write due: recorded
		// as dropped, the round that met its damage read it as damage that
		// survived a drop (damagedRound) and asked a pack's repair of a copy
		// no drop had touched.
		kept := named &^ dropped
		drop &^= kept
		found &^= kept
		for by, s := range byTrigger {
			if s &^= kept; s == 0 {
				delete(byTrigger, by)
			} else {
				byTrigger[by] = s
			}
		}
	}
	res.trashed = res.trashed || drop|found != 0
	res.dropped |= drop | found
	for _, by := range slices.Sorted(maps.Keys(byTrigger)) {
		// Derived files git could not read, or that failed their trailer,
		// dropped in place: they were damaged, not the repository, and the
		// round or the maintainer writes sound ones over the same packs.
		c.log.Warn("code_cache_derived_repaired", "repo", e.id, "incarnation", inc.n,
			"files", im.files(byTrigger[by]), "trigger", by)
	}

	now := c.now()
	e.mu.Lock()
	defer e.mu.Unlock()
	im.each(im.all(), func(i int, d *Derived) {
		s := derivedSet(1) << i
		if (drop|found)&s != 0 {
			// DAMAGED AGAIN: dropped for damage within RebuildQuiet of the
			// last drop of it for damage, whatever was written between —
			// the rewrite the first drop made due, and any ordinary one
			// after it, which a disk that keeps damaging damages too.
			if at, ok := inc.damagedAt[d.Name]; ok && now.Sub(at) < RebuildQuiet {
				again |= s
			}
			if inc.damagedAt == nil {
				inc.damagedAt = map[DerivedName]time.Time{}
			}
			inc.damagedAt[d.Name] = now
		}
		switch {
		case again&s != 0:
			if inc.againLogged&s == 0 {
				inc.againLogged |= s
				c.log.Warn("code_cache_derived_damaged_again", "repo", e.id, "incarnation", inc.n,
					"part", im.files(s))
			}
			c.countReports(d.Part, reportAgain)
			if d.Name == DerivedGraph {
				inc.graphFailed = true
			}
		case d.Name == DerivedMidx && (drop|found|res.dropped)&s != 0:
			// A multi-pack-index dropped as damaged is due at once: every
			// clone of the copy goes without its bitmap until it is written
			// again, and nothing but a push would otherwise make it due. A
			// graph dropped so needs nothing: the maintainer writes a
			// missing graph first, at once.
			inc.indexRepair = true
		}
	})
	return nil
}

// wrote stamps that this process has just changed the files of each
// declaration of s in inc — a write that moved a file's identity, or a drop —
// as a time.Now() reading, compared on the monotonic clock with a report's
// Began (D4); e.mu is held. Stamped as the write's step RETURNS, after its
// rename, so a run that began between the rename and the stamp is presumed to
// have read the file replaced: it spares a file, and never drops a fresh one.
func (c *Cache) wrote(inc *incarnation, s derivedSet) {
	now := time.Now()
	c.image.each(s, func(_ int, d *Derived) {
		if inc.written == nil {
			inc.written = map[DerivedName]time.Time{}
		}
		inc.written[d.Name] = now
	})
}
