package cache_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/crewlet/crewlet/internal/code"
	"github.com/crewlet/crewlet/internal/code/cache"
	"github.com/crewlet/crewlet/internal/code/gitproc/gittest"
	"github.com/crewlet/crewlet/internal/queue/topics"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/statelog/metrics"
)

var (
	linearizable = statelog.Freshness{Level: statelog.ReadLinearizable}
	stale        = statelog.Freshness{Level: statelog.ReadStale}
)

// A CACHE IS EXACT FROM ANY STARTING POINT: a crash, a full disk, an
// operator's rm or a hand edit leaves a directory the next process's FIRST
// round reads the repository's whole rows for and converges — so each row
// below prepares a directory, restarts, and holds one linearizable sync to
// the image a cache built from nothing at the same position has. Every row's
// first call is a full read (Since zero): the incarnation's first round in a
// process is.
func TestSyncConvergesFromAnyStartingPoint(t *testing.T) {
	t.Parallel()
	for _, row := range []struct {
		name string
		// prepare damages a warm cache of repo, whose packs are rows; it
		// answers how many downloads the converging round may make (-1:
		// any).
		prepare func(t *testing.T, w *syncWorld, c *cache.Cache, repo string, rows []code.PackRow) int
	}{
		{"no incarnation", func(t *testing.T, w *syncWorld, c *cache.Cache, repo string, _ []code.PackRow) int {
			if err := os.RemoveAll(w.dir(t, c, repo)); err != nil {
				t.Fatal(err)
			}
			return -1
		}},
		{"a state three pushes behind", func(t *testing.T, w *syncWorld, _ *cache.Cache, repo string, _ []code.PackRow) int {
			for range 3 {
				w.push(t, repo, "refs/heads/main", nil)
			}
			return 3
		}},
		{"a state from another stream", func(t *testing.T, w *syncWorld, _ *cache.Cache, _ string, _ []code.PackRow) int {
			w.log.recreate(w.log.state())
			return 0
		}},
		{"a stray pack", func(t *testing.T, w *syncWorld, c *cache.Cache, repo string, _ []code.PackRow) int {
			stray := w.client.Init(t)
			tip := w.client.Commit(t, stray, map[string]string{"x": "stray\n"}, "stray")
			gittest.Pack(t, w.client, w.dir(t, c, repo), stray, tip)
			return 0
		}},
		{"a loose object", func(t *testing.T, w *syncWorld, c *cache.Cache, repo string, _ []code.PackRow) int {
			write(t, filepath.Join(w.dir(t, c, repo), "objects", "ab", strings.Repeat("c", 38)), "loose")
			return 0
		}},
		{"a loose ref", func(t *testing.T, w *syncWorld, c *cache.Cache, repo string, _ []code.PackRow) int {
			write(t, filepath.Join(w.dir(t, c, repo), "refs", "heads", "x"), w.repos[repo].head+"\n")
			return 0
		}},
		{"a pack whose .idx is missing", func(t *testing.T, w *syncWorld, c *cache.Cache, repo string, rows []code.PackRow) int {
			removePack(t, w.dir(t, c, repo), rows[0].GitChecksum, ".idx")
			return 1
		}},
		{"refs ahead of packs", func(t *testing.T, w *syncWorld, c *cache.Cache, repo string, rows []code.PackRow) int {
			removePack(t, w.dir(t, c, repo), rows[len(rows)-1].GitChecksum, ".idx", ".pack", ".rev")
			return 1
		}},
		{"a pack truncated to half its size", func(t *testing.T, w *syncWorld, c *cache.Cache, repo string, rows []code.PackRow) int {
			path := filepath.Join(w.dir(t, c, repo), "objects", "pack", "pack-"+rows[1].GitChecksum+".pack")
			if err := os.Chmod(path, 0o600); err != nil {
				t.Fatal(err)
			}
			if err := os.Truncate(path, rows[1].Ref().PackBytes()/2); err != nil {
				t.Fatal(err)
			}
			return 1
		}},
		{"an alternates file", func(t *testing.T, w *syncWorld, c *cache.Cache, repo string, _ []code.PackRow) int {
			write(t, filepath.Join(w.dir(t, c, repo), "objects", "info", "alternates"), t.TempDir()+"\n")
			return 0
		}},
		{"a config with an extra line", func(t *testing.T, w *syncWorld, c *cache.Cache, repo string, _ []code.PackRow) int {
			path := filepath.Join(w.dir(t, c, repo), "config")
			write(t, path, read(t, path)+"\tfsmonitor = true\n")
			return 0
		}},
		{"an unreadable state", func(t *testing.T, w *syncWorld, c *cache.Cache, repo string, _ []code.PackRow) int {
			write(t, filepath.Join(w.dir(t, c, repo), "crewlet-state.json"), `{"format":1,`)
			return 0
		}},
	} {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()
			w := newSyncWorld(t, onMemory)
			repo := w.repo(t, 1)
			var rows []code.PackRow
			for range 3 {
				_, p := w.push(t, repo, "refs/heads/main", nil)
				rows = append(rows, p)
			}
			w.tag(t, repo, "v1", true)
			c := w.open(t)
			w.sync(t, c, repo).Release()

			downloads := row.prepare(t, w, c, repo, rows)
			before := w.objects.total()
			c = w.reopen(t, c)
			pages := len(w.log.firstPages(repo))
			s := w.sync(t, c, repo)

			first := w.log.firstPages(repo)[pages]
			if !first.Since.IsZero() {
				t.Errorf("the first round after the restart asked a delta from %s, want the whole answer", first.Since)
			}
			if got := w.image(t, c, repo); got != w.rebuilt(t, repo) {
				t.Errorf("the converged image:\n%s\nwant a fresh cache's:\n%s", got, w.rebuilt(t, repo))
			}
			if s.Kind != cache.SyncFull {
				t.Errorf("Kind = %s, want %s", s.Kind, cache.SyncFull)
			}
			if got := w.objects.total() - before; downloads >= 0 && got != downloads {
				t.Errorf("the round downloaded %d bundles, want %d (every pack the directory holds reused)", got, downloads)
			}
		})
	}
}

