package cache_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/code/cache"
	"github.com/crewlet/crewlet/internal/code/gitproc"
	"github.com/crewlet/crewlet/internal/procgroup/procgrouptest"
)

// hangDetector bounds a case's wait for a second process or for the
// janitor: a claim that blocked rather than refused, or a kick nothing
// answered, would otherwise hold the case until the package's timeout and say
// nothing about why. Never a performance claim.
const hangDetector = 30 * time.Second

// Repository ids the cases use.
const (
	repoA = "0b6b3cb1-2f2c-4f5e-9d6e-0c7a3b8e1f21"
	repoB = "5f0c2b0e-8d0b-4a8e-b2d4-1c9f3e7a6d55"
	repoC = "c3a1f9d2-6b1e-4c2a-8f3d-2e5b7a9c0d14"
)

// eventually waits, under the hang detector, until cond holds.
func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(hangDetector)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("%s: not within %v", what, hangDetector)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// OPEN TAKES ONLY WHAT PREPARE RETURNED, ONCE: the Layout carries Prepare's
// claim and its reading of repos/, so a Layout built by hand — even one naming
// a prepared directory — has neither, a Layout whose directory was changed
// names one Prepare did not read, and a Layout already opened has handed both
// on.
func TestOpenTakesOnlyALayoutPrepareReturned(t *testing.T) {
	t.Parallel()
	w := newWorld(t)

	t.Run("built by hand", func(t *testing.T) {
		_, err := cache.Open(t.Context(), w.options(t, func(o *cache.Options) {
			o.Layout = cache.Layout{Dir: w.layout.Dir, Home: w.layout.Home}
		}))
		if !errors.Is(err, cache.ErrNotACache) || !strings.Contains(err.Error(), "cache.Prepare") {
			t.Fatalf("Open of a hand-built Layout = %v, want ErrNotACache naming cache.Prepare", err)
		}
	})

	t.Run("another directory", func(t *testing.T) {
		_, err := cache.Open(t.Context(), w.options(t, func(o *cache.Options) {
			o.Layout.Dir = t.TempDir()
		}))
		if !errors.Is(err, cache.ErrOptions) || !strings.Contains(err.Error(), "Layout.Dir") {
			t.Fatalf("Open of a Layout naming another directory = %v, want ErrOptions naming Layout.Dir", err)
		}
	})

	t.Run("opened already", func(t *testing.T) {
		layout := w.layout
		c, err := cache.Open(t.Context(), w.options(t))
		if err != nil {
			t.Fatalf("Open: %v", err)
		}
		c.Stop(context.Background())
		_, err = cache.Open(t.Context(), w.options(t, func(o *cache.Options) { o.Layout = layout }))
		if !errors.Is(err, cache.ErrOptions) || !strings.Contains(err.Error(), "cache.Prepare") {
			t.Fatalf("a second Open of one Layout = %v, want ErrOptions naming cache.Prepare", err)
		}
	})
}

// ONE CACHE A POOL. The pool reports every run to one observer, which Open
// installs last: a second cache opened over a pool another cache observes is
// refused by name, and once that cache stops — Stop gives its observer back —
// a cache over the same pool opens.
//
// Mutation: Stop keeping its observer (the second cache is refused).
func TestOneCacheObservesAPool(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	first := w.open(t)
	pool := w.pool
	open := func() (*cache.Cache, error) {
		t.Helper()
		layout, err := cache.Prepare(t.Context(), filepath.Join(t.TempDir(), "second"))
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(layout.Release)
		return cache.Open(t.Context(), w.options(t, func(o *cache.Options) { o.Layout, o.Git = layout, pool }))
	}
	if c, err := open(); !errors.Is(err, cache.ErrOptions) || !errors.Is(err, gitproc.ErrInvalid) ||
		!strings.Contains(err.Error(), "stop that cache first") {
		if c != nil {
			c.Stop(context.Background())
		}
		t.Fatalf("a second cache over an observed pool = %v, want ErrOptions naming the cache to stop", err)
	}
	first.Stop(context.Background())
	second, err := open()
	if err != nil {
		t.Fatalf("a cache over the pool once its first stopped: %v", err)
	}
	second.Stop(context.Background())
}

