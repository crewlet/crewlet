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
	// AND NO SCOPED STAMP BESIDE THE VERSION. It was the column a claim on
	// another subject stamped a person's row with; every record that writes
	// the row now advances `version`, so a second stamp would be a value
	// every reader is entitled to fold into the wrong comparison.
	if cols := columnsOf(t, db, "iam_people"); slices.Contains(cols, "scoped_through") {
		t.Error("iam_people still carries scoped_through — nothing writes it " +
			"since the directory decided every login, address and seat on one " +
			"subject")
	}
}

// THE DIRECTORY'S LOOKUPS SEARCH AN INDEX, AND NEVER SCAN THE PEOPLE.
//
// A sign-in resolves a login or an address blind to its row, and every
// directory decision asks whether anybody else holds the value it is about to
// give — each a query with a BOUND value. 0034's indexes over those two columns
// were PARTIAL, over the rows whose value is not empty, which a bound value
// cannot be proved to satisfy: measured on this driver, both lookups were a
// SCAN of every person. 0053 replaced them with plain indexes. The plan is read as the driver
// reports it, for the shapes the directory runs — its uniqueness check and the
// sign-in's sighting.
//
// Mutation: take 0053 out and the login and address rows read
// `SCAN iam_people`.
func TestTheDirectoryLookupsSearchAnIndex(t *testing.T) {
	t.Parallel()
	db := openReplicated(t)
	for _, tc := range []struct {
		query string
		args  []any
		index string
	}{
		{`SELECT id FROM iam_people WHERE login = ? AND id <> ? LIMIT 1`,
			[]any{"jane.doe", "p1"}, "iam_people_login_idx"},
		{`SELECT id FROM iam_people WHERE email_blind = ? AND id <> ? LIMIT 1`,
			[]any{"blind", "p1"}, "iam_people_email_idx"},
		{`SELECT id FROM iam_people WHERE seat_id = ? AND id <> ? LIMIT 1`,
			[]any{"founder", "p1"}, "iam_people_seat_lookup_idx"},
		{`SELECT id, stage, login, seat_id, document FROM iam_people
		   WHERE login = ? LIMIT 2`,
			[]any{"jane.doe"}, "iam_people_login_idx"},
	} {
		plan := planOf(t, db, tc.query, tc.args...)
		if want := "SEARCH iam_people USING INDEX " + tc.index; !strings.Contains(plan, want) {
			t.Errorf("%s\nplans as %q, want %q — a lookup that scans reads "+
				"every person in the company on every sign-in", tc.query, plan, want)
		}
	}
}

// planOf is the driver's own EXPLAIN QUERY PLAN for query, one step per line.
func planOf(t *testing.T, db *store.DB, query string, args ...any) string {
	t.Helper()
	var steps []string
	if err := db.Replicated().Read(t.Context(), func(tx *sql.Tx) error {
		rows, err := tx.QueryContext(t.Context(), "EXPLAIN QUERY PLAN "+query, args...)
		if err != nil {
			return err
		}
		defer func() { _ = rows.Close() }()
		for rows.Next() {
			var id, parent, unused int
			var detail string
			if err := rows.Scan(&id, &parent, &unused, &detail); err != nil {
				return err
			}
			steps = append(steps, detail)
		}
		return rows.Err()
	}); err != nil {
		t.Fatalf("explain %q: %v", query, err)
	}
	return strings.Join(steps, "\n")
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

// THE CHART'S LOG LEAVES NOTHING BEHIND: no table of its domain in the
// replicated estate, no staged chart in the node's, and no column a revision
// kept about it.
//
// The chart is the company document's again, so these are tables no applier
// writes and columns no writer fills — and the estate is derived, so a table
// left behind is one a snapshot carries to every node that joins. The
// controls are the tracker's own table and the revision's payload, which a
// migration that dropped too much would take with it.
func TestTheChartLeavesNoTableAndNoColumnBehind(t *testing.T) {
	t.Parallel()
	db := openReplicated(t)
	names := func(estate *store.DB, query string) []string {
		var out []string
		if err := estate.Read(t.Context(), func(tx *sql.Tx) error {
			rows, err := tx.QueryContext(t.Context(), query)
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
			t.Fatalf("%s: %v", query, err)
		}
		return out
	}
	const tables = `SELECT name FROM sqlite_schema WHERE type = 'table'`
	replicated, node := names(db.Replicated(), tables), names(db, tables)
	if !slices.Contains(replicated, "tracker_projects") {
		t.Fatalf("the replicated estate lists %v, without the tracker's projects", replicated)
	}
	for _, name := range append(slices.Clone(replicated), node...) {
		if strings.HasPrefix(name, "chart_") {
			t.Errorf("%s is still shipped — nothing writes or reads it since the "+
				"chart went back into the company document", name)
		}
	}
	cols := names(db, `SELECT name FROM pragma_table_info('company_config')`)
	if !slices.Contains(cols, "payload") {
		t.Fatalf("company_config has columns %v, without its payload", cols)
	}
	for _, gone := range []string{"chart_position", "scrubbed_at"} {
		if slices.Contains(cols, gone) {
			t.Errorf("company_config still carries %s, which nothing writes", gone)
		}
	}
}