// removePack removes a pack's files from an incarnation, as a disk fault or
// an operator's rm would.
func removePack(t *testing.T, dir, checksum string, exts ...string) {
	t.Helper()
	for _, ext := range exts {
		if err := os.Remove(filepath.Join(dir, "objects", "pack", "pack-"+checksum+ext)); err != nil {
			t.Fatal(err)
		}
	}
}

// A DELTA CARRIES DELETIONS: a ref deleted in the log is a tombstone row, which
// a warm cache's delta reads and its packed-refs drops. Once the tombstones
// are swept, a cache older than the sweep cannot learn of a deletion from a
// delta, so it is answered whole — and the ref is gone all the same.
func TestADeltaCarriesDeletions(t *testing.T) {
	t.Parallel()
	w := newSyncWorld(t, onMemory)
	repo := w.repo(t, 1)
	w.push(t, repo, "refs/heads/main", nil)
	w.push(t, repo, "refs/heads/feature", nil)
	c := w.open(t)
	w.sync(t, c, repo).Release()

	w.log.write(repo, nil, map[string]string{"refs/heads/feature": ""})
	pages := len(w.log.firstPages(repo))
	s := w.sync(t, c, repo)
	if since := w.log.firstPages(repo)[pages].Since; since.IsZero() || s.Kind != cache.SyncDelta {
		t.Errorf("a warm cache's sync asked from %s and cost %s, want a delta", since, s.Kind)
	}
	if refs := read(t, filepath.Join(s.Dir, "packed-refs")); strings.Contains(refs, "refs/heads/feature") {
		t.Errorf("a deleted ref is still in packed-refs:\n%s", refs)
	}
	s.Release()

	w.push(t, repo, "refs/heads/topic", nil)
	w.log.write(repo, nil, map[string]string{"refs/heads/topic": ""})
	w.log.sweep(repo)
	s = w.sync(t, c, repo)
	if s.Kind != cache.SyncFull {
		t.Errorf("a cache older than the swept tombstones cost %s, want a full answer", s.Kind)
	}
	if refs := read(t, filepath.Join(s.Dir, "packed-refs")); strings.Contains(refs, "refs/heads/topic") {
		t.Errorf("a ref deleted and swept is still in packed-refs:\n%s", refs)
	}
	if got := w.image(t, c, repo); got != w.rebuilt(t, repo) {
		t.Errorf("image after the full answer:\n%s\nwant:\n%s", got, w.rebuilt(t, repo))
	}
	for kind, want := range map[cache.SyncKind]uint64{cache.SyncFull: 2, cache.SyncDelta: 1} {
		if got := histogramCount(w.rec, metrics.CodeCacheSyncDuration, "kind", string(kind)); got != want {
			t.Errorf("%s{kind=%s} counted %d syncs, want %d", metrics.CodeCacheSyncDuration, kind, got, want)
		}
	}
}

