package cache_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/code"
	"github.com/crewlet/crewlet/internal/code/cache"
	"github.com/crewlet/crewlet/internal/code/codefault"
	"github.com/crewlet/crewlet/internal/code/gitproc"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/statelog/metrics"
)

// verify is the whole of a check, as the host runs one: admitted, run and let
// go.
func verify(ctx context.Context, c *cache.Cache, repo string, repair bool) (cache.VerifyReport, error) {
	v, err := c.AdmitVerify(repo)
	if err != nil {
		return cache.VerifyReport{}, err
	}
	defer v.Release()
	return v.Run(ctx, cache.VerifyRun{Repair: repair})
}

// problemsOf fails t unless problems holds, for each of want, a problem
// containing it.
func problemsOf(t *testing.T, what string, problems []string, want ...string) {
	t.Helper()
	for _, w := range want {
		if !slices.ContainsFunc(problems, func(p string) bool { return strings.Contains(p, w) }) {
			t.Errorf("%s %q: none names %q", what, problems, w)
		}
	}
}

// damage overwrites n bytes of the file at path, at offset from its end,
// with b — a disk that went bad under an installed file.
func damage(t *testing.T, path string, fromEnd int64, b []byte) {
	t.Helper()
	if err := os.Chmod(path, 0o600); err != nil {
		t.Fatal(err)
	}
	f, err := os.OpenFile(path, os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	info, err := f.Stat()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteAt(b, info.Size()-fromEnd); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
}

// VERIFY REPAIRS A CORRUPT CACHE: it names each thing a cache holds that its
// rows do not say — bytes that hash to something else, a pack no row names,
// a loose ref — and, asked to repair, rebuilds the repository and checks the
// rebuilt cache the same way, reporting what it found before beside what is
// left after, which is nothing: a rebuild is the rows and the object store
// made files again.
func TestVerifyRepairsACorruptCache(t *testing.T) {
	t.Parallel()
	w := newSyncWorld(t, onMemory)
	repo, stranger := w.repo(t, 1), w.repo(t, 2)
	_, first := w.push(t, repo, "refs/heads/main", nil)
	tip, _ := w.push(t, repo, "refs/heads/main", nil)
	c := w.open(t)
	s := w.sync(t, c, repo)

	clean, err := verify(t.Context(), c, repo, false)
	if err != nil {
		t.Fatalf("Verify of a clean cache: %v", err)
	}
	if len(clean.Problems) != 0 || clean.Rebuilt || clean.Repo != repo || clean.Name != "repo-1" ||
		clean.Packs != 2 || clean.Refs != 1 || clean.Position != s.Position {
		t.Fatalf("Verify of a clean cache = %+v; want no problems over its 2 packs and 1 ref at %s", clean, s.Position)
	}

	// The bytes just before the first pack's trailer belong to its last
	// object, a blob, whose content no connectivity check reads: only the
	// hash can find them.
	damage(t, filepath.Join(s.Dir, "objects", "pack", "pack-"+first.GitChecksum+".pack"), 28,
		[]byte{0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff})
	_, stray := w.packInto(t, stranger, s.Dir, nil)
	write(t, filepath.Join(s.Dir, "refs", "heads", "loose"), tip+"\n")
	found := []string{
		"pack " + first.PackID + " (pack-" + first.GitChecksum + ") hashes to",
		"stray pack pack-" + stray + " (no row names it)",
		"unexpected refs/heads/loose",
	}

	report, err := verify(t.Context(), c, repo, false)
	if err != nil {
		t.Fatalf("Verify of a damaged cache: %v", err)
	}
	problemsOf(t, "Verify found", report.Problems, found...)
	if report.Rebuilt || len(report.Found) != 0 {
		t.Errorf("Verify without repair rebuilt: %+v", report)
	}

	repaired, err := verify(t.Context(), c, repo, true)
	if err != nil {
		t.Fatalf("Verify with repair: %v", err)
	}
	if !repaired.Rebuilt || len(repaired.Problems) != 0 {
		t.Errorf("Verify with repair = %+v; want a rebuild that left no problem", repaired)
	}
	problemsOf(t, "Verify's repair found first", repaired.Found, found...)
	if got, want := w.image(t, c, repo), w.rebuilt(t, repo); got != want {
		t.Errorf("the repaired image:\n%s\nwant a fresh cache's:\n%s", got, want)
	}
	if got := counter(w.rec, metrics.CodeCacheRebuilds, "reason", "verify"); got != 1 {
		t.Errorf("%s{reason=verify} = %d, want 1", metrics.CodeCacheRebuilds, got)
	}
	if lines := w.logs.lines(t, "code_cache_rebuilt"); len(lines) != 1 || lines[0]["reason"] != "verify" {
		t.Errorf("code_cache_rebuilt = %v, want one line, reason verify", lines)
	}
}

// VERIFY'S REPAIR DOWNLOADS ONLY WHAT IT FOUND DAMAGED: it has just held every
// pack to its row, so the rebuild links in each pack that hashed to its row —
// read-only files, named by their rows — and asks the object store only for
// the rest; a stray goes with the rebuild's own sweep, for nothing. One
// damaged pack among many costs one GET, not one a pack the repository holds.
func TestVerifysRepairDownloadsOnlyWhatItFoundDamaged(t *testing.T) {
	t.Parallel()
	w := newSyncWorld(t, onMemory)
	repo, stranger := w.repo(t, 1), w.repo(t, 2)
	_, first := w.push(t, repo, "refs/heads/main", nil)
	_, second := w.push(t, repo, "refs/heads/main", nil)
	_, third := w.push(t, repo, "refs/heads/dev", nil)
	c := w.open(t)
	s := w.sync(t, c, repo)
	damage(t, filepath.Join(s.Dir, "objects", "pack", "pack-"+second.GitChecksum+".pack"), 28,
		[]byte{0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff})
	_, stray := w.packInto(t, stranger, s.Dir, nil)
	before, _, _ := w.objects.counts()

	repaired, err := verify(t.Context(), c, repo, true)
	if err != nil {
		t.Fatalf("Verify with repair: %v", err)
	}
	if !repaired.Rebuilt || len(repaired.Problems) != 0 {
		t.Fatalf("Verify with repair = %+v; want a rebuild that left no problem", repaired)
	}
	problemsOf(t, "Verify's repair found first", repaired.Found,
		"pack "+second.PackID+" (pack-"+second.GitChecksum+") hashes to", "stray pack pack-"+stray)
	after, _, _ := w.objects.counts()
	for _, row := range []code.PackRow{first, second, third} {
		want := 0
		if row.PackID == second.PackID {
			want = 1
		}
		if got := after[row.Object] - before[row.Object]; got != want {
			t.Errorf("the repair downloaded pack %s %d times, want %d", row.PackID, got, want)
		}
	}
	if got, want := w.image(t, c, repo), w.rebuilt(t, repo); got != want {
		t.Errorf("the repaired image:\n%s\nwant a fresh cache's:\n%s", got, want)
	}
}

// A REBUILD VERIFY HAS SHOWN WHOLE ENDS ITS LINE: the cache cannot see a
// request succeed, so a rebuild it made at a git process's word holds the
// repository to a window before the next — which an operator's repair is not
// held to, since the damage is named — and a repair Verify then checked
// against every row is known to have ended what it was made for, so damage
// found on it later is new damage, rebuilt at once.
func TestARebuildVerifyHasShownWholeEndsItsLine(t *testing.T) {
	t.Parallel()
	w := newSyncWorld(t, onMemory)
	repo := w.repo(t, 1)
	w.push(t, repo, "refs/heads/main", nil)
	c := w.open(t)
	s := w.sync(t, c, repo)
	first, err := c.Rebuild(t.Context(), s, cache.RebuildCorrupt)
	if err != nil {
		t.Fatalf("Rebuild: %v", err)
	}
	defer first.Release()
	write(t, filepath.Join(first.Dir, "refs", "heads", "loose"), "x\n")
	if _, err := c.Rebuild(t.Context(), first, cache.RebuildCorrupt); !errors.Is(err, cache.ErrRebuildRefused) {
		t.Fatalf("a second rebuild of a rebuild's product = %v, want ErrRebuildRefused", err)
	}
	repaired, err := verify(t.Context(), c, repo, true)
	if err != nil || !repaired.Rebuilt || len(repaired.Problems) != 0 {
		t.Fatalf("Verify with repair of a repository a request may not rebuild = %+v, %v; want a clean rebuild",
			repaired, err)
	}
	rebuilt := w.sync(t, c, repo)
	again, err := c.Rebuild(t.Context(), rebuilt, cache.RebuildCorrupt)
	if err != nil {
		t.Fatalf("a rebuild of the incarnation a verified repair made: %v", err)
	}
	defer again.Release()
	if cache.Incarnation(again) != cache.Incarnation(rebuilt)+1 {
		t.Errorf("the rebuild answered incarnation %d, want %d", cache.Incarnation(again), cache.Incarnation(rebuilt)+1)
	}
}

// VERIFY NAMES WHAT A REBUILD CANNOT FIX: a pack gone from the object store
// as well as from the cache is damage no rebuild repairs — the rebuild reads
// the same rows and finds the same object missing — so Verify answers the
// problems it found with the rebuild's failure, which names the object.
func TestVerifyNamesWhatARebuildCannotFix(t *testing.T) {
	t.Parallel()
	w := newSyncWorld(t, onMemory)
	repo := w.repo(t, 1)
	w.push(t, repo, "refs/heads/main", nil)
	tip, row := w.push(t, repo, "refs/heads/main", nil)
	c := w.open(t)
	s := w.sync(t, c, repo)
	if err := w.store.Delete(t.Context(), row.Object); err != nil {
		t.Fatal(err)
	}
	for _, ext := range []string{".pack", ".idx", ".rev"} {
		if err := os.Remove(filepath.Join(s.Dir, "objects", "pack", "pack-"+row.GitChecksum+ext)); err != nil {
			t.Fatal(err)
		}
	}

	report, err := verify(t.Context(), c, repo, true)
	var missing *cache.ErrPackMissing
	if !errors.As(err, &missing) || missing.Pack.PackID != row.PackID {
		t.Fatalf("Verify with repair of a pack the store lost = %v, want an ErrPackMissing naming %s", err, row.PackID)
	}
	if report.Rebuilt || report.Repo != repo {
		t.Errorf("Verify answered %+v, want the first pass's report", report)
	}
	// fsck's first finding — the branch whose tip is gone — and never the
	// notice it ends that report with, which names nothing.
	problemsOf(t, "Verify found", report.Problems, "pack "+row.PackID+" (pack-"+row.GitChecksum+") is missing")
	if !slices.ContainsFunc(report.Problems, func(p string) bool {
		return strings.HasPrefix(p, "fsck: ") && strings.Contains(p, tip) && !strings.Contains(p, "notice:")
	}) {
		t.Errorf("Verify found %q: none is fsck's finding about the lost tip %s", report.Problems, tip)
	}
}

// A CHECK IS ADMITTED BEFORE IT READS ANYTHING: it takes its maintenance slot
// — a free one or none, the lane never waits — before its sync, its listing
// or a byte of a pack, so with the lane held it is refused busy at once,
// saying to run it again, having read nothing; and it holds that slot from
// its first read to its last, so on a one-slot lane two checks at once read
// one repository, never two, and the second is refused before it begins.
//
// Mutation: take the slot at the fsck rather than at the admission, and a
// check is admitted with the lane held, and a second beside the first.
func TestACheckIsAdmittedBeforeItReadsAnything(t *testing.T) {
	t.Parallel()
	w := newSyncWorld(t, onMemory)
	first, second := w.repo(t, 1), w.repo(t, 2)
	w.push(t, first, "refs/heads/main", nil)
	w.push(t, second, "refs/heads/main", nil)
	var mu sync.Mutex
	listed := map[string]int{}
	c := w.open(t, func(o *cache.Options) {
		cache.BeforeVerifyWaits(o, func(dir string) {
			mu.Lock()
			defer mu.Unlock()
			listed[dir]++
		})
	})
	if got := w.pool.Slots(gitproc.ClassMaintenance); got != 1 {
		t.Fatalf("the pool has %d maintenance slots, want 1", got)
	}
	firstDir, secondDir := w.sync(t, c, first).Dir, w.sync(t, c, second).Dir
	reads := func() int {
		opens, _, _ := w.objects.counts()
		n := 0
		for _, k := range opens {
			n += k
		}
		return n
	}
	before := reads()

	release := holdSlotUntil(t, w.pool, gitproc.ClassMaintenance)
	if _, err := c.AdmitVerify(first); !errors.Is(err, gitproc.ErrBusy) || !strings.Contains(err.Error(), "run it again") {
		t.Errorf("a check with the maintenance lane held = %v, want ErrBusy saying to run it again", err)
	}
	release()

	v, err := c.AdmitVerify(first)
	if err != nil {
		t.Fatalf("a check with the lane free: %v", err)
	}
	if _, err := c.AdmitVerify(second); !errors.Is(err, gitproc.ErrBusy) {
		t.Errorf("a second check beside the first on a one-slot lane = %v, want ErrBusy", err)
	}
	if report, err := v.Run(t.Context(), cache.VerifyRun{}); err != nil || len(report.Problems) != 0 {
		t.Errorf("the admitted check = %+v, %v", report, err)
	}
	if _, err := v.Run(t.Context(), cache.VerifyRun{}); !errors.Is(err, cache.ErrInvalid) {
		t.Errorf("a second Run of one check = %v, want ErrInvalid", err)
	}
	v.Release()
	v.Release()
	mu.Lock()
	if listed[firstDir] != 1 || listed[secondDir] != 0 {
		t.Errorf("the checks listed %v, want %s once and %s never", listed, firstDir, secondDir)
	}
	mu.Unlock()
	if got := reads(); got != before {
		t.Errorf("the checks read %d objects from the store, want none: both repositories are held", got-before)
	}
	if report, err := verify(t.Context(), c, second, false); err != nil || len(report.Problems) != 0 {
		t.Errorf("the second check once the first let go = %+v, %v", report, err)
	}
}

// A CHECK PASS ENDS AT ITS BOUND, AND SAYS SO AS IT BEGINS: every pass is told
// to its caller with the bound what the copy holds gives it (CheckBudget), a
// repair's rebuild with none — its reads of the object store are bounded each
// on its own — and a pass still running at its bound is ended there,
// ErrPastBound naming the bound, with what it found before; the copy is no
// worse for it, and a check with room checks it whole. The bound grows with
// what a copy holds: the hashing and the connectivity check over every byte.
//
// Mutation: run the pass under its caller's context alone, and the wedged
// check outlives its bound until its caller gives up.
func TestACheckPassEndsAtItsBound(t *testing.T) {
	t.Parallel()
	if small, large := cache.CheckBudget(1<<20), cache.CheckBudget(1<<30); small >= large ||
		small > time.Minute || large > time.Hour {
		t.Errorf("a check's bound is %s over a mebibyte and %s over a gibibyte; want it to grow with the "+
			"bytes, a mebibyte's in seconds and a gibibyte's under an hour", small, large)
	}
	if got, want := cache.CheckBudget(1<<30)-cache.CheckBudget(0), 1024*(cache.VerifyHashPerMiB+
		gitproc.PackWorkPerMiB); got != want {
		t.Errorf("a gibibyte adds %s to a check's bound, want %s: VerifyHashPerMiB and gitproc.PackWorkPerMiB "+
			"each mebibyte", got, want)
	}

	w := newSyncWorld(t, onMemory)
	repo := w.repo(t, 1)
	w.push(t, repo, "refs/heads/main", nil)
	var (
		mu     sync.Mutex
		bound  = time.Hour
		wedged bool
	)
	c := w.open(t, func(o *cache.Options) {
		cache.SetCheckBudget(o, func(int64) time.Duration {
			mu.Lock()
			defer mu.Unlock()
			return bound
		})
		cache.BeforeVerifyFsck(o, func(string) {
			mu.Lock()
			wait := wedged
			mu.Unlock()
			if wait {
				time.Sleep(200 * time.Millisecond)
			}
		})
	})
	w.sync(t, c, repo)

	var told []time.Duration
	checking := func(b time.Duration) { told = append(told, b) }
	mu.Lock()
	bound, wedged = 20*time.Millisecond, true
	mu.Unlock()
	v, err := c.AdmitVerify(repo)
	if err != nil {
		t.Fatal(err)
	}
	report, err := v.Run(t.Context(), cache.VerifyRun{Checking: checking})
	v.Release()
	if !errors.Is(err, cache.ErrPastBound) || !strings.Contains(err.Error(), "20ms") || report.Packs != 1 {
		t.Fatalf("a check wedged past its bound = %+v, %v; want ErrPastBound naming the bound, its packs "+
			"counted", report, err)
	}
	if len(told) != 1 || told[0] != 20*time.Millisecond {
		t.Errorf("the check told its caller %v, want its one pass's bound", told)
	}

	// THE REPAIR: a damaged copy found, the rebuild told with no bound, and
	// the rebuilt copy's pass told its own.
	mu.Lock()
	bound, wedged = time.Hour, false
	mu.Unlock()
	packs, err := filepath.Glob(filepath.Join(w.dir(t, c, repo), "objects", "pack", "pack-*.pack"))
	if err != nil || len(packs) != 1 {
		t.Fatalf("the copy's packs are %v (%v)", packs, err)
	}
	damage(t, packs[0], 28, []byte{0xff, 0xff, 0xff, 0xff})
	told = nil
	v, err = c.AdmitVerify(repo)
	if err != nil {
		t.Fatal(err)
	}
	report, err = v.Run(t.Context(), cache.VerifyRun{Repair: true, Checking: checking})
	v.Release()
	if err != nil || !report.Rebuilt || len(report.Problems) != 0 {
		t.Fatalf("a repair with room = %+v, %v; want the copy rebuilt whole", report, err)
	}
	if !slices.Equal(told, []time.Duration{time.Hour, 0, time.Hour}) {
		t.Errorf("a repair told its caller %v, want the check's bound, none for the rebuild, the recheck's", told)
	}
}

// A CHECK OF A REPOSITORY THIS NODE HOLDS NO COPY OF IS NONE: a check builds no
// copy, since building one is a whole download of the repository into a cache
// that then keeps it, evicting warm repositories to do so — so it is refused
// at once, having downloaded nothing and left the cache as it was.
//
// Mutation: drop the held check from AdmitVerify, and the check downloads the
// repository and leaves it in the cache.
func TestACheckOfARepositoryThisNodeDoesNotHoldIsNone(t *testing.T) {
	t.Parallel()
	w := newSyncWorld(t, onMemory)
	repo := w.repo(t, 1)
	w.push(t, repo, "refs/heads/main", nil)
	c := w.open(t)
	if _, err := verify(t.Context(), c, repo, false); !errors.Is(err, cache.ErrNotHeld) {
		t.Errorf("a check of a repository the node never served = %v, want ErrNotHeld", err)
	}
	if opens, _, _ := w.objects.counts(); len(opens) != 0 {
		t.Errorf("the refused check read %v from the store, want nothing", opens)
	}
	if got := cache.Repos(c); len(got) != 0 {
		t.Errorf("the refused check left %v in the cache, want nothing", got)
	}
	if _, err := c.AdmitVerify("not-an-id"); !errors.Is(err, cache.ErrInvalid) {
		t.Errorf("a check of an id that is none = %v, want ErrInvalid", err)
	}
}

// VERIFY HASHES THE BUNDLE, NOT THE PACK: a row's hash is of the pack, its
// index and its reverse index back to back, as the push stored them — so a
// byte gone bad in an index is found by the hash, and a section cut short by
// its size, each naming the pack and the section.
func TestVerifyHashesTheBundleNotThePack(t *testing.T) {
	t.Parallel()
	w := newSyncWorld(t, onMemory)
	repo := w.repo(t, 1)
	_, first := w.push(t, repo, "refs/heads/main", nil)
	_, second := w.push(t, repo, "refs/heads/main", nil)
	c := w.open(t)
	s := w.sync(t, c, repo)
	if report, err := verify(t.Context(), c, repo, false); err != nil || len(report.Problems) != 0 {
		t.Fatalf("Verify of a clean cache = %+v, %v; want no problems", report, err)
	}

	// The .idx's own trailing checksum: no lookup reads it, so the hash is
	// the only thing that can see it changed.
	idx := filepath.Join(s.Dir, "objects", "pack", "pack-"+second.GitChecksum+".idx")
	raw, err := os.ReadFile(idx)
	if err != nil {
		t.Fatal(err)
	}
	damage(t, idx, 1, []byte{raw[len(raw)-1] ^ 0xff})
	rev := filepath.Join(s.Dir, "objects", "pack", "pack-"+first.GitChecksum+".rev")
	if err := os.Truncate(rev, first.ReverseBytes-4); err != nil {
		t.Fatal(err)
	}
	report, err := verify(t.Context(), c, repo, false)
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	problemsOf(t, "Verify found", report.Problems,
		"pack "+second.PackID+" (pack-"+second.GitChecksum+") hashes to",
		"pack "+first.PackID+" (pack-"+first.GitChecksum+"): its .rev is")
	for _, p := range report.Problems {
		if strings.Contains(p, "hashes to") && !strings.Contains(p, second.PackID) {
			t.Errorf("a pack whose bytes are its row's was reported: %q", p)
		}
	}
}

// VERIFY LEAVES A MAINTENANCE RUN TO FINISH: a run in flight on the
// repository is never stopped for a check — its work thrown away would be
// done again at once, and on a lane of one slot the run done again took the
// slot back while the check hashed, so the check's fsck was refused on every
// retry — and the files it holds only while it runs (the index's and the
// graph's locks, a bitmap's temporary file) are no damage. The same files with
// no run in flight are, and each is named.
func TestVerifyLeavesAMaintenanceRunToFinish(t *testing.T) {
	t.Parallel()
	w := newSyncWorld(t, onMemory)
	clk := newClock()
	repo := w.repo(t, 1)
	w.push(t, repo, "refs/heads/main", nil)
	w.push(t, repo, "refs/heads/main", nil)
	holding, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	c := w.open(t, clk.on, func(o *cache.Options) {
		cache.BeforeMaintaining(o, func(ctx context.Context, _ string) {
			once.Do(func() {
				close(holding)
				select {
				case <-release:
				case <-ctx.Done():
				}
			})
		})
	})
	s := w.sync(t, c, repo)
	clk.Add(cache.MidxDebounce)

	started, wait := cache.StartMaintainerPass(c)
	if !slices.Equal(started, []string{repo}) {
		t.Fatalf("the maintainer started %v, want %s", started, repo)
	}
	<-holding
	// What git's index, bitmap and graph writes hold while they run.
	running := []string{
		filepath.Join("objects", "pack", "multi-pack-index.lock"),
		filepath.Join("objects", "pack", "tmp_bitmap_Ab3xYz"),
		filepath.Join("objects", "info", "commit-graph.lock"),
	}
	for _, rel := range running {
		write(t, filepath.Join(s.Dir, rel), "")
	}
	report, err := verify(t.Context(), c, repo, false)
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if len(report.Problems) != 0 {
		t.Errorf("Verify beside a maintenance run found %q", report.Problems)
	}
	if !cache.Maintaining(c, repo) {
		t.Fatal("Verify stopped the maintenance run on the repository")
	}
	// The run's own files were the case's stand-ins: gone before it writes
	// its own.
	for _, rel := range running {
		if err := os.Remove(filepath.Join(s.Dir, rel)); err != nil {
			t.Fatal(err)
		}
	}
	close(release)
	ended := make(chan []string, 1)
	go func() { ended <- wait() }()
	select {
	case ran := <-ended:
		if !slices.Equal(ran, []string{repo}) {
			t.Errorf("the maintenance run Verify ran beside ran every step on %v, want %s", ran, repo)
		}
	case <-time.After(hangDetector):
		t.Fatal("the maintenance run Verify ran beside never ended")
	}

	for _, rel := range running {
		write(t, filepath.Join(s.Dir, rel), "")
	}
	report, err = verify(t.Context(), c, repo, false)
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	for _, rel := range running {
		problemsOf(t, "Verify with no run in flight found", report.Problems, "unexpected "+filepath.ToSlash(rel))
	}
}

// NO MAINTENANCE RUN STARTS UNDER A VERIFY: its fsck asks for a maintenance
// slot and is refused rather than queued, so a run started while it checks
// would take the lane's last free slot — a one-slot lane's only one — and
// answer the person who asked "run it again", into the same race. So the
// repository is held off the maintainer from the listing to the fsck, and the
// maintainer kicked as Verify lets it go: a repository due all along is taken
// up at once rather than at the next tick.
func TestNoMaintenanceRunStartsUnderAVerify(t *testing.T) {
	t.Parallel()
	w := newSyncWorld(t, onMemory)
	clk := newClock()
	repo := w.repo(t, 1)
	w.push(t, repo, "refs/heads/main", nil)
	w.push(t, repo, "refs/heads/main", nil)
	var c *cache.Cache
	var mu sync.Mutex
	var under []string
	// check runs the maintainer's own pass, as a kick would have it run, and
	// notes a run it started or found in flight.
	check := func(stage string) {
		started, _ := cache.StartMaintainerPass(c)
		kicked := cache.MaintainerKicked(c)
		mu.Lock()
		defer mu.Unlock()
		if len(started) != 0 || cache.Maintaining(c, repo) {
			under = append(under, "a run at "+stage)
		}
		if kicked {
			under = append(under, "a kick at "+stage)
		}
	}
	c = w.open(t, clk.on, func(o *cache.Options) {
		cache.WhileVerifyLists(o, func(string) { check("the listing") })
		cache.BeforeVerifyFsck(o, func(string) { check("the fsck") })
	})
	if got := w.pool.Slots(gitproc.ClassMaintenance); got != 1 {
		t.Fatalf("the pool has %d maintenance slots, want 1", got)
	}
	w.sync(t, c, repo)
	clk.Add(cache.MidxDebounce)
	cache.MaintainerKicked(c) // the sync's own, answered

	report, err := verify(t.Context(), c, repo, false)
	if err != nil {
		t.Fatalf("Verify of a repository due for maintenance on a one-slot lane: %v", err)
	}
	if len(report.Problems) != 0 {
		t.Errorf("Verify found %q", report.Problems)
	}
	mu.Lock()
	if len(under) != 0 {
		t.Errorf("between Verify's listing and its fsck there was %q", under)
	}
	mu.Unlock()
	if !cache.MaintainerKicked(c) {
		t.Error("Verify let the repository go without kicking the maintainer")
	}
	if ran := cache.MaintainerPass(c); !slices.Equal(ran, []string{repo}) {
		t.Errorf("after Verify the maintainer ran %v, want %s, due all along", ran, repo)
	}
}

// A round that began after Verify's own sync and is still writing when
// Verify comes to list is waited for: the listing reads the directory the
// round left, never one half written.
func TestVerifyWaitsForARoundStillWriting(t *testing.T) {
	t.Parallel()
	w := newSyncWorld(t, onMemory)
	repo := w.repo(t, 1)
	w.push(t, repo, "refs/heads/main", nil)
	script := &codefault.Script{}
	reached, release := make(chan struct{}), make(chan struct{})
	type synced struct {
		at  statelog.Position
		err error
	}
	done := make(chan synced, 1)
	var once sync.Once
	var c *cache.Cache
	c = w.open(t, func(o *cache.Options) {
		o.Faults = script
		cache.BeforeVerifyWaits(o, func(string) {
			once.Do(func() {
				w.push(t, repo, "refs/heads/dev", nil)
				script.Arm(codefault.SyncRefs, codefault.Action{Block: release, Reached: reached})
				go func() {
					held, err := c.Sync(context.Background(), repo, linearizable)
					if err == nil {
						held.Release()
					}
					done <- synced{held.Position, err}
				}()
				<-reached
				// Long enough for a listing that did not wait to have begun.
				time.AfterFunc(200*time.Millisecond, func() { close(release) })
			})
		})
		cache.WhileVerifyLists(o, func(string) {
			select {
			case <-release:
			default:
				t.Error("Verify listed the repository while a round was still writing it")
			}
		})
	})
	w.sync(t, c, repo)

	report, err := verify(t.Context(), c, repo, false)
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if len(report.Problems) != 0 {
		t.Errorf("Verify after the round it waited for found %q", report.Problems)
	}
	round := <-done
	if round.err != nil {
		t.Fatal(round.err)
	}
	if report.Position != round.at {
		t.Errorf("Verify reported position %v, want the round's %v", report.Position, round.at)
	}
}

// A round waiting for a listing to end is a round of the cache's lifetime:
// Stop ends its wait, and it answers ErrStopped rather than run once the
// listing lets it.
func TestStopEndsARoundWaitingForAListing(t *testing.T) {
	t.Parallel()
	w := newSyncWorld(t, onMemory)
	repo := w.repo(t, 1)
	w.push(t, repo, "refs/heads/main", nil)
	var once sync.Once
	var c *cache.Cache
	c = w.open(t, func(o *cache.Options) {
		cache.WhileVerifyLists(o, func(string) {
			once.Do(func() {
				w.push(t, repo, "refs/heads/dev", nil)
				done := make(chan error, 1)
				go func() {
					held, err := c.Sync(context.Background(), repo, linearizable)
					if err == nil {
						held.Release()
					}
					done <- err
				}()
				// The round waits for this listing from the moment the
				// sync is its waiter.
				deadline := time.Now().Add(hangDetector)
				for cache.Waiting(c, repo) == 0 && time.Now().Before(deadline) {
					time.Sleep(time.Millisecond)
				}
				stopped, cancel := context.WithTimeout(context.Background(), hangDetector)
				defer cancel()
				c.Stop(stopped)
				select {
				case err := <-done:
					if !errors.Is(err, cache.ErrStopped) {
						t.Errorf("a round Stop ended while it waited for a listing = %v, want ErrStopped", err)
					}
				case <-time.After(hangDetector):
					t.Error("Stop left a round waiting for a listing")
				}
			})
		})
	})
	w.sync(t, c, repo)
	if _, err := verify(t.Context(), c, repo, false); err != nil {
		t.Fatalf("Verify: %v", err)
	}
}

// A CHECK NAMES THE DAMAGE ITS OWN SYNC MEETS, AND REPAIRS ONLY WHEN ASKED.
// The sync that brings the copy to the check's floor is a delta that peels a
// tag set since; when that peel meets the copy damaged — its shape refused,
// an object gone from its packs — the check is handed the copy as it stands
// and names the damage first among its problems. Repaired under it, a plain
// check downloaded the repository (or put the directory right in place) and
// reported the repaired copy clean, the damage it was asked to name hidden.
// Asked to repair, it rebuilds under reason verify, the sync's damage among
// what it found.
//
// Mutation: answer a checking caller of a damaged round as every other
// (damagedRound), and the plain check names nothing and the copy is repaired.
func TestACheckNamesTheDamageItsSyncMeets(t *testing.T) {
	t.Parallel()
	for name, d := range map[string]struct {
		reason cache.RebuildReason
		spoil  func(t *testing.T, dir string)
	}{
		"its shape": {cache.RebuildShape, func(t *testing.T, dir string) {
			write(t, filepath.Join(dir, "config"), gitproc.RepoConfig+"[core]\n\tsharedRepository = group\n")
		}},
		"an object gone from its packs": {cache.RebuildMissing, func(t *testing.T, dir string) {
			for _, f := range packFiles(t, dir) {
				if err := os.Remove(filepath.Join(dir, "objects", "pack", f)); err != nil {
					t.Fatal(err)
				}
			}
		}},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			w := newSyncWorld(t, onMemory)
			repo := w.repo(t, 1)
			w.push(t, repo, "refs/heads/main", nil)
			c := w.open(t)
			s := w.sync(t, c, repo)
			dir := s.Dir
			s.Release()
			d.spoil(t, dir)
			w.tag(t, repo, "v1", true)
			before := w.objects.total()

			report, err := verify(t.Context(), c, repo, false)
			if err != nil {
				t.Fatalf("Verify: %v", err)
			}
			met := "the sync before the check met the copy damaged (" + string(d.reason) + ")"
			if report.Rebuilt || len(report.Problems) == 0 || !strings.HasPrefix(report.Problems[0], met) {
				t.Errorf("a plain check's report = rebuilt %v, problems %q; want the sync's damage first, nothing "+
					"rebuilt", report.Rebuilt, report.Problems)
			}
			if got := cache.Repos(c)[repo]; got != 1 {
				t.Errorf("a plain check left incarnation %d, want the damaged 1 untouched", got)
			}
			// The tag's own pack, installed by the delta before its peel met
			// the damage, and nothing else.
			if got := w.objects.total() - before; got != 1 {
				t.Errorf("a plain check asked the object store for %d objects, want the tag's one pack", got)
			}
			for _, reason := range cache.RebuildReasons {
				if got := counter(w.rec, metrics.CodeCacheRebuilds, "reason", string(reason)); got != 0 {
					t.Errorf("%s{reason=%s} = %d under a plain check, want none", metrics.CodeCacheRebuilds, reason, got)
				}
			}

			report, err = verify(t.Context(), c, repo, true)
			if err != nil {
				t.Fatalf("Verify(repair): %v", err)
			}
			if !report.Rebuilt || len(report.Problems) != 0 || len(report.Found) == 0 ||
				!strings.HasPrefix(report.Found[0], met) {
				t.Errorf("a repairing check's report = rebuilt %v, found %q, problems %q; want the sync's damage "+
					"found and nothing left", report.Rebuilt, report.Found, report.Problems)
			}
			if got := counter(w.rec, metrics.CodeCacheRebuilds, "reason", "verify"); got != 1 {
				t.Errorf("%s{reason=verify} = %d, want the repair's one", metrics.CodeCacheRebuilds, got)
			}
		})
	}
}
