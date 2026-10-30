package cache_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/code"
	"github.com/crewlet/crewlet/internal/code/cache"
	"github.com/crewlet/crewlet/internal/code/gitproc"
	"github.com/crewlet/crewlet/internal/sourcetree"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/statelog/metrics"
)

// A BITMAP THE NODE'S MEMORY CANNOT HOLD IS REPLACED BY AN INDEX WITHOUT ONE.
// A bitmap holds about BitmapBytesPerObject an object of its repository, and
// one past what the node can give its background work is killed by the
// kernel at every duty cycle — or the engine is, in its place. So a
// repository whose bitmap's charge is past the budget never starts one: it is
// indexed WITHOUT a bitmap (and its graph written), and stays served from an
// index rather than from every pack's own; the bitmap's refusal is logged and
// counted once for its incarnation, kept on a gauge while it is refused, and
// raises code_maintenance_refused at the budget's own threshold. At a budget
// of the bitmap's charge the bitmap is written, after which the gauge and the
// alarm clear.
//
// Mutation: drop the index without a bitmap from plan, and the first pass
// writes the graph alone; drop the budget check from plan, and it writes the
// bitmap.
func TestABitmapTheNodesMemoryCannotHoldIsReplacedByAnIndexWithoutOne(t *testing.T) {
	t.Parallel()
	w := newSyncWorld(t, onMemory)
	clk := newClock()
	repo := w.repo(t, 1)
	w.push(t, repo, "refs/heads/main", nil)
	w.push(t, repo, "refs/heads/main", nil)
	c := w.open(t, clk.on)
	defer w.sync(t, c, repo).Release()
	bitmap := cache.BitmapCharge(c, repo)
	cache.SetMaintenanceBudget(c, bitmap-1)
	clk.Add(cache.MidxDebounce)
	tracker := statelog.NewTracker(nil, clk.Now)
	reading := func() statelog.Reading {
		var r statelog.Reading
		c.AlarmReading(&r)
		return r
	}

	if ran := cache.MaintainerPass(c); len(ran) != 1 || ran[0] != repo {
		t.Fatalf("on a budget a byte short of the bitmap's charge the maintainer ran %v, want the repository", ran)
	}
	if packs, withBitmap, graph := cache.Indexed(c, repo); packs != 2 || withBitmap || !graph {
		t.Fatalf("the run left an index over %d packs (bitmap %v) and graph %v, want both packs indexed "+
			"without a bitmap, and the graph", packs, withBitmap, graph)
	}
	for range 2 {
		clk.Add(cache.MidxMinInterval)
		if ran := cache.MaintainerPass(c); len(ran) != 0 {
			t.Errorf("a pass over a repository whose index is current ran %v", ran)
		}
	}
	if got := gaugeAt(t, w.rec, metrics.CodeCacheMaintenanceRefused, "step", "bitmap"); got != 1 {
		t.Errorf("%s{step=bitmap} = %v while the bitmap is refused, want 1", metrics.CodeCacheMaintenanceRefused, got)
	}
	if got := counter(w.rec, metrics.CodeCacheMaintenanceRefusals, "step", "bitmap"); got != 1 {
		t.Errorf("%s{step=bitmap} = %d after three passes, want the incarnation counted once",
			metrics.CodeCacheMaintenanceRefusals, got)
	}
	if got := counter(w.rec, metrics.CodeCacheMaintenanceRefusals, "step", "index"); got != 0 {
		t.Errorf("%s{step=index} = %d, want none: the index without a bitmap fit", metrics.CodeCacheMaintenanceRefusals,
			got)
	}
	lines := w.logs.lines(t, "code_cache_maintenance_refused")
	if len(lines) != 1 {
		t.Fatalf("the refusal was logged %d times over three passes, want once an incarnation", len(lines))
	}
	if l := lines[0]; l["repo"] != repo || l["step"] != "bitmap" || l["estimate"] != float64(bitmap) ||
		l["budget"] != float64(bitmap-1) {
		t.Errorf("the refusal's line is %v, want the repository, the bitmap, its charge %d and the budget %d", l,
			bitmap, bitmap-1)
	}
	r := reading()
	if r.CodeMaintenanceRefused != 1 || r.CodeMaintenanceLargest != bitmap || r.CodeMaintenanceBudget != bitmap-1 {
		t.Errorf("the alarm reading is %d refused, the largest %d, the budget %d; want 1, %d, %d",
			r.CodeMaintenanceRefused, r.CodeMaintenanceLargest, r.CodeMaintenanceBudget, bitmap, bitmap-1)
	}
	tracker.Observe(t.Context(), statelog.Evaluate(r))
	if _, firing := tracker.Firing()[statelog.KindCodeMaintenanceRefused]; !firing {
		t.Errorf("%s is not firing with a bitmap refused; fired %v", statelog.KindCodeMaintenanceRefused,
			tracker.Firing())
	}

	cache.SetMaintenanceBudget(c, bitmap)
	clk.Add(cache.MidxMinInterval)
	if ran := cache.MaintainerPass(c); len(ran) != 1 || ran[0] != repo {
		t.Fatalf("at a budget of the bitmap's own charge the maintainer ran %v, want the repository", ran)
	}
	if packs, withBitmap, _ := cache.Indexed(c, repo); packs != 2 || !withBitmap {
		t.Errorf("at the bitmap's charge the run left an index over %d packs (bitmap %v), want the bitmap", packs,
			withBitmap)
	}
	cache.MaintainerPass(c)
	if got := gaugeAt(t, w.rec, metrics.CodeCacheMaintenanceRefused, "step", "bitmap"); got != 0 {
		t.Errorf("%s{step=bitmap} = %v once the bitmap was written, want 0", metrics.CodeCacheMaintenanceRefused, got)
	}
	clk.Add(time.Minute)
	tracker.Observe(t.Context(), statelog.Evaluate(reading()))
	if _, firing := tracker.Firing()[statelog.KindCodeMaintenanceRefused]; firing {
		t.Errorf("%s still fires once nothing is refused", statelog.KindCodeMaintenanceRefused)
	}
}

// A GRAPH THE NODE'S MEMORY CANNOT HOLD IS SAID SO AS A BITMAP IS. A copy
// refused its commit-graph walks its history by date for as long as it is
// refused — a push of a commit dated before its parent walks every commit —
// so the refusal is logged and counted once for the incarnation and kept on
// the refused gauge under its own step while it stands, beside the bitmap's;
// the alarm stands for it, since no repository is refused its graph and not
// its bitmap. At a budget of the graph's charge the graph is written first,
// and the gauge's graph falls back.
//
// Mutation: count no refused graph in refusalWalk.note, or set the gauge for
// bitmaps alone in noteRefusals, and the graph's gauge stays at zero.
func TestAGraphTheNodesMemoryCannotHoldIsGaugedAsABitmapIs(t *testing.T) {
	t.Parallel()
	w := newSyncWorld(t, onMemory)
	clk := newClock()
	repo := w.repo(t, 1)
	w.push(t, repo, "refs/heads/main", nil)
	w.push(t, repo, "refs/heads/main", nil)
	c := w.open(t, clk.on)
	defer w.sync(t, c, repo).Release()
	graph := cache.GraphCharge(c, repo)
	cache.SetMaintenanceBudget(c, graph-1)
	for range 2 {
		if ran := cache.MaintainerPass(c); len(ran) != 0 {
			t.Fatalf("on a budget a byte short of the graph's charge the maintainer ran %v", ran)
		}
		clk.Add(cache.MidxMinInterval)
	}
	for step, want := range map[string]float64{"graph": 1, "bitmap": 1} {
		if got := gaugeAt(t, w.rec, metrics.CodeCacheMaintenanceRefused, "step", step); got != want {
			t.Errorf("%s{step=%s} = %v while the graph is refused, want %v", metrics.CodeCacheMaintenanceRefused,
				step, got, want)
		}
	}
	if got := counter(w.rec, metrics.CodeCacheMaintenanceRefusals, "step", "graph"); got != 1 {
		t.Errorf("%s{step=graph} = %d after two passes, want the incarnation counted once",
			metrics.CodeCacheMaintenanceRefusals, got)
	}
	var lines []map[string]any
	for _, l := range w.logs.lines(t, "code_cache_maintenance_refused") {
		if l["step"] == "graph" {
			lines = append(lines, l)
		}
	}
	if len(lines) != 1 || lines[0]["repo"] != repo || lines[0]["estimate"] != float64(graph) {
		t.Errorf("the graph's refusal was logged as %v, want once with its charge %d", lines, graph)
	}
	var r statelog.Reading
	c.AlarmReading(&r)
	if r.CodeMaintenanceRefused != 1 {
		t.Errorf("the alarm reading counts %d repositories refused, want the one refused its graph and its "+
			"bitmap", r.CodeMaintenanceRefused)
	}

	cache.SetMaintenanceBudget(c, graph)
	if ran := cache.MaintainerPass(c); len(ran) != 1 {
		t.Fatalf("at a budget of the graph's own charge the maintainer ran %v, want the repository's graph", ran)
	}
	if _, _, written := cache.Indexed(c, repo); !written {
		t.Error("at the graph's charge the run wrote no graph")
	}
	cache.MaintainerPass(c)
	if got := gaugeAt(t, w.rec, metrics.CodeCacheMaintenanceRefused, "step", "graph"); got != 0 {
		t.Errorf("%s{step=graph} = %v once the graph was written, want 0", metrics.CodeCacheMaintenanceRefused, got)
	}
}

