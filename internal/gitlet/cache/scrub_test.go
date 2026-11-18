package cache_test

import (
	"context"
	"errors"
	"maps"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/gitlet"
	"github.com/crewlet/crewlet/internal/gitlet/cache"
	"github.com/crewlet/crewlet/internal/gitlet/gitproc"
	"github.com/crewlet/crewlet/internal/gitlet/gitproc/gittest"
	"github.com/crewlet/crewlet/internal/objstore"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/statelog/metrics"
)

// THE SCRUB holds every byte of a used copy to its checksum behind the
// serving, vouched or not: once after a copy a previous process left first
// answers, every ScrubInterval while it is used, and at once on a report.
// What fails is installed again in place (one GET a pack) or dropped to be
// written again.

// scrubbedCopy is the measured history a previous process SERVED (bitmapped
// multi-pack-index and commit-graph over two packs), opened by this one on the
// case's clock, its first round and first scrub done and sound.
type scrubbedCopy struct {
	w   *syncWorld
	clk *clock
	h   history
	c   *cache.Cache
	dir string
}

// newScrubbedCopy is a scrubbedCopy over a cache opened with each of opts.
func newScrubbedCopy(t *testing.T, opts ...func(*cache.Options)) *scrubbedCopy {
	t.Helper()
	w := newSyncWorld(t, onMemory)
	clk := newClock()
	h := w.measured(t)
	w.served(t, h, clk)
	c := w.open(t, append([]func(*cache.Options){clk.on}, opts...)...)
	sc := &scrubbedCopy{w: w, clk: clk, h: h, c: c, dir: w.sync(t, c, h.repo).Dir}
	if why, _ := cache.ScrubDue(c, h.repo); why != "first" {
		t.Fatalf("after the first round of a copy a previous process left, its scrub is due for %q, want first", why)
	}
	if done := cache.ScrubPass(c); !slices.Equal(done, []string{h.repo}) {
		t.Fatalf("the first scrub pass scrubbed %v, want %s", done, h.repo)
	}
	if at, found, _ := cache.Scrubbed(c, h.repo); at.IsZero() || found {
		t.Fatalf("the first scrub of a sound copy ended at %v having found anything %t", at, found)
	}
	if got := counterOf(w.rec, metrics.CodeCacheChecks, map[string]string{"file": "pack", "outcome": "sound",
		"trigger": "first"}); got != 2 {
		t.Fatalf("the first scrub held %d packs sound, want both", got)
	}
	return sc
}

// used moves the clock past the scrub's interval and has a reader use the
// copy, making its scrub due again.
func (sc *scrubbedCopy) used(t *testing.T) {
	t.Helper()
	sc.clk.Add(cache.ScrubInterval)
	sc.w.sync(t, sc.c, sc.h.repo)
}

// repaired waits until the scrub's repair round has cleared its findings off
// the repair mark, and answers a sync after it.
func (sc *scrubbedCopy) repaired(t *testing.T) cache.Synced {
	t.Helper()
	eventually(t, "the scrub's findings repaired", func() bool {
		return cache.Settled(sc.c, sc.h.repo) && len(cache.Findings(sc.c, sc.h.repo)) == 0
	})
	return sc.w.sync(t, sc.c, sc.h.repo)
}

// reportsTotal is every report the cache's observer counted.
func reportsTotal(w *syncWorld) uint64 {
	return counterOf(w.rec, metrics.CodeCacheReports, nil)
}

// A PACK BODY NO GIT NAMES IS FOUND BY THE SCRUB. upload-pack's pack reuse
// sends a damaged entry of the bitmap's preferred pack (the newer, here) byte
// for byte and exits 0: S1 (a stated size one too large; it must be the
// preferred pack, or git names it) and C (eight bytes of the middle). Every
// clone fails at the client, no report comes, and damage IN PLACE after the
// vouch keeps the file's identity: only the scrub, once the copy is used past
// its interval, finds it, and the repair reinstalls it (one GET, same
// incarnation) so the next clone completes.
//
// Mutations: the scrub skips .pack files, or honours vouches — it finds
// nothing and the clone keeps failing; no interval (a first scrub only) —
// nothing is due once the interval has passed.
func TestTheScrubFindsAPackBodyNoGitNames(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name   string
		damage gittest.DamageCase
	}{
		{"S1 an entry's stated size", gittest.EntrySize},
		{"C eight bytes of a pack's middle", gittest.PackFlipped},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			sc := newScrubbedCopy(t)
			h, w := sc.h, sc.w
			gittest.DamageInPlace(t, sc.dir, tc.damage, h.commits[3])
			reported := reportsTotal(w)
			if lacks := cloneLacks(t, w, sc.c, sc.dir, h.commits[3]); lacks == "" {
				t.Fatal("a clone over the damaged pack completed: the case damaged nothing a clone sends")
			}
			if got := reportsTotal(w) - reported; got != 0 {
				t.Fatalf("the clone over a damaged pack body reached the cache as %d reports: the case is "+
					"of damage no git names", got)
			}
			if done := cache.ScrubPass(sc.c); len(done) != 0 {
				t.Fatalf("a scrub of %v before the interval passed", done)
			}

			sc.used(t)
			gets, before := w.objects.total(), cache.Repos(sc.c)[h.repo]
			if done := cache.ScrubPass(sc.c); !slices.Equal(done, []string{h.repo}) {
				t.Fatalf("once the copy was used past its interval the scrub pass scrubbed %v, want %s", done,
					h.repo)
			}
			if got := counterOf(w.rec, metrics.CodeCacheChecks, map[string]string{"file": "pack",
				"outcome": "damaged", "trigger": "scrub"}); got != 1 {
				t.Errorf("checks{file=pack,outcome=damaged,trigger=scrub} = %d, want the damaged pack", got)
			}
			s := sc.repaired(t)
			if got := w.objects.total() - gets; got != 1 {
				t.Errorf("the repair downloaded %d packs, want the one damaged", got)
			}
			if n := cache.Incarnation(s); n != before {
				t.Errorf("the repair answered incarnation %d, want %d in place", n, before)
			}
			if got := counter(w.rec, metrics.CodeCacheRebuilds, "reason", string(cache.RebuildStored)); got != 1 {
				t.Errorf("rebuilds{reason=stored} = %d, want 1", got)
			}
			lines := w.logs.lines(t, "code_cache_pack_reinstalled")
			if len(lines) != 1 || lines[0]["found"] != "trailer" || lines[0]["trigger"] != "scrub" {
				t.Errorf("code_cache_pack_reinstalled = %v, want the pack found by the scrub's trailer", lines)
			}
			// The derived files stay over the byte-identical pack
			// (TestAPackInstalledAgainForItsBodyKeepsItsIndexes); the
			// maintainer's pass writes whatever else is due.
			cache.MaintainerPass(sc.c)
			if lacks := cloneLacks(t, w, sc.c, s.Dir, h.commits[3]); lacks != "" {
				t.Errorf("after the repair a clone %s", lacks)
			}
			for _, bad := range wrong(t, w, sc.c, s.Dir, h) {
				t.Errorf("after the repair: %s", bad)
			}
		})
	}
}

