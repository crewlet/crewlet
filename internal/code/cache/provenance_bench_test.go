package cache_test

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"runtime"
	"slices"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"golang.org/x/sync/errgroup"

	"github.com/crewlet/crewlet/internal/code"
	"github.com/crewlet/crewlet/internal/code/cache"
	"github.com/crewlet/crewlet/internal/code/codetest"
	"github.com/crewlet/crewlet/internal/code/gitproc/gittest"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/statelog"
)

// BenchmarkProvenance is what the index of which push introduced each commit
// costs (provenance.go, backfill.go), each arm the figure a decision of
// constants.go or a statement of docs/concepts/scaling.md rests on:
//
//   - op=fold/bucket=B is a delta round's fold of its pushes into the index —
//     the run it builds, writes, syncs and commits — against the round it is
//     part of: a warm repository of 100 packs, each iteration one push of
//     medianPush bytes, synced as a delta, its pack downloaded from a store
//     in the process (memobj: the round's own side alone) or from the S3
//     fake at providerFirstByte (fake: the floor of what a bucket answers
//     in). fold-ns/op is the fold, ns/op the whole round, fold-% the share;
//     merges/push is the backfill's merges the folds leave due, taken after
//     each round untimed, and index-syncs/push the syncs folds and merges
//     make, counted from both (a fold's two, a merge's four).
//   - op=lookup/runs=R is a commit's lookup in an index of R runs holding
//     four million commits between them, the commit absent — every run
//     binary-searched, a lookup's worst case — in ns a lookup (ns/lookup),
//     against which MaxProvenanceRuns is chosen; bytes/commit is what runs
//     of pushes of three commits each hold for each commit introduced.
//   - op=backfill/pushes=N is a cold git node's backfill of a history of N
//     pushes through a solo node — the data node's own read of its push
//     rows, a page at a time, answered in process where the engine's router
//     adds its hop — paced as production paces it (BackfillIdle, one git
//     node to one data node): the pages, the pushes and the bytes of each
//     answer, the data node's read and the whole step a page in ms, and the
//     seconds the history took (s/op). 300 000 pushes run only under
//     gittest.LargeEnv.
//   - op=merge/refs=R is a merge of two runs of mergePushes pushes each, in
//     memory, every push moving the one branch (one) or a ref of its own
//     (each) — a repository whose seats push a branch apiece — in ns a push
//     and bytes allocated a ref (B/ref): what the merge holds of the refs,
//     which streamed a key at a time does not grow with them.
func BenchmarkProvenance(b *testing.B) {
	quiet(b)
	for _, bucket := range []string{"memobj", "fake"} {
		b.Run("op=fold/bucket="+bucket, func(b *testing.B) { benchFold(b, bucket) })
	}
	for _, runs := range []int{1, 8, 32, 64} {
		b.Run(fmt.Sprintf("op=lookup/runs=%d", runs), func(b *testing.B) { benchLookup(b, runs) })
	}
	for _, refs := range []string{"one", "each"} {
		b.Run("op=merge/refs="+refs, func(b *testing.B) { benchMerge(b, refs == "each") })
	}
	for _, pushes := range []int{30_000, 300_000} {
		b.Run(fmt.Sprintf("op=backfill/pushes=%d", pushes), func(b *testing.B) {
			if pushes > 30_000 {
				gittest.Large(b, "300 000 pushes proposed to a data node's committer, backfilled by a cold git node")
			}
			benchBackfill(b, pushes)
		})
	}
}

// BenchmarkBackfillPick is what the backfill's pick of its next step costs a
// node holding copies=N copies of repositories (schedule.go) — N at
// config.CodeMaxRepos, every repository a company may hold — each operation
// one pick and its step taken as done, the copies synthetic and holding no
// file:
//
//   - due=one: one copy with a step due among N, due again after each step,
//     as a busy repository's folds keep it — the steady state, where a pick
//     walking every copy cost them all on every push;
//   - due=all: every copy due, each picked once and its gap closed — the
//     warm-up after a restart, when every copy's first round opens a gap.
//
// ns/op is a pick. Beside the picks, run back to back, lookup-p99-ns and
// lookup-max-ns are what a request's lookup of its copy (Holds, which takes
// the lock every request's sync takes to find its copy) waited meanwhile.
func BenchmarkBackfillPick(b *testing.B) {
	quiet(b)
	for _, due := range []string{"one", "all"} {
		b.Run(fmt.Sprintf("copies=%d/due=%s", pickCopies, due), func(b *testing.B) {
			benchPick(b, due == "all")
		})
	}
}

