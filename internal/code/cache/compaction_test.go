package cache_test

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/code"
	"github.com/crewlet/crewlet/internal/code/cache"
	"github.com/crewlet/crewlet/internal/code/gitproc"
	"github.com/crewlet/crewlet/internal/statelog/metrics"
)

// THE LIVE PACKS A PLAN WEIGHS ARE THE ROWS: Packs answers the incarnation's
// live rows, sorted by pack id, each with the figures a compaction's plan reads
// — its checksum, its bundle's bytes and both indexes', and its objects — as
// the rows the cache synced to name them.
//
// Mutation: answer a row's objects from its index's size rather than the row,
// and the figures disagree with the rows.
func TestPacksAreTheLiveRowsAPlanWeighs(t *testing.T) {
	t.Parallel()
	w := newSyncWorld(t, onMemory)
	repo := w.repo(t, 1)
	var want []code.PackRow
	for range 3 {
		_, row := w.push(t, repo, "refs/heads/main", nil)
		want = append(want, code.PackRow{PackID: row.PackID, Size: row.Size, IndexBytes: row.IndexBytes,
			ReverseBytes: row.ReverseBytes, GitChecksum: row.GitChecksum, Objects: row.Objects})
	}
	slices.SortFunc(want, func(a, b code.PackRow) int { return strings.Compare(a.PackID, b.PackID) })
	c := w.open(t)
	s := w.sync(t, c, repo)
	if got := c.Packs(s); !slices.Equal(got, want) {
		t.Errorf("Packs = %+v\nwant the rows %+v", got, want)
	}
	if got := c.Packs(cache.Synced{}); got != nil {
		t.Errorf("Packs of a Synced the cache did not hand out = %+v, want nil", got)
	}
}

// A COMPACTION LEAVES THE MAINTAINER'S RESERVED SLOT, AND THE ROOM A PASS KEPT:
// a compaction's build holds a maintenance slot for its whole length, as a
// maintainer's run does, so it is admitted as one — at the maintainer's limit,
// every slot of the lane but the one a person's verify needs, and beside the
// room the latest pass kept for a repository it held back for memory — never
// beside them as a person's check is, which takes no slot of the maintainer's.
// And it counts as a run until it is released: a pass meanwhile starts nothing
// the lane has no slot for.
//
// Mutation: admit a compaction as a check is (admitOther), and it is admitted
// beside the maintainer's run, and beside the room the pass kept.
func TestACompactionLeavesTheMaintainersReservedSlot(t *testing.T) {
	t.Parallel()
	limits := code.Limits{MaxBlobBytes: 1}
	held := func(release <-chan struct{}) func(o *cache.Options) {
		return func(o *cache.Options) {
			cache.BeforeMaintaining(o, func(ctx context.Context, step string) {
				if step != "multi-pack-index" {
					return
				}
				select {
				case <-release:
				case <-ctx.Done():
				}
			})
		}
	}

	t.Run("the maintainer's slots", func(t *testing.T) {
		t.Parallel()
		w := newSyncWorld(t, onMemory)
		w.processes = 4 // a maintenance lane of two: the maintainer's one, and a person's
		clk := newClock()
		a, b := w.repo(t, 1), w.repo(t, 2)
		for _, repo := range []string{a, b} {
			w.push(t, repo, "refs/heads/main", nil)
			w.push(t, repo, "refs/heads/main", nil)
		}
		release := make(chan struct{})
		c := w.open(t, clk.on, held(release))
		if got := w.pool.Slots(gitproc.ClassMaintenance); got != 2 {
			t.Fatalf("the pool has %d maintenance slots, want 2", got)
		}
		sa, sb := w.sync(t, c, a), w.sync(t, c, b)
		graphFirst(t, c, a, b)
		clk.Add(cache.MidxDebounce)
		started, wait := cache.StartMaintainerPass(c)
		if len(started) != 1 {
			close(release)
			wait()
			t.Fatalf("the pass started %v, want one run in a lane of two", started)
		}
		if _, err := c.AdmitCompaction(sb, 1, limits); !errors.Is(err, gitproc.ErrBusy) {
			t.Errorf("a compaction admitted beside the maintainer's one run = %v, want ErrBusy", err)
		}
		close(release)
		wait()

		adm, err := c.AdmitCompaction(sa, 1, limits)
		if err != nil {
			t.Fatalf("a compaction once the maintainer's run ended = %v, want it admitted", err)
		}
		if started, wait := cache.StartMaintainerPass(c); len(started) != 0 {
			wait()
			t.Errorf("a pass beside an admitted compaction started %v, want nothing: the compaction is the run "+
				"the lane has room for", started)
		}
		adm.Release()
		adm.Release() // idempotent
		if ran := cache.MaintainerPass(c); len(ran) != 1 {
			t.Errorf("the pass once the compaction was released ran %v, want the repository still due", ran)
		}
	})

	t.Run("a pass's room kept", func(t *testing.T) {
		t.Parallel()
		w := newSyncWorld(t, onMemory)
		w.processes = 8 // room for three runs: the slots are not what holds this compaction back
		clk := newClock()
		a, b := w.repo(t, 1), w.repo(t, 2)
		for range 3 {
			w.push(t, a, "refs/heads/main", nil)
		}
		w.push(t, b, "refs/heads/main", nil)
		w.push(t, b, "refs/heads/main", nil)
		release := make(chan struct{})
		c := w.open(t, clk.on, held(release))
		w.sync(t, c, a)
		sb := w.sync(t, c, b)
		ea, eb := cache.BitmapCharge(c, a), cache.BitmapCharge(c, b)
		cache.SetMaintenanceMemory(c, 1<<40)
		cache.SetMaintenanceBudget(c, ea+eb-1)
		graphFirst(t, c, a, b)
		clk.Add(cache.MidxDebounce)
		started, wait := cache.StartMaintainerPass(c)
		defer func() {
			close(release)
			wait()
		}()
		if len(started) != 1 || started[0] != a {
			t.Fatalf("the pass started %v, want %s alone: %s held back for the room beside it", started, a, b)
		}
		// A build whose charge fits beside the run alone, and not beside the
		// run and the room the pass kept for b.
		if _, err := c.AdmitCompaction(sb, 1, limits); !errors.Is(err, gitproc.ErrBusy) {
			t.Errorf("a compaction that would take the room the pass kept for %s = %v, want ErrBusy", b, err)
		}
	})
}

