package cache

import (
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/crewlet/crewlet/internal/gitlet"
	"github.com/crewlet/crewlet/internal/gitlet/gitproc"
	"github.com/crewlet/crewlet/internal/objstore"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/words"
)

// VERIFY: an operator's question — does this node's cache of a repository
// hold exactly what its rows say? — answered by reading every byte of it
// against the rows and, when asked, repaired and checked again. Not a hot
// path: it hashes every pack and runs git's connectivity check.
//
// A check is ADMITTED ([Cache.AdmitVerify]) before it reads a byte — a free
// maintenance slot and its fsck's memory taken, or refused at once — so a
// refusal costs nothing and the lane bounds how many run; and only of a copy
// this node HOLDS ([ErrNotHeld]): checking a copy it lacks would download the
// whole repository into the cache and evict warm ones to keep it (package
// doc, "Damage is repaired once, in place where nothing is lost").

// VerifyReport is what Verify found in one incarnation of a repository.
type VerifyReport struct {
	// Repo is the repository's id, and Name its name.
	Repo string `json:"repo"`
	Name string `json:"name"`
	// Position is the one the incarnation was checked at: every row up to
	// here is what it was held to.
	Position statelog.Position `json:"position"`
	// Packs is how many packs were checked against their rows, and Refs how
	// many refs packed-refs holds.
	Packs int `json:"packs"`
	Refs  int `json:"refs"`
	// RepairedBySync is what the sync before the check found failing by its
	// own check and repaired in place (a derived file dropped, a pack fetched
	// again), each a sentence naming the file: no problem of the checked copy
	// and nothing a repair rebuilds. Shown first: a disk that damaged one file
	// may hold others damaged.
	RepairedBySync []string `json:"repaired_by_sync,omitempty"`
	// Problems is everything the incarnation holds that its rows do not say,
	// each a sentence naming the file and both values; none is a cache
	// exactly its rows.
	Problems []string `json:"problems,omitempty"`
	// RepairedInPlace: every problem the first check found was the copy's own
	// directory's (inPlaceOnly), so the copy was read whole in place — same
	// incarnation, nothing downloaded — and checked again. Rebuilt: the
	// problems (the first check's, or what the read in place left) sent it to
	// a rebuild; a problem a rebuild left is in the object store or the rows.
	// Problems is then the last check's, and Found the first's.
	RepairedInPlace bool     `json:"repaired_in_place,omitempty"`
	Rebuilt         bool     `json:"rebuilt,omitempty"`
	Found           []string `json:"found,omitempty"`

	// damaged: a problem is DAMAGE no read in place ends — a pack the rows
	// name missing, at another size or hashing to another bundle, a derived
	// file failing its checksum, or damage the sync's git met that is no
	// shape — so a repair rebuilds. Problems without it are the copy's own
	// directory out of place (inPlaceOnly).
	damaged bool
}

// inPlaceOnly reports whether r found problems and every one is the copy's
// own directory out of place, which a full round repairs in place (sweep.go;
// the classes are Cache.check's).
func (r VerifyReport) inPlaceOnly() bool { return len(r.Problems) > 0 && !r.damaged }

// met names first among r's problems the damage d a sync's own git met in the
// copy (by says which sync), if any: the check was handed the copy as it
// stands (damagedRound). A shape is the copy's own directory, which a read in
// place puts right; anything else is damage.
func (r *VerifyReport) met(by string, d *damaged) {
	if d == nil {
		return
	}
	r.Problems = append([]string{fmt.Sprintf("%s met the copy damaged (%s): %v", by, d.reason, d.err)},
		r.Problems...)
	r.damaged = r.damaged || d.reason != RebuildShape
}

// Verification is one check of a repository's cache this node holds:
// admitted by [Cache.AdmitVerify], run once by [Verification.Run], and let go
// by [Verification.Release], which its holder defers. Until released it holds
// a maintenance slot, the fsck's memory charge, a pin of the admitted
// incarnation (so the budget never evicts the copy being checked) and the
// maintainer off the repository (holdMaintainer).
type Verification struct {
	c     *Cache
	e     *entry
	label string

	// held is the pin taken at admission, slot the maintenance slot, and
	// unhold what lets the maintainer back on the repository.
	held   Synced
	slot   *gitproc.Reserved
	unhold func()

	// mu guards mem, ran and released. mem is the memory charged so far:
	// the admitted incarnation's figure, raised when a sync or a rebuild
	// brings the repository more objects (charge).
	mu       sync.Mutex
	mem      int64
	ran      bool
	released bool
}

// AdmitVerify admits a check of repoID's cache on this node before anything
// of it is read (see the file's doc), or refuses it at once, having read
// nothing:
//
//   - [ErrNotHeld]: this node holds no copy (never served here, or evicted);
//   - [gitproc.ErrBusy]: no free maintenance slot, or the background work in
//     flight leaves the check's memory no room — ask again;
//   - [ErrNoMemory]: the charge (verifyCharge) alone is past what this node
//     gives its background work — a node with more memory can run it;
//   - [ErrStopped] and [ErrInvalid] as for every request of a cache.
//
// Every holder defers the answer's Release.
func (c *Cache) AdmitVerify(repoID string) (*Verification, error) {
	switch {
	case c.isStopped():
		return nil, ErrStopped
	case !canonicalID(repoID):
		return nil, fmt.Errorf("%w: repository id %q is not a canonical lowercase UUID", ErrInvalid, repoID)
	}
	c.mu.Lock()
	e := c.entries[repoID]
	c.mu.Unlock()
	if e == nil {
		return nil, fmt.Errorf("cache: verify %s: %w", repoID, ErrNotHeld)
	}
	e.mu.Lock()
	inc := e.current
	if e.dead || inc == nil || inc.retired {
		e.mu.Unlock()
		return nil, fmt.Errorf("cache: verify %s: %w", repoID, ErrNotHeld)
	}
	held := c.pin(e, inc)
	label := cmp.Or(inc.repo.Name, repoID)
	mem := verifyCharge(inc)
	e.mu.Unlock()

	slot, err := c.git.Reserve()
	if err != nil {
		held.Release()
		return nil, fmt.Errorf("cache: verify %s: %w — a check holds a maintenance slot from its first read "+
			"to its last, and none is free; run it again", label, err)
	}
	if err := c.admitCheck(label, mem, 0); err != nil {
		slot.Release()
		held.Release()
		return nil, err
	}
	return &Verification{c: c, e: e, label: label, held: held, slot: slot, unhold: c.holdMaintainer(e), mem: mem},
		nil
}

