package storetest_test

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/store"
	"github.com/crewlet/crewlet/internal/store/storetest"
)

// A SEEDED ESTATE IS WHAT A FRESH MIGRATION LEAVES — the same applied
// sequence, the same schema object for object, and the same rows in every
// table, for both estates.
//
// The rows are the half a schema comparison cannot see. A migration that
// wrote something particular to the file it ran in — a minted identity, a
// clock reading outside a view — would make every fixture seeded from one
// image share it, and two tests that were independent on fresh files would
// stop being so. The ledger's applied_at is the one column that legitimately
// differs, and the only one left out.
//
// Mutation: build the image from the node estate alone, or seed one estate's
// image as the other's, and the schemas differ.
func TestASeededEstateIsWhatAFreshMigrationLeaves(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	fresh, err := store.OpenNode(ctx, filepath.Join(t.TempDir(), "fresh.db"), store.Options{})
	if err != nil {
		t.Fatalf("open a fresh store: %v", err)
	}
	defer func() { _ = fresh.Close() }()
	freshReplicated, err := fresh.OpenReplicated(ctx, 1)
	if err != nil {
		t.Fatalf("open a fresh replicated estate: %v", err)
	}
	seeded, estate := storetest.OpenEstate(t, filepath.Join(t.TempDir(), "seeded.db"), store.Options{}, 1)
	defer func() { _ = seeded.Close() }()
	seededReplicated := storetest.ReplicatedDB(t, estate)

	for _, pair := range []struct {
		estate        store.Estate
		fresh, seeded *store.DB
	}{
		{store.EstateNode, fresh, seeded},
		{store.EstateReplicated, freshReplicated, seededReplicated},
	} {
		applied, err := pair.seeded.AppliedMigrations(ctx)
		if err != nil {
			t.Fatalf("the seeded %s estate's ledger: %v", pair.estate, err)
		}
		if want := store.SchemaVersions(pair.estate); !slices.Equal(applied, want) {
			t.Errorf("the seeded %s estate has applied %v, want this binary's "+
				"whole sequence %v", pair.estate, applied, want)
		}
		if got, want := schemaOf(t, pair.seeded), schemaOf(t, pair.fresh); !slices.Equal(got, want) {
			t.Errorf("the seeded %s estate's schema differs from a fresh one's:\n"+
				"seeded: %v\nfresh:  %v", pair.estate, got, want)
		}
		for _, table := range tablesOf(t, pair.fresh) {
			if got, want := rowsOf(t, pair.seeded, table), rowsOf(t, pair.fresh, table); !slices.Equal(got, want) {
				t.Errorf("the seeded %s estate's %s holds %v, a fresh one's %v",
					pair.estate, table, got, want)
			}
		}
	}
}

// A SEEDED STORE MIGRATES NOTHING: its ledger is the image's, so two stores
// seeded at different moments carry the same instant for every version — which
// two migrations, each stamping the time it ran, never could.
//
// It is the half the comparison above cannot see. A seed that wrote nothing,
// or wrote a file the driver did not recognise as a database, would leave the
// open to migrate a fresh file, and every schema and row would still match:
// correct, and exactly as slow as the image exists to stop.
//
// Mutation: seed an empty file, or skip the write, and the two ledgers carry
// the instants of two separate migrations.
func TestASeededStoreMigratesNothing(t *testing.T) {
	t.Parallel()
	first := storetest.OpenNode(t, filepath.Join(t.TempDir(), "first.db"), store.Options{})
	defer func() { _ = first.Close() }()
	second := storetest.OpenNode(t, filepath.Join(t.TempDir(), "second.db"), store.Options{})
	defer func() { _ = second.Close() }()

	got, want := ledgerOf(t, second), ledgerOf(t, first)
	if len(want) == 0 {
		t.Fatal("a seeded store's ledger is empty, so this case can tell nothing apart")
	}
	if !slices.Equal(got, want) {
		t.Errorf("two seeded stores carry different ledgers, so at least one of "+
			"them migrated on open:\nfirst:  %v\nsecond: %v", want, got)
	}
}

// A DATABASE THAT IS THERE IS OPENED AS IT IS: a test reopening its own file,
// or handing in a replicated copy it advanced itself, must get that file — the
// engine's rejoin cases hand OpenEstate a donor's rows under
// store.replicated_path, and a seed written over them would serve an empty
// estate instead.
//
// Mutation: write the image whatever is at the path, and the rows below are
// gone.
func TestASeedLeavesADatabaseThatIsThereAlone(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	dir := t.TempDir()
	path := filepath.Join(dir, "node.db")
	donor := filepath.Join(dir, "donor", "rows.db")
	node, estate := storetest.OpenEstate(t, path, store.Options{ReplicatedPath: donor}, 1)
	for _, db := range []*store.DB{node, storetest.ReplicatedDB(t, estate)} {
		if err := db.Tx(ctx, func(tx *sql.Tx) error {
			_, err := tx.ExecContext(ctx, `CREATE TABLE storetest_kept (id INTEGER PRIMARY KEY)`)
			return err
		}); err != nil {
			t.Fatalf("mark %s: %v", db.Path(), err)
		}
	}
	if err := node.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	// FOLDED, so each mark is in its main file alone. Left in the -wal, it
	// would be replayed onto whatever main file a seed wrote over the old
	// one, and an overwrite would pass for a file left alone.
	for _, file := range []string{path, donor} {
		if err := store.QuiesceCopy(ctx, file); err != nil {
			t.Fatalf("fold %s: %v", file, err)
		}
	}

	again, estate := storetest.OpenEstate(t, path, store.Options{ReplicatedPath: donor}, 1)
	defer func() { _ = again.Close() }()
	for _, db := range []*store.DB{again, storetest.ReplicatedDB(t, estate)} {
		if !slices.Contains(tablesOf(t, db), "storetest_kept") {
			t.Errorf("%s was replaced by the image on a reopen: the table this "+
				"test created in it is gone", db.Path())
		}
	}
}

