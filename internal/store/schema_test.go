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

// A PROJECT CARRIES THE ACTIVATION ITS CHART-OWNED FIELDS CAME FROM.
//
// schema_migrations keys on the FILENAME, so editing a migration that has
// already run silently never re-runs it: every database that applied it keeps
// the old shape while the code assumes the new one. `chart_epoch` is the
// guard the tracker's applier compares before any chart-owned field of a
// project, so it has to be in the shipped estate rather than in a later file
// a database might never run.
func TestAProjectCarriesItsActivationStamp(t *testing.T) {
	t.Parallel()

	db := openReplicated(t)
	cols := columnsOf(t, db, "tracker_projects")
	if !slices.Contains(cols, "chart_epoch") {
		t.Error("tracker_projects does not carry chart_epoch — an applied " +
			"migration is history, not source, so no later file can add it to a " +
			"database that has already run this one")
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
// SUBJECT the framework already stores (the `(subject, applied_at)` index
// shipped beside it), and the loss each horizon leaves is recorded per kind by
// the framework (`statelog_ops_lost_kind`), so the shorter sweep never moves
// the table-wide watermark. A `kind` column would be
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
// fills from the FIRST record it ever writes — the three directory values on a
// person's row, which the directory's lookups seek on, and the bucket every
// table's sweep seeks on — so they have to ship in the migration that creates
// the table rather than in the one that starts using them.
//
// ONE COLUMN DID ARRIVE LATER, and the way it arrived is the rule's other
// half: an invitation's `seat_id` (replicated migration 0032) is added
// defaulted, and the rows an older build applied without it are filled by the
// identity applier's own Rederive — a derivation bump the first boot of the
// new build runs, in the Go that writes the column — never by an UPDATE in a
// migration. It is asserted here beside the rest so a later file cannot drop
// it unnoticed.
func TestTheIamTablesShipTheColumnsAMigrationCannotAddLater(t *testing.T) {
	t.Parallel()

	db := openReplicated(t)
	for table, columns := range map[string][]string{
		"iam_people":             {"login", "email_blind", "seat_id", "bucket"},
		"iam_credentials":        {"bucket"},
		"iam_invites":            {"bucket", "seat_id"},
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
}

// THE DIRECTORY'S LOOKUPS SEARCH AN INDEX, AND NEVER SCAN THE PEOPLE.
//
// A sign-in resolves a login or an address blind to its row, and every
// directory decision asks whether anybody else holds the value it is about to
// give — each a query with a BOUND value. An index over those two columns that
// is PARTIAL, over the rows whose value is not empty, is one a bound value
// cannot be proved to satisfy: measured on this driver, both lookups were then
// a SCAN of every person, so the estate ships plain ones. The plan is read as
// the driver reports it, for the shapes the directory runs — its uniqueness
// check and the sign-in's sighting.
//
// The listing of every binding is the other direction: a non-empty `seat_id`
// IS the seat's partial index's predicate, so it walks that index — the bound
// people, in seat order — where without it the read is every person plus a
// sort, on every seat listing and every party-registry rebuild.
//
// AN OPEN INVITATION HOLDS ITS ADDRESS AND ITS SEAT, so every directory
// decision that takes either asks the invitations too, with a bound value —
// a seek on the address's index or the seat's (migration 0032), which is
// plain for the people's reason — and the seat listing walks the OPEN
// invitations' partial index (0031's) and sorts what it finds: the
// outstanding invitations only, never every one that ever named a seat.
//
// Mutation: make the login and address indexes partial over their non-empty
// rows and those rows read `SCAN iam_people`; drop `iam_people_seat_claim_idx`
// and the listing does; drop `iam_invites_seat_idx` and the seat hold scans
// the invitations; drop `iam_invites_open_idx` and the invitation listing
// does.
func TestTheDirectoryLookupsSearchAnIndex(t *testing.T) {
	t.Parallel()
	db := openReplicated(t)
	for _, tc := range []struct {
		query string
		args  []any
		want  string
	}{
		{`SELECT id FROM iam_people WHERE login = ? AND id <> ? LIMIT 1`,
			[]any{"jane.doe", "p1"}, "SEARCH iam_people USING INDEX iam_people_login_idx"},
		{`SELECT id FROM iam_people WHERE email_blind = ? AND id <> ? LIMIT 1`,
			[]any{"blind", "p1"}, "SEARCH iam_people USING INDEX iam_people_email_idx"},
		{`SELECT id FROM iam_people WHERE seat_id = ? AND id <> ? LIMIT 1`,
			[]any{"founder", "p1"}, "SEARCH iam_people USING INDEX iam_people_seat_lookup_idx"},
		{`SELECT id, stage, login, seat_id, document FROM iam_people
		   WHERE login = ? LIMIT 2`,
			[]any{"jane.doe"}, "SEARCH iam_people USING INDEX iam_people_login_idx"},
		// SeatClaims' and SeatHolders' shape: every bound person, by seat.
		{`SELECT id, kind, login, stage, seat_id FROM iam_people
		   WHERE seat_id != '' ORDER BY seat_id, id`,
			nil, "SCAN iam_people USING INDEX iam_people_seat_claim_idx"},
		// heldByInvitation's two shapes: an open invitation on an address,
		// and on a seat.
		{`SELECT id FROM iam_invites
		   WHERE email_blind = ? AND redeemed_at = 0
		     AND expires_at > ? AND seat_id <> ''
		   ORDER BY created_at DESC LIMIT 1`,
			[]any{"blind", 1}, "SEARCH iam_invites USING INDEX iam_invites_email_idx"},
		{`SELECT id FROM iam_invites
		   WHERE seat_id = ? AND redeemed_at = 0
		     AND expires_at > ? AND seat_id <> ''
		   ORDER BY created_at DESC LIMIT 1`,
			[]any{"founder", 1}, "SEARCH iam_invites USING INDEX iam_invites_seat_idx"},
		// SeatClaims' other half: every open invitation onto a seat.
		{`SELECT id, seat_id, email_sealed, invited_by, created_at, expires_at
		    FROM iam_invites
		   WHERE redeemed_at = 0 AND expires_at > ? AND seat_id <> ''
		   ORDER BY seat_id, created_at DESC, id`,
			[]any{1}, "SCAN iam_invites USING INDEX iam_invites_open_idx"},
	} {
		plan := planOf(t, db, tc.query, tc.args...)
		if !strings.Contains(plan, tc.want) {
			t.Errorf("%s\nplans as %q, want %q — a directory read that scans "+
				"reads every person in the company", tc.query, plan, tc.want)
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