// A CALLER WHOSE NEED THE CACHE MEETS COSTS NO ESTATE CALL: stale, and session
// at a floor the cache has reached, are answered from what it holds — once
// this process has read the repository whole. Past the floor, on a repository
// it has never read, with a lag bound only the estate can measure, or from a
// state a previous process left, a caller is not.
func TestAStaleCallerIsAnsweredWithoutAnEstateCall(t *testing.T) {
	t.Parallel()
	w := newSyncWorld(t, onMemory)
	warm, cold := w.repo(t, 1), w.repo(t, 2)
	w.push(t, warm, "refs/heads/main", nil)
	w.push(t, cold, "refs/heads/main", nil)
	c := w.open(t)
	at := w.sync(t, c, warm).Position

	calls := func(repo string) int { return len(w.log.callsOf(repo)) }
	ask := func(repo string, fresh statelog.Freshness) cache.Synced {
		t.Helper()
		s, err := c.Sync(t.Context(), repo, fresh)
		if err != nil {
			t.Fatalf("Sync(%+v): %v", fresh, err)
		}
		t.Cleanup(s.Release)
		return s
	}
	for _, fresh := range []statelog.Freshness{
		stale,
		{Level: statelog.ReadConsistentPrefix},
		statelog.FloorAt(statelog.ReadSession, at),
	} {
		before := calls(warm)
		if s := ask(warm, fresh); s.Kind != cache.SyncNone || calls(warm) != before {
			t.Errorf("%s at the cache's own position cost %s and %d calls, want none", fresh.Level,
				s.Kind, calls(warm)-before)
		}
	}

	past := w.log.other()
	before := calls(warm)
	if s := ask(warm, statelog.FloorAt(statelog.ReadSession, past)); s.Kind != cache.SyncDelta ||
		calls(warm) != before+1 {
		t.Errorf("session past the cache's position cost %s and %d calls, want a delta and one", s.Kind,
			calls(warm)-before)
	}
	before = calls(warm)
	if ask(warm, statelog.Freshness{Level: statelog.ReadStale, MaxLagSeq: 5}); calls(warm) != before+1 {
		t.Errorf("a bounded stale caller cost %d calls, want one: only the estate measures lag", calls(warm)-before)
	}
	if ask(cold, stale); calls(cold) != 1 {
		t.Errorf("a stale caller of a repository never read cost %d calls, want one", calls(cold))
	}

	// A state a previous process left names a position on the log it was
	// read from, which a log restored while the node was down reuses for
	// other records: a caller after a restart is not handed it on its word.
	// The first round reads the repository whole, and then a stale caller
	// is answered at once again.
	c = w.reopen(t, c)
	before = calls(warm)
	if s := ask(warm, stale); s.Kind != cache.SyncFull || calls(warm) != before+1 {
		t.Errorf("a stale caller after a restart cost %s and %d calls, want a full round", s.Kind,
			calls(warm)-before)
	}
	before = calls(warm)
	if s := ask(warm, stale); s.Kind != cache.SyncNone || calls(warm) != before {
		t.Errorf("a stale caller after the restart's first round cost %s and %d calls, want none", s.Kind,
			calls(warm)-before)
	}

	// A directory with no state says nothing about what it reflects: a
	// stale caller is not handed it on its word.
	write(t, filepath.Join(w.dir(t, c, warm), "crewlet-state.json"), `{"format":`)
	c = w.reopen(t, c)
	before = calls(warm)
	if s := ask(warm, stale); s.Kind == cache.SyncNone || calls(warm) != before+1 {
		t.Errorf("a stale caller of a directory with no state cost %s and %d calls, want a round", s.Kind,
			calls(warm)-before)
	}
}

