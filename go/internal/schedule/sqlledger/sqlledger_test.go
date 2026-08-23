package sqlledger_test

import (
	"context"
	"database/sql"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/logging"
	"github.com/crewlet/crewlet/internal/schedule"
	"github.com/crewlet/crewlet/internal/schedule/scheduletest"
	"github.com/crewlet/crewlet/internal/schedule/sqlledger"
	"github.com/crewlet/crewlet/internal/store"
)

// TestMain silences the engine logger for the whole test binary: every Open
// logs a line per applied schema file and the suite opens a database per
// subtest, which buries the one line that says what failed.
func TestMain(m *testing.M) {
	logging.Configure(slog.LevelError, logging.FormatText, io.Discard)
	os.Exit(m.Run())
}

// certified is every driver the store may be opened with. The ledger's
// statements run against all of them, because a statement is only known to be
// in the dialect intersection once both have parsed it (d-002).
var certified = []store.Driver{store.DriverTurso, store.DriverSQLite}

// TestContract runs the SAME suite the memory twin runs. That is the point of
// having a twin at all: a semantic divergence between the two becomes a named
// failing case rather than a production surprise on whichever deployment runs
// the other one.
func TestContract(t *testing.T) {
	for _, drv := range certified {
		t.Run(string(drv), func(t *testing.T) {
			t.Parallel()
			requireDriver(t, drv)
			scheduletest.Run(t, func(t *testing.T) schedule.Ledger {
				return sqlledger.New(open(t, drv).SQL())
			})
		})
	}
}

// TestALedgerWithNoDatabaseSaysSo covers the one state the contract suite
// cannot construct: it is handed a working ledger by definition. A nil handle
// arrives from a caller that wired the scheduler before opening the store, and
// the failure has to name itself — a nil dereference three frames inside a
// tick names nothing.
func TestALedgerWithNoDatabaseSaysSo(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		l    *sqlledger.Ledger
	}{
		{"nil ledger", nil},
		{"nil database", sqlledger.New(nil)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			ctx := t.Context()
			if _, err := tc.l.Claim(ctx, schedule.Run{}); !errors.Is(err, sqlledger.ErrNoDB) {
				t.Errorf("Claim = %v, want ErrNoDB", err)
			}
			if _, err := tc.l.Recent(ctx, 10); !errors.Is(err, sqlledger.ErrNoDB) {
				t.Errorf("Recent = %v, want ErrNoDB", err)
			}
			if _, err := tc.l.Purge(ctx, time.Now()); !errors.Is(err, sqlledger.ErrNoDB) {
				t.Errorf("Purge = %v, want ErrNoDB", err)
			}
		})
	}
}

// TestAClaimAgainstNoTableIsUnknownNotARefusal is the tri-state at the
// backend's own boundary.
//
// A ledger pointed at a database with no schema fails every statement. The
// only wrong answer is (false, nil): the scheduler reads that as "somebody
// already fired this" and drops the run permanently, so a misconfiguration
// would present as a company whose schedules silently never run.
func TestAClaimAgainstNoTableIsUnknownNotARefusal(t *testing.T) {
	t.Parallel()
	requireDriver(t, store.DriverSQLite)
	db, err := sql.Open(string(store.DriverSQLite), filepath.Join(t.TempDir(), "bare.db"))
	if err != nil {
		t.Fatalf("open a schemaless database: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	ok, err := sqlledger.New(db).Claim(t.Context(), schedule.Run{
		FireKey: schedule.FireKey{ScopeID: "qa", ScheduleName: "smoke", FireLabel: "20260608T0900"},
	})
	if err == nil {
		t.Fatal("Claim against a schemaless database succeeded, want an error")
	}
	if ok {
		t.Fatal("Claim reported a granted claim it could not have written")
	}
}

// TestTheStatementsNameTheirColumns is a spelling check against the schema the
// store owns.
//
// The ledger's SQL and 0004_runtime.sql are in different packages and nothing
// links them: a column renamed there fails here at run time, in whichever test
// happens to touch it, with a driver error that names the column but not the
// reason. Reading the table's actual shape and comparing it to what the
// statements expect turns that into one failure that says what moved.
func TestTheStatementsNameTheirColumns(t *testing.T) {
	t.Parallel()
	requireDriver(t, store.DriverSQLite)
	db := open(t, store.DriverSQLite)

	rows, err := db.SQL().QueryContext(t.Context(), `SELECT * FROM scheduled_runs LIMIT 0`)
	if err != nil {
		t.Fatalf("read scheduled_runs: %v", err)
	}
	defer func() { _ = rows.Close() }()
	got, err := rows.Columns()
	if err != nil {
		t.Fatalf("columns: %v", err)
	}
	slices.Sort(got)

	want := []string{
		"fire_label", "fired_at", "outcome", "schedule_name", "scheduled_at",
		"scope_id", "scope_type", "target_handle", "trace_id",
	}
	if !slices.Equal(got, want) {
		t.Fatalf("scheduled_runs columns = %v, want %v — the ledger's statements name the "+
			"right-hand set, so a column that moved has to move in both places", got, want)
	}
}

// open builds a migrated store on its own file. Each one is exclusive: the
// engine owns its file, and sharing one between parallel subtests would
// exercise an arrangement nothing runs in.
func open(t *testing.T, drv store.Driver) *store.DB {
	t.Helper()
	db, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "schedule.db"),
		store.Options{Driver: drv})
	if err != nil {
		t.Fatalf("open %s: %v", drv, err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

// requireDriver skips rather than fails when a driver is not compiled in. Both
// are in go.mod today; a build that drops one should lose that driver's
// coverage, not the whole suite.
func requireDriver(t *testing.T, drv store.Driver) {
	t.Helper()
	if !slices.Contains(sql.Drivers(), string(drv)) {
		t.Skipf("driver %q is not registered in this build", drv)
	}
}