// AN INDEX FILE DAMAGED AFTER IT WAS VOUCHED FOR IS FOUND BY THE SCRUB. A
// bitmap damaged in place (K5) keeps its identity and so its vouch, and
// omits an object a clone needs: the clone fails at the client (`did not
// receive expected object`), upload-pack exits 0, nothing is reported. The scrub hashes vouched files too, the
// repair drops the bitmap, and the maintainer writes it again AT ONCE (the
// clock never past its own interval); the next clone completes.
//
// Mutation: the scrub honours vouches — the bitmap is skipped and every clone
// keeps failing.
func TestTheScrubFindsAnIndexDamagedAfterItWasVouched(t *testing.T) {
	t.Parallel()
	sc := newScrubbedCopy(t)
	h, w := sc.h, sc.w
	if len(cache.Vouched(sc.c, h.repo)) == 0 {
		t.Fatal("the copy's files are not vouched for: the case would not test what a vouch hides")
	}
	gittest.DamageInPlace(t, sc.dir, gittest.BitmapEntries)
	reported := reportsTotal(w)
	if lacks := cloneLacks(t, w, sc.c, sc.dir, h.commits[3]); lacks == "" {
		t.Fatal("a clone over the damaged bitmap completed: the case damaged nothing a clone reads")
	}
	if got := reportsTotal(w) - reported; got != 0 {
		t.Fatalf("the clone over a damaged bitmap reached the cache as %d reports: the case is of damage no "+
			"git names", got)
	}

	sc.used(t)
	if done := cache.ScrubPass(sc.c); !slices.Equal(done, []string{h.repo}) {
		t.Fatalf("the scrub pass scrubbed %v, want %s", done, h.repo)
	}
	if got := counterOf(w.rec, metrics.CodeCacheChecks, map[string]string{"file": "bitmap", "outcome": "damaged",
		"trigger": "scrub"}); got != 1 {
		t.Errorf("checks{file=bitmap,outcome=damaged,trigger=scrub} = %d, want the damaged bitmap", got)
	}
	s := sc.repaired(t)
	lines := w.logs.lines(t, "code_cache_derived_repaired")
	if !slices.ContainsFunc(lines, func(l map[string]any) bool { return l["trigger"] == "scrub" }) {
		t.Errorf("code_cache_derived_repaired = %v, want the drop asked by the scrub", lines)
	}
	if !cache.IndexRepair(sc.c, h.repo) {
		t.Error("the index dropped for damage is not due at once")
	}
	if ran := cache.MaintainerPass(sc.c); len(ran) != 1 {
		t.Fatalf("the maintainer ran %v after the drop, want %s at once", ran, h.repo)
	}
	if packs, bitmap, _ := cache.Indexed(sc.c, h.repo); packs != 2 || !bitmap {
		t.Errorf("after the repair the index covers %d packs, bitmap %t; want both packs and a bitmap", packs, bitmap)
	}
	if lacks := cloneLacks(t, w, sc.c, s.Dir, h.commits[3]); lacks != "" {
		t.Errorf("after the repair a clone %s", lacks)
	}
}

// heldHash, once armed, holds the scrub's hash of the first file whose path
// holds a substring until released, counting every hash begun meanwhile.
type heldHash struct {
	armed   atomic.Value // the substring, or ""
	reached chan string
	release chan struct{}
	during  atomic.Int64
	holding atomic.Bool
}

func newHeldHash() *heldHash {
	h := &heldHash{reached: make(chan string, 1), release: make(chan struct{})}
	h.armed.Store("")
	return h
}

// at is the WhileHashing knob.
func (h *heldHash) at(path string) {
	if h.holding.Load() {
		h.during.Add(1)
	}
	if sub := h.armed.Load().(string); sub != "" && strings.Contains(path, sub) {
		h.armed.Store("")
		h.holding.Store(true)
		h.reached <- path
		<-h.release
		h.holding.Store(false)
	}
}

// THE SCRUB HOLDS NO LOCK AND IS NO USE. Held mid-copy, it blocks no sync,
// adoption or reader of that copy. A second pass hashes nothing until the
// first is let go: read after synctest.Wait, then past every timer
// (pastEveryTimer) to catch one waiting a grace (nothing else runs then). A
// copy due AT ONCE goes before one due by interval. The scrub's pin and
// release are no use, so a copy unused since is not scrubbed at the next
// interval; the clock moves a second before each could stamp one.
//
// Mutations: the scrub takes the write lock; two scrubs at once, or after a
// minute's grace; the interval's copy first; the pin or the release counted
// as a use, at once or a minute on the bubble's clock after (last uses read
// past where they were).
func TestTheScrubHoldsNoLockAndIsNoUse(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		w := newSyncWorld(t, onMemory)
		clk := newClock()
		hold := newHeldHash()
		repos := []string{w.repo(t, 1), w.repo(t, 2)}
		for _, repo := range repos {
			w.push(t, repo, "refs/heads/main", nil)
		}
		c := w.open(t, clk.on, func(o *cache.Options) { cache.WhileHashing(o, hold.at) })
		for _, repo := range repos {
			w.sync(t, c, repo)
			if why, _ := cache.ScrubDue(c, repo); why != "" {
				t.Fatalf("a copy this process built has its scrub due for %q, want only its interval", why)
			}
		}
		if done := cache.ScrubPass(c); len(done) != 0 {
			t.Fatalf("copies this process built were scrubbed at once: %v", done)
		}
		clk.Add(cache.ScrubInterval)
		for _, repo := range repos {
			w.sync(t, c, repo)
		}
		// The copy whose id sorts LAST is made due at once by a crash: it
		// goes first all the same.
		first := slices.Max(repos)
		then := slices.Min(repos)
		cache.Report(c, gitproc.Ran{Dir: w.dir(t, c, first), Result: gitproc.Result{Crashed: true}})
		if why, _ := cache.ScrubDue(c, first); why != "report" {
			t.Fatalf("after a crash report the copy's scrub is due for %q, want report", why)
		}

		// Last uses, read before the clock moves and the first pass pins.
		firstUsed, thenUsed := cache.LastUsed(c, first), cache.LastUsed(c, then)
		clk.Add(time.Second)

		hold.armed.Store(filepath.Base(w.dir(t, c, first)))
		order := make(chan []string, 1)
		go func() { order <- cache.ScrubPass(c) }()
		var path string
		select {
		case path = <-hold.reached:
		case <-time.After(hangDetector):
			t.Fatal("the scrub never hashed the copy made due at once")
		}
		if !strings.Contains(path, filepath.Base(w.dir(t, c, first))) {
			t.Fatalf("the scrub hashed %s first, want the copy made due at once", path)
		}
		if got := cache.LastUsed(c, first); !got.Equal(firstUsed) {
			t.Errorf("the scrub's pin moved the copy's last use from %v to %v: its pin is no use", firstUsed, got)
		}
		ctx, cancel := context.WithTimeout(t.Context(), hangDetector)
		defer cancel()
		w.push(t, first, "refs/heads/main", nil)
		s, err := c.Sync(ctx, first, statelog.Freshness{Level: statelog.ReadLinearizable})
		if err != nil {
			t.Fatalf("a delta sync beside the scrub: %v", err)
		}
		pushID := uuid.Must(uuid.NewV7()).String()
		scratch, err := c.NewScratch(ctx, s, pushID)
		if err != nil {
			t.Fatalf("NewScratch beside the scrub: %v", err)
		}
		tip, cs := w.packInto(t, first, scratch.Dir, nil)
		row := w.storeBundle(t, w.bundle(cs), pushID)
		row.Added = w.log.write(first, []gitlet.PackRow{row}, map[string]string{"refs/heads/adopted": tip})
		if err := c.Adopt(ctx, first, scratch, row.Ref()); err != nil {
			t.Errorf("an adoption beside the scrub: %v", err)
		}
		scratch.Remove()
		s.Release()
		at, err := c.Sync(ctx, first, statelog.Freshness{Level: statelog.ReadStale})
		if err != nil {
			t.Fatalf("a reader at once beside the scrub: %v", err)
		}
		at.Release()
		used := cache.LastUsed(c, first)
		// Any use the scrub stamps from here on reads past this one.
		clk.Add(time.Second)
		cancel() // the steps beside the scrub are done

		second := make(chan []string, 1)
		go func() { second <- cache.ScrubPass(c) }()
		oneScrub := func(when string) {
			t.Helper()
			if n := hold.during.Load(); n != 0 {
				t.Errorf("%d hashes began while one scrub was held, %s: two scrubs ran at once", n, when)
			}
		}
		synctest.Wait()
		oneScrub("once every goroutine had blocked")
		time.Sleep(pastEveryTimer)
		synctest.Wait()
		oneScrub("once the clock had then moved past every timer the cache arms")
		close(hold.release)
		var passes [][]string
		for _, ch := range []chan []string{order, second} {
			select {
			case done := <-ch:
				passes = append(passes, done)
			case <-time.After(hangDetector):
				t.Fatal("a scrub pass never ended")
			}
		}
		var all []string
		for _, p := range passes {
			all = append(all, p...)
		}
		if !slices.Equal(all, []string{first, then}) {
			t.Errorf("the passes scrubbed %v, want the copy made due at once (%s) before the interval's (%s)",
				passes, first, then)
		}
		noUse := func(when string) {
			t.Helper()
			if got := cache.LastUsed(c, first); !got.Equal(used) {
				t.Errorf("the scrub moved the held copy's last use from %v to %v, %s: the scrub is no use",
					used, got, when)
			}
			if got := cache.LastUsed(c, then); !got.Equal(thenUsed) {
				t.Errorf("the scrub moved the interval's copy's last use from %v to %v, %s: the scrub is no use",
					thenUsed, got, when)
			}
		}
		noUse("once both passes had ended")
		time.Sleep(pastEveryTimer)
		synctest.Wait()
		noUse("once the clock had then moved past every timer the cache arms")
		clk.Add(cache.ScrubInterval)
		if done := cache.ScrubPass(c); len(done) != 0 {
			t.Errorf("copies nobody used after their scrub were scrubbed again: %v", done)
		}
	})
}

