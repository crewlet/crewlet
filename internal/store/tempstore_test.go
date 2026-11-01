package store_test

import (
	"database/sql"
	"os"
	"path/filepath"
	"testing"

	"github.com/crewlet/crewlet/internal/store"
	"github.com/crewlet/crewlet/internal/store/storetest"
)

// AN OPEN STORE KEEPS NOTHING IN THE TEMPORARY DIRECTORY, because what an open
// store keeps there is exactly what an exit that closes nothing leaves behind
// for good: the watchdog's two exits, a crash, a kill. The driver's own
// default put a temp database in a fresh directory under TMPDIR for every
// connection that took a write transaction, removed only when that connection
// closed, so every such exit left one `.tmpXXXXXX/tursodb-temp.db` per
// connection — eight a node, measured on the child the broker-death test ends
// — and nothing could sweep them, since a shared TMPDIR's entries are anyone's.
//
// THE CONTROL IS THE DRIVER ITSELF, on its own defaults, putting a directory
// there at a BEGIN IMMEDIATE and taking it away at the close: it proves the
// moved TMPDIR reaches the library, so the empty directory the store leaves is
// the store's doing rather than a variable the library never read.
func TestAnOpenStoreKeepsNothingInTheTemporaryDirectory(t *testing.T) {
	path := filepath.Join(t.TempDir(), "crewlet.db")
	tmp := t.TempDir()
	// SEEDED BEFORE TMPDIR MOVES, so the migrated image the files are copied
	// from is built (and its directory removed) where it always is.
	storetest.Seed(t, store.EstateNode, path)
	storetest.Seed(t, store.EstateReplicated, store.ReplicatedPath(path, ""))
	moveTemporaryDirectory(t, tmp)

	node, estate := storetest.OpenEstate(t, path, store.Options{}, 1)
	defer func() { _ = node.Close() }()
	writeThroughEveryPath(t, node, estate)
	if held := entries(t, tmp); len(held) != 0 {
		t.Fatalf("an open store keeps %v in TMPDIR (%s): an exit that closes "+
			"nothing leaves every one of them there for good", held, tmp)
	}

	// The control.
	raw, err := sql.Open("turso", filepath.Join(t.TempDir(), "control.db"))
	if err != nil {
		t.Fatalf("open the control: %v", err)
	}
	defer func() { _ = raw.Close() }()
	conn, err := raw.Conn(t.Context())
	if err != nil {
		t.Fatalf("connect the control: %v", err)
	}
	// Deferred as well as closed below, so a failure in between still closes
	// the connection, which is what takes the control's directory away.
	defer func() { _ = conn.Close() }()
	if _, err := conn.ExecContext(t.Context(), "BEGIN IMMEDIATE"); err != nil {
		t.Fatalf("begin on the control: %v", err)
	}
	if held := entries(t, tmp); len(held) == 0 {
		t.Fatalf("the driver on its own defaults put nothing in TMPDIR (%s) at "+
			"a BEGIN IMMEDIATE, so the moved variable never reached it and the "+
			"empty directory above certified nothing", tmp)
	}
	if _, err := conn.ExecContext(t.Context(), "COMMIT"); err != nil {
		t.Fatalf("commit on the control: %v", err)
	}
	_ = conn.Close()
	_ = raw.Close()
	if held := entries(t, tmp); len(held) != 0 {
		t.Fatalf("the control's close left %v in TMPDIR (%s): the driver no "+
			"longer removes its temp database at a close, which is a second "+
			"leak beside the one this test is about", held, tmp)
	}
}

// A WRITE NEEDS NO TEMPORARY DIRECTORY. The driver's default opened that temp
// database at every connection's first BEGIN IMMEDIATE, which every write
// transaction here takes, so a TMPDIR that is missing or cannot be written — a
// read-only root filesystem with nothing mounted at /tmp — refused every write
// the engine made with "I/O error (tempdir)", for a schema no statement here
// uses.
func TestAWriteNeedsNoTemporaryDirectory(t *testing.T) {
	path := filepath.Join(t.TempDir(), "crewlet.db")
	storetest.Seed(t, store.EstateNode, path)
	storetest.Seed(t, store.EstateReplicated, store.ReplicatedPath(path, ""))
	moveTemporaryDirectory(t, filepath.Join(t.TempDir(), "missing"))

	node, estate := storetest.OpenEstate(t, path, store.Options{}, 1)
	defer func() { _ = node.Close() }()
	writeThroughEveryPath(t, node, estate)
}

// moveTemporaryDirectory points every variable the driver takes its
// temporary directory from at dir: TURSO_TMPDIR, then SQLITE_TMPDIR, for the
// temp database, and TMPDIR for the rest. Moving TMPDIR alone certified
// nothing wherever either of the others is set, since the driver reads them
// first: the missing directory above was never asked for, and the control
// put its directory somewhere this test does not look.
func moveTemporaryDirectory(t *testing.T, dir string) {
	t.Helper()
	for _, name := range []string{"TURSO_TMPDIR", "SQLITE_TMPDIR", "TMPDIR"} {
		t.Setenv(name, dir)
	}
}

// writeThroughEveryPath writes once through each way a write reaches a file:
// the node's pool, the replicated estate's pool and a pinned writer on it —
// each a connection of its own, each taking its own BEGIN IMMEDIATE.
func writeThroughEveryPath(t *testing.T, node *store.DB, estate store.ReplicatedHandle) {
	t.Helper()
	ctx := t.Context()
	write := func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx,
			`CREATE TABLE IF NOT EXISTS temp_probe (v INTEGER)`); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, `INSERT INTO temp_probe (v) VALUES (1)`)
		return err
	}
	if err := node.Tx(ctx, write); err != nil {
		t.Fatalf("a write through the node's pool: %v", err)
	}
	if err := estate.Tx(ctx, write); err != nil {
		t.Fatalf("a write through the replicated estate's pool: %v", err)
	}
	w, err := estate.Writer(ctx)
	if err != nil {
		t.Fatalf("pin a writer on the replicated estate: %v", err)
	}
	defer func() { _ = w.Close() }()
	if err := w.Tx(ctx, write); err != nil {
		t.Fatalf("a write through the pinned writer: %v", err)
	}
}

// entries is every name in dir.
func entries(t *testing.T, dir string) []string {
	t.Helper()
	listed, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("list %s: %v", dir, err)
	}
	names := make([]string, 0, len(listed))
	for _, e := range listed {
		names = append(names, e.Name())
	}
	return names
}
