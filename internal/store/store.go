// Package store is the engine's local materialized-index database — the
// audit event log, the learning subsystem's memory, and the durable runtime
// state a turn leaves behind.
//
// # One file, one process
//
// The engine owns its database file EXCLUSIVELY. A second binary pointed at
// the same path is not a degraded configuration, it is corruption waiting for
// a schedule to collide — and the driver says so only sometimes, which is
// worse than never. Measured: Turso refuses a second opener, but as an opaque
// connect error ("File is locked by another process") that names no holder,
// and only while the first process still has live connections. A peer that
// opens the file in the window between the engine's connections finds nothing
// in its way at all.
//
// [Open] therefore takes an advisory OS lock for the life of the handle and
// answers a second PROCESS with [ErrLocked] — before any driver work, and
// naming the pid that holds the file. See lock.go for why an OS lock rather
// than a pid file, and why two handles inside one process share the claim
// instead. That is what makes the rule above true rather than merely stated:
// the secret-store CLIs open this database from a second process as their
// documented gesture, and before the lock the only defence was this comment.
//
// Everything that genuinely needs cross-process coordination — seat leases,
// config activations, the completion ledger, dedupe and rate valves — lives in
// the KV layer instead, and that separation is why nothing
// here has to be safe against a peer.
//
// It also collapses a whole idiom. The Postgres migrator took an advisory lock
// because `crewlet run`, `crewlet run api` and `crewlet config import` could
// each race the DDL from a different OS process. One process means one
// in-process mutex, and the lock protocol simply disappears.
//
// # One driver
//
// Turso (turso.tech/database/tursogo) is the database, and it is the only
// driver. There was a second — modernc.org/sqlite, kept as a certified
// fallback so that every statement here had to parse on both — and it is
// gone. The short version: the fallback never ran anything but its own test
// job, the two drivers are not substitutable for a database
// with rows in it (only Turso has the vector functions the learning
// subsystem's recall needs), and writing in the intersection of two dialects
// cost the engine every Turso-only feature it is on Turso for.
//
// What that buys is spent immediately and deliberately: recall's distance
// arithmetic now runs in the database (see internal/learning), because
// vector_distance_cos is present on the one driver rather than probable on
// two. [Capabilities] still measures what the pinned driver can do, and is
// still the tripwire for the two features Turso announces and does not yet
// reach Go — an ANN vector index and a full-text index.
package store

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"fmt"
	"time"

	"github.com/crewlet/crewlet/internal/logging"

	// The driver registers itself under the name [driverName]. A blank
	// import because nothing here touches its types — the whole point of
	// the database/sql seam is that the engine talks to an interface.
	_ "turso.tech/database/tursogo"
)

// driverName is what the Turso driver registers itself as with database/sql.
//
// Unexported, and there is no longer a knob that selects it: a store.driver
// config field and a CREWLET_STORE_DRIVER environment variable both chose
// between two implementations, and there is one: only Turso has the vector
// distance functions the agent-learning recall path reads through, so the
// second driver kept every table and silently lost recall. See internal/config
// for the retired-key message a file that still sets it gets.
const driverName = "turso"

// Defaults for Options. Both are anchored to the dashboard, which is the only
// component that reads this store concurrently with the engine writing it.
const (
	// The dashboard's query channel admits 4 concurrent queries, so 4
	// connections is what keeps a read burst off
	// the write path. More would not help: under WAL, readers never block
	// the writer, but writers serialise on the file lock regardless, so
	// connections past the read concurrency only deepen a queue.
	defaultMaxOpenConns = 4

	// Half the dashboard's 10 s query timeout. A busy wait longer than the
	// timeout above it turns lock contention into a request that fails with
	// no error to show; half leaves the blocked writer room to finish and
	// report what happened.
	defaultBusyTimeout = 5 * time.Second
)