// ONE PROCESS A CACHE DIRECTORY. Two engines pointed at one git.cache_dir
// would empty each other's tmp/ and scratch/ at boot and race on every
// repository's files, so the second is refused, told who holds it — from the
// first one's Prepare on, and before the second's Prepare moves the holder's
// live HOME aside — and the claim is the kernel's, so it is free again the
// moment the holder stops.
func TestTwoProcessesNeverShareACache(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	home, err := os.Stat(w.layout.Home)
	if err != nil {
		t.Fatal(err)
	}
	refusedElsewhere := func(when string) {
		t.Helper()
		out, err := openInAnotherProcess(t, w.layout.Dir)
		if err == nil || !strings.Contains(out, inUseMarker) {
			t.Fatalf("a second process opened a cache held %s, or failed otherwise: %v: %s", when, err, out)
		}
		if want := fmt.Sprintf("pid %d", os.Getpid()); !strings.Contains(out, want) {
			t.Errorf("the refusal %s does not name the holder (%q): %s", when, want, out)
		}
	}

	refusedElsewhere("between its Prepare and its Open")
	// Its pool made a HOME of its own at the same path had it moved the
	// holder's, so the question is whether the directory is the same one.
	if after, err := os.Stat(w.layout.Home); err != nil || !os.SameFile(home, after) {
		t.Errorf("the refused process moved the holder's HOME aside (%v)", err)
	}

	c, err := cache.Open(t.Context(), w.options(t))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	refusedElsewhere("while it is open")

	c.Stop(context.Background())
	out, err := openInAnotherProcess(t, w.layout.Dir)
	if err != nil || !strings.Contains(out, openedMarker) {
		t.Fatalf("after the holder stopped, a second process could not open the cache: %v: %s", err, out)
	}
}

// openInAnotherProcess runs this test binary as a second process opening the
// cache at dir.
func openInAnotherProcess(t *testing.T, dir string) (string, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), hangDetector)
	defer cancel()
	cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=NoSuchTest") //nolint:gosec // os.Args[0] is this test binary
	cmd.Env = append(os.Environ(), procgrouptest.GORACE+"="+procgrouptest.StandInRaceOptions(), openEnv+"="+dir)
	out, err := cmd.CombinedOutput()
	if ctx.Err() != nil && t.Context().Err() == nil {
		t.Fatalf("the second process has not answered in %v: %s", hangDetector, out)
	}
	return string(out), err
}

// EVERY REQUIRED FIELD IS REFUSED BY NAME, before the directory is touched: a
// cache missing its pool, its rows or its object store would open and then
// fail its first sync, in a request, far from the wiring that forgot it.
func TestOptionsAreRefusedByName(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	write(t, w.path("tmp", "kept"), "a refused Open touches nothing")

	cases := map[string]struct {
		mutate func(*cache.Options)
		names  string
	}{
		"no Layout":       {func(o *cache.Options) { o.Layout = cache.Layout{} }, "Layout"},
		"no budget":       {func(o *cache.Options) { o.MaxBytes = 0 }, "MaxBytes"},
		"a budget below":  {func(o *cache.Options) { o.MaxBytes = -1 }, "MaxBytes"},
		"no pool":         {func(o *cache.Options) { o.Git = nil }, "Git"},
		"no object store": {func(o *cache.Options) { o.Objects = nil }, "Objects"},
		"no rows":         {func(o *cache.Options) { o.Fetch = nil }, "Fetch"},
		"no limits":       {func(o *cache.Options) { o.Limits = nil }, "Limits"},
		"no clock":        {func(o *cache.Options) { o.Now = nil }, "Now"},
		"no hook command": {func(o *cache.Options) { o.HookCommand = nil }, "HookCommand"},
		"a relative hook": {func(o *cache.Options) { o.HookCommand = []string{"crewlet", "code"} }, "HookCommand[0]"},
		"a NUL in a hook argument": {
			func(o *cache.Options) { o.HookCommand = []string{"/bin/true", "a\x00b"} }, "HookCommand[1]",
		},
	}
	t.Run("refused", func(t *testing.T) {
		for name, tc := range cases {
			t.Run(name, func(t *testing.T) {
				t.Parallel()
				_, err := cache.Open(t.Context(), w.options(t, tc.mutate))
				if !errors.Is(err, cache.ErrOptions) || !strings.Contains(err.Error(), tc.names) {
					t.Fatalf("Open with %s = %v, want ErrOptions naming %s", name, err, tc.names)
				}
			})
		}
	})

	// Every refusal left the directory as it was and the Layout unopened —
	// and a nil Logger and a nil Metrics are legal.
	if !exists(w.path("tmp", "kept")) {
		t.Fatal("a refused Open touched tmp/")
	}
	c, err := cache.Open(t.Context(), w.options(t, func(o *cache.Options) { o.Logger, o.Metrics = nil, nil }))
	if err != nil {
		t.Fatalf("Open with no Logger and no Metrics, after every refusal: %v", err)
	}
	c.Stop(context.Background())
}

