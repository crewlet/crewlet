package cache_test

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/code"
	"github.com/crewlet/crewlet/internal/code/cache"
	"github.com/crewlet/crewlet/internal/code/gitproc"
	"github.com/crewlet/crewlet/internal/objstore"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/statelog/metrics"
)

// objectsOf is every object pushes' packs hold, as their rows count them.
func objectsOf(pushes []pushed) int64 {
	var n int64
	for _, p := range pushes {
		n += p.row.Objects
	}
	return n
}

// A PUBLICATION IS CHARGED BY ITS LARGEST STEP, each with room for a clone of
// its whole repository beside it: its steps run one after another in its one
// slot, so its charge is the largest of the multi-pack-index layer and its
// bitmap — PublishBytesPerObject an object of the WHOLE repository, however few
// objects the layer adds, since the bitmap walks every commit — the
// commit-graph layer — GraphLayerBytesPerObject an object of the layer — and
// its repack — CompactBytesPerObject an object of what it merges, and the
// delta window; and the charge a repository's publication is PLACED by
// (Cache.PublishCharge) is the first's, the one its admission holds it to.
//
// Mutation: charge the bitmap by the layer's objects, and a layer of one
// object over a repository of many fits a budget that holds no bitmap of it.
func TestAPublicationIsChargedByItsLargestStep(t *testing.T) {
	t.Parallel()
	w := newSyncWorld(t, onMemory)
	repo := w.repo(t, 1)
	objects := objectsOf(w.pushN(t, repo, 3))
	c := w.open(t)
	s := w.sync(t, c, repo)
	defer s.Release()
	clone := objects * gitproc.TransferBytesPerObject
	if got, want := c.PublishCharge(objects), objects*cache.PublishBytesPerObject+clone; got != want {
		t.Errorf("PublishCharge(%d) = %d, want %d: the bitmap's figure an object of the repository and a clone's "+
			"room beside it", objects, got, want)
	}
	limits := code.Limits{MaxBlobBytes: 1 << 20}
	for _, c2 := range []struct {
		name                  string
		layer, repack, charge int64
	}{
		{name: "the bitmap over the whole repository", layer: 1,
			charge: objects*cache.PublishBytesPerObject + clone},
		{name: "the commit-graph layer", layer: 10 * objects,
			charge: 10*objects*cache.GraphLayerBytesPerObject + clone},
		{name: "the repack and its window", layer: 1, repack: 100 * objects,
			charge: 100*objects*cache.CompactBytesPerObject + clone + cache.CompactWindowBytes(limits)},
	} {
		cache.SetMaintenanceBudget(c, c2.charge-1)
		_, err := c.AdmitPublication(s, c2.layer, c2.repack, limits)
		if !errors.Is(err, cache.ErrNoMemory) || !strings.Contains(err.Error(), fmt.Sprintf("needs about %d bytes",
			c2.charge)) {
			t.Errorf("%s: a publication on a budget a byte under its charge %d = %v, want ErrNoMemory at that "+
				"charge", c2.name, c2.charge, err)
		}
		cache.SetMaintenanceBudget(c, c2.charge)
		adm, err := c.AdmitPublication(s, c2.layer, c2.repack, limits)
		if err != nil {
			t.Errorf("%s: the same publication on a budget of exactly its charge = %v, want it admitted", c2.name, err)
			continue
		}
		adm.Release()
	}
}

