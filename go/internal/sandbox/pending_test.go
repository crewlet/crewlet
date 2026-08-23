package sandbox_test

import (
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"

	"github.com/crewlet/crewlet/internal/logging"
	"github.com/crewlet/crewlet/internal/sandbox"
	"github.com/crewlet/crewlet/internal/sandbox/sandboxtest"
	"github.com/crewlet/crewlet/internal/store"
)

// TestMain silences the engine logger. Every Open logs a line per applied
// schema file and the suite opens a database per subtest — several hundred
// lines of successful boot ahead of the one that says what failed.
func TestMain(m *testing.M) {
	logging.Configure(slog.LevelError, logging.FormatText, io.Discard)
	os.Exit(m.Run())
}

// certified is every driver the store may be opened with. A statement is only
// known to be in the dialect intersection once both have parsed it.
var certified = []store.Driver{store.DriverTurso, store.DriverSQLite}

func TestPendingStoreContract(t *testing.T) {
	// THE TWIN RUNS THE SAME SUITE. A memory twin nobody holds to the
	// contract is a twin that models the store wrongly and then certifies
	// the bug in every test that uses it.
	t.Run("memory", func(t *testing.T) {
		t.Parallel()
		sandboxtest.Run(t, func(*testing.T) sandbox.PendingStore {
			return sandbox.NewMemoryStore()
		})
	})
	for _, drv := range certified {
		t.Run(string(drv), func(t *testing.T) {
			t.Parallel()
			sandboxtest.Run(t, func(t *testing.T) sandbox.PendingStore {
				return sandbox.NewSQLStore(openDB(t, drv))
			})
		})
	}
}

func openDB(t *testing.T, drv store.Driver) *store.DB {
	t.Helper()
	db, err := store.Open(t.Context(), filepath.Join(t.TempDir(), "store.db"),
		store.Options{Driver: drv})
	if err != nil {
		t.Fatalf("open %s: %v", drv, err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}
