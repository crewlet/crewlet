package cache_test

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/code"
	"github.com/crewlet/crewlet/internal/code/cache"
	"github.com/crewlet/crewlet/internal/code/gitproc"
	"github.com/crewlet/crewlet/internal/code/gitproc/gittest"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/statelog/metrics"
)

// reportedCopy is a repository of two pushes synced into a cache, its
// multi-pack-index, bitmap and commit-graph written by the maintainer: what
// a report about a derived file is made over.
type reportedCopy struct {
	w    *syncWorld
	clk  *clock
	repo string
	c    *cache.Cache
	s    cache.Synced
}

// newReportedCopy is a reportedCopy over a cache read by clk (a fresh clock
// when nil) with each of opts applied.
func newReportedCopy(t *testing.T, clk *clock, opts ...func(*cache.Options)) *reportedCopy {
	t.Helper()
	if clk == nil {
		clk = newClock()
	}
	w := newSyncWorld(t, onMemory)
	repo := w.repo(t, 1)
	w.push(t, repo, "refs/heads/main", nil)
	w.push(t, repo, "refs/heads/main", nil)
	c := w.open(t, append([]func(*cache.Options){clk.on}, opts...)...)
	r := &reportedCopy{w: w, clk: clk, repo: repo, c: c, s: w.sync(t, c, repo)}
	clk.Add(cache.MidxDebounce)
	eventually(t, "the maintainer's index, bitmap and graph", func() bool {
		if !cache.Maintaining(c, repo) {
			cache.MaintainerPass(c)
		}
		return r.midx() != "" && r.bitmap() != "" && r.graph() != "" && !cache.Maintaining(c, repo)
	})
	return r
}

// midx, bitmap and graph are the copy's multi-pack-index, its bitmap and its
// commit-graph, each "" while it is not on disk.
func (r *reportedCopy) midx() string {
	return present(filepath.Join(r.s.Dir, "objects", "pack", "multi-pack-index"))
}

func (r *reportedCopy) bitmap() string {
	found, _ := filepath.Glob(filepath.Join(r.s.Dir, "objects", "pack", "multi-pack-index-*.bitmap"))
	if len(found) != 1 {
		return ""
	}
	return found[0]
}

func (r *reportedCopy) graph() string {
	return present(filepath.Join(r.s.Dir, "objects", "info", "commit-graph"))
}

// present is path, or "" when nothing is there.
func present(path string) string {
	if _, err := os.Lstat(path); err != nil {
		return ""
	}
	return path
}

// stat is the file at path, which must exist.
func stat(t *testing.T, path string) os.FileInfo {
	t.Helper()
	info, err := os.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}
	return info
}

// quiet waits until no round runs, is queued or is asked for the copy, and
// no maintenance run is in flight on it.
func (r *reportedCopy) quiet(t *testing.T) {
	t.Helper()
	eventually(t, "the copy quiet", func() bool {
		return cache.Settled(r.c, r.repo) && !cache.Maintaining(r.c, r.repo)
	})
}

// rewritten waits until the file find names is on disk as a file other than
// old — written again, not the damaged one kept — with nothing running on
// the copy, running the maintainer's pass whenever the copy is quiet without
// it (the clock never moved, so only what is due at once runs), and fails t
// unless every index file of the copy then holds to its trailer. It answers
// the new file.
func (r *reportedCopy) rewritten(t *testing.T, what string, find func() string, old os.FileInfo) os.FileInfo {
	t.Helper()
	var now os.FileInfo
	eventually(t, what+" written again", func() bool {
		if !cache.Settled(r.c, r.repo) || cache.Maintaining(r.c, r.repo) {
			return false
		}
		path := find()
		if path == "" {
			// The maintainer's pass, which a running cache's round wakes
			// (this one is never started): what is due at once runs.
			cache.MaintainerPass(r.c)
			return false
		}
		info, err := os.Lstat(path)
		if err != nil || os.SameFile(info, old) {
			return false
		}
		now = info
		return true
	})
	if bad, err := cache.Unsound(r.s.Dir); err != nil || len(bad) != 0 {
		t.Errorf("after the repair, %v fail their trailers (%v)", bad, err)
	}
	return now
}

// derivedReport is a report of a run over dir that began at began and named
// parts of a derived file damaged, as the pool hands one to its observer.
func derivedReport(dir string, parts gitproc.Part, began time.Time) gitproc.Ran {
	return gitproc.Ran{Dir: dir, Result: gitproc.Result{Began: began,
		Damage: &gitproc.Damage{Kind: gitproc.ErrCorrupt, Parts: parts, Began: began}}}
}

