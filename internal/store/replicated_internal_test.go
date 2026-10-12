package store

import (
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

// CLOSING A NODE GIVES BACK ITS REPLICATED ESTATE'S CLAIM.
//
// The claim on a path is shared per process (see lock.go), so a leaked claim
// still lets the next open in THIS process through — the leak is invisible
// from outside and only bites a later process, after this one has exited and
// taken the evidence with it. So this asserts the claims themselves, as
// TestPendingReleasesTheLockForTheMigrationThatFollows does: no entry, not
// merely a second caller getting past it.
func TestClosingANodeGivesBackItsReplicatedEstatesClaim(t *testing.T) {
	t.Parallel()
	node, err := OpenNode(t.Context(), filepath.Join(t.TempDir(), "node.db"), Options{})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	replicated, err := node.OpenReplicated(t.Context(), 2)
	if err != nil {
		t.Fatalf("open the replicated estate: %v", err)
	}
	if err := node.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	locksHeld.mu.Lock()
	defer locksHeld.mu.Unlock()
	for _, path := range []string{replicated.Path(), node.Path()} {
		if held := locksHeld.by[path]; held != nil {
			t.Errorf("%s is still claimed by %d handle(s) after its node closed",
				path, held.holds)
		}
	}
}

// A REPLICATED ESTATE THAT FAILS TO OPEN LEAVES NOTHING HELD, and the node as
// it was.
//
// A file the driver cannot open — a directory where it should be, a bad
// `store.replicated_path` — must leave the node serving what it has: its own
// file. And the file that failed must not stay claimed by a handle nobody got,
// or the next process to want it is refused by a lock nothing is using; nor
// may the slot answer a handle, or every replicated read would reach a file
// that never opened. The claim is asserted rather than a following open, for
// [TestClosingANodeGivesBackItsReplicatedEstatesClaim]'s reason.
func TestAFailedReplicatedOpenLeavesTheNodeAsItWas(t *testing.T) {
	t.Parallel()
	node, err := OpenNode(t.Context(), filepath.Join(t.TempDir(), "node.db"), Options{})
	if err != nil {
		t.Fatalf("open the node: %v", err)
	}
	defer func() { _ = node.Close() }()
	path := node.ReplicatedFile()
	if err := os.MkdirAll(path, 0o700); err != nil {
		t.Fatalf("put a directory where the replicated estate's file goes: %v", err)
	}
	if _, err := node.OpenReplicated(t.Context(), 1); err == nil {
		t.Fatal("a replicated estate whose file is a directory opened")
	}

	locksHeld.mu.Lock()
	leaked := locksHeld.by[path]
	locksHeld.mu.Unlock()
	if leaked != nil {
		t.Errorf("the failed open kept its claim on %s (%d holder(s))", path, leaked.holds)
	}
	if _, err := node.ReplicatedDB(); !errors.Is(err, ErrNoEstate) {
		t.Errorf("after a failed open the node answers its replicated estate with "+
			"%v, want ErrNoEstate", err)
	}
	if err := node.Read(t.Context(), func(*sql.Tx) error { return nil }); err != nil {
		t.Errorf("the node's own file is unreadable after a failed open: %v", err)
	}
}

// EVERY CONNECTION OF THE REPLICATED ESTATE KEEPS THE NODE'S PAGE CACHE, a
// pinned writer's included.
//
// The page cache is a per-connection setting, and a pinned writer holds its
// connection for the life of its apply loop — so a size that reached only
// the pool's readers, or only connections a later statement set, would leave
// every applier on the driver's own default. This reads the setting back off
// the pinned connection itself, through the only path an applier has to it.
func TestAPinnedWritersConnectionKeepsTheNodesPageCache(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	node, err := OpenNode(ctx, filepath.Join(t.TempDir(), "node.db"), Options{})
	if err != nil {
		t.Fatalf("open the node: %v", err)
	}
	defer func() { _ = node.Close() }()
	replicated, err := node.OpenReplicated(ctx, 1)
	if err != nil {
		t.Fatalf("open the replicated estate: %v", err)
	}
	w, err := replicated.Writer(ctx)
	if err != nil {
		t.Fatalf("pin the replicated estate's writer: %v", err)
	}
	defer func() { _ = w.Close() }()
	if got := cacheOn(t, w); got != nodeCacheKiB {
		t.Errorf("the replicated estate's pinned writer caches %d KiB, want %d",
			got, nodeCacheKiB)
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
	// NEGATIVE MEANS KIBIBYTES, which is the form openPool sets; a positive
	// answer is a page count, and is not what was asked for.
	if pages >= 0 {
		t.Fatalf("the writer's cache_size is %d pages, not a size in KiB", pages)
	}
	return -pages
}

// CLOSING A HANDLE TWICE GIVES BACK ITS OWN CLAIM ONCE.
//
// The claim on a path is one refcount shared by every handle this process has
// on the file, so a second Close that released it again would drop ANOTHER
// handle's share — and the file would be unlocked while that handle still had
// it open, for the next process to open beside it. The replicated estate is
// closed by its node and may be closed by whoever took it, which is how a
// second Close arrives.
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
	// AND EVERY STATEMENT BEHIND A GUARD, entered past it, which is where
	// such a close lands: the snapshot's copy and the ledger read each take
	// their connection by the same draw rather than from the pool.
	if err := db.vacuumInto(t.Context(), filepath.Join(t.TempDir(), "copy.db")); !errors.Is(err, ErrNoEstate) {
		t.Errorf("a copy begun on a handle closed after Backup's guard = %v, want ErrNoEstate", err)
	}
	if _, err := db.appliedVersions(t.Context()); !errors.Is(err, ErrNoEstate) {
		t.Errorf("a ledger read begun on a handle closed after AppliedMigrations' "+
			"guard = %v, want ErrNoEstate", err)
	}
}

// A NODE THAT HAS BEEN CLOSED CHANGES ITS SLOT NO MORE.
//
// [DB.Close] marks the node closed and then empties its slot under the slot's
// lock, and every gesture that changes the slot judges the node open UNDER
// THE SAME LOCK — so an open that raced the close either lands before the slot
// is emptied, and is closed with the rest, or finds the node closed. Judged
// only before the lock, an open that passed the check and then waited while
// the close emptied the slot would put a file into a slot nothing closes
// again, and its pool, its descriptors and this process's claim on it would
// outlive the node. That check is the only one there is, so a gesture on a
// node already closed exercises it: each must refuse with ErrNoEstate, and
// the open must create and claim nothing.
func TestANodeThatHasBeenClosedChangesItsSlotNoMore(t *testing.T) {
	t.Parallel()
	node, err := OpenNode(t.Context(), filepath.Join(t.TempDir(), "node.db"), Options{})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if err := node.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	if replicated, err := node.OpenReplicated(t.Context(), 1); !errors.Is(err, ErrNoEstate) {
		if replicated != nil {
			_ = replicated.Close()
		}
		t.Errorf("an open on a closed node = %v, want ErrNoEstate", err)
	}
	path := node.ReplicatedFile()
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("an open on a closed node created %s: %v", path, err)
	}
	locksHeld.mu.Lock()
	claim := locksHeld.by[path]
	locksHeld.mu.Unlock()
	if claim != nil {
		t.Errorf("an open on a closed node left %s claimed by %d handle(s)", path, claim.holds)
	}
	if _, err := node.ReplicatedDB(); !errors.Is(err, ErrNoEstate) {
		t.Errorf("a closed node answers its replicated estate with %v, want ErrNoEstate", err)
	}

	if err := node.CloseReplicated(); !errors.Is(err, ErrNoEstate) {
		t.Errorf("a close on a closed node = %v, want ErrNoEstate", err)
	}
}

