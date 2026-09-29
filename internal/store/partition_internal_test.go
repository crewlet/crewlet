package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"testing"
)

// CLOSING A NODE GIVES BACK EVERY PARTITION'S CLAIM.
//
// The claim on a path is shared per process (see lock.go), so a leaked claim
// still lets the next open in THIS process through — the leak is invisible
// from outside and only bites a later process, after this one has exited and
// taken the evidence with it. So this asserts the claims themselves, as
// TestPendingReleasesTheLockForTheMigrationThatFollows does: no entry, not
// merely a second caller getting past it.
func TestClosingANodeGivesBackEveryPartitionsClaim(t *testing.T) {
	t.Parallel()
	node, err := OpenNode(t.Context(), filepath.Join(t.TempDir(), "node.db"), Options{})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	files := []PartitionFile{
		{Layout: 1, Name: "tracker.000", Logs: 2},
		{Layout: 1, Name: "pages.000", Logs: 2},
	}
	var paths []string
	for _, f := range files {
		part, err := node.OpenPartition(t.Context(), f)
		if err != nil {
			t.Fatalf("open %s: %v", f.Name, err)
		}
		paths = append(paths, part.Path())
	}
	if err := node.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	locksHeld.mu.Lock()
	defer locksHeld.mu.Unlock()
	for _, path := range append(paths, node.Path()) {
		if held := locksHeld.by[path]; held != nil {
			t.Errorf("%s is still claimed by %d handle(s) after its node closed",
				path, held.holds)
		}
	}
}

// A PARTITION THAT FAILS TO OPEN LEAVES NOTHING HELD, and the node as it was.
//
// A file the driver cannot open — a directory where it should be, a bad
// `store.replicated_path` — must leave the node serving what it has: its own
// file and every partition it already holds. And the file that failed must not
// stay claimed by a handle nobody got, or the next process to want it is
// refused by a lock nothing is using. The claim is asserted rather than a
// following open, for [TestClosingANodeGivesBackEveryPartitionsClaim]'s reason.
func TestAFailedPartitionOpenLeavesTheNodeAsItWas(t *testing.T) {
	t.Parallel()
	node, err := OpenNode(t.Context(), filepath.Join(t.TempDir(), "node.db"), Options{})
	if err != nil {
		t.Fatalf("open the node: %v", err)
	}
	defer func() { _ = node.Close() }()
	held, err := node.OpenPartition(t.Context(), PartitionFile{Layout: 1, Name: "tracker.000", Logs: 1})
	if err != nil {
		t.Fatalf("open a partition: %v", err)
	}
	blocked := PartitionFile{Layout: 1, Name: "tracker.001", Logs: 1}
	path := node.PartitionPath(blocked)
	if err := os.MkdirAll(path, 0o700); err != nil {
		t.Fatalf("put a directory where tracker.001's file goes: %v", err)
	}
	if _, err := node.OpenPartition(t.Context(), blocked); err == nil {
		t.Fatal("a partition whose file is a directory opened")
	}

	locksHeld.mu.Lock()
	leaked := locksHeld.by[path]
	locksHeld.mu.Unlock()
	if leaked != nil {
		t.Errorf("the failed open kept its claim on %s (%d holder(s))", path, leaked.holds)
	}
	if got := node.OpenPartitions(); len(got) != 1 || got[0] != "tracker.000" {
		t.Errorf("after a failed open the node holds %v, want only tracker.000", got)
	}
	if err := held.Read(context.Background(), func(*sql.Tx) error { return nil }); err != nil {
		t.Errorf("the partition the node already held is unreadable after a failed open: %v", err)
	}
	if err := node.Read(t.Context(), func(*sql.Tx) error { return nil }); err != nil {
		t.Errorf("the node's own file is unreadable after a failed open: %v", err)
	}
}

