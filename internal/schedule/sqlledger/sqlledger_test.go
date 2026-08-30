package sqlledger_test

import (
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

// TestContract runs the SAME suite the memory twin runs. That is the point of
// having a twin at all: a semantic divergence between the two becomes a named
// failing case rather than a production surprise on whichever deployment runs
// the other one.
//
// One driver now (decisions/003), so the per-driver loop that used to wrap
// this is gone. What it certified — that the ledger's SQL means the same
// thing as the in-memory twin — never depended on there being two.
func TestContract(t *testing.T) {
	t.Parallel()
	scheduletest.Run(t, func(t *testing.T) schedule.Ledger {
		return sqlledger.New(open(t).SQL())
	})
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
	// A MIGRATED STORE WITH THE TABLE DROPPED, not a raw sql.Open on a
	// bare file. The driver loads a native library on its first connection
	// and PANICS on a half-written cache unless store.Open has prepared it
	// (internal/store/turso.go), so a second way into a connection is a
	// second way to take the test binary down — the same defect
	// store.Pending had. Dropping the table is the honest way to reach
	// "the statements have no table to run against".
	db := open(t)
	if _, err := db.SQL().ExecContext(t.Context(),
		`DROP TABLE scheduled_runs`); err != nil {
		t.Fatalf("drop scheduled_runs: %v", err)
	}

	ok, err := sqlledger.New(db.SQL()).Claim(t.Context(), schedule.Run{
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
	db := open(t)

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
func open(t *testing.T) *store.DB {
	t.Helper()
	db, err := store.Open(t.Context(), filepath.Join(t.TempDir(), "schedule.db"),
		store.Options{})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}