// A COMPACTION THE NODE'S MEMORY CANNOT HOLD IS REFUSED, AND SAID ONCE: its
// charge alone past the budget answers ErrNoMemory, logged
// code_cache_compaction_refused and counted once an incarnation however often
// it is asked again — a compactor asks at every pass, since the budget is read
// again at every maintainer pass and a node may grow under it.
func TestACompactionPastTheBudgetIsRefusedOnce(t *testing.T) {
	t.Parallel()
	w := newSyncWorld(t, onMemory)
	repo := w.repo(t, 1)
	w.push(t, repo, "refs/heads/main", nil)
	c := w.open(t)
	s := w.sync(t, c, repo)
	limits := code.Limits{MaxBlobBytes: 1 << 20}
	cache.SetMaintenanceBudget(c, cache.CompactWindowBytes(limits))
	for range 3 {
		if _, err := c.AdmitCompaction(s, 1, limits); !errors.Is(err, cache.ErrNoMemory) {
			t.Fatalf("a compaction whose window alone fills the budget = %v, want ErrNoMemory", err)
		}
	}
	if got := len(w.logs.lines(t, "code_cache_compaction_refused")); got != 1 {
		t.Errorf("three refusals logged code_cache_compaction_refused %d times, want once", got)
	}
	if got := counter(w.rec, metrics.CodeCacheMaintenanceRefusals, "step", "compact"); got != 1 {
		t.Errorf("%s{step=compact} = %d after three refusals, want 1", metrics.CodeCacheMaintenanceRefusals, got)
	}
	cache.SetMaintenanceBudget(c, 1<<30)
	adm, err := c.AdmitCompaction(s, 1, limits)
	if err != nil {
		t.Fatalf("the compaction once the node grew = %v, want it admitted", err)
	}
	adm.Release()
}

// A COMPACTION KEEPS ROOM FOR A CLONE OF ITS WHOLE REPOSITORY: its build is
// charged CompactBytesPerObject an object of the packs it merges — its own
// memory follows them — and, beside it, room for one transfer of the WHOLE
// repository, gitproc.TransferBytesPerObject an object its live packs hold, as
// every background step keeps (chargeOver), and its delta window: the clone it
// shares the node with is of the repository however few packs the build
// merges, and a merge of a large repository's few small packs charged by those
// packs alone is admitted beside a clone the budget kept no room for. A budget
// a byte short of that refuses it ErrNoMemory, naming the charge, and that
// budget admits it.
//
// Mutation: charge the clone's room by the objects merged
// (chargeOver(inputs, CompactBytesPerObject, inputs)), and a merge of one
// object fits a budget that holds no clone of its repository beside it.
func TestACompactionKeepsRoomForACloneOfItsWholeRepository(t *testing.T) {
	t.Parallel()
	w := newSyncWorld(t, onMemory)
	repo := w.repo(t, 1)
	var objects int64
	for range 3 {
		_, row := w.push(t, repo, "refs/heads/main", nil)
		objects += row.Objects
	}
	c := w.open(t)
	s := w.sync(t, c, repo)
	const inputs = 1
	if objects <= inputs {
		t.Fatalf("the repository holds %d objects, want more than the %d a merge of it takes", objects, inputs)
	}
	limits := code.Limits{MaxBlobBytes: 1}
	charge := inputs*cache.CompactBytesPerObject + objects*gitproc.TransferBytesPerObject +
		cache.CompactWindowBytes(limits)
	cache.SetMaintenanceBudget(c, charge-1)
	if adm, err := c.AdmitCompaction(s, inputs, limits); !errors.Is(err, cache.ErrNoMemory) ||
		!strings.Contains(err.Error(), fmt.Sprintf("needs about %d bytes", charge)) {
		if adm != nil {
			adm.Release()
		}
		t.Fatalf("a merge of %d object of a repository of %d on a budget of %d = %v, want ErrNoMemory at a "+
			"charge of %d: %d bytes an object merged, %d an object of the WHOLE repository for a clone's room "+
			"beside it, and its window", inputs, objects, charge-1, err, charge, cache.CompactBytesPerObject,
			gitproc.TransferBytesPerObject)
	}
	cache.SetMaintenanceBudget(c, charge)
	// ASKED AGAIN WHILE BUSY: the maintainer may hold the lane a moment for
	// the commit-graph a sync leaves due, and at a budget of exactly this
	// charge nothing may stand beside the build — ErrBusy is a moment's state,
	// which the compactor asks again at its next poll.
	deadline := time.Now().Add(hangDetector)
	for {
		adm, err := c.AdmitCompaction(s, inputs, limits)
		if err == nil {
			adm.Release()
			return
		}
		if !errors.Is(err, gitproc.ErrBusy) || time.Now().After(deadline) {
			t.Fatalf("the same merge on a budget of exactly its charge, %d = %v, want it admitted", charge, err)
		}
		time.Sleep(20 * time.Millisecond)
	}
}
