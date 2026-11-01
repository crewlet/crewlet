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

	"github.com/crewlet/crewlet/internal/code"
	"github.com/crewlet/crewlet/internal/code/gitproc"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/statelog/metrics"
)

// REQUEST-TIME REPAIR. A git process the host ran in a cache that git itself
// found damaged — a pack, an index, a multi-pack-index or a commit-graph it
// could not read (gitproc.ErrCorrupt, with the part its report named), or an
// object a row names that no installed pack holds (gitproc.ErrMissingObject)
// — or that the pool refused to start there because the directory is not the
// shape the engine writes (gitproc.ErrRepoShape) makes the host REPAIR the
// cache ([Cache.Rebuild]) and retry its request once on the answer.
// [DamageReason] is the one reading of such a failure, the host's and the
// cache's alike, and it reads what the pool read from git's trace2 format
// strings — never git's words, which a request can write (gitproc's package
// doc). A damage git names only in words — 2.56's corrupt packed object,
// under a bare "%s" — is a hint, which decides nothing until the pack it
// nominates is held to its row ([Cache.ConfirmHint]); the same sentence is a
// row on 2.55, a pack's damage, and the pack its message names is a
// hint beside it, so a confirmed one names to the repair the pack to fetch
// again (Failure.Packs).
//
// A DERIVED FILE IS REPAIRED IN PLACE. The multi-pack-index (its bitmap and
// reverse index beside it) and the commit-graph are files the maintainer
// writes from the packs the incarnation already holds, so damage git named in
// them alone ([RebuildDerived]) is repaired by dropping THOSE files — by the
// part the damage named, and both for a chunk-format file whose message names
// neither (gitproc.Damage.Drops) — in a whole read in place, with no download:
// the round or the maintainer writes them again over the same packs.
//
// A SHAPE IS REPAIRED IN PLACE. The pool refuses a directory for its shape
// before git starts, so a shape refusal says nothing against any pack — and
// the cache already has the repair that costs no GET: a full round, whose
// sweep puts config right and moves a commondir and an alternates file out,
// reusing every pack the incarnation holds (sweep.go), and holds what it
// leaves to the pool's own check. So the incarnation is marked to be read
// whole (markWhole), as a log recreated or gone back under the cache
// marks every one, and the request is answered by that round. Only a shape the
// sweep did NOT end — refused past the very round that swept it — is a
// rebuild, under the rules below; one per request is otherwise the cost of
// something outside the engine that keeps editing the directory, and each is
// a full read of the rows and no download.
//
// A PACK'S OR AN OBJECT'S DAMAGE IS REPAIRED IN PLACE FIRST, and never
// escalated past a write it may predate — a request's ([Cache.Rebuild] for
// [RebuildCorrupt] and [RebuildMissing]), in four steps under one call, and a
// round's own git's in the same four, each of its callers taken on from the
// step it has reached (damagedRound, repairDamaged in sync.go):
//
//  1. THE GUARD. A request whose process began before this process last
//     wrote a REPAIR into the copy (incarnation.repairWrite: a pack installed
//     again over its files, a pack it had lost put back, a derived file
//     dropped) may have failed on what the repair replaced, so it is answered
//     from the copy as it stands (Kind SyncNone, counted
//     reports{outcome=stale}) and runs once more over it, rather than
//     escalated by a transient — and answered only once no round holds the
//     copy (asItStands), since a round still writing its repair has the copy
//     half way (the derived files dropped, the pack not yet renamed over).
//     Nothing else this process writes trips it: a push's pack, an adoption
//     and the maintainer's index or graph add what the failing request never
//     needed and leave whatever failed it as it was, and counted as repairs,
//     a repository taking pushes answered every request's damage as it stands
//     and never repaired it. Asked again before step 3 and before step 4
//     (Cache.escalate): the scrubber's own repair of the same damage may land
//     beside the request's scrub.
//  2. IN PLACE. The copy is read whole by its next round (markWhole), FORCED:
//     every index file held to its trailer whatever its vouch — damage that
//     arrived after this process checked a file is what no vouch sees — and
//     the packs a confirmed hint named (Failure.Packs) installed again. Every
//     pack whose .idx or .rev fails, that the copy LOST (the rows name it at a
//     position the copy had read, and it is gone — install.go's lost) or holds
//     at the wrong size, or that a hint named, is installed again with one
//     GET, renamed over its files in the same incarnation; every derived file
//     failing its trailer is dropped. A round that did any of it answers the
//     request from the repaired copy (rebuilds{reason=stored}, or `derived`
//     when it only dropped); a push the copy had not yet read, installed by
//     the same round, repairs nothing. The forced check SPENDS the forces
//     once it has run, whatever its round meets after, and what it found
//     waits on the copy's repair mark (recordCheck) — so a round that fails
//     past its check, at a pack it adds that the store will not hand over,
//     leaves the rounds after it a finding to install and nothing to hash
//     again. BOUNDED as step 3 is: a forced check of the copy within
//     ScrubCooldown, with no repair written since it ended
//     (incarnation.forcedAt — recorded as its round ends, whatever that
//     round met after it: its install failing, its refs' peel), answers the
//     next step 2 that names no pack it could fetch — none, or only packs
//     whose download is held off after the store refused one
//     (repairMark.fetchable), which the install leaves as they stand — since
//     the same files would hash the same, and what they did not hash the same
//     the repair mark already holds; and each forced round reads the rows
//     whole and holds every reader of the repository while it hashes about a
//     quarter of the copy's pack bytes (4–5 s warm and some 39 s cold for the
//     largest repository the defaults admit): a client polling a broken
//     repository costs one forced round an hour, and its requests go straight
//     to the scrub's verdict and the rebuild's refusals, which answer at once
//     where they stand.
//  3. THE SCRUB, for the callers. A round that repaired nothing leaves damage
//     in a pack BODY no git named, or in the rows: the copy's scrub runs at
//     once for these callers (scrubFor), every pack hashed against its own
//     trailer, outside the write lock with the copy answering; what it finds
//     is installed again by the round it asks, which answers them — unless a
//     scrub of the copy already said: one whose findings still wait for their
//     repair, which is the answer (with the packs that scrub found whole); or
//     one that found nothing and either began after every failure it is asked
//     about, or ended within ScrubCooldown with no repair written since, whose
//     verdict stands — beside either, what a round's check has found since
//     that still waits for its repair, the answer too, its pack none the
//     rebuild links (recordCheck). A check's finding alone answers nothing
//     here: a check reads no pack body — and step 2's check finding one of a
//     scrub's files failing again leaves it that scrub's finding, which goes
//     on answering. It runs as soon as the scrub in flight lets go of the
//     node's one scrub, ahead of every scrub the scrubber has due, and a
//     routine scrub in flight is ended for it (scrub.go's scrubSlot).
//  4. THE WHOLE REBUILD, last, under the rules below — SEEDED with every pack
//     the scrub found whole (seedRebuild), so it downloads only what no scrub
//     vouched for: nothing at all when the damage is in the rows. NOT ASKED
//     while a pack it would download is held off (repairMark.heldOffFor): its
//     last download to install it again failed, the backoff holds the next,
//     and a rebuild would ask the store for the same pack under the same row
//     — a GET the backoff exists to spare, which cannot complete — so the
//     caller is answered with the store's own refusal of it instead, and
//     step 2 asks once more when the backoff passes.
//
// So a pack's damage costs one GET a damaged pack where it cost every pack the
// repository holds, whichever git met it, and the whole rebuild is what only
// the rows' damage reaches.
//
// A REBUILD IS THE NEXT INCARNATION, built BESIDE the one readers hold: a
// whole read at the log's head, every pack downloaded again — but those an
// operator's repair, or the scrub of a request's or a round's own repair, has
// just found whole, which it links (seedRebuild) —
// into repos/<id>.<n+1>.git, which becomes current only once it has
// converged. The one it replaces is RETIRED — its maintenance cancelled, its
// directory moved into trash/ with its last reader — so a git process
// already running there finishes on the files it opened, rather than meeting
// another directory's files under its GIT_DIR half way through its run. A
// rebuild that fails leaves the old incarnation current and moves the
// partial one into trash/.
//
// IT COSTS ONE REBUILD HOWEVER MANY REQUESTS FAILED. A request that failed on
// an incarnation already replaced is answered from the current one with no
// rebuild at all; one that arrives while a rebuild of its incarnation is
// queued or running joins it; and the callers of an ordinary sync the
// rebuild was queued over are answered by it when it succeeds and by an
// ordinary round when it fails, never with somebody else's failed repair.
//
// AND IT IS NOT REPEATED WHERE IT CANNOT HELP. A rebuild that did not end a
// failure will not end it a second time from the same rows — the damage is
// then the object store's or the rows', which `crewlet code verify` names —
// so an incarnation holding exactly what such a rebuild read is not rebuilt
// again for RebuildBackoffCap unless something writes into it, and a line of
// attempts that keep failing waits RebuildCooldown after the first and twice
// as long after each one after it, up to RebuildBackoffCap. A request
// refused is answered at once (ErrRebuildRefused) rather than paying for
// another whole download of the repository on every node it reaches. An
// ATTEMPT starts the window, success or not, and a refusal does not.
//
// A LINE ENDS WHEN THE ASKING STOPS. The cache never sees a request succeed,
// only one fail, so the one sign that a rebuild ended its failure is that
// nobody asks for another: a repository with no rebuild asked of it for
// RebuildQuiet starts a new line at its next ask — damage that arrives a day
// after an incident was repaired is met as the first attempt it is, not at
// the window the incident grew — while damage nothing repairs keeps being
// asked about, refused or not, and its line goes on.

