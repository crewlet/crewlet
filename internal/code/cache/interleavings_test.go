package cache_test

import (
	"errors"
	"fmt"
	"io/fs"
	"math/rand/v2"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/code"
	"github.com/crewlet/crewlet/internal/code/cache"
	"github.com/crewlet/crewlet/internal/code/codefault"
	"github.com/crewlet/crewlet/internal/objstore"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/statelog/metrics"
)

// THE BEHAVIOURAL HALF of TestNothingButSyncWritesACache: after any
// interleaving of what a node's cache meets, every cache equals a cache
// rebuilt from nothing at the same position — HEAD, config and packed-refs
// byte for byte, the same pack files and nothing outside the image (imageOf)
// — its published chain installed to the same layer as that fresh cache's,
// but for a layer it installed before the store lost a part of it (the
// layers' check, interleavings_layers_test.go); no file under repos/ holds
// the pack of a push no row names, and every repository's index of pushes,
// backfilled as far as it goes, says within the coverage it claims exactly
// what the push rows say (indexAgainstLog): the push that introduced each
// commit, and every move of every ref in log order.
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
//   - a COMPACTION — two or three live packs merged into one by git's own
//     merge and a record retiring them — which the copy's next round takes
//     out of it by a swap (swap.go); one an idle, maintained copy misses
//     until its retirements are swept, so its next round takes the packs out
//     as no retirement accounts for, the graph with them; the retirements
//     swept from the rows, so a copy below them reads them as packs no
//     retirement accounts for; a swap killed at its rename, which the copy's
//     next round tries again; and that kill followed by a restart, the next
//     boot reading what was leaving from the state's floor;
//   - DAMAGE A PREVIOUS PROCESS LEFT: a byte of an index file — a pack's .idx
//     or .rev, a multi-pack-index, its bitmap, the commit-graph, or a chained
//     copy's layer files, graph layers, tip and chain files — flipped in
//     place, its identity kept, and the same directory booted again; and a
//     file replaced under another identity while the check hashes it
//     (interleavings_check_test.go);
//   - DAMAGE AFTER THE VOUCH: a byte of any file of a held copy — a pack
//     itself among them — flipped in place while the cache runs; a run's
//     REPORT of a derived file, a pack, an object, or a crash, begun now or
//     before anything this process wrote, a third of them with a check
//     admitted around it; and a SCRUB with the clock past ScrubInterval
//     (interleavings_scrub_test.go);
//   - a PUBLICATION of the first repository's next layer — the second is
//     never published, so every operation meets a copy in own mode too — by
//     THIS node, its maintainer: the production compactor's, decided and
//     applied on the log, its layer and any repack's outputs adopted here, a
//     third of them after a burst of pushes past compact.LayerRepackPacks,
//     whose plan repacks them first; one by ANOTHER git node, a cache of its
//     own over the same log and store, whose layer this node's installer
//     fetches, a third of them from a git newer than this one's and a third
//     with this node's installer meeting a store that fails to answer for
//     the layer's part; a request's sync and a pass of the INSTALLER behind
//     it; a part of a published layer LOST from the store or SPOILED in it;
//     and a push into a chained copy's tail with the maintainer's pass over
//     it, the node's TIP commit-graph (interleavings_layers_test.go).
//
// Every check first scrubs every copy — the clock past ScrubInterval and
// each repository used — and then holds every file a scrub reads to its
// checksum: whatever damage the run left, the scrub found and the round
// before the check repaired. Each copy's installed chain is held sound as it
// stands, its round at the log's head having handed what moved to the
// installer; then the installer has its say, and each copy's chain is held
// to the rows' — installed to a prefix of it, the next layer refused — and
// to the fresh cache's.
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
// repositories over a run's steps never reach; the seeds draw that crash and
// that round ("round index over a crash's lock"), and putting the round's
// index back before the sweep turns the outcome — and the run — red. The
// maintainer is due again from interleavingMidxPacks packs beside its index
// rather than MidxPacks, at whose production count no seed maintains a
// repository over its own index and bitmap; and it is drawn at a weight of
// six, since a report's repair drops the index it would run over — at four,
// once the reports were drawn, no seed maintained a repository over its own
// index. A rebuild is drawn at a weight of five: at three, once the
// compaction's four operations were drawn beside the rest, no seed's rebuild
// met a copy a repair had just written, so none answered a request from the
// copy as it stands. A compaction a copy misses is an operation of its own
// rather than a compaction and a sweep meeting by chance: drawn apart, they
// met with no round of the repository between them over a copy holding its
// graph on no seed, so none reached a swap that dropped a graph the copy held.
// interleavingOutcomes holds the seeds to reaching every outcome; sixteen
// reach them all, in about half a minute under the race detector on two
// cores, the seeds in parallel. Sixty-four steps a seed, a check every ten:
// enough history for a recreation to go back to and short enough that a
// failure's trace reads in two screens. Forty were enough until the published
// chain's six operations drew a sixth of the steps and kept one repository's
// copy chained for most of a run, where a damage meets a layer's file rather
// than own mode's: at forty-eight, no seed's rebuild then answered a request
// from the copy as it stands, nor did any check drop a derived file.
const (
	interleavingSeeds = 16
	interleavingSteps = 64
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
	{"rebuild", 5, (*interleaving).rebuild},
	{"maintain", 6, (*interleaving).maintain},
	{"sweep tombstones", 2, (*interleaving).sweepTombstones},
	{"recreate backwards", 1, (*interleaving).recreateBackwards},
	{"damage and restart", 4, (*interleaving).damageAndRestart},
	{"replace while hashed", 3, (*interleaving).replaceWhileHashed},
	{"damage in place", 4, (*interleaving).damageInPlace},
	{"a request meets damage", 3, (*interleaving).requestMeetsDamage},
	{"report", 6, (*interleaving).report},
	{"scrub", 2, (*interleaving).scrub},
	{"compact", 6, (*interleaving).compact},
	{"sweep retirements", 2, (*interleaving).sweepRetirements},
	{"a compaction a copy missed", 2, (*interleaving).missedCompaction},
	{"a swap killed", 3, (*interleaving).killedSwap},
	{"a crash mid-swap", 2, (*interleaving).crashMidSwap},
	{"publish", 6, (*interleaving).publish},
	{"publish elsewhere", 5, (*interleaving).publishElsewhere},
	{"install", 6, (*interleaving).install},
	{"lose a layer's part", 2, (*interleaving).loseLayer},
	{"spoil a layer's part", 2, (*interleaving).spoilLayer},
	{"tip", 3, (*interleaving).tip},
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
	"crash leaving a maintenance run's files", "evict", "rebuild", "a request repaired in place",
	"a request answered from the copy as it stands", "maintain", "maintain over its own index",
	"sweep tombstones", "recreate backwards", "round index", "round index over a crash's lock",
	"a first round's check found damage", "a pack reinstalled in place", "a derived file dropped by its check",
	"a verdict withheld",
	"a report queued a repair round", "a stale report", "a deferred report", "a report in its scrub's cooldown",
	"a crash made the scrub due", "scrub", "a scrub found damage", "a pack body found damaged",
	"compact", "sweep retirements", "a swap with the graph kept", "a swap with the graph dropped",
	"a swap with no graph to keep", "a swap killed at its rename", "a failed swap retried by the next round",
	"a crash mid-swap",
	"a layer's file refetched by its check", "a chain file rewritten by its check",
	"a layer-only publication", "a repacking publication", "a publication by another git node",
	"a publication by a newer git", "a publication waited for its installer", "a repair",
	"a layer's part lost", "a layer's part spoiled", "a tip written",
	"layer installed", "layer adopted", "layer kept", "layer cut", "layer missing", "layer corrupt",
	"layer newer_git", "layer failed",
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
	// index in, and the crash's fields below — told by the cache's rounds,
	// on the cache's goroutines.
	indexMu sync.Mutex
	indexed map[string]bool
	// crashDir is the incarnation the last crash left a maintenance run's
	// files in; crashRounds, how many full rounds of the next process have
	// swept it since; crashIndexed, whether the FIRST of them wrote its
	// index there — between its own sweep and the next round's, rounds of
	// an incarnation running one at a time.
	crashDir     string
	crashRounds  int
	crashIndexed bool

	// swapMu guards swap, the file a replaceWhileHashed renames over while
	// the check hashes it — told by the cache's check, on its goroutines.
	swapMu sync.Mutex
	swap   string

	// killed is each repository whose swap a killedSwap failed at its
	// rename, by the incarnation the failure left serving and how many
	// code_cache_swapped lines there were then: what noteSwaps judges a
	// later swap from that incarnation by. A restart or an eviction ends
	// the line.
	killed map[string]swapKilled

	// damaged is every part of a published layer the run had the store lose
	// or spoil (loseLayer, spoilLayer): a layer a copy installed before may
	// stay installed there, and a fresh cache refuses it (check).
	damaged map[objstore.Key]bool
}