// A TIP THE NODE'S MEMORY CANNOT HOLD IS GAUGED, AND RAISES NOTHING. A copy
// serving from an installed chain writes one thing of its own, its tip over
// the tail, charged TipGraphBytesPerObject an object of the tail and
// TipGraphBaseBytes whatever its length, with room for a clone of the whole
// repository beside it (Write.charge). Past the budget it never runs: its
// refusal is logged and counted once for the incarnation and kept on the
// refused gauge under its own step while it stands — and the alarm, which
// says a repository is served without a bitmap, does not stand for it, since
// the copy is served through its chain's bitmap and graph layers and only the
// tail's commits walk by date. At a budget of the tip's own charge it is
// written at once, and the gauge falls back.
//
// Mutations: count no refused tip in refusalWalk.note, and the tip's gauge
// stays at zero; count it with the bitmaps, and the alarm reading names the
// repository; charge the tip by the repository's objects (plan's tip case
// dropped), and the budget of its charge holds no run; charge it without its
// fixed part (Write.charge's BaseBytes dropped), and the budget a byte short
// of its charge runs it.
func TestATipTheNodesMemoryCannotHoldIsGaugedAndRaisesNoAlarm(t *testing.T) {
	t.Parallel()
	w := newSyncWorld(t, onMemory)
	clk := newClock()
	repo := w.repo(t, 1)
	p := w.pushN(t, repo, 3)
	w.log.publish(repo, w.buildChain(t, p[:2]), nil)
	c := w.open(t, clk.on)
	defer w.sync(t, c, repo).Release()
	tip := cache.TipCharge(c, repo)
	objects, tail := cache.ObjectsOf(c, repo), cache.TailObjects(c, repo)
	if tail <= 0 || tail >= objects {
		t.Fatalf("the premise: a tail of %d objects in a repository of %d, want some and fewer than all", tail,
			objects)
	}
	room := objects * gitproc.TransferBytesPerObject
	if repoWide := objects*cache.TipGraphBytesPerObject + cache.TipGraphBaseBytes + room; tip >= repoWide ||
		tip <= cache.TipGraphBaseBytes+room {
		t.Fatalf("the tip's charge is %d over a repository of %d objects: want its figure over the tail alone, "+
			"under %d, beside its fixed part and the clone's room over the whole repository, %d", tip, objects,
			repoWide, cache.TipGraphBaseBytes+room)
	}
	cache.SetMaintenanceBudget(c, tip-1)
	for range 2 {
		if ran := cache.MaintainerPass(c); len(ran) != 0 {
			t.Fatalf("on a budget a byte short of the tip's charge the maintainer ran %v", ran)
		}
		clk.Add(cache.MidxMinInterval)
	}
	for step, want := range map[string]float64{"tip-graph": 1, "bitmap": 0, "graph": 0} {
		if got := gaugeAt(t, w.rec, metrics.CodeCacheMaintenanceRefused, "step", step); got != want {
			t.Errorf("%s{step=%s} = %v while the tip is refused, want %v", metrics.CodeCacheMaintenanceRefused,
				step, got, want)
		}
	}
	if got := counter(w.rec, metrics.CodeCacheMaintenanceRefusals, "step", "tip-graph"); got != 1 {
		t.Errorf("%s{step=tip-graph} = %d after two passes, want the incarnation counted once",
			metrics.CodeCacheMaintenanceRefusals, got)
	}
	var lines []map[string]any
	for _, l := range w.logs.lines(t, "code_cache_maintenance_refused") {
		if l["step"] == "tip-graph" {
			lines = append(lines, l)
		}
	}
	if len(lines) != 1 || lines[0]["repo"] != repo || lines[0]["estimate"] != float64(tip) {
		t.Errorf("the tip's refusal was logged as %v, want once with its charge %d", lines, tip)
	}
	var r statelog.Reading
	c.AlarmReading(&r)
	if r.CodeMaintenanceRefused != 0 || r.CodeMaintenanceLargest != 0 {
		t.Errorf("the alarm reading counts %d repositories refused, the largest %d: want none, a copy refused "+
			"its tip being served through its chain's bitmap", r.CodeMaintenanceRefused, r.CodeMaintenanceLargest)
	}
	tracker := statelog.NewTracker(nil, clk.Now)
	tracker.Observe(t.Context(), statelog.Evaluate(r))
	if _, firing := tracker.Firing()[statelog.KindCodeMaintenanceRefused]; firing {
		t.Errorf("%s fires with only a tip refused", statelog.KindCodeMaintenanceRefused)
	}

	cache.SetMaintenanceBudget(c, tip)
	if ran := cache.MaintainerPass(c); !slices.Equal(ran, []string{repo}) {
		t.Fatalf("at a budget of the tip's own charge the maintainer ran %v, want the repository's tip", ran)
	}
	if id, _ := cache.Tip(c, repo); id == "" {
		t.Error("at the tip's charge the run wrote no tip")
	}
	cache.MaintainerPass(c)
	if got := gaugeAt(t, w.rec, metrics.CodeCacheMaintenanceRefused, "step", "tip-graph"); got != 0 {
		t.Errorf("%s{step=tip-graph} = %v once the tip was written, want 0", metrics.CodeCacheMaintenanceRefused,
			got)
	}
}

// A REFUSED REPOSITORY KEEPS ITS PUSHES INDEXED. A repository whose bitmap the
// node cannot hold is one that only grows, usually the most pushed-to, and a
// pack outside every index is one more .idx every lookup searches: refused for
// good, it must not pile its pushes up outside one. So the index without a
// bitmap is written again at the cadence any index is — MidxPacks gathered
// beside it — and covers every pack pushed since.
//
// The budget is held a byte short of the bitmap's charge AS THE COPY STANDS
// after each push, so the bitmap is refused for good whatever the repository
// grows to, while the index and the graph — a fraction of the bitmap's bytes
// an object, beside the same room — always fit: a budget fixed at the
// repository's first size would refuse the index too once the repository
// outgrew it by the ratio of the bitmap's charge to the index's.
//
// Mutation: drop the index without a bitmap from plan, and no pack is ever
// indexed.
func TestARefusedRepositoryKeepsItsPushesIndexed(t *testing.T) {
	t.Parallel()
	w := newSyncWorld(t, onMemory)
	clk := newClock()
	repo := w.repo(t, 1)
	w.push(t, repo, "refs/heads/main", nil)
	w.push(t, repo, "refs/heads/main", nil)
	c := w.open(t, clk.on, cache.MidxPacksAt(2))
	w.sync(t, c, repo).Release()
	cache.SetMaintenanceBudget(c, cache.BitmapCharge(c, repo)-1)
	clk.Add(cache.MidxDebounce)
	if ran := cache.MaintainerPass(c); len(ran) != 1 {
		t.Fatalf("the first pass ran %v, want the repository indexed without a bitmap", ran)
	}
	for push := range 4 {
		w.push(t, repo, "refs/heads/main", nil)
		w.sync(t, c, repo).Release()
		cache.SetMaintenanceBudget(c, cache.BitmapCharge(c, repo)-1)
		clk.Add(cache.MidxMinInterval)
		cache.MaintainerPass(c)
		packs, withBitmap, _ := cache.Indexed(c, repo)
		if withBitmap {
			t.Fatalf("after push %d the index has a bitmap the budget cannot hold", push)
		}
		if want := 2 + push + 1; push%2 == 1 && packs != want {
			t.Errorf("after push %d — %d packs, two beside the index — it covers %d, want all %d", push, want, packs,
				want)
		}
	}
	if got := counter(w.rec, metrics.CodeCacheMaintenanceRefusals, "step", "bitmap"); got != 1 {
		t.Errorf("%s{step=bitmap} = %d, want the incarnation counted once however often it was indexed",
			metrics.CodeCacheMaintenanceRefusals, got)
	}
}

