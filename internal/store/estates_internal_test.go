package store

import (
	"os"
	"path/filepath"
	"testing"
)

// A FAILURE ON THE SECOND ESTATE TAKES THE FIRST DOWN WITH IT.
//
// A handle on one file and not the other is a node that would apply records
// into a database with no checkpoint table in it, and the caller has no way to
// ask which half it got.
//
// # Why this asserts the REFCOUNT rather than a following Open
//
// The claim on a path is shared per process (see lock.go), so a leaked claim
// still lets the next Open in THIS process through — the leak is invisible
// from outside and only bites a later process, after this one has exited and
// taken the evidence with it. The same reasoning as
// TestPendingReleasesTheLockForTheMigrationThatFollows, and the same
// assertion: no entry, not merely a second caller getting past it.
func TestAFailedReplicatedOpenReleasesTheNodeEstate(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "index.db")

	// A DIRECTORY where the replicated file must be: the driver cannot
	// open it, and a bad `store.replicated_path` is how an operator
	// reaches this in production.
	blocked := filepath.Join(t.TempDir(), "replicated.db")
	if err := os.MkdirAll(blocked, 0o700); err != nil {
		t.Fatalf("stage the blocked path: %v", err)
	}
	if _, err := Open(t.Context(), path, Options{ReplicatedPath: blocked}); err == nil {
		t.Fatal("Open succeeded with an unopenable replicated estate")
	}

	locksHeld.mu.Lock()
	held := locksHeld.by[path]
	locksHeld.mu.Unlock()
	if held != nil {
		t.Fatalf("the node estate kept its claim on %s (%d holder(s)) after the "+
			"replicated estate failed to open — the process never gives the file "+
			"back, and the next one to want it is refused by a lock nothing is "+
			"using", path, held.holds)
	}
}
