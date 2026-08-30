package store

import (
	"context"
	"database/sql"
)

// Capabilities records what the live driver can actually do, measured at Open
// rather than assumed from a version number.
//
// The three answers here are the ones where the docs and the code disagree:
// Turso ships the vector column type and the distance
// functions, but its ANN index and its full-text index are announced surface
// not yet reachable from Go. That is still true at the pinned version, and it
// is the reason this type survived the drop of the second driver: with one
// driver these are no longer a comparison between two implementations, they
// are a TRIPWIRE on one. A pin bump that lands a
// feature turns a skipping test into a passing one, and a pin bump that loses
// one fails the build — see capability_test.go, which turns each answer into a
// test that passes, skips, or fails deliberately.
//
// A probe never fails Open, including VectorFunctions. A driver regression
// here should read as a capability that vanished — in the log line and in the
// test — not as an engine that will not start. What it would actually cost is
// worth stating precisely rather than as "best effort": two of recall's three
// callers log and carry on with an empty block (the Plan-phase prefetches),
// and the third is the `query_episodes` builtin, which propagates and surfaces
// as a tool error to the planner. So a company keeps running and its seats
// stop remembering.
type Capabilities struct {
	// VectorFunctions is vector32() and vector_distance_cos(): similarity
	// computed by the database. It is what recall's ORDER BY is written
	// against — the distance arithmetic runs in the database and only the
	// rows that survive the limit cross the driver boundary.
	//
	// TRUE on the pinned driver. It was a branch when there were two
	// drivers and one of them had no vector functions at all; it is a
	// requirement now, and the honest reading of a false here is "this
	// build's recall returns nothing", not "recall takes the other path".
	VectorFunctions bool

	// VectorIndex is an approximate-nearest-neighbour index over a vector
	// column. FALSE on the pinned driver, so recall is a SCAN behind the
	// per-agent index rather than an index lookup — the arithmetic is the
	// database's, the row set it runs over is still every embedded row for
	// one agent. That is correct at the real workload (a per-agent diary
	// and compacted episodes, thousands of rows, always filtered by agent
	// first) and it is not the same claim as "native vector search".
	VectorIndex bool

	// FullTextSearch is a queryable full-text index — Turso's own fts()
	// index expression, or an fts5 virtual table. FALSE on the pinned
	// driver: fts5 is not a registered module and fts() is a parse error
	// in CREATE INDEX (measured).
	//
	// Nothing depends on it. Knowledge search is the external backends
	// behind knowledge.Searcher, and this is here so that the day Turso's
	// index reaches Go is a day this project notices.
	FullTextSearch bool
}

// probe measures each capability against the live connection.
//
// Every probe runs inside its own transaction and rolls it back, so a probe
// that half-succeeds leaves nothing behind and a probe that fails cannot
// poison the next one — a failed statement does not abort a SQLite
// transaction, but continuing to use one after an error is a rule the driver
// does not document, and one transaction per question costs microseconds.
//
// A probe never fails Open. An unavailable capability is an answer, not an
// error — see the type doc for what each false actually costs.
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
// parses". The fts5 arm is not dead code for a single driver: it is the shape
// Turso would most plausibly land, and a probe that only asked for the
// syntax this driver rejects today could report a capability it has as
// missing.
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