// THE NODE'S PAGE CACHE IS DIVIDED BY FILE SIZE ABOVE A FLOOR, AND ITS READERS
// ARE SPREAD WITH A FLOOR OF THEIR OWN.
//
// The exact cases pin the arithmetic a reader of [divide] can check by hand;
// the invariants below them are what the node relies on whatever the sizes: no
// file under either floor, and never more cache than the budget while the
// floors leave any of it.
func TestTheNodesPageCacheIsDividedByFileSizeAboveAFloor(t *testing.T) {
	t.Parallel()
	const budget, floor = int64(partitionCacheKiB), int64(partitionCacheFloorKiB)
	many := map[string]int64{}
	for i := range 200 {
		many[fmt.Sprintf("tracker.%03d", i)] = int64(i) << 20
	}
	for _, c := range []struct {
		name    string
		sizes   map[string]int64
		readers int
		want    map[string]allotment
	}{
		{
			// Exactly what the one replicated file had: every reader and
			// the whole cache, so layout 0 is sized as it always was.
			name:  "one file is sized as the one replicated file was",
			sizes: map[string]int64{"estate.000": 1 << 30}, readers: 4,
			want: map[string]allotment{"estate.000": {readers: 4, cacheKiB: budget}},
		},
		{
			name:  "above the floor, by size",
			sizes: map[string]int64{"a": 3 << 20, "b": 1 << 20}, readers: 4,
			want: map[string]allotment{
				"a": {readers: 2, cacheKiB: floor + (budget-2*floor)*3/4},
				"b": {readers: 2, cacheKiB: floor + (budget-2*floor)/4},
			},
		},
		{
			name:  "files nothing was written to share evenly",
			sizes: map[string]int64{"a": 0, "b": 0, "c": 0, "d": 0}, readers: 4,
			want: map[string]allotment{
				"a": {readers: 2, cacheKiB: floor + (budget-4*floor)/4},
				"b": {readers: 2, cacheKiB: floor + (budget-4*floor)/4},
				"c": {readers: 2, cacheKiB: floor + (budget-4*floor)/4},
				"d": {readers: 2, cacheKiB: floor + (budget-4*floor)/4},
			},
		},
		{
			name:  "a node's readers are spread, rounding up",
			sizes: map[string]int64{"a": 1, "b": 1, "c": 1}, readers: 10,
			want: map[string]allotment{
				"a": {readers: 4, cacheKiB: floor + (budget-3*floor)/3},
				"b": {readers: 4, cacheKiB: floor + (budget-3*floor)/3},
				"c": {readers: 4, cacheKiB: floor + (budget-3*floor)/3},
			},
		},
	} {
		if got := divide(c.sizes, c.readers); !maps.Equal(got, c.want) {
			t.Errorf("%s: divide(%v, %d) = %v, want %v", c.name, c.sizes, c.readers, got, c.want)
		}
	}

	// Past the files the floors can cover, every file keeps its floor
	// rather than the division going negative.
	for name, a := range divide(many, 4) {
		if a.cacheKiB != floor || a.readers != PartitionReadConns {
			t.Errorf("at %d files %s was given %+v, want the floors {%d %d}",
				len(many), name, a, PartitionReadConns, floor)
		}
	}

	for _, sizes := range []map[string]int64{
		{"a": 7, "b": 1 << 40, "c": 12345},
		{"a": 1 << 20, "b": 1 << 21, "c": 1 << 22, "d": 1 << 23, "e": 5},
		{"a": -1, "b": 10},
	} {
		var total int64
		for name, a := range divide(sizes, 4) {
			if a.cacheKiB < floor || a.readers < PartitionReadConns {
				t.Errorf("%v: %s was given %+v, under a floor", sizes, name, a)
			}
			total += a.cacheKiB
		}
		if total > budget {
			t.Errorf("%v: the files were given %d KiB between them, over the "+
				"node's budget of %d", sizes, total, budget)
		}
	}
}