// A STORED REPORT MAKES THE SCRUB DUE AT ONCE, AND IT REINSTALLS ONLY WHAT
// FAILED. Before the first maintenance (no multi-pack-index), a clone reads
// around reverse indexes with overwritten signatures, exits 0 and reports
// them. The report makes the scrub due at once; the repair installs each
// failing pack again in place (one GET each, same incarnation, no whole
// rebuild) while the copy answers, and the next clone reports nothing.
//
// Mutation: a whole rebuild in place of the reinstall (a new incarnation).
func TestAStoredReportReinstallsThePackWithOneGet(t *testing.T) {
	t.Parallel()
	w := newSyncWorld(t, onMemory)
	h := w.measured(t)
	c := w.open(t)
	s := w.sync(t, c, h.repo)
	gittest.DamageInPlace(t, s.Dir, gittest.RevSignature)

	reported := reports(w, "pack", "queued")
	if lacks := cloneLacks(t, w, c, s.Dir, h.commits[3]); lacks != "" {
		t.Fatalf("a clone over damaged reverse indexes %s: the case wants damage git reads around", lacks)
	}
	if got := reports(w, "pack", "queued") - reported; got == 0 {
		t.Fatal("the clone's report of the reverse indexes did not reach the cache as a pack report")
	}
	if why, _ := cache.ScrubDue(c, h.repo); why != "report" {
		t.Fatalf("after the report the copy's scrub is due for %q, want report", why)
	}
	gets := w.objects.total()
	before := cache.Incarnation(s)
	if done := cache.ScrubPass(c); !slices.Equal(done, []string{h.repo}) {
		t.Fatalf("the scrub pass scrubbed %v, want %s", done, h.repo)
	}
	eventually(t, "the scrub's findings repaired", func() bool {
		return cache.Settled(c, h.repo) && len(cache.Findings(c, h.repo)) == 0
	})
	after := w.sync(t, c, h.repo)
	if got := w.objects.total() - gets; got != 2 {
		t.Errorf("the repair downloaded %d packs, want the two whose reverse index failed", got)
	}
	if n := cache.Incarnation(after); n != before {
		t.Errorf("the repair answered incarnation %d, want %d in place", n, before)
	}
	if got := counter(w.rec, metrics.CodeCacheRebuilds, "reason", string(cache.RebuildStored)); got != 1 {
		t.Errorf("rebuilds{reason=stored} = %d, want the one round that reinstalled", got)
	}
	reported = reportsTotal(w)
	if lacks := cloneLacks(t, w, c, after.Dir, h.commits[3]); lacks != "" {
		t.Errorf("after the repair a clone %s", lacks)
	}
	if got := reportsTotal(w) - reported; got != 0 {
		t.Errorf("after the repair a clone still reported %d damages", got)
	}
}

// A REPORT-DRIVEN SCRUB WAITS ITS COOLDOWN, WHATEVER IS WRITTEN MEANWHILE. A
// report of a pack makes the copy's scrub due; the scrub finds nothing. A
// second report within ScrubCooldown — a push landing between the two — makes
// nothing due (`cooldown`); one past it does.
//
// Mutations: no cooldown (two scrubs within the hour); a write resets the
// cooldown (the push lets the second report through).
func TestAReportDrivenScrubWaitsItsCooldown(t *testing.T) {
	t.Parallel()
	w := newSyncWorld(t, onMemory)
	clk := newClock()
	repo := w.repo(t, 1)
	w.push(t, repo, "refs/heads/main", nil)
	c := w.open(t, clk.on)
	w.sync(t, c, repo)
	report := func() {
		cache.Report(c, gitproc.Ran{Dir: w.dir(t, c, repo), Result: gitproc.Result{
			Damage: &gitproc.Damage{Kind: gitproc.ErrCorrupt, Parts: gitproc.PartPack}}})
	}

	report()
	if done := cache.ScrubPass(c); !slices.Equal(done, []string{repo}) {
		t.Fatalf("the first report's scrub pass scrubbed %v, want %s", done, repo)
	}
	if _, found, _ := cache.Scrubbed(c, repo); found {
		t.Fatal("the scrub of a sound copy found something")
	}
	clk.Add(time.Minute)
	w.push(t, repo, "refs/heads/main", nil)
	w.sync(t, c, repo)
	report()
	if got := reports(w, "pack", "cooldown"); got != 1 {
		t.Errorf("reports{part=pack,outcome=cooldown} = %d, want the report within the cooldown", got)
	}
	if done := cache.ScrubPass(c); len(done) != 0 {
		t.Errorf("a report within the cooldown of a scrub that found nothing scrubbed %v again", done)
	}
	clk.Add(cache.ScrubCooldown)
	report()
	if done := cache.ScrubPass(c); !slices.Equal(done, []string{repo}) {
		t.Errorf("a report past the cooldown scrubbed %v, want %s", done, repo)
	}
}

// A CRASH MAKES THE SCRUB DUE. A reverse index overwritten past its pack's
// objects (before the first maintenance) kills every clone's pack-objects with
// SIGSEGV, naming no file. The pool reads it as gitproc.Result.Crashed, the
// cache makes the scrub due, the scrub finds the .rev failing its trailer and
// the repair reinstalls the pack: the next clone completes.
//
// Mutation: the observer ignores Crashed — nothing is due, and every clone
// keeps failing.
func TestACrashMakesTheScrubDue(t *testing.T) {
	t.Parallel()
	w := newSyncWorld(t, onMemory)
	h := w.measured(t)
	c := w.open(t)
	s := w.sync(t, c, h.repo)
	gittest.DamageInPlace(t, s.Dir, gittest.RevOverwritten, h.commits[3])
	if lacks := cloneLacks(t, w, c, s.Dir, h.commits[3]); lacks == "" {
		t.Fatal("a clone over the overwritten reverse index completed: the case damaged nothing a clone reads")
	}
	if got := reports(w, "crash", "queued"); got == 0 {
		t.Fatal("the clone's crash did not reach the cache as a crash report")
	}
	if done := cache.ScrubPass(c); !slices.Equal(done, []string{h.repo}) {
		t.Fatalf("the crash's scrub pass scrubbed %v, want %s", done, h.repo)
	}
	eventually(t, "the scrub's findings repaired", func() bool {
		return cache.Settled(c, h.repo) && len(cache.Findings(c, h.repo)) == 0
	})
	after := w.sync(t, c, h.repo)
	if lacks := cloneLacks(t, w, c, after.Dir, h.commits[3]); lacks != "" {
		t.Errorf("after the repair a clone %s", lacks)
	}
}

