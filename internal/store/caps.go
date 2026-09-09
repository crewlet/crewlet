package store

import (
	"context"
	"database/sql"
	"strings"
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

	// WithoutRowid is whether the driver accepts a `WITHOUT ROWID` table.
	//
	// FALSE on the pinned driver, which answers `Parse error: WITHOUT ROWID
	// tables are an experimental feature` — measured through Open with this
	// engine's own session pragmas. It is a TRIPWIRE with a caller waiting
	// for it: `kb_vectors_bin` is the narrow table every semantic search
	// scans first, and its rowid is pure overhead on a table whose primary
	// key is the only way anything reaches a row. The same is true of
	// `tracker_log_deferred_scope`.
	//
	// The design must probe what it depends on, which is why this is here
	// rather than remembered: a migration written with `WITHOUT ROWID`
	// would fail on every node and every store would refuse to open, and
	// nothing in the tree uses it today so no existing test would catch it.
	WithoutRowid bool

	// MaxVariables is how many bound parameters one statement accepts,
	// measured rather than assumed.
	//
	// It is the chunk size for every multi-row INSERT the appliers write:
	// rows ÷ columns per statement, which is the difference between one
	// round trip and a thousand on a batch this engine writes constantly.
	// SQLite has raised this default once already (999 → 32 766 in 3.32),
	// so a hardcoded 999 is 33× the round trips on the driver actually
	// pinned here, and a hardcoded 32 766 is a refused statement on any
	// engine that did not follow. The probe costs one prepare at open.
	//
	// A conservative 999 when the probe cannot tell: too small is slow,
	// too large is a runtime failure on a statement the caller cannot
	// retry differently.
	MaxVariables int

	// PageCacheKiB is `PRAGMA cache_size` as the driver actually applied
	// it, in kibibytes, or 0 when it could not be read.
	//
	// The session list asks for a deliberate size (see openPool). Asking is
	// not the same as getting: a driver that ignores the pragma leaves
	// every connection on its own default, and the symptom — a query plan
	// that spills where it used to fit — appears nowhere near the cause.
	// Reading it back is what turns the request into a fact.
	PageCacheKiB int
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
		WithoutRowid:    probeWithoutRowid(ctx, db),
		MaxVariables:    probeMaxVariables(ctx, db),
		PageCacheKiB:    probePageCache(ctx, db),
	}
}

// conservativeMaxVariables is the answer when the probe cannot establish one.
//
// SQLite's pre-3.32 default, which every engine in this family accepts. Too
// small costs round trips; too large costs a refused statement at the moment
// a batch is largest, which is the failure that cannot be retried into
// success.
const conservativeMaxVariables = 999

// probeMaxVariables finds the largest parameter count one statement accepts,
// by BINARY SEARCH over prepares.
//
// A prepare rather than an execution: the limit is a parser bound, so a
// statement that prepares would run, and preparing touches no table and needs
// no transaction. `SELECT ?,?,…` is the narrowest statement that carries N
// parameters and nothing else.
//
// The search is bounded above by 32 766 — SQLite's own post-3.32 default and
// the largest value any engine in this family reports — so the loop is at
// most fifteen prepares and cannot run away on a driver with no limit at all.
func probeMaxVariables(ctx context.Context, db *sql.DB) int {
	const ceiling = 32766
	accepts := func(n int) bool {
		stmt, err := db.PrepareContext(ctx, selectParams(n))
		if err != nil {
			return false
		}
		_ = stmt.Close()
		return true
	}
	if !accepts(conservativeMaxVariables) {
		// Below the floor every engine here clears. Reported rather than
		// searched further: something is wrong with the probe or the
		// driver, and a number derived from that is worse than the
		// documented minimum.
		return conservativeMaxVariables
	}
	if accepts(ceiling) {
		return ceiling
	}
	low, high := conservativeMaxVariables, ceiling
	for low+1 < high {
		mid := low + (high-low)/2
		if accepts(mid) {
			low = mid
		} else {
			high = mid
		}
	}
	return low
}

// selectParams builds `SELECT ?, ?, …` with n placeholders.
func selectParams(n int) string {
	var b strings.Builder
	b.Grow(len("SELECT ") + 3*n)
	b.WriteString("SELECT ")
	for i := range n {
		if i > 0 {
			b.WriteString(",")
		}
		b.WriteString("?")
	}
	return b.String()
}

// probePageCache reads `PRAGMA cache_size` back and converts it to KiB.
//
// The pragma answers in PAGES when positive and in KiB (negated) when
// negative, which is the whole reason this is a conversion rather than a
// read: a caller comparing the raw number against a byte budget would be
// comparing two different units depending on how it was set.
func probePageCache(ctx context.Context, db *sql.DB) int {
	// ONE CONNECTION for both pragmas. `cache_size` is per-connection state
	// and `page_size` is a property of the file, so reading them through the
	// pool can pair one connection's cache with another's page size — which
	// is a number that describes neither.
	conn, err := db.Conn(ctx)
	if err != nil {
		return 0
	}
	defer func() { _ = conn.Close() }()

	var raw int
	if err := conn.QueryRowContext(ctx, `PRAGMA cache_size`).Scan(&raw); err != nil {
		return 0
	}
	var pageSize int
	if err := conn.QueryRowContext(ctx, `PRAGMA page_size`).Scan(&pageSize); err != nil {
		return 0
	}
	return pageCacheKiB(raw, pageSize)
}

