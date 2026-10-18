package cache_test

import (
	"errors"
	"fmt"
	"io/fs"
	"math/rand/v2"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/code"
	"github.com/crewlet/crewlet/internal/code/cache"
	"github.com/crewlet/crewlet/internal/code/codefault"
	"github.com/crewlet/crewlet/internal/statelog"
)

// THE BEHAVIOURAL HALF of TestNothingButSyncWritesACache: after any
// interleaving of what a node's cache meets, every cache equals a cache
// rebuilt from nothing at the same position — HEAD, config and packed-refs
// byte for byte, the same pack files and nothing outside the image (imageOf)
// — and no file under repos/ holds the pack of a push no row names.
//
// Each seed draws interleavingSteps operations by weight over two
// repositories, each with a pushed commit to start from, and checks every
// repository every checkEvery steps and at the end: a linearizable sync, and
// its image against a fresh cache's. The operations are what a git node
// meets, at the cache's own seams:
//
//   - a push, a tag (annotated: a pack of its own; lightweight: a ref), a
//     ref deleted — rows written to the log, as another node's committer
//     would, and nothing else;
//   - a REFUSED push: received into a scratch over a linearizable sync, its
//     bundle uploaded to the store, and no row ever naming it — the committer
//     refused it, or the node died before its record landed;
//   - an adoption: a push received into a scratch, its row written, and the
//     pack linked in (Adopt) — the receiving node's own pack;
//   - a sync at stale, at the session floor of the repository's last write,
//     and at linearizable; and one KILLED at sync.download, sync.refs or
//     sync.state;
//   - a restart that is a CRASH: the process ends with a maintenance run's
//     lock and temporary files left in one repository's incarnation, as git
//     leaves them when it is killed (maintain.go), and the next boot opens the
//     same directory;
//   - an eviction of every repository (the budget at one byte for one
//     janitor pass), a rebuild, a maintenance pass with the clock past the
//     debounce, tombstones swept from the log, and the log recreated holding
//     an earlier state's rows;
//   - DAMAGE A PREVIOUS PROCESS LEFT: a byte of an index file — a pack's .idx
//     or .rev, a multi-pack-index, its bitmap, the commit-graph — flipped in
//     place, its identity kept, and the same directory booted again; and a
//     file replaced under another identity while the check hashes it
//     (interleavings_check_test.go).
//
// Every check also holds each incarnation's index files to their trailers:
// whatever damage the run left, the round before the check repaired.
//
// A failure names the seed, the step and every operation drawn before it.
//
// SIXTEEN SEEDS, because four did not reach what this half claims: at four,
// the log was never recreated with rows it no longer held (at a weight of one
// in a hundred it was drawn once, at a first step with nothing earlier to go
// back to), and no seed drew the crash that left a maintenance run's lock in
// a repository whose first round after it indexed its packs — the
// interleaving that showed the sweep moving the lock only after the round's
// index had been refused over it. A round indexes from
// interleavingRoundPacks packs here, not MidxRoundPacks, which two
// repositories over forty steps never reach; seeds 6, 12, 14 and 16 draw
// that crash and that round, and putting the round's index back before the
// sweep turns the outcome — and the run — red. The maintainer is due again
// from interleavingMidxPacks packs beside its index rather than MidxPacks,
// at whose production count no seed maintains a repository over its own
// index and bitmap. interleavingOutcomes holds the seeds to reaching every
// outcome; sixteen reach them all, in about nine seconds under the race
// detector, the seeds in parallel. Forty steps a seed, a check every ten:
// enough history for a recreation to go back to and short enough that a
// failure's trace reads in one screen.
const (
	interleavingSeeds = 16
	interleavingSteps = 40
	checkEvery        = 10
)

// interleavingOps are the operations an interleaving draws, by weight.
var interleavingOps = []struct {
	name   string
	weight int
	run    func(in *interleaving, repo string) string
}{
	{"push", 30, (*interleaving).push},
	{"tag", 8, (*interleaving).tag},
	{"delete a ref", 8, (*interleaving).deleteRef},
	{"refused push", 5, (*interleaving).refusedPush},
	{"adopt", 8, (*interleaving).adopt},
	{"sync", 15, (*interleaving).sync},
	{"killed sync", 8, (*interleaving).killedSync},
	{"restart", 4, (*interleaving).restart},
	{"evict", 4, (*interleaving).evict},
	{"rebuild", 3, (*interleaving).rebuild},
	{"maintain", 4, (*interleaving).maintain},
	{"sweep tombstones", 2, (*interleaving).sweepTombstones},
	{"recreate backwards", 1, (*interleaving).recreateBackwards},
	{"damage and restart", 4, (*interleaving).damageAndRestart},
	{"replace while hashed", 3, (*interleaving).replaceWhileHashed},
}