// Options configures Open. The zero value is valid.
type Options struct {
	// WrapDriver wraps the driver before any connection is opened. It
	// exists for FAULT INJECTION and nothing else.
	//
	// Every fail-open read in this codebase has a branch that only runs
	// when a result set fails PART WAY THROUGH — after the query
	// succeeded, during iteration. That branch decides whether a caller
	// gets "nothing is known" or a silent PARTIAL answer, which is the
	// dangerous one, and no amount of closing the database reaches it:
	// closing makes the query itself fail, which is the other branch.
	//
	// It wraps rather than replaces, so what runs underneath is still the
	// real driver against a real file. Nil in every non-test caller, and
	// there is no config field for it.
	WrapDriver func(driver.Driver) driver.Driver

	// MaxOpenConns bounds the connection pool; 0 means defaultMaxOpenConns.
	MaxOpenConns int

	// BusyTimeout is how long a statement waits for the file lock before
	// giving up; 0 means defaultBusyTimeout.
	BusyTimeout time.Duration

	// EmbeddingDim is the width of the vectors the active company config's
	// embedding model produces. It is a RUNTIME property, deliberately: the
	// Postgres schema templated it into `vector(N)` DDL, which forced the
	// migrator to run in two phases — bootstrap enough tables to read the
	// config, learn the width, then migrate the rest. Vector columns here
	// are plain BLOBs and this is the only thing that knows how wide they
	// are. 0 means no embedding model is configured.
	EmbeddingDim int
}

// maxOpenConns is the pool bound with the default applied. A method rather
// than a branch at each call site: [Open] and [Pending] both build a pool, and
// the second one skipping a bound the first applies is exactly the drift that
// made Pending a different database connection from the engine's.
func (o Options) maxOpenConns() int {
	if o.MaxOpenConns <= 0 {
		return defaultMaxOpenConns
	}
	return o.MaxOpenConns
}

// busyTimeout is the lock wait with the default applied. See maxOpenConns.
func (o Options) busyTimeout() time.Duration {
	if o.BusyTimeout <= 0 {
		return defaultBusyTimeout
	}
	return o.BusyTimeout
}

// DB is an open handle on the local store: a connection pool, the schema it
// has applied, and the capability answers probed against the live driver.
type DB struct {
	sql  *sql.DB
	path string
	caps Capabilities
	dim  int

	// lock is this process's exclusive claim on path, held for the life of
	// the handle and released by Close — or by the kernel, if this process
	// does not get to run Close. Nil for an in-memory database, which has
	// no file to exclude anyone from. See lock.go.
	lock *fileLock
}

var log = logging.Get("store")

// Open opens (creating if absent) the database at path, applies any pending
// schema, and probes the driver's capabilities.
//
// path is a filesystem path, and this process takes an EXCLUSIVE lock on it
// for the life of the handle — see the package doc for why, and lock.go for
// how. A second crewlet process opening the same path gets [ErrLocked] rather
// than a database the two of them corrupt between them.
func Open(ctx context.Context, path string, opts Options) (*DB, error) {
	// THE LOCK FIRST, before the native library and before the pool: both
	// of those touch shared state on the way up, and taking them for a
	// database this process turns out not to own is work done against a
	// file somebody else is writing.
	lock, err := lockStore(path)
	if err != nil {
		return nil, err
	}
	defer func() {
		// Released on every failure below. A handle that never reached a
		// caller has no Close coming, and the lock would outlive the
		// attempt for the life of the process.
		if err != nil {
			lock.release()
		}
	}()

	pool, err := openPrepared(ctx, path, opts)
	if err != nil {
		return nil, err
	}

	db := &DB{sql: pool, path: path, dim: opts.EmbeddingDim, lock: lock}
	applied, err := db.migrate(ctx)
	if err != nil {
		_ = pool.Close()
		return nil, err
	}
	db.caps = probe(ctx, pool)

	log.InfoContext(ctx, "store_opened",
		"path", path,
		"engine_version", engineVersion(ctx, pool),
		"migrations_applied", len(applied),
		"vector_functions", db.caps.VectorFunctions,
		"vector_index", db.caps.VectorIndex,
		"full_text_search", db.caps.FullTextSearch,
	)
	return db, nil
}