// THE SCRUBBER RUNS WHAT FALLS DUE. A started cache's scrubber is woken by a
// report and scrubs the copy with nobody calling it, and is woken again by the
// first use of a copy past its interval.
//
// Mutations: Start starts no scrubber; a use past the interval wakes nothing
// (the second scrub waits for a timer the case's clock never moves).
func TestTheScrubberRunsWhatFallsDue(t *testing.T) {
	t.Parallel()
	w := newSyncWorld(t, onMemory)
	clk := newClock()
	repo := w.repo(t, 1)
	w.push(t, repo, "refs/heads/main", nil)
	c := w.open(t, clk.on)
	w.sync(t, c, repo)
	c.Start(t.Context())
	cache.Report(c, gitproc.Ran{Dir: w.dir(t, c, repo), Result: gitproc.Result{Crashed: true}})
	eventually(t, "the started cache's scrub of a crash", func() bool {
		at, _, _ := cache.Scrubbed(c, repo)
		return !at.IsZero()
	})
	first, _, _ := cache.Scrubbed(c, repo)
	clk.Add(cache.ScrubInterval)
	var once sync.Once
	eventually(t, "the started cache's scrub of a copy used past its interval", func() bool {
		once.Do(func() { w.sync(t, c, repo) })
		at, _, _ := cache.Scrubbed(c, repo)
		//fixedwait:stated both scrubbed instants are the cache's c.now() on the case's clock (clk.on)
		return at.After(first)
	})
}

// BenchmarkScrub times the first scrub of a copy a previous process left:
// BenchmarkCheck's 64-pack repository, maintained, the cache restarted.
// Modes: cache=page (files in the page cache); cache=cold (dropped first, via
// posix_fadvise); beside=read (two read slots, one held, a probe asking every
// millisecond, so it shares ONE slot with the scrub as on a busy node); and
// beside=read,cache=cold (the usual later scrub, of a copy nobody read lately).
//
// Reports scrub-ms; scrubbed-MB; s/GB of packs, the figure the package doc
// and the deployment guide carry; piece-ms, one CheckPieceBytes piece at that
// rate, the longest a request waits behind it; and, beside=read, the probe's
// wait-p50-ms, wait-p99-ms and wait-max-ms.
func BenchmarkScrub(b *testing.B) {
	quiet(b)
	for _, arm := range []struct {
		name  string
		bytes int64
	}{{"100MB", 100_000_000}, {"1GB", 1_000_000_000}, {"5GB", 5_000_000_000}} {
		b.Run("size="+arm.name, func(b *testing.B) {
			if arm.bytes > 1_000_000_000 {
				gittest.Large(b, "a 5 GB history fast-imported, packed, stored and synced")
			}
			w := newSyncWorld(b, onMemory)
			w.processes = runtime.NumCPU()
			repo, src, _, _, total, _ := w.midxRepo(b, arm.bytes)
			if err := os.RemoveAll(src); err != nil {
				b.Fatal(err)
			}
			clk := newClock()
			quietly := func(o *cache.Options) { o.MaxBytes, o.Logger = 1<<40, nil }
			c := w.open(b, clk.on, quietly)
			w.sync(b, c, repo)
			clk.Add(cache.MidxDebounce)
			if ran := cache.MaintainerPass(c); len(ran) != 1 {
				b.Fatalf("the maintainer ran %v, want the repository", ran)
			}
			dir := w.dir(b, c, repo)
			scrubbed, pieces, err := cache.ScrubbedFiles(dir)
			if err != nil {
				b.Fatal(err)
			}
			for _, mode := range []string{"cache=page", "cache=cold", "beside=read", "beside=read,cache=cold"} {
				beside, cold := strings.HasPrefix(mode, "beside=read"), strings.HasSuffix(mode, "cache=cold")
				b.Run(mode, func(b *testing.B) {
					var (
						took  time.Duration
						waits []time.Duration
					)
					for b.Loop() {
						b.StopTimer()
						if beside {
							w.processes = 2
						} else {
							w.processes = runtime.NumCPU()
						}
						c = w.reopen(b, c, clk.on, quietly)
						w.sync(b, c, repo)
						if why, _ := cache.ScrubDue(c, repo); why != "first" {
							b.Fatalf("the restarted copy's scrub is due for %q, want its first", why)
						}
						if cold && !dropCached(b, dir) {
							b.Skip("no way to drop a file's cached pages on this platform")
						}
						var stop func() []time.Duration
						if beside {
							stop = probeReads(b, w.pool)
						}
						at := time.Now()
						b.StartTimer()
						done := cache.ScrubPass(c)
						b.StopTimer()
						//fixedwait:measure the scrub's duration, the benchmark's reported figure, held to no bound
						took += time.Since(at)
						if stop != nil {
							waits = append(waits, stop()...)
						}
						if len(done) != 1 {
							b.Fatalf("the scrub pass scrubbed %v, want the repository", done)
						}
						b.StartTimer()
					}
					ms := float64(took.Microseconds()) / 1e3 / float64(b.N)
					b.ReportMetric(ms, "scrub-ms")
					b.ReportMetric(float64(scrubbed)/1e6, "scrubbed-MB")
					b.ReportMetric(ms/1e3/(float64(total)/1e9), "s/GB")
					b.ReportMetric(ms*float64(cache.CheckPieceBytes)/float64(scrubbed), "piece-ms")
					b.ReportMetric(float64(pieces), "pieces")
					if len(waits) > 0 {
						slices.Sort(waits)
						at := func(q float64) float64 {
							return float64(waits[int(q*float64(len(waits)-1))].Microseconds()) / 1e3
						}
						b.ReportMetric(at(0.50), "wait-p50-ms")
						b.ReportMetric(at(0.99), "wait-p99-ms")
						b.ReportMetric(at(1), "wait-max-ms")
					}
				})
			}
		})
	}
}

// probeReads holds one of pool's read slots and asks for another every
// millisecond until stopped, answering how long each ask waited.
func probeReads(b *testing.B, pool *gitproc.Pool) (stop func() []time.Duration) {
	b.Helper()
	hold, err := pool.HoldRead(b.Context())
	if err != nil {
		b.Fatalf("holding a read slot: %v", err)
	}
	ctx, cancel := context.WithCancel(b.Context())
	var waits []time.Duration
	done := make(chan struct{})
	go func() {
		defer close(done)
		for ctx.Err() == nil {
			at := time.Now()
			release, err := pool.HoldRead(ctx)
			//fixedwait:measure how long a probe waited, which the benchmark reports and holds to no bound
			waited := time.Since(at)
			switch {
			case err == nil:
				release()
				waits = append(waits, waited)
			case errors.Is(err, gitproc.ErrBusy):
				waits = append(waits, waited)
			}
			select {
			case <-ctx.Done():
			//fixedwait:simulated a request on the node every millisecond, beside the scrub
			case <-time.After(time.Millisecond):
			}
		}
	}()
	return func() []time.Duration {
		cancel()
		<-done
		hold()
		return waits
	}
}

// wakes records every time the scrubber asked to be woken at, each a timer on
// the case's clock (clock.At) the case fires by moving the clock, and holds
// the next arming when asked (holdNext).
type wakes struct {
	clk   *clock
	mu    sync.Mutex
	asked []time.Time
	hold  *wakeHold
}

// wakeHold is an arming held at its start: held closes once it is, release
// lets it go on.
type wakeHold struct{ held, release chan struct{} }