// interleavingOutcomes is every outcome the seeds must reach between them —
// an operation that did something: a sync killed at each of its points, an
// eviction that evicted, a rebuild that was not refused, a log recreated
// with rows it no longer holds. An outcome no seed reaches is an operation
// this test only claims to interleave, so the seeds are held to all of them.
var interleavingOutcomes = []string{
	"push", "tag annotated", "tag lightweight", "delete a ref", "refused push", "adopt",
	"sync stale", "sync session", "sync linearizable",
	"sync killed at sync.download", "sync killed at sync.refs", "sync killed at sync.state",
	"crash leaving a maintenance run's files", "evict", "rebuild", "maintain", "maintain over its own index",
	"sweep tombstones", "recreate backwards", "round index", "round index over a crash's lock",
	"a first round's check found damage", "a pack reinstalled in place", "a derived file dropped by its check",
	"a verdict withheld",
}

// errKilled is what a killed sync's fault point answers.
var errKilled = errors.New("the sync was killed here")

// interleaving is one seed's run.
type interleaving struct {
	t      *testing.T
	w      *syncWorld
	rng    *rand.Rand
	clk    *clock
	faults *codefault.Script
	c      *cache.Cache
	repos  []string
	tags   int
	// last is each repository's last write: a session read's floor.
	last map[string]statelog.Position
	// refused is the checksum of every refused push's pack.
	refused []string
	// exercised is every outcome the run's operations reached
	// (interleavingOutcomes).
	exercised map[string]bool
	// history is the log's rows and the work trees' tips after each step,
	// the setup's first: what a recreation goes back to.
	history []interleaved
	trace   []string

	// indexMu guards indexed, every incarnation a full round wrote its own
	// index in — told by the cache's round, on the cache's goroutines.
	indexMu sync.Mutex
	indexed map[string]bool
	// crashDir is the incarnation a crash left a maintenance run's files
	// in, until a round of the next process has swept them.
	crashDir string

	// swapMu guards swap, the file a replaceWhileHashed renames over while
	// the check hashes it — told by the cache's check, on its goroutines.
	swapMu sync.Mutex
	swap   string
}

// interleaved is the log and the repositories' work trees at one moment.
type interleaved struct {
	log   fakeState
	tips  map[string][]string
	heads map[string]string
}

// interleave runs one seed and answers the outcomes its operations reached.
func interleave(t *testing.T, seed uint64) map[string]bool {
	w := newSyncWorld(t, onMemory)
	in := &interleaving{
		t: t, w: w, rng: rand.New(rand.NewPCG(seed, 1)), clk: newClock(), faults: &codefault.Script{},
		last: map[string]statelog.Position{}, exercised: map[string]bool{}, indexed: map[string]bool{},
	}
	// Logged whatever the verdict: shown with a failure, and under -v the
	// record of what a passing seed exercised.
	t.Cleanup(func() { t.Logf("seed %d drew:\n\t%s", seed, strings.Join(in.trace, "\n\t")) })
	for n := 1; n <= 2; n++ {
		repo := w.repo(t, n)
		_, row := w.push(t, repo, "refs/heads/main", nil)
		in.repos = append(in.repos, repo)
		in.last[repo] = row.Added
	}
	in.c = w.open(t, in.options)
	in.remember()

	total := 0
	for _, o := range interleavingOps {
		total += o.weight
	}
	for step := 1; step <= interleavingSteps; step++ {
		draw := in.rng.IntN(total)
		i := 0
		for draw >= interleavingOps[i].weight {
			draw -= interleavingOps[i].weight
			i++
		}
		o := interleavingOps[i]
		repo := in.repos[in.rng.IntN(len(in.repos))]
		in.trace = append(in.trace, fmt.Sprintf("%d: %s, repo-%d", step, o.name, slices.Index(in.repos, repo)+1))
		in.trace[len(in.trace)-1] += ": " + o.run(in, repo)
		in.noteRoundIndexes()
		in.noteChecks()
		in.remember()
		if step%checkEvery == 0 || step == interleavingSteps {
			in.check(step)
		}
	}
	return in.exercised
}

// saw records an outcome the run reached.
func (in *interleaving) saw(outcome string) { in.exercised[outcome] = true }

