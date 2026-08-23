package store_test

import (
	"database/sql"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/crewlet/crewlet/internal/logging"
	"github.com/crewlet/crewlet/internal/store"
	"github.com/crewlet/crewlet/internal/store/storetest"
)

// TestMain silences the engine logger for the whole test binary. Every Open
// logs a line per applied schema file, and the suite opens a database per
// subtest — several hundred lines of successful boot ahead of the one line
// that says what failed. Nothing here asserts on log output; failures come
// back as returned errors.
func TestMain(m *testing.M) {
	logging.Configure(slog.LevelError, logging.FormatText, io.Discard)
	os.Exit(m.Run())
}

// certified is every driver the store may be opened with. The contract suite
// runs against all of them, because a statement is only known to be in the
// dialect intersection once both have parsed it.
var certified = []store.Driver{store.DriverTurso, store.DriverSQLite}

func TestContract(t *testing.T) {
	for _, drv := range certified {
		t.Run(string(drv), func(t *testing.T) {
			t.Parallel()
			requireDriver(t, drv)
			storetest.Run(t, func(t *testing.T) *store.DB {
				return open(t, drv)
			})
		})
	}
}

// TestDriverSelection covers the one decision Open makes before it touches a
// file. An unknown name is an ERROR, not a fallback: a mistyped log level may
// safely resolve to info, but a mistyped driver name silently opening a
// different storage engine is a data-loss shape.
func TestDriverSelection(t *testing.T) {
	for _, tc := range []struct {
		name    string
		env     string
		opt     store.Driver
		want    store.Driver
		wantErr bool
	}{
		{name: "default is turso", want: store.DriverTurso},
		{name: "env selects", env: "sqlite", want: store.DriverSQLite},
		{name: "option beats env", env: "sqlite", opt: store.DriverTurso, want: store.DriverTurso},
		{name: "unknown env", env: "postgres", wantErr: true},
		{name: "unknown option", opt: "mysql", wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv(store.DriverEnv, tc.env)
			requireDriver(t, tc.want)
			db, err := store.Open(t.Context(),
				filepath.Join(t.TempDir(), "c.db"),
				store.Options{Driver: tc.opt})
			if tc.wantErr {
				if err == nil {
					_ = db.Close()
					t.Fatal("want an error for an unknown driver name")
				}
				return
			}
			if err != nil {
				t.Fatalf("open: %v", err)
			}
			defer func() { _ = db.Close() }()
			if db.Driver() != tc.want {
				t.Fatalf("driver %q, want %q", db.Driver(), tc.want)
			}
		})
	}
}

// open builds a database on its own file. Each one is exclusive: the engine
// owns its file, and sharing one between subtests would test an arrangement
// nothing runs in.
func open(t *testing.T, drv store.Driver) *store.DB {
	t.Helper()
	db, err := store.Open(t.Context(), filepath.Join(t.TempDir(), "store.db"),
		store.Options{Driver: drv})
	if err != nil {
		t.Fatalf("open %s: %v", drv, err)
	}
	// Tolerant of a test that closed it already — the reopen case does.
	t.Cleanup(func() { _ = db.Close() })
	return db
}

// requireDriver skips rather than fails when a driver is not compiled in. Both
// are in go.mod today; a build that drops one (a cgo-free constraint, a
// platform without the Turso native library) should lose that driver's
// coverage, not the whole suite.
func requireDriver(t *testing.T, drv store.Driver) {
	t.Helper()
	if drv == "" {
		return
	}
	if !slices.Contains(sql.Drivers(), string(drv)) {
		t.Skipf("driver %q is not registered in this build", drv)
	}
}
