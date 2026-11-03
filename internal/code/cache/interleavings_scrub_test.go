package cache_test

import (
	"errors"
	"fmt"
	"path/filepath"
	"slices"
	"time"

	"github.com/crewlet/crewlet/internal/code/cache"
	"github.com/crewlet/crewlet/internal/code/gitproc"
	"github.com/crewlet/crewlet/internal/statelog/metrics"
)

// THE REPAIRS' AND THE SCRUB'S OPERATIONS in an interleaving: damage that
// lands in a file this process already vouched for, what a git process over
// a copy reports, and the scrub that reads every byte behind the serving.

// damageInPlace flips a byte in the middle of one file of a held
// repository's incarnation — a pack, its index or reverse index, or a derived
// file — through its own inode, its identity kept, with the cache running:
// media damage after this process vouched for the file, which no check sees.
// The scrub finds it: the one a report makes due, a scrub step's, or the one
// every check runs first (check).
func (in *interleaving) damageInPlace(string) string {
	repo, dir, rel := in.heldFile(true)
	if rel == "" {
		return "nothing held"
	}
	flipInPlace(in.t, filepath.Join(dir, rel))
	return fmt.Sprintf("%s of repo-%d flipped", filepath.Base(rel), slices.Index(in.repos, repo)+1)
}

// requestMeetsDamage is a request whose git meets damage of the copy it
// reads: a linearizable sync of a held repository, a file of the copy that
// sync answered flipped in place, as media damage is, and the request's pack
// or object failure handed to Rebuild as the host hands it — begun after
// every write into the copy, so it is repaired, never answered as it stands:
// by the forced round where an index file or a derived one failed, by the
// scrub that round runs when it found nothing (a pack's body), and by the
// whole rebuild only where neither found anything to fetch again. The file
// is picked from the copy the sync answered, never before the sync: its
// round may take files out of the copy in place — the first round after a
// crash left a swap's leaving packs in it sweeps them there — or move the
// repository into its next incarnation.
func (in *interleaving) requestMeetsDamage(string) string {
	repo := in.heldRepo()
	if repo == "" {
		return "nothing held"
	}
	s := in.syncAt(repo, linearizable)
	defer s.Release()
	rel := in.fileOf(s.Dir, true)
	if rel == "" {
		return "nothing held"
	}
	flipInPlace(in.t, filepath.Join(s.Dir, rel))
	reason := []cache.RebuildReason{cache.RebuildCorrupt, cache.RebuildMissing}[in.rng.IntN(2)]
	r, err := in.c.Rebuild(in.t.Context(), s, cache.Failure{Reason: reason, Began: time.Now()})
	switch {
	case errors.Is(err, cache.ErrRebuildRefused):
		return fmt.Sprintf("%s of repo-%d flipped, %s: refused", filepath.Base(rel),
			slices.Index(in.repos, repo)+1, reason)
	case err != nil:
		in.fatalf("Rebuild: %v", err)
		return ""
	}
	r.Release()
	what := "rebuilt"
	switch {
	case r.Kind == cache.SyncNone:
		in.fatalf("a request that began after every write was answered from the copy as it stands")
	case cache.Incarnation(r) == cache.Incarnation(s):
		in.saw("a request repaired in place")
		what = "repaired in place"
	default:
		in.saw("rebuild")
	}
	return fmt.Sprintf("%s of repo-%d flipped, %s: %s", filepath.Base(rel), slices.Index(in.repos, repo)+1,
		reason, what)
}

// report hands the cache's observer a run over repo's current incarnation
// that named damage, as the pool does after every run: a derived file's — a
// repair round queued, which drops the file unless it was written after the
// run began — a pack's or an object's, or a crash — the scrub due at once, or
// in its cooldown after a scrub that found nothing. Half the runs began an
// hour ago, before anything this process wrote. A third of the reports are
// made with a check of the repository admitted, which defers a derived
// report until its release asks the round.
func (in *interleaving) report(repo string) string {
	if _, ok := cache.Repos(in.c)[repo]; !ok {
		return "not held"
	}
	dir := in.w.dir(in.t, in.c, repo)
	began, when := time.Now(), "now"
	if in.rng.IntN(2) == 0 {
		began, when = began.Add(-time.Hour), "an hour ago"
	}
	var ran gitproc.Ran
	what := []string{"commit-graph", "multi-pack-index", "pack", "object", "crash"}[in.rng.IntN(5)]
	switch what {
	case "commit-graph":
		ran = derivedReport(dir, gitproc.PartCommitGraph, began)
	case "multi-pack-index":
		ran = derivedReport(dir, gitproc.PartMultiPackIndex, began)
	case "pack", "object":
		part := map[string]gitproc.Part{"pack": gitproc.PartPack, "object": gitproc.PartObject}[what]
		ran = gitproc.Ran{Dir: dir, Result: gitproc.Result{Began: began,
			Damage: &gitproc.Damage{Kind: gitproc.ErrCorrupt, Parts: part, Began: began}}}
	case "crash":
		ran = gitproc.Ran{Dir: dir, Result: gitproc.Result{Began: began, Crashed: true}}
	}
	var held *cache.Verification
	if in.rng.IntN(3) == 0 {
		if v, err := in.c.AdmitVerify(repo); err == nil {
			held = v
		}
	}
	cache.Report(in.c, ran)
	detail := fmt.Sprintf("a %s report, begun %s", what, when)
	if held != nil {
		held.Release()
		detail += ", a check admitted around it"
	}
	in.settle()
	return detail
}