// A BOOT CLEARS WHAT THE LAST PROCESS LEFT IN FLIGHT, at once and without
// waiting on it: scratch/ and tmp/ held only that process's requests, so they
// are moved into trash/ as Open runs, and the janitor removes trash/ in the
// background. repos/ is the repositories, and is the scan's.
func TestABootSweepsScratchAndTmp(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	write(t, w.path("scratch", "0199a1d2-push.git", "objects", "pack", "x.pack"), "a push in flight")
	write(t, w.path("tmp", "stage-123"), "a staged file")
	write(t, w.path("trash", "old", "file"), "already being removed")
	kept := w.path("repos", repoA+".1.git", "objects", "pack", "pack-1.pack")
	write(t, kept, "a repository's pack")

	c := w.open(t)
	if got := names(t, w.path("scratch")); len(got) != 0 {
		t.Errorf("scratch/ after Open holds %v", got)
	}
	if got := names(t, w.path("tmp")); len(got) != 0 {
		t.Errorf("tmp/ after Open holds %v", got)
	}
	if got := read(t, kept); got != "a repository's pack" {
		t.Errorf("a repository's file changed at boot: %q", got)
	}

	cache.JanitorPass(c)
	if got := names(t, w.path("trash")); len(got) != 0 {
		t.Errorf("trash/ after a janitor pass holds %v", got)
	}
	if !exists(kept) {
		t.Error("the janitor removed a repository's file")
	}
}

// THE HOOK EXECS EXACTLY THE CONFIGURED COMMAND, whatever its arguments hold,
// and is settled before Open returns: a script just written can be refused by
// the kernel (text file busy) while a forked child still holds it open for
// writing, and the first push's receive-pack would be the one refused.
func TestTheHookScriptExecsTheConfiguredCommand(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	argv := []string{"/usr/bin/printf", "%s\n", "a b", "it's", "x"}
	c := w.open(t, func(o *cache.Options) { o.HookCommand = argv })
	hook := filepath.Join(c.Hooks(), "proc-receive")

	info, err := os.Stat(hook)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o500 {
		t.Errorf("the hook's mode is %v, want 0500", info.Mode().Perm())
	}
	want := "#!/bin/sh\n[ \"${1-}\" = --crewlet-probe ] && exit 0\nexec '/usr/bin/printf' '%s\n' 'a b' 'it'\\''s' 'x'\n"
	if got := read(t, hook); got != want {
		t.Errorf("the hook is\n%q\nwant\n%q", got, want)
	}

	// Exec'd directly, with no retry: Open settled it.
	out, err := exec.CommandContext(t.Context(), hook).Output()
	if err != nil {
		t.Fatalf("exec %s: %v", hook, err)
	}
	if string(out) != "a b\nit's\nx\n" {
		t.Errorf("the hook printed %q, want each argument on a line", out)
	}
	out, err = exec.CommandContext(t.Context(), hook, "--crewlet-probe").Output()
	if err != nil || len(out) != 0 {
		t.Errorf("the probe = %q, %v: want nothing and exit 0", out, err)
	}

	// A later Open writes it again: the binary's path changes across an
	// upgrade.
	c.Stop(context.Background())
	w.open(t, func(o *cache.Options) { o.HookCommand = []string{"/bin/true"} })
	if got := read(t, hook); !strings.HasSuffix(got, "exec '/bin/true'\n") {
		t.Errorf("a second Open did not rewrite the hook: %q", got)
	}
}