// A PUBLICATION'S CHARGE MOVES IN ITS OWN SLOT: a publication that plans
// again over the copy its admission pins — its maintainer replacing a base the
// object store lost, with a larger layer or a repack its packs now need —
// holds the new plan's charge in place of the first's, keeping its slot: as
// the node's budget allows it in that place, refused ErrNoMemory past the
// budget with its old charge kept, and its release gives back whatever it
// holds then. A compaction's admission, or a released one, holds no
// publication's charge.
//
// Mutation: count the old charge beside the new, and a charge the budget holds
// in its place is refused.
func TestAPublicationsChargeMovesInItsOwnSlot(t *testing.T) {
	t.Parallel()
	w := newSyncWorld(t, onMemory)
	repo := w.repo(t, 1)
	objects := objectsOf(w.pushN(t, repo, 3))
	c := w.open(t)
	s := w.sync(t, c, repo)
	defer s.Release()
	limits := code.Limits{MaxBlobBytes: 1 << 20}
	repack := 100 * objects
	large := repack*cache.CompactBytesPerObject + objects*gitproc.TransferBytesPerObject +
		cache.CompactWindowBytes(limits)
	cache.SetMaintenanceBudget(c, large)
	adm, err := c.AdmitPublication(s, 1, 0, limits)
	if err != nil {
		t.Fatalf("a publication of a layer over one object = %v, want it admitted", err)
	}
	if err := adm.RechargePublication(s, 1, repack, limits); err != nil {
		t.Errorf("the charge of a plan with a repack, %d bytes on a budget of as many, = %v; want it held in "+
			"the same slot", large, err)
	}
	if err := adm.RechargePublication(s, 1, 2*repack, limits); !errors.Is(err, cache.ErrNoMemory) {
		t.Errorf("a charge past the budget = %v, want ErrNoMemory", err)
	}
	if err := adm.RechargePublication(s, 1, 0, limits); err != nil {
		t.Errorf("a smaller charge = %v, want it held at once", err)
	}
	adm.Release()
	if err := adm.RechargePublication(s, 1, 0, limits); !errors.Is(err, cache.ErrInvalid) {
		t.Errorf("a released admission's charge moved = %v, want ErrInvalid", err)
	}
	again, err := c.AdmitPublication(s, 1, repack, limits)
	if err != nil {
		t.Errorf("a publication at the whole budget once the first was released = %v, want it admitted: the "+
			"release gives back what it held", err)
	} else {
		again.Release()
	}
	compaction, err := c.AdmitCompaction(s, objects, limits)
	if err != nil {
		t.Fatalf("a compaction of the repository = %v, want it admitted", err)
	}
	defer compaction.Release()
	if err := compaction.RechargePublication(s, 1, 0, limits); !errors.Is(err, cache.ErrInvalid) {
		t.Errorf("a compaction's admission charged as a publication = %v, want ErrInvalid", err)
	}
}

// A PUBLICATION PAST THE BUDGET IS REFUSED ONCE AND RAISES THE ALARM: a
// publication's charge alone past what the node gives its background work
// answers ErrNoMemory — logged code_cache_maintenance_refused with step
// `publish` and counted once an incarnation however often its maintainer asks
// — and the charge refused is kept, so every maintainer pass, judging it
// against the budget it reads, counts the repository on
// crewlet.code.cache.maintenance.refused{step=publish} and in the reading that
// raises code_maintenance_refused: a repository its maintainer cannot publish
// serves every pack pushed since outside every bitmap. A budget given the room
// clears it at the next pass; a publication admitted, or the rows' chain moved
// by another node's publication, drops the record.
//
// Mutations: leave the publication out of the reading, and the alarm never
// fires for it; keep the record past an admission, or past the chain moving,
// and the gauge stands for a repository published since.
func TestAPublicationPastTheBudgetIsRefusedOnceAndRaisesTheAlarm(t *testing.T) {
	t.Parallel()
	w := newSyncWorld(t, onMemory)
	repo := w.repo(t, 1)
	p := w.pushN(t, repo, 2)
	objects := objectsOf(p)
	// A CHAINED COPY, every pack in its chain: its own maintenance takes no
	// step, so what the walk counts is the publication alone.
	w.log.publish(repo, w.buildChain(t, p), nil)
	c := w.open(t)
	s := w.sync(t, c, repo)
	defer func() { s.Release() }()
	if got := cache.Installed(c, repo); len(got) != 1 {
		t.Fatalf("the premise: the copy holds %d layers installed, want the chain", len(got))
	}
	limits := code.Limits{MaxBlobBytes: 1}
	charge := c.PublishCharge(objects)
	refused := func(budget int64) {
		t.Helper()
		cache.SetMaintenanceBudget(c, budget)
		if _, err := c.AdmitPublication(s, 1, 0, limits); !errors.Is(err, cache.ErrNoMemory) {
			t.Fatalf("a publication on a budget of %d, under its charge %d = %v, want ErrNoMemory", budget, charge,
				err)
		}
	}
	standing := func(want int, what string) {
		t.Helper()
		cache.MaintainerPass(c)
		var r statelog.Reading
		c.AlarmReading(&r)
		gauge := gaugeAt(t, w.rec, metrics.CodeCacheMaintenanceRefused, "step", "publish")
		switch {
		case gauge != float64(want) || r.CodeMaintenanceRefused != want:
			t.Errorf("%s: the gauge's step publish is %v and the reading names %d repositories, want %d", what,
				gauge, r.CodeMaintenanceRefused, want)
		case want > 0 && (r.CodeMaintenanceLargest != charge || r.CodeMaintenanceLargest <= r.CodeMaintenanceBudget):
			t.Errorf("%s: the reading's largest charge is %d against a budget of %d, want the publication's %d "+
				"past it", what, r.CodeMaintenanceLargest, r.CodeMaintenanceBudget, charge)
		}
	}

	for range 3 {
		refused(charge - 1)
	}
	var publish int
	for _, l := range w.logs.lines(t, "code_cache_maintenance_refused") {
		if l["step"] == "publish" {
			publish++
		}
	}
	if publish != 1 {
		t.Errorf("three refusals logged code_cache_maintenance_refused with step publish %d times, want once",
			publish)
	}
	if got := counter(w.rec, metrics.CodeCacheMaintenanceRefusals, "step", "publish"); got != 1 {
		t.Errorf("%s{step=publish} = %d after three refusals, want 1", metrics.CodeCacheMaintenanceRefusals, got)
	}
	standing(1, "refused")

	cache.SetMaintenanceBudget(c, charge)
	standing(0, "the node given the room")
	cache.SetMaintenanceBudget(c, charge-1)
	standing(1, "the room taken back, no publication admitted between")

	cache.SetMaintenanceBudget(c, charge)
	adm, err := c.AdmitPublication(s, 1, 0, limits)
	if err != nil {
		t.Fatalf("the publication once the node grew = %v, want it admitted", err)
	}
	adm.Release()
	cache.SetMaintenanceBudget(c, charge-1)
	standing(0, "a publication admitted since")

	refused(charge - 1)
	standing(1, "refused again")
	w.log.publish(repo, w.buildChain(t, p[:1], p[1:]), nil)
	s.Release()
	s = w.sync(t, c, repo)
	standing(0, "the chain moved by another node's publication")
}