// A LIVE CONNECTION TAKES ITS FILE'S NEW SHARE.
//
// The division runs whenever a partition is opened or closed, and the page
// cache is a per-connection setting. A pinned writer holds its connection for
// the life of its apply loop, so a share that reached only connections opened
// after it would leave every applier on the cache it was opened with — the
// whole budget, once per file. This reads the setting back off the pinned
// connection itself, through the only path an applier has to it.
func TestAPinnedWritersConnectionTakesItsFilesNewShare(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	node, err := OpenNode(ctx, filepath.Join(t.TempDir(), "node.db"), Options{})
	if err != nil {
		t.Fatalf("open the node: %v", err)
	}
	defer func() { _ = node.Close() }()
	first, err := node.OpenPartition(ctx, PartitionFile{Layout: 1, Name: "tracker.000", Logs: 1})
	if err != nil {
		t.Fatalf("open the first partition: %v", err)
	}
	w, err := first.Writer(ctx)
	if err != nil {
		t.Fatalf("pin the first partition's writer: %v", err)
	}
	defer func() { _ = w.Close() }()

	alone := cacheOn(t, w)
	if alone != partitionCacheKiB {
		t.Fatalf("the only open partition's writer caches %d KiB, want the "+
			"whole budget of %d", alone, partitionCacheKiB)
	}
	if _, err := node.OpenPartition(ctx, PartitionFile{Layout: 1, Name: "tracker.001", Logs: 1}); err != nil {
		t.Fatalf("open the second partition: %v", err)
	}
	shared := cacheOn(t, w)
	if want := first.cache.Load(); shared != want {
		t.Errorf("after a second partition opened, the first's pinned writer "+
			"caches %d KiB and its file's share is %d: the connection kept the "+
			"share it was opened with", shared, want)
	}
	if shared >= alone {
		t.Errorf("after a second partition opened, the first's writer still "+
			"caches %d KiB of a %d KiB budget — nothing was divided", shared, alone)
	}
	if err := node.ClosePartition("tracker.001"); err != nil {
		t.Fatalf("close the second partition: %v", err)
	}
	if again := cacheOn(t, w); again != partitionCacheKiB {
		t.Errorf("after the second partition closed, the first's writer caches "+
			"%d KiB, want the whole budget of %d back", again, partitionCacheKiB)
	}
}

// cacheOn is the page cache, in KiB, of the writer's own connection, as the
// engine reports it inside a transaction the writer opened.
func cacheOn(t *testing.T, w *Writer) int64 {
	t.Helper()
	var pages int64
	if err := w.Tx(t.Context(), func(tx *sql.Tx) error {
		return tx.QueryRowContext(t.Context(), `PRAGMA cache_size`).Scan(&pages)
	}); err != nil {
		t.Fatalf("read the writer's cache_size: %v", err)
	}
	// NEGATIVE MEANS KIBIBYTES, which is the form sizeCache sets; a
	// positive answer is a page count, and is not what was asked for.
	if pages >= 0 {
		t.Fatalf("the writer's cache_size is %d pages, not a size in KiB", pages)
	}
	return -pages
}