// verifyCharge is what a check of inc holds of the node's memory, the most
// any step holds (its hashing reads through one buffer): [FsckBytesPerObject]
// for every object its fsck walks — the dropping view's too, since fsck runs
// through the view (heldView) and marks every object directory it is handed,
// and an open expunge's view has no bound — and room for one transfer of what
// a clone sends, objects/pack's alone. e.mu is held.
func verifyCharge(inc *incarnation) int64 {
	return chargeOver(inc.objects+inc.droppingObjects, FsckBytesPerObject, inc.objects)
}

// admitCheck charges a check's memory against the node's budget: mem in all,
// of which already is charged. ErrNoMemory when mem alone is past the budget,
// gitproc.ErrBusy when the work in flight leaves the rest no room.
func (c *Cache) admitCheck(label string, mem, already int64) error {
	if budget := c.maint.budgetNow(); mem > budget {
		return fmt.Errorf("cache: verify %s: %w: its connectivity check needs about %d bytes, and this node "+
			"can give its background work %d of its memory — run it on a node with more memory", label,
			ErrNoMemory, mem, budget)
	}
	if mem <= already {
		return nil
	}
	if c.maint.admitOther(mem-already) != admitted {
		return fmt.Errorf("cache: verify %s: %w — its connectivity check needs about %d bytes, which the "+
			"background work in flight leaves no room for; run it again", label, gitproc.ErrBusy, mem)
	}
	return nil
}

// charge raises v's memory charge to what the objects of the incarnation s
// pins need now: a sync or rebuild since admission may have brought more.
// Refused like the admission, before the incarnation is read. Read under s's
// own entry, which is v's unless the repository was purged and declared again
// meanwhile.
func (v *Verification) charge(s Synced) error {
	s.entry.mu.Lock()
	mem := verifyCharge(s.inc)
	s.entry.mu.Unlock()
	v.mu.Lock()
	defer v.mu.Unlock()
	if err := v.c.admitCheck(v.label, mem, v.mem); err != nil {
		return err
	}
	v.mem = max(v.mem, mem)
	return nil
}

// Release lets go of everything the admission took — the slot, the charge,
// the pin and the hold on the maintainer, which it kicks so a repository that
// fell due meanwhile is taken up at once. Once; a later call does nothing.
func (v *Verification) Release() {
	v.mu.Lock()
	if v.released {
		v.mu.Unlock()
		return
	}
	v.released = true
	mem := v.mem
	v.mu.Unlock()
	v.slot.Release()
	v.c.maint.doneOther(mem)
	v.held.Release()
	v.unhold()
}

// Run checks the repository's cache against its rows (Cache.check) after a
// sync to how.Fresh, ONCE: a second Run, or one after Release, is [ErrInvalid].
// Damage the sync's own git meets is named first, never repaired under the
// check, which would hide it (damagedRound); what the sync's own check
// repaired in place is RepairedBySync, apart from the problems: it asks no
// rebuild. With how.Repair and any problem, the copy is repaired the way the
// problems ask and checked again; a repair that fails answers the report
// before it (package doc, "Damage is repaired once, in place where nothing is
// lost"). Each check pass has its own bound (pass); the sync and a repair are
// bounded by their object-store reads (objstore.ReadBudget, objstore.Stall).
// Every answer carries the report so far beside its error, so a check cut
// short still says what it saw.
func (v *Verification) Run(ctx context.Context, how VerifyRun) (VerifyReport, error) {
	repair := how.Repair
	v.mu.Lock()
	if v.ran || v.released {
		v.mu.Unlock()
		return VerifyReport{Repo: v.e.id}, fmt.Errorf("%w: a verification runs once, and only before its Release",
			ErrInvalid)
	}
	v.ran = true
	v.mu.Unlock()
	c := v.c
	fresh := how.Fresh
	if fresh.Level == "" {
		fresh = statelog.Freshness{Level: statelog.ReadLinearizable}
	}
	s, damage, found, err := c.sync(ctx, v.e.id, fresh, true, true)
	if err != nil {
		return VerifyReport{Repo: v.e.id, Name: v.held.Repo.Name},
			fmt.Errorf("cache: verify %s: %w", v.label, err)
	}
	defer s.Release()
	report := VerifyReport{Repo: v.e.id, Name: cmp.Or(s.Repo.Name, v.held.Repo.Name), Position: s.Position}
	if err = v.charge(s); err != nil {
		return report, err
	}
	report, whole, err := v.pass(ctx, s, how.Checking)
	// What the sync's own check repaired in place (check.go) is no problem of
	// the copy this check was handed.
	report.RepairedBySync = slices.Clone(found)
	// What the sync's own git met is the check's first finding, never
	// repaired under it; a request waiting on the same round is still
	// repaired as its own damage asks.
	report.met("the sync before the check", damage)
	if err != nil || !repair || len(report.Problems) == 0 {
		return report, err
	}
	first := report
	if first.inPlaceOnly() {
		// The copy's own directory out of place is repaired IN PLACE first
		// (repairInPlace): a rebuild leaves no multi-pack-index, bitmap or
		// graph until the maintainer writes them again — hours of bitmapless
		// clones of a large repository, for good on a node whose memory now
		// refuses a bitmap.
		if how.Checking != nil {
			how.Checking(0)
		}
		var (
			inPlace     Synced
			met         *damaged
			repaired    []string
			second      VerifyReport
			secondWhole []string
		)
		if inPlace, met, repaired, err = c.repairInPlace(ctx, s); err != nil {
			return report, fmt.Errorf("cache: verify %s: reading it whole in place could not repair it: %w",
				report.label(), err)
		}
		defer inPlace.Release()
		if err = v.charge(inPlace); err != nil {
			return report, err
		}
		second, secondWhole, err = v.pass(ctx, inPlace, how.Checking)
		second.RepairedInPlace, second.Found = true, first.Problems
		second.RepairedBySync = append(slices.Clone(first.RepairedBySync), repaired...)
		// What the read's own git met (a shape written past its sweep) is the
		// second check's first finding, rebuilt below under this repair,
		// seeded, never handed to the repair a git process asks for.
		second.met("reading it whole in place", met)
		if err != nil {
			return second, err
		}
		if len(second.Problems) == 0 {
			endLine(inPlace)
			return second, nil
		}
		// What the read in place left (a write past the sweep, or damage the
		// first check could not see behind a directory git refused) is
		// rebuilt, seeded with what this check found whole.
		s, report, whole = inPlace, second, secondWhole
	}
	if how.Checking != nil {
		how.Checking(0)
	}
	rebuilt, err := c.rebuild(ctx, s, Failure{Reason: RebuildVerify}, false, whole)
	if err != nil {
		return report, fmt.Errorf("cache: verify %s: a rebuild could not repair it: %w", report.label(), err)
	}
	defer rebuilt.Release()
	if err = v.charge(rebuilt); err != nil {
		return report, err
	}
	second, _, err := v.pass(ctx, rebuilt, how.Checking)
	second.Rebuilt, second.RepairedInPlace = true, report.RepairedInPlace
	second.Found, second.RepairedBySync = first.Problems, first.RepairedBySync
	if err == nil && len(second.Problems) == 0 {
		endLine(rebuilt)
	}
	return second, err
}

