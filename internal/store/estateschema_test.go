package store_test

import (
	"database/sql"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/store"
)

// The helpers the cases about the SHIPPED replicated schema read it through,
// apart from those cases because several of them certify a domain's own
// declaration and so import the domain, while the helpers are about nothing
// but this package's files.

// migratedEstate brings up a node's replicated estate on a fresh file, with
// every migration applied BY THE MIGRATOR rather than seeded from the binary's
// image: these cases are about what the migrations ship, so the file has to be
// one the migrations made.
func migratedEstate(t *testing.T) store.ReplicatedHandle {
	t.Helper()
	node, err := store.OpenNode(t.Context(), filepath.Join(t.TempDir(), "node.db"),
		store.Options{})
	if err != nil {
		t.Fatalf("open a node's store: %v", err)
	}
	t.Cleanup(func() {
		if err := node.Close(); err != nil {
			t.Errorf("close the store: %v", err)
		}
	})
	if _, err := node.OpenReplicated(t.Context(), 1); err != nil {
		t.Fatalf("open the replicated estate: %v", err)
	}
	return node.Replicated()
}

// estateColumns is one replicated table's column names, sorted.
func estateColumns(t *testing.T, estate store.ReplicatedHandle, table string) []string {
	t.Helper()
	var out []string
	if err := estate.Read(t.Context(), func(tx *sql.Tx) error {
		rows, err := tx.QueryContext(t.Context(),
			`SELECT name FROM pragma_table_info(?)`, table)
		if err != nil {
			return err
		}
		defer func() { _ = rows.Close() }()
		for rows.Next() {
			var name string
			if err := rows.Scan(&name); err != nil {
				return err
			}
			out = append(out, name)
		}
		return rows.Err()
	}); err != nil {
		t.Fatalf("read %s's columns: %v", table, err)
	}
	if len(out) == 0 {
		t.Fatalf("%s has no columns, so it does not exist in the replicated "+
			"estate and every assertion about it would pass vacuously", table)
	}
	slices.Sort(out)
	return out
}

// replicatedDDL is every replicated migration's text, concatenated.
func replicatedDDL(t *testing.T) string {
	t.Helper()
	names := store.SchemaVersions(store.EstateReplicated)
	if len(names) == 0 {
		t.Fatal("the replicated estate ships no migrations, so this guard is " +
			"reading nothing")
	}
	var all strings.Builder
	for _, name := range names {
		body, err := store.SchemaFile(store.EstateReplicated, name)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		all.Write(body)
		all.WriteByte('\n')
	}
	return all.String()
}