// RebuildReason is why a repository was read whole into a cache again: a
// closed set ([RebuildReasons], [RebuildReason.Valid]) and the reason
// attribute of crewlet.code.cache.rebuilds.
type RebuildReason string

const (
	// RebuildCorrupt is a git process in the cache classified
	// gitproc.ErrCorrupt whose damage is NOT confined to derived files
	// (RebuildDerived is that): a pack or an index git could not read —
	// repaired in place first, each failing pack fetched again over its files
	// (RebuildStored), and read whole into the next incarnation only when
	// neither the in-place round nor a scrub finds what to fetch again: the
	// count is of those whole rebuilds.
	RebuildCorrupt RebuildReason = "corrupt"
	// RebuildDerived is damage git named only in DERIVED files — the
	// multi-pack-index (its bitmap, its reverse index), the commit-graph, or
	// a chunk-format file of either — in any process over the incarnation
	// (a gitproc.Damage whose parts are all derived, gitproc.Damage.Derived).
	// Those are files git only mmaps, written by the maintainer from the
	// packs the incarnation already holds, so it is repaired IN PLACE like a
	// shape: a whole read that DROPS the files the damage named
	// (gitproc.Damage.Drops: the index's, the graph, or both for a
	// chunk-format message, which names neither) so the round or the
	// maintainer writes sound ones over the same packs — no download, no new
	// incarnation, where a whole-repository rebuild would GET every pack
	// again (thousands, for a large repository). Only damage that SURVIVES
	// this in-place repair — a pack the dropped index named is itself
	// damaged — escalates to RebuildCorrupt (repair.go, damagedRound).
	RebuildDerived RebuildReason = "derived"
	// RebuildMissing is a git process in the cache classified
	// gitproc.ErrMissingObject: an object a row names that no installed
	// pack holds — repaired in place first as RebuildCorrupt is (a pack the
	// copy lacks installed, one whose files fail fetched again), and the
	// whole rebuild counted here only past that and a scrub.
	RebuildMissing RebuildReason = "missing"
	// RebuildShape is a git process the pool refused to start in the cache,
	// gitproc.ErrRepoShape: the incarnation's config is not
	// gitproc.RepoConfig byte for byte, it holds a commondir, or an
	// alternates file the engine never writes into one — a disk that damaged
	// the file, or something outside the engine that edited it. Git ran
	// nothing, so the refusal is all it says, and it is said on EVERY
	// process the directory is asked to run: no retry, and no delta round
	// (which never sweeps), ever ends it. The incarnation is read whole IN
	// PLACE by a full round, whose sweep repairs every shape the pool
	// refuses and reuses every pack (markWhole), and counted by that round;
	// only a shape that sweep did not end is rebuilt.
	RebuildShape RebuildReason = "shape"
	// RebuildStream is a cache whose log was RECREATED under it — its stream
	// identity is not the one the cache read from (ADR-0053: a stream the
	// engine provisions again is stamped anew; a snapshot restore is not a
	// recreation). Counted by the round that reads it whole.
	RebuildStream RebuildReason = "stream"
	// RebuildRewound is a cache AHEAD of its log on the same stream: a
	// linearizable answer below a position this cache read, which only a
	// log that went back can give — a broker restored from an older copy.
	// Counted by the round that reads it whole.
	RebuildRewound RebuildReason = "rewound"
	// RebuildVerify is an operator's repair: a check ([Verification.Run])
	// found the cache holding something its rows do not say, and was asked
	// to repair it — IN PLACE when everything it found is the copy's own
	// directory out of place (a full round of the same incarnation, every
	// pack and derived file kept, counted as it converges), and by a rebuild
	// into the next incarnation otherwise or for what that leaves, each
	// counted once. The cache's own, never a git process's: Rebuild refuses
	// it.
	RebuildVerify RebuildReason = "verify"
	// RebuildStored is a pack's STORED files — its .pack, .idx or .rev —
	// installed again from its bundle IN PLACE, one GET a pack, renamed over
	// the files a check before serving or a scrub behind it found failing
	// their checksums, a full round found at another size than their row's
	// or missing, or a confirmed hint named (install.go): no new incarnation,
	// and no other pack downloaded. It is the first repair of a pack's or an
	// object's damage — a request's (Cache.Rebuild) or a round's own git's
	// (repairDamaged) — whose in-place round is counted here when it fetched
	// a pack. Counted by the round that did it, once a round; never refused,
	// since a check holds only what this process has not vouched for, a
	// scrub runs at most once a ScrubCooldown at a report's word, and an
	// in-place round is one round however many callers ask it at once and,
	// once one has repaired nothing, none for the repairs of the next
	// ScrubCooldown that name no pack it could fetch, with no repair written
	// since.
	RebuildStored RebuildReason = "stored"
)

