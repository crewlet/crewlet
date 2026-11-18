package cache

import (
	"errors"
	"fmt"
	"maps"
	"path/filepath"
	"slices"
	"time"

	"github.com/crewlet/crewlet/internal/gitlet/gitproc"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/statelog/metrics"
)

// WHAT ANY GIT PROCESS REPORTS DAMAGED IS REPAIRED, WHETHER OR NOT IT FAILED
// (package doc, "What any git process reports damaged is repaired"). observe
// routes each run by its directory (incarnationAt, swapAt, mergeAt) and
// records a derived part whose declaration drops on a report (OnReported) on
// the incarnation's repair mark, queuing repairRound; dropDamaged guards the
// drop, and a part damaged again within RebuildQuiet waits for the ordinary
// schedule (D13). A part that keeps its files on a report is damage no drop
// repairs: like a stored part, a hint or a crash it makes the scrub due
// (scrubAsked), since queued it would ask a round that moves nothing. HELD FOR
// THE REQUEST: what the run's reader (gitproc.Cmd.Repairer; a round's
// roundReader) will repair is held for it (incarnation.held), then consumed,
// carried or handed on (consume, carry, handOn, settleReader).

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
	// reportQueued: a repair asked — a derived part a drop repairs recorded
	// on the repair mark with a round queued; anything else made the scrub
	// due, or answered by the one already due or running (scrub.go).
	reportQueued reportOutcome = "queued"
	// reportDeferred: recorded while a check of the repository is
	// admitted; the check's release queues the round.
	reportDeferred reportOutcome = "deferred"
	// reportStale: not dropped — this process wrote the part's current file
	// after the reporting run began.
	reportStale reportOutcome = "stale"
	// reportUnrouted: nothing recorded — a pack or object part, or a hint,
	// from a push's scratch or a view (it may be the client's pack), or a
	// derived part from a merge's build, whose store holds none.
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
	// reportRequest: a report HELD FOR THE REQUEST (or round, roundReader)
	// whose run made it, answered by the repair it asked (Cache.Rebuild,
	// damagedRound): no scrub is made due beside it.
	reportRequest reportOutcome = "request"
)

// heldReport is what one run reported that the copy's scrub answers: stored
// parts, derived parts whose every declaration keeps its files on a report, a
// hint, a crash. HELD (incarnation.held) while the run's request may still
// repair it.
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

// toScrub hands r to inc's scrub (scrubAsked), counting the outcome; e.mu is
// held.
func (c *Cache) toScrub(inc *incarnation, r heldReport) {
	c.countHeld(r, c.scrubAsked(inc))
}

// handOn hands every report held for reader by of inc to the ordinary rules
// (toScrub) as though just arrived, as the reader lets inc go, so a request
// that never asked its repair leaves nothing held. Held reports go to the copy
// serving inc's files (serving); about a copy a rebuild or a purge has since
// replaced, or once the cache stops, they are dropped, as observe drops one.
// e.mu is held.
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
// from's run failed over, repaired IN PLACE (Cache.Rebuild) and handed to the
// same request. The host lets go of from before it runs over to, so a report
// left under from would be handed on (handOn) while the request still runs and
// may yet repair a pack beneath the derived file (awaitRequest). A to on
// another incarnation, or already let go, takes nothing: from's release drops
// what it holds about a replaced copy. e.mu is held.
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
	// checksum and awaiting repair, by path relative to the incarnation, at
	// the identity it failed at: the round acts only while the file keeps
	// that identity (checked.still), then takes it off (settleFound); a round
	// that cannot act leaves it to the next. Only a SCRUB's findings
	// (checkedFile.scrubbed) stand for a scrub's answer (scrubWaits): a check
	// reads no pack body. A FINDING FOUND AGAIN STAYS AS IT WAS FOUND: a check
	// replaces only one at another identity (recordCheck), a scrub any
	// (runScrub).
	found map[string]checkedFile
	// forces is how many in-place repairs of a pack's or an object's damage
	// ([Cache.Rebuild], repairDamaged) asked the next full round to hold
	// EVERY index file to its trailer, vouched or not; that round's check
	// spends them once it has run, whatever follows (recordCheck).
	forces int
	// reinstall is the packs a confirmed hint named ([Failure].Packs), by
	// checksum, each at the identity of its .pack when it was asked: the
	// round installs each again, one GET, while it still has that identity.
	reinstall map[string]checkedFile
	// fetchFailed is the packs, by checksum, whose last download to install
	// them again failed (install.go), each held off for RebuildCooldown,
	// doubling per failure up to RebuildBackoffCap: asking a store that is
	// down or lost the bundle every round ends nothing. Taken off with the
	// pack's findings once it is installed.
	fetchFailed map[string]fetchBackoff
}