// THE HOOK CAN BE EXEC'D THE MOMENT OPEN RETURNS, whatever else the process
// is forking: a child forked while the script is open for writing holds it
// until that child execs — or, here, for as long as it lives, since the
// case hands it the descriptor — and the kernel refuses to exec a file
// anything holds open for writing (text file busy). The engine forks
// constantly — every git process — so the first push's receive-pack would be
// the exec refused. The case makes the window certain rather than likely: a
// child holds the staged hook's write descriptor for most of a second, and
// the hook is exec'd the moment Open returns, with no retry.
func TestTheHookIsSettledBeforeOpenReturns(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	var holders sync.WaitGroup
	defer holders.Wait()
	o := w.options(t)
	cache.WhileTheHookIsOpen(&o, func(f *os.File) {
		holder := exec.Command("/bin/sleep", "0.7") //nolint:gosec // a fixed command
		holder.ExtraFiles = []*os.File{f}
		if err := holder.Start(); err != nil {
			t.Errorf("starting the process that holds the hook open: %v", err)
			return
		}
		holders.Add(1)
		go func() {
			defer holders.Done()
			_ = holder.Wait()
		}()
	})
	c, err := cache.Open(t.Context(), o)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer c.Stop(context.Background())
	if err = exec.CommandContext(t.Context(), filepath.Join(c.Hooks(), "proc-receive")).Run(); err != nil {
		t.Fatalf("exec of the hook Open just wrote: %v", err)
	}
}

// A REPOSITORY HAS ONE DIRECTORY AFTER A BOOT: its highest incarnation, which
// holds the newest downloads whether it is a finished rebuild or a partial
// one. Every other entry of repos/ — a lower incarnation, a name that is not
// a repository's, a number spelled with a leading zero, an id not in its
// canonical spelling, a link — is moved aside and said so.
func TestOpenKeepsOneIncarnationPerRepository(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	writeState(t, w.path("repos", repoA+".1.git"), stateDoc{Format: 2, RepoID: repoA, DiskBytes: 7})
	write(t, w.path("repos", repoA+".3.git", "objects", "pack", "pack-9.pack"), strings.Repeat("p", 300))
	write(t, w.path("repos", repoA+".3.git", "packed-refs"), strings.Repeat("r", 20))
	write(t, w.path("repos", repoA+".3.git", "HEAD"), "ref: refs/heads/main\n")
	write(t, w.path("repos", "junk", "file"), "a stray")
	write(t, w.path("repos", repoA+".01.git", "HEAD"), "ref: refs/heads/main\n")
	write(t, w.path("repos", strings.ToUpper(repoB)+".1.git", "HEAD"), "ref: refs/heads/main\n")
	write(t, w.path("repos", repoB+".1.git"), "a file, not a directory")
	if err := os.Symlink(t.TempDir(), w.path("repos", repoB+".2.git")); err != nil {
		t.Fatal(err)
	}

	c := w.open(t)
	if got := names(t, w.path("repos")); !slices.Equal(got, []string{repoA + ".3.git"}) {
		t.Errorf("repos/ after Open holds %v, want only %s.3.git", got, repoA)
	}
	if got := cache.Repos(c); len(got) != 1 || got[repoA] != 3 {
		t.Errorf("the cache holds %v, want %s at incarnation 3", got, repoA)
	}
	if cache.HasState(c, repoA) {
		t.Error("an incarnation with no state file was read as having one")
	}

	trashed := map[string]bool{}
	for _, line := range w.logs.lines(t, "code_cache_trashed") {
		trashed[filepath.Base(fmt.Sprint(line["path"]))] = true
	}
	for _, name := range []string{repoA + ".1.git", "junk", repoA + ".01.git", strings.ToUpper(repoB) + ".1.git", repoB + ".1.git", repoB + ".2.git"} {
		if !trashed[name] {
			t.Errorf("%s was not logged code_cache_trashed (logged: %v)", name, trashed)
		}
	}
	if got := len(names(t, w.path("trash"))); got < 6 {
		t.Errorf("trash/ holds %d entries, want the six moved aside (and the boot's)", got)
	}

	opened := w.logs.lines(t, "code_cache_opened")
	if len(opened) != 1 {
		t.Fatalf("code_cache_opened logged %d times", len(opened))
	}
	if opened[0]["repos"] != float64(1) || opened[0]["trashed"] != float64(6) {
		t.Errorf("code_cache_opened = %v, want one repository and six trashed", opened[0])
	}
	// With no state, what the incarnation holds is measured: its objects
	// and packed-refs, and nothing else of it.
	if opened[0]["bytes"] != float64(320) {
		t.Errorf("code_cache_opened reports %v bytes, want the 320 under objects/ and packed-refs", opened[0]["bytes"])
	}
}