// A STALE BITMAP IS KEPT UNTIL MidxRoundPacks GATHER OUTSIDE IT. A repository
// that outgrows the node's memory has a bitmap the node wrote while it could,
// over most of its history; an index without a bitmap over every pack would
// delete it (git removes a bitmap that does not match the index it writes),
// and a clone would walk the whole history rather than count most of it. So
// the bitmap stands while the packs outside it are fewer than MidxRoundPacks —
// the most this cache serves outside an index anywhere — and past them, the
// index without a bitmap over every pack replaces it, so a refused
// repository's pushes never pile up outside an index without bound. The
// budget is held a byte short of the bitmap's charge as the copy stands after
// each batch of pushes (TestARefusedRepositoryKeepsItsPushesIndexed says why).
//
// Mutation: replace a stale bitmap at MidxPacks rather than MidxRoundPacks,
// and the second pass deletes it.
func TestAStaleBitmapIsKeptUntilMidxRoundPacksGatherOutsideIt(t *testing.T) {
	t.Parallel()
	w := newSyncWorld(t, onMemory)
	clk := newClock()
	repo := w.repo(t, 1)
	w.push(t, repo, "refs/heads/main", nil)
	w.push(t, repo, "refs/heads/main", nil)
	c := w.open(t, clk.on, cache.MidxPacksAt(2))
	w.sync(t, c, repo).Release()
	cache.SetRoundPacks(c, 4)
	clk.Add(cache.MidxDebounce)
	if ran := cache.MaintainerPass(c); len(ran) != 1 {
		t.Fatalf("the first pass ran %v, want the repository's bitmap", ran)
	}
	if packs, withBitmap, _ := cache.Indexed(c, repo); packs != 2 || !withBitmap {
		t.Fatalf("the first run left an index over %d packs (bitmap %v), want the bitmap over both", packs,
			withBitmap)
	}
	pushes := func(n int) {
		for range n {
			w.push(t, repo, "refs/heads/main", nil)
		}
		w.sync(t, c, repo).Release()
		cache.SetMaintenanceBudget(c, cache.BitmapCharge(c, repo)-1)
		clk.Add(cache.MidxMinInterval)
	}

	pushes(2)
	if ran := cache.MaintainerPass(c); len(ran) != 0 {
		t.Errorf("with MidxPacks beside a stale bitmap the maintainer ran %v, want it kept", ran)
	}
	if packs, withBitmap, _ := cache.Indexed(c, repo); packs != 2 || !withBitmap {
		t.Errorf("the stale bitmap is an index over %d packs (bitmap %v), want it standing over the first two",
			packs, withBitmap)
	}
	pushes(2)
	if ran := cache.MaintainerPass(c); len(ran) != 1 {
		t.Fatalf("with MidxRoundPacks beside a stale bitmap the maintainer ran %v, want the repository", ran)
	}
	if packs, withBitmap, _ := cache.Indexed(c, repo); packs != 6 || withBitmap {
		t.Errorf("the run left an index over %d packs (bitmap %v), want all six without a bitmap", packs, withBitmap)
	}
}

// EACH STEP IS ADMITTED ON ITS OWN CHARGE. A run's steps run one after the
// other and hold their own memory, so a budget past the graph's charge and
// short of the index's writes the graph alone, and one short of every step
// runs nothing — each step refused counted and logged once, under its name.
//
// Mutation: refuse a run whole when its bitmap is refused, and the small
// repository's graph is never written.
func TestEachStepIsAdmittedOnItsOwnCharge(t *testing.T) {
	t.Parallel()
	w := newSyncWorld(t, onMemory)
	clk := newClock()
	small, large := w.repo(t, 1), w.repo(t, 2)
	w.push(t, small, "refs/heads/main", nil)
	for range 4 {
		w.push(t, large, "refs/heads/main", nil)
	}
	c := w.open(t, clk.on)
	w.sync(t, c, small).Release()
	w.sync(t, c, large).Release()
	graph := cache.GraphCharge(c, small)
	if cache.IndexCharge(c, small) <= graph || cache.GraphCharge(c, large) <= graph {
		t.Fatalf("charges: the small repository's index %d and the large one's graph %d, want both past the small "+
			"one's graph %d", cache.IndexCharge(c, small), cache.GraphCharge(c, large), graph)
	}
	cache.SetMaintenanceBudget(c, graph)
	clk.Add(cache.MidxDebounce)

	if ran := cache.MaintainerPass(c); len(ran) != 1 || ran[0] != small {
		t.Fatalf("the pass ran %v, want the small repository's graph alone", ran)
	}
	if packs, _, written := cache.Indexed(c, small); packs != 0 || !written {
		t.Errorf("the small repository holds an index over %d packs and graph %v, want no index and the graph",
			packs, written)
	}
	if packs, _, written := cache.Indexed(c, large); packs != 0 || written {
		t.Errorf("the large repository holds an index over %d packs and graph %v, want nothing", packs, written)
	}
	for step, want := range map[string]uint64{"bitmap": 2, "index": 2, "graph": 1} {
		if got := counter(w.rec, metrics.CodeCacheMaintenanceRefusals, "step", step); got != want {
			t.Errorf("%s{step=%s} = %d, want %d", metrics.CodeCacheMaintenanceRefusals, step, got, want)
		}
	}
	if got := len(w.logs.lines(t, "code_cache_maintenance_refused")); got != 5 {
		t.Errorf("the refusals were logged %d times, want one a step of each repository, 5", got)
	}
}

// A FULL ROUND'S INDEX IS HELD TO THE NODE'S MEMORY TOO. It is a merge of the
// packs' indexes, IndexBytesPerObject an object — gigabytes over the largest
// repository the defaults admit — written in the READ lane before a request
// is answered, after a restart, a rebuild or an eviction. So past the budget
// alone it is refused (said once for the incarnation, as the maintainer's own
// would be) and the round answers without it; at the budget it is written;
// and when the work in flight leaves it no room, the round never waits for
// room — it answers without the index and says so.
//
// Mutation: drop the budget check from indexRound, and the first round writes
// an index the budget cannot hold.
func TestAFullRoundsIndexIsHeldToTheNodesMemory(t *testing.T) {
	t.Parallel()
	w := newSyncWorld(t, onMemory)
	repo, other := w.repo(t, 1), w.repo(t, 2)
	pushed := func(repo string) int64 {
		var objects int64
		for range 3 {
			_, row := w.push(t, repo, "refs/heads/main", nil)
			objects += row.Objects
		}
		return objects * (cache.IndexBytesPerObject + gitproc.TransferBytesPerObject)
	}
	mem, otherMem := pushed(repo), pushed(other)
	indexed := 0
	onIndex := cache.OnRoundIndex(func(string) { indexed++ })
	midx := func(c *cache.Cache, repo string) bool {
		_, err := os.Stat(filepath.Join(w.dir(t, c, repo), "objects", "pack", "multi-pack-index"))
		return err == nil
	}

	c := w.open(t, onIndex)
	cache.SetRoundPacks(c, 3)
	cache.SetMaintenanceBudget(c, mem-1)
	w.sync(t, c, repo).Release()
	if indexed != 0 || midx(c, repo) {
		t.Fatalf("a round whose index's charge %d is past the budget wrote it", mem)
	}
	if got := counter(w.rec, metrics.CodeCacheMaintenanceRefusals, "step", "index"); got != 1 {
		t.Errorf("%s{step=index} = %d after a round refused its index, want 1", metrics.CodeCacheMaintenanceRefusals,
			got)
	}

	c = w.reopen(t, c, onIndex)
	cache.SetRoundPacks(c, 3)
	cache.SetMaintenanceBudget(c, mem)
	w.sync(t, c, repo).Release()
	if indexed != 1 || !midx(c, repo) {
		t.Fatalf("a round whose index's charge is the budget wrote %d indexes, want it written", indexed)
	}

	cache.SetMaintenanceBudget(c, max(mem, otherMem))
	held, release := cache.HoldOther(c, max(mem, otherMem))
	if !held {
		t.Fatal("the case could not hold the budget")
	}
	w.sync(t, c, other).Release()
	release()
	if indexed != 1 || midx(c, other) {
		t.Errorf("a round the work in flight left no room for wrote its index")
	}
	if got := len(w.logs.lines(t, "code_cache_round_index_held")); got != 1 {
		t.Errorf("a round index held back by the work in flight was logged %d times, want once", got)
	}
}

