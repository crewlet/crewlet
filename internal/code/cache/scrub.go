package cache

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/crewlet/crewlet/internal/code/gitproc"
	"github.com/crewlet/crewlet/internal/statelog/metrics"
)

// THE SCRUB: EVERY BYTE A USED COPY HOLDS IS HELD TO ITS CHECKSUM, BEHIND THE
// SERVING (doc.go, "What this process did not write is checked before it
// serves, and every byte is scrubbed behind it"). The check a full round makes
// before it answers (check.go) reads the index files alone, and only those
// this process has neither written nor checked, so two kinds of damage pass
// it. A PACK BODY: on a copy the maintainer has served from, upload-pack's
// pack-objects copies a stored object to the client as it is stored, byte
// for byte — damaged bytes from any pack, and a damaged entry header from the
// bitmap's preferred pack, which single reuse (ServeConfig) sends without
// reading it, where git names the header of any other pack's entry — and the
// commit-graph spares every walk the commit's parse, so no git process on the
// node names it while every clone of it fails at the client (measured on the
// node pin and the floor).
// And damage that arrives in a file AFTER this process vouched for it with no
// change of the file's identity — media damage — which no vouch can see: a
// bitmap damaged so fails every clone, upload-pack exiting 0 and saying
// nothing. Holding the packs before a copy first answers would hold the
// largest repository's first request for as long as it takes to read twenty
// gigabytes, so a SCRUB reads them behind the serving instead — each .pack
// against its own trailer, which is its name, and every index file by the
// check's rule, vouched or not:
//
//   - ONCE after an incarnation's first full round in this process, when a
//     previous process left it (incarnation.inherited): its packs are bytes no
//     process of this run held to their rows. One this process built placed
//     every file after holding it to its row, so its interval starts at that
//     round instead;
//   - every ScrubInterval while the repository is USED — a reader took it,
//     let it go or made a push's scratch over it since its bytes were last
//     held to their checksums — and never for one nobody reads;
//   - AT ONCE when a git process over it reports a pack or an object it could
//     not read, names a pack in words, or crashes (report.go), bounded by
//     ScrubCooldown after a scrub that found nothing — unless the process
//     was one whose failure its reader repairs, a request's or a round's own
//     (roundReader), which holds the report for that reader's own repair:
//     that repair is what scrubs, for it (scrubFor), if its in-place round
//     does not end the damage.
//
// ONE SCRUB ON THE NODE AT A TIME (scrubSlot), given BY EVIDENCE: a FAILING
// REPAIR's (scrubFor: what the in-place round of a request's or a round's own
// pack damage did not end, rebuild.go) before anything the scrubber has due,
// then a REPORT's, then a copy's FIRST, then an INTERVAL's — within each the
// oldest asked first, the interval's the least recently held to its
// checksums. Evidence PREEMPTS routine work: a repair or a report that finds
// a first or an interval scrub running cancels it, and that scrub is due
// again as it was, so a node just restarted, with every inherited copy's
// first scrub queued, reads a failing copy next rather than after a terabyte
// of routine reads. A repair's scrub waits only for a report's or another
// repair's scrub in flight, and the scrubber takes nothing while one waits: a
// mutex let go and taken again by the scrubber's next scrub handed itself to
// a waiting repair only one scrub later, since Go's mutex wakes a waiter
// without handing it the lock. Each hashes a piece at a time under one slot of the
// read lane (scrubHold), through one buffer of its own. It holds NO LOCK of
// the entry, so the copy keeps answering and rounds and the maintainer run
// beside it: a verdict holds only for the file it hashed (trailerSound), and
// its PIN — which keeps the budget from evicting the copy and a retire from
// moving it under the scrub — is no USE, so a scrub never makes the next one
// due. It repairs nothing itself: each file that fails goes on the
// incarnation's repair mark at the identity it failed at, and the repair
// round (report.go) reinstalls a pack in place with one GET, or drops a
// derived file to be written again at once — only while the file still has
// that identity, and never under a check of the repository.

// scrubWhy is why an incarnation's scrub is due AT ONCE; zero when only its
// interval makes it due.
type scrubWhy uint8