// pickCopies is BenchmarkBackfillPick's copies: config.CodeMaxRepos.
const pickCopies = config.CodeMaxRepos

// benchPick is BenchmarkBackfillPick with every copy due where all.
func benchPick(b *testing.B, all bool) {
	w := newWorld(b)
	c := w.open(b)
	due := 1
	if all {
		due = pickCopies
	}
	ids := cache.SyntheticCopies(c, pickCopies, due)
	// The schedule's first placing of every copy, not a pick's own cost.
	if cache.PickAndSettle(c, true) == "" {
		b.Fatal("no copy was due")
	}
	stop := make(chan struct{})
	waited := make(chan []time.Duration, 1)
	go func() {
		var lookups []time.Duration
		for i := 0; ; i++ {
			select {
			case <-stop:
				waited <- lookups
				return
			default:
			}
			began := time.Now()
			c.Holds(ids[i%len(ids)])
			lookups = append(lookups, time.Since(began))
		}
	}()
	picked := 0
	for b.Loop() {
		if cache.PickAndSettle(c, !all) != "" {
			picked++
		}
	}
	close(stop)
	lookups := <-waited
	if picked != b.N {
		b.Fatalf("%d of %d picks found a copy due", picked, b.N)
	}
	slices.Sort(lookups)
	if len(lookups) > 0 {
		b.ReportMetric(float64(lookups[len(lookups)*99/100].Nanoseconds()), "lookup-p99-ns")
		b.ReportMetric(float64(lookups[len(lookups)-1].Nanoseconds()), "lookup-max-ns")
	}
}

// benchFold is op=fold over bucket.
func benchFold(b *testing.B, bucket string) {
	w := coldWorld(b, bucket, providerFirstByte)
	src := gittest.Empty(b)
	gittest.FastImport(b, w.client, src, pushStream(100, func(int) int64 { return 1 << 10 }))
	commits := commitsOf(b, w.client, src, "main")
	pushes := eachPush(commits)
	rows, _ := w.storePushes(b, src, pushes)
	repo := w.logRepo(1)
	w.writePushes(repo, pushes, rows)
	var folds, foldNs, merges atomic.Int64
	c, drop := w.coldCache(b, b.TempDir(), 0, cache.FetchConcurrency, func(o *cache.Options) {
		cache.FoldTimes(o, func(took time.Duration) {
			folds.Add(1)
			foldNs.Add(int64(took))
		})
		cache.MergesWith(o, func(int) { merges.Add(1) })
	})
	b.Cleanup(drop)
	s, err := c.Sync(b.Context(), repo, linearizable)
	if err != nil || s.Kind != cache.SyncFull {
		b.Fatalf("the warm cache's first round: a %s round, %v", s.Kind, err)
	}
	s.Release()
	tip, n := commits[len(commits)-1], 0
	folds.Store(0)
	foldNs.Store(0)
	var roundNs int64
	for b.Loop() {
		b.StopTimer()
		tip = w.nextPush(b, src, "refs/heads/main", repo, tip, n, medianPush)
		n++
		b.StartTimer()
		began := time.Now()
		s, err = c.Sync(b.Context(), repo, linearizable)
		roundNs += int64(time.Since(began))
		b.StopTimer()
		if err != nil || s.Kind != cache.SyncDelta {
			b.Fatalf("a sync after one push: a %s round, %v", s.Kind, err)
		}
		s.Release()
		// The backfill's merges the fold leaves due, as a started cache's
		// backfill takes them between pushes: what a push costs the node
		// beside its round, not timed with it.
		if _, err := cache.BackfillPass(c); err != nil {
			b.Fatal(err)
		}
		b.StartTimer()
	}
	if got := folds.Load(); got != int64(b.N) {
		b.Fatalf("%d rounds folded %d times, want once each", b.N, got)
	}
	b.ReportMetric(float64(foldNs.Load())/float64(b.N), "fold-ns/op")
	b.ReportMetric(100*float64(foldNs.Load())/float64(roundNs), "fold-%")
	// Each fold syncs its staged run and crewlet/; each merge those two and
	// the state's two: the syncs the index adds to a push, beside the
	// round's own state write.
	b.ReportMetric(float64(merges.Load())/float64(b.N), "merges/push")
	b.ReportMetric(float64(2*folds.Load()+4*merges.Load())/float64(b.N), "index-syncs/push")
}