// A LAYER'S SCRATCH NAMES EXACTLY ITS BASE: NewLayerScratch links every pack
// file the copy holds and its packed-refs — the same inodes — and the files
// of the first base layers of the rows' chain, with chain files naming exactly
// those layers and none above them, and no alternate; a base past what the
// copy holds installed is refused, and a scratch over no base names no chain.
//
// Mutation: name the whole installed chain in the scratch's chain file, and
// the scratch over one layer of two names both.
func TestALayerScratchNamesExactlyItsBase(t *testing.T) {
	t.Parallel()
	w := newSyncWorld(t, onMemory)
	repo := w.repo(t, 1)
	p := w.pushN(t, repo, 3)
	chain := w.buildChain(t, p[:2], p[2:])
	w.log.publish(repo, chain, nil)
	c := w.open(t)
	s := w.sync(t, c, repo)
	defer s.Release()
	if got := layerIDs(cache.Installed(c, repo)); !slices.Equal(got, layerIDs(chain)) {
		t.Fatalf("the premise: the copy holds %v installed, want %v", got, layerIDs(chain))
	}
	sc, err := c.NewLayerScratch(t.Context(), s, uuid.Must(uuid.NewV7()).String(), 1)
	if err != nil {
		t.Fatal(err)
	}
	defer sc.Remove()
	if sc.Alternate != "" {
		t.Errorf("a layer's scratch names the alternate %q, want none: it holds links", sc.Alternate)
	}
	midx, graph := chainFiles(t, sc.Dir)
	if !slices.Equal(midx, layerIDs(chain[:1])) || !slices.Equal(graph, graphIDs(chain[:1])) {
		t.Errorf("the scratch over one layer names the chains %v and %v, want exactly %v and %v", midx, graph,
			layerIDs(chain[:1]), graphIDs(chain[:1]))
	}
	same := func(rel string) bool {
		a, aerr := os.Stat(filepath.Join(s.Dir, rel))
		b, berr := os.Stat(filepath.Join(sc.Dir, rel))
		return aerr == nil && berr == nil && os.SameFile(a, b)
	}
	linked := []string{"packed-refs",
		"objects/pack/multi-pack-index.d/multi-pack-index-" + chain[0].LayerID + ".midx",
		"objects/pack/multi-pack-index.d/multi-pack-index-" + chain[0].LayerID + ".bitmap",
		"objects/info/commit-graphs/graph-" + chain[0].GraphID + ".graph"}
	for _, pp := range p {
		for _, ext := range []string{".pack", ".idx", ".rev"} {
			linked = append(linked, "objects/pack/pack-"+pp.row.GitChecksum+ext)
		}
	}
	for _, rel := range linked {
		if !same(filepath.FromSlash(rel)) {
			t.Errorf("the scratch's %s is not the copy's own file, linked", rel)
		}
	}
	if exists(filepath.Join(sc.Dir, "objects", "pack", "multi-pack-index.d",
		"multi-pack-index-"+chain[1].LayerID+".midx")) {
		t.Errorf("the scratch over one layer holds the layer above it")
	}
	if _, err := c.NewLayerScratch(t.Context(), s, uuid.Must(uuid.NewV7()).String(), 3); !errors.Is(err,
		cache.ErrInvalid) {
		t.Errorf("a scratch over three layers of a copy holding two = %v, want ErrInvalid", err)
	}
	root, err := c.NewLayerScratch(t.Context(), s, uuid.Must(uuid.NewV7()).String(), 0)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Remove()
	if midx, graph := chainFiles(t, root.Dir); len(midx) != 0 || len(graph) != 0 {
		t.Errorf("a scratch over no layer names the chains %v and %v, want none", midx, graph)
	}
}