// THE PARTITION CACHE'S FLOOR IS THE ENGINE'S OWN MINIMUM.
//
// Turso raises any per-connection page cache below its minimum to that
// minimum, silently, so a floor under it is a share the division believes it
// gave and the engine did not. This holds the constant to the engine from both
// sides: one KiB-step below the floor is raised to exactly the floor, and the
// floor itself is kept as asked. A driver that lowers its minimum fails the
// first (the floor can come down with it); one that raises it fails the
// second.
func TestThePartitionCacheFloorIsTheEnginesMinimum(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	node, err := OpenNode(ctx, filepath.Join(t.TempDir(), "node.db"), Options{})
	if err != nil {
		t.Fatalf("open the node: %v", err)
	}
	defer func() { _ = node.Close() }()
	part, err := node.OpenPartition(ctx, PartitionFile{Layout: 1, Name: "tracker.000", Logs: 1})
	if err != nil {
		t.Fatalf("open a partition: %v", err)
	}
	conn, err := part.sql.Conn(ctx)
	if err != nil {
		t.Fatalf("take a connection: %v", err)
	}
	defer func() { _ = conn.Close() }()
	var pageSize int
	if err := conn.QueryRowContext(ctx, `PRAGMA page_size`).Scan(&pageSize); err != nil {
		t.Fatalf("read the page size: %v", err)
	}
	for _, c := range []struct {
		askKiB, wantKiB int
	}{
		{askKiB: partitionCacheFloorKiB - 1, wantKiB: partitionCacheFloorKiB},
		{askKiB: partitionCacheFloorKiB, wantKiB: partitionCacheFloorKiB},
	} {
		if _, err := conn.ExecContext(ctx, fmt.Sprintf("PRAGMA cache_size = %d", -c.askKiB)); err != nil {
			t.Fatalf("ask for %d KiB: %v", c.askKiB, err)
		}
		var raw int
		if err := conn.QueryRowContext(ctx, `PRAGMA cache_size`).Scan(&raw); err != nil {
			t.Fatalf("read the cache size back: %v", err)
		}
		if got := pageCacheKiB(raw, pageSize); got != c.wantKiB {
			t.Errorf("asked for %d KiB of page cache on a %d-byte-page file, the "+
				"engine keeps %d KiB, want %d: partitionCacheFloorKiB is no longer "+
				"the engine's minimum, so re-derive it from what this reads",
				c.askKiB, pageSize, got, c.wantKiB)
		}
	}
}

// CLOSING A HANDLE TWICE GIVES BACK ITS OWN CLAIM ONCE.
//
// The claim on a path is one refcount shared by every handle this process has
// on the file, so a second Close that released it again would drop ANOTHER
// handle's share — and the file would be unlocked while that handle still had
// it open, for the next process to open beside it. A partition is closed by
// its node and may be closed by whoever took it, which is how a second Close
// arrives.
func TestClosingAHandleTwiceGivesBackItsClaimOnce(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "node.db")
	first, err := OpenNode(t.Context(), path, Options{})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	second, err := OpenNode(t.Context(), path, Options{})
	if err != nil {
		t.Fatalf("open a second handle: %v", err)
	}
	defer func() { _ = second.Close() }()
	for range 2 {
		if err := first.Close(); err != nil {
			t.Fatalf("close: %v", err)
		}
	}
	locksHeld.mu.Lock()
	held := locksHeld.by[path]
	locksHeld.mu.Unlock()
	if held == nil || held.holds != 1 {
		holds := 0
		if held != nil {
			holds = held.holds
		}
		t.Errorf("after one handle closed twice, %s is claimed by %d handle(s), "+
			"want the other handle's 1", path, holds)
	}
}

// A CONNECTION DRAWN FROM A HANDLE CLOSED BETWEEN THE GUARD AND THE DRAW
// answers ErrNoEstate too. Every entry point checks the handle is open before
// it starts, and the close can land between that check and the draw; this is
// that second half, taken deterministically by drawing after the close.
func TestADrawOnAHandleClosedUnderItAnswersNoEstate(t *testing.T) {
	t.Parallel()
	db, err := OpenNode(t.Context(), filepath.Join(t.TempDir(), "node.db"), Options{})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	if conn, err := db.conn(t.Context()); !errors.Is(err, ErrNoEstate) {
		if conn != nil {
			_ = conn.Close()
		}
		t.Errorf("a draw on a closed handle = %v, want ErrNoEstate", err)
	}
}

