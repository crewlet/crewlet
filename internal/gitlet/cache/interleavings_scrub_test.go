package cache_test

import (
	"errors"
	"fmt"
	"path/filepath"
	"slices"
	"time"

	"github.com/crewlet/crewlet/internal/gitlet/cache"
	"github.com/crewlet/crewlet/internal/gitlet/gitproc"
	"github.com/crewlet/crewlet/internal/statelog/metrics"
)

// THE REPAIRS' AND THE SCRUB'S OPERATIONS in an interleaving: damage in a
// file this process already vouched for, a git process's report over a copy,
// and the scrub that reads every byte behind the serving.

// damageInPlace flips a byte mid-file in one file of a held copy (a pack, its
// .idx or .rev, or a derived file), its identity kept, while the cache runs:
// media damage after this process vouched for the file, which no check sees.
// A scrub finds it: one a report makes due, a scrub step's, or check's.
func (in *interleaving) damageInPlace(string) string {
	repo, dir, rel := in.heldFile(true, true)
	if rel == "" {
		return "nothing held"
	}
	flipInPlace(in.t, filepath.Join(dir, rel))
	return fmt.Sprintf("%s of repo-%d flipped", filepath.Base(rel), slices.Index(in.repos, repo)+1)
}

// requestMeetsDamage is a request whose git meets media damage in the copy a
// linearizable sync answered, its failure handed to Rebuild as the host hands
// it. Begun after every write into the copy, it must be repaired, never
// answered as it stands: by the forced round (an index or derived file), by
// the scrub that round runs when it found nothing (a pack's body), else by the
// whole rebuild. The file is picked after the sync, whose round may sweep
// files out of the copy (a crashed swap's leaving packs) or move it to its
// next incarnation.
func (in *interleaving) requestMeetsDamage(string) string {
	repo := in.heldRepo()
	if repo == "" {
		return "nothing held"
	}
	s := in.syncAt(repo, linearizable)
	defer s.Release()
	rel := in.fileOf(s.Dir, true, false)
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
// that named damage, as the pool does: a derived file's (a repair round
// queued, which drops the file unless written after the run began), a pack's,
// an object's or a crash (the scrub due at once, or in its cooldown after a
// clean scrub). Half the runs began an hour ago, before anything this process
// wrote; a third are made under an admitted check, which defers a derived
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
// repair mark waits for one: repair rounds and scrubs run on the cache's own
// goroutines, and later steps set its knobs (SetMaxBytes) only between rounds.
//
// A repair whose download failed is HELD OFF (repairMark.fetchFailed) until
// its backoff passes; a killed sync's fault can hit another repository's
// repair round. So settle passes the backoff for such a copy and syncs it.
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

// scrub runs every scrub a day of serving makes due (scrubAll); the round a
// scrub asks repairs its findings.
func (in *interleaving) scrub(string) string {
	return fmt.Sprintf("scrubbed %d", len(in.scrubAll()))
}

// scrubAll runs every scrub due (a copy's first among them), then uses every
// held copy past ScrubInterval and runs the scrubs that makes due, answering
// the repositories scrubbed. Both, because a first scrub reads no index file
// this process vouched for: damage in one after the first round is the
// interval scrub's to find.
//
// EVERY HELD COPY IS USED BEFORE THE DAY AND AGAIN AFTER IT: a copy no full
// round of this process has converged (rounds killed, or refetched by a killed
// sync) is converged by its first use, whose round starts its interval
// (firstScrub). Used only after the day, it would not yet be due, and damage
// from before that round outlives the check.
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

// noteReports records the outcomes the reports and the scrubs reached.
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
