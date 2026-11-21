package store

import (
	"context"
	"database/sql"
	"errors"
	"slices"
	"strings"
	"sync"

	turso "turso.tech/database/tursogo"
)

// Capabilities records what the live driver can actually do, measured at Open
// rather than assumed from a version number.
//
// Each answer is a TRIPWIRE: a pin bump that lands a feature turns a skipping
// test into a passing one, and one that loses it fails the build
// (capability_test.go). A REFUSAL IS NOT AN ABSENCE: Turso refuses three of
// these at an experimental FLAG, so Gated is the second, flagged answer.
//
// A probe never fails Open: a regression reads as a vanished capability, not
// an engine that will not start. Recall's prefetches then carry on with an
// empty block and `query_episodes` returns a tool error.
type Capabilities struct {
	// VectorFunctions is vector32() and vector_distance_cos(): similarity
	// computed by the database. It is what recall's ORDER BY is written
	// against — the distance arithmetic runs in the database and only the
	// rows that survive the limit cross the driver boundary.
	//
	// TRUE on the pinned driver, and a requirement: a false means this
	// build's recall returns nothing, not that recall takes another path.
	VectorFunctions bool

	// VectorIndex is an approximate-nearest-neighbour index over a vector
	// column. FALSE on the pinned driver, so recall is a SCAN behind the
	// per-agent index: the arithmetic is the database's, but the row set is
	// still every embedded row for one agent. That is correct at the real
	// workload (thousands of rows, always filtered by agent first) and is
	// not "native vector search".
	//
	// ABSENT RATHER THAN GATED: `USING vector` and `USING diskann` both
	// answer `unknown module name` on a connection that opted into
	// `index_method`, so there is nothing behind the gate to opt into. See
	// Gated.
	VectorIndex bool

	// FullTextSearch is a queryable full-text index — Turso's own `USING
	// fts` index method, or an fts5 virtual table. FALSE on the pinned
	// driver, and ABSENT rather than gated: fts5 is not a registered
	// module, and `USING fts` answers `unknown module name 'fts'` on a
	// connection that opted into `index_method` (measured — see Gated).
	//
	// Nothing depends on it: the engine's lexical search is its OWN (the
	// analyzer in internal/textindex and the inverted list internal/search
	// maintains). A true here would open a decision about replacing that
	// index with the database's, not fix anything, which is why it is
	// measured.
	FullTextSearch bool

	// WithoutRowid is whether a `WITHOUT ROWID` table is reachable on the
	// connections THIS ENGINE OPENS.
	//
	// FALSE on the pinned driver while `Gated` carries "without_rowid": the
	// engine implements it only for `?experimental=without_rowid`, and this
	// pool opts into nothing. `kb_vectors_bin` and
	// `tracker_log_deferred_scope` pay for a rowid they never use.
	//
	// A gated true is NOT a licence to write one: opting the DSN in enables
	// an experimental feature for every statement, and adopting it is a new
	// numbered migration that rebuilds the tables. A migration using
	// `WITHOUT ROWID` today would make every store refuse to open.
	WithoutRowid bool

	// Gated names the capabilities this build of the database engine
	// IMPLEMENTS but serves only to a connection that opted into an
	// experimental feature, which this pool does not.
	//
	// A CAPABILITY'S NAME, not the flag's, since one gate fronts several
	// fields; [gatedCapability] holds the flag. The refusal is identical
	// whether a feature exists behind the gate or not, so a gated probe asks
	// TWICE: on the live pool (the fields) and on a throwaway in-memory
	// connection carrying the flag (this). A name appearing on a pin bump is
	// the day to decide whether to opt in.
	Gated []string

	// MaxVariables is how many bound parameters one statement accepts,
	// measured. It is the chunk size (rows ÷ columns) of every multi-row
	// INSERT the appliers write.
	//
	// PROBED, NOT PICKED: the limit is the ENGINE's, not the dialect's. A
	// hardcoded 32 766 is refused by a smaller-limit engine when a batch is
	// largest; 999 wastes round trips. The pinned driver has no limit the
	// probe can reach, so it reports the probe's 32 766 ceiling and
	// [RowsPerInsert]'s row cap bounds a statement instead. The probe must not
	// select a column per parameter ("too many columns" at 2 001).
	//
	// 999 when the probe cannot tell: too large is an unretryable failure.
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

// probeDriver measures the DRIVER's capabilities — every field but
// PageCacheKiB — against a live pool, and reports the first question the
// driver did not answer, or nil when it answered every one.
//
// Each probe runs in its own rolled-back transaction: the driver does not
// document reusing one after an error. A probe never fails Open; the error
// return says whether a conservative answer was a real one, which only
// [driverMeasurement] needs. An ended context needs no check: [answered]
// admits only the parser's refusal, and the context's own error is not one.
// The gated three are assigned after the literal because each needs the value
// it fills in.
func probeDriver(ctx context.Context, db *sql.DB) (Capabilities, error) {
	p := &prober{ctx: ctx, db: db}
	caps := Capabilities{
		VectorFunctions: p.vectorFunctions(),
		MaxVariables:    p.maxVariables(),
	}
	caps.VectorIndex = p.vectorIndex(&caps)
	caps.FullTextSearch = p.fullText(&caps)
	caps.WithoutRowid = p.withoutRowid(&caps)
	return caps, p.unanswered
}

// prober asks one pool the probe's questions and keeps the first one the
// driver did not answer.
type prober struct {
	ctx context.Context
	db  *sql.DB

	// unanswered is the first failure that was not the driver's own
	// answer — see [answered].
	unanswered error
}

// heard records one question's outcome and hands err back, so a probe can
// branch on it exactly as before.
func (p *prober) heard(err error) error {
	if err != nil && p.unanswered == nil && !answered(err) {
		p.unanswered = err
	}
	return err
}

// parseRefusal is the fragment every refusal a probe here is ASKING for
// carries, measured on the pinned driver: "no such function", "no such
// module", and the experimental gate's "… is an experimental feature" and
// "unknown module name" behind it all arrive as `turso: error: Parse error:
// …`. Matching the message is not a choice: the driver gives every one of
// them the same generic status.
const parseRefusal = "Parse error"

// answered reports whether err is the DRIVER ANSWERING a probe's question —
// its parser refusing the statement, which is a fact about the library — as
// opposed to the question going unanswered: a busy file (`database is
// locked`, the driver's busy status), a context that ended, a connection that
// could not be had. Measured, the first kind is always the driver's generic
// status carrying a parse error, and the second never is.
func answered(err error) bool {
	return errors.Is(err, turso.ErrTursoGeneric) && strings.Contains(err.Error(), parseRefusal)
}

// measuredDriver is the driver's half of [Capabilities] as this process
// measured it — see [driverMeasurement] for what it holds and when.
var measuredDriver driverMeasurement

// driverMeasurement is the driver's half of [Capabilities] as the first probe
// answered IN FULL found it, or nothing while none has.
//
// A TYPE so a test can hold its own empty one. Once per process because every
// answer is about the compiled-in library (only the page cache is about the
// file) and a probe cost a median 56-128 ms per open.
//
// Only a probe answered in full is kept: a busy file or ended context yields
// the same conservative answer as a refusal, and kept it would shrink every
// applier's statements for the process's life. Otherwise it is this handle's
// alone ([answered]). A pool on a [Options.WrapDriver] driver probes for itself
// and neither reads nor writes the measurement.
type driverMeasurement struct {
	mu   sync.Mutex
	caps *Capabilities
}

// capabilities is the driver's half of [Capabilities] for a pool: the
// measurement when there is one and the pool is on the unwrapped driver, and
// otherwise the pool's own probe — which becomes the measurement when the pool
// is unwrapped and the driver answered all of it. The error is a probe's
// unanswered question, as [probeDriver] reports it, and is always nil on an
// answer taken from the measurement.
//
// THE PROBE RUNS OUTSIDE THE LOCK. Opens racing to be the first each probe for
// themselves and the first full answer is kept. Held across a probe that can
// wait out a busy file, a process-wide lock would be a line every open in the
// process queues in for a question that needs none — the mistake the
// migration lock made until it was scoped to its file.
func (m *driverMeasurement) capabilities(ctx context.Context, db *sql.DB,
	wrapped bool) (Capabilities, error) {
	if wrapped {
		return probeDriver(ctx, db)
	}
	m.mu.Lock()
	known := m.caps
	m.mu.Unlock()
	if known != nil {
		return known.clone(), nil
	}
	caps, err := probeDriver(ctx, db)
	if err != nil {
		return caps, err
	}
	m.mu.Lock()
	if m.caps == nil {
		kept := caps.clone()
		m.caps = &kept
	}
	m.mu.Unlock()
	return caps, nil
}

// clone is c with a Gated slice of its own, so no handle's answer shares a
// backing array with another's or with the process's measurement.
func (c Capabilities) clone() Capabilities {
	c.Gated = slices.Clone(c.Gated)
	return c
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

// gated answers a capability whose refusal may be Turso's experimental GATE
// rather than an absent feature.
//
// Three outcomes, and the middle one is the whole reason this exists:
//
//   - the pool accepts the statements       -> gateUsable.
//   - the pool refuses AT THE GATE, and a
//     connection carrying the flag accepts  -> gateBehind.
//   - anything else                         -> gateAbsent, which is now a
//     measurement rather than an assumption.
func (p *prober) gated(capability gatedCapability, stmts []string) gateOutcome {
	ok, err := p.inRollback(stmts)
	if ok {
		return gateUsable
	}
	if err == nil || !strings.Contains(err.Error(), gateMarker) {
		return gateAbsent
	}
	behind, err := p.behindGate(capability.Feature, stmts)
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

// behindGate runs the same statements on a throwaway connection that opted
// into one experimental feature.
//
// IN MEMORY, never the store's own file: that file is exclusively owned by
// this process and a second handle to it is what this package exists to
// prevent. The question is the parser's and module registry's, not the disk's,
// so it carries none of the pool's pragmas either. Nothing to roll back.
//
// 3.4 ms per call, only on a probe that hit the gate, once per process
// ([driverMeasurement]).
func (p *prober) behindGate(feature string, stmts []string) (bool, error) {
	db, err := sql.Open(driverName, ":memory:?experimental="+feature)
	if err != nil {
		return false, p.heard(err)
	}
	defer func() { _ = db.Close() }()
	for _, stmt := range stmts {
		if _, err := db.ExecContext(p.ctx, stmt); err != nil {
			return false, p.heard(err)
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

// maxVariables finds the largest parameter count one statement accepts,
// by BINARY SEARCH over prepares.
//
// A prepare, not an execution: the limit is a parser bound, so a statement
// that prepares would run, with no table or transaction. [selectParams] binds
// N parameters and returns ONE column, so the parameter limit is the only
// bound it can meet (see [Capabilities.MaxVariables]).
//
// Bounded above by 32 766, the largest value any engine in this family
// reports, so it is at most fifteen prepares on a driver with no limit.
func (p *prober) maxVariables() int {
	const ceiling = 32766
	accepts := func(n int) bool {
		stmt, err := p.db.PrepareContext(p.ctx, selectParams(n))
		if p.heard(err) != nil {
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

// selectParams builds `SELECT 1 WHERE 1 IN (?, ?, …)` with n placeholders:
// n parameters, one result column, and no table, so the only limit a prepare
// of it can meet is the parameter limit.
func selectParams(n int) string {
	const head, tail = "SELECT 1 WHERE 1 IN (", ")"
	var b strings.Builder
	b.Grow(len(head) + 2*n + len(tail))
	b.WriteString(head)
	for i := range n {
		if i > 0 {
			b.WriteString(",")
		}
		b.WriteString("?")
	}
	b.WriteString(tail)
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

// vectorFunctions asks for a distance between two literal vectors. It needs
// no table, so a bare query is the whole probe.
func (p *prober) vectorFunctions() bool {
	var d float64
	err := p.db.QueryRowContext(p.ctx,
		`SELECT vector_distance_cos(vector32('[1,0,0,0]'), vector32('[0,1,0,0]'))`,
	).Scan(&d)
	return p.heard(err) == nil
}

// vectorIndex tries to build an ANN index: the index METHOD is the part
// Turso's parser rejects, and the column type and distance functions prove
// nothing about it.
//
// TURSO'S GRAMMAR, not libSQL's: `libsql_vector_idx(e)` is a parse error here
// whatever the driver ships, a tripwire that cannot fire. Both spellings Turso
// could plausibly land are tried.
//
// THROUGH [prober.gated], because `USING <method>` is refused at the gate
// before the method name is looked at. Behind it both answer `unknown module
// name`: genuinely absent.
func (p *prober) vectorIndex(caps *Capabilities) bool {
	// THE BEST OUTCOME ACROSS THE SPELLINGS, decided after both have
	// answered. One method being gated says nothing about the capability
	// while another may still be usable on the pool, so the marking
	// happens once, here, on what the whole loop found.
	best := gateAbsent
	for _, method := range []string{"vector", "diskann"} {
		got := p.gated(capVectorIndex, []string{
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

// withoutRowid asks for the narrower table shape two of this engine's own
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
func (p *prober) withoutRowid(caps *Capabilities) bool {
	return record(caps, capWithoutRowid, p.gated(capWithoutRowid, []string{
		`CREATE TABLE crewlet_probe_wr (a TEXT NOT NULL, b TEXT NOT NULL, ` +
			`PRIMARY KEY (a, b)) WITHOUT ROWID`,
	}))
}

// fullText accepts either mechanism: the capability is "a full-text index
// exists", not "this exact syntax parses". The fts5 arm is the shape a
// SQLite-compatible engine would most plausibly land.
//
// THE SECOND ARM IS TURSO'S OWN GRAMMAR, `CREATE INDEX … USING fts (col)`,
// behind the experimental `index_method` feature. It goes THROUGH THE GATE:
// unflagged, the refusal is about the flag, not fts; behind it the engine
// answers `unknown module name 'fts'`, the fact this records. See
// [Capabilities.FullTextSearch] for why a true would be a question, not a fix.
func (p *prober) fullText(caps *Capabilities) bool {
	if ok, _ := p.inRollback([]string{
		`CREATE VIRTUAL TABLE crewlet_probe_fts USING fts5(body)`,
	}); ok {
		return true
	}
	return record(caps, capFullText, p.gated(capFullText, []string{
		`CREATE TABLE crewlet_probe_txt (body TEXT NOT NULL)`,
		`CREATE INDEX crewlet_probe_txt_idx ON crewlet_probe_txt USING fts (body)`,
	}))
}

// inRollback runs statements in a transaction that is always rolled back,
// reporting whether every one of them succeeded and, when one did not, why —
// for a probe whose ERROR distinguishes "the feature is absent" from "this
// probe asked wrong".
func (p *prober) inRollback(stmts []string) (bool, error) {
	tx, err := p.db.BeginTx(p.ctx, nil)
	if err != nil {
		return false, p.heard(err)
	}
	defer func() { _ = tx.Rollback() }()
	for _, stmt := range stmts {
		if _, err := tx.ExecContext(p.ctx, stmt); err != nil {
			return false, p.heard(err)
		}
	}
	return true, nil
}