// unobserved is a git pool no cache observes: what a case runs a process
// through when it hands the Result over itself, at the moment it chooses
// (cache.Report).
func unobserved(t *testing.T) *gitproc.Pool {
	t.Helper()
	layout, err := cache.Prepare(t.Context(), filepath.Join(t.TempDir(), "unobserved"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(layout.Release)
	return newPool(t, layout)
}

// runIn runs one read in dir through pool and answers its Result.
func runIn(t *testing.T, pool *gitproc.Pool, dir string, args ...string) (gitproc.Result, error) {
	t.Helper()
	return pool.Run(t.Context(), gitproc.Cmd{Dir: dir, Args: args, Class: gitproc.ClassRead, Deadline: time.Minute})
}

// reports is the crewlet.code.cache.reports count of part with outcome.
func reports(w *syncWorld, part, outcome string) uint64 {
	return counterOf(w.rec, metrics.CodeCacheReports, map[string]string{"part": part, "outcome": outcome})
}

// roundHold holds the next full round after its sweep — the entry's write
// lock held — once armed, until released.
type roundHold struct {
	armed   atomic.Bool
	reached chan struct{}
	release chan struct{}
}

func newRoundHold() *roundHold {
	return &roundHold{reached: make(chan struct{}), release: make(chan struct{})}
}

// at is the AfterSweep knob.
func (h *roundHold) at(string) {
	if h.armed.CompareAndSwap(true, false) {
		close(h.reached)
		<-h.release
	}
}

// wait waits for the held round to reach the hold.
func (h *roundHold) wait(t *testing.T) {
	t.Helper()
	select {
	case <-h.reached:
	case <-time.After(hangDetector):
		t.Fatalf("no round reached the hold within %v", hangDetector)
	}
}

// A DERIVED FILE ANY RUN NAMES DAMAGED IS DROPPED AND WRITTEN AGAIN AT ONCE —
// whether or not the run failed. git reads around a damaged commit-graph
// (I), a truncated multi-pack-index (F2) and a bitmap whose signature is
// overwritten (K1): it warns, answers from the packs and exits 0, and a
// caller that got its answer never looks. The pool hands every run to the
// cache's observer, which queues a repair round with no request waiting:
// the round drops the file, and the maintainer writes it again AT ONCE — the
// graph first, the index as a repair — with the case's clock never moved
// past the debounce or the interval. Meanwhile the copy keeps answering: a
// reader at once is answered from it while the repair round holds the
// repository, because a tolerated report takes nothing out of service.
//
// Mutations: observe only failed runs (no round, the damage stays); no
// indexRepair (the index waits MidxMinInterval); markWhole for a tolerated
// report (the reader at once waits for the held round).
func TestAReportedDerivedFileIsDroppedAndRewrittenAtOnce(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name   string
		damage gittest.DamageCase
		file   func(*reportedCopy) string
		part   string
		args   func(r *reportedCopy) []string
	}{
		{"I a commit-graph's signature", gittest.GraphSignature, (*reportedCopy).graph, "commit-graph",
			func(r *reportedCopy) []string { return []string{"rev-list", "--objects", r.w.repos[r.repo].head} }},
		{"F2 a multi-pack-index truncated", gittest.MidxTruncated, (*reportedCopy).midx, "multi-pack-index",
			func(r *reportedCopy) []string { return []string{"rev-list", "--objects", r.w.repos[r.repo].head} }},
		{"K1 a bitmap's signature", gittest.BitmapSignature, (*reportedCopy).bitmap, "multi-pack-index",
			func(*reportedCopy) []string {
				return []string{"rev-list", "--objects", "--use-bitmap-index", "--count", "--all"}
			}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			h := newRoundHold()
			defer close(h.release)
			r := newReportedCopy(t, nil, func(o *cache.Options) { cache.AfterSweep(o, h.at) })
			gittest.Damage(t, r.s.Dir, tc.damage)
			damaged := stat(t, tc.file(r))

			h.armed.Store(true)
			res, err := runIn(t, r.w.pool, r.s.Dir, tc.args(r)...)
			var d *gitproc.Damage
			if err != nil || !errors.As(res.Damage, &d) {
				t.Fatalf("the read over %s answered %v naming %v: the case wants a run git answered over "+
					"damage it reported", tc.damage, err, res.Damage)
			}

			// The repair round runs, held; the copy answers at once anyway.
			h.wait(t)
			if !cache.WriteLockHeld(r.c, r.repo) {
				t.Fatal("the held round holds no write lock: the case holds nothing")
			}
			ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
			at, err := r.c.Sync(ctx, r.repo, statelog.Freshness{Level: statelog.ReadStale})
			cancel()
			if err != nil {
				t.Fatalf("a reader at once while the repair round ran: %v, want answered from the copy", err)
			}
			at.Release()
			h.release <- struct{}{}

			r.rewritten(t, tc.name, func() string { return tc.file(r) }, damaged)
			if got := reports(r.w, tc.part, "queued"); got == 0 {
				t.Errorf("reports{part=%s,outcome=queued} = 0, want the run's report counted", tc.part)
			}
			lines := r.w.logs.lines(t, "code_cache_derived_repaired")
			if !slices.ContainsFunc(lines, func(l map[string]any) bool { return l["trigger"] == "report" }) {
				t.Errorf("code_cache_derived_repaired = %v, want a line with trigger report", lines)
			}
			if got := counter(r.w.rec, metrics.CodeCacheRebuilds, "reason", "derived"); got != 0 {
				t.Errorf("%s{reason=derived} = %d: a report reads nothing whole", metrics.CodeCacheRebuilds, got)
			}
		})
	}
}

