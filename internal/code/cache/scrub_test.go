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
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/code"
	"github.com/crewlet/crewlet/internal/code/cache"
	"github.com/crewlet/crewlet/internal/code/gitproc"
	"github.com/crewlet/crewlet/internal/code/gitproc/gittest"
	"github.com/crewlet/crewlet/internal/objstore"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/statelog/metrics"
)

// THE SCRUB: every byte a used copy holds held to its checksum behind the
// serving — each .pack against its own trailer, every index file by the
// check's rule, vouched or not — once after a copy a previous process left
// first answers, every ScrubInterval while it is used, and at once at a
// report's word; what fails is installed again in place with one GET a pack,
// or dropped to be written again.

// scrubbedCopy is the measured history a previous process SERVED — the
// maintainer's bitmapped multi-pack-index and commit-graph over its two packs
// — opened by this one on the case's clock: its first round has checked its
// indexes, and its first scrub has found every file whole.
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
// copy: what makes a used copy's scrub due again.
func (sc *scrubbedCopy) used(t *testing.T) {
	t.Helper()
	sc.clk.Add(cache.ScrubInterval)
	sc.w.sync(t, sc.c, sc.h.repo)
}

// repaired waits until the repair round a scrub asked has run and taken its
// findings off the copy's repair mark, and answers a sync after it.
func (sc *scrubbedCopy) repaired(t *testing.T) cache.Synced {
	t.Helper()
	eventually(t, "the scrub's findings repaired", func() bool {
		return cache.Settled(sc.c, sc.h.repo) && len(cache.Findings(sc.c, sc.h.repo)) == 0
	})
	return sc.w.sync(t, sc.c, sc.h.repo)
}

// reportsTotal is every report the cache's observer counted, whatever its
// part and outcome.
func reportsTotal(w *syncWorld) uint64 {
	return counterOf(w.rec, metrics.CodeCacheReports, nil)
}

// A PACK BODY NO GIT NAMES IS FOUND BY THE SCRUB. On a copy the maintainer has
// served from, upload-pack's pack reuse sends a damaged entry of the bitmap's
// preferred pack — the newer pack here (served) — to the client byte for byte
// and exits 0 saying nothing — S1, an entry's stated size one too large, and
// C, eight bytes of the pack's middle — so every clone fails at the client
// while no report reaches the cache. (Which pack the bitmap prefers matters
// to S1 alone: the newer pack's entry under a bitmap preferring the older pack
// has its stated size read and its pack named by git, while C's bytes are
// copied as they are stored either way — measured on the node pin and the
// floor.) Damage written IN PLACE after this process vouched for the pack
// keeps the file's identity, so nothing but the scrub can see it: once the
// copy has been used past its interval, the scrub holds the pack to its
// trailer, and the repair round installs it again over its files — one GET,
// the same incarnation — and the next clone completes. On every git the suite
// runs.
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
			// The derived files over the pack stay — it is the same pack byte
			// for byte (TestAPackInstalledAgainForItsBodyKeepsItsIndexes) —
			// and a maintainer's pass writes whatever else is due.
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
// bitmap whose commit bitmaps are damaged in place (K5) — its identity kept,
// so the vouch this process gave it stands — names an object less for the
// branch a clone asks for: the clone is sent a pack without it and fails at
// the client (`did not receive expected object`), upload-pack exiting 0 with
// no report. The scrub hashes every file vouched or not: it finds the
// bitmap failing its trailer, the repair round drops it, and the maintainer
// writes the index and bitmap again AT ONCE, with the clock never moved past
// the maintainer's own interval; the next clone completes.
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

// heldHash holds the scrub's hash of the first file whose path holds a
// substring, once armed, until released — and counts every hash begun
// meanwhile.
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