// STOP GIVES THE DIRECTORY BACK BEFORE IT RETURNS when nothing of the cache's
// is running in it, whatever its context says. A harness stops with a
// context the test has already ended, and a node stops with one its shutdown
// may already have spent; deciding "is anything running?" by racing that
// context against the answer took the dead context's branch nearly every
// time, logged a stop that had not happened, and left the claim held past
// Stop — so the next boot of the directory was refused. Here every round's
// Stop gets an ended context, and the directory is prepared again at once; a
// started cache whose janitor is parked between passes holds nothing either.
func TestStopWithNothingRunningFreesTheDirectoryAtOnce(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	ended, cancel := context.WithCancel(context.Background())
	cancel()
	for round := range 20 {
		c := w.open(t)
		if round%2 == 1 {
			c.Start(t.Context())
			eventually(t, "the janitor's first pass", func() bool {
				return cache.Running(c) == 0 && len(names(t, w.path("trash"))) == 0
			})
		}
		c.Stop(ended)
		layout, err := cache.Prepare(t.Context(), w.layout.Dir)
		if err != nil {
			t.Fatalf("round %d: Prepare right after Stop(ended context) = %v, want the directory free", round, err)
		}
		layout.Release()
	}
	if got := w.logs.lines(t, "code_cache_stop_unfinished"); len(got) != 0 {
		t.Errorf("a Stop with nothing running logged code_cache_stop_unfinished %d times", len(got))
	}
}

// A STARTED CACHE EMPTIES trash/ WHEN KICKED, with no pass asked of it: the
// boot's own moves at once, and a retired incarnation nobody holds the moment
// it is retired — not at a tick, which this case never delivers, so no tick
// can pass for the kick.
func TestAStartedCacheEmptiesTrashWhenKicked(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	write(t, w.path("tmp", "stage-1"), "the last process's staged file")
	writeState(t, w.path("repos", repoA+".1.git"), aState(repoA))
	c := w.open(t, func(o *cache.Options) { cache.TicksFrom(o, make(chan time.Time)) })
	c.Start(t.Context())
	eventually(t, "the boot's moves leave trash/", func() bool { return len(names(t, w.path("trash"))) == 0 })

	if !cache.Retire(c, repoA) {
		t.Fatal("nothing to retire")
	}
	if exists(w.path("repos", repoA+".1.git")) {
		t.Fatal("an unpinned incarnation stayed in repos/ after it was retired")
	}
	eventually(t, "the retired incarnation leaves trash/", func() bool { return len(names(t, w.path("trash"))) == 0 })
}

// NOTHING OF A STOPPED CACHE STARTS AGAIN: a Start after Stop runs no
// janitor, so nothing of this process touches a directory that may already be
// the next boot's.
func TestStartAfterStopDoesNothing(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	c := w.open(t)
	c.Stop(context.Background())
	c.Start(t.Context())
	write(t, w.path("trash", "the-next-boots"), "not this cache's to remove")
	time.Sleep(200 * time.Millisecond)
	if !exists(w.path("trash", "the-next-boots")) {
		t.Fatal("a cache started after its Stop removed a file from trash/")
	}
	if got := cache.Running(c); got != 0 {
		t.Errorf("a stopped cache has %d pieces of work running", got)
	}
}

// heldRemoval opens a started cache whose janitor stops inside its first
// removal until release is called — a pass in flight, held still.
func heldRemoval(t *testing.T, w *world) (c *cache.Cache, release func()) {
	t.Helper()
	write(t, w.path("trash", "large"), "what the pass is removing")
	in := make(chan struct{})
	held := make(chan struct{})
	var once sync.Once
	release = func() { once.Do(func() { close(held) }) }
	var first sync.Once
	c = w.open(t, func(o *cache.Options) {
		cache.BeforeRemoving(o, func(string) {
			first.Do(func() {
				close(in)
				<-held
			})
		})
	})
	// Released before the cleanup Stop waits for the pass (cleanups run
	// last-registered first).
	t.Cleanup(release)
	c.Start(t.Context())
	select {
	case <-in:
	case <-time.After(hangDetector):
		t.Fatalf("the janitor's first pass did not reach a removal in %v", hangDetector)
	}
	return c, release
}

