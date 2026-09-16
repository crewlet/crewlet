package store

import (
	"context"
	"database/sql"
	"slices"
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
// AND A REFUSAL IS NOT AUTOMATICALLY AN ABSENCE. Turso puts three of these
// behind experimental flags and refuses at the FLAG, before it resolves the
// module or builds the table — so the error an unflagged connection gets is
// the same whether the feature is fully implemented behind the gate or was
// never written. Measured only that way, all three read false and one of them
// (WITHOUT ROWID) was wrong. Each therefore asks twice, and Gated is the
// second answer.
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
	//
	// ABSENT RATHER THAN GATED, and the two are only distinguishable
	// behind the flag: `USING vector` and `USING diskann` both answer
	// `unknown module name` on a connection that opted into
	// `index_method`, so there is nothing on the far side of the gate to
	// opt into. See Gated.
	VectorIndex bool

	// FullTextSearch is a queryable full-text index — Turso's own `USING
	// fts` index method, or an fts5 virtual table. FALSE on the pinned
	// driver, and ABSENT rather than gated: fts5 is not a registered
	// module, and `USING fts` answers `unknown module name 'fts'` on a
	// connection that opted into `index_method` (measured — see Gated).
	//
	// Nothing depends on it. The engine's lexical search is its OWN — the
	// analyzer in internal/textindex and the inverted list internal/search
	// maintains — which is what this false is the reason for. So a true here
	// would not fix anything; it would open a question about whether the
	// engine's own index should be replaced by the database's, which is a
	// decision rather than a fallback. That is exactly why it is measured.
	FullTextSearch bool

	// WithoutRowid is whether a `WITHOUT ROWID` table is reachable on the
	// connections THIS ENGINE OPENS.
	//
	// FALSE on the pinned driver — and `Gated` carries "without_rowid"
	// beside it, which is a different fact from the other two falses here
	// and the reason that field exists. The engine IMPLEMENTS the feature
	// and serves it to a connection that opted into
	// `?experimental=without_rowid`; this engine's pool opts into nothing,
	// so it is refused with `WITHOUT ROWID tables are an experimental
	// feature` (measured both ways).
	//
	// It is a TRIPWIRE with a caller waiting for it: `kb_vectors_bin` is the
	// narrow table every semantic search scans first, and its rowid is pure
	// overhead on a table whose primary key is the only way anything reaches
	// a row. The same is true of `tracker_log_deferred_scope`.
	//
	// A gated true is NOT a licence to write one. Opting the DSN in turns an
	// experimental feature of a pre-release engine on for every statement the
	// store runs, and the tables are already built with rowids, so adopting
	// it is a new numbered migration that rebuilds them rather than an edit
	// to the ones that shipped. That is a decision to take deliberately,
	// which is what publishing the reading is for.
	//
	// The design must probe what it depends on, which is why this is here
	// rather than remembered: a migration written with `WITHOUT ROWID`
	// would fail on every node and every store would refuse to open, and
	// nothing in the tree uses it today so no existing test would catch it.
	WithoutRowid bool

	// Gated names the capabilities this build of the database engine
	// IMPLEMENTS but serves only to a connection that opted into one of
	// Turso's experimental features — which this engine's pool does not.
	//
	// A CAPABILITY'S NAME, not the flag's: one gate stands in front of more
	// than one of the fields above, so a list of flags could not say which
	// of them had landed. Each name here matches the field it explains, and
	// [gatedCapability] holds the flag that would unlock it.
	//
	// WITHOUT IT EVERY PROBE ABOVE MEASURED THE GATE RATHER THAN THE
	// FEATURE, and could not have done otherwise: Turso refuses `USING
	// <anything>` and `WITHOUT ROWID` at the gate, BEFORE it resolves the
	// module or builds the table, so the refusal an unflagged connection
	// gets is identical whether the feature is fully implemented behind it
	// or was never written. All three read false, and the one that is
	// actually present was indistinguishable from the two that are not.
	//
	// So a gated probe asks TWICE — once on the live pool, which is what
	// `WithoutRowid` and its siblings report, and once on a throwaway
	// in-memory connection carrying the flag, which is what this reports.
	// Empty is the healthy reading of a driver with nothing left gated;
	// a name appearing here on a pin bump is the day to decide whether to
	// opt in.
	Gated []string

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
// The gated three are assigned rather than composed in the literal, because
// each needs the value it is filling in: a gate they answer false at is a
// reading about the DRIVER that belongs beside their own — see
// [Capabilities.Gated].
func probe(ctx context.Context, db *sql.DB) Capabilities {
	caps := Capabilities{
		VectorFunctions: probeVectorFunctions(ctx, db),
		MaxVariables:    probeMaxVariables(ctx, db),
		PageCacheKiB:    probePageCache(ctx, db),
	}
	caps.VectorIndex = probeVectorIndex(ctx, db, &caps)
	caps.FullTextSearch = probeFullText(ctx, db, &caps)
	caps.WithoutRowid = probeWithoutRowid(ctx, db, &caps)
	return caps
}

// gatedCapability pairs what [Capabilities.Gated] reports with the Turso
// experimental feature that would unlock it.
//
// TWO NAMES BECAUSE THEY ARE TWO THINGS, and one gate covers more than one
// capability: `index_method` is the flag in front of the full-text index AND
// the ANN index alike, so a reading that named the flag could not say which of
// them had landed. What is published is therefore the capability — the same
// name as the field it explains — and the flag stays an argument to the probe.
type gatedCapability struct {
	// Name is as Gated reports it, matching the Capabilities field.
	Name string
	// Feature is as the DSN's `experimental=` list takes it, which is the
	// driver's `--experimental-<name>` flag with its dashes turned into
	// underscores.
	//
	// MISSPELLING ONE IS SILENT: the driver hands the list to the engine,
	// which ignores a name it does not know rather than refusing to open.
	// So a probe can never assume its flag took effect — it reads the
	// SECOND refusal, and a gate error surviving the retry is the probe
	// asking wrong rather than a feature that is absent. That is the one
	// case here worth a log.
	Feature string
}

var (
	capFullText     = gatedCapability{Name: "full_text_search", Feature: "index_method"}
	capVectorIndex  = gatedCapability{Name: "vector_index", Feature: "index_method"}
	capWithoutRowid = gatedCapability{Name: "without_rowid", Feature: "without_rowid"}
)

// gateMarker is the fragment every one of Turso's experimental refusals
// carries. Matching the message is not a choice: the refusal is a PARSE error
// with no code, no pragma and no capability flag behind it.
const gateMarker = "experimental feature"

// gateOutcome is what one gated probe found.
//
// A VALUE RATHER THAN A MUTATION, because a capability with ALTERNATIVE
// spellings asks more than once and only the whole loop knows the answer: a
// probe that recorded "gated" as it went marked the capability the moment its
// first spelling turned out to be gated, and a second spelling that then
// worked on the live pool left the reading claiming the capability was usable
// AND unreachable at the same time. The caller decides once, at the end.
type gateOutcome int

// THE ORDER IS LOAD-BEARING: [probeVectorIndex] takes the max across a
// capability's alternative spellings, so worst to best is what makes the best
// answer win whichever spelling produced it. Insert a new outcome at its rank,
// never at the end.
const (
	// gateAbsent — the engine does not implement it, flag or no flag.
	gateAbsent gateOutcome = iota
	// gateBehind — implemented, but served only to a connection that
	// opted into an experimental feature this engine's pool does not.
	gateBehind
	// gateUsable — the live pool accepts it.
	gateUsable
)

// probeGated answers a capability whose refusal may be Turso's experimental
// GATE rather than an absent feature.
//
// Three outcomes, and the middle one is the whole reason this exists:
//
//   - the pool accepts the statements       -> gateUsable.
//   - the pool refuses AT THE GATE, and a
//     connection carrying the flag accepts  -> gateBehind.
//   - anything else                         -> gateAbsent, which is now a
//     measurement rather than an assumption.
func probeGated(ctx context.Context, db *sql.DB,
	capability gatedCapability, stmts []string,
) gateOutcome {
	ok, err := probeReporting(ctx, db, stmts)
	if ok {
		return gateUsable
	}
	if err == nil || !strings.Contains(err.Error(), gateMarker) {
		return gateAbsent
	}
	behind, err := probeBehindGate(ctx, capability.Feature, stmts)
	switch {
	case behind:
		return gateBehind
	case err != nil && strings.Contains(err.Error(), gateMarker):
		// THE ONE MISCONFIGURATION THAT CANNOT BE SEEN ANY OTHER WAY.
		// The flag was set and the gate still refused, so the name this
		// probe passed is not the one the engine knows — and since an
		// unknown name is ignored silently, every capability behind that
		// gate would read false for ever with nothing to say why.
		log.Warn("store_experimental_probe_misconfigured",
			"capability", capability.Name, "feature", capability.Feature,
			"error", err.Error(),
			"detail", "this probe enabled a Turso experimental feature by a "+
				"name the engine does not know, so it is still measuring the "+
				"gate; correct the name in internal/store/caps.go against the "+
				"driver's own --experimental-* flags")
	}
	return gateAbsent
}

// record turns one capability's outcome into its boolean and, where the
// feature exists but this engine cannot reach it, its entry in
// [Capabilities.Gated].
func record(caps *Capabilities, capability gatedCapability, got gateOutcome) bool {
	if got == gateBehind {
		markGated(caps, capability.Name)
	}
	return got == gateUsable
}

// probeBehindGate runs the same statements on a throwaway connection that
// opted into one experimental feature.
//
// IN MEMORY, never the store's own file: the file is exclusively owned by this
// process and a second handle to it is the one thing this package exists to
// prevent — and the question is the PARSER's and the module registry's, which
// have nothing to do with which bytes are on disk. It carries none of the
// pool's session pragmas for the same reason.
//
// The database is discarded with the connection, so there is nothing to roll
// back and no debris to clean up.
//
// 3.4 ms per call, measured, and only on a probe that actually hit the gate —
// at most once per capability, once per [Open] of the node estate, which is
// once per process.
func probeBehindGate(ctx context.Context, feature string, stmts []string) (bool, error) {
	db, err := sql.Open(driverName, ":memory:?experimental="+feature)
	if err != nil {
		return false, err
	}
	defer func() { _ = db.Close() }()
	for _, stmt := range stmts {
		if _, err := db.ExecContext(ctx, stmt); err != nil {
			return false, err
		}
	}
	return true, nil
}

// markGated records a capability as implemented-but-gated, keeping the list
// sorted and free of repeats so a log line and a test can compare against it.
//
// A REPEAT IS ORDINARY rather than a bug: the ANN probe asks twice, once per
// index method, and both arms answer for the same capability.
func markGated(caps *Capabilities, name string) {
	if !slices.Contains(caps.Gated, name) {
		caps.Gated = append(caps.Gated, name)
		slices.Sort(caps.Gated)
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
//
// THROUGH [probeGated], because `USING <method>` is refused at the
// experimental gate before the method name is looked at — so an unflagged
// connection answers identically for a method that exists and one that does
// not, and this measured neither until it asked behind the gate as well.
// Today both answer `unknown module name` there: genuinely absent.
func probeVectorIndex(ctx context.Context, db *sql.DB, caps *Capabilities) bool {
	// THE BEST OUTCOME ACROSS THE SPELLINGS, decided after both have
	// answered. One method being gated says nothing about the capability
	// while another may still be usable on the pool, so the marking
	// happens once, here, on what the whole loop found.
	best := gateAbsent
	for _, method := range []string{"vector", "diskann"} {
		got := probeGated(ctx, db, capVectorIndex, []string{
			`CREATE TABLE crewlet_probe_vec (id TEXT PRIMARY KEY, e F32_BLOB(4))`,
			`CREATE INDEX crewlet_probe_vec_idx ON crewlet_probe_vec USING ` +
				method + ` (e)`,
		})
		if got == gateUsable {
			return record(caps, capVectorIndex, gateUsable)
		}
		best = max(best, got)
	}
	return record(caps, capVectorIndex, best)
}

// probeWithoutRowid asks for the narrower table shape two of this engine's own
// tables would take.
//
// CREATED AND ROLLED BACK, because the refusal is a PARSE error rather than a
// capability flag: there is no pragma to read, and the only honest question is
// whether the statement a migration would carry is one this driver accepts.
//
// THE ONE CAPABILITY THAT IS PRESENT BEHIND THE GATE. Asked on the pool it is
// refused as experimental; asked on a connection carrying
// `experimental=without_rowid` the table is created. So this answers false —
// which is the truth about the statement a migration would carry — and
// [Capabilities.Gated] carries the name, which is the truth about the engine.
func probeWithoutRowid(ctx context.Context, db *sql.DB, caps *Capabilities) bool {
	return record(caps, capWithoutRowid, probeGated(ctx, db, capWithoutRowid, []string{
		`CREATE TABLE crewlet_probe_wr (a TEXT NOT NULL, b TEXT NOT NULL, ` +
			`PRIMARY KEY (a, b)) WITHOUT ROWID`,
	}))
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
// AND IT GOES THROUGH THE GATE, which is what makes the second arm a
// measurement at all. An unflagged connection is refused before the module is
// resolved, so the answer it gives is about the flag and not about fts; behind
// the flag the engine answers `unknown module name 'fts'`, which is the fact
// this records. The arm still exists for the day that changes — see
// [Capabilities.FullTextSearch] for why a true would be a question rather than
// a fix.
func probeFullText(ctx context.Context, db *sql.DB, caps *Capabilities) bool {
	if probeInRollback(ctx, db, []string{
		`CREATE VIRTUAL TABLE crewlet_probe_fts USING fts5(body)`,
	}) {
		return true
	}
	return record(caps, capFullText, probeGated(ctx, db, capFullText, []string{
		`CREATE TABLE crewlet_probe_txt (body TEXT NOT NULL)`,
		`CREATE INDEX crewlet_probe_txt_idx ON crewlet_probe_txt USING fts (body)`,
	}))
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
