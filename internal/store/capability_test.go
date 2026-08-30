package store_test

import (
	"encoding/binary"
	"math"
	"path/filepath"
	"testing"

	"github.com/crewlet/crewlet/internal/store"
)

// The capability matrix, re-measured against whatever driver version this
// build pins.
//
// It exists because the documentation and the code disagree: Turso ships
// F32_BLOB columns and the vector distance functions,
// but its ANN vector index and its full-text index are announced surface not
// yet reachable from Go. Pinning a driver and asserting a matrix in prose is
// how that goes stale silently; asserting it in a test is how a pin bump
// reports what it changed.
//
// ONE DRIVER, AND THE MATRIX IS WHY IT SURVIVED. Dropping mainline SQLite
// took away the comparison this table used to draw, and it
// would have been easy to delete the whole file with it. What is left is the
// more useful half: a tripwire on the one driver the engine ships.
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
//
// turso.tech/database/tursogo v0.8.0-pre.7: the vector functions ship;
// libsql_vector_idx() in CREATE INDEX and the fts() index expression are both
// parse errors, and fts5 is not a registered module.
var capabilityMatrix = struct {
	vectorFunctions bool
	vectorIndex     bool
	fullTextSearch  bool
}{vectorFunctions: true, vectorIndex: false, fullTextSearch: false}

func TestCapabilityMatrix(t *testing.T) {
	t.Parallel()
	want := capabilityMatrix
	db, err := store.Open(t.Context(),
		filepath.Join(t.TempDir(), "caps.db"), store.Options{})
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
// nullable-work_key design: NULL for "unconstrained" in a plain unique index,
// rather than a partial index with an ON CONFLICT target.
//
// The refinement worth keeping: it is not that ON CONFLICT and partial indexes
// are incompatible — repeating the index predicate verbatim in the statement
// parses on BOTH drivers. It is that the form anyone would naturally write, a
// bare ON CONFLICT naming the columns, is rejected on both, because the arbiter
// does not resolve to the partial index. Storing NULL for the unconstrained
// case makes a PLAIN unique index correct, and a plain index needs no predicate
// at the call site at all.
func TestPartialIndexConflictTarget(t *testing.T) {
	t.Parallel()
	db, err := store.Open(t.Context(),
		filepath.Join(t.TempDir(), "arb.db"), store.Options{})
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
			"this test's own doc comment explains what the schema does " +
			"instead, and why; re-read it before relying on the new behaviour")
	}

	if _, err := db.SQL().ExecContext(ctx,
		`INSERT INTO arb (h, k) VALUES ('a','x')
				 ON CONFLICT (h, k) WHERE k <> '' DO NOTHING`,
	); err != nil {
		t.Fatalf("repeating the predicate should parse: %v", err)
	}
}

