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

	"github.com/crewlet/crewlet/internal/gitlet"
	"github.com/crewlet/crewlet/internal/gitlet/cache"
	"github.com/crewlet/crewlet/internal/gitlet/gitproc"
	enginenode "github.com/crewlet/crewlet/internal/node"
	"github.com/crewlet/crewlet/internal/queue"
	"github.com/crewlet/crewlet/internal/sandbox/executor"
	"github.com/crewlet/crewlet/internal/sourcetree"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/statelog/metrics"
	"github.com/crewlet/crewlet/internal/store"
)

// A BITMAP THE NODE'S MEMORY CANNOT HOLD IS REPLACED BY AN INDEX WITHOUT ONE.
// A bitmap holds about BitmapBytesPerObject an object, and one past the
// background budget gets git, or the engine, killed by the kernel. So it is
// never started: the repository is indexed without a bitmap (graph written),
// and the refusal is logged and counted once an incarnation, gauged while it
// stands, and raises code_maintenance_refused. At a budget of the bitmap's
// charge it is written, and the gauge and the alarm clear.
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
	c := w.open(t, clk.on, cache.NoFixedParts())
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
// refused its commit-graph walks history by date (a push of a commit dated
// before its parent walks every commit), so the refusal is logged, counted
// once an incarnation and gauged under its own step beside the bitmap's; the
// alarm counts the repository refused both once. At the graph's charge the
// graph is written first, and its gauge falls back.
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
	// Short of the bitmap too, the cheaper here: the graph's fixed part
	// (GraphBaseBytes) outweighs a bitmap of so few objects.
	cache.SetMaintenanceBudget(c, min(graph, cache.BitmapCharge(c, repo))-1)
	for range 2 {
		if ran := cache.MaintainerPass(c); len(ran) != 0 {
			t.Fatalf("on a budget a byte short of the graph's charge and the bitmap's the maintainer ran %v", ran)
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
// on an installed chain writes only its tip over the tail, charged
// TipGraphBytesPerObject an object of the tail plus GraphBaseBytes, beside a
// whole-repository clone's room (Write.charge). Refused, it is logged, counted
// once an incarnation and gauged under its own step, but raises no alarm: the
// chain's bitmap and graph layers serve the copy, and only the tail walks by
// date. At its own charge it is written, and the gauge falls back.
//
// Mutations: count no refused tip in refusalWalk.note, and its gauge stays
// zero; count it with the bitmaps, and the alarm names it; charge it by the
// repository's objects (plan's tip case dropped), and its charge runs
// nothing; drop Write.charge's BaseBytes, and a byte short runs it.
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
	if repoWide := objects*cache.TipGraphBytesPerObject + cache.GraphBaseBytes + room; tip >= repoWide ||
		tip <= cache.GraphBaseBytes+room {
		t.Fatalf("the tip's charge is %d over a repository of %d objects: want its figure over the tail alone, "+
			"under %d, beside its fixed part and the clone's room over the whole repository, %d", tip, objects,
			repoWide, cache.GraphBaseBytes+room)
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

// A REFUSED REPOSITORY KEEPS ITS PUSHES INDEXED. A repository refused its
// bitmap is often the most pushed-to, and each pack outside an index is one
// more .idx every lookup searches, so the index without a bitmap is rewritten
// at the usual cadence (MidxPacks beside it) over every pack since.
//
// The budget is reset a byte short of the bitmap's charge AS THE COPY STANDS
// after each push, so the bitmap stays refused while the far cheaper index
// and graph fit; a budget fixed at the first size would refuse the index too
// once the repository grew.
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
	c := w.open(t, clk.on, cache.MidxPacksAt(2), cache.NoFixedParts())
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
// that outgrew the node's memory keeps the bitmap written while it fit; an
// index without a bitmap would delete it (git removes a bitmap not matching
// the index it writes), and clones would walk the whole history. So it stands
// while fewer than MidxRoundPacks (the most served outside an index anywhere)
// are outside it; past them, the index without a bitmap replaces it. The
// budget follows the copy as in TestARefusedRepositoryKeepsItsPushesIndexed.
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
	c := w.open(t, clk.on, cache.MidxPacksAt(2), cache.NoFixedParts())
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

// EACH STEP IS ADMITTED ON ITS OWN CHARGE. A run's steps run one after
// another, each holding its own memory, so a budget of the small repository's
// bitmap (its graph costs more: GraphBaseBytes outweighs a bitmap of few
// objects) writes the index with its bitmap alone, a repository short of
// every step runs nothing, and each refused step is counted and logged once.
//
// Mutation: refuse a run whole when its graph is refused, and the small
// repository's index is never written.
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
	bitmap := cache.BitmapCharge(c, small)
	if cache.GraphCharge(c, small) <= bitmap || cache.BitmapCharge(c, large) <= bitmap ||
		cache.IndexCharge(c, large) <= bitmap {
		t.Fatalf("charges: the small repository's graph %d and the large one's bitmap %d and index %d, want each "+
			"past the small one's bitmap %d", cache.GraphCharge(c, small), cache.BitmapCharge(c, large),
			cache.IndexCharge(c, large), bitmap)
	}
	cache.SetMaintenanceBudget(c, bitmap)
	clk.Add(cache.MidxDebounce)

	if ran := cache.MaintainerPass(c); len(ran) != 1 || ran[0] != small {
		t.Fatalf("the pass ran %v, want the small repository's bitmap alone", ran)
	}
	if packs, withBitmap, written := cache.Indexed(c, small); packs != 1 || !withBitmap || written {
		t.Errorf("the small repository holds an index over %d packs (bitmap %t) and graph %t, want its one pack "+
			"indexed with a bitmap, and no graph", packs, withBitmap, written)
	}
	if packs, _, written := cache.Indexed(c, large); packs != 0 || written {
		t.Errorf("the large repository holds an index over %d packs and graph %v, want nothing", packs, written)
	}
	for step, want := range map[string]uint64{"bitmap": 1, "index": 1, "graph": 2} {
		if got := counter(w.rec, metrics.CodeCacheMaintenanceRefusals, "step", step); got != want {
			t.Errorf("%s{step=%s} = %d, want %d", metrics.CodeCacheMaintenanceRefusals, step, got, want)
		}
	}
	if got := len(w.logs.lines(t, "code_cache_maintenance_refused")); got != 4 {
		t.Errorf("the refusals were logged %d times, want one a step refused of each repository, 4", got)
	}
}

// A FULL ROUND'S INDEX IS HELD TO THE NODE'S MEMORY TOO. It costs
// IndexBytesPerObject an object (gigabytes at the largest repository the
// defaults admit) and is written in the READ lane before a request is
// answered. Past the budget it is refused, said once an incarnation, and the
// round answers without it; at the budget it is written; with no room left by
// the work in flight, the round never waits: it answers without it, logged.
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
		return objects*(cache.IndexBytesPerObject+gitproc.TransferBytesPerObject) + cache.IndexBaseBytes
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
// the budget alone but not beside the runs in flight waits; were smaller
// repositories behind it let in as room drained, a steady stream of them would
// starve it. So nothing starts behind it that would leave it short of its
// charge once the runs ahead end, and it starts as soon as they have.
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
	c := w.open(t, clk.on, cache.NoFixedParts(), func(o *cache.Options) {
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

// THE RUNS IN FLIGHT FIT THE BUDGET TOGETHER: two that each fit alone can
// still get the node killed side by side. So even with room in the lane and
// the memory, the second waits for the first, held back rather than refused:
// it runs at the next pass after the first ends and is never counted refused.
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
	c := w.open(t, clk.on, cache.NoFixedParts(), func(o *cache.Options) {
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

// THE MEMORY IS READ AGAIN AT EVERY PASS: a container's limit can change in
// place (Kubernetes resizes a pod without a restart), and a budget read once
// would keep admitting runs past a lowered limit. Each pass rereads it and
// moves the budget, logged; a failed read keeps the last reading, logged once.
//
// Mutation: read the memory only at Open, and the pass after a resize writes a
// bitmap the new limit cannot hold.
func TestTheMemoryIsReadAgainAtEveryPass(t *testing.T) {
	t.Parallel()
	w := newSyncWorld(t, onMemory)
	clk := newClock()
	repo := w.repo(t, 1)
	w.push(t, repo, "refs/heads/main", nil)
	c := w.open(t, clk.on, cache.NoFixedParts())
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
// connectivity check holds FsckBytesPerObject an object (some 11 GB at the
// largest repository the defaults admit), so it shares the background budget:
// past it alone it is ErrNoMemory on this node, and with no room left by the
// work in flight it is busy, as a full lane is.
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

// OPEN HOLDS THE BACKGROUND WORK TO THE NODE'S MEMORY: the budget is the
// machine's memory as the pool reads it, less TransferMemory a transfer the
// lane admits and ServingMemoryPerCore a core the memory backs, never below
// zero, and the boot's line logs all four.
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
	reserved := cache.MemoryReserve(w.pool.Slots(gitproc.ClassTransfer), w.pool.Slots(gitproc.ClassRead), memory, 0)
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
// memory limit but no CPU limit sees every host core; reserving for each, 64
// cores and 16 GiB would keep 112 GiB, leave the background nothing and admit
// 256 transfers it cannot hold. Counting only the cores its memory backs, it
// runs as an eight-core node: 32 transfers, 2 GiB for maintenance.
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

// THE SHAPE GIVES THE MAINTAINER ITS SHARE: at the pool's default lanes a
// node of CoreMemory a core leaves MaintenanceMemoryPerCore a core to its
// background work (the share every memory figure here was sized with), and
// one with less leaves it for each core its memory backs, never nothing.
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
		transfers := lanesOf(t, node.cores, node.memory, nil)
		left := node.memory - cache.MemoryReserve(transfers, node.cores, node.memory, 0)
		if want := node.backedShares * cache.MaintenanceMemoryPerCore; left != want {
			t.Errorf("a node of %d cores and %d bytes at the default lanes leaves its maintenance %d, want "+
				"MaintenanceMemoryPerCore for each of the %d cores its memory backs, %d", node.cores, node.memory,
				left, node.backedShares, want)
		}
	}
}

// A NODE RUNNING SEATS SIZES GITLET BY WHAT THE ENGINE'S POOLS LEAVE: with
// gitproc.Options.Reserve keeping them, a node gives its transfers, its
// serving share and its maintenance exactly what a node of the memory left
// would, so the two worst cases fit the node together, and the budget holds
// the pools out beside them. Without a reserve the node is sized as before.
//
// Mutations, each red: drop the pools from memoryReserve; size the serving
// share or the maintenance share by the whole memory.
func TestTheEnginesPoolsAreKeptBeforeGitletsShares(t *testing.T) {
	t.Parallel()
	for _, node := range []struct {
		cores  int
		memory int64
	}{{1, 2 << 30}, {2, 4 << 30}, {4, 8 << 30}, {8, 16 << 30}} {
		sized := func(memory, reserve int64) (budget, maintenance, reserved int64, transfers int) {
			w := newWorld(t)
			w.processes, w.memory, w.reserve = node.cores, memory, fixed(reserve)
			c := w.open(t)
			return c.BackgroundBudget(), cache.MaintenanceMemory(c), cache.MemoryReserved(c),
				w.pool.Slots(gitproc.ClassTransfer)
		}
		pools := executorPools(true, node.memory)
		budget, maintenance, reserved, transfers := sized(node.memory, pools)
		leftBudget, leftMaintenance, leftReserved, leftTransfers := sized(node.memory-pools, 0)
		if transfers != leftTransfers || maintenance != leftMaintenance || budget != leftBudget ||
			reserved != leftReserved+pools {
			t.Errorf("%d cores and %d bytes beside %d bytes of pools: %d transfers, maintenance %d, budget %d, "+
				"keeping %d; want a node of the %d bytes left's %d, %d, %d, and its %d kept with the pools",
				node.cores, node.memory, pools, transfers, maintenance, budget, reserved, node.memory-pools,
				leftTransfers, leftMaintenance, leftBudget, leftReserved)
		}
		alone, _, _, aloneTransfers := sized(node.memory, 0)
		if alone != node.memory-cache.MemoryReserve(aloneTransfers, node.cores, node.memory, 0) ||
			aloneTransfers != gitproc.TransfersPerProcess*node.cores || alone <= budget {
			t.Errorf("%d cores and %d bytes without seats: budget %d, %d transfers; want the node's own "+
				"sizing, %d transfers a core, above the %d it gives beside the pools", node.cores, node.memory,
				alone, aloneTransfers, gitproc.TransfersPerProcess, budget)
		}
	}
}

// A NODE SHORT OF WHAT IT KEEPS BOOTS AND SAYS SO ONCE, WITH WHAT HOLDS IT:
// 2 GiB on one core running seats beside Gitlet, or with gitlet.max_transfers
// set past the serving share's cap (beside a page cache growing with the
// memory too), keeps more than its memory. Its open is not refused; it logs
// code_cache_memory_short once at WARN with the figures and memory_needed, and
// no pass after logs it again. Booted at memory_needed it is quiet, and one
// byte below it logs once, short by that byte. A node whose pools fit logs none.
//
// Mutations, each red: never log; log at INFO, whatever the memory, at no
// shortfall or only past a margin; drop memory_from or memory_needed; name the
// pools' excess or the whole reserve as the shortfall; log on a pass's reread.
func TestANodeShortOfWhatItKeepsSaysSoOnceAtBoot(t *testing.T) {
	t.Parallel()
	memory := gitproc.CoreMemory
	boot := func(memory int64, transfers int, reserve func(int64) int64) (*world, []map[string]any) {
		w := newWorld(t)
		w.processes, w.memory, w.transfers, w.reserve = 1, memory, transfers, reserve
		c := w.open(t)
		cache.SetNodeMemory(c, memory-1)
		cache.MaintainerPass(c)
		return w, w.logs.lines(t, "code_cache_memory_short")
	}
	for _, node := range []struct {
		name      string
		transfers int
		reserve   func(int64) int64
	}{
		{"running seats at the default lanes", 0, fixed(executorPools(true, memory))},
		{"with gitlet.max_transfers set past the serving share's cap", 6, fixed(executorPools(false, memory))},
		{"with gitlet.max_transfers set past it beside its store's page cache", 6, gitletKeeps(t, false, 1)},
	} {
		w, lines := boot(memory, node.transfers, node.reserve)
		transfers, pools := w.pool.Slots(gitproc.ClassTransfer), node.reserve(memory)
		kept := cache.MemoryReserve(transfers, 1, memory, pools)
		if kept <= memory || pools >= memory {
			t.Fatalf("the premise: a %d-byte node %s keeps %d bytes beside %d of pools, which is no more than "+
				"its memory, or its pools alone are not within it", memory, node.name, kept, pools)
		}
		if len(lines) != 1 {
			t.Fatalf("a node %s keeping %d bytes of %d logged code_cache_memory_short %d times across its boot "+
				"and a pass, want once", node.name, kept, memory, len(lines))
		}
		needed := int64(numberOf(t, lines[0], "memory_needed"))
		for key, want := range map[string]int64{"memory": memory, "pools": pools,
			"transfer_slots": int64(transfers), "memory_reserved": kept, "shortfall": kept - memory} {
			if got := int64(numberOf(t, lines[0], key)); got != want {
				t.Errorf("a node %s: code_cache_memory_short says %s %d, want %d", node.name, key, got, want)
			}
		}
		from, _ := lines[0]["memory_from"].(string)
		if level := lines[0]["level"]; level != "WARN" || from == "" {
			t.Errorf("a node %s: code_cache_memory_short is logged at %v naming memory_from %q, want WARN "+
				"naming where the memory was read", node.name, level, from)
		}
		holdsFrom(t, needed, transfers, 1, node.reserve)
		if _, lines := boot(needed, node.transfers, node.reserve); len(lines) != 0 {
			t.Errorf("a node %s booted at the memory_needed its line named, %d bytes, logged "+
				"code_cache_memory_short %d times: %v", node.name, needed, len(lines), lines)
		}
		w, lines = boot(needed-1, node.transfers, node.reserve)
		if len(lines) != 1 {
			t.Fatalf("a node %s booted one byte short of memory_needed logged code_cache_memory_short %d "+
				"times, want once", node.name, len(lines))
		}
		if got := w.pool.Slots(gitproc.ClassTransfer); got != transfers {
			t.Fatalf("a node %s booted one byte short of memory_needed has %d transfers, not the %d it was "+
				"named for", node.name, got, transfers)
		}
		for key, want := range map[string]int64{"shortfall": 1, "memory_needed": needed} {
			if got := int64(numberOf(t, lines[0], key)); got != want {
				t.Errorf("a node %s one byte short of memory_needed: code_cache_memory_short says %s %d, want %d",
					node.name, key, got, want)
			}
		}
	}
	if _, lines := boot(memory, 0, fixed(executorPools(false, memory))); len(lines) != 0 {
		t.Errorf("a %d-byte node keeping its %d bytes of pools within its memory logged code_cache_memory_short",
			memory, executorPools(false, memory))
	}
}

// THE MEMORY A SHORT NODE IS TOLD TO HAVE HOLDS WHAT IT KEEPS, AND NO LESS
// DOES: the serving share and the store's page cache grow with the memory, so
// the figure is where they meet, checked through MemoryReserve at every lane on
// up to eight cores, beside no pools, the transcript reads' and a node running
// seats', each with its page cache and without, both where the share's cap
// comes first and where it does not.
//
// Mutations, each red: need a step more or less; drop the share's cap; size
// what the node keeps at the memory it booted with.
func TestTheMemoryAShortNodeIsToldToHaveHoldsWhatItKeeps(t *testing.T) {
	t.Parallel()
	for _, cores := range []int{1, 2, 4, 8} {
		for _, engine := range []func(int64) int64{fixed(0), fixed(executorPools(false, 0)),
			fixed(executorPools(true, 0)), gitletKeeps(t, false, cores), gitletKeeps(t, true, cores)} {
			for transfers := 1; transfers <= 48; transfers++ {
				holdsFrom(t, cache.MemoryNeeded(transfers, cores, engine), transfers, cores, engine)
			}
		}
	}
}

// holdsFrom fails t unless needed is the least memory at and past which a
// node of transfers transfers and cores read slots, keeping engine(memory)
// beside Gitlet, holds what it keeps (MemoryReserve): within needed, and at
// the start of each of the serving share's next 4 096 steps of what engine
// leaves, where a node is nearest short; past needed less one byte.
func holdsFrom(t *testing.T, needed int64, transfers, cores int, engine func(int64) int64) {
	t.Helper()
	fits := func(memory int64) bool {
		return cache.MemoryReserve(transfers, cores, memory, engine(memory)) <= memory
	}
	if !fits(needed) || fits(needed-1) {
		t.Errorf("%d bytes is not the least memory that holds what %d transfers on %d cores beside %d bytes "+
			"of pools keep: %d holds it %v, one byte less %v", needed, transfers, cores, engine(needed), needed,
			fits(needed), fits(needed-1))
		return
	}
	step, left := gitproc.CoreMemory>>20, needed-engine(needed)
	for q := left/step + 1; q <= left/step+4096; q++ {
		// The least memory that leaves q steps past engine: engine grows more
		// slowly than the memory, so each shortfall added comes nearer it.
		memory := q*step + engine(q*step)
		for memory-engine(memory) < q*step {
			memory += q*step - (memory - engine(memory))
		}
		if !fits(memory) {
			t.Errorf("%d transfers on %d cores beside their pools are held at %d bytes but not at %d",
				transfers, cores, needed, memory)
			return
		}
	}
}

// fixed is a reserve of n bytes whatever the memory.
func fixed(n int64) func(int64) int64 { return func(int64) int64 { return n } }

// gitletKeeps is what a Gitlet node of cores keeps beside Gitlet of a node of
// memory bytes, as the engine passes it: the executor's pools (executorPools)
// and the store's page cache over every connection of its two files at the
// default readers, its replicated estate's writers one for each of the state
// logs the concept page names (internal/engine holds the page to its register).
func gitletKeeps(t *testing.T, seats bool, cores int) func(int64) int64 {
	t.Helper()
	conns := store.Options{MaxOpenConns: max(store.MinReaderConns, cores)}.NodeConnections(stateLogs(t))
	return func(memory int64) int64 { return executorPools(seats, memory) + store.PageCacheShare(memory, conns) }
}

// stateLogs is how many state logs docs/concepts/gitlet.md says a Gitlet node's
// replicated estate pins a writer for.
func stateLogs(t *testing.T) int {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(sourcetree.Root(t), "docs", "concepts", "gitlet.md"))
	if err != nil {
		t.Fatal(err)
	}
	const says = "one pinned writer for each of its "
	text := strings.Join(strings.Fields(string(raw)), " ")
	i := strings.Index(text, says)
	if i < 0 {
		t.Fatalf("docs/concepts/gitlet.md no longer says %q, which sizes a Gitlet node's page cache", says)
	}
	n, err := strconv.Atoi(strings.Fields(text[i+len(says):])[0])
	if err != nil {
		t.Fatalf("docs/concepts/gitlet.md says %q and no number of state logs: %v", says, err)
	}
	return n
}

// A BUDGET OF NOTHING IS STATED AS THE CACHE COMPUTES IT, AND SO IS WHAT
// CLEARS IT. The budget is the memory less Open's fixed reserve
// (maintenanceBudget), reread every pass. An explicit gitlet.max_transfers
// zeroes it well before its transfers fill memory (42 on 16 GiB and eight
// cores, which hold 73); lowering the lane or adding memory, in place or at a
// boot, clears it, and a restart alone only with the lane unset after memory
// was lowered. This finds that lane through MemoryReserve, drives real caches
// through each clear, and holds the remedy and both docs pages to them.
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
	for cache.MemoryReserve(zeroing, cores, memory, 0) < memory {
		zeroing++
	}
	if zeroing >= holds {
		t.Fatalf("%d transfers zero a %d-byte, %d-core node's budget, no fewer than the %d its memory holds "+
			"at TransferMemory each", zeroing, memory, cores, holds)
	}
	if left := memory - cache.MemoryReserve(zeroing-1, cores, memory, 0); left <= 0 {
		t.Fatalf("%d transfers already leave a %d-byte node nothing", zeroing-1, memory)
	}
	// THE FRACTION the prose states: with a core for every CoreMemory or more,
	// serving keeps ServingMemoryPerCore of each CoreMemory and the transfers
	// zero the budget at the rest, so the lane found keeps at least that share
	// and one fewer less than a point more (the figure is the boundary,
	// rounded down).
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
	alone, seats := mib(executorPools(false, memory)), mib(executorPools(true, memory))
	for _, want := range []string{
		fmt.Sprintf("%s for every transfer its git lane admits", transfer),
		fmt.Sprintf("%s for each core its memory backs at %s a core", serving, core),
		fmt.Sprintf("no more than one for every %s, and leaves the background work %s for each backed core",
			amount(t, gitproc.TransferShare), amount(t, cache.MaintenanceMemoryPerCore)),
		fmt.Sprintf("once its transfers' %s each reach the memory less the cores' %s each: at as little as "+
			"%d%% of the transfers the memory holds at %s each, where the node has a core for every %s of it "+
			"or more", transfer, serving, percent, transfer, core),
		"clears when gitlet.max_transfers is lowered or unset, or when the node is given memory past what it " +
			"keeps, in place or at a boot",
		"A restart alone keeps the same lanes over the same memory, and clears it only where " +
			"gitlet.max_transfers is unset and the memory was lowered after the node booted",
		fmt.Sprintf("first the executor's pools it runs (%s MiB of transcript reads, %s MiB on a node running "+
			"seats) and the store's page cache (a sixteenth of its memory), then of the memory they leave", alone,
			seats),
		fmt.Sprintf("A node whose boot logged `code_cache_memory_short` keeps more than its memory, so its "+
			"budget is zero: give it the `memory_needed` that line names, %s or more in all where it runs seats "+
			"beside Gitlet at the default node.max_concurrent.", amount(t, seatsFloor(t))),
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
			fmt.Sprintf("both of what the executor's pools (%s MiB, or %s MiB on a node running seats too) and the "+
				"store's [page cache](../guides/deployment.md#the-page-cache) leave", alone, seats),
			"[`code_maintenance_refused`](../reference/alarms.md) says so until the figure is lowered or " +
				"unset, or the node is given memory past what it keeps, in place or at a boot. A restart alone " +
				"changes nothing.",
		}},
		{filepath.Join("docs", "concepts", "gitlet.md"), []string{
			fmt.Sprintf("once its transfers' %s each reach the memory less the %s of each core it backs — as "+
				"little as %d%% of the transfers the memory holds at %s each, where the node has a core for "+
				"every %s or more", transfer, serving, percent, transfer, core),
			"Lowering or unsetting it, or memory past what the node keeps, gives the budget back; a restart " +
				"alone does not.",
			fmt.Sprintf("Give the node more memory, or — where `gitlet.max_transfers` is set — fewer transfers, "+
				"each of which keeps %s of it.", transfer),
		}},
		{filepath.Join("docs", "guides", "deployment.md"), []string{
			fmt.Sprintf("keeps them first and sizes Gitlet by what is left "+
				"([Memory](../concepts/gitlet.md#the-cache-on-a-gitlet-node)), %s MiB for the transcript reads its "+
				"API answers and %s MiB where it runs seats too", alone, seats),
			fmt.Sprintf("logging `code_cache_memory_short` once, but at the default `node.max_concurrent` give a "+
				"node running seats beside Gitlet %s of memory or more in all", amount(t, seatsFloor(t))),
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
// catalogue cannot import the cache (it imports them), so this holds their
// words to its constants: each step's bytes an object, the transfer's beside
// them, the reread interval, a compaction's charge over the packs it merges
// (never the whole repository) plus a multiple of max_blob_bytes, and an
// expunge's over the packs it sorts beside a WHOLE-repository clone's room
// (Cache.AdmitExpunge).
//
// Mutation: change IndexBytesPerObject, GraphBytesPerObject,
// TipGraphBytesPerObject, GraphBaseBytes, CompactBytesPerObject,
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
			fmt.Sprintf("`graph` (%d, and %s whatever the repository's size for its changed-path filters)",
				cache.GraphBytesPerObject, amount(t, cache.GraphBaseBytes)),
			fmt.Sprintf("`tip-graph` (%d an object of the tail", cache.TipGraphBytesPerObject),
			fmt.Sprintf("and %s whatever its length", amount(t, cache.GraphBaseBytes)),
			fmt.Sprintf("`compact` (%d bytes an object of the packs merged, beside a transfer's room of them — %d "+
				"bytes an object merged, never of the whole repository, whose size alone would refuse every merge "+
				"of a large one — and %s times max_blob_bytes for the build's delta search)",
				cache.CompactBytesPerObject, gitproc.TransferBytesPerObject, window),
			fmt.Sprintf("`publish` (%d bytes an object of the repository", cache.PublishBytesPerObject),
			fmt.Sprintf("`expunge` (%d bytes an object of the packs an expunge sorts, beside the room for a clone "+
				"of the whole repository, whose history its build walks", cache.CompactBytesPerObject),
		},
		metrics.CodeCacheMaintenanceRefused: {reread},
		// The compactor counts the cache's publication refusal too, at the
		// cache's charge.
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

// THE CONCEPT PAGE SIZES A GITLET NODE BY THE CACHE'S OWN FIGURES: operators
// size a node from docs/concepts/gitlet.md's bytes an object, reserve, budget
// on its worked shapes and indexing cadence. Each is a constant here or in
// gitproc, or arithmetic over them, so this holds every figure to its
// constant and computes each example through MemoryReserve at the default
// lanes: a stale page would have somebody buy a node too small.
//
// Mutation: change any constant the page names — BitmapBytesPerObject,
// FsckBytesPerObject, gitproc.TransferMemory, MaintenanceMemoryPerCore,
// MidxPacks, TipGraphBytesPerObject, GraphBaseBytes, GraphLayerBytesPerObject,
// CompactBytesPerObject, CompactWindowBytes' multiple — and the page no
// longer says what the cache does.
func TestTheConceptPageSizesAGitletNodeByTheCachesOwnFigures(t *testing.T) {
	t.Parallel()
	path := filepath.Join(sourcetree.Root(t), "docs", "concepts", "gitlet.md")
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
		transfers = lanesOf(t, cores, memory, nil)
		return memory - cache.MemoryReserve(transfers, cores, memory, 0), transfers
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
		fmt.Sprintf("%d for the index without one and %s beside it whatever the repository's size, %d for the "+
			"commit-graph and %s beside it whatever the repository's size,", cache.IndexBytesPerObject,
			amount(t, cache.IndexBaseBytes), cache.GraphBytesPerObject, amount(t, cache.GraphBaseBytes)),
		fmt.Sprintf("%d for `crewlet gitlet verify`'s connectivity check, and a clone about %d more than the %s its "+
			"git transfer slot keeps for it", cache.FsckBytesPerObject, gitproc.TransferBytesPerObject,
			amount(t, gitproc.TransferMemory)),
		fmt.Sprintf("keeps %s of it for every git transfer it admits at once and %s for each core its git runs on",
			amount(t, gitproc.TransferMemory), amount(t, cache.ServingMemoryPerCore)),
		fmt.Sprintf("the cores the memory backs at %s a core", amount(t, gitproc.CoreMemory)),
		fmt.Sprintf("a node of 64 cores and 16 GiB admits %d transfers rather than %d, and runs as the %s-core node "+
			"its memory makes it", wide, gitproc.TransfersPerProcess*64, word(t, int((16<<30)/gitproc.CoreMemory))),
		fmt.Sprintf("about %s bytes an object, with the clone beside it", spaced(bitmap)),
		fmt.Sprintf("A node of %s a core leaves %s a core (%s on four cores: bitmaps of repositories of up to "+
			"about %s objects, indexes of up to about %s); 16 GiB on four cores leaves %s: bitmaps of up to about "+
			"%s objects, indexes of up to about %s.", amount(t, gitproc.CoreMemory), share, amount(t, shape),
			atMost(shape/bitmap), atMost((shape-cache.IndexBaseBytes)/index), amount(t, large), atMost(large/bitmap),
			atMost((large-cache.IndexBaseBytes)/index)),
		fmt.Sprintf("as many run at once as %s a core holds beside the largest", share),
		fmt.Sprintf("%s a core and no more than one for each %s of the memory the executor's pools leave, "+
			"each kept %s",
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
			"whatever its length", spaced(cache.TipGraphBytesPerObject), amount(t, cache.GraphBaseBytes)),
		fmt.Sprintf("it is charged the largest of theirs: %s bytes an object of the whole repository for its bitmap",
			spaced(cache.PublishBytesPerObject)),
		fmt.Sprintf("when the layer is the chain's root, and %s when it is written over a base",
			spaced(cache.PublishTailBytesPerObject)),
		fmt.Sprintf("%s an object of the layer for its commit-graph layer, and %s beside it for its changed-path "+
			"filters' diffs", spaced(cache.GraphLayerBytesPerObject), amount(t, cache.GraphBaseBytes)),
		// A COMPACTION'S BUILD, as its admission charges it, in its own
		// section and in the Memory paragraph.
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
	for _, want := range seatsBesideGitlet(t) {
		if !strings.Contains(page, want) {
			t.Errorf("%s does not say %q", path, want)
		}
	}
}

// seatsBesideGitlet is what the concept page says of the executor's pools
// beside Gitlet, from the pools a node keeps (executorPools) and the cache's
// reserve: each pool, what a turn adds, a row of its table for each shape
// with and without seats, and what 2 GiB running seats keeps past its memory.
func seatsBesideGitlet(t *testing.T) []string {
	t.Helper()
	turn := executor.NodeWork{Boxes: true, Turns: 1}.MemoryPools(0) - executor.NodeWork{Boxes: true}.MemoryPools(0)
	want := []string{
		fmt.Sprintf("%s MiB for the [transcript reads](code-sandbox.md#reading-a-seats-sessions) its API answers, "+
			"and on a node running seats too %s MiB at the default `node.max_concurrent`",
			mib(executorPools(false, 0)), mib(executorPools(true, 0))),
		fmt.Sprintf("reads of its boxes for its peers, %.1f MiB a turn,", float64(turn)/(1<<20)),
	}
	for _, cores := range []int{1, 2, 4, 8} {
		memory := int64(cores) * gitproc.CoreMemory
		for _, seats := range []bool{false, true} {
			keeps := gitletKeeps(t, seats, cores)
			reserve, pools := keeps(memory), executorPools(seats, memory)
			transfers := lanesOf(t, cores, memory, keeps)
			kept := cache.MemoryReserve(transfers, cores, memory, reserve)
			serving := kept - reserve - int64(transfers)*gitproc.TransferMemory
			yes, plural := "no", "s"
			if seats {
				yes = "yes"
			}
			if cores == 1 {
				plural = ""
			}
			want = append(want, fmt.Sprintf("| %s, %s core%s | %s | %s | %s | %d | %s | %s |", amount(t, memory),
				word(t, cores), plural, yes, mib(pools), mib(reserve-pools), transfers, mib(serving),
				mib(max(0, memory-kept))))
			if seats && cores == 1 {
				want = append(want, fmt.Sprintf("so %s running seats beside Gitlet keeps %s MiB more than its "+
					"memory", amount(t, memory), mib(kept-memory)))
			}
		}
	}
	if cost := readsCost(t, true); cost != 0 {
		t.Errorf("the transcript reads' pool costs a node running seats up to %d transfers at %s a core, and "+
			"the page states its cost for a node without seats alone", cost, amount(t, gitproc.CoreMemory))
	}
	turnMiB := float64(turn) / (1 << 20)
	return append(want,
		fmt.Sprintf("so every Gitlet node keeps the transcript reads' pool, with seats or without; on a node "+
			"without seats of %s of memory a core in all, that costs it at most %s transfer",
			amount(t, gitproc.CoreMemory), word(t, readsCost(t, false))),
		fmt.Sprintf("logs `code_cache_memory_short` once at boot with the figures; at the default "+
			"`node.max_concurrent`, give a node running seats beside Gitlet %s of memory or more in all",
			amount(t, seatsFloor(t))),
		fmt.Sprintf("the executor's `pools`, %s for each of its `transfer_slots`, and the serving share",
			amount(t, gitproc.TransferMemory)),
		fmt.Sprintf("or, on a node running seats, a lower `node.max_concurrent` (%.1f MiB a turn)", turnMiB))
}

// readsCost is the most transfers the transcript reads' pool, which every
// Gitlet node keeps as every one serves ingress, costs a node of
// gitproc.CoreMemory a core with seats or without, beside its page cache, at
// the shapes the table sizes.
func readsCost(t *testing.T, seats bool) int {
	t.Helper()
	cost := 0
	for _, cores := range []int{1, 2, 4, 8} {
		memory := int64(cores) * gitproc.CoreMemory
		without := nodeWork(seats)
		without.PersonReads = false
		keeps := gitletKeeps(t, seats, cores)
		cached := keeps(memory) - executorPools(seats, memory)
		cost = max(cost, lanesOf(t, cores, memory, fixed(without.MemoryPools(memory)+cached))-
			lanesOf(t, cores, memory, keeps))
	}
	return cost
}

// seatsFloor is the least memory, at gitproc.CoreMemory a core, at which a
// node running seats beside Gitlet keeps no more than its memory at the
// default lanes: what the docs recommend for one.
func seatsFloor(t *testing.T) int64 {
	t.Helper()
	for cores := 1; cores <= 64; cores++ {
		memory := int64(cores) * gitproc.CoreMemory
		keeps := gitletKeeps(t, true, cores)
		if cache.MemoryReserve(lanesOf(t, cores, memory, keeps), cores, memory, keeps(memory)) <= memory {
			return memory
		}
	}
	t.Fatal("no node of up to 64 cores at CoreMemory a core holds what a node running seats keeps")
	return 0
}

// lanesOf is the transfer slots a real pool gives a node of cores read slots
// and memory bytes keeping reserve(memory) of it beside Gitlet (nil: nothing),
// at the default lanes: every figure the docs derive goes through gitproc's
// own rule, never a copy.
func lanesOf(t *testing.T, cores int, memory int64, reserve func(int64) int64) int {
	t.Helper()
	layout, err := cache.Prepare(t.Context(), filepath.Join(t.TempDir(), "c"))
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	t.Cleanup(layout.Release)
	return newPoolOf(t, layout, "", cores, 0, memory, reserve).Slots(gitproc.ClassTransfer)
}

// executorPools is what a Gitlet node of memory bytes keeps of the
// executor's pools, as the engine passes them: the transcript reads its API
// answers, as every Gitlet node serves ingress, and with seats its boxes'
// pools at the default node.max_concurrent and the box host's reads for its
// peers (queue.ConcurrentAnswers, the engine's boxReadsHeld).
func executorPools(seats bool, memory int64) int64 { return nodeWork(seats).MemoryPools(memory) }

// nodeWork is the executor's work a Gitlet node runs, as executorPools sizes it.
func nodeWork(seats bool) executor.NodeWork {
	w := executor.NodeWork{PersonReads: true}
	if seats {
		w.Boxes, w.Turns, w.PeerReads = true, enginenode.DefaultMaxConcurrent, queue.ConcurrentAnswers
	}
	return w
}

// mib is n bytes in whole mebibytes, rounded, as the docs write them.
func mib(n int64) string { return spaced((n + 1<<19) >> 20) }

// windowBlobs is how many of the epoch's largest blob a compaction's build
// is charged for its delta search (cache.CompactWindowBytes): the docs spell
// it as a whole multiple of max_blob_bytes.
func windowBlobs(t *testing.T) int {
	t.Helper()
	const blob = 1 << 20
	w := cache.CompactWindowBytes(gitlet.Limits{MaxBlobBytes: blob})
	if w%blob != 0 {
		t.Fatalf("CompactWindowBytes is %d bytes at a max_blob_bytes of %d, no whole multiple of it: say in the "+
			"docs how it is written, and here", w, blob)
	}
	return int(w / blob)
}

// THE SCALING PAGE SIZES A GITLET NODE BY THE SAME FIGURES:
// docs/concepts/scaling.md states a bitmap's and a clone's bytes an object and
// a maintainer's compaction and publication charges, deferring the rest to the
// concept page; held here as that page's are, so neither goes stale alone.
//
// Mutation: change BitmapBytesPerObject, PublishBytesPerObject,
// gitproc.TransferBytesPerObject, CompactBytesPerObject or CompactWindowBytes'
// multiple, and the scaling page no longer says what the cache charges.
func TestTheScalingPageSizesAGitletNodeByTheCachesOwnFigures(t *testing.T) {
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
	// THE CHARGE A REPOSITORY'S MAINTAINER IS PLACED BY: the larger of its
	// publication's and a merge of every object's.
	publish := fmt.Sprintf("its bitmap at %s bytes an object of the whole repository and a clone's %d beside it, "+
		"%s bytes an object", spaced(cache.PublishBytesPerObject), gitproc.TransferBytesPerObject,
		spaced(int64(cache.PublishBytesPerObject+gitproc.TransferBytesPerObject)))
	merge := fmt.Sprintf("for a merge of every object, %s bytes an object beside the build's delta window — %s of "+
		"the largest blob `code.limits.max_blob_bytes` admits", spaced(int64(cache.CompactBytesPerObject+
		gitproc.TransferBytesPerObject)), word(t, windowBlobs(t)))
	placed := fmt.Sprintf("holds the larger of %s × N bytes and %s × N bytes plus that window",
		spaced(int64(cache.PublishBytesPerObject+gitproc.TransferBytesPerObject)),
		spaced(int64(cache.CompactBytesPerObject+gitproc.TransferBytesPerObject)))
	for _, want := range []string{publish, merge, placed} {
		if !strings.Contains(page, want) {
			t.Errorf("%s does not say %q", path, want)
		}
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