// A REPOSITORY IS PURGED ONLY BY THE LOG'S HEAD: a copy that has not applied a
// repository's create — or is behind it some other way — answers "no such
// repository" exactly as a purge does, so a not-found read below
// linearizable is asked again at the log's head before anything is retired;
// and a caller that arrived while that round ran is answered by a read that
// began after it, never by a stale copy's word.
func TestARepositoryIsPurgedOnlyByTheLogsHead(t *testing.T) {
	t.Parallel()
	w := newSyncWorld(t, onMemory)
	repo := w.repo(t, 1)
	w.push(t, repo, "refs/heads/main", nil)
	c := w.open(t)
	held := w.sync(t, c, repo)
	n := cache.Incarnation(held)

	reached, release := make(chan struct{}), make(chan struct{})
	var first sync.Once
	w.log.set(func(f *fakeLog) {
		f.hook = func(call fakeCall) error {
			if call.Level != statelog.ReadStale || call.Page != 1 {
				return nil
			}
			first.Do(func() {
				close(reached)
				<-release
			})
			return fmt.Errorf("%w: %s", code.ErrRepoNotFound, repo)
		}
	})
	before := len(w.log.callsOf(repo))
	// A bounded stale caller: only the estate can measure its lag, so it
	// is never answered from the cache at once.
	bounded := statelog.Freshness{Level: statelog.ReadStale, MaxLagSeq: 5}
	var a, b cache.Synced
	var aErr, bErr error
	var wg sync.WaitGroup
	wg.Go(func() { a, aErr = c.Sync(t.Context(), repo, bounded) })
	<-reached
	wg.Go(func() { b, bErr = c.Sync(t.Context(), repo, linearizable) })
	eventually(t, "the linearizable caller queued behind the stale round", func() bool {
		return cache.Waiting(c, repo) == 2
	})
	close(release)
	wg.Wait()
	if aErr != nil || bErr != nil {
		t.Fatalf("the callers of a repository a stale copy did not know: %v, %v", aErr, bErr)
	}
	defer a.Release()
	defer b.Release()
	for _, s := range []cache.Synced{a, b} {
		if cache.Incarnation(s) != n {
			t.Errorf("a caller was handed incarnation %d, want the one it had, %d", cache.Incarnation(s), n)
		}
	}
	var levels []string
	for _, call := range w.log.callsOf(repo)[before:] {
		levels = append(levels, string(call.Level))
	}
	if fmt.Sprint(levels) != "[stale linearizable linearizable]" {
		t.Errorf("the rounds read at %v, want stale, its re-read at linearizable, then the queued round's", levels)
	}
	if !statOK(t, held.Dir) || cache.Repos(c)[repo] != n {
		t.Error("a stale copy's not-found retired the repository's incarnation")
	}
}

// THE GAUGES ARE KEPT, NOT RECOUNTED: what every incarnation holds is a running
// total, moved where it moves — an install, a refs write, a sweep's measure, a
// discard — so a round sets the gauges in two loads rather than a walk of
// every repository on the node, and the total is always the files on disk: a
// purged incarnation a reader still holds counted until its last release
// moves it away.
func TestTheGaugesAreTheFilesOnDisk(t *testing.T) {
	t.Parallel()
	w := newSyncWorld(t, onMemory)
	a, b := w.repo(t, 1), w.repo(t, 2)
	for range 3 {
		w.push(t, a, "refs/heads/main", nil)
	}
	w.tag(t, a, "v1", true)
	w.push(t, b, "refs/heads/main", nil)
	c := w.open(t)
	sa := w.sync(t, c, a)
	sb := w.sync(t, c, b)
	check := func(when string, repos int, dirs ...string) {
		t.Helper()
		var want int64
		for _, dir := range dirs {
			want += heldBy(t, dir)
		}
		if got := gauge(t, w.rec, metrics.CodeCacheBytes); got != float64(want) {
			t.Errorf("%s: %s = %v, want the %d bytes on disk", when, metrics.CodeCacheBytes, got, want)
		}
		if got := gauge(t, w.rec, metrics.CodeCacheRepos); got != float64(repos) {
			t.Errorf("%s: %s = %v, want %d", when, metrics.CodeCacheRepos, got, repos)
		}
	}
	check("after two cold syncs", 2, sa.Dir, sb.Dir)

	w.push(t, a, "refs/heads/dev", nil)
	w.sync(t, c, a)
	check("after a delta", 2, sa.Dir, sb.Dir)

	w.log.purge(b)
	if _, err := c.Sync(t.Context(), b, linearizable); !errors.Is(err, code.ErrRepoNotFound) {
		t.Fatalf("a sync of a purged repository: %v", err)
	}
	check("with the purged repository's directory still held", 1, sa.Dir, sb.Dir)
	sb.Release()
	check("once its reader let go", 1, sa.Dir)

	c = w.reopen(t, c)
	check("after a restart", 1, w.dir(t, c, a))
}

