package store_test

import (
	"database/sql"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/iamdomain"
	"github.com/crewlet/crewlet/internal/pages"
	"github.com/crewlet/crewlet/internal/search"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/store"
	"github.com/crewlet/crewlet/internal/tracker"
	"github.com/crewlet/crewlet/internal/usage"
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
// Four of the five registered domains reach it through [statelogtest.Run],
// which runs the same check as one of its four suites. The IAM domain cannot:
// that suite also runs the apply cases, and a domain has no applier on the
// change that declares it — the engine's boot check refuses a register entry
// with a nil applier, so a registration cannot land before its applier. The
// check itself needs only a migrated estate and a declaration, and the
// migrated estate is this package's. Running all five here also makes
// the walk two-sided: a domain whose tables stopped being created at all would
// fail here rather than quietly stop being certified — the two COMPACTED
// domains (the vectors and each node's usage) included, whose ledgerless
// declarations are the ones a migration is likeliest to get wrong unnoticed.
func TestTheShippedSchemaAcceptsTheFrameworksOwnStatements(t *testing.T) {
	t.Parallel()

	for _, domain := range []statelog.Domain{
		tracker.Domain{}, pages.Domain{}, iamdomain.Domain{},
		search.Domain{}, usage.Domain{},
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

// A PROJECT CARRIES THE ACTIVATION ITS CHART-OWNED FIELDS CAME FROM, AND NO LOG
// POSITION.
//
// schema_migrations keys on the FILENAME, so editing a migration that has
// already run silently never re-runs it: every database that applied it keeps
// the old shape while the code assumes the new one. `chart_epoch` is the
// guard the tracker's applier compares before any chart-owned field of a
// project, so it has to be in the shipped estate rather than in a later file
// a database might never run.
func TestAProjectCarriesItsActivationStampAndNoLogPosition(t *testing.T) {
	t.Parallel()

	db := openReplicated(t)
	cols := columnsOf(t, db, "tracker_projects")
	if !slices.Contains(cols, "chart_epoch") {
		t.Error("tracker_projects does not carry chart_epoch — an applied " +
			"migration is history, not source, so no later file can add it to a " +
			"database that has already run this one")
	}
	// AND THE ONE IT REPLACED IS GONE. A guard column with no writer is
	// worse than an absent one: it reads as a fact about the row, a later
	// reconcile is tempted to compare it, and what it held was a log
	// position in a number space no activation stamp shares.
	if slices.Contains(cols, "chart_position") {
		t.Error("tracker_projects still carries chart_position — nothing writes " +
			"it, and a column no writer fills is a value every reader is " +
			"entitled to misread")
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

// THE IAM OPS LEDGER TAKES THE FRAMEWORK'S FIVE COLUMNS TOO, and no `kind`.
// This ledger DOES keep a second horizon — a session's operations go after an
// hour, everything else after the framework's month — but it is keyed on the
// SUBJECT the framework already stores (`0038`'s index over it), and the loss
// each horizon leaves is recorded per kind by the framework (`0045`), so the
// shorter sweep never moves the table-wide watermark. A `kind` column would be
// a second copy of what the subject already says, and one nothing populates.
func TestTheIamOpsLedgerCarriesTheFrameworksFiveColumns(t *testing.T) {
	t.Parallel()

	db := openReplicated(t)
	got := columnsOf(t, db, iamdomain.Domain{}.OpsTable())
	want := []string{"applied_at", "op_id", "position", "stored_at", "subject"}
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
		t.Error("an index over iam_ops keyed on a kind is shipped — this " +
			"domain's second horizon is keyed on the subject the ledger already " +
			"stores, and a kind column is one nothing populates")
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
		"iam_sessions":           {"bucket"},
		"iam_revocation_epochs":  {"bucket"},
		"iam_session_generation": {"generation"},
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
	// AND A BINDING CARRIES NO CHART POSITION. Nothing writes the column
	// once a seat is resolved against the running organisation, and a
	// column no writer fills is a value every reader is entitled to misread
	// as the position a bind was decided at.
	if cols := columnsOf(t, db, "iam_people"); slices.Contains(cols, "chart_position") {
		t.Error("iam_people still carries chart_position — nothing writes it " +
			"since a seat binding stopped recording the chart's position")
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