// THE FIRST REPOSITORY HELD BACK IS GIVEN THE ROOM IT NEEDS. A run that fits
// the budget alone but not beside the runs in flight waits for them — and
// were the smaller repositories behind it in the pass's order let in as the
// room drained, a steady stream of them would keep it waiting for as long as
// they came: the repository with the most packs uncovered, often the
// largest, starved by the rest. So nothing starts behind it that would leave
// it less than its charge once the runs ahead of it end, and it starts as
// soon as they have.
//
// Mutation: hold no reservation in maintainerPass, and the second pass starts
// the small repository in the large one's room.
func TestTheFirstRepositoryHeldBackIsGivenTheRoomItNeeds(t *testing.T) {
	t.Parallel()
	w := newSyncWorld(t, onMemory)
	w.processes = 8
	clk := newClock()
	first, large, small := w.repo(t, 1), w.repo(t, 2), w.repo(t, 3)
	perObject := int64(cache.BitmapBytesPerObject + gitproc.TransferBytesPerObject)
	pushed := func(repo string, n int) int64 {
		var objects int64
		for range n {
			_, row := w.push(t, repo, "refs/heads/main", nil)
			objects += row.Objects
		}
		return objects * perObject
	}
	cFirst, cLarge, cSmall := pushed(first, 1), pushed(large, 6), pushed(small, 1)
	release := make(chan struct{})
	c := w.open(t, clk.on, func(o *cache.Options) {
		cache.BeforeMaintaining(o, func(ctx context.Context, step string) {
			if step != "multi-pack-index" {
				return
			}
			select {
			case <-release:
			case <-ctx.Done():
			}
		})
	})
	cache.SetMaintenanceMemory(c, 1<<40)
	budget := cLarge + cFirst - 1
	if cSmall+cFirst > budget || cSmall+cFirst+cLarge <= budget {
		t.Fatalf("charges %d, %d and %d on a budget of %d: the case needs the small run to fit beside the first "+
			"and not beside both", cFirst, cLarge, cSmall, budget)
	}
	cache.SetMaintenanceBudget(c, budget)
	w.sync(t, c, first).Release()
	graphFirst(t, c, first)
	clk.Add(cache.MidxDebounce)
	started, waitFirst := cache.StartMaintainerPass(c)
	if len(started) != 1 || started[0] != first {
		close(release)
		waitFirst()
		t.Fatalf("the first pass started %v, want %s", started, first)
	}

	w.sync(t, c, large).Release()
	w.sync(t, c, small).Release()
	graphFirst(t, c, large, small)
	clk.Add(cache.MidxDebounce)
	started, waitHeld := cache.StartMaintainerPass(c)
	close(release)
	waitFirst()
	waitHeld()
	if len(started) != 0 {
		t.Fatalf("with %s held back for room, the pass started %v in the room it needs", large, started)
	}
	if ran := cache.MaintainerPass(c); !slices.Contains(ran, large) {
		t.Errorf("once the run ahead of it ended the pass ran %v, want %s", ran, large)
	}
}

// THE RUNS IN FLIGHT FIT THE BUDGET TOGETHER: two runs that each fit it but
// not side by side would hold more than the node can give at once, and a
// node killed by two is no better off than one killed by one. So with room
// in the lane and an allowance past both, the second waits for the first —
// held back, not refused: it runs at the next pass after the first ends,
// and nothing counts it refused.
//
// Mutation: hold only one run to the budget, not the runs in flight
// together, and the first pass starts both.
func TestTheRunsInFlightFitTheBudgetTogether(t *testing.T) {
	t.Parallel()
	w := newSyncWorld(t, onMemory)
	w.processes = 8
	clk := newClock()
	a, b := w.repo(t, 1), w.repo(t, 2)
	for range 3 {
		w.push(t, a, "refs/heads/main", nil)
	}
	w.push(t, b, "refs/heads/main", nil)
	w.push(t, b, "refs/heads/main", nil)
	release := make(chan struct{})
	c := w.open(t, clk.on, func(o *cache.Options) {
		cache.BeforeMaintaining(o, func(ctx context.Context, step string) {
			if step != "multi-pack-index" {
				return
			}
			select {
			case <-release:
			case <-ctx.Done():
			}
		})
	})
	defer w.sync(t, c, a).Release()
	defer w.sync(t, c, b).Release()
	ea, eb := cache.BitmapCharge(c, a), cache.BitmapCharge(c, b)
	cache.SetMaintenanceMemory(c, 1<<40)
	cache.SetMaintenanceBudget(c, max(ea, eb)+min(ea, eb)-1)
	graphFirst(t, c, a, b)
	clk.Add(cache.MidxDebounce)

	started, wait := cache.StartMaintainerPass(c)
	if len(started) != 1 || started[0] != a {
		close(release)
		wait()
		t.Fatalf("the pass started %v, want only %s (the more packs uncovered): charges %d and %d against a "+
			"budget of %d for both", started, a, ea, eb, max(ea, eb)+min(ea, eb)-1)
	}
	close(release)
	wait()
	if ran := cache.MaintainerPass(c); len(ran) != 1 || ran[0] != b {
		t.Errorf("the next pass ran %v, want %s, which the budget held back while %s ran", ran, b, a)
	}
	if got := counter(w.rec, metrics.CodeCacheMaintenanceRefusals, "reason", "memory"); got != 0 {
		t.Errorf("a run held back for room was counted refused (%d): it fits the budget alone", got)
	}
}

// THE MEMORY IS READ AGAIN AT EVERY PASS. A running container's limit can be
// changed in place — Kubernetes resizes a pod's memory without a restart — and
// a budget read once would go on admitting runs past a limit lowered under
// it, the kill this exists to prevent. So each maintainer pass reads it again
// and moves the budget with it, saying so; and a read that fails keeps the
// reading before it, said once, never a guess.
//
// Mutation: read the memory only at Open, and the pass after a resize writes a
// bitmap the new limit cannot hold.
func TestTheMemoryIsReadAgainAtEveryPass(t *testing.T) {
	t.Parallel()
	w := newSyncWorld(t, onMemory)
	clk := newClock()
	repo := w.repo(t, 1)
	w.push(t, repo, "refs/heads/main", nil)
	c := w.open(t, clk.on)
	defer w.sync(t, c, repo).Release()
	clk.Add(cache.MidxDebounce)
	resized := cache.MemoryReserved(c) + cache.BitmapCharge(c, repo) - 1
	cache.SetNodeMemory(c, resized)

	if ran := cache.MaintainerPass(c); len(ran) != 1 {
		t.Fatalf("the pass after the resize ran %v, want the repository", ran)
	}
	if _, withBitmap, _ := cache.Indexed(c, repo); withBitmap {
		t.Errorf("the pass after a resize wrote a bitmap the new memory cannot hold")
	}
	lines := w.logs.lines(t, "code_cache_memory_changed")
	if len(lines) != 1 || lines[0]["memory"] != float64(resized) ||
		lines[0]["maintenance_budget"] != float64(cache.BitmapCharge(c, repo)-1) {
		t.Errorf("the resize was logged as %v, want one line naming the memory %d and the budget it leaves", lines,
			resized)
	}

	cache.NodeMemoryUnreadable(c, errors.New("reading /proc/meminfo: no such file"))
	for range 2 {
		cache.MaintainerPass(c)
	}
	if got := len(w.logs.lines(t, "code_cache_memory_unreadable")); got != 1 {
		t.Errorf("two passes that could not read the memory logged it %d times, want once", got)
	}
	if got := c.BackgroundBudget(); got != cache.BitmapCharge(c, repo)-1 {
		t.Errorf("a failed read moved the budget to %d, want the last reading's %d", got,
			cache.BitmapCharge(c, repo)-1)
	}
}

