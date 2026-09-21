package store_test

import (
	"database/sql"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/chart"
	"github.com/crewlet/crewlet/internal/pages"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/store"
	"github.com/crewlet/crewlet/internal/tracker"
)

// EVERY DOMAIN'S SHIPPED DDL ACCEPTS THE STATEMENTS THE FRAMEWORK GENERATES.
//
// A domain declares three table NAMES and the framework writes their COLUMNS,
// and nothing in Go connects the two. A migration that spelled a column
// differently compiles, migrates, opens and serves every read — and fails the
// first time a record this build cannot decode arrives, which is the rarest
// path in the system and the one whose failure is a stalled log.
//
// # Why this lives in internal/store rather than beside each domain
//
// Two of the three domains reach it through [statelogtest.Run], which runs the
// same check as one of its four suites. The CHART cannot: that suite also runs
// the apply cases, and this domain has no applier yet — the engine's boot check
// refuses a register entry with a nil applier, so a registration cannot land
// before its applier. The check itself needs only a migrated estate and a
// declaration, and the migrated estate is this package's. Running all three
// here also makes the walk two-sided: a domain whose tables stopped being
// created at all would fail here rather than quietly stop being certified.
func TestTheShippedSchemaAcceptsTheFrameworksOwnStatements(t *testing.T) {
	t.Parallel()

	for _, domain := range []statelog.Domain{
		tracker.Domain{}, pages.Domain{}, chart.Domain{},
	} {
		t.Run(domain.Name(), func(t *testing.T) {
			t.Parallel()
			db := openReplicated(t)
			if err := statelog.CheckTables(t.Context(), db, domain); err != nil {
				t.Fatalf("the replicated estate's migrations do not carry the "+
					"shape %s's log machinery needs: %v", domain.Name(), err)
			}
		})
	}
}

// THE CHART'S OPS LEDGER TAKES THE FRAMEWORK'S FOUR COLUMNS AND NOTHING ELSE.
//
// `0001_the_state_log_lands.sql` documents the shape and the framework writes
// the statements, so a fifth column is not a compile error and not a migration
// failure — it is a column nothing ever populates, and an index over it is an
// index nothing ever uses. The one that would be tempting is `kind`, with an
// index on (kind, applied_at) so a structural record's op id could be swept on
// a different schedule from a content record's. That is a SECOND OPS HORIZON,
// and this domain declares one: two horizons on one ledger is a retry that
// resolves against a history half of which has been deleted.
//
// The `applied_at` index is asserted in the same case rather than a separate
// one, because its absence is the mirror failure: the sweep is a range delete
// over the age, and without the index a node returning from a month away scans
// the whole table on every tick and deletes its whole month in one statement
// holding this store's only writer.
func TestTheChartOpsLedgerCarriesTheFrameworksFourColumns(t *testing.T) {
	t.Parallel()

	db := openReplicated(t)
	got := columnsOf(t, db, chart.Domain{}.OpsTable())
	want := []string{"applied_at", "op_id", "position", "subject"}
	if !slices.Equal(got, want) {
		t.Errorf("%s has columns %v, want exactly %v — the framework writes the "+
			"statements that fill this table, so a column it does not know is "+
			"one nothing ever populates",
			chart.Domain{}.OpsTable(), got, want)
	}

	// The index, read from the estate's own DDL rather than from a
	// PRAGMA: the assertion is about what the migration SHIPS, and a
	// deployment that had somehow acquired the index another way would
	// still leave the next fresh database without it.
	ddl := replicatedDDL(t)
	if !strings.Contains(ddl, "ON chart_ops (applied_at)") {
		t.Error("no index over chart_ops (applied_at) is shipped — the ops sweep " +
			"is a range delete over the age, and a range delete ships its index")
	}
	if strings.Contains(ddl, "chart_ops (kind") || strings.Contains(ddl, "chart_ops(kind") {
		t.Error("an index over chart_ops keyed on a kind is shipped — this domain " +
			"declares ONE ops horizon, and an index implying a second is how the " +
			"second gets written")
	}
}

// AND THE TWO COLUMNS A LATER CHANGE CANNOT ADD ARE HERE NOW.
//
// schema_migrations keys on the FILENAME, so editing a migration that has
// already run silently never re-runs it: every database that applied it keeps
// the old shape while the code assumes the new one. `former_keys_json` and
// `email_index` are both columns the applier fills from the first record it
// ever writes, so they have to ship in the migration that creates the table
// rather than in the one that starts using them.
func TestTheChartTablesShipTheColumnsAMigrationCannotAddLater(t *testing.T) {
	t.Parallel()

	db := openReplicated(t)
	for table, column := range map[string]string{
		"chart_units": "former_keys_json",
		"chart_seats": "email_index",
		// And the additive column on a table this domain does not own,
		// beside the epoch guard that stays and stays written until a
		// later change moves that writer.
		"tracker_projects": "chart_position",
	} {
		if cols := columnsOf(t, db, table); !slices.Contains(cols, column) {
			t.Errorf("%s does not carry %s — an applied migration is history, "+
				"not source, so no later file can add it to a database that has "+
				"already run this one", table, column)
		}
	}
	// The one it sits beside is still there. A column removed in the same
	// change that adds its successor would leave the writer that fills it
	// writing into nothing.
	if cols := columnsOf(t, db, "tracker_projects"); !slices.Contains(cols, "chart_epoch") {
		t.Error("tracker_projects no longer carries chart_epoch — the tracker's " +
			"own ApplyChart reconcile is still its writer, and dropping it is a " +
			"later change with a migration of its own")
	}
}

// EVERY TABLE THE CHART DOMAIN DECLARES IS ONE THE ESTATE ACTUALLY SHIPS.
//
// The declaration is what the scrub list, the identity claim and the local
// sweep are all derived from, so a name in it that no migration creates is
// three lists that are silently short — and a table the migration creates that
// the domain does not classify joins whichever behaviour its absence resembled.
// Both directions, for internal/skipgate's reason.
func TestTheChartDomainDeclaresExactlyTheTablesItShips(t *testing.T) {
	t.Parallel()

	shipped := tablesIn(t, store.EstateReplicated)
	declared := chart.Domain{}.Tables()
	if len(declared) == 0 {
		t.Fatal("the chart domain declares no tables, so this guard checks nothing")
	}
	for name := range declared {
		if !shipped[name] {
			t.Errorf("the chart domain declares %s and no replicated migration "+
				"creates it", name)
		}
	}
	for name := range shipped {
		if !strings.HasPrefix(name, "chart_") {
			continue
		}
		if _, ok := declared[name]; !ok {
			t.Errorf("%s is shipped and the chart domain does not classify it — "+
				"the scrub list, the identity claim and the sweep are all derived "+
				"from that map, so a table missing from it joins whichever "+
				"behaviour its absence resembled", name)
		}
	}
}

// openReplicated brings up a fresh store with every migration applied.
func openReplicated(t *testing.T) *store.DB {
	t.Helper()
	db, err := store.Open(t.Context(), filepath.Join(t.TempDir(), "node.db"),
		store.Options{PinnedWriters: 1})
	if err != nil {
		t.Fatalf("open a store: %v", err)
	}
	t.Cleanup(func() {
		if err := db.Close(); err != nil {
			t.Errorf("close the store: %v", err)
		}
	})
	return db
}

// columnsOf is one table's column names, sorted.
func columnsOf(t *testing.T, db *store.DB, table string) []string {
	t.Helper()
	var out []string
	if err := db.Replicated().Read(t.Context(), func(tx *sql.Tx) error {
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