// A REPORT A DECLARATION KEEPS ITS FILES ON RECORDS NO DROP, AND ASKS NONE.
// A report of the multi-pack-index is EventReported for the index's
// declaration, and what it does is the declaration's to say (OnReported).
// With the build's declarations it drops: one repair round drops the index,
// logs it repaired and makes its rewrite due at once, and the copy's scrub is
// not asked. Over declarations in which the index keeps its files on a
// report, the report is damage no drop repairs: no round is queued — one
// would read the copy whole and move nothing, for every git process that
// reported it — nothing says the index was repaired, the same file stays on
// disk, and the report makes the copy's scrub due, where the bytes decide.
// That scrub over a sound index finds nothing, and the next report falls in
// its cooldown: still no round. And what the bytes decide is no report: the
// same index damaged in place, its next scrub finds it failing its trailer
// and the repair round drops it, logged and due, whatever its declaration
// says of a report.
//
// Mutations: queue a repair round for every declaration a report names
// (report.go observe) — the kept arm sweeps the copy for the report and
// makes no scrub due; drop what a check or a scrub found as a report
// (dropDamaged's eventFailedCheck as EventReported) — the kept arm keeps the
// index failing its trailer, logs nothing and makes nothing due.
func TestAReportADeclarationKeepsItsFilesOnRecordsNoDrop(t *testing.T) {
	t.Parallel()
	keep := cache.DerivedFiles()
	for i := range keep {
		if keep[i].Name == cache.DerivedMidx {
			keep[i].OnReported = cache.Keep
		}
	}
	for _, tc := range []struct {
		name    string
		decls   []cache.Derived
		dropped bool
	}{
		{"the build's declarations", cache.DerivedFiles(), true},
		{"the index kept on a report", keep, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var sweeps atomic.Int64
			r := newReportedCopy(t, nil, cache.WithDerived(tc.decls), func(o *cache.Options) {
				cache.AfterSweep(o, func(string) { sweeps.Add(1) })
			})
			midx := r.midx()
			before := stat(t, midx)
			swept := sweeps.Load()
			report := func() {
				t.Helper()
				cache.Report(r.c, derivedReport(r.s.Dir, gitproc.PartMultiPackIndex, time.Now()))
				eventually(t, "every round the report asked", func() bool { return cache.Settled(r.c, r.repo) })
			}

			report()
			if got := reports(r.w, "multi-pack-index", "queued"); got != 1 {
				t.Fatalf("reports{part=multi-pack-index,outcome=queued} = %d, want the report counted once: the "+
					"case holds no report", got)
			}
			rounds := sweeps.Load() - swept
			if want := map[bool]int64{true: 1, false: 0}[tc.dropped]; rounds != want {
				t.Errorf("the report swept the copy in %d full rounds, want %d", rounds, want)
			}
			after, err := os.Lstat(midx)
			if kept := err == nil && os.SameFile(before, after); kept == tc.dropped {
				t.Errorf("the index after the report: kept %t (%v), want kept %t", kept, err, !tc.dropped)
			}
			lines := r.w.logs.lines(t, "code_cache_derived_repaired")
			if got := len(lines) > 0; got != tc.dropped {
				t.Errorf("code_cache_derived_repaired = %v, want logged %t", lines, tc.dropped)
			}
			if got := cache.IndexRepair(r.c, r.repo); got != tc.dropped {
				t.Errorf("the index due at once as a repair: %t, want %t", got, tc.dropped)
			}
			why, _ := cache.ScrubDue(r.c, r.repo)
			if want := map[bool]string{true: "", false: "report"}[tc.dropped]; why != want {
				t.Fatalf("after the report the copy's scrub is due for %q, want %q", why, want)
			}
			if tc.dropped {
				return
			}

			// The scrub the report asked, over a sound index: nothing found,
			// and the next report of the same index is in its cooldown.
			if done := cache.ScrubPass(r.c); !slices.Equal(done, []string{r.repo}) {
				t.Fatalf("the scrub pass scrubbed %v, want %s", done, r.repo)
			}
			if _, found, _ := cache.Scrubbed(r.c, r.repo); found {
				t.Fatal("the scrub of a sound copy found something")
			}
			report()
			if got := reports(r.w, "multi-pack-index", "cooldown"); got != 1 {
				t.Errorf("reports{part=multi-pack-index,outcome=cooldown} = %d, want the second report in the "+
					"scrub's cooldown", got)
			}
			if got := reports(r.w, "multi-pack-index", "queued"); got != 1 {
				t.Errorf("reports{part=multi-pack-index,outcome=queued} = %d after the second report, want the "+
					"first alone", got)
			}
			if rounds := sweeps.Load() - swept; rounds != 0 {
				t.Errorf("two reports of a kept index swept the copy in %d full rounds, want none", rounds)
			}

			// What the bytes decide is not the declaration's to keep: past the
			// cooldown, the index damaged in place — its identity kept, as
			// media damage keeps it — is reported again, the scrub that report
			// asks finds it failing its trailer, and the repair round drops it
			// and makes it due. Kept, it would be served failing its check
			// and found again by every scrub a report of it asked.
			r.clk.Add(cache.ScrubCooldown)
			gittest.DamageInPlace(t, r.s.Dir, gittest.MidxTrailer)
			report()
			if done := cache.ScrubPass(r.c); !slices.Equal(done, []string{r.repo}) {
				t.Fatalf("the scrub pass after the index was damaged scrubbed %v, want %s", done, r.repo)
			}
			eventually(t, "the scrub's findings repaired", func() bool {
				return cache.Settled(r.c, r.repo) && len(cache.Findings(r.c, r.repo)) == 0
			})
			if after, err := os.Lstat(midx); err == nil && os.SameFile(before, after) {
				t.Error("the index failing its trailer is still on disk: a declaration that keeps its files on a " +
					"report kept what its check failed")
			}
			lines = r.w.logs.lines(t, "code_cache_derived_repaired")
			if !slices.ContainsFunc(lines, func(l map[string]any) bool { return l["trigger"] == "scrub" }) {
				t.Errorf("code_cache_derived_repaired = %v, want the scrub's drop", lines)
			}
			if !cache.IndexRepair(r.c, r.repo) {
				t.Error("the index the scrub's round dropped is not due at once as a repair")
			}
		})
	}
}