// heldBy is what an incarnation's files hold, as the cache counts it: every
// file of objects/pack, packed-refs, the commit-graph and every run of the
// index of pushes.
func heldBy(t *testing.T, dir string) int64 {
	t.Helper()
	var total int64
	for _, name := range names(t, filepath.Join(dir, "objects", "pack")) {
		info, err := os.Stat(filepath.Join(dir, "objects", "pack", name))
		if err != nil {
			t.Fatal(err)
		}
		total += info.Size()
	}
	for _, path := range []string{filepath.Join(dir, "packed-refs"), filepath.Join(dir, "objects", "info", "commit-graph")} {
		if info, err := os.Stat(path); err == nil {
			total += info.Size()
		} else if !os.IsNotExist(err) {
			t.Fatal(err)
		}
	}
	// And the cache's own derived files, the index of pushes' runs under
	// crewlet/ — none before a delta has folded a push.
	if statOK(t, filepath.Join(dir, "crewlet")) {
		for _, name := range names(t, filepath.Join(dir, "crewlet")) {
			info, err := os.Stat(filepath.Join(dir, "crewlet", name))
			if err != nil {
				t.Fatal(err)
			}
			total += info.Size()
		}
	}
	return total
}

// CONCURRENT LINEARIZABLE CALLERS SHARE ONE SYNC: twenty callers of one cold
// repository cost the round running when they arrive and one round queued
// behind it — two page-1 reads, never twenty — and every caller is handed one
// directory.
func TestConcurrentLinearizableCallersShareOneSync(t *testing.T) {
	t.Parallel()
	w := newSyncWorld(t, onMemory)
	repo := w.repo(t, 1)
	w.push(t, repo, "refs/heads/main", nil)
	c := w.open(t)
	release := make(chan struct{})
	var first sync.Once
	w.log.set(func(f *fakeLog) {
		f.hook = func(call fakeCall) error {
			held := false
			first.Do(func() { held = true })
			if held {
				<-release
			}
			return nil
		}
	})
	const callers = 20
	got := make([]cache.Synced, callers)
	errs := make([]error, callers)
	var wg sync.WaitGroup
	for i := range callers {
		wg.Go(func() { got[i], errs[i] = c.Sync(t.Context(), repo, linearizable) })
	}
	eventually(t, "twenty callers waiting", func() bool { return cache.Waiting(c, repo) == callers })
	close(release)
	wg.Wait()
	for i := range callers {
		if errs[i] != nil {
			t.Fatalf("caller %d: %v", i, errs[i])
		}
		defer got[i].Release()
		if got[i].Dir != got[0].Dir || got[i].Position != got[callers-1].Position {
			t.Errorf("caller %d was handed %s at %s; caller 0 %s, the last %s", i, got[i].Dir, got[i].Position,
				got[0].Dir, got[callers-1].Position)
		}
	}
	if n := len(w.log.firstPages(repo)); n != 2 {
		t.Errorf("%d callers cost %d page-1 reads, want 2: the round running and the one queued behind it", callers, n)
	}
}

