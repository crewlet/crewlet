package store_test

import (
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"path/filepath"
	"slices"
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
// A FOURTH OUTCOME IS THE GATE, and it is why `gated` is in the matrix beside
// the booleans. Turso refuses `USING <method>` and `WITHOUT ROWID` as
// experimental BEFORE it resolves the module or builds the table, so an
// unflagged connection cannot tell a feature that exists behind the gate from
// one that was never written — all three read false and only one of them is
// actually missing. [store.Capabilities.Gated] carries what the probe found
// behind the gate, so a feature landing in a gated build is recorded here
// rather than discovered the day somebody tries to use it.
//
// turso.tech/database/tursogo v0.8.0-pre.8, measured both ways:
//
//   - the vector functions ship, unflagged.
//   - fts5 is not a registered module, and behind `experimental=index_method`
//     `USING fts`, `USING vector` and `USING diskann` all answer `unknown
//     module name` — so the full-text and ANN indexes are genuinely ABSENT
//     rather than gated.
//   - WITHOUT ROWID is PRESENT AND GATED: refused on the pool as an
//     experimental feature, accepted on a connection carrying
//     `experimental=without_rowid`.
var capabilityMatrix = struct {
	vectorFunctions bool
	vectorIndex     bool
	fullTextSearch  bool
	withoutRowid    bool
	gated           []string
}{
	vectorFunctions: true, vectorIndex: false,
	fullTextSearch: false, withoutRowid: false,
	gated: []string{"without_rowid"},
}

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
	t.Run("WithoutRowid", func(t *testing.T) {
		gate(t, caps.WithoutRowid, want.withoutRowid,
			"WITHOUT ROWID tables reach the Go driver")
		exerciseWithoutRowid(t, db)
	})
	// THE GATED SET IS ASSERTED AS A WHOLE rather than per capability,
	// because both directions are the notification this file exists to
	// give: a name ARRIVING is a feature Turso implemented while this
	// engine's connections still cannot reach it — the day to decide
	// whether to opt the DSN in — and a name LEAVING is either the gate
	// lifting (the boolean above turns true and its exercise runs) or the
	// probe's flag name going stale, which is silent by construction
	// because the engine ignores an experimental name it does not know.
	t.Run("Gated", func(t *testing.T) {
		if !slices.Equal(caps.Gated, want.gated) {
			t.Fatalf("gated = %v, matrix records %v — a capability moved "+
				"across Turso's experimental gate; re-measure and update "+
				"capabilityMatrix", caps.Gated, want.gated)
		}
	})
}

