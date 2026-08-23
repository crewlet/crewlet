// Package store is the engine's local materialized-index database — the
// audit event log, the learning subsystem's memory, and the durable runtime
// state a turn leaves behind.
//
// # One file, one process
//
// The engine owns its database file EXCLUSIVELY. Turso does not support
// multi-process access to a file, so a second binary pointed at the same path
// is not a degraded configuration, it is corruption waiting for a schedule to
// collide. Everything that genuinely needs cross-process coordination — seat
// leases, config activations, the completion ledger, dedupe and rate valves —
// lives in the KV layer instead (REWRITE_PLAN D8, rewrite/decisions/201), and
// that separation is why nothing here has to be safe against a peer.
//
// It also collapses a whole idiom. The Postgres migrator took an advisory lock
// because `crewlet run`, `crewlet run api` and `crewlet config import` could
// each race the DDL from a different OS process. One process means one
// in-process mutex, and the lock protocol simply disappears.
//
// # Two drivers, one dialect
//
// Two drivers are certified and selected by CREWLET_STORE_DRIVER: "turso"
// (the default) and "sqlite" (modernc.org/sqlite — pure Go, no cgo). Every
// statement in this package must parse on BOTH, because Turso's dialect is the
// narrower of the two today and the dual-driver test job is the only thing
// that catches a divergence. See rewrite/decisions/002 for what the spike
// measured and Capabilities for what the probe re-measures at open.
package store

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/crewlet/crewlet/internal/logging"

	// Both drivers register themselves under the names Driver selects
	// between. They are blank imports because nothing here touches their
	// types — the whole point of the database/sql seam is that swapping one
	// for the other changes a string.
	_ "modernc.org/sqlite"
	_ "turso.tech/database/tursogo"
)

// Driver names one of the two certified database drivers.
type Driver string

const (
	// DriverTurso is turso.tech/database/tursogo, the default.
	DriverTurso Driver = "turso"
	// DriverSQLite is modernc.org/sqlite: mainline SQLite compiled to pure
	// Go. It is the certified fallback, and it is what proves a statement
	// is written in the dialect intersection rather than in Turso's.
	DriverSQLite Driver = "sqlite"
)

// DriverEnv selects the driver when Options leaves it unset.
const DriverEnv = "CREWLET_STORE_DRIVER"

// ErrUnknownDriver is returned when a driver name matches neither certified
// driver. It is an error rather than a fallback: a mistyped log level may
// safely resolve to info, but a mistyped driver name silently opening a
// different storage engine is a data-loss shape, not a cosmetic one.
var ErrUnknownDriver = errors.New("store: unknown driver")

// Defaults for Options. Both are anchored to the dashboard, which is the only
// component that reads this store concurrently with the engine writing it.
const (
	// The dashboard's query channel admits 4 concurrent queries
	// (REWRITE_PLAN §14), so 4 connections is what keeps a read burst off
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

// Options configures Open. The zero value is valid and selects the driver from
// the environment.
type Options struct {
	// Driver overrides CREWLET_STORE_DRIVER. Empty consults the
	// environment, and an unset environment means DriverTurso.
	Driver Driver

	// WrapDriver wraps the certified driver before any connection is
	// opened. It exists for FAULT INJECTION and nothing else.
	//
	// Every fail-open read in this codebase has a branch that only runs
	// when a result set fails PART WAY THROUGH — after the query
	// succeeded, during iteration. That branch decides whether a caller
	// gets "nothing is known" or a silent PARTIAL answer, which is the
	// dangerous one, and no amount of closing the database reaches it:
	// closing makes the query itself fail, which is the other branch.
	//
	// It wraps rather than replaces, so the two-certified-drivers rule
	// holds: what runs underneath is still turso or sqlite. Nil in every
	// non-test caller, and there is no config field for it.
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

// DB is an open handle on the local store: a connection pool, the schema it
// has applied, and the capability answers probed against the live driver.
type DB struct {
	sql  *sql.DB
	drv  Driver
	path string
	caps Capabilities
	dim  int
}

var log = logging.Get("store")

// Open opens (creating if absent) the database at path, applies any pending
// schema, and probes the driver's capabilities.
//
// path is a filesystem path. The caller owns it exclusively for the life of
// the process — see the package doc.
func Open(ctx context.Context, path string, opts Options) (*DB, error) {
	drv, err := resolveDriver(opts.Driver)
	if err != nil {
		return nil, err
	}
	maxConns := opts.MaxOpenConns
	if maxConns <= 0 {
		maxConns = defaultMaxOpenConns
	}
	busy := opts.BusyTimeout
	if busy <= 0 {
		busy = defaultBusyTimeout
	}

	pool, err := openPool(string(drv), path, busy, opts.WrapDriver)
	if err != nil {
		return nil, err
	}
	pool.SetMaxOpenConns(maxConns)
	// Idle capacity matches open capacity: these are file handles on local
	// storage, not sockets to a remote server, so retiring one buys nothing
	// and paying to re-establish it (plus its session pragmas) on the next
	// query costs real latency on the read path.
	pool.SetMaxIdleConns(maxConns)

	if err := pool.PingContext(ctx); err != nil {
		_ = pool.Close()
		return nil, fmt.Errorf("store: open %s (%s): %w", path, drv, err)
	}

	db := &DB{sql: pool, drv: drv, path: path, dim: opts.EmbeddingDim}
	applied, err := db.migrate(ctx)
	if err != nil {
		_ = pool.Close()
		return nil, err
	}
	db.caps = probe(ctx, pool)

	log.InfoContext(ctx, "store_opened",
		"driver", string(drv), "path", path,
		"migrations_applied", len(applied),
		"vector_functions", db.caps.VectorFunctions,
		"vector_index", db.caps.VectorIndex,
		"full_text_search", db.caps.FullTextSearch,
	)
	return db, nil
}

// resolveDriver picks the driver from the explicit option, then the
// environment, then the default.
func resolveDriver(want Driver) (Driver, error) {
	name := string(want)
	if name == "" {
		name = os.Getenv(DriverEnv)
	}
	switch Driver(name) {
	case "":
		return DriverTurso, nil
	case DriverTurso:
		return DriverTurso, nil
	case DriverSQLite:
		return DriverSQLite, nil
	default:
		return "", fmt.Errorf("%w %q (want %q or %q)",
			ErrUnknownDriver, name, DriverTurso, DriverSQLite)
	}
}

// Close releases the pool.
func (d *DB) Close() error {
	if d == nil || d.sql == nil {
		return nil
	}
	return d.sql.Close()
}

// Caps reports what the live driver can do. Probed once at Open — the answers
// are a property of the compiled-in driver version, so nothing re-measures
// them per query.
func (d *DB) Caps() Capabilities { return d.caps }

// Driver reports which certified driver is serving this handle.
func (d *DB) Driver() Driver { return d.drv }

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
// passed in the DSN either — modernc.org/sqlite accepts `?_pragma=…` and Turso
// does not, and a per-driver DSN dialect is exactly the divergence this
// package exists to avoid. A connector wrapping the driver is the one place
// that runs on every connection, on both drivers, identically.
func openPool(driverName, path string, busy time.Duration,
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
			// and vice versa. Without it modernc.org/sqlite returns
			// SQLITE_BUSY under any concurrent write at all (measured:
			// 70 failures out of 80 writes across 4 connections).
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