// interleavingRoundPacks is the packs a full round leaves before it writes
// its own index in an interleaving, in place of MidxRoundPacks: two
// repositories over forty steps hold a dozen packs or so between them, and
// at the production count no round would ever index — nor meet a dead
// maintenance run's lock in the way of its index.
const interleavingRoundPacks = 4

// interleavingMidxPacks is the packs gathered beside the maintainer's index
// that make a repository due again in an interleaving, in place of
// MidxPacks: at the production count no repository of a dozen packs is ever
// maintained over its own index and bitmap, only over none or over a round's.
const interleavingMidxPacks = 2

// options runs the cache on the interleaving's clock and fault script, its
// rounds indexing from interleavingRoundPacks packs and telling the run
// where they did, and the maintainer due again at interleavingMidxPacks.
func (in *interleaving) options(o *cache.Options) {
	o.Now, o.Faults = in.clk.Now, in.faults
	cache.RoundPacksAt(interleavingRoundPacks)(o)
	cache.MidxPacksAt(interleavingMidxPacks)(o)
	cache.OnRoundIndex(func(dir string) {
		in.indexMu.Lock()
		defer in.indexMu.Unlock()
		in.indexed[dir] = true
	})(o)
	cache.CheckPiecesOf(o, cache.CheckPieceBytes, in.hashed)
}

// noteRoundIndexes records the outcomes the rounds' own indexes reached: any
// round's, and the first round after a crash writing its index in the
// incarnation the crash left a maintenance run's lock in — the lock its sweep
// must move before git will write the index over it.
func (in *interleaving) noteRoundIndexes() {
	in.indexMu.Lock()
	defer in.indexMu.Unlock()
	if len(in.indexed) > 0 {
		in.saw("round index")
	}
	if in.crashDir == "" {
		return
	}
	lock := filepath.Join(in.crashDir, "objects", "pack", "multi-pack-index.lock")
	if _, err := os.Lstat(lock); err == nil {
		return // no round has swept the crash's files yet
	}
	if in.indexed[in.crashDir] {
		in.saw("round index over a crash's lock")
	}
	in.crashDir = ""
}

// remember records the log and the work trees as they stand.
func (in *interleaving) remember() {
	at := interleaved{log: in.w.log.state(), tips: map[string][]string{}, heads: map[string]string{}}
	for _, repo := range in.repos {
		f := in.w.repos[repo]
		at.tips[repo], at.heads[repo] = slices.Clone(f.tips), f.head
	}
	in.history = append(in.history, at)
}

// fatalf fails the run at the operation it is in.
func (in *interleaving) fatalf(format string, args ...any) {
	in.t.Helper()
	at := "the setup"
	if len(in.trace) > 0 {
		at = in.trace[len(in.trace)-1]
	}
	in.t.Fatalf("at %s: %s", at, fmt.Sprintf(format, args...))
}

// syncAt is a sync that must succeed, held until the caller releases it.
func (in *interleaving) syncAt(repo string, fresh statelog.Freshness) cache.Synced {
	in.t.Helper()
	s, err := in.c.Sync(in.t.Context(), repo, fresh)
	if err != nil {
		in.fatalf("a %s sync of %s: %v", fresh.Level, repo, err)
	}
	return s
}

// branch is a branch a push moves.
func (in *interleaving) branch() string {
	return []string{"refs/heads/main", "refs/heads/feature", "refs/heads/topic"}[in.rng.IntN(3)]
}

func (in *interleaving) push(repo string) string {
	ref := in.branch()
	_, row := in.w.push(in.t, repo, ref, nil)
	in.last[repo] = row.Added
	in.saw("push")
	return ref
}

func (in *interleaving) tag(repo string) string {
	annotated := in.rng.IntN(2) == 0
	in.tags++
	name := fmt.Sprintf("t%d", in.tags)
	in.w.tag(in.t, repo, name, annotated)
	in.last[repo] = in.w.log.head()
	kind := "lightweight"
	if annotated {
		kind = "annotated"
	}
	in.saw("tag " + kind)
	return kind + " " + name
}

func (in *interleaving) deleteRef(repo string) string {
	refs := in.w.log.liveRefs(repo)
	if len(refs) == 0 {
		return "no ref to delete"
	}
	ref := refs[in.rng.IntN(len(refs))]
	in.last[repo] = in.w.log.write(repo, nil, map[string]string{ref: ""})
	in.saw("delete a ref")
	return ref
}