// at is the ScrubWakesWith knob.
func (w *wakes) at(next time.Time) <-chan time.Time {
	w.mu.Lock()
	hold := w.hold
	w.hold = nil
	w.mu.Unlock()
	if hold != nil {
		close(hold.held)
		<-hold.release
	}
	w.mu.Lock()
	w.asked = append(w.asked, next)
	w.mu.Unlock()
	return w.clk.At(next)
}

// holdNext holds the scrubber's next arming after its pass has read the
// clock, answering a channel closed once held and the release; cleanup
// releases too, so a failing case leaves no scrubber waiting.
func (w *wakes) holdNext(t *testing.T) (held <-chan struct{}, release func()) {
	hold := &wakeHold{held: make(chan struct{}), release: make(chan struct{})}
	w.mu.Lock()
	w.hold = hold
	w.mu.Unlock()
	release = sync.OnceFunc(func() { close(hold.release) })
	t.Cleanup(release)
	return hold.held, release
}

// has reports whether the scrubber has asked to be woken at at.
func (w *wakes) has(at time.Time) bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	return slices.ContainsFunc(w.asked, at.Equal)
}

// A USED COPY'S SCRUB COMES ROUND AGAIN, HOWEVER ITS USES FALL. The interval
// counts only for a used copy: the FIRST use since its last scrub, anywhere
// in the interval and of any kind (reader taken, reader released, push's
// scratch), wakes the scrubber to arm a wake at the interval's end, and the
// scrub runs then with no further use. Inherited and built copies each go
// three rounds per kind. In round two a kick re-arms from a clock read before
// it moved: a wake armed for a time already come fires at once, as a timer
// does.
//
// Mutations: wake only on a first use past the interval, or drop the
// release's or the scratch's wake — no wake armed after that use; a wake for
// a time already come waits for the clock to move — round two never fires.
func TestAUsedCopysScrubComesRoundAgain(t *testing.T) {
	t.Parallel()
	// uses is each kind of first use since the copy's last scrub; held is a
	// reader held across that scrub.
	uses := []struct {
		name string
		use  func(t *testing.T, w *syncWorld, c *cache.Cache, repo string, held cache.Synced)
	}{
		{"a reader taken inside it", func(t *testing.T, w *syncWorld, c *cache.Cache, repo string,
			_ cache.Synced) {
			w.sync(t, c, repo)
		}},
		{"the release of a reader held across its beginning", func(_ *testing.T, _ *syncWorld, _ *cache.Cache,
			_ string, held cache.Synced) {
			held.Release()
		}},
		{"a scratch made inside it over a reader held across its beginning", func(t *testing.T, _ *syncWorld,
			c *cache.Cache, repo string, held cache.Synced) {
			sc, err := c.NewScratch(t.Context(), held, uuid.Must(uuid.NewV7()).String())
			if err != nil {
				t.Fatalf("a scratch over %s: %v", repo, err)
			}
			t.Cleanup(sc.Remove)
		}},
	}
	for _, tc := range []struct {
		name string
		copy func(t *testing.T, w *syncWorld, clk *clock, opts ...func(*cache.Options)) (*cache.Cache, string)
	}{
		{"left by a previous process", func(t *testing.T, w *syncWorld, clk *clock,
			opts ...func(*cache.Options)) (*cache.Cache, string) {
			h := w.measured(t)
			w.served(t, h, clk)
			c := w.open(t, append([]func(*cache.Options){clk.on}, opts...)...)
			w.sync(t, c, h.repo)
			if done := cache.ScrubPass(c); !slices.Equal(done, []string{h.repo}) {
				t.Fatalf("the first scrub pass scrubbed %v, want %s", done, h.repo)
			}
			return c, h.repo
		}},
		{"built by this one", func(t *testing.T, w *syncWorld, clk *clock,
			opts ...func(*cache.Options)) (*cache.Cache, string) {
			repo := w.repo(t, 1)
			w.push(t, repo, "refs/heads/main", nil)
			c := w.open(t, append([]func(*cache.Options){clk.on}, opts...)...)
			w.sync(t, c, repo)
			return c, repo
		}},
	} {
		for _, u := range uses {
			t.Run(tc.name+"/"+u.name, func(t *testing.T) {
				t.Parallel()
				w := newSyncWorld(t, onMemory)
				clk := newClock()
				wk := &wakes{clk: clk}
				c, repo := tc.copy(t, w, clk, func(o *cache.Options) { cache.ScrubWakesWith(o, wk.at) })
				c.Start(t.Context())
				// A reader taken at the interval's very beginning: no use since.
				held := w.sync(t, c, repo)
				for round := range 3 {
					at, _, base := cache.Scrubbed(c, repo)
					if base.IsZero() {
						t.Fatalf("round %d: the copy's interval has not begun", round)
					}
					if !clk.Now().Equal(base) {
						t.Fatalf("round %d: the clock reads %v, not the interval's beginning %v", round, clk.Now(), base)
					}
					// The round's first use an hour in; then a reader (no first
					// use) held across the scrub for the next round's use.
					clk.Set(base.Add(time.Hour))
					u.use(t, w, c, repo, held)
					clk.Set(base.Add(2 * time.Hour))
					held = w.sync(t, c, repo)
					end := base.Add(cache.ScrubInterval)
					eventually(t, "the scrubber's wake armed for the interval's end", func() bool { return wk.has(end) })
					// Moving the clock to the interval's end fires whichever
					// wake the scrubber waits on (a pending kick may have
					// re-armed it), and at once one armed from a clock read
					// before the move. Round two forces that: a kick whose
					// pass is held after reading the clock until it moves.
					var release func()
					if round == 1 {
						var held <-chan struct{}
						held, release = wk.holdNext(t)
						cache.KickScrubber(c)
						select {
						case <-held:
						case <-time.After(hangDetector): //fixedwait:liveness
							t.Fatalf("round %d: the scrubber, kicked, armed no wake within %v", round, hangDetector)
						}
					}
					clk.Set(end)
					if release != nil {
						release()
					}
					eventually(t, "the scrub at the interval's end", func() bool {
						now, _, _ := cache.Scrubbed(c, repo)
						//fixedwait:stated both scrubbed instants are the cache's c.now() on the case's clock (clk.on)
						return now.After(at) || (at.IsZero() && !now.IsZero())
					})
				}
			})
		}
	}
}

// repoOfPath is the repository whose incarnation the path lies in
// (repos/<id>.<n>.git/...), or "".
func repoOfPath(path string) string {
	for _, part := range strings.Split(filepath.ToSlash(path), "/") {
		if id, _, ok := strings.Cut(part, "."); ok && strings.HasSuffix(part, ".git") {
			return id
		}
	}
	return ""
}

// packOrder records, in order, the repository of every .pack a scrub begins to
// hash, and holds the first such hash once armed.
type packOrder struct {
	mu    sync.Mutex
	order []string
	hold  *heldHash
}

// at is the WhileHashing knob.
func (p *packOrder) at(path string) {
	if strings.HasSuffix(path, ".pack") {
		p.mu.Lock()
		p.order = append(p.order, repoOfPath(path))
		p.mu.Unlock()
	}
	p.hold.at(path)
}

func (p *packOrder) seen() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return slices.Clone(p.order)
}