// SEEDING FOLLOWS WHERE THE CALLER WILL OPEN: OpenEstate seeds the replicated
// estate where store.replicated_path puts it and nowhere else, and OpenNode
// seeds no replicated estate at all — a file beside a node opened alone is one
// a fresh node would not have, and Pending, a backup or an estate-presence
// check would find it.
//
// Mutation: seed the replicated image beside the node whatever the
// configuration says, or from OpenNode, and a file appears where none should.
func TestSeedingFollowsWhereTheCallerWillOpen(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	alone := storetest.OpenNode(t, filepath.Join(dir, "alone", "node.db"), store.Options{})
	defer func() { _ = alone.Close() }()
	if _, err := os.Stat(alone.ReplicatedFile()); !os.IsNotExist(err) {
		t.Errorf("a node opened alone has a replicated estate beside it at %s (%v)",
			alone.ReplicatedFile(), err)
	}

	configured := filepath.Join(dir, "elsewhere", "rows.db")
	node, _ := storetest.OpenEstate(t, filepath.Join(dir, "node", "node.db"),
		store.Options{ReplicatedPath: configured}, 1)
	defer func() { _ = node.Close() }()
	if node.ReplicatedFile() != configured {
		t.Fatalf("the node keeps its replicated estate at %s, want the configured %s",
			node.ReplicatedFile(), configured)
	}
	beside := store.ReplicatedPath(node.Path(), "")
	if _, err := os.Stat(beside); !os.IsNotExist(err) {
		t.Errorf("a replicated estate was seeded at the default %s although the "+
			"node keeps its own at %s (%v)", beside, configured, err)
	}
}

// schemaOf is every object the file's schema holds, as the driver keeps it.
func schemaOf(t *testing.T, db *store.DB) []string {
	t.Helper()
	return query(t, db, `SELECT type, name, tbl_name, coalesce(sql, '')
		FROM sqlite_master ORDER BY type, name`)
}

// tablesOf is every table the file holds, the driver's own excluded.
func tablesOf(t *testing.T, db *store.DB) []string {
	t.Helper()
	var names []string
	if err := db.Read(t.Context(), func(tx *sql.Tx) error {
		rows, err := tx.QueryContext(t.Context(), `SELECT name FROM sqlite_master
			WHERE type = 'table' AND name NOT LIKE 'sqlite_%' ORDER BY name`)
		if err != nil {
			return err
		}
		defer func() { _ = rows.Close() }()
		for rows.Next() {
			var name string
			if err := rows.Scan(&name); err != nil {
				return err
			}
			names = append(names, name)
		}
		return rows.Err()
	}); err != nil {
		t.Fatalf("list the tables of %s: %v", db.Path(), err)
	}
	return names
}

// rowsOf is a table's rows, each rendered whole and the set sorted, so two
// files holding the same rows in a different physical order compare equal.
// The ledger is read without applied_at, the one column two migrations of the
// same sequence legitimately disagree on.
func rowsOf(t *testing.T, db *store.DB, table string) []string {
	t.Helper()
	if table == "schema_migrations" {
		return query(t, db, `SELECT version FROM schema_migrations ORDER BY version`)
	}
	rows := query(t, db, `SELECT * FROM "`+strings.ReplaceAll(table, `"`, `""`)+`"`)
	slices.Sort(rows)
	return rows
}

// ledgerOf is the migration ledger whole, applied_at included.
func ledgerOf(t *testing.T, db *store.DB) []string {
	t.Helper()
	return query(t, db, `SELECT version, applied_at FROM schema_migrations ORDER BY version`)
}

// query renders each row of q as one string of its columns.
func query(t *testing.T, db *store.DB, q string) []string {
	t.Helper()
	var out []string
	if err := db.Read(t.Context(), func(tx *sql.Tx) error {
		rows, err := tx.QueryContext(t.Context(), q)
		if err != nil {
			return err
		}
		defer func() { _ = rows.Close() }()
		cols, err := rows.Columns()
		if err != nil {
			return err
		}
		for rows.Next() {
			cells := make([]any, len(cols))
			into := make([]any, len(cols))
			for i := range cells {
				into[i] = &cells[i]
			}
			if err := rows.Scan(into...); err != nil {
				return err
			}
			out = append(out, fmt.Sprintf("%v", cells))
		}
		return rows.Err()
	}); err != nil {
		t.Fatalf("%s on %s: %v", q, db.Path(), err)
	}
	return out
}