const (
	// scrubFirst: its first full round in this process has run, and a
	// previous process left it.
	scrubFirst scrubWhy = iota + 1
	// scrubReport: a git process over it reported a pack or an object it
	// could not read, named a pack in words, or crashed.
	scrubReport
)

// trigger is the `trigger` a scrub made due for w is counted and logged
// under: `first` (as the first round's check is), `report`, or `scrub` for
// one its interval made due.
func (w scrubWhy) trigger() checkTrigger {
	switch w {
	case scrubFirst:
		return triggerFirst
	case scrubReport:
		return triggerReport
	}
	return triggerScrub
}

// prio is the priority a scrub made due for w is given in.
func (w scrubWhy) prio() scrubPrio {
	switch w {
	case scrubFirst:
		return prioFirst
	case scrubReport:
		return prioReport
	}
	return prioInterval
}

// scrubPrio is how a scrub is asked for, weakest first: the order the node's
// one scrub is given in, and what preempts what (scrubSlot).
type scrubPrio uint8

const (
	// prioInterval: a used copy's interval has passed.
	prioInterval scrubPrio = iota
	// prioFirst: a copy a previous process left has had its first round.
	prioFirst
	// prioReport: a git process over the copy reported a pack or an object
	// it could not read, named a pack in words, or crashed.
	prioReport
	// prioRequest: a pack's or an object's damage failed a request on the
	// copy, or a round's own git, and the in-place round of its repair did
	// not end it (scrubFor).
	prioRequest
)

// routine reports whether a scrub at p is one the cache makes on its own
// account, with no evidence of damage: what evidence preempts.
func (p scrubPrio) routine() bool { return p < prioReport }

// errPreempted ends a routine scrub that a scrub asked by evidence of damage
// — a request's, a report's — preempted (scrubSlot).
var errPreempted = errors.New("cache: a routine scrub was preempted by one evidence of damage asked for")

// scrubSlot is the node's ONE scrub: whether it is held, at what priority and
// how to end it, and how many requests wait for it. A mutex would serialise
// the scrubs and nothing more — no order, no preemption — and a mutex let go
// and taken again at once is taken again by the same goroutine: Go wakes a
// waiter without handing it the lock.
type scrubSlot struct {
	mu     sync.Mutex
	held   bool
	prio   scrubPrio
	cancel context.CancelCauseFunc
	// asking counts the requests waiting for the slot: while any does,
	// nothing else takes it.
	asking int
	// changed is closed, and replaced, whenever the slot is given back or a
	// waiting request gives up.
	changed chan struct{}
}

// newScrubSlot is a free slot.
func newScrubSlot() *scrubSlot { return &scrubSlot{changed: make(chan struct{})} }

// take waits for the slot — and, for anything but a request, for every
// request waiting for it — preempting a routine scrub that holds it when prio
// is evidence's. It answers the context the scrub runs under, which ctx, a
// preemption (errPreempted) and the giving back each end, and how to give the
// slot back; or ctx's end.
func (s *scrubSlot) take(ctx context.Context, prio scrubPrio) (context.Context, func(), error) {
	s.mu.Lock()
	if prio == prioRequest {
		s.asking++
	}
	for {
		if !s.held && (prio == prioRequest || s.asking == 0) {
			if prio == prioRequest {
				s.asking--
			}
			sctx, cancel := context.WithCancelCause(ctx)
			s.held, s.prio, s.cancel = true, prio, cancel
			s.mu.Unlock()
			return sctx, sync.OnceFunc(func() {
				cancel(nil)
				s.mu.Lock()
				s.held, s.cancel = false, nil
				s.broadcast()
				s.mu.Unlock()
			}), nil
		}
		if s.held && s.prio.routine() && !prio.routine() {
			s.cancel(errPreempted)
		}
		changed := s.changed
		s.mu.Unlock()
		select {
		case <-changed:
		case <-ctx.Done():
			s.mu.Lock()
			if prio == prioRequest {
				s.asking--
				s.broadcast()
			}
			s.mu.Unlock()
			return nil, nil, context.Cause(ctx)
		}
		s.mu.Lock()
	}
}

// raise is the priority of the scrub holding the slot, now that its holder
// knows what it scrubs: the scrubber takes the slot before it picks.
func (s *scrubSlot) raise(prio scrubPrio) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.held {
		s.prio = prio
	}
}