// RebuildReasons is every [RebuildReason].
var RebuildReasons = []RebuildReason{RebuildCorrupt, RebuildDerived, RebuildMissing, RebuildShape, RebuildStream,
	RebuildRewound, RebuildVerify, RebuildStored}

// gitReasons are the reasons a git process's failure gives through
// [DamageReason] and [ShapeReason] and that reach [askRebuild] — so the ones
// crewlet.code.cache.rebuild.refusals can carry. RebuildDerived is NOT one:
// it is always repaired in place, and so is never refused — a derived file
// whose damage survives its drop is asked as RebuildCorrupt.
var gitReasons = []RebuildReason{RebuildCorrupt, RebuildMissing, RebuildShape}

// rebuildReasons are every reason [Cache.Rebuild] takes: the git reasons plus
// RebuildDerived.
var rebuildReasons = append(append([]RebuildReason{}, gitReasons...), RebuildDerived)

// inPlaceReasons are the damage a WHOLE READ IN PLACE repairs — no new
// incarnation: a shape its sweep puts right (RebuildShape), derived files its
// round drops and the round or the maintainer rewrites over the same packs
// (RebuildDerived), both with no download, and a pack's stored files its
// round installs again over themselves, one GET a pack (RebuildStored). The
// others replace the incarnation.
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
	// Began is when the failing process began (gitproc.Damage.Began; zero
	// when nothing says): a drop spares a file this process wrote after it,
	// which the failing run may not have read, and a pack's or an object's
	// repair answers from the copy a failure that predates a write into it.
	Began time.Time
	// Packs is the packs a confirmed hint named ([Cache.ConfirmHint]), by
	// checksum: a [RebuildCorrupt] or [RebuildMissing] repair installs each
	// again in place, one GET a pack, and none for any other reason.
	Packs []string
}