// A SCRATCH'S DERIVED REPORT IS ITS ALTERNATE'S, AND NOTHING ELSE IT REPORTS
// IS. A process in a push's scratch reads the incarnation only as its
// alternate, and reads the alternate's derived files: a merge-base there
// over a damaged commit-graph names the graph, and the incarnation's graph is
// dropped and written again. Its pack and object parts are another matter:
// index-pack reading a client's pack cut short says `early EOF`, a pack's
// format — the CLIENT's pack — so it is counted `unrouted` and queues
// nothing.
//
// Mutation: route a scratch's pack parts to its alternate (the unrouted count
// stays zero, and the incarnation's scrub is made due).
func TestAScratchsDerivedReportIsItsAlternates(t *testing.T) {
	t.Parallel()
	r := newReportedCopy(t, nil)
	gittest.Damage(t, r.s.Dir, gittest.GraphSignature)
	damaged := stat(t, r.graph())
	sc, err := r.c.NewScratch(t.Context(), r.s, uuid.Must(uuid.NewV7()).String())
	if err != nil {
		t.Fatalf("NewScratch: %v", err)
	}
	defer sc.Remove()

	tips := r.w.repos[r.repo].tips
	res, err := r.w.pool.Run(t.Context(), gitproc.Cmd{Dir: sc.Dir, Alternate: sc.Alternate,
		Args: []string{"merge-base", "--is-ancestor", tips[0], tips[1]}, Class: gitproc.ClassRead, Deadline: time.Minute})
	var d *gitproc.Damage
	if err != nil || !errors.As(res.Damage, &d) || d.Parts&gitproc.PartCommitGraph == 0 {
		t.Fatalf("merge-base in the scratch answered %v naming %v, want it to name its alternate's graph", err,
			res.Damage)
	}
	r.rewritten(t, "the alternate's commit-graph", r.graph, damaged)
	if got := reports(r.w, "commit-graph", "queued"); got != 1 {
		t.Errorf("reports{part=commit-graph,outcome=queued} = %d, want the scratch's report", got)
	}

	packs := packFiles(t, r.s.Dir)
	var pack []byte
	for _, name := range packs {
		if filepath.Ext(name) == ".pack" {
			if pack, err = os.ReadFile(filepath.Join(r.s.Dir, "objects", "pack", name)); err != nil {
				t.Fatal(err)
			}
			break
		}
	}
	res, err = r.w.pool.Run(t.Context(), gitproc.Cmd{Dir: sc.Dir, Alternate: sc.Alternate,
		Args: []string{"index-pack", "--stdin"}, Stdin: bytes.NewReader(pack[:len(pack)/2]),
		Class: gitproc.ClassRead, Deadline: time.Minute})
	if err == nil || !errors.As(res.Damage, &d) || d.Parts&gitproc.PartPack == 0 {
		t.Fatalf("index-pack of a pack cut short answered %v naming %v, want a pack's damage", err, res.Damage)
	}
	if got := reports(r.w, "pack", "unrouted"); got != 1 {
		t.Errorf("reports{part=pack,outcome=unrouted} = %d, want the client's pack counted and left", got)
	}
	if cache.RepairPending(r.c, r.repo) || !cache.Settled(r.c, r.repo) {
		t.Error("a scratch's pack report asked the incarnation for a round")
	}
	if why, _ := cache.ScrubDue(r.c, r.repo); why != "" {
		t.Errorf("a scratch's pack report made the incarnation's scrub due for %q", why)
	}
}