// THE SCRUB HOLDS NO LOCK AND IS NO USE. Held part way through one copy's
// hashes, it keeps nothing from that copy: a push's delta sync, an adoption
// and a reader answered at once each complete beside it. Only one scrub runs
// on the node: a second pass started meanwhile hashes nothing until the first
// is let go. The copy made due AT ONCE goes before one only its interval made
// due, whatever their order otherwise. And the scrub's own pin is no use: the
// copy's last use stays where it was, so a copy nobody uses after its scrub is
// not scrubbed again at the next interval.
//
// Mutations: the scrub takes the entry's write lock (the sync waits for it);
// two scrubs at once (the second pass hashes while the first is held); the
// interval's taken first; the scrub's pin counted as a use (the unused copy is
// scrubbed again).
func TestTheScrubHoldsNoLockAndIsNoUse(t *testing.T) {
	t.Parallel()
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
	// The copy whose id sorts LAST is made due at once by a report of a
	// crash: it goes first all the same.
	first := slices.Max(repos)
	then := slices.Min(repos)
	cache.Report(c, gitproc.Ran{Dir: w.dir(t, c, first), Result: gitproc.Result{Crashed: true}})
	if why, _ := cache.ScrubDue(c, first); why != "report" {
		t.Fatalf("after a crash report the copy's scrub is due for %q, want report", why)
	}

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
	// Beside the held scrub: a push's delta sync, an adoption and a reader
	// answered at once.
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
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
	row.Added = w.log.write(first, []code.PackRow{row}, map[string]string{"refs/heads/adopted": tip})
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

	// One scrub on the node: a second pass hashes nothing while the first
	// is held.
	second := make(chan []string, 1)
	go func() { second <- cache.ScrubPass(c) }()
	time.Sleep(100 * time.Millisecond)
	if n := hold.during.Load(); n != 0 {
		t.Errorf("%d hashes began while one scrub was held: two scrubs ran at once", n)
	}
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
	if got := cache.LastUsed(c, first); !got.Equal(used) {
		t.Errorf("the scrub moved the copy's last use from %v to %v: its pin is no use", used, got)
	}
	clk.Add(cache.ScrubInterval)
	if done := cache.ScrubPass(c); len(done) != 0 {
		t.Errorf("copies nobody used after their scrub were scrubbed again: %v", done)
	}
}

// A STORED REPORT MAKES THE SCRUB DUE AT ONCE, AND IT REINSTALLS ONLY WHAT
// FAILED. On a copy before its first maintenance — packs, their indexes and
// reverse indexes, no multi-pack-index — every pack's reverse index with its
// signature overwritten is read around by a clone: upload-pack exits 0 and
// reports the reverse indexes damaged. That report makes the copy's scrub due
// at once; the scrub finds each .rev failing its trailer, and the repair round
// installs each pack again over its files — one GET a damaged pack, the same
// incarnation, never a whole rebuild — while the copy kept answering; the
// next clone reports nothing.
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

// A CRASH MAKES THE SCRUB DUE. A reverse index whose every position is
// overwritten past its pack's objects, damaged in place on a copy before its
// first maintenance, kills every clone's pack-objects with SIGSEGV on every
// git — upload-pack saying only that its child died, a crash that names no
// file. The pool reads it as a crash (gitproc.Result.Crashed), the cache makes
// the copy's scrub due, the scrub finds the reverse index failing its trailer
// and the repair round installs the pack again: the next clone completes.
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
		return at.After(first)
	})
}