// lookupPushes and lookupCommits are op=lookup's index: four million commit
// records, 96 MB of them — every run of it, at 64 runs as at one, past a
// server's last-level cache, so a lookup's binary searches pay the memory
// misses a large repository's index does, as an index of a few hundred
// thousand commits, whose runs a cache holds, would not. A lookup reads the
// commit records alone, so the pushes are few and each introduces
// code.MaxIntroduced.
const (
	lookupPushes  = 4_000
	lookupCommits = code.MaxIntroduced
)

// shapePushes and shapeCommits are the index bytes/commit is read off: a
// history's pushes as the large model pushes, three commits each.
const (
	shapePushes  = 20_000
	shapeCommits = 3
)

// lookupsPerOp is how many lookups one op=lookup operation runs: one lookup
// is microseconds, below what a single timed operation resolves.
const lookupsPerOp = 10_000

// benchLookup is op=lookup at runs runs.
func benchLookup(b *testing.B, runs int) {
	p, _ := cache.SyntheticIndex(b, runs, lookupPushes, lookupCommits)
	_, size := cache.SyntheticIndex(b, runs, shapePushes, shapeCommits)
	absent := make([]string, 1024)
	for i := range absent {
		absent[i] = cache.DrawnOID("absent-" + strconv.Itoa(i))
	}
	if _, ok := p.Commit(cache.DrawnOID("commit-7-1")); !ok {
		b.Fatal("the index does not hold a commit it was built with")
	}
	for b.Loop() {
		for i := range lookupsPerOp {
			if _, ok := p.Commit(absent[i%len(absent)]); ok {
				b.Fatal("the index holds a commit nobody pushed")
			}
		}
	}
	b.ReportMetric(float64(b.Elapsed().Nanoseconds())/float64(b.N*lookupsPerOp), "ns/lookup")
	b.ReportMetric(float64(size)/float64(shapePushes*shapeCommits), "bytes/commit")
}

// mergePushes is each run's pushes in op=merge: two runs of 100 000, so the
// arm that moves a ref a push merges 200 000 refs — a large repository's
// seat branches over a few weeks of churn.
const mergePushes = 100_000

// benchMerge is op=merge, every push moving a ref of its own where eachRef.
func benchMerge(b *testing.B, eachRef bool) {
	merge := cache.SyntheticMerge(b, mergePushes, eachRef)
	refs := 1
	if eachRef {
		refs = 2 * mergePushes
	}
	runtime.GC()
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	for b.Loop() {
		if err := merge(io.Discard); err != nil {
			b.Fatal(err)
		}
	}
	runtime.ReadMemStats(&after)
	b.ReportMetric(float64(b.Elapsed().Nanoseconds())/float64(b.N*2*mergePushes), "ns/push")
	b.ReportMetric(float64(after.TotalAlloc-before.TotalAlloc)/float64(b.N*refs), "B/ref")
}

// backfillDeadline is how long op=backfill waits for a history to be read
// before it fails: a hang detector, far past the large arm's time.
const backfillDeadline = 10 * time.Minute

// backfillLogBytes is op=backfill's code log ceiling: 256 MiB admits about
// 210 MB of records beside its gate reserve, the large arm's 300 000 pushes of
// about 580 bytes a record among them, where a solo node's own 64 MiB admits
// about 7 MB — some 12 000 pushes.
const backfillLogBytes = 256 << 20

// backfillWorkers is how many pushes op=backfill proposes at once, each
// worker moving a branch of its own, so group commit lands several a record
// as a busy repository's pushes land.
const backfillWorkers = 16