// preempt ends a routine scrub holding the slot: evidence of damage has made
// another scrub due, which the scrubber takes next. The ended scrub is due
// again as it was (runScrub).
func (s *scrubSlot) preempt() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.held && s.prio.routine() {
		s.cancel(errPreempted)
	}
}

// broadcast wakes everything waiting for the slot; s.mu is held.
func (s *scrubSlot) broadcast() {
	close(s.changed)
	s.changed = make(chan struct{})
}

// kickScrubber wakes the scrubber without waiting for it.
func (c *Cache) kickScrubber() {
	select {
	case c.scrubKicks <- struct{}{}:
	default:
	}
}

// scrubber runs every scrub due whenever it is kicked and when the earliest
// interval falls due, until ctx ends or the cache stops. Between passes it is
// parked, holding nothing of the directory. The first use of a copy since its
// last scrub — a reader taking it, letting it go, or making a push's scratch
// over it — kicks it (used), so its pass knows when that copy's interval ends
// and wakes then; so does a report (report.go) and an inherited copy's first
// full round (firstScrub). A copy nobody uses is never due, and only those
// say it has become so.
func (c *Cache) scrubber(ctx context.Context) {
	timer := time.NewTimer(time.Hour)
	timer.Stop()
	defer timer.Stop()
	for {
		if !c.enter() {
			return
		}
		// A kick sent before this pass reads the entries is answered by it.
		select {
		case <-c.scrubKicks:
		default:
		}
		_, next := c.scrubPass(ctx)
		c.leave()
		var wake <-chan time.Time
		switch {
		case next.IsZero():
		case c.knobs != nil && c.knobs.scrubWake != nil:
			wake = c.knobs.scrubWake(next)
		default:
			timer.Reset(max(next.Sub(c.now()), 0))
			wake = timer.C
		}
		select {
		case <-ctx.Done():
			return
		case <-c.life.Done():
			return
		case <-c.scrubKicks:
		case <-wake:
		}
	}
}

// scrubPass runs every scrub due now, one after another, and answers the
// repositories it scrubbed, in order, and when the next interval falls due —
// zero when no copy waits on one. Each takes the node's one scrub (scrubSlot)
// before it picks what to scrub, so it never takes it while a request's waits,
// and a scrub a report preempted is picked again after the report's.
func (c *Cache) scrubPass(ctx context.Context) ([]string, time.Time) {
	var done []string
	for ctx.Err() == nil && c.life.Err() == nil {
		sctx, give, err := c.scrubs.take(ctx, prioInterval)
		if err != nil {
			return done, time.Time{}
		}
		e, inc, why, next := c.nextScrub()
		if e == nil {
			give()
			return done, next
		}
		c.scrubs.raise(why.prio())
		_, err = c.runScrub(sctx, e, inc, why.trigger(), why)
		give()
		if !errors.Is(err, errPreempted) {
			done = append(done, e.id)
		}
	}
	return done, time.Time{}
}

// scrubCandidate is a current incarnation whose scrub is due, with the order
// it is taken in: by evidence — a report's, a first's, an interval's — and
// within each the oldest asked first, the interval's by when each was last
// held to its checksums.
type scrubCandidate struct {
	e     *entry
	inc   *incarnation
	why   scrubWhy
	since time.Time
}