// A MERGE'S REPORT IS ITS INCARNATION'S. A compaction's build runs in a
// scratch over a store holding nothing but the incarnation's own packs,
// linked, and reads nothing else: what it names of a pack — never a client's,
// as a push's scratch's may be — is that copy's, and makes the copy's scrub
// due, where a push's scratch's is counted unrouted; a derived part, of which
// the store holds none, is nobody's and asks nothing. Once its scratch is
// removed the store is no merge's, and a report naming it reaches nothing.
//
// Mutation: route a merge's report as a push's scratch's (its pack part
// counted unrouted, the scrub not made due).
func TestAMergesReportIsItsIncarnations(t *testing.T) {
	t.Parallel()
	w := newSyncWorld(t, onMemory)
	repo := w.repo(t, 1)
	_, p1 := w.push(t, repo, "refs/heads/main", nil)
	_, p2 := w.push(t, repo, "refs/heads/main", nil)
	c := w.open(t)
	s := w.sync(t, c, repo)
	sc, err := c.NewMergeScratch(t.Context(), s, uuid.Must(uuid.NewV7()).String(),
		[]string{p1.GitChecksum, p2.GitChecksum})
	if err != nil {
		t.Fatalf("NewMergeScratch: %v", err)
	}
	report := func(parts gitproc.Part) {
		t.Helper()
		began := time.Now()
		cache.Report(c, gitproc.Ran{Dir: sc.Dir, Alternate: sc.Alternate, Result: gitproc.Result{Began: began,
			Damage: &gitproc.Damage{Kind: gitproc.ErrCorrupt, Parts: parts, Began: began}}})
	}

	report(gitproc.PartCommitGraph)
	if got := reports(w, "commit-graph", "unrouted"); got != 1 {
		t.Errorf("reports{part=commit-graph,outcome=unrouted} = %d, want the merge's derived report counted and left",
			got)
	}
	if why, _ := cache.ScrubDue(c, repo); why != "" || cache.RepairPending(c, repo) {
		t.Errorf("a merge's derived report asked the copy for a round (%t) or a scrub (%q)",
			cache.RepairPending(c, repo), why)
	}

	report(gitproc.PartPack)
	if got := reports(w, "pack", "queued"); got != 1 {
		t.Errorf("reports{part=pack,outcome=queued} = %d, want the merge's pack report the copy's", got)
	}
	if got := reports(w, "pack", "unrouted"); got != 0 {
		t.Errorf("reports{part=pack,outcome=unrouted} = %d: a merge's pack is the incarnation's, never a client's",
			got)
	}
	if why, _ := cache.ScrubDue(c, repo); why == "" {
		t.Error("a merge's pack report left the copy's scrub not due")
	}

	sc.Remove()
	queued := reports(w, "pack", "queued")
	report(gitproc.PartPack)
	if got := reports(w, "pack", "queued") + reports(w, "pack", "unrouted"); got != queued {
		t.Errorf("a report naming a removed merge's store was counted (%d reports, %d before): it is no merge's",
			got, queued)
	}
}

// A REPORT ABOUT A FILE SINCE REWRITTEN DROPS NOTHING. A run that began
// before this process wrote a derived file may have read the file the write
// replaced, so its report — however late it arrives — names a file that is
// gone: a long clone over a damaged graph would otherwise drop each rewrite
// in turn. Held on the MONOTONIC clock: the cache's own clock here is an hour
// behind, and a stamp read from it would place the rewrite before the run.
//
// Mutations: drop the Began guard (the fresh graph is dropped); stamp
// written with Options.Now (the same).
func TestAReportAboutAFileSinceRewrittenDropsNothing(t *testing.T) {
	t.Parallel()
	clk := newClock()
	clk.Set(time.Now().Add(-time.Hour))
	r := newReportedCopy(t, clk)
	gittest.Damage(t, r.s.Dir, gittest.GraphSignature)
	damaged := stat(t, r.graph())

	// A run over the damaged graph, its report held back.
	res, err := runIn(t, unobserved(t), r.s.Dir, "rev-list", "--objects", r.w.repos[r.repo].head)
	var d *gitproc.Damage
	if err != nil || !errors.As(res.Damage, &d) || d.Parts&gitproc.PartCommitGraph == 0 {
		t.Fatalf("the read over the damaged graph answered %v naming %v", err, res.Damage)
	}
	// The graph is dropped and written again since.
	cache.Report(r.c, derivedReport(r.s.Dir, gitproc.PartCommitGraph, time.Now()))
	fresh := r.rewritten(t, "the commit-graph", r.graph, damaged)
	repaired := len(r.w.logs.lines(t, "code_cache_derived_repaired"))

	// The held report arrives.
	cache.Report(r.c, gitproc.Ran{Dir: r.s.Dir, Result: res})
	r.quiet(t)
	if now := r.graph(); now == "" || !os.SameFile(stat(t, now), fresh) {
		t.Errorf("a report about the graph before its rewrite dropped the rewrite (now %q)", now)
	}
	if got := reports(r.w, "commit-graph", "stale"); got != 1 {
		t.Errorf("reports{part=commit-graph,outcome=stale} = %d, want the late report", got)
	}
	if got := len(r.w.logs.lines(t, "code_cache_derived_repaired")); got != repaired {
		t.Errorf("code_cache_derived_repaired went from %d lines to %d on a stale report", repaired, got)
	}
}