// endLine records that Verify has just seen the incarnation s pins whole
// after a repair: the damage its line of rebuilds was for has ENDED, so its
// next failure is rebuilt at once rather than held to the window a rebuild at
// a git process's word stands in (rebuild.go), which exists only because the
// cache never sees a request succeed.
func endLine(s Synced) {
	s.entry.mu.Lock()
	s.inc.rebuilds, s.inc.unmoved = 0, false
	s.entry.mu.Unlock()
}

// repairInPlace is Verify's repair of a copy whose problems are all its own
// directory's (VerifyReport.inPlaceOnly): the incarnation s pins is marked to
// be read WHOLE, IN PLACE (markWhole), counted under RebuildVerify once read,
// and answered pinned by a linearizable full round begun after the mark, whose
// sweep puts the directory right reusing every pack and derived file, with no
// download and no new incarnation. An incarnation replaced or purged since s
// was pinned answers by an ordinary linearizable sync.
//
// The read is the CHECK'S, as the sync before the first pass is: damage its
// own git meets is answered with the copy as it stands (damagedRound), for
// Verification.Run to rebuild seeded and counted under verify, rather than
// at a git process's word: unseeded, and refused by that process's windows.
func (c *Cache) repairInPlace(ctx context.Context, s Synced) (Synced, *damaged, []string, error) {
	e := s.entry
	linearizable := statelog.Freshness{Level: statelog.ReadLinearizable}
	e.mu.Lock()
	if e.dead || e.current != s.inc {
		e.mu.Unlock()
		return c.sync(ctx, e.id, linearizable, true, true)
	}
	c.markWhole(e, s.inc, Failure{Reason: RebuildVerify}, dropRequest)
	w := newWaiter(linearizable)
	w.checks = true
	c.join(e, w)
	e.mu.Unlock()
	got, err := c.await(ctx, e, w)
	// Written before the answer's close, which await waited for, or never:
	// a caller that left is handed nothing (Cache.run, damagedRound).
	return got, w.damage, w.found, err
}

// VerifyRun is how a [Verification] runs.
type VerifyRun struct {
	// Repair repairs a copy the check finds anything wrong with — in place
	// where every problem is the copy's own directory out of place, by a
	// rebuild otherwise or for what that leaves — and checks it again.
	Repair bool
	// Fresh is what the copy is brought to before the check: the zero value is
	// the log's head (linearizable, a barrier on the code log); a caller
	// holding a position names it as a session floor instead, answered at once
	// by a copy at or past it, with no barrier. The check holds the copy to
	// its rows at whatever position it reached.
	Fresh statelog.Freshness
	// Checking, when set, is told each check pass's bound as the pass begins
	// (CheckBudget), and zero as a repair begins: what the caller says of
	// when the node ends what it is doing. Called on the verification's own
	// goroutine.
	Checking func(bound time.Duration)
}

// CheckBudget bounds one check pass over a copy holding bytes on disk (its
// packs, packed-refs and the maintainer's files): its hash (HashBudget) and
// git's connectivity check at gitproc.PackWorkDeadline, the deadline its fsck
// runs under. A BOUND, never a schedule: a whole check measured 9.4 ms a
// mebibyte (BenchmarkVerify; package doc, "Measured") where this gives about
// 1.1 s. Past it is a disk or node far slower than every bound of the cache's
// work allows for, or a wedged check, ended rather than holding its
// maintenance slot and the operator's sweep.
func CheckBudget(bytes int64) time.Duration {
	return HashBudget(bytes) + gitproc.PackWorkDeadline(bytes)
}

