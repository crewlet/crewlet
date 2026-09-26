package store_test

import (
	"database/sql"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/store"
)

// A SCRATCH STORE STARTS EMPTY, which is the whole of what it promises.
//
// A node without the `data` role keeps nothing that has to outlive it, and
// the store is where that promise would quietly break: a disposable node
// restarted onto the previous incarnation's rows answers from state nobody
// intended it to keep.
func TestAScratchStoreStartsEmpty(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "scratch.db")
	first, err := store.Open(t.Context(), path, store.Options{Scratch: true, NodeOnly: true})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if err := first.Tx(t.Context(), func(tx *sql.Tx) error {
		_, err := tx.ExecContext(t.Context(), `CREATE TABLE leftover (x INTEGER)`)
		return err
	}); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := first.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	second, err := store.Open(t.Context(), path, store.Options{Scratch: true, NodeOnly: true})
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer second.Close()
	var n int
	if err := second.SQL().QueryRowContext(t.Context(),
		`SELECT COUNT(*) FROM sqlite_master WHERE name = 'leftover'`).Scan(&n); err != nil {
		t.Fatalf("read: %v", err)
	}
	if n != 0 {
		t.Fatal("a scratch store reopened onto the previous incarnation's rows")
	}

	// THE CONTROL: a store that is NOT scratch keeps what it held, or the
	// case above passes on an Open that loses everything.
	if err := second.Tx(t.Context(), func(tx *sql.Tx) error {
		_, err := tx.ExecContext(t.Context(), `CREATE TABLE kept (x INTEGER)`)
		return err
	}); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := second.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	third, err := store.Open(t.Context(), path, store.Options{NodeOnly: true})
	if err != nil {
		t.Fatalf("reopen without scratch: %v", err)
	}
	defer third.Close()
	if err := third.SQL().QueryRowContext(t.Context(),
		`SELECT COUNT(*) FROM sqlite_master WHERE name = 'kept'`).Scan(&n); err != nil {
		t.Fatalf("read: %v", err)
	}
	if n != 1 {
		t.Fatal("a store opened WITHOUT scratch lost what it held")
	}
}

// A SCRATCH OPEN NEVER DELETES A DATABASE SOMEBODY ELSE HOLDS.
//
// The deletion is taken under the store's own lock, so a node told its store
// is scratch and pointed by mistake at a path another engine is running on is
// refused naming the holder — the same refusal a second opener gets — and the
// other engine's rows survive it.
func TestAScratchOpenNeverDeletesADatabaseSomebodyElseHolds(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "held.db")
	held, err := store.Open(t.Context(), path, store.Options{NodeOnly: true})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer held.Close()
	if err := held.Tx(t.Context(), func(tx *sql.Tx) error {
		_, err := tx.ExecContext(t.Context(), `CREATE TABLE precious (x INTEGER)`)
		return err
	}); err != nil {
		t.Fatalf("write: %v", err)
	}

	// ANOTHER PROCESS, which is the case the lock exists for.
	out, err := runHelper(t, path, "scratch")
	if err == nil || !strings.Contains(out, lockedMarker) {
		t.Fatalf("a scratch open from another process over a held store = (%v) %s, "+
			"want it refused by the lock", err, out)
	}

	// AND THIS ONE: the in-process claim is shared, so the lock alone would
	// let a scratch open delete a database a caller here is reading.
	if _, err := store.Open(t.Context(), path, store.Options{Scratch: true, NodeOnly: true}); err == nil {
		t.Fatal("a scratch open over a store this process holds was allowed")
	}

	var n int
	if err := held.SQL().QueryRowContext(t.Context(),
		`SELECT COUNT(*) FROM sqlite_master WHERE name = 'precious'`).Scan(&n); err != nil {
		t.Fatalf("read after the refusals: %v", err)
	}
	if n != 1 {
		t.Fatal("a refused scratch open still deleted the holder's rows")
	}
}

// A NODE-ONLY STORE HAS NO REPLICATED ESTATE, and says so on every use.
//
// A node without the `data` role holds no copy of the replicated estate, and
// a code path that reached for one anyway must be told — an empty database in
// its place reads as a company with nothing in it, which is the answer a seat
// acts on by filing a duplicate.
func TestANodeOnlyStoreAnswersNoEstate(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	db, err := store.Open(t.Context(), filepath.Join(dir, "node.db"), store.Options{NodeOnly: true})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer db.Close()
	if db.Replicated() != nil {
		t.Fatal("a node-only store opened a replicated estate")
	}
	if err := db.Replicated().Read(t.Context(), func(*sql.Tx) error { return nil }); !errors.Is(err, store.ErrNoEstate) {
		t.Errorf("a read through the absent estate = %v, want ErrNoEstate", err)
	}
	if got := db.ReplicatedPath(); got != "" {
		t.Errorf("a node-only store names a replicated path %q it will never have", got)
	}
	matches, _ := filepath.Glob(filepath.Join(dir, "*replicated*"))
	if len(matches) != 0 {
		t.Errorf("a node-only store created %v beside itself", matches)
	}
}