// A FAILED REQUEST'S DROP SPARES A FILE WRITTEN AFTER IT BEGAN. pre7's path —
// a request whose git failed on a damaged multi-pack-index asks Rebuild to
// drop it — carries when its run began (cache.Failure.Began), and a
// multi-pack-index this process wrote after that is not the file the run
// failed on: the round drops nothing.
//
// Mutation: drop unguarded, as pre7 did (the fresh index is dropped).
func TestAFailedRequestsDropSparesAFileWrittenAfterItBegan(t *testing.T) {
	t.Parallel()
	r := newReportedCopy(t, nil)
	gittest.Damage(t, r.s.Dir, gittest.MidxSignature)
	damaged := stat(t, r.midx())
	_, err := runIn(t, unobserved(t), r.s.Dir, "rev-list", "--objects", r.w.repos[r.repo].head)
	f, ok := cache.DamageReason(err)
	if !ok || f.Reason != cache.RebuildDerived || f.Began.IsZero() {
		t.Fatalf("the read over the damaged index failed %v, read as %+v, %v: want a derived failure with "+
			"its Began", err, f, ok)
	}
	cache.Report(r.c, derivedReport(r.s.Dir, gitproc.PartMultiPackIndex, time.Now()))
	fresh := r.rewritten(t, "the multi-pack-index", r.midx, damaged)

	repaired, err := r.c.Rebuild(t.Context(), r.s, f)
	if err != nil {
		t.Fatalf("Rebuild: %v", err)
	}
	defer repaired.Release()
	r.quiet(t)
	if now := r.midx(); now == "" || !os.SameFile(stat(t, now), fresh) {
		t.Errorf("the request's drop took the index written after its run began (now %q)", now)
	}
	if got := reports(r.w, "multi-pack-index", "stale"); got != 1 {
		t.Errorf("reports{part=multi-pack-index,outcome=stale} = %d, want the request's drop spared", got)
	}
}

// NOTHING IS REPAIRED UNDER A CHECK. A check holds rounds off only for its
// listing, so a repair between its listing and its hashing would take a
// file the listing admitted: what a run reports while a check is admitted is
// recorded and DEFERRED, and the check's release asks the round. Here the
// check's own freshness sync peels a tag over a truncated multi-pack-index
// (F2), which git reads around and reports; the check still finds the index
// and names it, and only once it is let go is the index dropped and written
// again.
//
// Mutations: start the repair round at once (the check's listing misses the
// index, or its hashing finds it gone); never start it after the release
// (the index stays damaged).
func TestNothingIsRepairedUnderACheck(t *testing.T) {
	t.Parallel()
	r := newReportedCopy(t, nil)
	gittest.Damage(t, r.s.Dir, gittest.MidxTruncated)
	damaged := stat(t, r.midx())
	r.w.tag(t, r.repo, "v1", true) // the check's freshness sync peels it, loading the index

	var v *cache.Verification
	eventually(t, "the check admitted", func() bool {
		var err error
		v, err = r.c.AdmitVerify(r.repo)
		return err == nil
	})
	report, err := v.Run(t.Context(), cache.VerifyRun{})
	if err != nil {
		v.Release()
		t.Fatalf("the check: %v", err)
	}
	if got := reports(r.w, "multi-pack-index", "deferred"); got == 0 {
		t.Error("the freshness sync's report of the index was not deferred under the check")
	}
	problemsOf(t, "the check's problems", report.Problems, "multi-pack-index")
	if now := r.midx(); now == "" || !os.SameFile(stat(t, now), damaged) {
		t.Errorf("the damaged index was taken under the check (now %q)", now)
	}
	v.Release()
	r.rewritten(t, "the multi-pack-index", r.midx, damaged)
}

// A REPAIR ROUND SWEEPS NO ADOPTED PACK. A full round's sweep removes every
// pack its snapshot does not name, and drops the index and the graph with
// it; an adoption installs a pack once its row has applied, past the
// incarnation's own position. So the repair round a report queues reads
// LINEARIZABLE: its snapshot names the adopted pack, which stays, and so does
// the index beside it — only the graph the report named is dropped. The log
// here answers any other level from before the adoption, as a data node
// behind it would.
//
// Mutation: read the repair round at session from the incarnation's position
// (the adopted pack is swept, and the index with it).
func TestARepairRoundSweepsNoAdoptedPack(t *testing.T) {
	t.Parallel()
	r := newReportedCopy(t, nil)
	f := r.w.repos[r.repo]
	before := f.head
	a := r.w.receiveOver(t, r.c, r.s, r.repo)
	if err := r.c.Adopt(t.Context(), r.repo, a.sc, a.row.Ref()); err != nil {
		t.Fatalf("Adopt: %v", err)
	}
	adopted := filepath.Join(r.s.Dir, "objects", "pack", "pack-"+a.row.GitChecksum+".pack")
	if present(adopted) == "" {
		t.Fatal("the adoption installed nothing")
	}
	index := stat(t, r.midx())
	graph := stat(t, r.graph())
	r.w.log.set(func(l *fakeLog) {
		l.after = func(call fakeCall, snap *code.RepoSnapshot) {
			if call.Level == statelog.ReadLinearizable || call.Page != 1 {
				return
			}
			snap.Packs = slices.DeleteFunc(snap.Packs, func(p code.PackRow) bool { return p.PackID == a.row.PackID })
			for i := range snap.Refs {
				if snap.Refs[i].Ref == "refs/heads/main" {
					snap.Refs[i].OID = before
				}
			}
		}
	})

	cache.Report(r.c, derivedReport(r.s.Dir, gitproc.PartCommitGraph, time.Now()))
	r.rewritten(t, "the commit-graph", r.graph, graph)
	if present(adopted) == "" {
		t.Error("the repair round swept the adopted pack")
	}
	if now := r.midx(); now == "" || !os.SameFile(stat(t, now), index) {
		t.Errorf("the repair round of the graph took the index beside the adopted pack (now %q)", now)
	}
}

