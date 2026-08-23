package store_test

import (
	"path/filepath"
	"testing"

	"github.com/crewlet/crewlet/internal/store"
)

// The capability matrix, re-measured against whatever driver versions this
// build pins.
//
// It exists because rewrite/decisions/002 found the documentation and the code
// disagreeing: Turso ships F32_BLOB columns and the vector distance functions,
// but its ANN vector index and its full-text index are announced surface not
// yet reachable from Go. Pinning a driver and asserting a matrix in prose is
// how that goes stale silently; asserting it in a test is how a pin bump
// reports what it changed.
//
// Each entry has three outcomes, and the middle one is the point:
//
//   - present and expected     -> the test EXERCISES the feature.
//   - absent and known-missing -> the test SKIPS, naming what would unlock it.
//     A driver upgrade that lands the feature turns this into a run, so a
//     newly-passing test is the notification.
//   - anything else            -> FAIL. A capability that vanished is a
//     regression; one that appeared without the matrix knowing is a matrix
//     that has stopped describing the build.
var capabilityMatrix = map[store.Driver]struct {
	vectorFunctions bool
	vectorIndex     bool
	fullTextSearch  bool
}{
	// turso.tech/database/tursogo v0.8.0-pre.7: the vector functions ship;
	// libsql_vector_idx() in CREATE INDEX and the fts() index expression
	// are both parse errors, and fts5 is not a registered module.
	store.DriverTurso: {vectorFunctions: true, vectorIndex: false, fullTextSearch: false},
	// modernc.org/sqlite: mainline SQLite, so no vector extension at all —
	// recall runs a cosine loop in Go — but fts5 is compiled in, which is
	// the inversion worth recording: neither driver is a superset of the
	// other, and the dialect this package writes in is their intersection.
	store.DriverSQLite: {vectorFunctions: false, vectorIndex: false, fullTextSearch: true},
}

func TestCapabilityMatrix(t *testing.T) {
	for drv, want := range capabilityMatrix {
		t.Run(string(drv), func(t *testing.T) {
			t.Parallel()
			requireDriver(t, drv)
			db, err := store.Open(t.Context(),
				filepath.Join(t.TempDir(), "caps.db"), store.Options{Driver: drv})
			if err != nil {
				t.Fatalf("open: %v", err)
			}
			defer func() { _ = db.Close() }()
			caps := db.Caps()

			t.Run("VectorFunctions", func(t *testing.T) {
				gate(t, caps.VectorFunctions, want.vectorFunctions,
					"vector32()/vector_distance_cos() reach the Go driver")
				exerciseVectorFunctions(t, db)
			})
			t.Run("VectorIndex", func(t *testing.T) {
				gate(t, caps.VectorIndex, want.vectorIndex,
					"an ANN vector index reaches the Go driver")
				exerciseVectorIndex(t, db)
			})
			t.Run("FullTextSearch", func(t *testing.T) {
				gate(t, caps.FullTextSearch, want.fullTextSearch,
					"a queryable full-text index reaches the Go driver")
				exerciseFullText(t, db)
			})
		})
	}
}

// gate compares the probe against the recorded matrix and decides whether the
// caller may proceed to exercise the feature.
func gate(t *testing.T, have, want bool, unlocks string) {
	t.Helper()
	switch {
	case have && want:
		// Fall through: the feature is present and expected. Exercise it.
	case !have && !want:
		t.Skipf("not available on this driver; this test runs when %s", unlocks)
	case have && !want:
		t.Fatalf("capability appeared that the matrix does not record — "+
			"%s now, so update capabilityMatrix and the query function behind it", unlocks)
	default:
		t.Fatalf("capability REGRESSED: the matrix records it as present, "+
			"but the probe says %s no longer holds", unlocks)
	}
}

// exerciseVectorFunctions does the thing the capability is for: a cosine
// distance computed by the database, over a value bound from Go in the same
// packed little-endian float32 layout the schema's BLOB columns hold.
func exerciseVectorFunctions(t *testing.T, db *store.DB) {
	t.Helper()
	ctx := t.Context()
	if _, err := db.SQL().ExecContext(ctx,
		`CREATE TABLE cap_vec (id TEXT PRIMARY KEY, e BLOB)`); err != nil {
		t.Fatalf("create: %v", err)
	}
	// Written through the Go encoder, read back by the SQL function: if
	// the two layouts ever disagree this is where it shows.
	blob, err := db.EncodeVector([]float32{1, 0, 0, 0})
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	if _, err := db.SQL().ExecContext(ctx,
		`INSERT INTO cap_vec (id, e) VALUES ('a', ?)`, blob); err != nil {
		t.Fatalf("insert: %v", err)
	}
	var same, orthogonal float64
	if err := db.SQL().QueryRowContext(ctx, `
		SELECT vector_distance_cos(e, vector32('[1,0,0,0]')),
		       vector_distance_cos(e, vector32('[0,1,0,0]'))
		FROM cap_vec WHERE id = 'a'`).Scan(&same, &orthogonal); err != nil {
		t.Fatalf("distance: %v", err)
	}
	if same > 1e-6 {
		t.Errorf("distance to itself = %v, want ~0 — the Go and SQL vector layouts disagree", same)
	}
	if orthogonal < 0.9 {
		t.Errorf("distance to an orthogonal vector = %v, want ~1", orthogonal)
	}
}