// HashBudget bounds a HASH of bytes of a copy's files held to their rows:
// the read of the rows, one read attempt ([RowReadAttempt]), and the bytes
// hashed at VerifyHashPerMiB. It is CheckBudget's hash term, so the two
// cannot drift, and the whole bound of a hint's confirmation (hint.go), which
// hashes one bundle and runs no fsck: about 2.4 minutes for the largest pack
// (MaxPackBytes), where CheckBudget would allow some 24.
func HashBudget(bytes int64) time.Duration {
	mib := max(bytes+objstore.MiB-1, 0) / objstore.MiB
	return RowReadAttempt + time.Duration(mib)*VerifyHashPerMiB
}

// pass is one check of the incarnation s pins, run under the pass's own bound
// — CheckBudget over what the incarnation holds, counted from now — which
// checking is told as the pass begins. A pass the bound ends fails
// ErrPastBound, naming the bound; one its caller ended fails as that.
func (v *Verification) pass(ctx context.Context, s Synced, checking func(time.Duration)) (
	VerifyReport, []string, error) {

	c := v.c
	s.entry.mu.Lock()
	held := s.inc.diskBytes + s.inc.metaBytes
	s.entry.mu.Unlock()
	bound := CheckBudget(held)
	if c.knobs != nil && c.knobs.checkBudget != nil {
		bound = c.knobs.checkBudget(held)
	}
	if checking != nil {
		checking(bound)
	}
	past := fmt.Errorf("%w — %s over the %d bytes this node holds of the repository; ask again", ErrPastBound,
		bound, held)
	bounded, cancel := context.WithTimeoutCause(ctx, bound, past)
	defer cancel()
	report, whole, err := c.check(bounded, s, v.slot)
	if err != nil && ctx.Err() == nil && errors.Is(context.Cause(bounded), ErrPastBound) &&
		!errors.Is(err, ErrPastBound) {
		err = fmt.Errorf("cache: verify %s: %w (%w)", report.label(), past, err)
	}
	return report, whole, err
}

// holdMaintainer keeps the maintainer from starting a run on e until the
// function it answers is called, which lets go and kicks the maintainer so a
// repository that fell due meanwhile is taken up at once. A check holds it
// from admission to release because its listing reads once, as it begins,
// whether a run is in flight, and admits a run's lock and temporary files only
// then: a run started later, or between a repair's two checks, would leave
// files reported as damage. A run already in flight is left to finish, its
// files admitted (Write.Holds): work thrown away is work redone at once.
// Nothing is repaired under a check either: a report recorded meanwhile is
// repaired by the round its release asks (report.go).
func (c *Cache) holdMaintainer(e *entry) func() {
	e.mu.Lock()
	e.verifying++
	e.mu.Unlock()
	return func() {
		e.mu.Lock()
		e.verifying--
		if e.verifying == 0 && e.current != nil && e.current.repair.pending() {
			// What runs reported while the check held repairs off
			// (report.go) is repaired now.
			c.repairRound(e)
		}
		e.mu.Unlock()
		c.kickMaintainer()
	}
}

// label is how an error names the repository: its name, else its id.
func (r VerifyReport) label() string {
	return cmp.Or(r.Name, r.Repo)
}

// check is one pass of Verify over the incarnation s pins, its fsck run in
// slot: the listing (list), the rows, every pack (checkPacks), derived file
// (checkDerived) and installed layer (checkLayers), packed-refs (checkRefs),
// then fsck. Beside its report it answers the checksums of the packs it found
// WHOLE, which a repair need not download again.
//
// Each problem is classed by what repairs it where it is found, never by its
// sentence, which a rewording would move unseen: the listing's, packed-refs,
// a stray pack and fsck are the copy's own directory (or the rows, which no
// repair here ends), put right by a full round in place (sweep.go, refs.go);
// the rest is DAMAGE (VerifyReport.damaged).
func (c *Cache) check(ctx context.Context, s Synced, slot *gitproc.Reserved) (VerifyReport, []string, error) {
	e, inc := s.entry, s.inc
	r := VerifyReport{Repo: e.id, Name: s.Repo.Name, Position: s.Position}
	l, err := c.list(ctx, e, inc)
	if err != nil {
		return r, nil, fmt.Errorf("cache: verify %s: listing %s: %w", r.label(), inc.dir, err)
	}
	defer l.close()
	r.Name, r.Position, r.Problems = cmp.Or(l.repo.Name, r.Name), l.at, l.problems

	// The rows are read AFTER the listing, at least as fresh: a pack installed
	// between the two has its row applied first. No retirement asked: a pack
	// file no row names, one a swap has not taken out yet included, is a stray,
	// which a read in place answers by the swap.
	snap, err := c.read(ctx, e.id, l.at, statelog.Position{}, l.stream, true, false,
		statelog.FloorAt(statelog.ReadSession, l.at))
	switch {
	case err != nil:
		return r, nil, fmt.Errorf("cache: verify %s: reading its rows: %w", r.label(), err)
	case !snap.streamCreated.Equal(l.stream):
		return r, nil, fmt.Errorf("cache: verify %s: the code log under the cache was recreated while it was "+
			"checked (made %s, the cache read one made %s); run it again", r.label(),
			snap.streamCreated.Format(time.RFC3339Nano), l.stream.Format(time.RFC3339Nano))
	}

	packs, whole, held, err := checkPacks(ctx, inc.dir, l, snap)
	r.Packs, r.Problems = packs, append(r.Problems, held.damaged...)
	r.Problems = append(r.Problems, held.strays...)
	r.damaged = len(held.damaged) > 0
	if err != nil {
		return r, whole, fmt.Errorf("cache: verify %s: %w", r.label(), err)
	}
	problems, err := c.checkDerived(ctx, e, inc, l)
	r.Problems = append(r.Problems, problems...)
	r.damaged = r.damaged || len(problems) > 0
	if err != nil {
		return r, whole, fmt.Errorf("cache: verify %s: %w", r.label(), err)
	}
	problems, err = c.checkLayers(ctx, inc.dir, l)
	r.Problems = append(r.Problems, problems...)
	r.damaged = r.damaged || len(problems) > 0
	if err != nil {
		return r, whole, fmt.Errorf("cache: verify %s: %w", r.label(), err)
	}
	refs, problem, err := l.checkRefs()
	r.Refs = refs
	if problem != "" {
		r.Problems = append(r.Problems, problem)
	}
	if err != nil {
		return r, whole, fmt.Errorf("cache: verify %s: %w", r.label(), err)
	}
	// fsck failing is NO DAMAGE of its own: over packs that all hash to their
	// rows it is the directory's (a loose ref, a stray object, HEAD, a shape),
	// which a read in place puts right, or the rows', which a rebuild linking
	// the same packs and writing the same refs would not.
	problem, err = c.fsck(ctx, e, inc, r.label(), slot)
	if problem != "" {
		r.Problems = append(r.Problems, problem)
	}
	return r, whole, err
}