// refusedPush receives a push into a scratch and uploads its bundle, and no
// row ever names it; the work tree goes back to where it was, so later pushes
// carry the objects the refused one would have.
func (in *interleaving) refusedPush(repo string) string {
	t := in.t
	f := in.w.repos[repo]
	tips, head := slices.Clone(f.tips), f.head
	s := in.syncAt(repo, linearizable)
	defer s.Release()
	pushID := uuid.Must(uuid.NewV7()).String()
	sc, err := in.c.NewScratch(t.Context(), s, pushID)
	if err != nil {
		in.fatalf("NewScratch: %v", err)
	}
	defer sc.Remove()
	_, cs := in.w.packInto(t, repo, sc.Dir, nil)
	in.w.storeBundle(t, in.w.bundle(cs), pushID)
	in.refused = append(in.refused, cs)
	f.tips, f.head = tips, head
	in.w.client.Run(t, f.work, "reset", "-q", "--hard", head)
	in.saw("refused push")
	return "pack-" + cs
}

// adopt is a push this node received: into a scratch, its row written, and
// its pack linked into the cache.
func (in *interleaving) adopt(repo string) string {
	t := in.t
	s := in.syncAt(repo, linearizable)
	defer s.Release()
	pushID := uuid.Must(uuid.NewV7()).String()
	sc, err := in.c.NewScratch(t.Context(), s, pushID)
	if err != nil {
		in.fatalf("NewScratch: %v", err)
	}
	defer sc.Remove()
	tip, cs := in.w.packInto(t, repo, sc.Dir, nil)
	row := in.w.storeBundle(t, in.w.bundle(cs), pushID)
	ref := in.branch()
	row.Added = in.w.log.write(repo, []code.PackRow{row}, map[string]string{ref: tip})
	in.last[repo] = row.Added
	if err := in.c.Adopt(t.Context(), repo, sc, row.Ref()); err != nil {
		in.fatalf("Adopt: %v", err)
	}
	in.saw("adopt")
	return ref + " pack-" + cs
}

func (in *interleaving) sync(repo string) string {
	fresh := []statelog.Freshness{
		stale,
		{Level: statelog.ReadSession, MinPosition: in.last[repo]},
		linearizable,
	}[in.rng.IntN(3)]
	s := in.syncAt(repo, fresh)
	defer s.Release()
	in.saw(fmt.Sprintf("sync %s", fresh.Level))
	return fmt.Sprintf("%s, %s", fresh.Level, s.Kind)
}

// killedSync is a linearizable sync whose round fails at one of its fault
// points — when the round reaches it: a round with nothing to install, to
// rename or to record does not.
func (in *interleaving) killedSync(repo string) string {
	point := []codefault.Point{codefault.SyncDownload, codefault.SyncRefs, codefault.SyncState}[in.rng.IntN(3)]
	in.faults.Arm(point, codefault.Action{Once: true, Err: errKilled})
	defer in.faults.Disarm(point)
	s, err := in.c.Sync(in.t.Context(), repo, linearizable)
	switch {
	case err == nil:
		s.Release()
		return fmt.Sprintf("at %s: not reached", point)
	case errors.Is(err, errKilled):
		in.saw(fmt.Sprintf("sync killed at %s", point))
		return fmt.Sprintf("at %s: killed", point)
	}
	in.fatalf("a sync killed at %s failed otherwise: %v", point, err)
	return ""
}

// restart is a crash: the cache stops with a maintenance run's lock and
// temporary files in one repository's incarnation — what git leaves when it
// is killed — and the same directory boots again. Stop stands in for the
// process's end: what a crash leaves in tmp/ and scratch/ instead, the next
// boot moves into trash/ whichever way the process ended, and what a killed
// git leaves in an incarnation is what is written here.
func (in *interleaving) restart(string) string {
	held := cache.Repos(in.c)
	var victims []string
	for _, repo := range in.repos {
		if _, ok := held[repo]; ok {
			victims = append(victims, repo)
		}
	}
	detail := "nothing held"
	var dir string
	if len(victims) > 0 {
		victim := victims[in.rng.IntN(len(victims))]
		dir = in.w.dir(in.t, in.c, victim)
		detail = fmt.Sprintf("a maintenance run killed in repo-%d", slices.Index(in.repos, victim)+1)
	}
	in.c.Stop(in.t.Context())
	if dir != "" {
		write(in.t, filepath.Join(dir, "objects", "pack", "multi-pack-index.lock"), "half an index")
		write(in.t, filepath.Join(dir, "objects", "pack", "tmp_bitmap_Xq3vRk"), "half a bitmap")
		write(in.t, filepath.Join(dir, "objects", "info", "commit-graph.lock"), "half a graph")
		in.saw("crash leaving a maintenance run's files")
		in.indexMu.Lock()
		in.crashDir = dir
		delete(in.indexed, dir)
		in.indexMu.Unlock()
	}
	in.c = in.w.open(in.t, in.options)
	return detail
}