// THE NODE'S ONE SCRUB IS GIVEN BY EVIDENCE. Three inherited copies have
// their first scrub due; the first is being scrubbed when evidence arrives
// for the LAST. A REQUEST's scrub (Cache.escalate, its in-place round having
// found nothing) preempts the routine one in flight and runs at once, the
// preempted copy due again before the one queued behind it; a REPORT's scrub
// goes ahead of every first scrub due, however late it was asked. A plain mutex would not do: Go wakes a waiter without
// handing it the lock, so the scrubber's next scrub would take it first.
//
// Mutations: a request's scrub that waits for the scrub in flight (no
// preemption) — the held copy's pack is hashed to its end first; due-at-once
// scrubs ordered by when they were asked — the report's scrub goes last.
func TestTheNodesScrubIsGivenByEvidence(t *testing.T) {
	t.Parallel()
	for _, evidence := range []string{"request", "report"} {
		t.Run(evidence, func(t *testing.T) {
			t.Parallel()
			w := newSyncWorld(t, onMemory)
			repos := []string{w.repo(t, 1), w.repo(t, 2), w.repo(t, 3)}
			for _, repo := range repos {
				w.push(t, repo, "refs/heads/main", nil)
			}
			c := w.open(t)
			for _, repo := range repos {
				w.sync(t, c, repo).Release()
			}
			order := &packOrder{hold: newHeldHash()}
			c = w.reopen(t, c, func(o *cache.Options) { cache.WhileHashing(o, order.at) })
			for _, repo := range repos {
				w.sync(t, c, repo).Release()
				if why, _ := cache.ScrubDue(c, repo); why != "first" {
					t.Fatalf("an inherited copy's scrub is due for %q after its first round, want first", why)
				}
			}
			slices.Sort(repos)
			first, middle, last := repos[0], repos[1], repos[2]

			order.hold.armed.Store(".pack")
			passed := make(chan []string, 1)
			go func() { passed <- cache.ScrubPass(c) }()
			select {
			case path := <-order.hold.reached:
				if repoOfPath(path) != first {
					t.Fatalf("the pass began with %s, want the first copy queued (%s)", path, first)
				}
			case <-time.After(hangDetector): //fixedwait:liveness
				t.Fatal("the pass never began a scrub")
			}

			asked := make(chan error, 1)
			switch evidence {
			case "request":
				s, ok := cache.Pin(c, last)
				if !ok {
					t.Fatal("the last copy is not held")
				}
				defer s.Release()
				go func() {
					r, err := c.Rebuild(context.Background(), s,
						cache.Failure{Reason: cache.RebuildMissing, Began: time.Now()})
					if err == nil {
						r.Release()
					}
					asked <- err
				}()
				eventually(t, "the request's scrub waiting for the node's one scrub", func() bool {
					return cache.ScrubAsking(c) == 1
				})
			case "report":
				cache.Report(c, gitproc.Ran{Dir: w.dir(t, c, last), Result: gitproc.Result{Crashed: true}})
				close(asked)
			}
			close(order.hold.release)
			if err := <-asked; err != nil {
				t.Fatalf("the %s: %v", evidence, err)
			}
			select {
			case <-passed:
			case <-time.After(hangDetector): //fixedwait:liveness
				t.Fatal("the scrub pass never ended")
			}
			if evidence == "request" {
				// The pass ended with its preempted copy due again.
				cache.ScrubPass(c)
			}
			want := []string{first, last, first, middle}
			if got := order.seen(); !slices.Equal(got, want) {
				t.Errorf("the scrubs hashed packs of %v, want %v: the %s's scrub preempting the routine one "+
					"in flight and going before every first scrub due", got, want, evidence)
			}
			for _, repo := range []string{first, middle} {
				if at, _, _ := cache.Scrubbed(c, repo); at.IsZero() {
					t.Errorf("%s's first scrub, preempted or queued, never ran", repo)
				}
			}
		})
	}
}

// A REPAIR THE STORE CANNOT SERVE LEAVES THE COPY SERVING. A scrub finds a
// damaged reverse index and the store cannot hand over its pack. A reinstall
// is a REPAIR, never the round's precondition: the round converges and serves
// a push meanwhile, and the finding waits on the repair mark under a backoff
// from RebuildCooldown, with no round asking the store again. A crash over the
// damage meanwhile is `covered` by the waiting finding, not scrubbed again.
// Past the backoff, with the store back, the next round installs the pack.
//
// Mutations: fail the round with its reinstall (the push is never served,
// every later round failing); hold off nothing (every round asks the store
// again); scrub again on every report while the finding waits.
func TestARepairTheStoreCannotServeLeavesTheCopyServing(t *testing.T) {
	t.Parallel()
	w := newSyncWorld(t, onMemory)
	clk := newClock()
	h := w.measured(t)
	c := w.open(t, clk.on)
	s := w.sync(t, c, h.repo)
	gittest.DamageInPlace(t, s.Dir, gittest.RevTrailer, h.commits[3])
	s.Release()
	down := errors.New("the object store is down")
	w.objects.mu.Lock()
	w.objects.fail = func(_ context.Context, k objstore.Key) error {
		if k == h.newer.Object {
			return down
		}
		return nil
	}
	w.objects.mu.Unlock()
	installed, _, _ := w.objects.counts()
	opens := func() int {
		got, _, _ := w.objects.counts()
		return got[h.newer.Object] - installed[h.newer.Object]
	}
	crash := func() {
		cache.Report(c, gitproc.Ran{Dir: w.dir(t, c, h.repo), Result: gitproc.Result{Crashed: true}})
	}

	crash()
	if done := cache.ScrubPass(c); len(done) != 1 {
		t.Fatalf("the crash's scrub ran for %v", done)
	}
	eventually(t, "the repair round", func() bool { return cache.Quiet(c, h.repo) && opens() == 1 })
	if found := cache.Findings(c, h.repo); len(found) != 1 {
		t.Fatalf("the scrub's findings are %v once the store refused the pack, want the reverse index kept", found)
	}
	if lines := w.logs.lines(t, "code_cache_pack_reinstall_failed"); len(lines) != 1 {
		t.Errorf("code_cache_pack_reinstall_failed = %v, want the one download the store refused", lines)
	}
	if !cache.Converged(c, h.repo) {
		t.Error("the copy is not converged: the refused reinstall failed its round")
	}

	// Served meanwhile; the held-off pack is not asked for, no report scrubs.
	tip, _ := w.push(t, h.repo, "refs/heads/main", map[string]string{"after.txt": "landed\n"})
	after := w.sync(t, c, h.repo)
	if got, exit := serveGit(t, w, c, after.Dir, "rev-parse", "refs/heads/main"); exit != 0 ||
		strings.TrimSpace(got) != tip {
		t.Errorf("after the refused reinstall main is %q (exit %d), want the push's %s", got, exit, tip)
	}
	after.Release()
	for range 3 {
		clk.Add(time.Minute)
		crash()
		w.sync(t, c, h.repo).Release()
	}
	if done := cache.ScrubPass(c); len(done) != 0 {
		t.Errorf("reports while the finding waited scrubbed %v again", done)
	}
	if got := reports(w, "crash", "covered"); got != 3 {
		t.Errorf("reports{part=crash,outcome=covered} = %d, want the three crashes while the finding waited", got)
	}
	if got := opens(); got != 1 {
		t.Errorf("the store was asked for the held-off pack %d times, want once", got)
	}

	// The backoff passed and the store back.
	w.objects.mu.Lock()
	w.objects.fail = nil
	w.objects.mu.Unlock()
	clk.Add(cache.RebuildCooldown)
	w.sync(t, c, h.repo).Release()
	eventually(t, "the finding repaired", func() bool {
		return cache.Settled(c, h.repo) && len(cache.Findings(c, h.repo)) == 0
	})
	if got := opens(); got != 2 {
		t.Errorf("the store was asked for the pack %d times in all, want twice", got)
	}
	repaired := w.sync(t, c, h.repo)
	defer repaired.Release()
	if bad, err := cache.Unsound(repaired.Dir); err != nil || len(bad) != 0 {
		t.Errorf("after the repair, %v fail their trailers (%v)", bad, err)
	}
}