// A WIDTH LEARNED WHILE THE REPLICATED ESTATE IS BEING OPENED IS NOT LOST.
//
// [DB.OpenReplicated] reads the node's width and publishes the handle under
// the slot's lock, and opening a file takes long enough for an apply to learn
// a width in between. Read without that lock, the learner found the slot
// empty while the open had already taken width 0, and the estate came up with
// its dimension guard off for good — the two-widths state
// [DB.LearnEmbeddingDim] exists to prevent. So the width is learned while an
// adoption's reopen HOLDS the lock, and the reopened handle must carry it.
func TestAWidthLearnedDuringAReopenReachesTheReopenedEstate(t *testing.T) {
	t.Parallel()
	node, err := OpenNode(t.Context(), filepath.Join(t.TempDir(), "node.db"), Options{})
	if err != nil {
		t.Fatalf("open the node: %v", err)
	}
	defer func() { _ = node.Close() }()
	if _, err := node.OpenReplicated(t.Context(), 1); err != nil {
		t.Fatalf("open the replicated estate: %v", err)
	}
	if err := node.CloseReplicated(); err != nil {
		t.Fatalf("close the replicated estate: %v", err)
	}

	reopened := make(chan error, 1)
	go func() {
		_, err := node.OpenReplicated(t.Context(), 1)
		reopened <- err
	}()
	// Learn once the reopen holds the lock — or has already finished, which
	// is the interleaving that was always right and still is.
	for node.slot.mu.TryLock() {
		node.slot.mu.Unlock()
		if node.slot.open.Load() != nil {
			break
		}
		runtime.Gosched()
	}
	node.LearnEmbeddingDim(64)
	if err := <-reopened; err != nil {
		t.Fatalf("reopen the replicated estate: %v", err)
	}
	held := node.slot.open.Load()
	if held == nil {
		t.Fatal("the reopened estate is not in the node's slot")
	}
	if got := held.EmbeddingDim(); got != 64 {
		t.Errorf("a replicated estate reopened while the node learned its width "+
			"reports %d, want 64: its dimension guard is off", got)
	}
}
