package cache

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"path/filepath"
	"slices"
	"time"

	"github.com/crewlet/crewlet/internal/code/gitproc"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/statelog/metrics"
)

// REQUEST-TIME REPAIR. A git process the host ran in a cache that git itself
// found damaged — a pack, an index or a multi-pack-index it could not read
// (gitproc.ErrCorrupt), or an object a row names that no installed pack holds
// (gitproc.ErrMissingObject) — or that the pool refused to start there because
// the directory is not the shape the engine writes (gitproc.ErrRepoShape)
// makes the host REPAIR the cache ([Cache.Rebuild]) and retry its request once
// on the answer. [DamageReason] is the one reading of such a failure, the
// host's and the cache's alike.
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
// A REBUILD IS THE NEXT INCARNATION, built BESIDE the one readers hold: a
// whole read at the log's head, every pack downloaded again — but those an
// operator's repair has just found whole, which it links (seedRebuild) —
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
	// gitproc.ErrCorrupt whose damage is NOT confined to the
	// multi-pack-index (RebuildMidx is that): a pack or an index git could
	// not read, which only a whole re-read from the object store repairs.
	RebuildCorrupt RebuildReason = "corrupt"
	// RebuildMidx is damage confined to the multi-pack-index — its reverse
	// index, its bitmap, or the index itself (gitproc.ErrCorrupt whose line
	// names the multi-pack-index: "invalid MIDX object position", "MIDX is
	// likely corrupt", or "...multi-pack-index..."). It is a DERIVED file git
	// only mmaps, written by the maintainer from the packs the incarnation
	// already holds, so it is repaired IN PLACE like a shape: a whole read
	// that DROPS the damaged multi-pack-index (and its bitmap and .rev; the
	// commit-graph, which names no position in it, is kept) so
	// the round or the maintainer writes a sound one over the same packs —
	// no download, no new incarnation, where a whole-repository rebuild
	// would GET every pack again (thousands, for a large repository). Only
	// damage that SURVIVES this in-place repair — a pack the dropped index
	// named is itself damaged — escalates to RebuildCorrupt (repair.go).
	RebuildMidx RebuildReason = "midx"
	// RebuildMissing is a git process in the cache classified
	// gitproc.ErrMissingObject: an object a row names that no installed
	// pack holds.
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
	// identity is not the one the cache read from (ADR-0035: a stream the
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
	// to repair it.
	// The cache's own, never a git process's: Rebuild refuses it.
	RebuildVerify RebuildReason = "verify"
)

// RebuildReasons is every [RebuildReason].
var RebuildReasons = []RebuildReason{RebuildCorrupt, RebuildMidx, RebuildMissing, RebuildShape, RebuildStream,
	RebuildRewound, RebuildVerify}

// gitReasons are the reasons a git process's failure gives through
// [DamageReason] and [ShapeReason] and that reach [askRebuild] — so the ones
// crewlet.code.cache.rebuild.refusals can carry. RebuildMidx is NOT one: it is
// supplied by the host (a fetch whose side band names the multi-pack-index),
// is always repaired in place, and so is never refused.
var gitReasons = []RebuildReason{RebuildCorrupt, RebuildMissing, RebuildShape}

// rebuildReasons are every reason [Cache.Rebuild] takes: the git reasons plus
// the host's RebuildMidx.
var rebuildReasons = append(append([]RebuildReason{}, gitReasons...), RebuildMidx)

// inPlaceReasons are the damage a WHOLE READ IN PLACE repairs — no new
// incarnation, no download: a shape its sweep puts right (RebuildShape), and a
// multi-pack-index its round drops and rewrites over the same packs
// (RebuildMidx). The others replace the incarnation.
var inPlaceReasons = []RebuildReason{RebuildShape, RebuildMidx}

// inPlace reports whether r is repaired by a whole read in place rather than a
// new incarnation.
func inPlace(r RebuildReason) bool { return slices.Contains(inPlaceReasons, r) }