// A LINEARIZABLE CALLER IS NEVER ANSWERED BY AN OLDER SYNC: a round already
// running read its barrier before the caller asked, and may predate the write
// the caller is about to read — so a caller that arrives while it runs is
// answered by the round after it, at a position past that write.
func TestALinearizableCallerIsNeverAnsweredByAnOlderSync(t *testing.T) {
	t.Parallel()
	w := newSyncWorld(t, onMemory)
	repo := w.repo(t, 1)
	w.push(t, repo, "refs/heads/main", nil)
	c := w.open(t)
	reached, held := make(chan struct{}), make(chan struct{})
	var first sync.Once
	w.log.set(func(f *fakeLog) {
		f.after = func(fakeCall, *code.RepoSnapshot) {
			hold := false
			first.Do(func() { hold = true })
			if hold {
				close(reached)
				<-held
			}
		}
	})
	var a, x cache.Synced
	var aErr, xErr error
	var wg sync.WaitGroup
	wg.Go(func() { a, aErr = c.Sync(t.Context(), repo, linearizable) })
	<-reached
	wg.Go(func() { x, xErr = c.Sync(t.Context(), repo, linearizable) })
	eventually(t, "the second caller waiting", func() bool { return cache.Waiting(c, repo) == 2 })
	_, row := w.push(t, repo, "refs/heads/main", nil)
	close(held)
	wg.Wait()
	if aErr != nil || xErr != nil {
		t.Fatalf("the callers: %v, %v", aErr, xErr)
	}
	defer a.Release()
	defer x.Release()
	if before, _ := a.Position.Before(row.Added); !before {
		t.Errorf("the first caller's round read at %s, after the write at %s it never waited for", a.Position, row.Added)
	}
	if before, _ := x.Position.Before(row.Added); before {
		t.Errorf("the caller that arrived during the first round was answered at %s, before the write at %s "+
			"that preceded its sync", x.Position, row.Added)
	}
}

// A CALLER THAT HANGS UP NEVER CANCELS THE SYNC: rounds run under the cache's
// own lifetime, so the others waiting on one are answered whatever the first
// caller's context does — and the pin a round takes for a caller that has gone
// is never left behind. The caller that hung up releases what it was
// answered, the zero Synced, which releases nothing: a holder deferring the
// release of a variable a later Sync may overwrite relies on it.
func TestACallerThatHangsUpNeverCancelsTheSync(t *testing.T) {
	t.Parallel()
	w := newSyncWorld(t, onMemory)
	repo := w.repo(t, 1)
	w.push(t, repo, "refs/heads/main", nil)
	c := w.open(t)
	reached, held := make(chan struct{}), make(chan struct{})
	var first sync.Once
	w.log.set(func(f *fakeLog) {
		f.hook = func(fakeCall) error {
			hold := false
			first.Do(func() { hold = true })
			if hold {
				close(reached)
				<-held
			}
			return nil
		}
	})
	ctx, cancel := context.WithCancel(t.Context())
	firstErr := make(chan error, 1)
	go func() {
		s, err := c.Sync(ctx, repo, stale)
		s.Release() // the zero Synced when it failed: a no-op, never a nil call
		firstErr <- err
	}()
	<-reached
	var second cache.Synced
	var secondErr error
	done := make(chan struct{})
	go func() {
		defer close(done)
		second, secondErr = c.Sync(t.Context(), repo, stale)
	}()
	eventually(t, "both callers on the round", func() bool { return cache.Waiting(c, repo) == 2 })
	cancel()
	if err := <-firstErr; !errors.Is(err, context.Canceled) {
		t.Fatalf("the caller that hung up got %v, want context.Canceled", err)
	}
	close(held)
	<-done
	if secondErr != nil {
		t.Fatalf("the caller still waiting got %v: the first caller's hang-up cancelled the round", secondErr)
	}
	if got := cache.Pins(c, repo); got != 1 {
		t.Errorf("pins = %d while the second caller holds the directory, want 1", got)
	}
	second.Release()
	second.Release() // idempotent
	if got := cache.Pins(c, repo); got != 0 {
		t.Errorf("pins = %d after every caller let go, want 0", got)
	}
	cache.Synced{}.Release() // what a failed Sync answers releases nothing
}