// fetchBackoff is a pack's download failures in a row, when the next attempt
// may be made (Options.Now's clock), and the store's last answer
// (ErrStoreRead, ErrPackMissing, ErrCorruptPack), given to a caller whose
// repair waits on the pack (heldOffFor).
type fetchBackoff struct {
	failures int
	until    time.Time
	err      error
}

// pending reports whether the mark asks anything at all.
func (m repairMark) pending() bool {
	return len(m.drops) > 0 || len(m.found) > 0 || m.forces > 0 || len(m.reinstall) > 0
}

// scrubWaits reports whether a SCRUB's finding still waits for its repair: it
// answers a repair's step 3 in that scrub's place (scrubFor) and covers a
// report within ScrubCooldown (scrubAsked). A check's finding does neither: a
// check reads no pack body.
func (m repairMark) scrubWaits() bool {
	for _, f := range m.found {
		if f.scrubbed {
			return true
		}
	}
	return false
}

// due reports whether the mark asks a full round anything it can do at now:
// a drop, a forced check, or a finding not on a held-off pack. A mark of
// held-off packs alone asks nothing, so rounds stay deltas until the backoff
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
// a forced round can install again (reinstallHinted). install leaves a
// held-off pack as it stands, so a repair whose named packs are all held off
// is decided as one naming none (Cache.Rebuild's and repairDamaged's step 2).
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
// meet first, answered without running it: the last download failure of a
// held-off pack seed does not link, or nil. A rebuild downloads every pack it
// does not link, so it would ask the store for that pack again within the
// backoff (fetchFailed), making a new incarnation for a rebuild that cannot
// complete. The rows still name every such pack: only a purge drops one, and
// it ends the entry. The error names the earliest pack and its backoff left,
// and wraps the store's failure so a host answers it as the store's
// (ErrStoreRead's 503, ErrPackMissing's, ErrCorruptPack's).
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