// listing is an incarnation as Verify's listing found it.
type listing struct {
	// at, stream and repo are what the directory reflects; digest is the
	// packed-refs this cache wrote, and refs that file, opened while nothing
	// could replace it.
	at     statelog.Position
	stream time.Time
	repo   gitlet.RepoRef
	digest string
	refs   *os.File
	// packs is every pack file of a pack directory, by the place it lies in
	// (objects/pack, the dropping view) and its checksum: each extension's
	// size.
	packs map[packPlace]map[string]map[string]int64
	// derived is every file a declaration owns that the grammar admitted
	// (derived.go), with its declaration, in declaration order.
	derived []ownedFile
	// installed is the published chain the incarnation holds (layers.go),
	// each layer with its row; chains what each chain file holds when sound
	// (chainWants); mark the chain files' in-place writes as chains was read
	// (markChain), since a run the listing leaves to finish writes the tip in
	// place; chained every pack an installed layer names, held whatever its
	// rows say.
	installed []gitlet.LayerRow
	chains    map[string][]byte
	mark      chainMark
	chained   map[string]bool
	// problems is what the grammar does not admit, and what it requires that
	// is missing.
	problems []string
}

// close lets go of the packed-refs the listing opened.
func (l *listing) close() {
	if l.refs != nil {
		_ = l.refs.Close() // read-only: what was read is the answer
	}
}

// list reads inc's directory against the image grammar with no round writing
// it (entry.write, waited for under ctx). A maintenance run in flight on inc
// is left to finish (holdMaintainer starts no other), and the files it holds
// only while it runs are admitted (Write.Holds); with no run in flight they
// are damage, and named as such.
func (c *Cache) list(ctx context.Context, e *entry, inc *incarnation) (*listing, error) {
	if c.knobs != nil && c.knobs.verifyWaits != nil {
		c.knobs.verifyWaits(inc.dir)
	}
	if err := c.acquireWrite(ctx, e); err != nil {
		return nil, fmt.Errorf("waiting for the repository's round: %w", context.Cause(ctx))
	}
	defer c.releaseWrite(e, writeExclusive)
	if c.knobs != nil && c.knobs.verifyListing != nil {
		c.knobs.verifyListing(inc.dir)
	}

	l := &listing{packs: map[packPlace]map[string]map[string]int64{placeLive: {}, placeDropping: {}},
		chained: map[string]bool{}}
	e.mu.Lock()
	l.at, l.stream, l.repo, l.digest = inc.at, inc.stream, inc.repo, inc.refsDigest
	l.installed, l.chains, l.mark = slices.Clone(inc.installed), chainWants(inc), markChain(inc)
	for cs := range inc.installedPacks {
		l.chained[cs] = true
	}
	running := e.maint != nil && e.maint.inc == inc
	e.mu.Unlock()
	im := c.image
	unexpected := func(rel string) { l.problems = append(l.problems, "unexpected "+filepath.ToSlash(rel)) }
	missing := func(rel string) { l.problems = append(l.problems, "missing "+filepath.ToSlash(rel)) }
	// admit is the image grammar (image.imaged) and what a listing admits
	// beside it: a pack's files, whichever the rows name — checkPacks judges
	// them against the rows, and names a stray — and, while a maintenance
	// run is in flight on inc, the files a declaration holds only while its
	// write runs (Write.Holds).
	admit := func(rel, name string, kind entryKind) bool {
		switch {
		case im.imaged(rel, name, kind):
			return true
		case kind == dirEntry:
			return false
		case slices.Contains(packDirs, rel) && packFileName().MatchString(name):
			return true
		}
		return running && im.holderOf(rel, name) >= 0
	}

	// admitted is every entry the grammar admitted, by its path: a
	// directory is listed only when its parent admitted it as one, and one
	// every incarnation has that is not there is missing. derived is every
	// owned file admitted, by declaration, in the order each directory
	// lists it.
	admitted := map[string]bool{".": true}
	derived := make([][]ownedFile, len(im.decls))
	type packFile struct {
		place packPlace
		d     fs.DirEntry
	}
	var packFiles []packFile
	dir := func(rel string) error {
		if !admitted[rel] {
			if im.required[rel] {
				missing(rel)
			}
			return nil
		}
		list, err := os.ReadDir(filepath.Join(inc.dir, rel))
		if err != nil {
			return err
		}
		for _, d := range list {
			path := filepath.Join(rel, d.Name())
			if !admit(rel, d.Name(), kindOf(d)) {
				unexpected(path)
				continue
			}
			admitted[path] = true
			// A directory of the image under objects/pack — the installed
			// chain's, multi-pack-index.d — is listed as one, below; every
			// other entry there, and every one of the dropping view's, is a
			// file a pack's or a declaration's.
			switch {
			case rel == packRel && !im.dir[path]:
				packFiles = append(packFiles, packFile{placeLive, d})
			case rel == droppingRel:
				packFiles = append(packFiles, packFile{placeDropping, d})
			}
			if f, ok := im.owned(path); ok {
				derived[f.i] = append(derived[f.i], f)
			}
		}
		return nil
	}
	if err := dir("."); err != nil {
		return nil, err
	}
	// The state is not required: a cache serves without it, and its first
	// round of a process writes it.
	for _, name := range []string{"HEAD", "config", "packed-refs"} {
		if !admitted[name] {
			missing(name)
		}
	}
	for _, f := range []struct {
		name string
		ok   func([]byte) bool
		says string
	}{
		{"HEAD", func(b []byte) bool { return string(b) == string(headFor(l.repo.DefaultBranch)) },
			"is not a symbolic ref to the default branch " + l.repo.DefaultBranch},
		{"config", func(b []byte) bool { return string(b) == gitproc.RepoConfig },
			"is not the one the engine writes"},
	} {
		if !admitted[f.name] {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(inc.dir, f.name))
		if err != nil {
			return nil, err
		}
		if !f.ok(raw) {
			l.problems = append(l.problems, fmt.Sprintf("%s %s: it reads %q", f.name, f.says, raw))
		}
	}
	for _, rel := range im.dirs[1:] {
		if err := dir(rel); err != nil {
			return nil, err
		}
	}
	for _, f := range packFiles {
		d := f.d
		if !d.Type().IsRegular() {
			unexpected(filepath.Join(f.place.rel(), d.Name()))
			continue
		}
		m := packFileName().FindStringSubmatch(d.Name())
		if m == nil {
			continue // a declaration's
		}
		info, ierr := d.Info()
		if errors.Is(ierr, fs.ErrNotExist) {
			continue
		}
		if ierr != nil {
			return nil, ierr
		}
		at := l.packs[f.place]
		if at[m[1]] == nil {
			at[m[1]] = map[string]int64{}
		}
		at[m[1]]["."+m[2]] = info.Size()
	}
	for _, files := range derived {
		l.derived = append(l.derived, files...)
	}
	// packed-refs, opened while no round can rename another over it: the
	// file and the digest this cache wrote with it are read together.
	if admitted["packed-refs"] {
		f, err := os.Open(filepath.Join(inc.dir, "packed-refs"))
		if err != nil {
			return nil, err
		}
		l.refs = f
	}
	return l, nil
}