// DamageReason is the damage a git process whose directory is one of this
// cache's incarnations failed with names, and whether it is damage the cache
// repairs ([Cache.Rebuild]), as a [Failure]: [RebuildShape] for
// gitproc.ErrRepoShape — the pool's refusal to start one there, which names
// the incarnation, since a process run in it names no alternate;
// [RebuildDerived] for a gitproc.Damage of kind gitproc.ErrCorrupt whose
// parts are all derived files (gitproc.Damage.Derived; the files to drop are
// its Drops, and Began when its process began); [RebuildCorrupt] for any
// other gitproc.ErrCorrupt; [RebuildMissing] for
// gitproc.ErrMissingObject — read with errors.Is, so a bare sentinel (the
// peel's own, refs.go) reads too. Never gitproc.ErrNotOurRef, which a want the
// repository never served and a lost pack print alike (the next linearizable
// sync re-downloads any pack whose file is gone), and never a refusal of the
// call itself (gitproc.ErrConfig and its siblings), which no directory causes.
// Every one of these is read from what the pool read of git's trace2 format
// strings, which carry no request text (gitproc's package doc).
//
// THE PROCESS'S DIRECTORY decides what its failure describes. One run in a
// push's scratch or an advertisement view reads the incarnation only as its
// alternate, and there only its shape is the cache's: the scratch's
// index-pack reads the CLIENT's pack, so its ErrCorrupt is that pack's
// (gitproc.ExitError) — such a caller asks [ShapeReason].
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

// ShapeReason is [DamageReason] for a git process whose directory is a
// scratch or an advertisement view of an incarnation rather than the
// incarnation itself, which names the incarnation as its alternate: the
// pool's refusal of THE ALTERNATE's shape (gitproc.ShapeError.InAlternate) is
// the cache's to repair, and nothing else such a process says is — a refusal
// of the scratch's own shape is a directory one request made, which the next
// makes again.
func ShapeReason(err error) (Failure, bool) {
	var shape *gitproc.ShapeError
	if errors.As(err, &shape) && shape.InAlternate {
		return Failure{Reason: RebuildShape}, true
	}
	return Failure{}, false
}