// A DERIVED FILE DAMAGED AGAIN AFTER ITS REPAIR WAITS ITS TURN. A graph a
// repair wrote that is reported damaged again is a disk that keeps damaging
// what is written to it, or a git that rejects its own write: it is dropped,
// but not written again at once — the graph waits for the ordinary rules
// (graphFailed) — logged once and counted `again`, so the loop costs one
// rewrite rather than one a report. A report of the graph once it is gone
// reaches no file of the copy — counted stale, nothing dropped, no round —
// and the graph stays waiting. The ordinary write that follows ends
// nothing: it lands on the same disk, and a report of it within RebuildQuiet
// of the last drop for damage waits its turn too, where a mark the ordinary
// write cleared made every other damage a rewrite at once — one at once and
// one ordinary, alternating for ever. A RebuildQuiet after the last drop for
// damage, damage is new again and is repaired at once.
//
// Mutation: no D13 — every drop for damage is made due at once (a second
// graph-first run writes the graph again). Mutation: the maintainer's
// ordinary graph write forgets the last drop for damage (the report after
// it is repaired at once).
func TestADerivedFileDamagedAgainAfterItsRepairWaitsItsTurn(t *testing.T) {
	t.Parallel()
	r := newReportedCopy(t, nil)
	cache.Report(r.c, derivedReport(r.s.Dir, gitproc.PartCommitGraph, time.Now()))
	r.rewritten(t, "the commit-graph", r.graph, stat(t, r.graph()))

	for range 2 {
		cache.Report(r.c, derivedReport(r.s.Dir, gitproc.PartCommitGraph, time.Now()))
		r.quiet(t)
		cache.MaintainerPass(r.c)
		r.quiet(t)
		if r.graph() != "" {
			t.Fatal("the graph a repair wrote, reported again, was written again at once")
		}
	}
	if !cache.GraphFailed(r.c, r.repo) {
		t.Error("the graph is not left to the ordinary rules")
	}
	if got := reports(r.w, "commit-graph", "again"); got != 1 {
		t.Errorf("reports{part=commit-graph,outcome=again} = %d, want the report after the repair", got)
	}
	if got := reports(r.w, "commit-graph", "stale"); got != 1 {
		t.Errorf("reports{part=commit-graph,outcome=stale} = %d, want the report of the graph once it was gone",
			got)
	}

	// The ordinary rules write it — an index run, the graph after it — and
	// the disk damages that write too.
	r.ordinaryGraph(t)
	cache.Report(r.c, derivedReport(r.s.Dir, gitproc.PartCommitGraph, time.Now()))
	r.quiet(t)
	cache.MaintainerPass(r.c)
	r.quiet(t)
	if r.graph() != "" {
		t.Error("the graph the maintainer wrote ordinarily, reported within RebuildQuiet of the last drop " +
			"for damage, was written again at once")
	}
	if got := reports(r.w, "commit-graph", "again"); got != 2 {
		t.Errorf("reports{part=commit-graph,outcome=again} = %d, want the report after the ordinary write "+
			"counted with the one before it", got)
	}

	// A RebuildQuiet after the last drop for damage, damage reported is new
	// and repaired at once.
	r.clk.Add(cache.RebuildQuiet)
	r.ordinaryGraph(t)
	cache.Report(r.c, derivedReport(r.s.Dir, gitproc.PartCommitGraph, time.Now()))
	r.quiet(t)
	if cache.GraphFailed(r.c, r.repo) {
		t.Error("damage reported a RebuildQuiet after the last drop for damage was left to the ordinary rules")
	}
	if got := reports(r.w, "commit-graph", "again"); got != 2 {
		t.Errorf("reports{part=commit-graph,outcome=again} = %d, want 2: a RebuildQuiet passed", got)
	}
	r.rewritten(t, "the commit-graph, at once", r.graph, nil)
	if lines := r.w.logs.lines(t, "code_cache_derived_damaged_again"); len(lines) != 1 {
		t.Errorf("code_cache_derived_damaged_again = %v, want one line however often it recurs", lines)
	}
}