// settle waits until no repository's round is running or queued and no
// repair mark waits for one: a report's repair round and a scrub's run on the
// cache's own goroutines, and the steps after them set the cache's knobs
// (SetMaxBytes) only between rounds.
//
// A repair whose download failed is HELD OFF (repairMark.fetchFailed) and
// nothing repairs it until its backoff has passed: a killed sync's fault,
// armed for that step's sync, is met by whichever round downloads first, a
// repair round of another repository among them. So settle lets a day pass
// for such a copy and syncs it, and its round installs the pack.
func (in *interleaving) settle() {
	for _, repo := range in.repos {
		if _, ok := cache.Repos(in.c)[repo]; !ok {
			continue
		}
		what := fmt.Sprintf("repo-%d's repair round", slices.Index(in.repos, repo)+1)
		eventually(in.t, what, func() bool { return cache.Settled(in.c, repo) || cache.HeldOff(in.c, repo) })
		if cache.Settled(in.c, repo) {
			continue
		}
		in.clk.Add(cache.RebuildBackoffCap)
		s := in.syncAt(repo, linearizable)
		s.Release()
		eventually(in.t, what, func() bool { return cache.Settled(in.c, repo) })
	}
}

// scrub runs every scrub due, with the clock moved past ScrubInterval and
// every repository used first, so each held copy is due: what a day of
// serving brings. A scrub's findings are repaired by the round it asks.
func (in *interleaving) scrub(string) string {
	return fmt.Sprintf("scrubbed %d", len(in.scrubAll()))
}

// scrubAll runs every scrub due at once — a copy's first among them — and
// then uses every held repository past its interval and runs the scrubs that
// makes due, answering the repositories scrubbed. Both, because a copy's
// first scrub reads no index file this process holds a vouch for: damage
// that landed in one after the copy's first round is the interval's scrub to
// find, a day of serving later.
//
// EVERY HELD COPY IS USED BEFORE THE DAY PASSES, AND AGAIN AFTER IT: a copy no
// full round of this process has converged yet — its rounds killed, or the
// repository evicted and fetched again by a killed sync — is converged by
// that first use, and its interval begins only then (firstScrub: this process
// placed every file of it), so damage that landed in a file of it this
// process had already vouched for is the scrub of a day after that round.
// Used only after the day, such a copy began its interval then and was due
// for nothing, and damage in place before its first round outlived the check.
func (in *interleaving) scrubAll() []string {
	done := cache.ScrubPass(in.c)
	in.settle()
	use := func() {
		for _, repo := range in.repos {
			if _, ok := cache.Repos(in.c)[repo]; ok {
				s := in.syncAt(repo, stale)
				s.Release()
			}
		}
	}
	use()
	in.settle()
	in.clk.Add(cache.ScrubInterval)
	use()
	done = append(done, cache.ScrubPass(in.c)...)
	if len(done) > 0 {
		in.saw("scrub")
	}
	in.settle()
	return done
}

// noteReports records the outcomes the reports and the scrubs reached: a
// derived report that queued a repair round, one found stale by it, one
// deferred under a check, one in its scrub's cooldown, a crash that made the
// scrub due, and damage a scrub found.
func (in *interleaving) noteReports() {
	for outcome, saw := range map[string]string{
		"queued": "a report queued a repair round", "stale": "a stale report", "deferred": "a deferred report",
		"cooldown": "a report in its scrub's cooldown",
	} {
		if counterOf(in.w.rec, metrics.CodeCacheReports, map[string]string{"outcome": outcome}) > 0 {
			in.saw(saw)
		}
	}
	if counterOf(in.w.rec, metrics.CodeCacheReports, map[string]string{"part": "crash", "outcome": "queued"}) > 0 {
		in.saw("a crash made the scrub due")
	}
	for _, trigger := range []string{"scrub", "report"} {
		if counterOf(in.w.rec, metrics.CodeCacheChecks, map[string]string{"trigger": trigger, "outcome": "damaged"}) > 0 {
			in.saw("a scrub found damage")
		}
	}
	if counterOf(in.w.rec, metrics.CodeCacheChecks, map[string]string{"file": "pack", "outcome": "damaged"}) > 0 {
		in.saw("a pack body found damaged")
	}
}