// A CHECK THE NODE'S MEMORY CANNOT HOLD IS REFUSED, NEVER RUN. Verify's
// connectivity check holds FsckBytesPerObject an object of its repository —
// some 11 GB over the largest the defaults admit — so it is held to the same
// budget as the background work: past it alone it is refused for this node
// (ErrNoMemory), and when the work in flight leaves it no room it is busy,
// as a full lane is, and answered when asked again.
//
// Mutation: drop the budget check from fsck, and the first Verify runs it.
func TestACheckTheNodesMemoryCannotHoldIsRefused(t *testing.T) {
	t.Parallel()
	w := newSyncWorld(t, onMemory)
	repo := w.repo(t, 1)
	w.push(t, repo, "refs/heads/main", nil)
	c := w.open(t)
	w.sync(t, c, repo).Release()
	fsck := cache.FsckCharge(c, repo)

	cache.SetMaintenanceBudget(c, fsck-1)
	if _, err := verify(t.Context(), c, repo, false); !errors.Is(err, cache.ErrNoMemory) {
		t.Errorf("Verify on a budget a byte short of its check = %v, want ErrNoMemory", err)
	}
	cache.SetMaintenanceBudget(c, fsck)
	held, release := cache.HoldOther(c, 1)
	if !held {
		t.Fatal("the case could not hold a byte of the budget")
	}
	if _, err := verify(t.Context(), c, repo, false); !errors.Is(err, gitproc.ErrBusy) {
		t.Errorf("Verify with the work in flight leaving no room = %v, want busy", err)
	}
	release()
	if r, err := verify(t.Context(), c, repo, false); err != nil || len(r.Problems) != 0 {
		t.Errorf("Verify at its check's own charge = %+v, %v; want it run, and clean", r, err)
	}
}

// OPEN HOLDS THE BACKGROUND WORK TO THE NODE'S MEMORY: on the machine the
// suite runs on, read by the pool as a node's is, the budget is that memory
// less TransferMemory a transfer of the pool's lane and ServingMemoryPerCore
// a core its memory backs, never below zero — and the boot's line says all
// four.
func TestOpenHoldsTheBackgroundWorkToTheNodesMemory(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	w.machineMemory = true
	c := w.open(t)
	lines := w.logs.lines(t, "code_cache_opened")
	if len(lines) != 1 {
		t.Fatalf("the open logged %d code_cache_opened lines", len(lines))
	}
	l := lines[0]
	memory, from := int64(numberOf(t, l, "memory")), l["memory_from"]
	if memory != w.pool.Memory().Bytes || from != w.pool.Memory().From || from == "gitproc.Options.Memory" {
		t.Errorf("the open read the memory as %d bytes from %v, want the machine's as its pool read it, %+v", memory,
			from, w.pool.Memory())
	}
	reserved := cache.MemoryReserve(w.pool.Slots(gitproc.ClassTransfer), w.pool.Slots(gitproc.ClassRead), memory)
	if got := int64(numberOf(t, l, "memory_reserved")); got != reserved {
		t.Errorf("the open kept %d bytes for the rest of the node, want %d for its pool's lanes", got, reserved)
	}
	want := max(0, memory-reserved)
	if got := int64(numberOf(t, l, "maintenance_budget")); got != want || c.BackgroundBudget() != want {
		t.Errorf("the budget is %d (logged %d), want the memory %d less %d kept, %d", c.BackgroundBudget(), got,
			memory, reserved, want)
	}
}

// A NODE OF MANY CORES AND MODEST MEMORY STILL MAINTAINS. A container with a
// memory limit and no CPU limit sees its host's every core, and kept for each
// of them, 64 cores with 16 GiB kept 112 GiB for its lanes and its serving,
// left its background work nothing and refused every repository — while its
// transfer lane admitted 256 transfers its memory could not hold. Counted as
// the cores its memory backs, it runs the transfers and keeps the memory of
// an eight-core node: 32 transfers, and 2 GiB for its maintenance.
//
// Mutation: keep ServingMemoryPerCore for every core in memoryReserve, and the
// budget is zero.
func TestANodeOfManyCoresAndModestMemoryStillMaintains(t *testing.T) {
	t.Parallel()
	w := newSyncWorld(t, onMemory)
	w.processes, w.memory = 64, 16<<30
	clk := newClock()
	repo := w.repo(t, 1)
	w.push(t, repo, "refs/heads/main", nil)
	c := w.open(t, clk.on)
	defer w.sync(t, c, repo).Release()
	if got := w.pool.Slots(gitproc.ClassTransfer); got != 32 {
		t.Errorf("64 cores and 16 GiB run %d transfers, want the 32 the memory backs", got)
	}
	if got, want := c.BackgroundBudget(), int64(2<<30); got != want {
		t.Errorf("64 cores and 16 GiB give the background work %d, want %d, an eight-core node's", got, want)
	}
	clk.Add(cache.MidxDebounce)
	if ran := cache.MaintainerPass(c); len(ran) != 1 {
		t.Errorf("the maintainer ran %v on 64 cores and 16 GiB, want the repository", ran)
	}
}

// THE SHAPE GIVES THE MAINTAINER ITS SHARE: a node of exactly CoreMemory a
// core, at the pool's default lanes, has MaintenanceMemoryPerCore a core left
// to its background work — the share every memory figure of the cache was
// sized with — and a node with less memory than that a core has it for each
// core its memory backs, never nothing; what it keeps is gitproc's transfer
// bound and the rest.
func TestTheMemoryShapeGivesTheMaintainerItsShare(t *testing.T) {
	t.Parallel()
	if got := int64(gitproc.TransfersPerProcess) * gitproc.TransferMemory; got != 896<<20 {
		t.Errorf("TransfersPerProcess × TransferMemory = %d, want gitproc's 896 MiB a core", got)
	}
	if cache.ServingMemoryPerCore != 896<<20 {
		t.Errorf("ServingMemoryPerCore = %d, want the 896 MiB a core CoreMemory leaves", cache.ServingMemoryPerCore)
	}
	for _, node := range []struct {
		cores        int
		memory       int64
		backedShares int64
	}{
		{1, gitproc.CoreMemory, 1}, {2, 2 * gitproc.CoreMemory, 2}, {4, 4 * gitproc.CoreMemory, 4},
		{16, 16 * gitproc.CoreMemory, 16}, {64, 64 * gitproc.CoreMemory, 64},
		{64, 16 << 30, 8}, {32, 4 << 30, 2}, {4, 2 << 30, 1},
	} {
		transfers := int(min(int64(gitproc.TransfersPerProcess*node.cores), node.memory/gitproc.TransferShare))
		left := node.memory - cache.MemoryReserve(transfers, node.cores, node.memory)
		if want := node.backedShares * cache.MaintenanceMemoryPerCore; left != want {
			t.Errorf("a node of %d cores and %d bytes at the default lanes leaves its maintenance %d, want "+
				"MaintenanceMemoryPerCore for each of the %d cores its memory backs, %d", node.cores, node.memory,
				left, node.backedShares, want)
		}
	}
}