// THE CHAIN IS ONE READING, AND A REFUSED LAYER LISTS ITS REPOSITORY DAMAGED:
// Chain answers the rows' chain, how much of it the copy holds installed and
// what it refused, read together; Damaged lists every repository whose copy
// holds a layer it refused — what its maintainer repairs whether or not
// anything was pushed since — and none whose chain is whole.
//
// Mutation: list a repository damaged by a layer merely pending, and the whole
// copy is listed.
func TestTheChainIsOneReadingAndARefusedLayerListsItsRepositoryDamaged(t *testing.T) {
	t.Parallel()
	w := newSyncWorld(t, onMemory)
	whole, lost := w.repo(t, 1), w.repo(t, 2)
	pw, pl := w.pushN(t, whole, 2), w.pushN(t, lost, 2)
	chainW, chainL := w.buildChain(t, pw), w.buildChain(t, pl)
	gone := chainL[0].Parts[0].Object
	w.objects.mu.Lock()
	w.objects.fail = func(k objstore.Key) error {
		if k == gone {
			return objstore.ErrNotFound
		}
		return nil
	}
	w.objects.mu.Unlock()
	c := w.open(t)
	w.sync(t, c, whole).Release()
	w.sync(t, c, lost).Release()
	w.log.publish(whole, chainW, nil)
	w.log.publish(lost, chainL, nil)
	w.sync(t, c, whole).Release()
	w.sync(t, c, lost).Release()
	cache.InstallPass(c)
	// The copies as they are once the installer has placed what it could: a
	// placement is a swap, so a Synced taken before it holds the copy before.
	sw, sl := w.sync(t, c, whole), w.sync(t, c, lost)
	defer sw.Release()
	defer sl.Release()
	if got := c.Chain(sw); len(got.Rows) != 1 || got.Installed != 1 || len(got.Refused) != 0 {
		t.Errorf("the whole copy's chain reads %d rows, %d installed, refused %v; want one, installed, none refused",
			len(got.Rows), got.Installed, got.Refused)
	}
	if got := c.Chain(sl); len(got.Rows) != 1 || got.Installed != 0 ||
		got.Refused[chainL[0].LayerID] != "missing" {
		t.Errorf("the copy whose layer the store lost reads %d rows, %d installed, refused %v; want one, none "+
			"installed, it refused missing", len(got.Rows), got.Installed, got.Refused)
	}
	damaged := c.Damaged()
	if len(damaged) != 1 || damaged[0].ID != lost {
		t.Errorf("Damaged = %v, want %s alone", damaged, lost)
	}
	if got := c.Chain(cache.Synced{}); got.Rows != nil || got.Installed != 0 {
		t.Errorf("the chain of a Synced the cache did not hand out = %+v, want nothing", got)
	}
}
