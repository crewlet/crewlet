package store

import (
	"context"
	"database/sql"
)

// Capabilities records what the live driver can actually do, measured at Open
// rather than assumed from a version number.
//
// The three answers here are the ones rewrite/decisions/002 found the docs and
// the code disagreeing about: Turso ships the vector column type and the
// distance functions, but its ANN index and its full-text index are announced
// surface not yet reachable from Go. Each feature sits behind exactly one
// query function, so when a driver upgrade lands one the change is that
// function and nothing else — and the probe is what tells the change it may
// happen. See capability_test.go, which turns each answer into a test that
// passes, skips, or fails deliberately.
type Capabilities struct {
	// VectorFunctions is vector32() and vector_distance_cos(): similarity
	// computed by the database. When false, recall runs a cosine loop in
	// Go over the same rows. Both are correct at the real workload — a
	// per-agent diary and compacted episodes, thousands of rows, always
	// filtered by agent first — so this decides where the arithmetic
	// happens, not whether recall works.
	VectorFunctions bool

	// VectorIndex is an approximate-nearest-neighbour index over a vector
	// column. False means recall is a brute-force scan behind the
	// per-agent index, which is what it is today on both drivers.
	VectorIndex bool

	// FullTextSearch is a queryable full-text index — an fts5 virtual
	// table on mainline SQLite, or Turso's own fts() index expression.
	// Nothing in v1 depends on it: knowledge search is the external
	// backends behind KnowledgeSearcher, exactly as in the Python engine.
	FullTextSearch bool
}

// probe measures each capability against the live connection.
//
// Every probe runs inside its own transaction and rolls it back, so a probe
// that half-succeeds leaves nothing behind and a probe that fails cannot
// poison the next one — a failed statement does not abort a SQLite
// transaction, but continuing to use one after an error is a rule neither
// driver documents, and one transaction per question costs microseconds.
//
// A probe never fails Open. An unavailable capability is an answer, not an
// error: the whole point is that the engine runs on a driver that has none of
// them.
func probe(ctx context.Context, db *sql.DB) Capabilities {
	return Capabilities{
		VectorFunctions: probeVectorFunctions(ctx, db),
		VectorIndex:     probeVectorIndex(ctx, db),
		FullTextSearch:  probeFullText(ctx, db),
	}
}

// probeVectorFunctions asks for a distance between two literal vectors. It
// needs no table, so a bare query is the whole probe.
func probeVectorFunctions(ctx context.Context, db *sql.DB) bool {
	var d float64
	err := db.QueryRowContext(ctx,
		`SELECT vector_distance_cos(vector32('[1,0,0,0]'), vector32('[0,1,0,0]'))`,
	).Scan(&d)
	return err == nil
}

// probeVectorIndex tries to build an ANN index. The index expression is the
// part Turso's parser rejects today, so creating one is the only honest test —
// the column type and the distance functions are already present and prove
// nothing about it.
func probeVectorIndex(ctx context.Context, db *sql.DB) bool {
	return probeInRollback(ctx, db, []string{
		`CREATE TABLE crewlet_probe_vec (id TEXT PRIMARY KEY, e F32_BLOB(4))`,
		`CREATE INDEX crewlet_probe_vec_idx ON crewlet_probe_vec (libsql_vector_idx(e))`,
	})
}

// probeFullText accepts either mechanism, because the capability the engine
// would eventually use is "a full-text index exists", not "this exact syntax
// parses". A true answer still leaves the query layer to branch on which one
// answered — which is why Phase 8 is design-gated rather than a port.
func probeFullText(ctx context.Context, db *sql.DB) bool {
	if probeInRollback(ctx, db, []string{
		`CREATE VIRTUAL TABLE crewlet_probe_fts USING fts5(body)`,
	}) {
		return true
	}
	return probeInRollback(ctx, db, []string{
		`CREATE TABLE crewlet_probe_txt (body TEXT NOT NULL)`,
		`CREATE INDEX crewlet_probe_txt_idx ON crewlet_probe_txt (fts(body))`,
	})
}

// probeInRollback runs statements in a transaction that is always rolled back,
// reporting whether every one of them succeeded.
func probeInRollback(ctx context.Context, db *sql.DB, stmts []string) bool {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return false
	}
	defer func() { _ = tx.Rollback() }()
	for _, stmt := range stmts {
		if _, err := tx.ExecContext(ctx, stmt); err != nil {
			return false
		}
	}
	return true
}