// packFindings is what checkPacks found, by what repairs it: damaged, a pack
// the rows name missing, at another size or hashing to another bundle — the
// object store's bytes again, which a rebuild fetches; strays, a pack no row
// names, which a full round of the copy moves aside in place (sweep.go).
type packFindings struct {
	damaged, strays []string
}

// checkPacks holds every pack a row added at or before the listing's position
// to its row, one at a time, where its rows place it (packPlace), and names as
// a stray every pack file lying where no row places it — a pack only dropping
// rows name in objects/pack among them, which a push could rely on. It
// answers how many packs it checked, the checksums of those found whole, and
// its findings. A row added after the listing is not asked about: what the
// listing saw of it says nothing.
func checkPacks(ctx context.Context, dir string, l *listing, snap snapshot) (int, []string, packFindings,
	error) {

	rows := make([]gitlet.PackRow, 0, len(snap.packs))
	for _, p := range snap.packs {
		rows = append(rows, p)
	}
	slices.SortFunc(rows, func(a, b gitlet.PackRow) int {
		return cmp.Or(cmp.Compare(a.PackID, b.PackID), cmp.Compare(a.Part, b.Part))
	})
	var found packFindings
	var whole []string
	// EACH PACK WHERE ITS ROWS PLACE IT (packPlace): a dropping pack in the
	// dropping view, every other in objects/pack — a pack's files at the
	// other place are no pack the rows name there, and a stray.
	places := snap.places()
	checked := 0
	seen := map[string]bool{}
	buf := make([]byte, copyBuffer)
	for _, row := range rows {
		if !reached(l.at, row.Added) || seen[row.GitChecksum] {
			// Two rows of one checksum name one set of files, held once.
			continue
		}
		seen[row.GitChecksum] = true
		checked++
		place := places[row.GitChecksum]
		label := "pack " + row.PackID + " (pack-" + row.GitChecksum + ")"
		if place == placeDropping {
			label += " in the dropping view"
		}
		files := l.packs[place][row.GitChecksum]
		if _, ok := files[".idx"]; !ok {
			found.damaged = append(found.damaged, label+" is missing")
			continue
		}
		if _, ok := files[".pack"]; !ok {
			found.damaged = append(found.damaged, label+" is missing")
			continue
		}
		ref, sized := row.Ref(), true
		for _, section := range []struct {
			ext  string
			want int64
		}{{".pack", ref.PackBytes()}, {".idx", ref.IndexBytes}, {".rev", ref.ReverseBytes}} {
			got, ok := files[section.ext]
			switch {
			case !ok:
				found.damaged = append(found.damaged, fmt.Sprintf("%s: its %s is missing", label, section.ext))
				sized = false
			case got != section.want:
				found.damaged = append(found.damaged, fmt.Sprintf("%s: its %s is %d bytes; its row says %d",
					label, section.ext, got, section.want))
				sized = false
			}
		}
		if !sized {
			continue
		}
		sum, err := hashBundle(ctx, filepath.Join(dir, place.rel(), "pack-"+row.GitChecksum), buf, nil)
		switch {
		case errors.Is(err, fs.ErrNotExist):
			found.damaged = append(found.damaged, label+" is missing")
		case err != nil:
			return checked, whole, found, fmt.Errorf("hashing %s: %w", label, err)
		case sum != row.Hash:
			found.damaged = append(found.damaged, fmt.Sprintf("%s hashes to %s; its row says %s", label, sum,
				row.Hash))
		default:
			whole = append(whole, row.GitChecksum)
		}
	}
	for _, place := range []packPlace{placeLive, placeDropping} {
		strays := make([]string, 0)
		for cs := range l.packs[place] {
			// A pack an installed layer names is the chain's, held in
			// objects/pack until the layer goes (layers.go): no stray, its
			// row retired or not.
			if places[cs] != place && (place != placeLive || !l.chained[cs]) {
				strays = append(strays, cs)
			}
		}
		slices.Sort(strays)
		for _, cs := range strays {
			switch {
			case places[cs] == placeNone:
				found.strays = append(found.strays, "stray pack pack-"+cs+" (no row names it)")
			case place == placeDropping:
				found.strays = append(found.strays,
					"stray pack pack-"+cs+" in the dropping view (a live row names it)")
			default:
				found.strays = append(found.strays,
					"stray pack pack-"+cs+" in objects/pack (only dropping rows name it)")
			}
		}
	}
	return checked, whole, found, nil
}