func (in *interleaving) evict(string) string {
	before := len(cache.Repos(in.c))
	cache.SetMaxBytes(in.c, 1)
	cache.JanitorPass(in.c)
	cache.SetMaxBytes(in.c, 1<<30)
	evicted := before - len(cache.Repos(in.c))
	if evicted > 0 {
		in.saw("evict")
	}
	return fmt.Sprintf("%d of %d repositories evicted", evicted, before)
}

func (in *interleaving) rebuild(repo string) string {
	s := in.syncAt(repo, linearizable)
	defer s.Release()
	reason := []cache.RebuildReason{cache.RebuildCorrupt, cache.RebuildMissing}[in.rng.IntN(2)]
	r, err := in.c.Rebuild(in.t.Context(), s, reason, 0)
	switch {
	case err == nil:
		r.Release()
		in.saw("rebuild")
		return fmt.Sprintf("%s: incarnation %d", reason, cache.Incarnation(r))
	case errors.Is(err, cache.ErrRebuildRefused):
		return fmt.Sprintf("%s: refused", reason)
	}
	in.fatalf("Rebuild: %v", err)
	return ""
}

// maintain runs a maintainer pass with the clock past the interval. A run in a
// repository that already holds the maintainer's bitmap is the one
// MidxPacks makes due again: it writes over its own index and bitmap.
func (in *interleaving) maintain(string) string {
	in.clk.Add(cache.MidxMinInterval + cache.MidxDebounce)
	bitmapped := map[string]bool{}
	for repo := range cache.Repos(in.c) {
		for _, name := range names(in.t, filepath.Join(in.w.dir(in.t, in.c, repo), "objects", "pack")) {
			if filepath.Ext(name) == ".bitmap" {
				bitmapped[repo] = true
			}
		}
	}
	ran := cache.MaintainerPass(in.c)
	if len(ran) > 0 {
		in.saw("maintain")
	}
	for _, repo := range ran {
		if bitmapped[repo] {
			in.saw("maintain over its own index")
		}
	}
	return fmt.Sprintf("maintained %d", len(ran))
}

func (in *interleaving) sweepTombstones(repo string) string {
	in.w.log.sweep(repo)
	in.saw("sweep tombstones")
	return "swept"
}

// recreateBackwards recreates the log holding the rows it held after an
// earlier step, and puts the work trees back to where they were then.
func (in *interleaving) recreateBackwards(string) string {
	at := 0
	if len(in.history) > 1 {
		// Strictly earlier than the rows it holds now.
		at = in.rng.IntN(len(in.history) - 1)
	}
	h := in.history[at]
	in.w.log.recreate(h.log)
	for _, repo := range in.repos {
		f := in.w.repos[repo]
		f.tips, f.head = slices.Clone(h.tips[repo]), h.heads[repo]
		in.w.client.Run(in.t, f.work, "reset", "-q", "--hard", f.head)
		in.last[repo] = in.w.log.head()
	}
	if at < len(in.history)-1 {
		in.saw("recreate backwards")
	}
	return fmt.Sprintf("to the rows after step %d", at)
}

// check holds every repository's cache to a fresh cache's at the log's
// position now, and repos/ to holding no refused push's pack.
func (in *interleaving) check(step int) {
	t := in.t
	for _, repo := range in.repos {
		s := in.syncAt(repo, linearizable)
		got := imageOf(t, s.Dir)
		in.checkSound(step, repo, s.Dir)
		s.Release()
		if want := in.w.rebuilt(t, repo); got != want {
			t.Fatalf("after step %d, repo-%d's cache:\n%s\nwant a fresh cache's at the same position:\n%s",
				step, slices.Index(in.repos, repo)+1, got, want)
		}
	}
	err := filepath.WalkDir(in.w.path("repos"), func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		for _, cs := range in.refused {
			if strings.Contains(d.Name(), cs) {
				t.Errorf("after step %d, %s holds a refused push's pack", step, path)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("reading repos/: %v", err)
	}
	if t.Failed() {
		t.FailNow()
	}
}

// head is the log's checkpoint.
func (f *fakeLog) head() statelog.Position {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.at()
}

// liveRefs is repo's refs that are not tombstones, sorted.
func (f *fakeLog) liveRefs(repo string) []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	for name, ref := range f.repos[repo].refs {
		if ref.oid != "" {
			out = append(out, name)
		}
	}
	slices.Sort(out)
	return out
}
