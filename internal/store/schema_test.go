package store_test

import (
	"database/sql"
	"slices"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/accessdomain"
	"github.com/crewlet/crewlet/internal/advisordomain"
	"github.com/crewlet/crewlet/internal/artifactdata"
	"github.com/crewlet/crewlet/internal/artifactsdomain"
	"github.com/crewlet/crewlet/internal/assistantdomain"
	"github.com/crewlet/crewlet/internal/atlasdomain"
	"github.com/crewlet/crewlet/internal/chatlet"
	"github.com/crewlet/crewlet/internal/doclet"
	"github.com/crewlet/crewlet/internal/executordomain"
	"github.com/crewlet/crewlet/internal/gitlet"
	"github.com/crewlet/crewlet/internal/iamdomain"
	"github.com/crewlet/crewlet/internal/search"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/store"
	"github.com/crewlet/crewlet/internal/usage"
	"github.com/crewlet/crewlet/internal/worklet"
)

// EVERY DOMAIN'S SHIPPED DDL ACCEPTS THE STATEMENTS THE FRAMEWORK GENERATES.
//
// A domain declares three table NAMES and the framework writes their COLUMNS,
// and nothing in Go connects the two. A misspelt column compiles, migrates and
// serves every read, then fails when a record this build cannot decode arrives,
// the rarest path in the system, whose failure is a stalled log.
//
// It lives here, not beside each domain ([statelogtest.Run] runs the same check
// per domain), because it needs only a migrated estate. Running every domain
// here also makes the walk two-sided: a domain whose tables stopped being
// created fails here rather than quietly stopping its certification, the two
// COMPACTED domains (vectors, usage) included.
func TestTheShippedSchemaAcceptsTheFrameworksOwnStatements(t *testing.T) {
	t.Parallel()

	for _, domain := range []statelog.Domain{
		worklet.Domain{}, doclet.Domain{}, iamdomain.Domain{},
		search.Domain{}, usage.Domain{}, artifactsdomain.Domain{}, artifactdata.Domain{},
		assistantdomain.Domain{},
		chatlet.Domain{},
		executordomain.Domain{},
		gitlet.Domain{},
		accessdomain.Domain{},
		atlasdomain.Domain{},
		advisordomain.Domain{},
	} {
		t.Run(domain.Name(), func(t *testing.T) {
			t.Parallel()
			db := migratedEstate(t)
			if err := statelog.CheckTables(t.Context(), db, domain, domain.Stream()); err != nil {
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

	db := migratedEstate(t)
	cols := estateColumns(t, db, "tracker_projects")
	if !slices.Contains(cols, "chart_epoch") {
		t.Error("tracker_projects does not carry chart_epoch — an applied " +
			"migration is history, not source, so no later file can add it to a " +
			"database that has already run this one")
	}
}

// EVERY TABLE A DOMAIN DECLARES IS ONE THE ESTATE ACTUALLY SHIPS, AND BACK.
//
// The declaration is what the scrub list, the identity claim and the local
// sweep derive from, so a name no migration creates leaves three lists silently
// short, and a created table the domain does not classify joins whichever
// behaviour its absence resembled. Both directions, for skipgate's reason.
//
// The reverse walk is keyed on each domain's table PREFIX, a convention the
// framework does not enforce, so the case also asserts every declared table
// carries it, or a renamed one would silently leave the walk.
func TestEachDomainDeclaresExactlyTheTablesItShips(t *testing.T) {
	t.Parallel()

	shipped := tablesIn(t, store.EstateReplicated)
	for _, tc := range []struct {
		prefix string
		domain statelog.Domain
	}{
		{"iam_", iamdomain.Domain{}},
		{"assistant_", assistantdomain.Domain{}},
		{"chat_", chatlet.Domain{}},
		{"executor_", executordomain.Domain{}},
		{"code_", gitlet.Domain{}},
		{"access_", accessdomain.Domain{}},
		{"atlas_", atlasdomain.Domain{}},
		{"advisor_", advisordomain.Domain{}},
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

	db := migratedEstate(t)
	got := estateColumns(t, db, iamdomain.Domain{}.OpsTable())
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
// already run never re-runs it. Every column here is one the applier fills from
// the FIRST record it writes (the three directory values the lookups seek on,
// and the bucket every sweep seeks on), so it must ship in the migration that
// creates the table.
//
// ONE COLUMN ARRIVES IN A MIGRATION OF ITS OWN: an invitation's `seat_id`
// (replicated migration 0042) is added defaulted and is DERIVED, recomputed by
// the identity applier's Rederive when a checkpoint's rule set differs, in Go,
// never by an UPDATE in a migration. Asserted so a later file cannot drop it.
func TestTheIamTablesShipTheColumnsAMigrationCannotAddLater(t *testing.T) {
	t.Parallel()

	db := migratedEstate(t)
	for table, columns := range map[string][]string{
		"iam_people":             {"login", "email_blind", "seat_id", "bucket"},
		"iam_credentials":        {"bucket"},
		"iam_invites":            {"bucket", "seat_id"},
		"iam_sessions":           {"bucket"},
		"iam_revocation_epochs":  {"bucket"},
		"iam_session_generation": {"generation"},
		"iam_history":            {"class", "bucket"},
	} {
		got := estateColumns(t, db, table)
		for _, column := range columns {
			if !slices.Contains(got, column) {
				t.Errorf("%s does not carry %s — an applied migration is "+
					"history, not source, so no later file can add it to a "+
					"database that has already run this one", table, column)
			}
		}
	}
}

// THE DIRECTORY'S INDEXES ARE DECLARED, AS ITS STATEMENTS NEED THEM.
//
// How each statement plans is internal/iamdomain's gate
// (TestTheDirectoryStatementsArePlanned). Here is the SCHEMA's half: the
// migrations declare each index on its table and columns, and the three lookups
// are PLAIN while the bindings' index is PARTIAL, since a partial index over
// the non-empty rows is one a bound value cannot be proved to satisfy, and the
// bindings' listing is the one read whose own predicate is the index's.
//
// Mutation: make `iam_people_login_idx` partial over its non-empty rows, or
// drop 0069's re-keyed `iam_history_person_idx`, and the case names the index.
func TestTheDirectoryIndexesAreDeclared(t *testing.T) {
	t.Parallel()
	db := migratedEstate(t)
	for name, want := range map[string]struct {
		table, columns string
		partial        bool
	}{
		"iam_people_login_idx":       {"iam_people", "(login)", false},
		"iam_people_email_idx":       {"iam_people", "(email_blind)", false},
		"iam_people_seat_lookup_idx": {"iam_people", "(seat_id)", false},
		"iam_people_seat_claim_idx":  {"iam_people", "(seat_id, id)", true},
		"iam_people_stage_idx":       {"iam_people", "(stage, id)", false},
		"iam_people_kind_idx":        {"iam_people", "(kind)", false},
		"iam_invites_email_idx":      {"iam_invites", "(email_blind, created_at DESC)", false},
		"iam_invites_seat_idx":       {"iam_invites", "(seat_id, created_at DESC)", false},
		"iam_invites_open_idx":       {"iam_invites", "(bucket, expires_at)", true},
		"iam_invites_spent_idx":      {"iam_invites", "(bucket, redeemed_at)", true},
		"iam_history_person_idx":     {"iam_history", "(person_id, version)", false},
		"iam_history_object_idx":     {"iam_history", "(object_kind, object_id, broker_at DESC)", false},
		"iam_history_version_idx":    {"iam_history", "(version)", false},
		"iam_history_class_idx":      {"iam_history", "(class, broker_at)", false},
		"iam_history_sweep_idx":      {"iam_history", "(bucket, class, version)", false},
		"iam_removed_seat_idx":       {"iam_removed", "(seat_id, seat_rebound_by, person_id)", false},
	} {
		var table, ddl string
		if err := db.Read(t.Context(), func(tx *sql.Tx) error {
			return tx.QueryRowContext(t.Context(),
				`SELECT tbl_name, sql FROM sqlite_master
				 WHERE type = 'index' AND name = ?`, name).Scan(&table, &ddl)
		}); err != nil {
			t.Errorf("read the declaration of %s: %v — the directory's "+
				"statements read through it", name, err)
			continue
		}
		columns := ddl[strings.Index(ddl, "("):]
		where := strings.Contains(strings.ToUpper(columns), " WHERE ")
		if where {
			columns = columns[:strings.Index(strings.ToUpper(columns), " WHERE ")]
		}
		switch {
		case table != want.table:
			t.Errorf("%s is on %s, want %s", name, table, want.table)
		case strings.TrimSpace(columns) != want.columns:
			t.Errorf("%s is over %s, want %s", name, columns, want.columns)
		case where != want.partial:
			t.Errorf("%s: partial is %v, want %v:\n%s", name, where, want.partial, ddl)
		}
	}
}
