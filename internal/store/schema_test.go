package store_test

import (
	"database/sql"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/chart"
	"github.com/crewlet/crewlet/internal/iamdomain"
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
// Two of the four domains reach it through [statelogtest.Run], which runs the
// same check as one of its four suites. The CHART and the IAM domain cannot:
// that suite also runs the apply cases, and a domain has no applier on the
// change that declares it — the engine's boot check refuses a register entry
// with a nil applier, so a registration cannot land before its applier. The
// check itself needs only a migrated estate and a declaration, and the migrated
// estate is this package's. Running all four here also makes the walk
// two-sided: a domain whose tables stopped being created at all would fail here
// rather than quietly stop being certified.
func TestTheShippedSchemaAcceptsTheFrameworksOwnStatements(t *testing.T) {
	t.Parallel()

	for _, domain := range []statelog.Domain{
		tracker.Domain{}, pages.Domain{}, chart.Domain{}, iamdomain.Domain{},
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
		// which is the tracker's chart guard now that the writer has
		// moved onto it.
		"tracker_projects": "chart_position",
	} {
		if cols := columnsOf(t, db, table); !slices.Contains(cols, column) {
			t.Errorf("%s does not carry %s — an applied migration is history, "+
				"not source, so no later file can add it to a database that has "+
				"already run this one", table, column)
		}
	}
	// AND THE ONE IT REPLACED IS GONE. A guard column with no writer is
	// worse than an absent one: it reads as a fact about the row, a later
	// reconcile is tempted to compare it, and what it actually holds is a
	// wall-clock reading from whichever node last ran the old build.
	if cols := columnsOf(t, db, "tracker_projects"); slices.Contains(cols, "chart_epoch") {
		t.Error("tracker_projects still carries chart_epoch — nothing writes it " +
			"since the chart guard moved onto chart_position, and a column no " +
			"writer fills is a value every reader is entitled to misread")
	}
}

// EVERY TABLE A DOMAIN DECLARES IS ONE THE ESTATE ACTUALLY SHIPS, AND BACK.
//
// The declaration is what the scrub list, the identity claim and the local
// sweep are all derived from, so a name in it that no migration creates is
// three lists that are silently short — and a table the migration creates that
// the domain does not classify joins whichever behaviour its absence resembled.
// Both directions, for internal/skipgate's reason.
//
// The reverse walk is keyed on the table PREFIX each domain names its tables
// with, which is a convention rather than a rule the framework enforces — so
// the case also asserts that every declared table actually carries it, or a
// domain that renamed one out of its own prefix would silently stop being
// walked in the direction that catches an unclassified table.
func TestEachDomainDeclaresExactlyTheTablesItShips(t *testing.T) {
	t.Parallel()

	shipped := tablesIn(t, store.EstateReplicated)
	for _, tc := range []struct {
		prefix string
		domain statelog.Domain
	}{
		{"chart_", chart.Domain{}},
		{"iam_", iamdomain.Domain{}},
	} {
		t.Run(tc.domain.Name(), func(t *testing.T) {
			t.Parallel()
			declared := tc.domain.Tables()
			if len(declared) == 0 {
				t.Fatalf("the %s domain declares no tables, so this guard "+
					"checks nothing", tc.domain.Name())
			}
			for name := range declared {
				if !shipped[name] {
					t.Errorf("the %s domain declares %s and no replicated "+
						"migration creates it", tc.domain.Name(), name)
				}
				if !strings.HasPrefix(name, tc.prefix) {
					t.Errorf("the %s domain declares %s, which is outside the "+
						"%q prefix this walk reads the estate by — a table "+
						"renamed out of its domain's prefix stops being "+
						"checked in the direction that catches an "+
						"unclassified one", tc.domain.Name(), name, tc.prefix)
				}
			}
			for name := range shipped {
				if !strings.HasPrefix(name, tc.prefix) {
					continue
				}
				if _, ok := declared[name]; !ok {
					t.Errorf("%s is shipped and the %s domain does not classify "+
						"it — the scrub list, the identity claim and the sweep "+
						"are all derived from that map, so a table missing from "+
						"it joins whichever behaviour its absence resembled",
						name, tc.domain.Name())
				}
			}
		})
	}
}

// THE IAM OPS LEDGER TAKES THE FRAMEWORK'S FOUR COLUMNS TOO, and the fifth
// column that would be tempting here is the same one the chart's case refuses
// for the same reason: a `kind`, so a session's op id could be swept on a
// different schedule from an enrolment's. This domain declares ONE ops horizon
// and its authentication TRAIL declares two, which is not a contradiction —
// they answer different questions and are measured against different things,
// an audit obligation and the longest a client will retry.
func TestTheIamOpsLedgerCarriesTheFrameworksFourColumns(t *testing.T) {
	t.Parallel()

	db := openReplicated(t)
	got := columnsOf(t, db, iamdomain.Domain{}.OpsTable())
	want := []string{"applied_at", "op_id", "position", "subject"}
	if !slices.Equal(got, want) {
		t.Errorf("%s has columns %v, want exactly %v — the framework writes the "+
			"statements that fill this table, so a column it does not know is "+
			"one nothing ever populates",
			iamdomain.Domain{}.OpsTable(), got, want)
	}

	ddl := replicatedDDL(t)
	if !strings.Contains(ddl, "ON iam_ops (applied_at)") {
		t.Error("no index over iam_ops (applied_at) is shipped — the ops sweep " +
			"is a range delete over the age, and a range delete ships its index")
	}
	if strings.Contains(ddl, "iam_ops (kind") || strings.Contains(ddl, "iam_ops(kind") {
		t.Error("an index over iam_ops keyed on a kind is shipped — this domain " +
			"declares ONE ops horizon, and an index implying a second is how the " +
			"second gets written")
	}
}

// THE IAM TABLES SHIP THE COLUMNS A LATER MIGRATION CANNOT ADD.
//
// schema_migrations keys on the FILENAME, so editing a migration that has
// already run silently never re-runs it. Every column here is one the applier
// fills from the FIRST record it ever writes — the three claims it denormalises
// onto a person's row so the duplicate report can scan them, and the bucket
// every table's sweep seeks on — so they have to ship in the migration that
// creates the table rather than in the one that starts using them.
func TestTheIamTablesShipTheColumnsAMigrationCannotAddLater(t *testing.T) {
	t.Parallel()

	db := openReplicated(t)
	for table, columns := range map[string][]string{
		"iam_people":             {"login", "email_blind", "seat_id", "bucket"},
		"iam_credentials":        {"bucket"},
		"iam_invites":            {"bucket"},
		"iam_bootstrap_codes":    {"bucket"},
		"iam_sessions":           {"bucket"},
		"iam_session_generation": {"bucket"},
		"iam_history":            {"class", "bucket"},
	} {
		got := columnsOf(t, db, table)
		for _, column := range columns {
			if !slices.Contains(got, column) {
				t.Errorf("%s does not carry %s — an applied migration is "+
					"history, not source, so no later file can add it to a "+
					"database that has already run this one", table, column)
			}
		}
	}
}

// AND THE THREE DUPLICATE-CLAIM INDEXES ARE SHIPPED, PARTIAL AND NON-UNIQUE.
//
// The uniqueness half is already covered by the estate-wide scan in
// schemarules_test.go, which refuses UNIQUE in any replicated migration. What
// THAT cannot say is the positive claim: that these three indexes exist at all.
// A duplicate claim is a state ordinary traffic cannot produce — the broker
// refuses the second claim on a subject — and can arise from a restore or a
// reanchor, so the duty that REPORTS one is the whole remedy, and a duty whose
// index nobody shipped is a full scan of the directory on every tick.
func TestTheDuplicateClaimIndexesArePartialAndNotUnique(t *testing.T) {
	t.Parallel()

	ddl := replicatedDDL(t)
	for _, want := range []string{
		"ON iam_people (email_blind, id) WHERE email_blind != ''",
		"ON iam_people (login, id) WHERE login != ''",
		"ON iam_people (seat_id, id) WHERE seat_id != ''",
	} {
		if !strings.Contains(ddl, want) {
			t.Errorf("no index matching %q is shipped — the duplicate-claim "+
				"duty reads these three, and without them its scan is the whole "+
				"directory on every tick", want)
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