// nextScrub takes the next incarnation due a scrub — pinned as no use, marked
// scrubbing, its due-at-once mark taken — and answers why it is due; or nil,
// and when the next interval falls due (zero when none waits on one).
func (c *Cache) nextScrub() (*entry, *incarnation, scrubWhy, time.Time) {
	now := c.now()
	var (
		due  []scrubCandidate
		next time.Time
	)
	c.mu.Lock()
	for _, e := range c.entries {
		e.mu.Lock()
		if cand, ok := scrubbable(e); ok {
			switch at := cand.inc.scrubBase.Add(ScrubInterval); {
			case cand.why != 0:
				cand.since = cand.inc.scrubAsked
				due = append(due, cand)
			case cand.inc.scrubBase.IsZero() || !cand.inc.lastUsed.After(cand.inc.scrubBase):
				// Not used since its bytes were last held to their
				// checksums: never due until it is, and its first use
				// since kicks the scrubber (used), whose next pass arms
				// the wake below for its interval's end.
			case !at.After(now):
				cand.since = cand.inc.scrubBase
				due = append(due, cand)
			case next.IsZero() || at.Before(next):
				next = at
			}
		}
		e.mu.Unlock()
	}
	c.mu.Unlock()
	slices.SortFunc(due, func(a, b scrubCandidate) int {
		return cmp.Or(cmp.Compare(b.why.prio(), a.why.prio()), a.since.Compare(b.since),
			strings.Compare(a.e.id, b.e.id))
	})
	for _, cand := range due {
		e := cand.e
		e.mu.Lock()
		if cur, ok := scrubbable(e); ok && cur.inc == cand.inc && (cur.why != 0) == (cand.why != 0) {
			inc := cur.inc
			// Its due mark taken, its asking kept: a scrub preempted is due
			// again as it was, as old as it was (runScrub).
			inc.scrubbing, inc.scrubRunning, inc.scrubDue = true, cur.why.prio(), 0
			inc.pins++
			e.mu.Unlock()
			return e, inc, cur.why, time.Time{}
		}
		e.mu.Unlock()
	}
	return nil, nil, 0, next
}

// scrubbable is e's current incarnation as a candidate for a scrub, when it is
// one: in service, its first full round of this process run (the check before
// it answered held its indexes), and no scrub of it running. e.mu is held.
func scrubbable(e *entry) (scrubCandidate, bool) {
	inc := e.current
	if e.dead || inc == nil || inc.retired || !inc.checkedOnce || inc.scrubbing {
		return scrubCandidate{}, false
	}
	return scrubCandidate{e: e, inc: inc, why: inc.scrubDue}, true
}

// firstScrub is what an incarnation's first full round in this process asks
// of the scrub, once that round has converged it: an inherited copy's first
// scrub, due at once; and of one this process built — every file of which it
// placed after holding it to its row — its interval, from now. e.mu is held.
func (c *Cache) firstScrub(inc *incarnation) {
	if !inc.scrubBase.IsZero() || !inc.scrubbedAt.IsZero() || inc.scrubDue != 0 || inc.scrubbing {
		return
	}
	if !inc.inherited {
		inc.scrubBase = c.now()
		return
	}
	inc.scrubDue, inc.scrubAsked = scrubFirst, c.now()
	c.kickScrubber()
}

// scrubAsked makes inc's scrub due at once for a report — unless a scrub of
// it asked by evidence is running or due, which answers the report; or one
// ended within ScrubCooldown having found nothing, or whose findings still
// wait for their repair — wakes the scrubber, preempting a routine scrub it is
// running, and answers the report's outcome. e.mu is held.
//
// A REPORT RAISES A ROUTINE SCRUB OF THE COPY to its own: one only due for
// the copy's first round or its interval is due for the report now, taken
// ahead of every routine one, and one RUNNING is ended and runs again as the
// report's — the copy's first scrub reads no index file this process vouched
// for, which a report of a crash may be about, and a routine scrub in flight
// is what evidence preempts anyway. Left as "already due", a failing copy's
// report waited its turn behind every first scrub a restart had queued.
//
// THE COOLDOWN HOLDS WHATEVER IS WRITTEN MEANWHILE: a push, an adoption or a
// maintenance run does not reset it, since a request that can provoke a
// report — a git that crashes, a hint in words — beside a busy repository
// would otherwise be bounded by nothing; and a report while the copy's scrub
// runs is that scrub's, which hashes every file anyway. IT HOLDS AFTER A SCRUB
// WHOSE FINDINGS STILL WAIT, TOO (`covered`): a repair round held behind
// another, or one whose download the object store refused (its pack held off,
// install.go), leaves them on the mark, and every crash of a clone over the
// damage they name would otherwise scrub the whole copy again — about 24 GB
// for the largest repository the defaults admit — to find the same files.
// The repair is the answer to those reports; past the cooldown a report scrubs
// again, at most once a ScrubCooldown, so damage that landed elsewhere in the
// copy meanwhile is not left to the interval. Only the SCRUB's findings cover
// a report (scrubWaits): what a round's check found since waits on the same
// mark, but a check reads no pack body, so its finding says nothing of the
// bytes a report may be about — and a check that finds one of the scrub's
// files failing again, at the identity the scrub found it at, leaves it the
// scrub's (recordCheck), so the forced round of a request failing on that
// very damage uncovers nothing.
func (c *Cache) scrubAsked(inc *incarnation) reportOutcome {
	now := c.now()
	recent := !inc.scrubbedAt.IsZero() && now.Sub(inc.scrubbedAt) < ScrubCooldown
	switch {
	case inc.scrubbing && !inc.scrubRunning.routine(), inc.scrubDue == scrubReport:
		return reportQueued
	case recent && !inc.scrubFound:
		return reportCooldown
	case recent && inc.repair.scrubWaits():
		return reportCovered
	}
	inc.scrubDue, inc.scrubAsked = scrubReport, now
	c.kickScrubber()
	c.scrubs.preempt()
	return reportQueued
}