// ordinaryGraph has the maintainer write the copy's index and then its graph
// by the ordinary rules — not a graph-first run — and fails t unless the graph
// is on disk and no longer left to the ordinary rules afterwards.
func (r *reportedCopy) ordinaryGraph(t *testing.T) {
	t.Helper()
	cache.WantIndex(r.c, r.repo)
	cache.MaintainerPass(r.c)
	r.quiet(t)
	if r.graph() == "" || cache.GraphFailed(r.c, r.repo) {
		t.Fatalf("the maintainer's ordinary run wrote no graph (left to the ordinary rules: %v)",
			cache.GraphFailed(r.c, r.repo))
	}
}

// AN OBSERVER NEVER WAITS FOR A ROUND. It runs on the reporting run's own
// goroutine — a fetch's, a push's, a maintenance run's — so it takes the
// entry's mu and never its write lock: a report made while a round holds the
// repository returns at once, and is repaired by the round queued behind it.
//
// Mutation: take the write lock in observe (the report waits for the held
// round, which waits for the case).
func TestAnObserverNeverWaitsForARound(t *testing.T) {
	t.Parallel()
	h := newRoundHold()
	r := newReportedCopy(t, nil, func(o *cache.Options) { cache.AfterSweep(o, h.at) })
	graph := stat(t, r.graph())
	h.armed.Store(true)
	cache.MarkWhole(r.c, r.repo, cache.RebuildShape, 0)
	synced := make(chan error, 1)
	go func() {
		s, err := r.c.Sync(context.Background(), r.repo, statelog.Freshness{Level: statelog.ReadLinearizable})
		if err == nil {
			s.Release()
		}
		synced <- err
	}()
	h.wait(t)

	done := make(chan struct{})
	go func() {
		cache.Report(r.c, derivedReport(r.s.Dir, gitproc.PartCommitGraph, time.Now()))
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(hangDetector):
		t.Error("the observer waited for the round holding the repository")
	}
	if !cache.WriteLockHeld(r.c, r.repo) {
		t.Error("the round was not holding the repository: the case held nothing")
	}
	close(h.release)
	if err := <-synced; err != nil {
		t.Fatalf("the held round: %v", err)
	}
	<-done
	r.rewritten(t, "the commit-graph", r.graph, graph)
}

// A MAINTAINER'S FAILED WRITE DROPS THE INDEX IT COULD NOT REPLACE. Writing a
// multi-pack-index over one whose offsets are damaged (MO) is no repair:
// git says it is ignoring the existing index and then fails reading objects
// through it, every time. The maintainer's own run is observed like any
// other, so its `ignoring existing multi-pack-index; checksum mismatch`
// reaches the cache: the index is dropped and written again at once, sound.
//
// Mutation: drop that row from gitproc's table (the run names nothing, and
// every later write fails the same way over the damaged index).
func TestAMaintainersFailedWriteDropsTheIndexItCouldNotReplace(t *testing.T) {
	t.Parallel()
	r := newReportedCopy(t, nil)
	tip := r.w.repos[r.repo].head
	tree, err := runGit(t, r.w.pool, r.s.Dir, "", "rev-parse", tip+"^{tree}")
	if err != nil {
		t.Fatal(err)
	}
	gittest.Damage(t, r.s.Dir, gittest.MidxOffset, tip, trimmed(tree))
	damaged := stat(t, r.midx())

	cache.WantIndex(r.c, r.repo)
	cache.MaintainerPass(r.c)
	r.rewritten(t, "the multi-pack-index", r.midx, damaged)
	if r.bitmap() == "" {
		t.Error("the index written again has no bitmap")
	}
	if got := reports(r.w, "multi-pack-index", "queued"); got == 0 {
		t.Error("the maintainer's failed write was not reported")
	}
}

// A REPORT IS NARROWED TO WHAT ITS PROCESS LOADS. A chunk-format message
// names neither file, and so reads as both; a process that loads only the
// multi-pack-index (the cache's wants check: cat-file asked about ids alone
// never parses a commit) reports the index, and the commit-graph beside it
// — whose loss puts every walk back on commit dates — is kept.
//
// Mutation: read the report unnarrowed (the graph is dropped too).
func TestAReportIsNarrowedToWhatItsProcessLoads(t *testing.T) {
	t.Parallel()
	r := newReportedCopy(t, nil)
	index, graph := stat(t, r.midx()), stat(t, r.graph())
	ran := derivedReport(r.s.Dir, gitproc.PartChunk, time.Now())
	ran.Loads = gitproc.PartMultiPackIndex
	cache.Report(r.c, ran)
	r.quiet(t) // the repair round, and no maintainer pass yet
	if r.midx() != "" {
		t.Error("the repair round kept the multi-pack-index its report named")
	}
	if now := r.graph(); now == "" || !os.SameFile(stat(t, now), graph) {
		t.Errorf("a chunk report from a process loading the index alone took the graph (now %q)", now)
	}
	r.rewritten(t, "the multi-pack-index", r.midx, index)
}

// trimmed is s without its trailing newline.
func trimmed(s string) string {
	for len(s) > 0 && (s[len(s)-1] == '\n' || s[len(s)-1] == '\r') {
		s = s[:len(s)-1]
	}
	return s
}