// Rebuild repairs the incarnation failed was handed out of, and answers the
// repaired directory pinned. f is what the host's git process over
// failed.Dir was refused or classified as ([DamageReason], [ShapeReason]):
// its reason, and the derived files a [RebuildDerived] drops — none for any
// other reason:
//
//   - [RebuildShape] reads the incarnation whole IN PLACE (markWhole): the
//     round that answers is a full one that started after the mark, whose
//     sweep repairs every shape the pool refuses and reuses every pack the
//     incarnation holds — no new incarnation, no download, and none of a
//     rebuild's refusals, since it costs a read of the rows. A shape that
//     sweep did not end is rebuilt by the round itself (Cache.run).
//   - [RebuildDerived] reads the incarnation whole IN PLACE the same way,
//     and its round DROPS the derived files f.Drops names (apply) so it or
//     the maintainer writes sound ones over the same packs — no download —
//     whatever else the incarnation is already marked to be read whole for
//     (a shape, a log recreated or gone back), which the read is then
//     counted under instead (incarnation.drop) — unless this process wrote a
//     file of them after the failing process began (f.Began), which that
//     process may not have read (dropDamaged). Damage it does not end (a
//     pack the index named is itself damaged) escalates to
//     [RebuildCorrupt] on the retry (repair.go). It drops only the files of
//     a declaration that drops on a report (OnReported): damage whose every
//     declaration keeps its files is no derived file a drop ends, and is
//     repaired as [RebuildCorrupt] is, at once. Neither in-place repair
//     reads a pack, so what the failing process reported beside the shape or
//     the derived file and was held for failed's reader ([Synced.Repairer],
//     report.go) — a crash, a stored part, a hint — is no part it answers:
//     it is CARRIED to the reader the repaired copy is answered to
//     (awaitInPlace), the same request running again, for that request's
//     next repair to consume or its release to hand on.
//   - [RebuildCorrupt] and [RebuildMissing] are repaired IN PLACE FIRST, in
//     the four steps above: a failure that predates a write into the copy is
//     answered from it as it stands — the one answer of Kind SyncNone, since
//     every repair is answered by a linearizable round — then a forced round
//     reinstalls what it finds failing and the packs f.Packs names, then a
//     scrub finds a pack body no git named, and only then is the copy
//     replaced by the NEXT incarnation, read whole at the log's head and
//     seeded with what the scrub found whole: one whose rebuild is queued or
//     running joins it, and one a rebuild could not end the failure of is
//     refused ([ErrRebuildRefused], see rebuildRefusal). What the failing
//     process reported was held for failed's reader ([Synced.Repairer],
//     report.go) rather than made the copy's scrub, and this repair answers
//     it: once the caller has its answer, it is consumed (awaitRequest).
//
// A failed that is no longer its repository's current incarnation — rebuilt
// or purged since it was pinned — is answered by a linearizable [Cache.Sync]
// with no repair. One a SWAP replaced (swap.go) is answered from the copy
// serving its files now AS IT STANDS — Kind SyncNone, counted
// reports{outcome=stale} for a pack's or an object's — so the request runs
// once more over it with its repair unspent (swappedAnswer). failed stays
// the caller's to release. ctx bounds the caller's wait and nothing else:
// the repair runs on for anybody else waiting on it.
func (c *Cache) Rebuild(ctx context.Context, failed Synced, f Failure) (Synced, error) {
	reason, drops := f.Reason, f.Drops
	if !slices.Contains(rebuildReasons, reason) {
		return Synced{}, fmt.Errorf("%w: a rebuild's reason is what the host's git process over the cache "+
			"failed with, one of %v, and %q is not", ErrInvalid, rebuildReasons, reason)
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
		// DAMAGE NO DROP REPAIRS: every declaration it names of those the
		// copy holds files of (image.holding) keeps its files on a report
		// (OnReported) — the installed chain's among them, the copy's index
		// in CHAIN mode — so what git could not read is no derived file a
		// drop of the copy ends: repaired as a pack's damage is, as a round
		// whose own git met it asks (damagedRound). Read whole to drop
		// nothing, the request was answered from the copy that failed it.
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

// repairRequest is Rebuild for a request's pack or object damage — steps 1
// and 2 of the paragraphs above; Cache.run hands a round that repaired
// nothing to escalate, steps 3 and 4.
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
		// STEP 2 IS BOUNDED as step 3 is: a forced check of this copy ran
		// within ScrubCooldown, and no repair has been written since it
		// ended — another would hash the same files the same, what they did
		// not the repair mark already holding, and read the rows whole and
		// hold every reader of the repository while it hashed a quarter of
		// the copy's bytes, for every request a client polling a broken
		// repository makes. The scrub's verdict and the rebuild's refusals
		// answer it, at once where they stand. A pack the request's hint
		// named whose download is held off is no pack here: the forced round
		// could not fetch it, and would learn nothing the last one did not.
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
// (swappedOut) from the copy serving that copy's files now, AS IT STANDS —
// Kind SyncNone, which the host reads as no repair (host's repair loop) —
// once no round holds it (asItStands). The swap is a write into the copy
// after the request pinned it, and the run that failed read the old
// directory, whose files the new one holds only where it kept them: so the
// request's git runs once more over the copy serving, its repair unspent.
// Damage in a file that left — a retired pack, the multi-pack-index — is
// gone, and that run succeeds; damage in a file the two share fails it again,
// BEGUN AFTER THE SWAP, and its repair is that copy's, in place. Answered by a
// sync of the copy serving, as for a copy a rebuild replaced, the request
// spent its repair on a copy no repair had touched and gave up on damage the
// next repair would have ended (host's maxRepairs) — a request across the
// swap of a repository compacted every few minutes. What failed's reader
// holds stays held on the old copy, for its release to hand on to the copy
// serving (handOn). e.mu is held, and let go.
func (c *Cache) swappedAnswer(ctx context.Context, e *entry) (Synced, error) {
	w := newWaiter(statelog.Freshness{Level: statelog.ReadLinearizable})
	w.request = true
	c.asItStands(e, e.current, w)
	e.mu.Unlock()
	return c.await(ctx, e, w)
}

// reinstallHinted asks inc's next full round to install again over its files
// every pack of packs — those a confirmed hint named (Failure.Packs) — at the
// identity its .pack has now, so a round that finds the pack already
// replaced fetches nothing for it; e.mu is held. A pack the copy lacks is
// installed by the round anyway.
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

// awaitRequest is await for w, a request's repair of the pack or object damage
// its run over failed met: and once that repair has answered the request —
// whatever it answered, the copy repaired, as it stands, or a rebuild refused —
// every report HELD for the request's reader (failed.Repairer) is that
// repair's, and consumed (reports{outcome=request}): the forced round, the
// scrub and the rebuild the repair is made of answer what a stored report, a
// hint or a crash names, so none of them makes the copy's scrub due beside
// it. A request that LEFT before its answer consumes nothing: a caller gone is
// handed to no scrub (Cache.run), so its reports go to the ordinary rules
// with its pin's release (handOn).
func (c *Cache) awaitRequest(ctx context.Context, e *entry, w *waiter, failed Synced) (Synced, error) {
	s, err := c.await(ctx, e, w)
	e.mu.Lock()
	if !w.gone {
		c.consume(failed.inc, failed.Repairer)
	}
	e.mu.Unlock()
	return s, err
}

// awaitInPlace is await for w, a request's IN-PLACE repair of a shape or a
// derived file in the copy failed was handed out of: once it has answered the
// request, every report held for failed's reader is CARRIED to the reader it
// answers (carry, report.go), since that repair read no pack and so answers
// none of them, and the request goes on over the copy it was handed while
// letting failed go. An answer that failed, or a caller that left, carries
// nothing: the request ends, and failed's release hands its reports on.
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
// ScrubCooldown with no repair written into it since it ended — its round's
// own included (incarnation.forcedAt): what another would find, this one has
// found, and what it found waits on the repair mark. e.mu is held.
func (c *Cache) forcedLately(inc *incarnation) bool {
	return !inc.forcedAt.IsZero() && c.now().Sub(inc.forcedAt) < ScrubCooldown &&
		!inc.repairWrite.After(inc.forcedEnded)
}

// asItStands answers w, a failed request the guard says may have failed on
// what a repair since replaced, from inc as it stands — Kind SyncNone, the
// answer the host reads as no repair — once no round holds inc: a round
// running now may be the one repairing it, its write a moment ago one of
// several (the derived files dropped before a pack's files are renamed
// over), and a copy pinned half way through it fails the retry as it failed
// the request. Joined to the round queued behind the running one, a
// linearizable read the repair's last write is behind; answered at once when
// none runs. e.mu is held.
func (c *Cache) asItStands(e *entry, inc *incarnation, w *waiter) {
	if e.running == nil {
		c.answer(e, w, inc, SyncNone, nil)
		return
	}
	w.asIs = true
	c.queue(e, w)
}

// requested is what in-place repairs of a pack's or an object's damage — a
// failed request's (Cache.Rebuild) and a round's own (repairDamaged) — asked
// inc's next full round to do (repairMark.forces and reinstall): nothing while
// a check of the repository is admitted, whose release asks the round again.
// Copied, not taken: the forces are spent once the round's check has run
// (recordCheck), whatever the round meets after, and the packs are taken off
// (settleRequested) only once its install has acted on them, so a round that
// fails before its check leaves both for the next, and one that fails at its
// install leaves the packs. e.mu is not held.
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
// acted on: each still asked at the identity it was asked at — a repair asked
// since keeps its own, and a pack in unrepaired, whose download failed or is
// held off, stays asked. The forces are the check's to spend (recordCheck).
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

// escalate is steps 3 and 4 for ws, the callers of an in-place repair of a
// pack's or an object's damage of inc that it did not end: a request's
// in-place round that repaired nothing (Cache.run), and a round whose own git
// met the damage again past its in-place round, or with that round skipped
// (damagedRound, repairDamaged). The scrub, run for them now (scrubFor); its
// findings repaired by the round it asks, which answers them; and with none,
// the whole rebuild seeded with every pack it found whole — asked as Rebuild
// asks one (askRebuild: joined, refused by the same rules, counted as an ask
// of the incarnation's line), and not asked while a pack it would download is
// held off, the callers answered with the store's refusal of it instead
// (repairMark.heldOffFor). An incarnation replaced meanwhile answers them
// from the current one, by an ordinary round.
//
// THE GUARD IS ASKED AGAIN, before the scrub and before the rebuild: the
// copy is written into while the repair runs — the in-place round installs a
// push it had not yet read, and the scrubber's own repair of the same damage
// (made due by ANOTHER reader's report — one past its first byte, or one
// handed on as its request ended — or by a background run, the failing
// process's own report being held for this repair: report.go, HELD FOR THE
// REQUEST) installs the pack again beside this scrub, which then finds every
// file whole — and a caller whose failure began before such a write is
// answered as the guard answers it (staleAnswered), exactly as Rebuild
// answers one, rather than handed a whole rebuild of a copy that may no
// longer be damaged.
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
				code.ErrRepoNotFound))
		}
	case err != nil:
		for _, w := range ws {
			c.answer(e, w, nil, "", err)
		}
	case e.current != inc || len(out.damaged) > 0:
		// Replaced meanwhile, or what the scrub found is on the repair mark
		// with a round asked: the next round answers. Should that round's own
		// git meet the damage again on the copy the scrub read, the scrub
		// has said what it can: the caller goes on to the whole rebuild,
		// seeded with what it found whole (damagedRound, waiter.scrubbedOn).
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

