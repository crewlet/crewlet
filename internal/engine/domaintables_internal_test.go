package engine

import (
	"database/sql"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/store"
)

// EVERY DOMAIN'S TABLES ARE IN THE REPLICATED SCHEMA, AND NOTHING ELSE IS: the
// `replicated/` migrations create the framework's `statelog_` tables and
// exactly the tables the registered domains declare. A declared table they do
// not create fails its applier on the first record on every data node; a
// created table no domain declares holds rows no snapshot claims.
//
// HERE rather than in internal/store, because the domains are the engine's
// register, and a store test naming them would be a second list.
//
// FROM THE DATABASE, NOT THE DDL'S TEXT: sqlite_master is what the migrations
// built after every drop and rebuild; a pattern over the text misreads it.
func TestEveryDomainTableIsInTheReplicatedSchema(t *testing.T) {
	t.Parallel()
	db, err := store.OpenEstate(t.Context(), store.EstateReplicated,
		filepath.Join(t.TempDir(), "replicated.db"), store.Options{})
	if err != nil {
		t.Fatalf("apply the replicated schema: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	schema := map[string]bool{}
	err = db.Read(t.Context(), func(tx *sql.Tx) error {
		rows, err := tx.Query(`SELECT name FROM sqlite_master WHERE type = 'table'`)
		if err != nil {
			return err
		}
		defer func() { _ = rows.Close() }()
		for rows.Next() {
			var name string
			if err := rows.Scan(&name); err != nil {
				return err
			}
			name = strings.ToLower(name)
			// The migrator's table, and the database engine's own
			// bookkeeping: SQLite's `sqlite_` tables and Turso's
			// AUTOINCREMENT sequence table.
			if name == "schema_migrations" || strings.HasPrefix(name, "sqlite_") ||
				strings.HasPrefix(name, "__turso_internal_") {
				continue
			}
			schema[name] = true
		}
		return rows.Err()
	})
	if err != nil {
		t.Fatalf("read the replicated schema's tables: %v", err)
	}

	framework := map[string]bool{}
	for name := range schema {
		if strings.HasPrefix(name, frameworkTablePrefix) {
			framework[name] = true
		}
	}
	if len(framework) == 0 {
		t.Fatalf("no %s table in the replicated schema, so the framework's "+
			"own checkpoint has nowhere to commit — or this derivation is "+
			"reading the wrong schema", frameworkTablePrefix)
	}

	declared := map[string]bool{}
	for _, d := range registeredDomains() {
		for table := range d.Tables() {
			declared[table] = true
			if strings.HasPrefix(table, frameworkTablePrefix) {
				t.Errorf("the domain %s declares %s, a table in the framework's own "+
					"namespace — a domain's rows and the framework's checkpoint "+
					"would be one claim", d.Name(), table)
			}
			if !schema[table] {
				t.Errorf("the domain %s declares the table %s, and the replicated "+
					"schema does not create it: the domain's applier fails on its "+
					"first record on every data node", d.Name(), table)
			}
		}
	}
	var unclaimed []string
	for table := range schema {
		if !framework[table] && !declared[table] {
			unclaimed = append(unclaimed, table)
		}
	}
	slices.Sort(unclaimed)
	if len(unclaimed) > 0 {
		t.Errorf("the replicated schema creates %v, which no registered domain "+
			"declares and which is not the framework's: rows there are in no "+
			"snapshot claim and no applier's scope. Declare each in its domain's "+
			"Tables(), or drop it with a new migration", unclaimed)
	}
}

// frameworkTablePrefix is the namespace of the state-log framework's own
// tables (the cursor, the arbitration anchor, the lost-ledger record), which
// no domain declares.
const frameworkTablePrefix = "statelog_"