// STOP WAITS FOR A PASS IN FLIGHT before it gives the directory back: a
// janitor still removing in trash/ is this process writing in a directory the
// next boot would own.
func TestStopWaitsForAPassInFlight(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	c, release := heldRemoval(t, w)

	stopped := make(chan struct{})
	go func() {
		c.Stop(context.Background())
		close(stopped)
	}()
	select {
	case <-stopped:
		t.Fatal("Stop returned while a janitor pass was in flight")
	case <-time.After(200 * time.Millisecond):
	}
	if _, err := cache.Prepare(t.Context(), w.layout.Dir); !errors.Is(err, cache.ErrCacheInUse) {
		t.Fatalf("Prepare while the stopping cache's pass is in flight = %v, want ErrCacheInUse", err)
	}
	release()
	select {
	case <-stopped:
	case <-time.After(hangDetector):
		t.Fatalf("Stop has not returned %v after the pass was let go", hangDetector)
	}
	layout, err := cache.Prepare(t.Context(), w.layout.Dir)
	if err != nil {
		t.Fatalf("Prepare after Stop returned = %v, want the directory free", err)
	}
	layout.Release()
}

// A STOP ITS CONTEXT CUTS SHORT KEEPS THE CLAIM until the last of the cache's
// work has ended: it returns, and says so, but another process is refused
// the directory while this one is still writing in it — and gets it once the
// work ends.
func TestAStopCutShortKeepsTheClaimUntilTheWorkEnds(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	c, release := heldRemoval(t, w)

	short, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
	defer cancel()
	c.Stop(short)
	if got := w.logs.lines(t, "code_cache_stop_unfinished"); len(got) != 1 {
		t.Errorf("a Stop cut short logged code_cache_stop_unfinished %d times, want once", len(got))
	}
	out, err := openInAnotherProcess(t, w.layout.Dir)
	if err == nil || !strings.Contains(out, inUseMarker) {
		t.Fatalf("a second process opened a directory whose cache is still removing in it: %v: %s", err, out)
	}
	if want := fmt.Sprintf("pid %d", os.Getpid()); !strings.Contains(out, want) {
		t.Errorf("the refusal does not name the holder (%q): %s", want, out)
	}

	release()
	c.Stop(context.Background()) // waits for the pass, now let go
	out, err = openInAnotherProcess(t, w.layout.Dir)
	if err != nil || !strings.Contains(out, openedMarker) {
		t.Fatalf("after the pass ended, a second process could not open the cache: %v: %s", err, out)
	}
}

// A REPOSITORY'S LAST USE SURVIVES A RESTART, to within a tick: a boot reads
// it from the disk, and a state is rewritten only when its directory changes,
// so a repository read all day and pushed to rarely came back from every
// restart as the oldest entry there is — the first the budget evicts, and a
// GET a push to bring back. Each tick stamps the moment of last use on what
// the boot reads, and only for a repository used since its last stamp. The
// tick's step is run here synchronously (RecordUse), over a repository with a
// state and one without; TestTheJanitorsTickStampsTheLastUse is the janitor
// running it at its tick, and at nothing else.
func TestALastUseSurvivesARestart(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	then := time.Date(2026, 9, 1, 8, 0, 0, 0, time.UTC)
	used := time.Date(2026, 10, 1, 9, 30, 0, 0, time.UTC)
	for _, id := range []string{repoA, repoB} {
		writeState(t, w.path("repos", id+".1.git"), aState(id))
		if err := os.Chtimes(w.path("repos", id+".1.git", "crewlet-state.json"), then, then); err != nil {
			t.Fatal(err)
		}
	}
	// C has no state: what a boot reads is its directory's.
	write(t, w.path("repos", repoC+".1.git", "objects", "pack", "pack-1.pack"), "p")
	if err := os.Chtimes(w.path("repos", repoC+".1.git"), then, then); err != nil {
		t.Fatal(err)
	}

	clock := func() time.Time { return used }
	c := w.open(t, func(o *cache.Options) { o.Now = clock })
	for _, id := range []string{repoA, repoC} {
		s, ok := cache.Pin(c, id)
		if !ok {
			t.Fatalf("nothing to pin for %s", id)
		}
		s.Release()
	}
	cache.RecordUse(c)
	info, err := os.Stat(w.path("repos", repoB+".1.git", "crewlet-state.json"))
	if err != nil {
		t.Fatal(err)
	}
	if !info.ModTime().Equal(then) {
		t.Errorf("an unused repository's state was stamped %v", info.ModTime())
	}
	c.Stop(context.Background())

	again := w.open(t)
	for id, want := range map[string]time.Time{repoA: used, repoB: then, repoC: used} {
		if got := cache.LastUsed(again, id); !got.Equal(want) {
			t.Errorf("after a restart %s was last used %v, want %v", id, got, want)
		}
	}
}