// A BUDGET OF NOTHING IS STATED AS THE CACHE COMPUTES IT, AND SO IS WHAT
// CLEARS IT. The background work's budget is the node's memory less what Open
// keeps of it — gitproc.TransferMemory for every transfer the pool admits and
// ServingMemoryPerCore for every core the memory backs at gitproc.CoreMemory a
// core — never below zero (maintenanceBudget); every pass reads the memory
// again, and the reserve stays what Open fixed. A transfer lane the pool sized
// itself — git.max_transfers unset — leaves MaintenanceMemoryPerCore a backed
// core. An explicit git.max_transfers is kept as written, and it zeroes the
// budget long before its transfers take the whole memory: 42 of them on
// 16 GiB and eight cores, where 16 GiB holds 73 at 224 MiB each. The remedy
// called a budget of zero a memory lowered after the node booted, which a
// restart re-sizes the lanes to — so the operator who set that figure was
// sent to restart a node no restart changes, and a budget of zero was said to
// clear at a restart where memory given in place past what the node keeps
// clears it too.
//
// Here the smallest explicit lane that zeroes a 16 GiB, eight-core node's
// budget is found through the cache's own reserve: below what the memory holds
// at TransferMemory each, at the fraction of the memory the prose states, and
// one transfer fewer leaves a budget. Real caches then show what clears it: at
// that lane the budget is nothing; memory given in place past what Open kept
// gives it back at the next pass; a restart over the same memory leaves it
// nothing; and with the lane unset, a memory lowered in place to nothing is
// given back by a restart over the lowered memory. The remedy, the
// configuration page's entry for the setting and the concept page state those
// figures and those clears.
//
// Mutation: state another fraction, drop the serving reserve or the in-place
// clear from the remedy or a page, or name another lane there, and this fails;
// read the memory only at Open, and the in-place clear does.
func TestTheBitmapAlarmSaysWhatLeavesItsBudgetNothing(t *testing.T) {
	t.Parallel()
	const memory, cores = int64(16 << 30), 8
	// THE BOUNDARY, from the cache's own reserve.
	holds := int(memory / gitproc.TransferMemory)
	zeroing := 1
	for cache.MemoryReserve(zeroing, cores, memory) < memory {
		zeroing++
	}
	if zeroing >= holds {
		t.Fatalf("%d transfers zero a %d-byte, %d-core node's budget, no fewer than the %d its memory holds "+
			"at TransferMemory each", zeroing, memory, cores, holds)
	}
	if left := memory - cache.MemoryReserve(zeroing-1, cores, memory); left <= 0 {
		t.Fatalf("%d transfers already leave a %d-byte node nothing", zeroing-1, memory)
	}
	// THE FRACTION the prose states: where the memory backs no more than one
	// core for every CoreMemory of it, serving keeps ServingMemoryPerCore of
	// every CoreMemory, and the transfers zero the budget at the rest: the
	// lane found keeps at least that share of the memory, and one transfer
	// fewer less than a point more — the figure is the boundary, rounded down.
	percent := 100 * (gitproc.CoreMemory - cache.ServingMemoryPerCore) / gitproc.CoreMemory
	kept := int64(zeroing) * gitproc.TransferMemory
	if kept*100 < percent*memory || (kept-gitproc.TransferMemory)*100 >= (percent+1)*memory {
		t.Fatalf("%d transfers keep %d bytes of %d, which is not the %d%% the prose states", zeroing, kept,
			memory, percent)
	}

	// WHAT CLEARS IT, on real caches.
	w := newWorld(t)
	w.processes, w.transfers, w.memory = cores, zeroing, memory
	c := w.open(t)
	if got := c.BackgroundBudget(); got != 0 {
		t.Fatalf("%d transfers on %d bytes and %d cores leave a budget of %d, want nothing", zeroing, memory,
			cores, got)
	}
	resized := memory + 8<<30
	cache.SetNodeMemory(c, resized)
	cache.MaintainerPass(c)
	if got, want := c.BackgroundBudget(), resized-cache.MemoryReserved(c); want <= 0 || got != want {
		t.Errorf("the node given %d bytes in place has a budget of %d, want the %d past what Open kept",
			resized, got, want)
	}
	c.Stop(context.Background())
	c = w.open(t)
	if got := c.BackgroundBudget(); got != 0 {
		t.Errorf("a restart over the same memory and lane gave the background work %d, want nothing", got)
	}
	c.Stop(context.Background())

	w.transfers = 0
	c = w.open(t)
	if got, want := c.BackgroundBudget(), int64(cores)*cache.MaintenanceMemoryPerCore; got != want {
		t.Fatalf("an unset lane on %d bytes and %d cores leaves %d, want MaintenanceMemoryPerCore a core, %d",
			memory, cores, got, want)
	}
	lowered := memory / 2
	cache.SetNodeMemory(c, lowered)
	cache.MaintainerPass(c)
	if got := c.BackgroundBudget(); got != 0 {
		t.Fatalf("an unset lane sized for %d bytes, lowered in place to %d, leaves %d, want nothing", memory,
			lowered, got)
	}
	c.Stop(context.Background())
	w.memory = lowered
	if got := w.open(t).BackgroundBudget(); got <= 0 {
		t.Errorf("a restart over the lowered memory with the lane unset gave the background work %d, want "+
			"the lanes re-sized to leave a budget", got)
	}

	// THE PROSE, in the same figures.
	var remedy string
	for _, a := range statelog.Evaluate(statelog.Reading{CodeMaintenanceRefused: 1, CodeMaintenanceLargest: 1}) {
		if a.Kind == statelog.KindCodeMaintenanceRefused {
			remedy = a.Remedy
		}
	}
	if remedy == "" {
		t.Fatalf("%s did not fire over a bitmap charged past a budget of nothing", statelog.KindCodeMaintenanceRefused)
	}
	transfer, serving, core := amount(t, gitproc.TransferMemory), amount(t, cache.ServingMemoryPerCore),
		amount(t, gitproc.CoreMemory)
	for _, want := range []string{
		fmt.Sprintf("%s for every transfer its git lane admits", transfer),
		fmt.Sprintf("%s for each core its memory backs at %s a core", serving, core),
		fmt.Sprintf("no more than one for every %s, and leaves the background work %s for each backed core",
			amount(t, gitproc.TransferShare), amount(t, cache.MaintenanceMemoryPerCore)),
		fmt.Sprintf("once its transfers' %s each reach the memory less the cores' %s each: at as little as "+
			"%d%% of the transfers the memory holds at %s each, where the node has a core for every %s of it "+
			"or more", transfer, serving, percent, transfer, core),
		"clears when git.max_transfers is lowered or unset, or when the node is given memory past what it " +
			"keeps, in place or at a boot",
		"A restart alone keeps the same lanes over the same memory, and clears it only where " +
			"git.max_transfers is unset and the memory was lowered after the node booted",
	} {
		if !strings.Contains(remedy, want) {
			t.Errorf("the %s remedy does not say %q: %s", statelog.KindCodeMaintenanceRefused, want, remedy)
		}
	}
	for _, page := range []struct {
		path  string
		wants []string
	}{
		{filepath.Join("docs", "concepts", "configuration.md"), []string{
			fmt.Sprintf("keeps %s of its memory for every transfer it names whatever they serve, beside %s for "+
				"each core its memory backs at %s a core", transfer, serving, core),
			fmt.Sprintf("as little as %d%% of that, where the node has a core for every %s of memory or more: "+
				"on %s and %s cores, %d transfers, of the %d that %s holds", percent, core, amount(t, memory),
				word(t, cores), zeroing, holds, amount(t, memory)),
			"[`code_maintenance_refused`](../reference/alarms.md) says so until the figure is lowered or " +
				"unset, or the node is given memory past what it keeps, in place or at a boot. A restart alone " +
				"changes nothing.",
		}},
		{filepath.Join("docs", "concepts", "code-host.md"), []string{
			fmt.Sprintf("once its transfers' %s each reach the memory less the %s of each core it backs — as "+
				"little as %d%% of the transfers the memory holds at %s each, where the node has a core for "+
				"every %s or more", transfer, serving, percent, transfer, core),
			"Lowering or unsetting it, or memory past what the node keeps, gives the budget back; a restart " +
				"alone does not.",
			fmt.Sprintf("Give the node more memory, or — where `git.max_transfers` is set — fewer transfers, "+
				"each of which keeps %s of it.", transfer),
		}},
	} {
		path := filepath.Join(sourcetree.Root(t), page.path)
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		text := strings.Join(strings.Fields(string(raw)), " ")
		for _, want := range page.wants {
			if !strings.Contains(text, want) {
				t.Errorf("%s does not say %q", path, want)
			}
		}
	}
}