// swapKilled is a swap a killedSwap failed: the incarnation it left serving,
// and the code_cache_swapped lines logged before it.
type swapKilled struct {
	from, lines int
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
		killed: map[string]swapKilled{}, damaged: map[objstore.Key]bool{},
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
		if onPublished[o.name] {
			repo = in.published()
		}
		in.trace = append(in.trace, fmt.Sprintf("%d: %s, repo-%d", step, o.name, slices.Index(in.repos, repo)+1))
		in.trace[len(in.trace)-1] += ": " + o.run(in, repo)
		in.noteRoundIndexes()
		in.noteChecks()
		in.noteReports()
		in.noteSwaps()
		in.noteLayers()
		in.remember()
		in.audit()
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
// repositories over a run's steps hold a few dozen packs at the most between
// them, and at the production count no round would ever index — nor meet a
// dead maintenance run's lock in the way of its index.
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
		if dir == in.crashDir && in.crashRounds == 1 {
			in.crashIndexed = true
		}
	})(o)
	cache.AfterSweep(o, func(dir string) {
		in.indexMu.Lock()
		defer in.indexMu.Unlock()
		if dir == in.crashDir {
			in.crashRounds++
		}
	})
	cache.CheckPiecesOf(o, cache.CheckPieceBytes, in.hashed)
}