// THE JANITOR'S TICK IS WHAT STAMPS A LAST USE, and only its tick: a started
// cache stamps, at each tick, the last use of every repository used since the
// one before — nothing asks for it, so a node that ran a day comes back from a
// restart knowing what it served — and a pass a kick woke stamps nothing,
// which is what holds the cost to one metadata write per used repository per
// tick however often the janitor is kicked.
func TestTheJanitorsTickStampsTheLastUse(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	then := time.Date(2026, 9, 1, 8, 0, 0, 0, time.UTC)
	used := time.Date(2026, 10, 1, 9, 30, 0, 0, time.UTC)
	stateOf := func(id string) string { return w.path("repos", id+".1.git", "crewlet-state.json") }
	stamped := func(id string) time.Time {
		t.Helper()
		info, err := os.Stat(stateOf(id))
		if err != nil {
			t.Fatal(err)
		}
		return info.ModTime()
	}
	for _, id := range []string{repoA, repoB, repoC} {
		writeState(t, w.path("repos", id+".1.git"), aState(id))
		if err := os.Chtimes(stateOf(id), then, then); err != nil {
			t.Fatal(err)
		}
	}
	ticks := make(chan time.Time)
	c := w.open(t, func(o *cache.Options) {
		o.Now = func() time.Time { return used }
		cache.TicksFrom(o, ticks)
	})
	// B is never used. Its state is moved to a time of its own once the boot
	// has read its last use, so a stamp of B shows even when what it writes
	// is the last use the boot read.
	untouched := time.Date(2026, 9, 15, 12, 0, 0, 0, time.UTC)
	if err := os.Chtimes(stateOf(repoB), untouched, untouched); err != nil {
		t.Fatal(err)
	}
	c.Start(t.Context())
	// Nothing of the cache's is running and trash/ is empty only once the
	// pass that emptied it has ended, whatever that pass was going to stamp.
	passed := func() bool { return cache.Running(c) == 0 && len(names(t, w.path("trash"))) == 0 }
	eventually(t, "the janitor's first pass", passed)

	s, ok := cache.Pin(c, repoA)
	if !ok {
		t.Fatal("nothing to pin for A")
	}
	s.Release()

	// A kick: C, retired with nobody holding it, goes into trash/ and wakes
	// the janitor.
	if !cache.Retire(c, repoC) {
		t.Fatal("nothing to retire")
	}
	eventually(t, "the pass the kick woke", passed)
	if got := stamped(repoA); !got.Equal(then) {
		t.Errorf("a pass a kick woke stamped a last use: A's state is at %v, want %v", got, then)
	}

	select {
	case ticks <- time.Now():
	case <-time.After(hangDetector):
		t.Fatalf("the janitor did not take a tick in %v", hangDetector)
	}
	eventually(t, "the tick stamps A's last use", func() bool {
		return cache.Running(c) == 0 && stamped(repoA).Equal(used)
	})
	if got := stamped(repoB); !got.Equal(untouched) {
		t.Errorf("a tick stamped a repository nobody used: B's state is at %v, want %v", got, untouched)
	}

	c.Stop(context.Background())
	again := w.open(t)
	for id, want := range map[string]time.Time{repoA: used, repoB: untouched} {
		if got := cache.LastUsed(again, id); !got.Equal(want) {
			t.Errorf("after a restart %s was last used %v, want %v", id, got, want)
		}
	}
}