// A DROP REFUSED FOR NAMING ANOTHER FILE LEAVES THE OPEN SET UNTOUCHED.
//
// The set is read without its lock by every lookup a domain makes, and a
// lookup that misses is read by the runtime as a LOST partition, which it
// restores — halting the appliers and resetting their consumers for a file
// that never went anywhere. So a refusal must not take the partition out of
// the set even for the instant it takes to look at it and put it back: the
// set is the same value after the refusal as before it, and a lookup running
// beside a burst of refusals never misses.
func TestARefusedDropLeavesTheOpenSetUntouched(t *testing.T) {
	t.Parallel()
	node, err := OpenNode(t.Context(), filepath.Join(t.TempDir(), "node.db"), Options{})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer func() { _ = node.Close() }()
	file := PartitionFile{Layout: 1, Name: "tracker.000", Logs: 2}
	if _, err := node.OpenPartition(t.Context(), file); err != nil {
		t.Fatalf("open %s: %v", file.Name, err)
	}
	other := file
	other.Logs = 1

	before := node.parts.open.Load()
	if err := node.DropPartition(t.Context(), other); err == nil {
		t.Fatal("a drop naming another file under an open partition's name was not refused")
	}
	if after := node.parts.open.Load(); after != before {
		t.Error("a refused drop replaced the open set, so every lookup between " +
			"the replacement and its undoing answered a partition that stayed " +
			"open as not open")
	}

	stop := make(chan struct{})
	missed := make(chan int)
	go func() {
		n := 0
		for {
			select {
			case <-stop:
				missed <- n
				return
			default:
			}
			if _, err := node.PartitionDB(file.Name); err != nil {
				n++
			}
		}
	}()
	for range 500 {
		_ = node.DropPartition(t.Context(), other)
	}
	close(stop)
	if n := <-missed; n > 0 {
		t.Errorf("%d lookups beside refused drops answered the open partition %s "+
			"as not open", n, file.Name)
	}
	if _, err := os.Stat(node.PartitionPath(file)); err != nil {
		t.Errorf("a refused drop touched the file: %v", err)
	}
}

// A NODE THAT HAS BEEN CLOSED CHANGES ITS SET NO MORE.
//
// [DB.Close] marks the node closed and then empties its set under the set's
// lock, and every gesture that changes the set judges the node open UNDER THE
// SAME LOCK — so an open that raced the close either lands before the set is
// emptied, and is closed with the rest, or finds the node closed. Judged only
// before the lock, an open that passed the check and then waited while the
// close emptied the set would put a file into a set nothing closes again, and
// its pool, its descriptors and this process's claim on it would outlive the
// node. That check is the only one there is, so a gesture on a node already
// closed exercises it: each must refuse with ErrNoEstate, the open must create
// and claim nothing, and the drop must delete nothing.
func TestANodeThatHasBeenClosedChangesItsSetNoMore(t *testing.T) {
	t.Parallel()
	node, err := OpenNode(t.Context(), filepath.Join(t.TempDir(), "node.db"), Options{})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	held := PartitionFile{Layout: 1, Name: "tracker.000", Logs: 1}
	if _, err := node.OpenPartition(t.Context(), held); err != nil {
		t.Fatalf("open %s: %v", held.Name, err)
	}
	if err := node.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	late := PartitionFile{Layout: 1, Name: "tracker.001", Logs: 1}
	if part, err := node.OpenPartition(t.Context(), late); !errors.Is(err, ErrNoEstate) {
		if part != nil {
			_ = part.Close()
		}
		t.Errorf("an open on a closed node = %v, want ErrNoEstate", err)
	}
	path := node.PartitionPath(late)
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("an open on a closed node created %s: %v", path, err)
	}
	locksHeld.mu.Lock()
	claim := locksHeld.by[path]
	locksHeld.mu.Unlock()
	if claim != nil {
		t.Errorf("an open on a closed node left %s claimed by %d handle(s)", path, claim.holds)
	}
	if got := node.OpenPartitions(); len(got) != 0 {
		t.Errorf("a closed node lists %v open", got)
	}

	if err := node.ClosePartition(held.Name); !errors.Is(err, ErrNoEstate) {
		t.Errorf("a close on a closed node = %v, want ErrNoEstate", err)
	}
	if err := node.DropPartition(t.Context(), held); !errors.Is(err, ErrNoEstate) {
		t.Errorf("a drop on a closed node = %v, want ErrNoEstate", err)
	}
	if _, err := os.Stat(node.PartitionPath(held)); err != nil {
		t.Errorf("a drop on a closed node deleted %s: %v", held.Name, err)
	}
}