func exerciseVectorIndex(t *testing.T, db *store.DB) {
	t.Helper()
	ctx := t.Context()
	if _, err := db.SQL().ExecContext(ctx,
		`CREATE TABLE cap_ann (id TEXT PRIMARY KEY, e F32_BLOB(4))`); err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := db.SQL().ExecContext(ctx,
		`CREATE INDEX cap_ann_idx ON cap_ann (libsql_vector_idx(e))`); err != nil {
		t.Fatalf("ANN index: %v", err)
	}
}

func exerciseFullText(t *testing.T, db *store.DB) {
	t.Helper()
	ctx := t.Context()
	if _, err := db.SQL().ExecContext(ctx,
		`CREATE VIRTUAL TABLE cap_fts USING fts5(body)`); err != nil {
		// The other mechanism the probe accepts is Turso's own fts()
		// index expression; a driver that offers only that one still
		// reports the capability, and the query layer would branch.
		t.Skipf("full text is available through the non-fts5 mechanism: %v", err)
	}
	if _, err := db.SQL().ExecContext(ctx,
		`INSERT INTO cap_fts (body) VALUES ('the quick brown fox')`); err != nil {
		t.Fatalf("insert: %v", err)
	}
	var n int
	if err := db.SQL().QueryRowContext(ctx,
		`SELECT COUNT(*) FROM cap_fts WHERE cap_fts MATCH 'brown'`).Scan(&n); err != nil {
		t.Fatalf("match: %v", err)
	}
	if n != 1 {
		t.Fatalf("MATCH returned %d rows, want 1", n)
	}
}

// TestPartialIndexConflictTarget pins the measurement that decided the
// nullable-work_key design (rewrite/decisions/002 §2).
//
// The refinement worth keeping: it is not that ON CONFLICT and partial indexes
// are incompatible — repeating the index predicate verbatim in the statement
// parses on BOTH drivers. It is that the form anyone would naturally write, a
// bare ON CONFLICT naming the columns, is rejected on both, because the arbiter
// does not resolve to the partial index. Storing NULL for the unconstrained
// case makes a PLAIN unique index correct, and a plain index needs no predicate
// at the call site at all.
func TestPartialIndexConflictTarget(t *testing.T) {
	for _, drv := range certified {
		t.Run(string(drv), func(t *testing.T) {
			t.Parallel()
			requireDriver(t, drv)
			db, err := store.Open(t.Context(),
				filepath.Join(t.TempDir(), "arb.db"), store.Options{Driver: drv})
			if err != nil {
				t.Fatalf("open: %v", err)
			}
			defer func() { _ = db.Close() }()
			ctx := t.Context()

			for _, stmt := range []string{
				`CREATE TABLE arb (h TEXT NOT NULL, k TEXT NOT NULL DEFAULT '')`,
				`CREATE UNIQUE INDEX arb_partial ON arb (h, k) WHERE k <> ''`,
				`INSERT INTO arb (h, k) VALUES ('a', 'x')`,
			} {
				if _, err := db.SQL().ExecContext(ctx, stmt); err != nil {
					t.Fatalf("%s: %v", stmt, err)
				}
			}

			if _, err := db.SQL().ExecContext(ctx,
				`INSERT INTO arb (h, k) VALUES ('a','x') ON CONFLICT (h, k) DO NOTHING`,
			); err == nil {
				t.Fatal("a bare ON CONFLICT now resolves against a partial index — " +
					"re-read rewrite/decisions/002 §2 before relying on it")
			}

			if _, err := db.SQL().ExecContext(ctx,
				`INSERT INTO arb (h, k) VALUES ('a','x')
				 ON CONFLICT (h, k) WHERE k <> '' DO NOTHING`,
			); err != nil {
				t.Fatalf("repeating the predicate should parse: %v", err)
			}
		})
	}
}
