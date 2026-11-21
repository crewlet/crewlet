package store

import (
	"path/filepath"
	"sync"
	"testing"
)

// A DISCARD HOLDS ITS FILE ALONE, both ways, in the one step that claims it.
//
// A scratch open deletes the file before it opens it, so no handle in this
// process may be open on the path while that runs: one already open must
// refuse the discard, and one asking while the discard holds the path must be
// refused rather than share a claim on a file being deleted. Checked and then
// claimed in two steps, an open landing between them shared the claim and had
// its database deleted under it.
//
// Mutation: let a discard's claim be shared, or let a discard share a held
// one, and an open lands on a file being deleted.
func TestADiscardHoldsItsFileAlone(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()

	discarding, err := claimStore(filepath.Join(dir, "discarding.db"), true)
	if err != nil {
		t.Fatalf("claim a path to discard: %v", err)
	}
	defer discarding.release()
	if shared, err := lockStore(filepath.Join(dir, "discarding.db")); err == nil {
		shared.release()
		t.Error("an open shared the claim a discard holds while it deletes the file")
	}

	held, err := lockStore(filepath.Join(dir, "held.db"))
	if err != nil {
		t.Fatalf("claim a path to hold: %v", err)
	}
	defer held.release()
	if alone, err := claimStore(filepath.Join(dir, "held.db"), true); err == nil {
		alone.release()
		t.Error("a discard claimed a path a handle in this process holds")
	}
}

// AND [discard] TAKES THAT CLAIM, holding it until the file is gone.
//
// The case above holds [claimStore] to its rule but not whether discard asks
// for it: look-then-share passes every assertion there and leaves the window
// open. So this holds a discard part way through, claim taken and file not yet
// removed, and opens the path: it must be refused, and free once discard ends.
//
// NO WALL CLOCK: the discard is held inside its own remove, and the case waits
// for that or for the discard returning.
//
// Mutation: put back the look-then-share body, or let discard take the claim
// [lockStore] takes, and the open lands on the file being removed.
func TestADiscardRefusesAnOpenUntilItHasRemovedTheFile(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	path := filepath.Join(t.TempDir(), "scratch.db")
	removing, finish := make(chan struct{}), make(chan struct{})
	var finished sync.Once
	letFinish := func() { finished.Do(func() { close(finish) }) }
	t.Cleanup(letFinish)

	discarded := make(chan error, 1)
	go func() {
		discarded <- discard(path, func(dbPath string) error {
			close(removing)
			<-finish
			return removeDatabaseFiles(dbPath)
		})
	}()
	select {
	case <-removing:
	case err := <-discarded:
		t.Fatalf("the discard returned before it removed anything: %v", err)
	}

	locksHeld.mu.Lock()
	claim := locksHeld.by[path]
	alone := claim != nil && claim.discarding
	locksHeld.mu.Unlock()
	if !alone {
		t.Error("the discard is removing the file under a claim that is not marked " +
			"as a discard's, so any open in this process may share it")
	}
	if db, err := OpenNode(ctx, path, Options{}); err == nil {
		_ = db.Close()
		t.Error("an open of the path succeeded while a discard was removing its file")
	}

	letFinish()
	if err := <-discarded; err != nil {
		t.Fatalf("discard: %v", err)
	}
	db, err := OpenNode(ctx, path, Options{})
	if err != nil {
		t.Fatalf("an open after the discard finished: %v — the discard left its claim behind", err)
	}
	_ = db.Close()
}