// BenchmarkScrub is what one scrub of a copy costs: BenchmarkCheck's
// repository (64 packs of a source repository's history, its maintainer's
// index, bitmap and graph written), the cache restarted — so the copy is one
// a previous process left — its first round run, and then its first scrub,
// which hashes every pack, index and derived file it holds, timed alone.
//
//   - cache=page: the files in the page cache, as on a node that served the
//     copy moments before;
//   - cache=cold: every file of the incarnation dropped from the page cache
//     first (posix_fadvise), so the scrub reads at the volume's throughput;
//   - beside=read: cache=page on a pool of two read slots, one held by the
//     arm throughout, while a probe asks the lane for a slot every
//     millisecond — so the probe and the scrub share ONE slot, the most a
//     request can find on a node whose every other read slot is busy, and
//     its waits are what the scrub costs the requests beside it;
//   - beside=read,cache=cold: the same with the files dropped from the page
//     cache first — a scrub of a copy no request has read lately, which is
//     every scrub but the first, so a piece is read at the volume's
//     throughput while the probe waits behind it.
//
// Each reports scrub-ms, the scrub's own time; scrubbed-MB, what it read;
// s/GB, its seconds per gigabyte of the copy's packs — the figure the
// package doc and the deployment guide carry; and piece-ms, the time one
// whole CheckPieceBytes piece takes at that rate, which is the longest a
// request waits behind it for a slot. beside=read reports the probe's
// wait-p50-ms, wait-p99-ms and wait-max-ms beside them.
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

// probeReads holds one of pool's read slots and asks the lane for another
// every millisecond until stopped, answering how long each ask waited — a
// request on a node whose every other read slot is busy.
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

// wakes is the scrubber's wakes as a case's clock sees them: every interval's
// end it asked to be woken at, and the channel that wakes it then.
type wakes struct {
	mu    sync.Mutex
	asked []time.Time
	chans []chan time.Time
}

// at is the ScrubWakesWith knob.
func (w *wakes) at(next time.Time) <-chan time.Time {
	ch := make(chan time.Time, 1)
	w.mu.Lock()
	defer w.mu.Unlock()
	w.asked, w.chans = append(w.asked, next), append(w.chans, ch)
	return ch
}

// has reports whether the scrubber has asked to be woken at at.
func (w *wakes) has(at time.Time) bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	return slices.ContainsFunc(w.asked, at.Equal)
}

// fire wakes the scrubber from the last wake it asked for at at, and reports
// whether it asked for one.
func (w *wakes) fire(at time.Time) bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	for i := len(w.asked) - 1; i >= 0; i-- {
		if w.asked[i].Equal(at) {
			w.chans[i] <- at
			return true
		}
	}
	return false
}