// runScrub runs one scrub of inc, pinned and marked scrubbing by its caller,
// under sctx — the node's scrub slot's context (scrubSlot) — and records it:
// when it began and ended, whether it found anything, the packs it found
// whole, and what it found on the repair mark, with a repair round asked. A
// scrub the cache's stop, its caller's context or a preemption ended records
// nothing and leaves the copy due for why as it was (none for a request's),
// and answers the context's cause; one that could not list a file records
// what it read, so the cooldown and the interval bound how soon it runs again,
// and says so.
func (c *Cache) runScrub(sctx context.Context, e *entry, inc *incarnation, trigger checkTrigger,
	why scrubWhy) (scrubbed, error) {

	ctx, cancel := context.WithCancelCause(sctx)
	//nolint:contextcheck // The cache's lifetime ends the scrub too: Stop must end its reads however long the pass's ctx lives.
	stop := context.AfterFunc(c.life, func() { cancel(ErrStopped) })
	began := time.Now()
	out, err := c.scrub(ctx, e, inc, trigger)
	stop()
	ended := ctx.Err() != nil
	cause := context.Cause(ctx)
	cancel(nil)

	// A SWAP WHILE THE SCRUB READ linked the files it read into the next
	// incarnation (swap.go): its verdict is that incarnation's — the one
	// serving them now — for every file it holds as the same inode. Dropped
	// with the old one, a scrub of a repository compacted more often than a
	// scrub of it lasts never landed, and was due again at once, for ever.
	e.mu.Lock()
	inc.scrubbing = false
	at, atDir := serving(inc), ""
	if at != inc {
		atDir = at.dir
	}
	e.mu.Unlock()
	same := map[string]bool{}
	for _, f := range out.damaged {
		if atDir == "" || lstatID(filepath.Join(atDir, f.rel)) == f.id {
			same[f.rel] = true
		}
	}
	e.mu.Lock()
	switch {
	case serving(inc) != at:
		// Swapped again while the findings were matched: the next scrub
		// reads the copy that serves now.
	case ended:
		if at.scrubDue == 0 && why != 0 {
			at.scrubDue = why
		}
	case e.current == at && !e.dead && !at.retired:
		now := c.now()
		whole := out.whole
		if at != inc {
			whole = slices.DeleteFunc(slices.Clone(whole), func(cs string) bool {
				_, live := at.byChecksum[cs]
				return !live
			})
		}
		at.scrubbedAt, at.scrubBase, at.scrubFound = now, now, len(same) > 0
		at.scrubBegan, at.scrubEnded, at.scrubWhole = began, time.Now(), whole
		at.scrubAsked = time.Time{}
		for _, f := range out.damaged {
			if !same[f.rel] {
				continue
			}
			if at.repair.found == nil {
				at.repair.found = map[string]checkedFile{}
			}
			at.repair.found[f.rel] = f
		}
		if len(same) > 0 {
			// Repaired by the incarnation's next full round — never under a
			// check of the repository, whose release asks it (report.go).
			c.repairRound(e)
		}
	}
	// What it vouched for and found is what the backfill's look at the copy
	// reads of its runs (schedule.go), however the scrub ended.
	c.backfillDirty(e)
	gone := c.unpin(e, inc)
	e.mu.Unlock()
	c.trash(gone)
	switch {
	case c.life.Err() != nil:
		return out, ErrStopped
	case ended:
		return out, cause
	case err != nil:
		c.log.Warn("code_cache_scrub_failed", "repo", e.id, "incarnation", inc.n, "error", err)
	}
	return out, nil
}