// benchBackfill is op=backfill over a history of pushes pushes: a solo fleet
// of two git nodes, the history proposed to the data node's committer, and
// the second git node — which never read the repository — emptied, its first
// round reading the repository whole and its history then wanted.
func benchBackfill(b *testing.B, pushes int) {
	var pagesRead, readNs, readBytes atomic.Int64
	f := codetest.SoloNode(b, codetest.SoloOptions{Shape: codetest.Shape{GitNodes: 2}, People: []string{"ceo"},
		LogBytes: backfillLogBytes,
		// What the router reads for each of the backfill's pages: its time
		// and the bytes of its answer. One row is the horizon's read, never
		// a page of history.
		WrapReads: func(next codetest.Reads) codetest.Reads {
			return func(ctx context.Context, q code.ReadQuery) (code.ReadAnswer, error) {
				began := time.Now()
				a, err := next(ctx, q)
				if err != nil || q.Kind != code.ReadIntroduced || q.Limit == 1 {
					return a, err
				}
				readNs.Add(int64(time.Since(began)))
				pagesRead.Add(1)
				if enc, merr := json.Marshal(a); merr == nil {
					readBytes.Add(int64(len(enc)))
				}
				return a, nil
			}
		}})
	f.Declare(b, code.Declared{Name: codetest.RepoName, Settings: codetest.Settings()})
	p := codetest.NewPerson(b, f, "ceo")
	work := p.Init()
	first := p.Commit(work, map[string]string{"a": "a\n"}, "a")
	second := p.Commit(work, map[string]string{"b": "b\n"}, "b")
	if res := p.Push(b.Context(), work, f.Nodes()[0].URL()+"/git/"+codetest.RepoName, "main"); res.Err != nil {
		b.Fatalf("git push: %s", res)
	}
	rows, err := f.Rows(b.Context(), codetest.RepoName, codetest.Linearizable)
	if err != nil {
		b.Fatal(err)
	}
	repo := rows.Repo.ID
	proposeHistory(b, f, repo, first, second, pushes)
	g := f.GitNode(1)
	for b.Loop() {
		b.StopTimer()
		dir := g.Node().CacheDir()
		g.Stop(b.Context())
		if rerr := os.RemoveAll(dir); rerr != nil {
			b.Fatal(rerr)
		}
		g.Restart(b)
		c := g.Node().Cache()
		s, serr := c.Sync(b.Context(), repo, codetest.Linearizable)
		if serr != nil {
			b.Fatalf("the cold node's first round: %v", serr)
		}
		s.Release()
		pagesRead.Store(0)
		readNs.Store(0)
		readBytes.Store(0)
		b.StartTimer()
		c.WantHistory(repo)
		for deadline := time.Now().Add(backfillDeadline); ; {
			if ix, ok := cache.Index(c, repo); ok && ix.Complete {
				break
			}
			if time.Now().After(deadline) {
				b.Fatalf("the backfill of %d pushes was not complete %s on", pushes, backfillDeadline)
			}
			time.Sleep(5 * time.Millisecond)
		}
	}
	n := float64(pagesRead.Load())
	if n == 0 {
		b.Fatal("the backfill read no page")
	}
	perOp := b.Elapsed().Seconds() / float64(b.N)
	b.ReportMetric(n, "pages")
	b.ReportMetric(float64(pushes)/n, "pushes/page")
	b.ReportMetric(float64(readBytes.Load())/n, "bytes/page")
	b.ReportMetric(float64(readNs.Load())/n/1e6, "read-ms/page")
	// Each step is followed by BackfillIdle times its own duration at one
	// git node to one data node: what a page took is the paced time over
	// 1 + BackfillIdle.
	b.ReportMetric(perOp/n/(1+cache.BackfillIdle)*1e3, "step-ms/page")
	b.ReportMetric(perOp, "s/op")
}

// proposeHistory proposes n pushes to repo's committer, as many at once as
// backfillWorkers, each worker moving a branch of its own between first and
// second — second a fast-forward, first back by force — and every push naming
// three commits it introduced, ids drawn as git's are: a history's push rows
// without a pack each, since every move names a commit the repository holds.
func proposeHistory(b *testing.B, f *codetest.SoloFleet, repo, first, second string, n int) {
	b.Helper()
	pusher := code.Principal{Kind: code.PrincipalOperator, OperatorID: "bench", Seat: "ceo"}
	eg, ctx := errgroup.WithContext(b.Context())
	for wk := range backfillWorkers {
		eg.Go(func() error {
			ref, at := fmt.Sprintf("refs/heads/w%02d", wk), code.ZeroOID
			for i := wk; i < n; i += backfillWorkers {
				next, forced := first, at == second
				if at == first {
					next = second
				}
				intro := make([]string, 3)
				for j := range intro {
					intro[j] = cache.DrawnOID(fmt.Sprintf("introduced-%d-%d", i, j))
				}
				res, err := f.Data().Served().Propose(ctx, code.Proposal{
					PushID: uuid.Must(uuid.NewV7()).String(), RepoID: repo, Principal: pusher, Via: code.ViaGit,
					Node: "bench", ReceivedAt: time.Now().UTC(), Introduced: intro, IntroducedCount: len(intro),
					Commands: []code.Command{{Ref: ref, Old: at, New: next, Forced: forced}},
				})
				switch {
				case err != nil:
					return fmt.Errorf("push %d: %w", i, err)
				case res.Outcome != statelog.OutcomeApplied || len(res.Answer.Commands) != 1 ||
					res.Answer.Commands[0].Result != code.ResultOK:
					return fmt.Errorf("push %d was answered %s: %+v", i, res.Outcome, res.Answer.Commands)
				}
				at = next
			}
			return nil
		})
	}
	if err := eg.Wait(); err != nil {
		b.Fatalf("proposing the history: %v", err)
	}
}