// A REQUEST THAT IS NOT A SYNC IS REFUSED before any estate call: a sync
// states its freshness, a session sync its floor, a floor is on the code log,
// a repository id is a path component, and a stopped cache syncs nothing.
func TestARequestThatIsNotASyncIsRefused(t *testing.T) {
	t.Parallel()
	w := newSyncWorld(t, onMemory)
	repo := w.repo(t, 1)
	c := w.open(t)
	onPages := statelog.Position{Stream: topics.PagesLogStream, Seq: 3}
	for _, ask := range []struct {
		name  string
		repo  string
		fresh statelog.Freshness
		want  error
	}{
		{"no level", repo, statelog.Freshness{}, code.ErrBadLevel},
		{"an unknown level", repo, statelog.Freshness{Level: "eventual"}, code.ErrBadLevel},
		{"session with no floor", repo, statelog.Freshness{Level: statelog.ReadSession}, code.ErrBadLevel},
		{"a floor on another log", repo, statelog.FloorAt(statelog.ReadSession, onPages), cache.ErrInvalid},
		{"a path for an id", "../x", linearizable, cache.ErrInvalid},
		{"an id in upper case", strings.ToUpper(repo), linearizable, cache.ErrInvalid},
	} {
		if _, err := c.Sync(t.Context(), ask.repo, ask.fresh); !errors.Is(err, ask.want) {
			t.Errorf("%s: %v, want %v", ask.name, err, ask.want)
		}
	}
	c.Stop(context.Background())
	if _, err := c.Sync(t.Context(), repo, linearizable); !errors.Is(err, cache.ErrStopped) {
		t.Errorf("a sync after Stop: %v, want ErrStopped", err)
	}
	if calls := w.log.callsOf(repo); len(calls) != 0 {
		t.Errorf("refused requests made %d estate calls", len(calls))
	}
}

// THE STATE IS WRITTEN ONLY WHEN THE DIRECTORY CHANGED: a repository fetched a
// hundred times a second while nobody pushes to it writes nothing, its
// position moving in memory alone — which is correct because nothing between
// the state's position and the new one changed the directory.
func TestTheStateIsWrittenOnlyWhenTheDirectoryChanged(t *testing.T) {
	t.Parallel()
	w := newSyncWorld(t, onMemory)
	repo := w.repo(t, 1)
	w.push(t, repo, "refs/heads/main", nil)
	c := w.open(t)
	prev := w.sync(t, c, repo)
	statePath := filepath.Join(prev.Dir, "crewlet-state.json")
	written := read(t, statePath)
	for range 3 {
		w.log.other()
		s := w.sync(t, c, repo)
		if before, _ := prev.Position.Before(s.Position); !before {
			t.Errorf("a sync after another repository's record answered %s, not past %s", s.Position, prev.Position)
		}
		if got := read(t, statePath); got != written {
			t.Errorf("a round that changed nothing rewrote the state:\n%s\nwas:\n%s", got, written)
		}
		prev = s
	}

	c = w.reopen(t, c)
	pages := len(w.log.firstPages(repo))
	if s := w.sync(t, c, repo); s.Kind != cache.SyncFull || s.Position != prev.Position {
		t.Errorf("the first round after a restart cost %s at %s, want a full read at %s", s.Kind, s.Position, prev.Position)
	}
	if since := w.log.firstPages(repo)[pages].Since; !since.IsZero() {
		t.Errorf("the first round after a restart asked from %s, want the whole answer", since)
	}
	if got := w.image(t, c, repo); got != w.rebuilt(t, repo) {
		t.Errorf("image:\n%s\nwant:\n%s", got, w.rebuilt(t, repo))
	}
	written = read(t, statePath)
	w.push(t, repo, "refs/heads/main", nil)
	if s := w.sync(t, c, repo); s.Kind != cache.SyncDelta {
		t.Errorf("a sync after a push cost %s, want a delta", s.Kind)
	}
	if read(t, statePath) == written {
		t.Error("a round that installed a pack and moved a ref left the state as it was")
	}
}

// A REPOSITORY NO ROW HOLDS COSTS NO DIRECTORY: an incarnation is made once
// the rows say the repository exists, so a sync of an id nobody holds is told
// so and leaves nothing under repos/ or in trash/ — and no entry behind it.
func TestARepositoryNoRowHoldsCostsNoDirectory(t *testing.T) {
	t.Parallel()
	w := newSyncWorld(t, onMemory)
	c := w.open(t)
	cache.JanitorPass(c) // the boot's own trash
	nobody := "00000000-0000-4000-8000-000000000001"
	if _, err := c.Sync(t.Context(), nobody, linearizable); !errors.Is(err, code.ErrRepoNotFound) {
		t.Fatalf("a sync of a repository no row holds: %v, want code.ErrRepoNotFound", err)
	}
	if got := names(t, w.path("repos")); len(got) != 0 {
		t.Errorf("repos/ holds %v", got)
	}
	if got := names(t, w.path("trash")); len(got) != 0 {
		t.Errorf("trash/ holds %v: a directory was made for a repository nobody holds", got)
	}
	if got := cache.Repos(c); len(got) != 0 {
		t.Errorf("the cache holds %v", got)
	}
}

