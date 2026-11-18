package cache

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/crewlet/crewlet/internal/gitlet/gitproc"
	"github.com/crewlet/crewlet/internal/statelog/metrics"
)

// The scrub holds every byte of a used copy to its checksum behind the
// serving: the pack bodies the check (check.go) never reads, and media damage
// landing after a vouch (package doc, "What this process did not write is
// checked before it serves, and every byte is scrubbed behind it"). It runs
// once after an inherited copy's first full round, every ScrubInterval while
// the copy is used, and at once on a report (report.go) bounded by
// ScrubCooldown, except for a process whose own reader repairs it
// (roundReader): that repair scrubs, if its in-place round fails (scrubFor).
// One scrub on the node at a time, by evidence (scrubSlot). It holds no lock
// of the entry, its pin is no use, and it repairs nothing itself: findings go
// on the repair mark for the repair round.

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
// how to end it, and how many requests wait for it. Not a mutex: a mutex
// gives no order and no preemption, and one let go and taken again at once
// goes to the same goroutine, since Go wakes a waiter without handing it the
// lock.
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
// interval falls due, until ctx ends or the cache stops, parked between passes
// holding nothing of the directory. A copy nobody uses is never due: its first
// use since its last scrub kicks it (used), so the pass arms that copy's
// interval; so do a report (report.go) and an inherited copy's first full
// round (firstScrub).
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
				// Unused since last scrubbed: never due until its first
				// use kicks the scrubber (used), whose next pass arms the
				// wake below.
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

// scrubAsked makes inc's scrub due at once for a report, ahead of every
// routine one and preempting one running (a first scrub skips the vouched
// index files a crash may be about), and answers the outcome — unless a scrub
// asked by evidence is running or due (it answers the report), or one ended
// within ScrubCooldown having found nothing (`cooldown`) or with findings
// still waiting for their repair (`covered`: the repair answers, rather than
// every crash over that damage rescanning the whole copy). Writes do not reset
// the cooldown, or a request provoking reports beside a busy repository would
// be bounded by nothing. Only the scrub's findings cover (scrubWaits): a check
// reads no pack body, and one refinding a scrub's file leaves it the scrub's
// (recordCheck). e.mu is held.
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

	// A swap while the scrub read linked its files into the next incarnation
	// (swap.go): the verdict is the serving one's for every file it holds as
	// the same inode, or a repository compacted faster than a scrub lasts
	// would never record one.
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
	// What it found is what the backfill's look at the copy reads of its runs
	// (schedule.go), however the scrub ended: marked in the hold that records
	// it, as each vouch was in its own.
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

// scrubFor runs a scrub of inc at prioRequest (scrubSlot) for the callers of
// a pack's or an object's repair whose in-place round did not end the damage
// (Cache.escalate step 3, repairDamaged) and whose failing processes began at
// or before began (zero: unknown), and answers its findings (on the repair
// mark, a repair round asked) and the packs it found whole. The last scrub's
// answer is reused unread where it still holds (the case below); nothing for
// an incarnation no longer current; ErrStopped when the cache stops. It takes
// no context: it runs on the cache's lifetime, since every caller of the round
// shares its answer and one leaving must not end it for the rest.
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
		// The last scrub answers: its findings still waiting, with what a
		// round's check found since on the same mark (recordCheck); or its
		// verdict of nothing found, when it began after every failure asked
		// about or ended within ScrubCooldown with no repair written since. A
		// check's finding alone answers nothing: the scrub is asked for pack
		// bodies a check never reads. The packs it found whole answer too,
		// less every pack a waiting finding names: what a rebuild past the
		// repair links rather than downloads (seedRebuild). Answered with
		// none, repairDamaged's step 4 rebuild would download every pack;
		// with one a check found damaged since, it would link that damage in.
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

// scrub hashes every file of inc's directory the image names — each pack's
// .idx, .pack and .rev where all three are there (a pack missing one is the
// next full round's to install), and every derived file present — a piece at
// a time under a read slot (scrubHold), and answers what failed. A file
// replaced or removed while read has no verdict; one whose read fails is
// damaged (`unreadable`), since a bad sector is what a repair replaces. What
// passes is vouched for again. An error is the listing's or the context's.
// Every scrub reads vouched files too (damage after a vouch is what it is
// for), except a copy's FIRST, which skips vouched index files: its first
// round checked them minutes earlier, and it is for the packs a previous
// process left.
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
			if m := packFileName().FindStringSubmatch(filepath.Base(f.rel)); m != nil {
				soundOf[m[1]]++
			}
			e.mu.Lock()
			vouch(inc, f.rel, id)
			if c.backfillReads(f.rel) {
				c.backfillDirty(e)
			}
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
// forty bytes: each pack's three files where all three are there — in
// objects/pack and in the dropping view alike, since a damaged dropping pack
// is sent to a client still reading it as a live one is — its .pack held to
// the trailer that is its name, its .idx and .rev to their pack's checksum
// before their own; and every derived file present, held as its declaration
// says (derived.go): a chain file to what chains says it holds (chainWants),
// the ids the incarnation installed.
func scrubFiles(im *image, dir string, chains map[string][]byte) ([]checkedFile, map[string]expect, error) {
	var files []checkedFile
	wants := map[string]expect{}
	for _, place := range []packPlace{placeLive, placeDropping} {
		list, err := im.listed(dir, place.rel())
		if err != nil {
			return nil, nil, err
		}
		held := map[string]int{}
		for _, d := range list {
			if m := packFileName().FindStringSubmatch(d.Name()); m != nil && d.Type().IsRegular() {
				held[m[1]]++
			}
		}
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
				rel := packFileRel(place, cs, f.ext)
				files = append(files, checkedFile{kind: f.kind, rel: rel})
				wants[rel] = f.want
			}
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
			if c.backfillReads(f.rel) {
				// A run's failure settled: the backfill's look reads it
				// (runMarks).
				c.backfillDirty(e)
			}
		}
	}
}