// checkDerived holds every derived file the listing found to its bytes as its
// declaration's Check says (ownedFile.held, check.go), today git's hashfile
// trailer: the one check naming damage git reads unchecked — a bitmap no git
// command verifies, a graph or index answering wrong. A file replaced or
// removed since the listing (rounds are held off for the listing alone), or a
// chain file written in place since its record was read (chainVerdict), is no
// finding. No read slot is taken a piece: the check holds its maintenance slot
// throughout. Passed over: a declaration whose every read holds its files
// (Check.hashed), and every file of an installed layer, which checkLayers
// reads once; a chain file and a layer file no installed layer names are
// this pass's.
func (c *Cache) checkDerived(ctx context.Context, e *entry, inc *incarnation, l *listing) ([]string, error) {
	var problems []string
	p := c.pieces(nil)
	dir := inc.dir
	layered := map[string]bool{}
	for _, row := range l.installed {
		for _, rel := range layerRels(row.Layer) {
			layered[rel] = rel != ""
		}
	}
	for _, f := range l.derived {
		if !f.of.Check.hashed() || layered[f.rel] {
			continue
		}
		_, want, err := f.held(l.chains)
		if err != nil {
			return problems, err
		}
		if c.knobs != nil && c.knobs.hashing != nil {
			c.knobs.hashing(filepath.Join(dir, f.rel))
		}
		v, _, _, err := trailerSound(ctx, filepath.Join(dir, f.rel), p, want)
		v = chainVerdict(e, inc, want, l.mark, v)
		switch {
		case err != nil:
			return problems, fmt.Errorf("hashing %s: %w", filepath.ToSlash(f.rel), err)
		case v == hashDamaged && want.whole:
			problems = append(problems, fmt.Sprintf("%s does not list the installed chain %s",
				filepath.ToSlash(f.rel), words.And(strings.Fields(string(want.content)))))
		case v == hashDamaged:
			problems = append(problems, filepath.ToSlash(f.rel)+" fails its checksum")
		}
	}
	return problems, nil
}

// checkLayers holds every layer the incarnation installed to the row it was
// installed from (gitlet.CheckLayer, each file read once, by this pass alone):
// its trailers, sections, packs, counts and base's graph layers, which catches
// a trailer sound over bytes the row does not describe. Each finding names the
// layer and is damage: a rebuild seeds every layer that passes (seedRebuild).
// A file gone is one finding. Its bytes are within the pass's bound (CheckBudget), hashed with no read
// slot a piece, as checkDerived's.
func (c *Cache) checkLayers(ctx context.Context, dir string, l *listing) ([]string, error) {
	var problems []string
	p := c.pieces(nil).layer()
	for i, row := range l.installed {
		if err := ctx.Err(); err != nil {
			return problems, context.Cause(ctx)
		}
		if c.knobs != nil && c.knobs.hashing != nil {
			for _, rel := range layerRels(row.Layer) {
				if rel != "" {
					c.knobs.hashing(filepath.Join(dir, rel))
				}
			}
		}
		err := checkLayerAt(ctx, dir, row, baseOf(l.installed, i), p)
		switch {
		case errors.Is(err, fs.ErrNotExist):
			problems = append(problems, fmt.Sprintf("layer %s: a file of it is missing: %v", row.Layer.LayerID, err))
		case errors.Is(err, gitlet.ErrBadLayer):
			problems = append(problems, fmt.Sprintf("layer %s: %v", row.Layer.LayerID, err))
		case err != nil:
			return problems, fmt.Errorf("reading layer %s: %w", row.Layer.LayerID, err)
		}
	}
	return problems, nil
}

// checkLayerAt holds the files of row's layer in the incarnation at dir to the
// row over base (gitlet.CheckLayer), each trailer hashed as p says and stopped
// with ctx: ErrNotExist for a file gone, gitlet.ErrBadLayer for one the row does
// not describe, and a failed read, a refused hold or an ended ctx as it came.
func checkLayerAt(ctx context.Context, dir string, row gitlet.LayerRow, base []string, p gitlet.Pieces) error {
	var readers [3]io.ReaderAt
	for i, rel := range layerRels(row.Layer) {
		if rel == "" {
			continue
		}
		f, err := os.Open(filepath.Join(dir, rel))
		if err != nil {
			return err
		}
		defer func() { _ = f.Close() }() // read-only: what was read is the answer
		readers[i] = f
	}
	return gitlet.CheckLayer(ctx, row.Layer, readers[0], readers[1], readers[2], base, p)
}