// exerciseWithoutRowid does the thing the capability is for: the narrow
// clustered table `kb_vectors_bin` would be if the driver allowed one.
//
// THE DAY THIS RUNS IS THE DAY THE DECISION IS REVISITED. The stage-1 scan
// reaches a row only through its primary key, so the rowid it is forced to
// carry is an extra b-tree and an extra indirection per candidate — on the one
// table every semantic search reads end to end.
func exerciseWithoutRowid(t *testing.T, db *store.DB) {
	t.Helper()
	ctx := t.Context()
	if _, err := db.SQL().ExecContext(ctx,
		`CREATE TABLE cap_wr (source TEXT NOT NULL, source_id TEXT NOT NULL, `+
			`bits BLOB NOT NULL, PRIMARY KEY (source, source_id)) WITHOUT ROWID`,
	); err != nil {
		t.Fatalf("create WITHOUT ROWID: %v", err)
	}
	if _, err := db.SQL().ExecContext(ctx,
		`INSERT INTO cap_wr (source, source_id, bits) VALUES ('page', 'p1', x'00')`,
	); err != nil {
		t.Fatalf("insert: %v", err)
	}
	var got string
	if err := db.SQL().QueryRowContext(ctx,
		`SELECT source_id FROM cap_wr WHERE source = 'page'`).Scan(&got); err != nil {
		t.Fatalf("read back: %v", err)
	}
	if got != "p1" {
		t.Fatalf("read back %q", got)
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

// exerciseVectorIndex builds the index the probe measured — in TURSO'S
// grammar, which is the only one that can ever run here.
//
// It asked for `libsql_vector_idx(e)` until this was written: a function of
// the C fork, a parse error on this driver whatever it ever ships, and
// therefore an exercise that could only fail — on the one day it runs, which
// is the day the capability lands. The probe had already been corrected to
// Turso's spelling; this had not, so the two disagreed about what the
// capability even is.
func exerciseVectorIndex(t *testing.T, db *store.DB) {
	t.Helper()
	ctx := t.Context()
	if _, err := db.SQL().ExecContext(ctx,
		`CREATE TABLE cap_ann (id TEXT PRIMARY KEY, e F32_BLOB(4))`); err != nil {
		t.Fatalf("create: %v", err)
	}
	// Both spellings, in the probe's own order: whichever method the
	// driver landed is the one that answered true.
	var errs []error
	for _, method := range []string{"vector", "diskann"} {
		_, err := db.SQL().ExecContext(ctx,
			`CREATE INDEX cap_ann_idx ON cap_ann USING `+method+` (e)`)
		if err == nil {
			return
		}
		errs = append(errs, fmt.Errorf("USING %s: %w", method, err))
	}
	t.Fatalf("ANN index: the probe reports one, but neither method builds one: %v",
		errors.Join(errs...))
}

// exerciseFullText searches through WHICHEVER mechanism the probe measured.
//
// THE FALLBACK USED TO BE A SKIP, which made the arm most likely to be the one
// that lands the least tested: the probe accepts an fts5 virtual table OR
// Turso's own `USING fts` index method, and today only the second could
// plausibly arrive — so the day the capability turns true, this reported a
// pass having run no search at all. A skip inside an exercise is the exercise
// admitting it does not know what it is testing.
func exerciseFullText(t *testing.T, db *store.DB) {
	t.Helper()
	if _, err := db.SQL().ExecContext(t.Context(),
		`CREATE VIRTUAL TABLE cap_fts USING fts5(body)`); err != nil {
		// The MATCH is written against the indexed COLUMNS rather than
		// the table, which is the whole difference between the two
		// mechanisms at the query layer — so it is the half worth
		// exercising.
		exerciseNativeFullText(t, db)
		return
	}
	exerciseMatch(t, db, "cap_fts", `cap_fts MATCH 'brown'`)
}

// exerciseNativeFullText is Turso's own index method, in the grammar the probe
// builds and the query layer would have to write.
func exerciseNativeFullText(t *testing.T, db *store.DB) {
	t.Helper()
	if _, err := db.SQL().ExecContext(t.Context(),
		`CREATE TABLE cap_fts_native (body TEXT NOT NULL)`); err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := db.SQL().ExecContext(t.Context(),
		`CREATE INDEX cap_fts_native_idx ON cap_fts_native USING fts (body)`,
	); err != nil {
		t.Fatalf("the probe reports full text, but neither fts5 nor the fts "+
			"index method builds an index: %v", err)
	}
	exerciseMatch(t, db, "cap_fts_native", `(body) MATCH 'brown'`)
}

// exerciseMatch inserts one row and searches for a word in it.
func exerciseMatch(t *testing.T, db *store.DB, table, match string) {
	t.Helper()
	ctx := t.Context()
	if _, err := db.SQL().ExecContext(ctx,
		`INSERT INTO `+table+` (body) VALUES ('the quick brown fox')`); err != nil {
		t.Fatalf("insert: %v", err)
	}
	var n int
	if err := db.SQL().QueryRowContext(ctx,
		`SELECT COUNT(*) FROM `+table+` WHERE `+match).Scan(&n); err != nil {
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

// EVERY ESTATE REPORTS THE PROBED LIMIT, however it was opened.
//
// The replicated estate used to report a zero MaxVariables on its own handle
// until [Open] copied the node's probe onto it — after openEstate had already
// written its `store_opened` line — and a standalone
// [store.OpenEstate](EstateReplicated) handle never got one at all. That was
// invisible for as long as nothing read the field. [store.InsertRows] reads it
// to size every applier's statements and treats 0 as "no room for even one
// row", so a zero here is not a cosmetic log defect: it is every child-row
// write silently back on one statement per row, with nothing to say so.
func TestEveryEstateReportsTheProbedVariableLimit(t *testing.T) {
	t.Parallel()
	ctx := t.Context()

	node, err := store.Open(ctx, filepath.Join(t.TempDir(), "caps.db"), store.Options{})
	if err != nil {
		t.Fatalf("open a node: %v", err)
	}
	defer func() { _ = node.Close() }()

	want := node.Caps().MaxVariables
	if want <= 0 {
		t.Fatalf("the node estate probed MaxVariables = %d, so this test can "+
			"prove nothing about the peer", want)
	}
	if got := node.Replicated().Caps().MaxVariables; got != want {
		t.Errorf("the replicated peer reports MaxVariables = %d, want the node's "+
			"probed %d — every applier sizes its multi-row inserts from this, "+
			"and a zero is one statement per row", got, want)
	}

	// The standalone shape a snapshot artefact and a backup member take.
	alone, err := store.OpenEstate(ctx, store.EstateReplicated,
		filepath.Join(t.TempDir(), "alone.db"), store.Options{})
	if err != nil {
		t.Fatalf("open a lone replicated estate: %v", err)
	}
	defer func() { _ = alone.Close() }()
	if got := alone.Caps().MaxVariables; got <= 0 {
		t.Errorf("a lone replicated estate reports MaxVariables = %d — it has no "+
			"sibling to inherit a probe from, so it has to run its own", got)
	}
}