// A CACHE HOLDS A COPY ONLY ONCE IT HAS ONE OR IS FILLING ONE — what a git node
// asks before a read it was not asked to fill (ADR-0082), so that one node a
// walk fills a repository nobody holds. Not one it never synced, nor one whose
// only round ended before any copy (its estate read refused); one whose round
// is filling it now, before the round has made its copy; one it synced; one a
// restarted process finds on its disk before any round has run on it; and
// never one purged.
//
// Mutations: answer from the entry alone (any entry not purged) — the round
// that ended with no copy leaves the repository held, red; leave the running
// round out — the cache filling the repository holds nothing, red.
func TestACacheHoldsACopyOnlyOnceItHasOneOrIsFillingOne(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	w := newSyncWorld(t, onMemory)
	repo := w.repo(t, 1)
	w.push(t, repo, "refs/heads/main", nil)
	c := w.open(t)
	if c.Holds(repo) {
		t.Fatal("a cache that never synced the repository holds it")
	}

	refused := errors.New("the estate is out of reach")
	w.log.set(func(l *fakeLog) { l.hook = func(fakeCall) error { return refused } })
	if _, err := c.Sync(ctx, repo, linearizable); err == nil {
		t.Fatal("a sync whose estate read is refused answered a copy")
	}
	if c.Holds(repo) {
		t.Error("a round that ended before any copy left the repository held")
	}

	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	w.log.set(func(l *fakeLog) {
		l.hook = func(fakeCall) error {
			once.Do(func() { close(entered) })
			<-release
			return nil
		}
	})
	filled := make(chan error, 1)
	go func() {
		s, err := c.Sync(ctx, repo, linearizable)
		s.Release()
		filled <- err
	}()
	<-entered
	if !c.Holds(repo) {
		t.Error("a cache whose round is filling the repository does not hold it")
	}
	close(release)
	if err := <-filled; err != nil {
		t.Fatalf("the filling round: %v", err)
	}
	w.log.set(func(l *fakeLog) { l.hook = nil })
	if !c.Holds(repo) {
		t.Error("a cache that synced the repository does not hold it")
	}

	c = w.reopen(t, c)
	if !c.Holds(repo) {
		t.Error("a restarted cache does not hold the copy its disk keeps")
	}
	w.log.purge(repo)
	if _, err := c.Sync(ctx, repo, linearizable); !errors.Is(err, code.ErrRepoNotFound) {
		t.Fatalf("a sync of a purged repository: %v, want code.ErrRepoNotFound", err)
	}
	if c.Holds(repo) {
		t.Error("a cache holds a purged repository")
	}
}

// A PURGED REPOSITORY IS REMOVED ONCE UNPINNED: the rows are gone, so the
// cache's entry is too and every sync is told so — but a reader already in the
// directory finishes on it, and only its release moves it into trash/.
func TestAPurgedRepositoryIsRemovedOnceUnpinned(t *testing.T) {
	t.Parallel()
	w := newSyncWorld(t, onMemory)
	repo := w.repo(t, 1)
	tip, _ := w.push(t, repo, "refs/heads/main", nil)
	c := w.open(t)
	held, err := c.Sync(t.Context(), repo, linearizable)
	if err != nil {
		t.Fatal(err)
	}
	w.log.purge(repo)
	if _, err = c.Sync(t.Context(), repo, linearizable); !errors.Is(err, code.ErrRepoNotFound) {
		t.Fatalf("a sync of a purged repository: %v, want code.ErrRepoNotFound", err)
	}
	if _, err = runGit(t, w.pool, held.Dir, "", "cat-file", "-e", tip); err != nil {
		t.Fatalf("the reader's directory stopped serving under it: %v", err)
	}
	held.Release()
	if statOK(t, held.Dir) {
		t.Fatal("the purged repository's directory stayed in repos/ after its last reader let go")
	}
	cache.JanitorPass(c)
	if got := names(t, w.path("trash")); len(got) != 0 {
		t.Errorf("trash/ after a pass holds %v", got)
	}
	if got := gauge(t, w.rec, metrics.CodeCacheRepos); got != 0 {
		t.Errorf("%s = %v, want 0", metrics.CodeCacheRepos, got)
	}
}