// THE PROSE NAMES THE CACHE'S OWN FIGURES: the alarm table and the metrics
// catalogue cannot import the cache (the cache imports them), so their prose
// states the bytes an object in words — every step's it names, and the
// transfer's beside them — and how often the node's memory is read again;
// and this holds the words to the constants, so a retune cannot leave the
// alarm reference or the metrics reference stale. A compaction's build is
// one of those steps, charged its own bytes an object of the packs it merges,
// a transfer's room by those packs — never by the whole repository — and its
// delta search a multiple of max_blob_bytes. An expunge's build is another,
// charged the compaction's figure an object of the packs it sorts
// (Cache.AdmitExpunge) with room for a clone of the WHOLE repository beside
// it, whose history its build walks — so its words are held to that constant
// too, since a retune of it once left them stating the old figure.
//
// Mutation: change IndexBytesPerObject, GraphBytesPerObject,
// TipGraphBytesPerObject, TipGraphBaseBytes, CompactBytesPerObject,
// CompactWindowBytes' multiple, PublishBytesPerObject or MidxDebounce, and the
// catalogue no longer says what the cache does.
func TestTheProseNamesTheCachesOwnFigures(t *testing.T) {
	t.Parallel()
	bitmap := fmt.Sprintf("%d bytes an object", cache.BitmapBytesPerObject)
	transfer := fmt.Sprintf("%d more", gitproc.TransferBytesPerObject)
	reread := "again every " + spell(t, cache.MidxDebounce)
	window := word(t, windowBlobs(t))
	publish := fmt.Sprintf("%d bytes an object of the repository", cache.PublishBytesPerObject)
	ref := statelog.AlarmReference()
	if strings.Count(ref, bitmap) < 2 || !strings.Contains(ref, transfer) || !strings.Contains(ref, reread) {
		t.Errorf("the alarm reference does not say %q in both the code_maintenance_refused alarm's meaning and its "+
			"remedy, %q of the transfer beside it, and that the memory is read %q", bitmap, transfer, reread)
	}
	if strings.Count(ref, publish) < 2 {
		t.Errorf("the alarm reference does not say %q of a publication the alarm counts, in both its meaning and "+
			"its remedy", publish)
	}
	shows := map[string]string{}
	for _, in := range metrics.Catalogue() {
		shows[in.Name] = in.Shows
	}
	for name, want := range map[string][]string{
		metrics.CodeCacheMaintenanceRefusals: {
			bitmap, transfer,
			fmt.Sprintf("`index` (%d)", cache.IndexBytesPerObject),
			fmt.Sprintf("`graph` (%d)", cache.GraphBytesPerObject),
			fmt.Sprintf("`tip-graph` (%d an object of the tail", cache.TipGraphBytesPerObject),
			fmt.Sprintf("and %s whatever its length", amount(t, cache.TipGraphBaseBytes)),
			fmt.Sprintf("`compact` (%d bytes an object of the packs merged, beside a transfer's room of them — %d "+
				"bytes an object merged, never of the whole repository, whose size alone would refuse every merge "+
				"of a large one — and %s times max_blob_bytes for the build's delta search)",
				cache.CompactBytesPerObject, gitproc.TransferBytesPerObject, window),
			fmt.Sprintf("`publish` (%d bytes an object of the repository", cache.PublishBytesPerObject),
			fmt.Sprintf("`expunge` (%d bytes an object of the packs an expunge sorts, beside the room for a clone "+
				"of the whole repository, whose history its build walks", cache.CompactBytesPerObject),
		},
		metrics.CodeCacheMaintenanceRefused: {reread},
		// A publication refused for memory is the cache's refusal, counted
		// by the compactor too: its reason's charge is the cache's figure.
		metrics.CodePublishRefusals: {publish},
	} {
		text, ok := shows[name]
		if !ok {
			t.Errorf("the metrics catalogue has no %s", name)
			continue
		}
		for _, w := range want {
			if !strings.Contains(text, w) {
				t.Errorf("%s's description does not say %q", name, w)
			}
		}
	}
}

// THE CONCEPT PAGE SIZES A GIT NODE BY THE CACHE'S OWN FIGURES: an operator
// sizes a git node's memory from what docs/concepts/code-host.md says each
// git process holds an object of its repository, what the node keeps back of
// its memory, and what that leaves its background work on the shapes the page
// works through — and reads there when the background indexing runs. Every
// one is a constant here or in gitproc, or arithmetic over them, so this
// holds each figure the page states to the constant it spells and computes
// every worked example through the cache's own reserve at the pool's default
// lanes: a retune that left the page behind fails here rather than sending
// somebody to buy a node too small.
//
// Mutation: change any constant the page names — BitmapBytesPerObject,
// FsckBytesPerObject, gitproc.TransferMemory, MaintenanceMemoryPerCore,
// MidxPacks, TipGraphBytesPerObject, TipGraphBaseBytes, MaxNewFilters,
// CompactBytesPerObject, CompactWindowBytes' multiple — and the page no
// longer says what the cache does.
func TestTheConceptPageSizesAGitNodeByTheCachesOwnFigures(t *testing.T) {
	t.Parallel()
	path := filepath.Join(sourcetree.Root(t), "docs", "concepts", "code-host.md")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	page := strings.Join(strings.Fields(string(raw)), " ")
	bitmap := int64(cache.BitmapBytesPerObject + gitproc.TransferBytesPerObject)
	index := int64(cache.IndexBytesPerObject + gitproc.TransferBytesPerObject)
	// left is what a node of cores and memory leaves its background work at
	// the pool's default lanes, and how many transfers those lanes admit.
	left := func(cores int, memory int64) (budget int64, transfers int) {
		transfers = int(min(int64(gitproc.TransfersPerProcess*cores), memory/gitproc.TransferShare))
		return memory - cache.MemoryReserve(transfers, cores, memory), transfers
	}
	shape, _ := left(4, 4*gitproc.CoreMemory)
	large, _ := left(4, 16<<30)
	_, wide := left(64, 16<<30)
	if cache.MaintenanceMemoryPerCore != 1<<30/4 {
		t.Fatalf("the concept page calls MaintenanceMemoryPerCore (%d) a quarter of a gibibyte a core: say "+
			"there what it is, and here", cache.MaintenanceMemoryPerCore)
	}
	const share = "a quarter of a gibibyte"
	for _, want := range []string{
		fmt.Sprintf("needs about %s bytes of memory for every object of a repository to write its bitmap",
			spaced(cache.BitmapBytesPerObject)),
		fmt.Sprintf("about %s bytes an object for the bitmap", spaced(cache.BitmapBytesPerObject)),
		fmt.Sprintf("%d for the index without one, %d for the commit-graph, %d for `crewlet code verify`'s "+
			"connectivity check, and a clone about %d more than the %s its git transfer slot keeps for it",
			cache.IndexBytesPerObject, cache.GraphBytesPerObject, cache.FsckBytesPerObject,
			gitproc.TransferBytesPerObject, amount(t, gitproc.TransferMemory)),
		fmt.Sprintf("keeps %s of it for every git transfer it admits at once and %s for each core its git runs on",
			amount(t, gitproc.TransferMemory), amount(t, cache.ServingMemoryPerCore)),
		fmt.Sprintf("the cores the memory backs at %s a core", amount(t, gitproc.CoreMemory)),
		fmt.Sprintf("a node of 64 cores and 16 GiB admits %d transfers rather than %d, and runs as the %s-core node "+
			"its memory makes it", wide, gitproc.TransfersPerProcess*64, word(t, int((16<<30)/gitproc.CoreMemory))),
		fmt.Sprintf("about %s bytes an object, with the clone beside it", spaced(bitmap)),
		fmt.Sprintf("A node of %s a core leaves %s a core (%s on four cores: bitmaps of repositories of up to "+
			"about %s objects, indexes of up to about %s); 16 GiB on four cores leaves %s: bitmaps of up to about "+
			"%s objects, indexes of up to about %s.", amount(t, gitproc.CoreMemory), share, amount(t, shape),
			atMost(shape/bitmap), atMost(shape/index), amount(t, large), atMost(large/bitmap), atMost(large/index)),
		fmt.Sprintf("as many run at once as %s a core holds beside the largest", share),
		fmt.Sprintf("%s a core and no more than one for each %s of its memory, each kept %s",
			word(t, gitproc.TransfersPerProcess), amount(t, gitproc.TransferShare), amount(t, gitproc.TransferMemory)),
		fmt.Sprintf("about %d bytes an object of the repository, past the %s",
			gitproc.TransferBytesPerObject, amount(t, gitproc.TransferMemory)),
		fmt.Sprintf("it reads it again every %s", spell(t, cache.MidxDebounce)),
		fmt.Sprintf("packs have stood still for %s — or the oldest has waited %s,",
			spell(t, cache.MidxDebounce), spell(t, cache.MidxMaxWait)),
		fmt.Sprintf("once %d more packs have gathered beside them, at most every %s and at most a %s of a core's "+
			"time", cache.MidxPacks, spell(t, cache.MidxMinInterval), fraction(t, cache.MidxDutyCycle)),
		fmt.Sprintf("until %d packs have gathered outside it", cache.MidxRoundPacks),
		fmt.Sprintf("holding %d packs or more gets an index", cache.MidxRoundPacks),
		fmt.Sprintf("charged by the tail rather than the repository: %s bytes an object of the tail and %s "+
			"whatever its length", spaced(cache.TipGraphBytesPerObject), amount(t, cache.TipGraphBaseBytes)),
		fmt.Sprintf("it is charged the largest of theirs: %s bytes an object of the whole repository for its bitmap",
			spaced(cache.PublishBytesPerObject)),
		fmt.Sprintf("%s an object of the layer for its commit-graph layer", spaced(cache.GraphLayerBytesPerObject)),
		fmt.Sprintf("One write computes at most %s new filters", spaced(cache.MaxNewFilters)),
		// A COMPACTION'S BUILD, charged as its admission charges it, in the
		// section that runs it and in the Memory paragraph beside every
		// other step.
		fmt.Sprintf("(%d bytes an object merged, %d more an object merged for a transfer's room beside them, "+
			"and %s times `code.limits.max_blob_bytes` for its delta search",
			cache.CompactBytesPerObject, gitproc.TransferBytesPerObject, word(t, windowBlobs(t))),
		fmt.Sprintf("and is charged by them: %d bytes an object merged, %d more an object merged for a "+
			"transfer's room beside them, and %s times `code.limits.max_blob_bytes` for its delta search",
			cache.CompactBytesPerObject, gitproc.TransferBytesPerObject, word(t, windowBlobs(t))),
	} {
		if !strings.Contains(page, want) {
			t.Errorf("%s does not say %q", path, want)
		}
	}
}