// observe is the pool's observer (gitproc.Pool.Observe), called after every
// git run on the run's own goroutine. It finds the incarnation the run's
// damage, hints and crash are about, records a derived part a drop repairs
// and queues a round, and makes the scrub due for the rest — or HOLDS it for
// the run's reader (Ran.Repairer; HELD FOR THE REQUEST). It takes the entry's
// mu, never its write lock, and does no I/O, so it never holds its run.
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
			// A MERGE'S BUILD reads only its store's links of the
			// incarnation's own packs (the very inodes), so its pack, object
			// and hint reports and its crash are that copy's; the store holds
			// no derived file, so a derived part it names is not.
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
	// named file may be one the swap left behind (the multi-pack-index never
	// crosses one), so a drop on the serving copy would read it whole for
	// nothing, while the scrub checks every file it holds. Dropped instead,
	// a damaged pack both copies share waits for the next daily scrub.
	evidence := staged || at != inc
	var named, drops derivedSet
	if derived != 0 {
		// Only declarations whose files the copy holds (image.holding; in
		// CHAIN mode, the installed chain): a part whose file is gone since
		// the run read it is counted stale, since dropping what is not there
		// would read the copy whole for nothing.
		named = c.image.reported(dmg.Drops()) & c.image.holding(inc)
		if named == 0 {
			c.countReports(derived, reportStale)
		}
		if !evidence {
			drops = named & c.image.dropping(EventReported)
		}
		// A DERIVED PART NO DROP REPAIRS (every declaration it names keeps
		// its files on a report, OnReported) goes to the copy's scrub, whose
		// bytes decide: recorded as a drop it would queue a full round that
		// moves nothing, for every process that reported it.
		r.kept = c.image.naming(derived, named&^drops)
	}
	if r.scrubs() {
		// Held only for a run IN the incarnation: for a scratch or a view
		// the host repairs only the alternate's shape, which no process
		// reports. Held on the incarnation the reader holds, even a
		// replaced one: its release hands them to the copy serving (handOn).
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

// incarnationAt is the entry and incarnation whose directory dir is
// (repos/<id>.<n>.git) — its CURRENT one, or a retiring one a SWAP replaced,
// whose kept inodes the current one serves (swappedOut) — or nil; the one
// parse of an incarnation's name besides newIncarnation's. One a REBUILD
// replaced is none: its successor was read whole and checked.
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
// index decides: what a failed request or the round's git asked
// (incarnation.drop) and what reports asked (incarnation.repair, unless a
// check is admitted), under EventReported (dropFor reads each declaration
// again); and what the round's check or a scrub found failing (ck.still),
// under eventFailedCheck whatever its declaration says, since the bytes
// decided. Each asked declaration is TAKEN first, so a run naming it meanwhile
// marks it for the next round; a failed drop puts it back. THE GUARD: one whose
// current file this process wrote after the asking run began is spared
// (`stale`); one dropped for damage again within RebuildQuiet is not made due
// at once (`again`, D13); any other is (the index as an indexRepair, the graph
// by the maintainer's graph-first run). The check's drops are unguarded.
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
		// declaration answers (OnReported), and the bytes', which none does:
		// a file failing its check is never kept, or every scrub its reports
		// asked would find it failing for ever.
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
		// WHAT FOLLOWS RECORDS ONLY WHAT WAS DROPPED: a declaration dropFor
		// kept, recorded as dropped, reads as damage that survived a drop
		// (damagedRound) and asks a pack's repair of an untouched copy.
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
		// Dropped in place: the derived files were damaged, not the
		// repository, and the round or the maintainer rewrites them.
		c.log.Warn("code_cache_derived_repaired", "repo", e.id, "incarnation", inc.n,
			"files", im.files(byTrigger[by]), "trigger", by)
	}

	now := c.now()
	e.mu.Lock()
	defer e.mu.Unlock()
	im.each(im.all(), func(i int, d *Derived) {
		s := derivedSet(1) << i
		if (drop|found)&s != 0 {
			// DAMAGED AGAIN within RebuildQuiet, whatever was written
			// between: a disk that keeps damaging damages the rewrites too.
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
			// A dropped multi-pack-index is due at once: clones go without
			// its bitmap until it is rewritten, and only a push would make it
			// due. A graph needs nothing: the maintainer writes a missing one
			// first.
			inc.indexRepair = true
		}
	})
	return nil
}

// wrote stamps that this process just changed the files of each declaration
// of s in inc (a write that moved a file's identity, or a drop), as a
// time.Now() reading compared on the monotonic clock with a report's Began
// (D4); e.mu is held. Stamped after the rename, so a run begun between the
// rename and the stamp is presumed to have read the replaced file: it spares a file, never drops a
// fresh one.
func (c *Cache) wrote(inc *incarnation, s derivedSet) {
	now := time.Now()
	c.image.each(s, func(_ int, d *Derived) {
		if inc.written == nil {
			inc.written = map[DerivedName]time.Time{}
		}
		inc.written[d.Name] = now
	})
}
