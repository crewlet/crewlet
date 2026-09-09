package store_test

import (
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/store"
)

// A NODE IS TWO DATABASES, and one Open brings both up.
//
// The boundary is a file rather than a naming convention because of what
// rests on it: a snapshot is a copy of the replicated estate, and taken from
// one file it would be a copy of everything followed by a delete — which on a
// driver with no in-place VACUUM leaves the audit log's pages in the artifact
// as free pages, in the transfer, in the checksum and in the integrity check.
func TestOpenBringsUpBothEstates(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	db, err := store.Open(t.Context(), filepath.Join(dir, "index.db"), store.Options{})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer func() { _ = db.Close() }()

	if db.Estate() != store.EstateNode {
		t.Errorf("Open returned the %s estate, want the node's", db.Estate())
	}
	rep := db.Replicated()
	if rep == nil {
		t.Fatal("the node handle has no replicated peer")
	}
	if rep.Estate() != store.EstateReplicated {
		t.Errorf("the peer is the %s estate", rep.Estate())
	}
	if rep.Path() == db.Path() {
		t.Fatal("both estates name one file, so there is no boundary at all")
	}
	if _, err := os.Stat(rep.Path()); err != nil {
		t.Errorf("the replicated estate's file is not on disk: %v", err)
	}

	// NO CHAIN. A caller holding one estate must not be able to walk back
	// to the other's peer and lose track of which file it is writing.
	if rep.Replicated() != nil {
		t.Error("the replicated estate has a peer of its own")
	}

	// TWO SEQUENCES, INDEPENDENTLY NUMBERED. schema_migrations is per
	// file, so `0001` in one estate and `0001` in the other are two
	// migrations and neither can mask the other.
	nodeApplied, err := db.AppliedMigrations(t.Context())
	if err != nil {
		t.Fatalf("node AppliedMigrations: %v", err)
	}
	if len(nodeApplied) != len(store.SchemaVersions(store.EstateNode)) {
		t.Errorf("the node estate applied %d migrations, want %d",
			len(nodeApplied), len(store.SchemaVersions(store.EstateNode)))
	}
	repApplied, err := rep.AppliedMigrations(t.Context())
	if err != nil {
		t.Fatalf("replicated AppliedMigrations: %v", err)
	}
	if len(repApplied) != len(store.SchemaVersions(store.EstateReplicated)) {
		t.Errorf("the replicated estate applied %d migrations, want %d",
			len(repApplied), len(store.SchemaVersions(store.EstateReplicated)))
	}
}

// THEY ARE DIFFERENT DATABASES, not two handles on one.
//
// The whole design rests on it: no transaction spans the two, no read joins
// across them, and a snapshot of one carries nothing of the other. A table
// created in one must therefore be invisible in the other.
func TestTheTwoEstatesAreDifferentDatabases(t *testing.T) {
	t.Parallel()
	db := openBoth(t)
	if err := db.Replicated().Tx(t.Context(), func(tx *sql.Tx) error {
		_, err := tx.ExecContext(t.Context(),
			`CREATE TABLE crewlet_estate_probe (id INTEGER PRIMARY KEY)`)
		return err
	}); err != nil {
		t.Fatalf("create in the replicated estate: %v", err)
	}

	err := db.Read(t.Context(), func(tx *sql.Tx) error {
		var n int
		return tx.QueryRowContext(t.Context(),
			`SELECT count(*) FROM crewlet_estate_probe`).Scan(&n)
	})
	if err == nil {
		t.Fatal("a table created in the replicated estate is readable from the " +
			"node estate, so the two handles are one database")
	}
	if !strings.Contains(strings.ToLower(err.Error()), "crewlet_estate_probe") {
		t.Errorf("the refusal does not name the missing table: %v", err)
	}
}

// CLOSING THE NODE HANDLE CLOSES BOTH, and gives both locks back.
//
// A process holding one estate's lock and not the other's is a state nothing
// asked for and nothing can recover from: the next Open is refused for a file
// nobody is using, and the error names this process.
func TestClosingANodeReleasesBothEstates(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "index.db")
	db, err := store.Open(t.Context(), path, store.Options{})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	replicated := db.Replicated().Path()
	if err := db.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	// Both, reopened independently — which only works if both claims are
	// gone.
	again, err := store.Open(t.Context(), path, store.Options{})
	if err != nil {
		t.Fatalf("reopen after close: %v", err)
	}
	if got := again.Replicated().Path(); got != replicated {
		t.Errorf("the replicated estate moved to %s", got)
	}
	_ = again.Close()
}

// AN EXPLICIT PATH WINS, and the derived one is beside the node's file.
//
// Beside it because the two files are one node's — taken together by a backup
// and lost together with the disk — which is what makes "back up the data
// directory" a true instruction rather than half of one.
func TestTheReplicatedPathIsBesideTheNodesUnlessNamed(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	node := filepath.Join(dir, "index.db")
	if got, want := store.ReplicatedPath(node, ""), filepath.Join(dir, "crewlet-replicated.db"); got != want {
		t.Errorf("derived %q, want %q", got, want)
	}
	elsewhere := filepath.Join(t.TempDir(), "fast.db")
	if got := store.ReplicatedPath(node, elsewhere); got != elsewhere {
		t.Errorf("an explicit path was overridden: %q", got)
	}

	db, err := store.Open(t.Context(), node, store.Options{ReplicatedPath: elsewhere})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer func() { _ = db.Close() }()
	if got := db.Replicated().Path(); got != elsewhere {
		t.Errorf("the replicated estate opened at %q, want %q", got, elsewhere)
	}
}

// PENDING REPORTS BOTH, because a deploy gate that named one estate would
// pass while the other is behind.
func TestPendingReportsEveryEstate(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "index.db")
	schemas, err := store.Pending(t.Context(), path, store.Options{})
	if err != nil {
		t.Fatalf("Pending: %v", err)
	}
	if len(schemas) != len(store.Estates) {
		t.Fatalf("Pending reported %d estates, want %d", len(schemas), len(store.Estates))
	}
	for _, sch := range schemas {
		if len(sch.Applied) != 0 {
			t.Errorf("a fresh %s estate reports %d applied", sch.Estate, len(sch.Applied))
		}
		if len(sch.Pending) != len(store.SchemaVersions(sch.Estate)) {
			t.Errorf("the %s estate has %d pending, want %d",
				sch.Estate, len(sch.Pending), len(store.SchemaVersions(sch.Estate)))
		}
		if sch.Path == "" {
			t.Errorf("the %s estate's report names no file", sch.Estate)
		}
	}
}

// THE SAME FILE FOR BOTH ESTATES IS REFUSED BY THE CONFIG, and this is the
// engine-side half: two exclusive claims on one path do not deadlock, they
// migrate one estate's schema into the other's database.
func TestOneFileForBothEstatesIsRefused(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "index.db")
	_, err := store.Open(t.Context(), path, store.Options{ReplicatedPath: path})
	if err == nil {
		t.Fatal("Open accepted one file as both estates")
	}
	if !errors.Is(err, store.ErrOneFile) {
		t.Errorf("error = %v, want ErrOneFile", err)
	}
}

// openBoth is a node with both estates, closed when the test ends.
func openBoth(t *testing.T) *store.DB {
	t.Helper()
	db, err := store.Open(t.Context(), filepath.Join(t.TempDir(), "index.db"), store.Options{})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}