// Valid reports whether r is one of [RebuildReasons].
func (r RebuildReason) Valid() bool { return slices.Contains(RebuildReasons, r) }

// DamageReason is the damage a git process whose directory is one of this
// cache's incarnations failed with names, and whether it is damage the cache
// repairs ([Cache.Rebuild]): [RebuildCorrupt] for gitproc.ErrCorrupt,
// [RebuildMissing] for gitproc.ErrMissingObject, [RebuildShape] for
// gitproc.ErrRepoShape — the pool's refusal to start one there, which names
// the incarnation, since a process run in it names no alternate. Never
// gitproc.ErrNotOurRef, which a want the repository never served and a lost
// pack print alike (the next linearizable sync re-downloads any pack whose
// file is gone), and never a refusal of the call itself (gitproc.ErrConfig
// and its siblings), which no directory causes.
//
// THE PROCESS'S DIRECTORY decides what its failure describes. One run in a
// push's scratch or an advertisement view reads the incarnation only as its
// alternate, and there only its shape is the cache's: the scratch's
// index-pack reads the CLIENT's pack, so its ErrCorrupt is that pack's
// (gitproc.ExitError) — such a caller asks [ShapeReason].
func DamageReason(err error) (RebuildReason, bool) {
	switch {
	case errors.Is(err, gitproc.ErrRepoShape):
		return RebuildShape, true
	case errors.Is(err, gitproc.ErrCorrupt):
		return RebuildCorrupt, true
	case errors.Is(err, gitproc.ErrMissingObject):
		return RebuildMissing, true
	}
	return "", false
}

// ShapeReason is [DamageReason] for a git process whose directory is a
// scratch or an advertisement view of an incarnation rather than the
// incarnation itself, which names the incarnation as its alternate: the
// pool's refusal of THE ALTERNATE's shape (gitproc.ShapeError.InAlternate) is
// the cache's to repair, and nothing else such a process says is — a refusal
// of the scratch's own shape is a directory one request made, which the next
// makes again.
func ShapeReason(err error) (RebuildReason, bool) {
	var shape *gitproc.ShapeError
	if errors.As(err, &shape) && shape.InAlternate {
		return RebuildShape, true
	}
	return "", false
}