// A USED COPY'S SCRUB COMES ROUND AGAIN, HOWEVER ITS USES FALL. The interval
// counts only for a copy somebody uses, so it is a use that tells the
// scrubber a copy's interval has begun to count — the FIRST use since its
// bytes were last held to their checksums, wherever in the interval it falls
// and whichever kind of use it is: a reader taking the copy, a reader letting
// it go, or a push's scratch made over it. The scrubber's next pass arms its
// wake for the interval's end, and the scrub runs then with nobody using the
// copy again. Woken only by a use made once the interval had passed, a busy
// repository — used an hour after each scrub, and through the day — was never
// scrubbed again: its later uses were not first ones, and at the scrubber's
// last pass it had not been used since its scrub, so no wake was armed for
// it. Woken only by a reader taking it, a repository read by one long request
// at a time — a clone of a large repository running past the scrub's end, a
// CI loop of them — was never scrubbed again either: the release of the
// reader held across the scrub was its first use after it, so the reader
// taken next was no first use, and nothing woke the scrubber. Both a copy a
// previous process left (its first scrub this process's) and one this
// process built (whose first round begins its interval with no scrub) go
// round three times under each kind of first use, each round taking a reader
// after it that is held across the scrub at the interval's end and is the one
// the next round lets go of or makes its scratch over.
//
// Mutations: wake the scrubber only on a first use past the interval (pin's
// old condition) — no wake is armed after a reader taken inside it; drop the
// release's wake — none after the release; drop the scratch's — none after
// the scratch.
func TestAUsedCopysScrubComesRoundAgain(t *testing.T) {
	t.Parallel()
	// uses is each kind of first use since the copy's last scrub: held is a
	// reader taken before that scrub ended, and held across it.
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
				var wk wakes
				c, repo := tc.copy(t, w, clk, func(o *cache.Options) { cache.ScrubWakesWith(o, wk.at) })
				c.Start(t.Context())
				// A reader taken as the interval begins, at the instant the
				// copy's bytes were held to their checksums: no use since.
				held := w.sync(t, c, repo)
				for round := range 3 {
					at, _, base := cache.Scrubbed(c, repo)
					if base.IsZero() {
						t.Fatalf("round %d: the copy's interval has not begun", round)
					}
					if !clk.Now().Equal(base) {
						t.Fatalf("round %d: the clock reads %v, not the interval's beginning %v", round, clk.Now(), base)
					}
					// The round's first use, an hour into the interval; then a
					// reader taken — no first use, the one before it was — and
					// held across the scrub at the interval's end, for the next
					// round's use.
					clk.Set(base.Add(time.Hour))
					u.use(t, w, c, repo, held)
					clk.Set(base.Add(2 * time.Hour))
					held = w.sync(t, c, repo)
					end := base.Add(cache.ScrubInterval)
					eventually(t, "the scrubber's wake armed for the interval's end", func() bool { return wk.has(end) })
					clk.Set(end)
					wk.fire(end)
					eventually(t, "the scrub at the interval's end", func() bool {
						now, _, _ := cache.Scrubbed(c, repo)
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

// packOrder records the repository of every .pack a scrub begins to hash, in
// order — one entry a pack — and holds the first such hash once armed.
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

// THE NODE'S ONE SCRUB IS GIVEN BY EVIDENCE. Three copies a previous process
// left have had their first rounds, so each has its first scrub due — routine
// work a restart queues for every copy it inherited. The first of them is
// being scrubbed when evidence of damage arrives for the LAST:
//
//   - a REQUEST whose in-place round found nothing (its scrub, Cache.escalate)
//     ends the routine scrub in flight and runs at once, before either copy
//     still queued — that copy's own scrub then due again as it was, so it
//     runs after the request's, and before the copy queued behind it;
//   - a REPORT (a crash) does the same, and its scrub is taken ahead of every
//     first scrub due, however much later it was asked.
//
// A mutex released and taken again by the scrubber's next scrub handed itself
// to a waiting request only a scrub later, since Go wakes a waiter without
// handing it the lock; and the due-at-once scrubs were taken by when each was
// asked, so a failing copy's report waited behind every first scrub a restart
// had queued — hours of them on a node holding a terabyte.
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
			case <-time.After(hangDetector):
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
			case <-time.After(hangDetector):
				t.Fatal("the scrub pass never ended")
			}
			if evidence == "request" {
				// The request's scrub ran beside the pass; the pass ended
				// with its first copy's scrub due again, run now.
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

// A REPAIR THE STORE CANNOT SERVE LEAVES THE COPY SERVING. A scrub finds one
// pack's reverse index damaged in place, and its repair round asks the object
// store for the pack — which it cannot hand over (down, or no longer holding
// the bundle). A reinstall over files the copy holds is a REPAIR, never the
// round's precondition: the round leaves that pack as it stands and goes on —
// it converges, a push landing meanwhile is installed and served — and the
// finding waits on the repair mark under a backoff from RebuildCooldown,
// during which no round asks the store for that pack again and every round
// stays a delta. Every crash of a clone over the damage meanwhile is answered
// by the finding waiting (`covered`), not by another scrub of the whole copy.
// Once the backoff has passed and the store answers, the next round installs
// the pack and the finding goes.
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
	w.objects.fail = func(k objstore.Key) error {
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

	// Served meanwhile: a push lands and is installed; the held-off pack is
	// not asked for again, and no report scrubs the copy again.
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

	// The backoff passed and the store back: the next round installs it.
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
// repair's step 3 asks the copy's scrub (Cache.escalate), and a scrub whose
// findings still wait for their repair — here one pack's reverse index, whose
// reinstall the store refused — answers with those findings rather than read
// the copy again. What it found WHOLE answers beside them, less the packs the
// findings name: the rebuild a round's caller asks past the findings' repair
// (step 4) links those rather than downloads them (seedRebuild), so one
// damaged pack among thousands is one GET there too.
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
	w.objects.fail = func(k objstore.Key) error {
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

// A PACK A CHECK FOUND DAMAGED IS LINKED BY NO REBUILD. A repair's step 3 asks
// the copy's scrub, and a scrub whose verdict stands — here one that found
// nothing within the hour, nothing written since — answers unread, with the
// packs it found whole for the rebuild past it to link (seedRebuild). But a
// round's check may have found a pack's index damaged since — damage that
// landed in place after the scrub read it — and left it on the repair mark,
// its round having failed at its install before it could put the pack in
// again (refusePush). That finding answers beside the verdict, its repair the
// answer, and its pack is no pack the rebuild links: linked, the rebuild
// carried the damaged index into the copy that replaced the damaged one.
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

// A FINDING FOUND AGAIN STAYS AS IT WAS FOUND. A scrub finds one pack's
// reverse index damaged in place, and the object store will not hand that
// pack over, so the finding waits on the repair mark under its backoff
// (TestARepairTheStoreCannotServeLeavesTheCopyServing). A request failing on
// the damage meanwhile takes its forced round, whose check holds every index
// file to its trailer and finds that reverse index failing again at the same
// identity: the same damage, which the mark already names. It stays the
// SCRUB's finding — and a scrub's finding still waiting is what answers a
// request's scrub in that scrub's place and covers a report within its
// cooldown (scrubWaits). Taken for the check's, it answered neither: every
// crash of a clone over the damage within the hour asked for another scrub of
// the whole copy, and every request's step 3 read the whole copy again —
// about 1.13 times its pack bytes, some 24 GB for the largest repository the
// defaults admit — once an hour for as long as the store refused the pack.
//
// The forced round fails at its install, on a push whose bundle the store
// refuses as well, so no scrub of the request's runs after it: a crash after
// it is `covered`, and the next request — within the hour of that forced
// round, so it takes no forced round of its own (forcedLately) — is answered
// at its step 3 by the scrub's finding, hashing nothing. Once the store
// answers and the backoff has passed, a sync installs the pack again as the
// scrub found it.
//
// Mutation: a check's finding replaces whatever the mark holds for its file
// (recordCheck) — the crash makes a scrub due, the next request's step 3
// scrubs the whole copy, and the reinstall is logged as that scrub found it
// (trigger=request).
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
		w.objects.fail = func(k objstore.Key) error {
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

// derivedIdentities is the identity of the copy's multi-pack-index, its
// bitmap and its commit-graph, by name.
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

// A PACK INSTALLED AGAIN FOR ITS BODY KEEPS THE INDEXES BUILT OVER IT. The
// multi-pack-index and its bitmap record each object's offset as the pack's
// index placed it, and the commit-graph was written reading commits through
// that index; a pack fetched again whose .pack alone failed is the same pack
// byte for byte — its name is its checksum, its bundle held to its row — so
// every offset and every commit they record holds over the new files, and they
// stay: no index repair, no rewrite of the bitmap over the whole history (12
// to 14 s a gigabyte up to a gigabyte, more past it), and no clone without a
// bitmap meanwhile — or for good, where the node's memory no longer holds the
// bitmap's charge. A pack whose .idx failed is another matter: what was built
// from that index may be wrong, so they go and are written again at once.
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
// first scrub of a copy a previous process left is for its PACKS — bytes no
// process of this run has held to anything — since the round before it held
// every index file to its trailer minutes earlier; read again, those are about
// a fifth of what the first scrub reads, for a verdict minutes old, on every
// copy a restart inherited. A later scrub reads every file, vouched or not:
// damage that lands after a vouch is what it is for.
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