// noteRoundIndexes records the outcomes the rounds' own indexes reached: any
// round's, and the FIRST round after a crash writing its index in the
// incarnation the crash left a maintenance run's lock in — the lock its sweep
// must move before git will write the index over it. Judged at that round
// alone (crashIndexed): a later round of the same step — a repair a report
// queued, the check's own — finds the lock gone whatever the first did.
func (in *interleaving) noteRoundIndexes() {
	in.indexMu.Lock()
	defer in.indexMu.Unlock()
	if len(in.indexed) > 0 {
		in.saw("round index")
	}
	if in.crashIndexed {
		in.saw("round index over a crash's lock")
	}
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

// audit holds the backfill's schedule to a look at every copy after each
// step (cache.BackfillAudit): every change the step made to a copy's work —
// its index, its runs' vouches and checks, its convergence, its eviction, its
// rebuild — reached the schedule as a mark, or the backfill would never take
// it up.
func (in *interleaving) audit() {
	in.t.Helper()
	if problems := cache.BackfillAudit(in.c); len(problems) > 0 {
		in.fatalf("the backfill's schedule holds what no look at its copies says:\n\t%s",
			strings.Join(problems, "\n\t"))
	}
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
	row.Added = in.w.log.writePushes(repo, fakePush{packs: []code.PackRow{row}, refs: map[string]string{ref: tip},
		introduced: []string{tip}})
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
		statelog.FloorAt(statelog.ReadSession, in.last[repo]),
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
		in.crashDir, in.crashRounds = dir, 0
		delete(in.indexed, dir)
		in.indexMu.Unlock()
	}
	// What a killed swap left leaving the next process reads from the
	// state, in place: no swap of this one's follows it.
	clear(in.killed)
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
	for repo := range in.killed {
		if _, held := cache.Repos(in.c)[repo]; !held {
			delete(in.killed, repo)
		}
	}
	return fmt.Sprintf("%d of %d repositories evicted", evicted, before)
}