// A SCRUB ANSWERED FROM ITS FINDINGS NAMES THE PACKS IT FOUND WHOLE. A
// repair's step 3 (Cache.escalate) asks a scrub whose findings still wait
// (here a reinstall the store refused); it answers from them unread, plus
// the packs it found WHOLE less those the findings name, which the step-4
// rebuild links rather than downloads (seedRebuild): one damaged pack among
// thousands stays one GET.
//
// Mutation: answer the pending findings with no packs whole (scrubFor's
// repair.found case) — the rebuild past them downloads every pack the
// repository holds.
func TestAScrubAnsweredFromItsFindingsNamesThePacksItFoundWhole(t *testing.T) {
	t.Parallel()
	w := newSyncWorld(t, onMemory)
	h := w.measured(t)
	c := w.open(t, newClock().on)
	s := w.sync(t, c, h.repo)
	gittest.DamageInPlace(t, s.Dir, gittest.RevTrailer, h.commits[3])
	s.Release()
	w.objects.mu.Lock()
	w.objects.fail = func(_ context.Context, k objstore.Key) error {
		if k == h.newer.Object {
			return errors.New("the object store is down")
		}
		return nil
	}
	w.objects.mu.Unlock()
	cache.Report(c, gitproc.Ran{Dir: w.dir(t, c, h.repo), Result: gitproc.Result{Crashed: true}})
	if done := cache.ScrubPass(c); len(done) != 1 {
		t.Fatalf("the crash's scrub ran for %v", done)
	}
	eventually(t, "the repair round", func() bool { return cache.Quiet(c, h.repo) })
	found := cache.Findings(c, h.repo)
	if len(found) != 1 {
		t.Fatalf("the scrub's findings are %v once the store refused the pack, want the reverse index kept", found)
	}
	checked := counterOf(w.rec, metrics.CodeCacheChecks, map[string]string{"trigger": "request"})

	damaged, whole, err := cache.ScrubFor(c, h.repo)
	if err != nil {
		t.Fatalf("the repair's scrub: %v", err)
	}
	if !slices.Equal(damaged, found) {
		t.Errorf("the repair's scrub answered %v damaged, want the finding still waiting, %v", damaged, found)
	}
	if want := []string{h.older.GitChecksum}; !slices.Equal(whole, want) {
		t.Errorf("the repair's scrub answered %v whole, want %v: every pack the scrub found whole but the "+
			"one its finding names", whole, want)
	}
	if got := counterOf(w.rec, metrics.CodeCacheChecks, map[string]string{"trigger": "request"}); got != checked {
		t.Errorf("the repair's scrub hashed %d files, want none: its findings answer it", got-checked)
	}
}

// A PACK A CHECK FOUND DAMAGED IS LINKED BY NO REBUILD. A scrub verdict that
// stands answers a repair's step 3 unread, with its whole packs for the
// rebuild to link (seedRebuild). A round's check may since have found a pack's
// index damaged in place and left it on the repair mark, its round failing at
// install (refusePush). That finding answers beside the verdict, and its pack
// is not linked: linked, the rebuild would carry the damaged index over.
//
// Mutations: answer the verdict's packs whole as it found them (the damaged
// pack among them); answer the verdict without the check's finding.
func TestAPackACheckFoundDamagedIsLinkedByNoRebuild(t *testing.T) {
	t.Parallel()
	w := newSyncWorld(t, onMemory)
	h := w.measured(t)
	c := w.open(t, newClock().on)
	s := w.sync(t, c, h.repo)
	both := slices.Sorted(slices.Values([]string{h.older.GitChecksum, h.newer.GitChecksum}))
	if damaged, whole, err := cache.ScrubFor(c, h.repo); err != nil || len(damaged) != 0 ||
		!slices.Equal(whole, both) {
		t.Fatalf("the sound copy's scrub answered %v damaged and %v whole (%v), want both packs whole", damaged,
			whole, err)
	}
	gittest.DamageInPlace(t, s.Dir, gittest.IdxTrailer, h.commits[3])
	w.refusePush(t, h)
	if _, err := c.Rebuild(t.Context(), s, cache.Failure{Reason: cache.RebuildMissing,
		Began: time.Now()}); !errors.Is(err, errPushRefused) {
		t.Fatalf("Rebuild: %v, want its forced round failing on the store's refusal of the push", err)
	}
	idx := filepath.Join("objects", "pack", "pack-"+h.newer.GitChecksum+".idx")
	if got := cache.Findings(c, h.repo); !slices.Equal(got, []string{idx}) {
		t.Errorf("the repair mark holds %v once the forced round failed, want its check's finding %s", got, idx)
	}
	checked := counterOf(w.rec, metrics.CodeCacheChecks, map[string]string{"trigger": "request"})

	damaged, whole, err := cache.ScrubFor(c, h.repo)
	if err != nil {
		t.Fatalf("the repair's scrub: %v", err)
	}
	if !slices.Equal(damaged, []string{idx}) {
		t.Errorf("the repair's scrub answered %v damaged, want the check's finding still waiting, %s", damaged, idx)
	}
	if want := []string{h.older.GitChecksum}; !slices.Equal(whole, want) {
		t.Errorf("the repair's scrub answered %v whole, want %v: every pack its verdict found whole but the one "+
			"the check found damaged since", whole, want)
	}
	if got := counterOf(w.rec, metrics.CodeCacheChecks, map[string]string{"trigger": "request"}); got != checked {
		t.Errorf("the repair's scrub hashed %d files, want none: its verdict stands", got-checked)
	}
}

// A FINDING FOUND AGAIN STAYS AS IT WAS FOUND. While a scrub's finding waits
// on a refused pack, a request's forced round finds the same .rev failing at
// the same identity; it stays the SCRUB's finding, which answers a request's
// scrub and covers a report (scrubWaits). Taken for the check's, each crash
// and request would rescan the whole copy (about 1.13x its pack bytes). Here
// a crash after the forced round is `covered`, the next request within the
// hour takes no forced round (forcedLately) and hashes nothing at step 3, and
// past the backoff the pack is reinstalled as the scrub found it.
//
// Mutation: a check's finding replaces the mark's for its file (recordCheck)
// — the crash makes a scrub due, the next request scrubs the whole copy, and
// the reinstall logs trigger=request.
func TestAFindingFoundAgainStaysAsItWasFound(t *testing.T) {
	t.Parallel()
	w := newSyncWorld(t, onMemory)
	clk := newClock()
	h := w.measured(t)
	c := w.open(t, clk.on)
	s := w.sync(t, c, h.repo)
	gittest.DamageInPlace(t, s.Dir, gittest.RevTrailer, h.commits[3])
	down := errors.New("the object store is down")
	refuse := func(objects ...objstore.Key) {
		w.objects.mu.Lock()
		defer w.objects.mu.Unlock()
		w.objects.fail = func(_ context.Context, k objstore.Key) error {
			if slices.Contains(objects, k) {
				return down
			}
			return nil
		}
	}
	refuse(h.newer.Object)
	crash := func() {
		cache.Report(c, gitproc.Ran{Dir: w.dir(t, c, h.repo), Result: gitproc.Result{Crashed: true}})
	}
	repair := func(what string) {
		t.Helper()
		clk.Add(time.Minute)
		if _, err := c.Rebuild(t.Context(), s, cache.Failure{Reason: cache.RebuildCorrupt,
			Began: time.Now()}); !errors.Is(err, down) {
			t.Fatalf("%s: %v, want its round failing on the store's refusal of the push", what, err)
		}
	}
	forced := func() uint64 {
		return counterOf(w.rec, metrics.CodeCacheChecks, map[string]string{"trigger": "forced"})
	}
	scrubbed := func() uint64 {
		return counterOf(w.rec, metrics.CodeCacheChecks, map[string]string{"trigger": "request"})
	}
	rev := filepath.Join("objects", "pack", "pack-"+h.newer.GitChecksum+".rev")

	crash()
	if done := cache.ScrubPass(c); !slices.Equal(done, []string{h.repo}) {
		t.Fatalf("the crash's scrub pass scrubbed %v, want %s", done, h.repo)
	}
	eventually(t, "the repair round the store refused", func() bool { return cache.HeldOff(c, h.repo) })
	if got := cache.Findings(c, h.repo); !slices.Equal(got, []string{rev}) {
		t.Fatalf("the repair mark holds %v once the store refused the pack, want the scrub's finding %s", got, rev)
	}

	_, push := w.push(t, h.repo, "refs/heads/main", nil)
	refuse(h.newer.Object, push.Object)
	repair("the request")
	if got := checks(w, "rev", "damaged", "forced"); got != 1 {
		t.Fatalf("checks{file=rev,outcome=damaged,trigger=forced} = %d, want the request's forced round finding "+
			"the reverse index again", got)
	}
	if got := cache.Findings(c, h.repo); !slices.Equal(got, []string{rev}) {
		t.Errorf("the repair mark holds %v once the forced round failed, want the one finding %s", got, rev)
	}

	covered := reports(w, "crash", "covered")
	clk.Add(time.Minute)
	crash()
	if got := reports(w, "crash", "covered") - covered; got != 1 {
		t.Errorf("reports{part=crash,outcome=covered} grew by %d, want the crash covered by the scrub's finding "+
			"still waiting", got)
	}
	if why, _ := cache.ScrubDue(c, h.repo); why != "" {
		t.Errorf("the crash made the copy's scrub due for %q, want none: the scrub's finding answers it", why)
	}

	before, hashed := forced(), scrubbed()
	repair("the next request")
	if got := forced() - before; got != 0 {
		t.Fatalf("the next request held %d index files to their trailers in a forced round, want none within the "+
			"hour of the last: the case would not reach its step 3", got)
	}
	if got := scrubbed() - hashed; got != 0 {
		t.Errorf("the next request's scrub hashed %d files, want none: the scrub's finding still waiting answers it",
			got)
	}

	refuse()
	clk.Add(cache.RebuildCooldown)
	r := w.sync(t, c, h.repo)
	lines := w.logs.lines(t, "code_cache_pack_reinstalled")
	if len(lines) != 1 || lines[0]["pack_id"] != h.newer.PackID || lines[0]["found"] != "trailer" ||
		lines[0]["trigger"] != "report" {
		t.Errorf("code_cache_pack_reinstalled = %v, want the damaged pack installed again as the scrub found it",
			lines)
	}
	if got := cache.Findings(c, h.repo); len(got) != 0 {
		t.Errorf("the repair mark still holds %v once its pack is installed again", got)
	}
	if bad, err := cache.Unsound(r.Dir); err != nil || len(bad) != 0 {
		t.Errorf("once the store answers, %v fail their trailers (%v)", bad, err)
	}
}