// scrubFor runs a scrub of inc for the callers of a pack's or an object's
// repair that its in-place round did not end (Cache.escalate, step 3: a
// request's whose forced round repaired nothing, rebuild.go, and a sync's
// whose round's own git met the damage again or skipped that round,
// repairDamaged) and whose failing processes began at or before began (zero:
// unknown) — the node's one scrub, taken ahead of every scrub the scrubber
// has due and preempting a routine one it is running (scrubSlot) — and
// answers what it found: its findings, on the repair mark already with a
// repair round asked, and the packs it found whole. What another scrub of inc
// already said answers instead, unread again: findings it made that still
// wait for their repair, which is the answer; or, when it found nothing, its
// verdict, when it began after every failure it is asked about (it read the
// bytes they failed on) or ended within ScrubCooldown with no repair written
// into inc since (the same bytes would be found whole again) — either one
// with the packs that scrub found whole less every pack a finding still
// waiting names (what a rebuild past their repair links, seedRebuild), and
// beside it what a round's check found since that waits too (recordCheck).
// Nothing at all for an incarnation no longer current; ErrStopped when the
// cache stops.
//
// It takes no context: this scrub runs on the cache's lifetime, never a
// caller's, since every caller of the round that asks it shares its answer
// and one leaving must not end it for the rest.
//
//nolint:contextcheck // the scrub slot's context is derived from the cache's lifetime (above)
func (c *Cache) scrubFor(e *entry, inc *incarnation, began time.Time) (scrubbed, error) {
	sctx, give, err := c.scrubs.take(c.life, prioRequest)
	if err != nil {
		return scrubbed{}, ErrStopped
	}
	defer give()
	// No scrub of the copy runs: the scrubber marks one scrubbing only
	// while it holds the node's one scrub, which this request now does.
	e.mu.Lock()
	recent := !inc.scrubbedAt.IsZero() && !inc.scrubFound &&
		((!began.IsZero() && inc.scrubBegan.After(began)) ||
			(c.now().Sub(inc.scrubbedAt) < ScrubCooldown && !inc.repairWrite.After(inc.scrubEnded)))
	switch {
	case e.dead || e.current != inc || inc.retired:
		e.mu.Unlock()
		return scrubbed{}, nil
	case inc.repair.scrubWaits(), recent:
		// WHAT THE LAST SCRUB SAID ANSWERS: its findings still waiting, or
		// its verdict of nothing found — and BESIDE IT, WHAT A ROUND'S CHECK
		// HAS FOUND SINCE that waits on the same mark, an index damaged in
		// place after the scrub read it (recordCheck), whose repair is the
		// answer as the scrub's findings' is. A check's finding alone answers
		// nothing here: the scrub is asked for the pack bodies a check never
		// reads. Nor does a check that finds a scrub's file failing again make
		// it the check's: the forced round of the very request this answers
		// held every index file to its trailer and met the scrub's files as
		// they were, and the scrub's findings stayed the scrub's (recordCheck)
		// — taken for the check's, they answered nothing here, and the request
		// read the whole copy again to find the files the mark already named.
		// THE PACKS THE LAST SCRUB FOUND WHOLE answer with them, less
		// every pack a finding still waiting names — the scrub's or a
		// check's: what a rebuild past the findings' repair links rather
		// than downloads (seedRebuild), exactly as when that scrub ran for
		// the caller. Answered with no packs, the rebuild a round's caller
		// asks once the findings' round still meets the damage
		// (repairDamaged's step 4) downloaded every pack the repository
		// holds; answered with a pack a check found damaged since, it linked
		// that damage into the copy that replaced the damaged one.
		var out scrubbed
		for _, f := range inc.repair.found {
			out.damaged = append(out.damaged, f)
		}
		out.whole = slices.DeleteFunc(slices.Clone(inc.scrubWhole), func(cs string) bool {
			for _, f := range inc.repair.found {
				if have, ok := packOfRel(f.rel); ok && have == cs {
					return true
				}
			}
			return false
		})
		e.mu.Unlock()
		return out, nil
	}
	// Every file, vouched or not: it answers whatever scrub of the copy was
	// due, which is due again only if it is ended first.
	why := inc.scrubDue
	inc.scrubbing, inc.scrubRunning, inc.scrubDue = true, prioRequest, 0
	inc.pins++
	e.mu.Unlock()
	return c.runScrub(sctx, e, inc, triggerRequest, why)
}