// windowBlobs is how many of the epoch's largest blob a compaction's build
// is charged for its delta search (cache.CompactWindowBytes), the multiple
// the docs spell: a window that is no whole multiple of max_blob_bytes is
// one they cannot.
func windowBlobs(t *testing.T) int {
	t.Helper()
	const blob = 1 << 20
	w := cache.CompactWindowBytes(code.Limits{MaxBlobBytes: blob})
	if w%blob != 0 {
		t.Fatalf("CompactWindowBytes is %d bytes at a max_blob_bytes of %d, no whole multiple of it: say in the "+
			"docs how it is written, and here", w, blob)
	}
	return int(w / blob)
}

// THE SCALING PAGE SIZES A GIT NODE BY THE SAME FIGURES: docs/concepts/scaling.md
// tells an operator that a git node is sized by its memory before its disk,
// in the bytes an object its bitmap and a clone beside it hold, and what a
// repository's maintainer is charged beside them for its compaction and its
// publication, and sends them to the concept page for everything else — so
// the figures it does state are the cache's own and gitproc's, held here as
// the concept page's are above, or a retune would leave one page saying what
// the other no longer does.
//
// Mutation: change BitmapBytesPerObject, PublishBytesPerObject,
// gitproc.TransferBytesPerObject, CompactBytesPerObject or CompactWindowBytes'
// multiple, and the scaling page no longer says what the cache charges.
func TestTheScalingPageSizesAGitNodeByTheCachesOwnFigures(t *testing.T) {
	t.Parallel()
	path := filepath.Join(sourcetree.Root(t), "docs", "concepts", "scaling.md")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	page := strings.Join(strings.Fields(string(raw)), " ")
	for _, want := range []string{
		fmt.Sprintf("writing the bitmap of its background index takes about %s bytes an object, "+
			"and a clone beside it about %d more — about %s an object",
			spaced(cache.BitmapBytesPerObject), gitproc.TransferBytesPerObject,
			spaced(int64(cache.BitmapBytesPerObject+gitproc.TransferBytesPerObject))),
		fmt.Sprintf("about %d bytes an object it merges, %d more an object merged for a transfer's room beside "+
			"them and %s times `code.limits.max_blob_bytes` for its delta search",
			cache.CompactBytesPerObject, gitproc.TransferBytesPerObject, word(t, windowBlobs(t))),
	} {
		if !strings.Contains(page, want) {
			t.Errorf("%s does not say %q", path, want)
		}
	}
	// THE PUBLICATION'S CHARGE, which places a repository's maintainer.
	publish := fmt.Sprintf("its bitmap at %s bytes an object of the whole repository and a clone's %d beside it, "+
		"%s bytes an object", spaced(cache.PublishBytesPerObject), gitproc.TransferBytesPerObject,
		spaced(int64(cache.PublishBytesPerObject+gitproc.TransferBytesPerObject)))
	if !strings.Contains(page, publish) {
		t.Errorf("%s does not say %q", path, publish)
	}
}

// spaced is n as the docs write a number: its thousands set apart by a space
// (1 000, 860 000).
func spaced(n int64) string {
	s := strconv.FormatInt(n, 10)
	for i := len(s) - 3; i > 0; i -= 3 {
		s = s[:i] + " " + s[i:]
	}
	return s
}

// atMost is a count of objects a memory holds as the docs write it: cut DOWN
// to two significant figures, because "up to" a count must be one the memory
// holds (7 793 287 is 7.7 million, never 7.8), in millions from a million
// (3.3 million, 30 million) and spaced below one (860 000).
func atMost(n int64) string {
	unit := int64(1)
	for n/unit >= 100 {
		unit *= 10
	}
	n = n / unit * unit
	switch whole, tenths := n/1_000_000, n%1_000_000/100_000; {
	case whole == 0:
		return spaced(n)
	case tenths == 0:
		return fmt.Sprintf("%d million", whole)
	default:
		return fmt.Sprintf("%d.%d million", whole, tenths)
	}
}

// amount is a number of bytes as the docs write it: in gibibytes where it is
// whole ones, else in mebibytes.
func amount(t *testing.T, n int64) string {
	t.Helper()
	switch {
	case n%(1<<30) == 0:
		return fmt.Sprintf("%d GiB", n>>30)
	case n%(1<<20) == 0:
		return fmt.Sprintf("%d MiB", n>>20)
	}
	t.Fatalf("%d bytes is no whole mebibyte: say in the concept page how it is written, and here", n)
	return ""
}

// words are the numbers the docs spell rather than write.
var words = []string{"zero", "one", "two", "three", "four", "five", "six", "seven", "eight", "nine", "ten",
	"eleven", "twelve"}

// word is n as the docs spell it.
func word(t *testing.T, n int) string {
	t.Helper()
	if n < 0 || n >= len(words) {
		t.Fatalf("%d is no number the docs spell: write it in the concept page as a figure, and here", n)
	}
	return words[n]
}

// spell is a duration as the docs spell it: ten seconds, ten minutes.
func spell(t *testing.T, d time.Duration) string {
	t.Helper()
	for _, unit := range []struct {
		d           time.Duration
		one, plural string
	}{{time.Hour, "an hour", "hours"}, {time.Minute, "a minute", "minutes"}, {time.Second, "a second", "seconds"}} {
		if d < unit.d || d%unit.d != 0 {
			continue
		}
		if n := int(d / unit.d); n != 1 {
			return word(t, n) + " " + unit.plural
		}
		return unit.one
	}
	t.Fatalf("%v is no whole number of seconds: say in the concept page how it is written, and here", d)
	return ""
}

// fraction is one over n as the docs spell it: a tenth.
func fraction(t *testing.T, n int) string {
	t.Helper()
	spelled := map[int]string{2: "half", 3: "third", 4: "quarter", 5: "fifth", 6: "sixth", 7: "seventh",
		8: "eighth", 9: "ninth", 10: "tenth", 11: "eleventh", 12: "twelfth"}
	s, ok := spelled[n]
	if !ok {
		t.Fatalf("one over %d is no fraction the docs spell: write it in the concept page, and here", n)
	}
	return s
}

// numberOf is a logged line's numeric field.
func numberOf(t *testing.T, line map[string]any, field string) float64 {
	t.Helper()
	n, ok := line[field].(float64)
	if !ok {
		t.Fatalf("the line's %s is %v, not a number", field, line[field])
	}
	return n
}