// rebuild is a request's pack or object failure over repo's current
// incarnation handed to Rebuild: answered from the copy as it stands when the
// failing run began before the cache last wrote into it (half the runs began
// an hour ago), repaired in place when its forced round or its scrub finds
// what to fetch again, and otherwise the whole rebuild, seeded with what the
// scrub found whole — or refused.
func (in *interleaving) rebuild(repo string) string {
	s := in.syncAt(repo, linearizable)
	defer s.Release()
	reason := []cache.RebuildReason{cache.RebuildCorrupt, cache.RebuildMissing}[in.rng.IntN(2)]
	began := time.Now()
	if in.rng.IntN(2) == 0 {
		began = began.Add(-time.Hour)
	}
	r, err := in.c.Rebuild(in.t.Context(), s, cache.Failure{Reason: reason, Began: began})
	switch {
	case err == nil:
		r.Release()
		switch {
		case r.Kind == cache.SyncNone:
			in.saw("a request answered from the copy as it stands")
			return fmt.Sprintf("%s: answered from incarnation %d as it stands", reason, cache.Incarnation(r))
		case cache.Incarnation(r) == cache.Incarnation(s):
			in.saw("a request repaired in place")
			return fmt.Sprintf("%s: incarnation %d repaired in place", reason, cache.Incarnation(r))
		}
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

// retiredRows is how many retirements of repo the rows still keep.
func (f *fakeLog) retiredRows(repo string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.repos[repo].retired)
}

// compact merges two or three of repo's live packs into one, as a git node's
// compaction would, and writes the record retiring them and adding it: what
// the copy's next round takes out of it by a swap (swap.go). Never a pack a
// published layer names, as no compaction's plan takes one and the decide
// refuses one: a copy's chain loads only over every pack it names.
func (in *interleaving) compact(repo string) string {
	live := in.w.log.unpublishedPacks(repo)
	if len(live) < 2 {
		return "fewer than two unpublished packs to merge"
	}
	in.rng.Shuffle(len(live), func(i, j int) { live[i], live[j] = live[j], live[i] })
	n := 2 + in.rng.IntN(min(len(live), 3)-1)
	out := in.w.compacted(in.t, repo, live[:n]...)
	in.last[repo] = out.Added
	in.saw("compact")
	return fmt.Sprintf("%d packs into pack-%s", n, out.GitChecksum)
}

// sweepRetirements takes every retirement of repo the rows keep out of them,
// as the code-retention duty does a day after a compaction: a copy last
// synced below it reads its next round whole, and that answer serves none of
// them, so whatever the copy still holds of them leaves as packs no
// retirement accounts for.
func (in *interleaving) sweepRetirements(repo string) string {
	n := in.w.log.retiredRows(repo)
	if n == 0 {
		return "no retirement to sweep"
	}
	in.w.log.sweepRetired(repo, in.w.log.head())
	in.last[repo] = in.w.log.head()
	in.saw("sweep retirements")
	return fmt.Sprintf("%d swept", n)
}

// missedCompaction is a day on an idle repository's copy: the maintainer
// indexes it — its commit-graph first — and then a compaction retires some
// of its packs and the code-retention duty sweeps the retirements before the
// copy's next round, which reads it whole below the sweep: its answer serves
// none of them, so the packs the compaction took out leave the copy as packs
// no retirement accounts for, by a swap that takes the graph with them. One
// operation rather than three: a compaction and a sweep, each drawn on its
// own, all but never met with no round of the repository between them over a
// copy holding its graph, and no seed of sixteen reached a swap that dropped
// a graph the copy held until this was drawn.
func (in *interleaving) missedCompaction(repo string) string {
	return strings.Join([]string{in.maintain(repo), in.compact(repo), in.sweepRetirements(repo)}, "; ")
}

// killedSwap is a compaction, then a linearizable sync whose swap fails at
// the point before its rename: the round answers from the copy it applied
// to, which keeps every pack, and what is leaving waits for the next round
// to swap it — whichever comes, the check's own included (noteSwaps).
func (in *interleaving) killedSwap(repo string) string {
	detail := in.compact(repo)
	hits := in.faults.Hits(codefault.SyncSwapBeforeRename)
	in.faults.Arm(codefault.SyncSwapBeforeRename, codefault.Action{Once: true, Err: errKilled})
	defer in.faults.Disarm(codefault.SyncSwapBeforeRename)
	lines := len(in.w.logs.lines(in.t, "code_cache_swapped"))
	s := in.syncAt(repo, linearizable)
	n := cache.Incarnation(s)
	s.Release()
	if in.faults.Hits(codefault.SyncSwapBeforeRename) == hits {
		return detail + "; no swap reached"
	}
	in.killed[repo] = swapKilled{from: n, lines: lines}
	in.saw("a swap killed at its rename")
	return fmt.Sprintf("%s; the swap from incarnation %d killed at its rename", detail, n)
}

// crashMidSwap is a killed swap and then a restart: the process ends with
// what was leaving the copy still in it, and the next boot's first round
// reads it from the state's floor (LeavingSince) as the retirements it is,
// in place.
func (in *interleaving) crashMidSwap(repo string) string {
	detail := in.killedSwap(repo)
	_, killed := in.killed[repo]
	in.c.Stop(in.t.Context())
	clear(in.killed)
	in.c = in.w.open(in.t, in.options)
	if killed {
		in.saw("a crash mid-swap")
	}
	return detail + "; restarted"
}

// noteSwaps records the outcomes the rounds' swaps reached: a swap that kept
// the commit-graph, one that dropped it, one of a copy that held none, and a
// killed swap TRIED AGAIN — a later swap of the same repository from the
// incarnation the killed one left serving, with nothing leaving it after.
func (in *interleaving) noteSwaps() {
	if counter(in.w.rec, metrics.CodeCacheSwaps, "graph", "kept") > 0 {
		in.saw("a swap with the graph kept")
	}
	if counter(in.w.rec, metrics.CodeCacheSwaps, "graph", "dropped") > 0 {
		in.saw("a swap with the graph dropped")
	}
	if counter(in.w.rec, metrics.CodeCacheSwaps, "graph", "none") > 0 {
		in.saw("a swap with no graph to keep")
	}
	if len(in.killed) == 0 {
		return
	}
	lines := in.w.logs.lines(in.t, "code_cache_swapped")
	for repo, k := range in.killed {
		for _, l := range lines[k.lines:] {
			if l["repo"] == repo && l["from"] == float64(k.from) && len(cache.Leaving(in.c, repo)) == 0 {
				in.saw("a failed swap retried by the next round")
				delete(in.killed, repo)
				break
			}
		}
	}
}

// check holds every repository's cache to a fresh cache's at the log's
// position now, and repos/ to holding no refused push's pack — after a scrub
// of every copy, whose findings the linearizable sync's round repairs.
func (in *interleaving) check(step int) {
	t := in.t
	in.scrubAll()
	in.noteReports()
	// THE INDEX OF PUSHES, read back as far as it goes: the first
	// repository's history wanted, as a seat's read of it here as its home
	// would want it, and every step the backfill has due taken — the gaps
	// the run's full rounds passed over, that history, the horizon — so the
	// check below holds what the backfill wrote as well as what the rounds
	// folded.
	in.c.WantHistory(in.repos[0])
	if _, err := cache.BackfillPass(in.c); err != nil {
		in.fatalf("the backfill: %v", err)
	}
	// THE PUBLISHED CHAIN AS IT STANDS, each copy's round at the log's head
	// having handed what moved to the installer; then the installer let have
	// its say, and every copy held to the rows' chain and to a fresh
	// cache's — its packs back to the rows' once the replacement of a layer
	// that held some is placed (the chain holds its packs until then).
	for _, repo := range in.repos {
		s := in.syncAt(repo, linearizable)
		in.checkLayersAsTheyStand(step, repo, s.Dir)
		s.Release()
	}
	for _, repo := range in.repos {
		s := settledSync(t, in.c, repo, in.waitOutInstaller)
		got := imageOf(t, s.Dir)
		in.checkSound(step, repo, s.Dir)
		if problems := indexAgainstLog(in.c, in.w.log, repo, s); len(problems) > 0 {
			t.Fatalf("after step %d, repo-%d's index of pushes:\n\t%s", step, slices.Index(in.repos, repo)+1,
				strings.Join(problems, "\n\t"))
		}
		want, fresh := in.w.rebuiltChain(t, repo)
		in.checkLayersSettled(step, repo, s.Dir, in.c.Chain(s), fresh)
		s.Release()
		if got != want {
			t.Fatalf("after step %d, repo-%d's cache:\n%s\nwant a fresh cache's at the same position:\n%s",
				step, slices.Index(in.repos, repo)+1, got, want)
		}
	}
	in.noteLayers()
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
	// The check's own rounds swap what a killed swap left leaving.
	in.noteSwaps()
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