// scrubbed is what one scrub found: every file it found failing, at the
// identity it failed at, how many files were sound, the packs whose three
// files it found sound, by checksum (what a rebuild may link rather than
// download, seedRebuild), and the bytes read.
type scrubbed struct {
	damaged []checkedFile
	sound   int
	whole   []string
	bytes   int64
}

// scrub hashes every file inc's directory holds that the image names — each
// pack's .idx, .pack and .rev where all three are there (a pack missing one is
// the next full round's to install), and every derived file present — one
// piece at a time under a read slot (scrubHold), vouched or not, and answers
// what failed. A file with no verdict (replaced or removed while it was read)
// is neither sound nor damaged; one whose READ fails is damaged at the
// identity it had (`unreadable`), and goes on the mark like one that failed
// its checksum: a bad sector under a file is what a repair replaces. What
// passes is vouched for again. An error is the listing's, or the context's.
//
// A copy's FIRST scrub hashes no index file this process has vouched for: its
// first round held every one to its trailer minutes earlier, or this process
// wrote it, and what the first scrub is for is the packs a previous process
// left (a fifth of the bytes it would read again for nothing). Every other
// scrub reads them vouched or not — damage that lands after a vouch is what
// it is for.
func (c *Cache) scrub(ctx context.Context, e *entry, inc *incarnation, trigger checkTrigger) (scrubbed, error) {
	var out scrubbed
	began := time.Now()
	e.mu.Lock()
	chains, mark, tip := chainWants(inc), markChain(inc), recordedTip(inc)
	e.mu.Unlock()
	files, wants, err := scrubFiles(c.image, inc.dir, chains)
	if err != nil {
		return out, fmt.Errorf("listing %s: %w", inc.dir, err)
	}
	defer func() {
		if c.metrics != nil && out.bytes > 0 {
			c.metrics.Observe(metrics.CodeCacheCheckDuration, time.Since(began),
				metrics.Attrs{"trigger": string(trigger)})
		}
	}()
	p := c.pieces(c.scrubHold)
	// soundOf counts each pack's files found sound: three is a pack whole.
	soundOf := map[string]int{}
	for _, f := range files {
		path := filepath.Join(inc.dir, f.rel)
		if trigger == triggerFirst && f.kind != filePack {
			e.mu.Lock()
			skip := vouchedAt(inc, f.rel, lstatID(path))
			e.mu.Unlock()
			if skip {
				if cs, ok := packOfRel(f.rel); ok {
					soundOf[cs]++
				}
				continue
			}
		}
		if c.knobs != nil && c.knobs.hashing != nil {
			c.knobs.hashing(path)
		}
		v, id, n, err := trailerSound(ctx, path, p, wants[f.rel])
		// A chain file the node's tip or a cut wrote in place while the
		// scrub ran — the maintainer's runs go on beside a scrub — is no
		// verdict, its record and its bytes of two moments (chainVerdict).
		v = chainVerdict(e, inc, wants[f.rel], mark, v)
		out.bytes += n
		f.id = id
		switch {
		case err != nil && ctx.Err() != nil:
			return out, context.Cause(ctx)
		case err != nil:
			c.countCheck(f.kind, checkUnreadable, trigger)
			f.trigger, f.scrubbed = trigger, true
			out.damaged = append(out.damaged, f)
			c.log.Warn("code_cache_check_damaged", "repo", e.id, "incarnation", inc.n, "file", f.rel,
				"trigger", string(trigger), "action", c.image.repairAction(f.rel, tip), "error", err)
			continue
		case v == hashNone:
			c.countCheck(f.kind, checkReplaced, trigger)
			continue
		case v == hashSound:
			c.countCheck(f.kind, checkSound, trigger)
			out.sound++
			if m := packFileName.FindStringSubmatch(filepath.Base(f.rel)); m != nil {
				soundOf[m[1]]++
			}
			e.mu.Lock()
			vouch(inc, f.rel, id)
			e.mu.Unlock()
			continue
		}
		c.countCheck(f.kind, checkDamaged, trigger)
		f.trigger, f.scrubbed = trigger, true
		out.damaged = append(out.damaged, f)
		c.log.Warn("code_cache_check_damaged", "repo", e.id, "incarnation", inc.n, "file", f.rel,
			"trigger", string(trigger), "action", c.image.repairAction(f.rel, tip))
	}
	for cs, n := range soundOf {
		if n == 3 {
			out.whole = append(out.whole, cs)
		}
	}
	slices.Sort(out.whole)
	return out, nil
}