// hashBundle is the SHA-256 of the pack at stem as the push stored it: its
// .pack, .idx and .rev back to back, streamed through buf with ctx checked
// between reads. hold, when set, is the read slot each CheckPieceBytes piece
// is hashed in (gitproc.Pool.HoldRead); nil for a caller holding its own slot
// across the hash (a check, in its reserved maintenance slot).
func hashBundle(ctx context.Context, stem string, buf []byte,
	hold func(context.Context) (func(), error)) (objstore.Hash, error) {

	sum := sha256.New()
	for _, ext := range []string{".pack", ".idx", ".rev"} {
		if err := hashFile(ctx, sum, stem+ext, buf, hold); err != nil {
			return "", err
		}
	}
	return objstore.Hash(hex.EncodeToString(sum.Sum(nil))), nil
}

// hashFile adds the bytes of the file at path to sum, a CheckPieceBytes piece
// at a time, each in a slot hold takes (none when nil): the one piece loop
// every hash of this node's files reads by (gitlet.HashPieces), so each holds
// the read lane alike. It hashes the bytes the file held when opened; a
// rewrite meanwhile is its caller's to see, by the files' identity (hashPack).
func hashFile(ctx context.Context, sum io.Writer, path string, buf []byte,
	hold func(context.Context) (func(), error)) error {

	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }() // read-only: what was read is the answer
	info, err := f.Stat()
	if err != nil {
		return err
	}
	_, err = gitlet.HashPieces(ctx, sum, f, info.Size(), gitlet.Pieces{Buf: buf, Size: CheckPieceBytes, Hold: hold})
	return err
}

// checkRefs hashes the packed-refs the listing opened against the digest
// this cache wrote with it, and counts its refs (every line but the header
// and the peel lines).
func (l *listing) checkRefs() (int, string, error) {
	if l.refs == nil {
		return 0, "", nil
	}
	sum := sha256.New()
	count := &refLines{start: true}
	if _, err := io.Copy(io.MultiWriter(sum, count), l.refs); err != nil {
		return 0, "", fmt.Errorf("reading packed-refs: %w", err)
	}
	got := hex.EncodeToString(sum.Sum(nil))
	if l.digest != "" && got != l.digest {
		return count.refs, fmt.Sprintf("packed-refs hashes to %s; this cache wrote %s", got, l.digest), nil
	}
	return count.refs, "", nil
}

// refLines counts the ref lines of a packed-refs passing through it.
type refLines struct {
	refs  int
	start bool
}

func (r *refLines) Write(p []byte) (int, error) {
	for _, b := range p {
		if r.start && b != '#' && b != '^' {
			r.refs++
		}
		r.start = b == '\n'
	}
	return len(p), nil
}

// fsck runs git's connectivity check in inc, in the maintenance slot the
// check's admission holds, and answers a problem when git found one. Its
// memory, FsckBytesPerObject an object (some 11 GB over the largest
// repository the defaults admit), was charged at admission and raised to inc
// (verifyCharge, Verification.charge).
func (c *Cache) fsck(ctx context.Context, e *entry, inc *incarnation, label string,
	slot *gitproc.Reserved) (string, error) {

	e.mu.Lock()
	held := inc.diskBytes + inc.metaBytes
	e.mu.Unlock()
	var firstOut string
	findings := 0
	out := gitproc.NewLines(func(line []byte) error {
		if findings == 0 {
			firstOut = string(line)
		}
		findings++
		return nil
	})
	if c.knobs != nil && c.knobs.verifyFsck != nil {
		c.knobs.verifyFsck(inc.dir)
	}
	// Through the dropping view where the copy holds one (heldView): the
	// check is of what the copy serves, and a ref a push revived onto a
	// dropping object in an expunge's window reaches it, kept live (its rescue).
	_, err := slot.Run(ctx, gitproc.Cmd{
		Dir: inc.dir, Dropping: heldView(inc.dir),
		Args:   []string{"fsck", "--connectivity-only", "--no-dangling", "--no-progress"},
		Config: c.ServeConfig(), Stdout: out, Class: gitproc.ClassMaintenance,
		Deadline: gitproc.PackWorkDeadline(held),
	})
	var exit *gitproc.ExitError
	switch {
	case err == nil:
		return "", nil
	case errors.As(err, &exit):
		return fsckProblem(exit.Result, firstOut, findings), nil
	case errors.Is(err, gitproc.ErrRepoShape):
		return "fsck did not run, the repository being no shape the engine writes: " + err.Error(), nil
	}
	return "", fmt.Errorf("cache: verify %s: fsck: %w", label, err)
}

// fsckProblem is the sentence a failed fsck is reported as: its FIRST
// finding — an error it printed, or an object it named on its output — and
// how many more there were. Not its last line: fsck ends a damaged
// repository's report with a notice ("No default references") that names
// nothing. A stderr cut to its tail (gitproc.StderrTail) loses its first,
// partial, line, and the count is then a floor.
func fsckProblem(res gitproc.Result, firstOut string, outFindings int) string {
	stderr := res.Stderr
	if len(stderr) >= gitproc.StderrTail {
		if i := strings.IndexByte(stderr, '\n'); i >= 0 {
			stderr = stderr[i+1:]
		}
	}
	var findings []string
	for _, line := range strings.Split(stderr, "\n") {
		if line = strings.TrimSpace(line); line != "" && !strings.HasPrefix(line, "notice:") {
			findings = append(findings, line)
		}
	}
	if firstOut != "" {
		findings = append(findings, firstOut)
	}
	n := len(findings) + max(outFindings-1, 0)
	switch n {
	case 0:
		return fmt.Sprintf("fsck: exited %d naming nothing", res.Exit)
	case 1:
		return "fsck: " + findings[0]
	}
	return fmt.Sprintf("fsck: %s (and %d more)", findings[0], n-1)
}
