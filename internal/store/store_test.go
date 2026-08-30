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
	// THE LOCK HELPER SHARES THIS ENTRY POINT. A re-exec of the test binary
	// is the only way to get a second OS process, and the second process is
	// the only thing the store's file lock is about — see lock_ext_test.go.
	if path := os.Getenv(helperEnv); path != "" {
		os.Exit(runLockHelper(path, os.Getenv(helperModeEnv)))
	}
	os.Exit(m.Run())
}

// TestContract runs the store's contract suite. One driver now, so the loop
// that ran it per driver is gone — what it certifies did not change with it.
func TestContract(t *testing.T) {
	t.Parallel()
	storetest.Run(t, func(t *testing.T) *store.DB { return open(t) })
}

// THE ONLY DRIVER LINKED IN IS TURSO, and this is what says so.
//
// It replaces TestDriverSelection, which covered a choice that no longer
// exists: `store.driver` in the Tier A file and CREWLET_STORE_DRIVER both
// picked between turso and mainline SQLite (decisions/003). Deleting a
// selector is easy to do halfway — the field goes, the blank import stays,
// and the binary quietly carries a second storage engine that nothing can
// reach but that anything with a raw sql.Open can. So the assertion is about
// the LINKED SET, not about a config field: "sqlite" registered here means
// modernc.org/sqlite came back into the build.
func TestTursoIsTheOnlyDriverInTheBinary(t *testing.T) {
	t.Parallel()
	linked := sql.Drivers()
	if !slices.Contains(linked, "turso") {
		t.Fatalf("the turso driver is not registered (have %v) — the store "+
			"cannot open anything, and every test below would skip rather "+
			"than fail", linked)
	}
	if slices.Contains(linked, "sqlite") {
		t.Errorf("modernc.org/sqlite is linked into this binary (drivers: %v). "+
			"It was removed with the second-driver escape hatch; a blank "+
			"import that came back ships a whole storage engine nothing "+
			"selects", linked)
	}
}

// open builds a database on its own file. Each one is exclusive: the engine
// owns its file, and sharing one between subtests would test an arrangement
// nothing runs in.
func open(t *testing.T) *store.DB {
	t.Helper()
	db, err := store.Open(t.Context(), filepath.Join(t.TempDir(), "store.db"),
		store.Options{})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	// Tolerant of a test that closed it already — the reopen case does.
	t.Cleanup(func() { _ = db.Close() })
	return db
}