// scrubFiles is every file of the incarnation at dir a scrub hashes, sorted by
// its path relative to the incarnation, with what its name says of its last
// forty bytes: each pack's three files where all three are there — its .pack
// held to the trailer that is its name, its .idx and .rev to their pack's
// checksum before their own — and every derived file present, held as its
// declaration says (derived.go): a chain file to what chains says it holds
// (chainWants), the ids the incarnation installed.
func scrubFiles(im *image, dir string, chains map[string][]byte) ([]checkedFile, map[string]expect, error) {
	list, err := os.ReadDir(filepath.Join(dir, packRel))
	if err != nil {
		return nil, nil, err
	}
	held := map[string]int{}
	for _, d := range list {
		if m := packFileName.FindStringSubmatch(d.Name()); m != nil && d.Type().IsRegular() {
			held[m[1]]++
		}
	}
	var files []checkedFile
	wants := map[string]expect{}
	for cs, n := range held {
		if n != 3 {
			continue
		}
		sum := checksumBytes(cs)
		for _, f := range []struct {
			kind fileKind
			ext  string
			want expect
		}{{fileIdx, ".idx", expect{prior: sum}}, {filePack, ".pack", expect{trailer: sum}},
			{fileRev, ".rev", expect{prior: sum}}} {
			rel := packFileRel(cs, f.ext)
			files = append(files, checkedFile{kind: f.kind, rel: rel})
			wants[rel] = f.want
		}
	}
	present, err := im.present(dir)
	if err != nil {
		return nil, nil, err
	}
	derived, err := heldFiles(present, wants, chains)
	if err != nil {
		return nil, nil, err
	}
	files = append(files, derived...)
	slices.SortFunc(files, func(a, b checkedFile) int { return strings.Compare(a.rel, b.rel) })
	return files, wants, nil
}

// scrubHold takes a slot of the read lane for one piece of a scrub, WAITING ITS
// TURN HOWEVER LONG: no caller waits on a scrub, so where a request is refused
// busy after gitproc.SlotWait (gitproc.ErrBusy), the scrub queues again — in
// arrival order, behind whatever came meanwhile and never ahead of it — until
// its context ends.
func (c *Cache) scrubHold(ctx context.Context) (func(), error) {
	for {
		release, err := c.git.HoldRead(ctx)
		if !errors.Is(err, gitproc.ErrBusy) {
			return release, err
		}
	}
}

// settleFound takes off inc's repair mark every finding of found — a check's
// or a scrub's, as the round's check answered them (recordCheck) — a full
// round has repaired, or found already repaired, once its install and its
// drops have acted on them: each still on the mark at the identity it failed
// at; a newer finding about the same file stays, and so does one of a pack in
// unrepaired, whose download failed or is held off. A round that fails before
// then leaves them all for the next.
func (c *Cache) settleFound(e *entry, inc *incarnation, found []checkedFile, unrepaired map[string]bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	for _, f := range found {
		cs, isPack := packOfRel(f.rel)
		if isPack && unrepaired[cs] {
			continue
		}
		if have, ok := inc.repair.found[f.rel]; ok && have.id == f.id {
			delete(inc.repair.found, f.rel)
			if isPack {
				delete(inc.repair.fetchFailed, cs)
			}
		}
	}
}