// derivedIdentities is the identity of the copy's multi-pack-index, bitmap
// and commit-graph, by name.
func derivedIdentities(t *testing.T, dir string) map[string][4]int64 {
	t.Helper()
	out := map[string][4]int64{}
	for _, name := range derivedOf(t, dir, gitproc.PartCommitGraph|gitproc.PartMultiPackIndex) {
		path := filepath.Join(dir, "objects", "pack", name)
		if name == "commit-graph" {
			path = filepath.Join(dir, "objects", "info", name)
		}
		out[name] = gittest.FileIdentity(t, path)
	}
	return out
}

// A PACK INSTALLED AGAIN FOR ITS BODY KEEPS THE INDEXES BUILT OVER IT. A pack
// fetched again because its .pack alone failed is byte-identical (named by
// its checksum, its bundle held to its row), so the multi-pack-index, bitmap
// and commit-graph built through its .idx still hold and stay: no bitmap
// rewrite (12-14 s a GB), no clone without a bitmap meanwhile, or for good
// once the node's memory cannot hold its charge. A pack whose .idx
// failed may have misled what was built from it, so those go and are written
// again at once.
//
// Mutation: drop the derived files before any placement over a present pack
// (the .pack arm's bitmap is replaced and an index repair made due).
func TestAPackInstalledAgainForItsBodyKeepsItsIndexes(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name   string
		damage gittest.DamageCase
		ids    func(history) []string
		keeps  bool
	}{
		{"its .pack", gittest.PackFlipped, func(h history) []string { return []string{h.commits[3]} }, true},
		{"its .idx", gittest.IdxOffset, tipBlob, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			sc := newScrubbedCopy(t)
			before := derivedIdentities(t, sc.dir)
			if len(before) != 3 {
				t.Fatalf("the served copy holds %v, want its index, its bitmap and its graph", before)
			}
			gittest.DamageInPlace(t, sc.dir, tc.damage, tc.ids(sc.h)...)
			sc.used(t)
			if done := cache.ScrubPass(sc.c); len(done) != 1 {
				t.Fatalf("the scrub pass scrubbed %v", done)
			}
			s := sc.repaired(t)
			if got := counter(sc.w.rec, metrics.CodeCacheRebuilds, "reason", string(cache.RebuildStored)); got != 1 {
				t.Fatalf("rebuilds{reason=stored} = %d, want the pack installed again", got)
			}
			after := derivedIdentities(t, s.Dir)
			switch {
			case tc.keeps && !maps.Equal(after, before):
				t.Errorf("installing the pack again for its body changed the derived files: %v, were %v", after,
					before)
			case tc.keeps && cache.IndexRepair(sc.c, sc.h.repo):
				t.Error("installing the pack again for its body made an index repair due")
			case !tc.keeps && len(after) != 0:
				t.Errorf("installing the pack again for its index left %v, which may have been built from it", after)
			}
			cache.MaintainerPass(sc.c)
			if lacks := cloneLacks(t, sc.w, sc.c, s.Dir, sc.h.commits[3]); lacks != "" {
				t.Errorf("after the repair a clone %s", lacks)
			}
			for _, bad := range wrong(t, sc.w, sc.c, s.Dir, sc.h) {
				t.Errorf("after the repair: %s", bad)
			}
		})
	}
}

// A COPY'S FIRST SCRUB READS NO INDEX FILE ITS FIRST ROUND JUST CHECKED. The
// first scrub of an inherited copy is for its PACKS: the round before it held
// every index file to its trailer minutes earlier, and rereading them adds
// about a fifth for nothing. A later scrub reads every file, vouched or not:
// damage landing after a vouch is what it is for.
//
// Mutation: the first scrub hashes vouched index files too — their `first`
// counts grow with the scrub.
func TestACopysFirstScrubReadsNoIndexItsFirstRoundChecked(t *testing.T) {
	t.Parallel()
	w := newSyncWorld(t, onMemory)
	clk := newClock()
	h := w.measured(t)
	w.served(t, h, clk)
	c := w.open(t, clk.on)
	w.sync(t, c, h.repo)
	indexes := func(trigger string) uint64 {
		var n uint64
		for _, file := range []string{"idx", "rev", "multi-pack-index", "bitmap", "commit-graph"} {
			n += checks(w, file, "sound", trigger)
		}
		return n
	}
	checked := indexes("first")
	if checked == 0 {
		t.Fatal("the first round checked no index file")
	}
	if done := cache.ScrubPass(c); !slices.Equal(done, []string{h.repo}) {
		t.Fatalf("the first scrub pass scrubbed %v, want %s", done, h.repo)
	}
	if got := indexes("first"); got != checked {
		t.Errorf("the first scrub hashed %d index files the first round had checked, want none", got-checked)
	}
	if got := checks(w, "pack", "sound", "first"); got != 2 {
		t.Errorf("the first scrub held %d packs sound, want both", got)
	}
	clk.Add(cache.ScrubInterval)
	w.sync(t, c, h.repo)
	if done := cache.ScrubPass(c); !slices.Equal(done, []string{h.repo}) {
		t.Fatalf("the interval's scrub pass scrubbed %v, want %s", done, h.repo)
	}
	if got := indexes("scrub"); got != checked {
		t.Errorf("the interval's scrub hashed %d index files, want every one of the %d, vouched or not", got,
			checked)
	}
}