// openPrepared readies the native library and returns a live pool with this
// package's bounds and session state applied.
//
// ONE PATH TO A CONNECTION, and that is the whole reason it exists. [Open] and
// [Pending] both need a pool, and Pending used to build its own: it resolved
// the driver, called openPool and pinged — but never prepared the native
// library, so the first connection `crewlet migrate` made went straight into
// the driver's own loader, whose answer to a half-written cache is a PANIC
// inside a sync.Once (see turso.go). The command that exists to report the
// schema safely was the one command that could take the process down on it.
// It also silently ran on an unbounded pool. Neither was a decision; both were
// a second code path drifting from the first.
//
// It does NOT lock. The claim on the file belongs to the caller, because the
// two callers want opposite things from it: Open holds it for the life of the
// handle, and Pending takes it only across its read.
func openPrepared(ctx context.Context, path string, opts Options) (*sql.DB, error) {
	// BEFORE THE POOL, because the driver loads its native library on the
	// first connection and PANICS if the shared cache it loads from is
	// half-written. Preparing it here turns a process that dies on its
	// first query into an error a caller can read, and stops two engines
	// starting at once from corrupting that cache at all.
	if err := prepareTursoLibrary(); err != nil {
		return nil, err
	}
	pool, err := openPool(path, opts.busyTimeout(), opts.WrapDriver)
	if err != nil {
		return nil, err
	}
	pool.SetMaxOpenConns(opts.maxOpenConns())
	// Idle capacity matches open capacity: these are file handles on local
	// storage, not sockets to a remote server, so retiring one buys nothing
	// and paying to re-establish it (plus its session pragmas) on the next
	// query costs real latency on the read path.
	pool.SetMaxIdleConns(opts.maxOpenConns())

	if err := pool.PingContext(ctx); err != nil {
		_ = pool.Close()
		return nil, fmt.Errorf("store: open %s: %w", path, err)
	}
	return pool, nil
}

// engineVersion is what the database engine calls itself, for the one log line
// an operator reads when a store behaves unlike it did before a driver bump.
// Worth having because the driver is pre-1.0 and pinned, so "what changed" is
// a real support question with no other answer inside the process.
//
// TWO NUMBERS, because there are two and they disagree. Measured at
// tursogo v0.8.0-pre.7: turso_version() answers "3.47.0" and sqlite_version()
// answers "3.50.4". Neither is the driver's own module version, and this file
// deliberately does not claim to know which of them is the engine's release
// and which is a compatibility level — it reports what was asked rather than
// an interpretation that could be wrong in a log line nobody can re-check.
// The identifier that is unambiguous is the pin in go.mod.
//
// Best effort: a driver that stopped answering either query must not fail an
// open that has already succeeded.
func engineVersion(ctx context.Context, pool *sql.DB) string {
	ask := func(fn string) string {
		var v string
		if err := pool.QueryRowContext(ctx, `SELECT `+fn+`()`).Scan(&v); err != nil {
			return "unknown"
		}
		return v
	}
	return fmt.Sprintf("turso=%s sqlite=%s", ask("turso_version"), ask("sqlite_version"))
}

// Close releases the pool and this process's claim on the file.
//
// THE LOCK LAST, after the pool: a peer that saw the lock free while this
// process still had connections open would be the two-writer case the lock
// exists to prevent, in the one window where it looked safe.
func (d *DB) Close() error {
	if d == nil || d.sql == nil {
		return nil
	}
	err := d.sql.Close()
	d.lock.release()
	return err
}

// Caps reports what the live driver can do. Probed once at Open — the answers
// are a property of the compiled-in driver version, so nothing re-measures
// them per query.
func (d *DB) Caps() Capabilities { return d.caps }

// Path reports the file this handle owns.
func (d *DB) Path() string { return d.path }