// predates reports whether a failure whose process began at began — a
// time.Now() reading, zero when unknown — began before this process last
// wrote a repair into inc (incarnation.repairWrite); e.mu is held. Such a
// failure may have been of what that repair replaced.
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

// staleAnswered answers as the guard answers it (standOrSync), counted
// reports{outcome=stale}, every caller in ws whose failure predates a repair
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

// standOrSync answers w, a caller of a pack's or an object's repair whose
// failure predates a repair written into inc, as the guard answers it; e.mu
// is held. A REQUEST is answered from inc as it stands (asItStands): it runs
// its git again over the copy, which tells. A SYNC — a round's own git met the
// damage — is answered only by a round that converged, never from a copy no
// round converged at the freshness it asked: so it is queued for a round of
// inc as it stands, which meets the damage again, and repairs it from the
// first step, or answers it.
func (c *Cache) standOrSync(e *entry, inc *incarnation, w *waiter) {
	if w.request {
		c.asItStands(e, inc, w)
		return
	}
	c.queue(e, w)
	c.start(e)
}

// rebuild is Rebuild's body, for a failed this cache handed out and a reason
// already judged: refuse says whether the rules a rebuild is refused by
// apply. Verify's repair is the one caller that passes false — an operator
// asked for it, and verify has just named the damage a rebuild is for — and
// it still joins a rebuild of the incarnation already queued or running,
// and still counts as an attempt of the incarnation's line. whole is the
// checksums of the packs of failed's incarnation the caller has just held to
// their rows (seedRebuild) — Verify's; nil from a git process's failure,
// which says nothing of which pack is damaged.
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
		// AN OPERATOR'S REPAIR OF A COPY A SWAP REPLACED since its check
		// (Verify): the copy serving its files holds every one it kept as the
		// same inode, the damaged ones among them, so it is the copy rebuilt,
		// seeded with what the check found whole — the same inodes, wherever
		// it kept them.
		failed.inc = e.current
	}
	if e.dead || e.current != failed.inc {
		// Somebody rebuilt it, or a purge removed it, since this request
		// pinned it: the current incarnation answers, or the purge does.
		e.mu.Unlock()
		return c.Sync(ctx, e.id, linearizable)
	}
	if inPlace(reason) {
		// IN PLACE (a shape, or damaged derived files): a linearizable
		// caller joins only the round queued behind the one running, which
		// starts after the mark and so reads the incarnation whole — the
		// derived case dropping the damaged files so the round or the
		// maintainer rewrites them, with no download and no new incarnation
		// (apply).
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

// askRebuild is a rebuild of inc, the entry's current incarnation, asked for
// reason now; e.mu is held. It answers the rebuild round to wait on — the one
// already queued or running for inc, or one it queues — or the rule that
// refuses one (rebuildRefusal) when refuse says the rules apply. Every ask
// counts toward the line's quiet, refused or not: a line of attempts ends when
// the asking stops (RebuildQuiet), measured from the last ask, which this one
// now is. A SHAPE reaches it only past the sweep of the round that met it
// (Cache.run), which repairs every shape the pool refuses in place: a
// directory still refused there is one something keeps writing into, or a
// filesystem not keeping what it is given.
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
		// A SHAPE REFUSAL IS ITSELF PROOF THAT SOMETHING WROTE INTO THE
		// INCARNATION: no round or rebuild makes a directory the pool
		// refuses, so one refused for its shape no longer holds what the last
		// attempt read, and the unchanged rule — another rebuild would make
		// the same directory — is false of it. The cooldown still holds: a
		// rebuild is a download of the whole repository, and a directory
		// something keeps editing past every sweep is not one to download
		// again on every request.
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

// rebuildRefusal is the rule a rebuild of inc is refused by now, or "" when
// it is not; e.mu is held, and a line that went quiet has already been ended
// (Cache.Rebuild). A rebuild re-downloads the whole repository — a GET a
// push, on every node a failing request reaches — so it is attempted only
// where it can still end the failure:
//
//   - an incarnation no rebuild stands behind (inc.rebuilds zero) is rebuilt
//     at once: this is the first attempt at its damage.
//   - one that holds EXACTLY what the last attempt read (inc.unmoved: it is
//     that rebuild's product, or that rebuild failed definitively on its
//     rows, and nothing has written into it since) waits RebuildBackoffCap
//     from that attempt: a rebuild reads the same rows and the same objects
//     again, every byte verified against its row on the way in, and makes
//     the same directory — damage it did not end is the object store's or
//     the rows', which `crewlet code verify` names. A day, rather than for
//     as long as it stays so, because the store can come to hold what it
//     lacked and a directory can be damaged on its own disk, neither of
//     which a rebuild sees coming. Anything that writes into it — a round
//     that moves its packs or refs, writes its index or sweeps a file out of
//     it, the maintainer's index or graph — a restart, an eviction, or a
//     line ended by its quiet ends the refusal sooner.
//   - otherwise it waits rebuildWindow(inc.rebuilds) from the last attempt:
//     something wrote into it since, but nothing a push brings repairs a
//     damaged stored pack or supplies an object a ref's history lacks, so
//     each attempt in a row that did not end the failure doubles the wait.
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

// refusal is a rebuild of e's repository, which name is the name of, refused
// by rule now; e.mu is held. Logged at most once a RebuildCooldown a
// repository — every request that fails on a damaged repository asks again,
// and one line a request would bury the line that says what to do.
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
// naming the repository, the rule's reason and `crewlet code verify`.
func (f refused) err() error {
	if f.rule == refusedUnchanged {
		return fmt.Errorf("%w: %s (%s) holds exactly what a rebuild %v ago read, and that rebuild did not end the "+
			"failure: another reads the same rows and objects and makes the same directory — `crewlet code verify %s` "+
			"names what is damaged", ErrRebuildRefused, f.name, f.repo, f.since, f.name)
	}
	return fmt.Errorf("%w: %s (%s) was rebuilt %v ago, and a rebuild that did not end the failure will not end it a "+
		"second time — `crewlet code verify %s` names what is damaged", ErrRebuildRefused, f.name, f.repo, f.since,
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

// queueRebuild makes the round queued on e a rebuild of inc, starting it when
// nothing runs; e.mu is held. A queued ordinary round is UPGRADED rather than
// left behind the rebuild: a whole read at the log's head answers whatever
// its callers asked for, so they are handed the rebuilt incarnation — and
// should the rebuild fail, each of them is queued again for an ordinary
// round (Cache.run), never answered with the rebuild's failure. whole seeds
// the rebuild (seedRebuild).
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

// seedRebuild links into a rebuild's new incarnation every pack of the one it
// replaces whose checksum is in whole — the packs a check has just held WHOLE
// — so the rebuild's install finds them installed and asks the object store
// only for the rest. Two checks hand it one:
//
//   - Verify, which holds each bundle to its row: every section at its row's
//     size, the bundle hashing to the row's Hash. An operator's repair of one
//     damaged pack among thousands is then one GET, not one a pack the
//     repository holds.
//   - A failing request's or round's scrub (scrubFor, step 3 of the repair of
//     a pack's or an object's damage), which holds each of a pack's three
//     files to its own trailer — the .pack's being its name, the checksum its
//     row names — and counts a pack whole only when all three pass. The whole
//     rebuild that repair asks for last (step 4) links what that scrub
//     vouched for and downloads only the rest: nothing at all when the damage
//     is in the rows rather than in any pack. Its list is not always a hash
//     made for the failure: scrubFor answers from an EARLIER scrub's verdict
//     — one that began after the failing process, or one that found nothing
//     within ScrubCooldown with no repair written since (its recent rule), or
//     one whose findings still wait for their repair, whose packs found whole
//     answer less the ones a waiting finding names — so the list can be up
//     to an hour old, or as old as a finding the store has not yet let the
//     cache repair.
//
// A LINK IS EXACTLY AS GOOD AS A DOWNLOAD for a pack a check has just hashed:
// Verify's list, and a scrub that ran for the failure. Both incarnations are
// this cache's, on one filesystem; a pack's files are read-only and nothing
// of the engine's writes one in place, so the link IS the bytes the check
// hashed; and the rebuild's own full round then holds each to its row's sizes
// and removes whatever no row still names, as it does a download. A REUSED
// verdict links the bytes as they were when that scrub read them: a pack the
// disk damaged in place since — the same identity, so no vouch notices — is
// linked, and vouched, as it is, and the rebuild ends nothing for it. What
// repairs it then is the scrub of the NEW copy, which has no scrub record to
// answer from: the failing retry's report, handed on as the request lets the
// copy go, makes that scrub due at once (report.go), and so does the next
// request's or round's own repair of it (step 3); and the pack is fetched
// again with one GET. Hashing every listed pack again before linking it would
// read the whole copy once more for every failure the hour's verdict answers
// — what the reuse is there to save. What a link cannot bring is a pack no
// check found whole — damaged, missing, or never asked about — which is why
// only whole is linked, and why the one rebuild no check precedes links
// nothing: the one a shape past its round's sweep asks for (damagedRound),
// which says nothing of any pack. And what no link brings is a GET: a rebuild
// that would download a pack whose last download to install it again the
// store refused, within the backoff that holds its next (repairMark.
// heldOffFor), is not asked at all.
//
// A pack gone from replaced since the check held it — a round removed it — is
// passed over, and one that cannot be linked for any other reason is logged
// and left to the download, which needs nothing from it. The directory is
// synced once every link is made, as an install's renames are, before any
// ref names what they hold.
//
// Of the derived files (derived.go) it links only those whose declaration a
// rebuild links (Seed: SeedLink) — the index of pushes, a function of the rows
// a rebuild would read again unchanged (seedIndex). The memo, a function of
// object ids the new incarnation holds as the old did, is no link's: it MOVES
// into the new incarnation whole, one rename, as that is promoted (SeedMove,
// promote's carryMemo), so the replaced copy serves it until then and a
// rebuild that fails leaves it there. Every other is written again over the
// new incarnation's packs — the round's index by its own round, the
// maintainer's index and the commit-graph by the maintainer, the graph first
// — rather than linked from the copy being replaced, whose derived files may
// be what failed.
//
// THE INSTALLED CHAIN IS SEEDED TOO (SeedLink, layers.go): every layer of the
// chain replaced holds, root first, whose every pack was linked above — a
// layer is valid only over its packs — and whose files, linked, pass
// code.CheckLayer there and then against the row it was installed from, over
// the graph layers seeded below it; the first that does not ends the seed,
// since nothing above a layer is valid without it. A layer seeded is no GET:
// the new incarnation records it installed, its files vouched as held to
// their row, its chain files written, so the rebuild's round keeps it, and
// fetches the rest. What a check costs here — one read of each layer's files,
// hashed a piece at a time in a slot of the read lane and stopped with ctx,
// as the round's own check is — a rebuild, rare and downloading whatever it
// does not seed, can afford, and it seeds no layer it did not check.
func (c *Cache) seedRebuild(ctx context.Context, e *entry, replaced, inc *incarnation, whole []string) error {
	c.seedIndex(e, replaced, inc)
	c.seedCodeIndex(e, replaced, inc)
	// EACH PACK WHERE THE COPY REPLACED KEPT IT: a dropping pack into the
	// new incarnation's dropping view, every other into objects/pack. The
	// rebuild's own full round holds each to where its rows place it, and
	// downloads one its rows moved.
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
			// Found whole by the check that seeded it, bundle and all: what
			// it holds is vouched in the new incarnation as in the old.
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

// seedLayers is seedRebuild's chain (above): the layers of replaced's
// installed chain, root first, as far as each names only packs linked and its
// files, linked into inc, pass code.CheckLayer — placed with both chain
// files, vouched, and recorded installed in inc. A hash that reached no
// verdict — no read slot in time, ctx ended — ends the seed as a failing
// layer does, unlogged: it says nothing of the layer, which the rebuild's
// round fetches.
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
	var seeded []code.LayerRow
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

// promote makes a rebuild's incarnation inc current in place of old, which is
// retired — moved into trash/ once the entry is let go if nobody holds it,
// else with its last reader — and records the rebuild: inc stands one attempt
// further along old's line, and holds exactly what that attempt read. The
// memo moves from old into inc first, in one rename (carryMemo): served from
// old until here, and from inc from here on.
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

// rebuilt counts one repair of a copy, for reason: crewlet.code.cache.rebuilds
// under that reason, and the status's windows ([Cache.Status]). THE ONE PLACE
// A REPAIR IS COUNTED — a whole rebuild as it is promoted, an in-place round
// by what it did — so the counter and the windows a person reads beside it
// can never disagree about how many there were, and a node with no recorder
// still counts what its status reports.
func (c *Cache) rebuilt(reason RebuildReason) {
	c.churned(churned{rebuilds: 1})
	if c.metrics != nil {
		c.metrics.Add(metrics.CodeCacheRebuilds, 1, metrics.Attrs{"reason": string(reason)})
	}
}

// failedRebuild records a rebuild of inc that failed with err; e.mu is held.
// inc is still current and serving, one attempt further along its line, and
// — when the failure was the rows' or the stored bytes' own answer, which
// asking again reads the same — it holds what the attempt read as far as a
// rebuild can tell. A failure that may not recur (the store or the estate
// unreachable, the cache stopping) leaves it free to be retried once the
// window has passed.
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