// pageCacheKiB converts `PRAGMA cache_size` into kibibytes.
//
// The pragma answers in PAGES when positive and in KiB (negated) when
// negative, which is the whole reason this is a conversion rather than a read:
// a caller comparing the raw number against a byte budget would be comparing
// two different units depending on how the value was set.
//
// Pure, so the rule is testable without a driver — which is what the first
// version of this was not, and it measured a pool rather than the arithmetic.
func pageCacheKiB(raw, pageSize int) int {
	if raw < 0 {
		return -raw
	}
	if pageSize <= 0 {
		return 0
	}
	return raw * pageSize / 1024
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

// probeVectorIndex tries to build an ANN index. The index METHOD is the part
// Turso's parser rejects today, so creating one is the only honest test — the
// column type and the distance functions are already present and prove
// nothing about it.
//
// TURSO'S GRAMMAR, not libSQL's. The probe this replaces asked for
// `libsql_vector_idx(e)`, which is a function of the C fork and a parse error
// here whatever the driver ever ships — so it could only ever answer false,
// and a tripwire that cannot fire is a claim rather than a measurement. Both
// spellings Turso could plausibly land are tried.
func probeVectorIndex(ctx context.Context, db *sql.DB) bool {
	for _, method := range []string{"vector", "diskann"} {
		if probeInRollback(ctx, db, []string{
			`CREATE TABLE crewlet_probe_vec (id TEXT PRIMARY KEY, e F32_BLOB(4))`,
			`CREATE INDEX crewlet_probe_vec_idx ON crewlet_probe_vec USING ` +
				method + ` (e)`,
		}) {
			return true
		}
	}
	return false
}

// probeWithoutRowid asks for the narrower table shape two of this engine's own
// tables would take.
//
// CREATED AND ROLLED BACK, because the refusal is a PARSE error rather than a
// capability flag: there is no pragma to read, and the only honest question is
// whether the statement a migration would carry is one this driver accepts.
func probeWithoutRowid(ctx context.Context, db *sql.DB) bool {
	return probeInRollback(ctx, db, []string{
		`CREATE TABLE crewlet_probe_wr (a TEXT NOT NULL, b TEXT NOT NULL, ` +
			`PRIMARY KEY (a, b)) WITHOUT ROWID`,
	})
}

// probeFullText accepts either mechanism, because the capability the engine
// would eventually use is "a full-text index exists", not "this exact syntax
// parses". The fts5 arm is not dead code for a single driver: it is the shape
// a SQLite-compatible engine would most plausibly land, and a probe that only
// asked for the syntax this driver rejects today could report a capability it
// has as missing.
//
// THE SECOND ARM IS TURSO'S OWN GRAMMAR — `CREATE INDEX … USING fts (col)`,
// gated behind the experimental `index_method` feature. The probe it replaces
// wrote `(fts(body))` as an index EXPRESSION, which Turso has never accepted
// in any build, so the arm meant to catch the feature landing could not have
// caught it.
//
// A failure naming the experimental GATE rather than the module is a
// misconfigured probe, not an absent capability: the driver accepts a
// misspelled experimental name silently, so a probe that ran without the flag
// would report false forever and nobody would know the difference. It is
// therefore logged loudly and still answers false — the tripwire's job is to
// notice the day the answer changes, and an answer nobody can trust is worse
// than a false one.
func probeFullText(ctx context.Context, db *sql.DB) bool {
	if probeInRollback(ctx, db, []string{
		`CREATE VIRTUAL TABLE crewlet_probe_fts USING fts5(body)`,
	}) {
		return true
	}
	ok, err := probeReporting(ctx, db, []string{
		`CREATE TABLE crewlet_probe_txt (body TEXT NOT NULL)`,
		`CREATE INDEX crewlet_probe_txt_idx ON crewlet_probe_txt USING fts (body)`,
	})
	if err != nil && strings.Contains(err.Error(), "experimental feature") {
		log.Warn("store_fts_probe_gated", "error", err.Error(),
			"detail", "the driver recognises the fts index method but refuses it "+
				"without its experimental flag; full-text search is reported "+
				"absent and this probe is measuring the gate rather than the "+
				"feature")
	}
	return ok
}

// probeReporting is probeInRollback with the failure kept, for a probe whose
// ERROR distinguishes "the feature is absent" from "this probe asked wrong".
func probeReporting(ctx context.Context, db *sql.DB, stmts []string) (bool, error) {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer func() { _ = tx.Rollback() }()
	for _, stmt := range stmts {
		if _, err := tx.ExecContext(ctx, stmt); err != nil {
			return false, err
		}
	}
	return true, nil
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