// EmbeddingDim reports the configured vector width, or 0 when no embedding
// model is configured. See Options.EmbeddingDim for why this is not in the
// schema.
func (d *DB) EmbeddingDim() int { return d.dim }

// SQL exposes the pooled handle for store implementations built on this
// database. Application code goes through a typed store instead — a caller
// that reaches for raw SQL is writing a query nobody can find later.
func (d *DB) SQL() *sql.DB { return d.sql }

// openPool builds a pool whose every connection has the session state this
// package depends on already applied.
//
// The session state cannot be set once on the pool: database/sql opens
// connections lazily and replaces them freely, so a PRAGMA issued through the
// pool lands on whichever connection answered and no other. It cannot be
// passed in the DSN either — Turso's DSN parser takes a path and a small set
// of its own options, and none of them is a pragma. A connector wrapping the
// driver is the one place that runs on every connection, identically.
func openPool(path string, busy time.Duration,
	wrap func(driver.Driver) driver.Driver,
) (*sql.DB, error) {
	// sql.Open is lazy — it validates the driver name and nothing else — so
	// this costs no I/O and exists only to reach the registered driver
	// value, which database/sql offers no other accessor for.
	probeHandle, err := sql.Open(driverName, path)
	if err != nil {
		return nil, fmt.Errorf("store: driver %q: %w", driverName, err)
	}
	drv := probeHandle.Driver()
	_ = probeHandle.Close()
	if wrap != nil {
		drv = wrap(drv)
	}

	return sql.OpenDB(&connector{
		drv: drv,
		dsn: path,
		session: []string{
			// WAL, so a dashboard read never blocks the engine's write
			// and vice versa.
			"PRAGMA journal_mode = WAL",
			// SQLite defaults foreign keys OFF, which makes a declared
			// constraint look enforced right up until the day it
			// matters. synthesized_skill_versions declares one so that
			// deleting a skill cascades its history rather than
			// orphaning it; this is what makes the declaration true.
			"PRAGMA foreign_keys = ON",
			fmt.Sprintf("PRAGMA busy_timeout = %d", busy.Milliseconds()),
		},
	}), nil
}

// connector opens driver connections and applies session state to each one.
type connector struct {
	drv     driver.Driver
	dsn     string
	session []string
}

func (c *connector) Connect(ctx context.Context) (driver.Conn, error) {
	conn, err := c.drv.Open(c.dsn)
	if err != nil {
		return nil, fmt.Errorf("store: connect %s: %w", c.dsn, err)
	}
	exec, ok := conn.(driver.ExecerContext)
	if !ok {
		_ = conn.Close()
		return nil, fmt.Errorf("store: driver %T cannot execute session pragmas", c.drv)
	}
	for _, stmt := range c.session {
		if _, err := exec.ExecContext(ctx, stmt, nil); err != nil {
			_ = conn.Close()
			return nil, fmt.Errorf("store: session %q: %w", stmt, err)
		}
	}
	return conn, nil
}

func (c *connector) Driver() driver.Driver { return c.drv }

// Tx runs fn inside a transaction, committing when it returns nil and rolling
// back otherwise.
//
// A PANIC rolls back and re-panics rather than leaving the transaction open.
// Without that, a panic in fn returns through the runtime with the connection
// still holding an uncommitted transaction — and on a single-writer database
// that connection going back to the pool with an open transaction blocks every
// subsequent write, so one bug in one handler wedges the whole process.
//
// The rollback error is deliberately discarded on the failure paths: fn's error
// is what the caller needs, and replacing it with "rollback failed" would hide
// the reason the rollback was necessary.
func (d *DB) Tx(ctx context.Context, fn func(*sql.Tx) error) (err error) {
	tx, err := d.sql.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("store: begin: %w", err)
	}
	defer func() {
		if p := recover(); p != nil {
			_ = tx.Rollback()
			panic(p)
		}
	}()
	if err := fn(tx); err != nil {
		_ = tx.Rollback()
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("store: commit: %w", err)
	}
	return nil
}