// Rebuild repairs the incarnation failed was handed out of, and answers the
// repaired directory pinned. reason is what the host's git process over
// failed.Dir was refused or classified as ([DamageReason], [ShapeReason]):
//
//   - [RebuildShape] reads the incarnation whole IN PLACE (markWhole): the
//     round that answers is a full one that started after the mark, whose
//     sweep repairs every shape the pool refuses and reuses every pack the
//     incarnation holds — no new incarnation, no download, and none of a
//     rebuild's refusals, since it costs a read of the rows. A shape that
//     sweep did not end is rebuilt by the round itself (Cache.run).
//   - [RebuildMidx] reads the incarnation whole IN PLACE the same way, and
//     its round DROPS the damaged multi-pack-index (apply) so it or the
//     maintainer writes a sound one over the same packs — no download —
//     whatever else the incarnation is already marked to be read whole for
//     (a shape, a log recreated or gone back), which the read is then
//     counted under instead (dropMidx). The
//     host asks it only when a fetch's side band names the multi-pack-index;
//     damage it does not end (a pack the index named is itself damaged)
//     escalates to [RebuildCorrupt] on the retry (repair.go).
//   - [RebuildCorrupt] and [RebuildMissing] replace it with the NEXT
//     incarnation, read whole at the log's head: one whose rebuild is queued
//     or running joins it, and one a rebuild could not end the failure of is
//     refused ([ErrRebuildRefused], see rebuildRefusal).
//
// A failed that is no longer its repository's current incarnation — rebuilt
// or purged since it was pinned — is answered by a linearizable [Cache.Sync]
// with no repair. failed stays the caller's to release. ctx bounds the
// caller's wait and nothing else: the repair runs on for anybody else
// waiting on it.
func (c *Cache) Rebuild(ctx context.Context, failed Synced, reason RebuildReason) (Synced, error) {
	if !slices.Contains(rebuildReasons, reason) {
		return Synced{}, fmt.Errorf("%w: a rebuild's reason is what the host's git process over the cache "+
			"failed with, one of %v, and %q is not", ErrInvalid, rebuildReasons, reason)
	}
	if err := c.handedOut(failed, "a rebuild"); err != nil {
		return Synced{}, err
	}
	if c.isStopped() {
		return Synced{}, ErrStopped
	}
	return c.rebuild(ctx, failed, reason, true, nil)
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
func (c *Cache) rebuild(ctx context.Context, failed Synced, reason RebuildReason, refuse bool,
	whole []string) (Synced, error) {
	e := failed.entry
	now := c.now()
	e.mu.Lock()
	linearizable := statelog.Freshness{Level: statelog.ReadLinearizable}
	if e.dead || e.current != failed.inc {
		// Somebody rebuilt it, or a purge removed it, since this request
		// pinned it: the current incarnation answers, or the purge does.
		e.mu.Unlock()
		return c.Sync(ctx, e.id, linearizable)
	}
	if inPlace(reason) {
		// IN PLACE (a shape, or a damaged multi-pack-index): a linearizable
		// caller joins only the round queued behind the one running, which
		// starts after the mark and so reads the incarnation whole — the
		// midx case dropping the damaged index so the round rewrites it, with
		// no download and no new incarnation (apply).
		markWhole(failed.inc, reason)
		w := newWaiter(linearizable)
		c.join(e, w)
		e.mu.Unlock()
		return c.await(ctx, e, w)
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
// replaces whose checksum is in whole — the packs Verify has just held WHOLE
// to their rows: every section at its row's size, the bundle hashing to the
// row's Hash — so the rebuild's install finds them installed and asks the
// object store only for the rest. An operator's repair of one damaged pack
// among thousands is then one GET, not one a pack the repository holds.
//
// A LINK IS EXACTLY AS GOOD AS A DOWNLOAD HERE. Both incarnations are this
// cache's, on one filesystem; a pack's files are read-only and nothing
// writes one in place, so the link IS the bytes Verify hashed; and the
// rebuild's own full round then holds each to its row's sizes and removes
// whatever no row still names, as it does a download. What a link cannot
// bring is a pack Verify did not find whole — damaged, missing, or never
// asked about — which is why only whole is linked, and why the rebuild a
// request's failure asks for, which says nothing of which pack is damaged,
// links nothing.
//
// A pack gone from replaced since Verify held it — a round removed it — is
// passed over, and one that cannot be linked for any other reason is logged
// and left to the download, which needs nothing from it. The directory is
// synced once every link is made, as an install's renames are, before any
// ref names what they hold.
func (c *Cache) seedRebuild(e *entry, replaced, inc *incarnation, whole []string) error {
	from := filepath.Join(replaced.dir, "objects", "pack")
	into := filepath.Join(inc.dir, "objects", "pack")
	seeded := 0
	for _, cs := range whole {
		linked, err := linkBundle(filepath.Join(from, "pack-"+cs), into, cs, nil)
		switch {
		case linked:
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
	return syncDir(into)
}

// promote makes a rebuild's incarnation inc current in place of old, which is
// retired — moved into trash/ once the entry is let go if nobody holds it,
// else with its last reader — and records the rebuild: inc stands one attempt
// further along old's line, and holds exactly what that attempt read.
func (c *Cache) promote(e *entry, old, inc *incarnation, reason RebuildReason, took time.Duration) {
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
	e.mu.Unlock()
	c.trash(gone)
	if c.metrics != nil {
		c.metrics.Add(metrics.CodeCacheRebuilds, 1, metrics.Attrs{"reason": string(reason)})
	}
	c.log.Warn("code_cache_rebuilt", "repo", e.id, "reason", string(reason), "incarnation", inc.n, "took", took)
	c.setGauges()
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