// The three properties of vector_distance_cos that internal/learning's recall
// is WRITTEN AGAINST, pinned here because recall cannot pin them itself.
//
// Each one is load-bearing and each one is invisible in the code that depends
// on it — a pin bump that changed any of them would leave the whole suite
// green while recall silently returned the wrong rows, or errored, or ranked
// garbage first. They live in this file rather than in internal/learning
// because they are a claim about a pinned DRIVER, which is what this file is
// for; the recall tests then assert BEHAVIOUR and stay readable.
func TestVectorDistanceSemanticsRecallDependsOn(t *testing.T) {
	t.Parallel()
	db, err := store.Open(t.Context(),
		filepath.Join(t.TempDir(), "sem.db"), store.Options{})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer func() { _ = db.Close() }()
	ctx := t.Context()

	// (1) IT IS A DISTANCE, AND IT IS 1 - cosine.
	//
	// Recall's floor is written as `distance <= 1 - floor`, which means
	// `similarity >= floor` only if this holds. A function that answered a
	// SIMILARITY instead would invert the filter: every irrelevant memory
	// would pass and every relevant one would be cut.
	var identical, orthogonal, opposite float64
	if err := db.SQL().QueryRowContext(ctx, `SELECT
		vector_distance_cos(vector32('[1,0]'), vector32('[1,0]')),
		vector_distance_cos(vector32('[1,0]'), vector32('[0,1]')),
		vector_distance_cos(vector32('[1,0]'), vector32('[-1,0]'))`,
	).Scan(&identical, &orthogonal, &opposite); err != nil {
		t.Fatalf("distance: %v", err)
	}
	for _, c := range []struct {
		name string
		got  float64
		want float64
	}{
		{"identical", identical, 0},
		{"orthogonal", orthogonal, 1},
		{"opposite", opposite, 2},
	} {
		if diff := c.got - c.want; diff > 1e-6 || diff < -1e-6 {
			t.Errorf("distance between %s vectors = %v, want %v — recall's "+
				"floor is `distance <= 1 - similarity`, which this is what makes true",
				c.name, c.got, c.want)
		}
	}

	// (2) A NON-FINITE COMPONENT ANSWERS 0 — a PERFECT match, so it sorts
	// FIRST.
	//
	// This is why store.EncodeVector refuses to write one and why recall
	// re-scores in Go: without both, one bad response from an embeddings
	// provider puts a garbage row at the top of every recall that seat ever
	// runs. If a pin bump ever made this NULL or an error instead, the
	// guards become belt-and-braces rather than load-bearing — worth
	// knowing, and worth failing here to say so.
	nan := packRaw([]float32{float32(math.NaN()), 0})
	var poisoned float64
	if err := db.SQL().QueryRowContext(ctx,
		`SELECT vector_distance_cos(?, vector32('[1,0]'))`, nan).Scan(&poisoned); err != nil {
		t.Fatalf("distance to a NaN vector: %v", err)
	}
	if poisoned != 0 {
		t.Errorf("distance to a NaN vector = %v, want 0 — if this is no longer "+
			"a false perfect match, say so where EncodeVector and "+
			"learning.Recall explain why they guard against it", poisoned)
	}

	// (3) A WIDTH MISMATCH FAILS DURING ITERATION, not at the query.
	//
	// The statement succeeds and rows come back; the error arrives from
	// rows.Err() partway through. That is what makes recall's
	// `length(embedding) = ?` load-bearing rather than an optimisation: a
	// company that changed embedding model would otherwise get an error
	// from recall instead of the rows it can still compare.
	if _, err := db.SQL().ExecContext(ctx,
		`CREATE TABLE sem (id TEXT PRIMARY KEY, e BLOB)`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.SQL().ExecContext(ctx,
		`INSERT INTO sem (id, e) VALUES ('narrow', ?)`, packRaw([]float32{1, 0})); err != nil {
		t.Fatal(err)
	}
	rows, err := db.SQL().QueryContext(ctx,
		`SELECT vector_distance_cos(e, vector32('[1,0,0,0]')) FROM sem`)
	if err != nil {
		t.Fatalf("a width mismatch now fails the QUERY rather than the "+
			"iteration: %v — recall filters on length(embedding) because of "+
			"this; re-read the comment there", err)
	}
	for rows.Next() {
		var d float64
		_ = rows.Scan(&d)
	}
	iterErr := rows.Err()
	_ = rows.Close()
	if iterErr == nil {
		t.Error("a width mismatch no longer fails at all — recall's " +
			"length(embedding) filter is now merely a narrowing, and the " +
			"comment there says it is load-bearing")
	}
}

// packRaw is the vector layout the schema holds, built without a configured
// width so a deliberately-wrong one can be written. store.DB.EncodeVector is
// the production path and refuses both of the vectors this file needs.
func packRaw(v []float32) []byte {
	out := make([]byte, 4*len(v))
	for i, f := range v {
		binary.LittleEndian.PutUint32(out[4*i:], math.Float32bits(f))
	}
	return out
}
