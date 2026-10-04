package engine

import (
	"database/sql"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/store"
)

// EVERY DOMAIN'S TABLES ARE IN THE REPLICATED SCHEMA, AND NOTHING ELSE IS.
//
// The replicated estate's file is built from one migration sequence —
// `replicated/` — and every registered domain's log is applied into it. A
// domain that declares a table that sequence does not create has an applier
// that fails on its first record, on every data node at once — and a table the
// sequence creates that no domain declares carries rows no snapshot claims, no
// identity covers and no applier writes, which is the phantom the estate's
// derived-table gates already refuse one level down. So it is EQUALITY: the
// sequence creates the framework's own `statelog_` tables and exactly the
// tables the registered domains declare, nothing else.
//
// HERE rather than in internal/store, because the domains are the ENGINE's
// register, never a copy: a domain registered there is the case this exists
// for, and a store test naming the domains would be a second list of them.
//
// FROM THE DATABASE, NOT FROM THE DDL'S TEXT: sqlite_master is what the
// migrations actually built, after every drop and every rebuild — a pattern
// over the migrations once read a comment's "a CREATE TABLE plus an applier"
// as a table called `plus`.
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
			// schema_migrations is the migrator's, in both estates, and
			// the database engine's own bookkeeping is nobody's: SQLite's
			// `sqlite_` tables and the sequence table Turso keeps behind an
			// AUTOINCREMENT column, which no statement of ours names.
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
// tables — the cursor, the arbitration anchor and the record of operation
// ledgers lost — which the replicated file carries beside every domain's, and
// which no domain declares.
const frameworkTablePrefix = "statelog_"
