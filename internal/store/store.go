// Package store is the engine's local materialized-index database — the
// audit event log, the learning subsystem's memory, and the durable runtime
// state a turn leaves behind.
//
// # Two files, one process
//
// A node keeps TWO databases, and [Open] brings both up: the NODE estate at
// the path it is given, and the REPLICATED estate beside it. The boundary is
// [Estate], and what rests on it is written there — a snapshot is a copy of
// one file rather than a copy of everything with the other's pages deleted
// out of it, the identity claim a peer verifies is a checksum with nothing in
// the way, and an applier's write cadence is its own rather than shared with
// every audit insert.
//
// NO TRANSACTION SPANS THE TWO and no read joins across them, which is a rule
// a static walk enforces rather than a convention: a transaction is one file.
//
// Everything below is true of each of them separately.
//
// # One file, one process
//
// The engine owns each database file EXCLUSIVELY. A second binary pointed at
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
	"errors"
	"fmt"
	"math/rand/v2"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
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
	//
	// READERS ONLY. Every pinned writer ([DB.Writer]) holds a connection of
	// its own for its lifetime and counts against the same bound, so the
	// pool is this plus [Options.PinnedWriters] rather than a constant —
	// see maxOpenConns. A constant here was outgrown the moment a second
	// long-lived writer existed: its pin came out of the readers' four and
	// nothing said so.
	defaultReaderConns = 4

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

	// MaxOpenConns bounds the connection pool; 0 means the derived bound —
	// defaultReaderConns plus PinnedWriters. Setting it wins outright, and
	// a caller that sets it owns the arithmetic PinnedWriters does for
	// everybody else.
	MaxOpenConns int

	// PinnedWriters is how many connections will be held for the life of
	// this handle by [DB.Writer], and it is passed by the one caller that
	// knows the number rather than fixed here.
	//
	// A pinned connection counts against MaxOpenConns like any other, so a
	// handle with N pins and a fixed pool of four leaves 4−N for every
	// reader — the dashboard, the probes, the coverage checks — with
	// nothing naming the loss. The engine registers the writers, so the
	// engine passes the count; the store owns the arithmetic because the
	// pool is what is shared.
	PinnedWriters int

	// ReplicatedPath is where the replicated estate lives. Empty derives
	// it from the node estate's own path — see [ReplicatedPath].
	//
	// Configurable because the two files have different appetites: the
	// replicated estate is what a snapshot copies and what an adopting
	// node writes at line rate, so an operator with a fast local disk and
	// a large network volume has a real reason to separate them. Both are
	// still THIS NODE's, exclusively locked, and neither is shared.
	ReplicatedPath string

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
	//
	// It is fixed for the life of the handle once it is non-zero: the
	// vectors already in the file are this wide, and the engine refuses a
	// revision that would change it. The one exception is a store opened
	// at 0 — a node with no active revision, holding no rows — which
	// learns its width from its first apply via [DB.LearnEmbeddingDim].
	EmbeddingDim int
}

// forEstate resolves the options one estate is opened with, which today is
// the pool bound and nothing else.
//
// THE PINS ARE THE REPLICATED ESTATE'S: a pinned connection belongs to an
// applier, and an applier writes there. Sizing both pools for them would
// leave the node estate with headroom nothing takes, and sizing neither
// would leave a writer silently holding a reader's connection.
func (o Options) forEstate(estate Estate) Options {
	if o.MaxOpenConns <= 0 {
		o.MaxOpenConns = defaultReaderConns
		if estate == EstateReplicated {
			o.MaxOpenConns += o.PinnedWriters
		}
	}
	return o
}

// poolSize is the pool bound with the default applied. A method rather than a
// branch at each call site: [Open] and [Pending] both build a pool, and the
// second one skipping a bound the first applies is exactly the drift that
// made Pending a different database connection from the engine's.
func (o Options) poolSize() int {
	if o.MaxOpenConns <= 0 {
		return defaultReaderConns
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
	sql    *sql.DB
	path   string
	caps   Capabilities
	estate Estate

	// replicated is the OTHER estate, held by the node handle and nil on
	// the replicated one. The node handle owns its lifetime: one Open
	// brings both up and one Close takes both down, because a process
	// holding one file's lock and not the other's is a state no caller
	// asked for and none could recover from.
	replicated *DB

	// dim is [Options.EmbeddingDim], re-stated by every config apply. ATOMIC
	// because the applying goroutine writes it while turns are reading it to
	// validate their vectors — a plain int here is a data race the detector
	// finds on any company that writes memory during a reconcile.
	dim atomic.Int64

	// lock is this process's exclusive claim on path, held for the life of
	// the handle and released by Close — or by the kernel, if this process
	// does not get to run Close. Nil for an in-memory database, which has
	// no file to exclude anyone from. See lock.go.
	lock *fileLock

	// pins bounds how many connections [DB.Writer] may hand out, and how
	// many it has. DECLARED rather than discovered: a pin past the count
	// the pool was sized for is refused NAMING the count, because the
	// alternative is a writer that silently takes a reader's connection
	// and a read burst that queues behind it with nothing to read.
	pins struct {
		mu       sync.Mutex
		declared int
		held     int
	}
}

var log = logging.Get("store")

// ErrOneFile reports a node configured to keep both estates in one file.
//
// Its own sentinel because it is a CONFIGURATION mistake with an obvious
// remedy, and because it is the one failure here that would otherwise succeed:
// nothing crashes, the schema sequences interleave in one database and both
// appliers write beside the audit log.
var ErrOneFile = errors.New("store: the node and replicated estates cannot be the same file")

// Open opens (creating if absent) a node's TWO databases, applies any pending
// schema to each, and probes the driver's capabilities.
//
// path is the NODE estate's file; the replicated estate's is
// [Options.ReplicatedPath], or derived from path when that is empty. This
// process takes an EXCLUSIVE lock on each for the life of the handle — see the
// package doc for why, and lock.go for how. A second crewlet process opening
// either gets [ErrLocked] rather than a database the two of them corrupt
// between them.
//
// The returned handle is the node estate. Its peer is [DB.Replicated], whose
// lifetime it owns: this Open brings both up, and [DB.Close] takes both down.
func Open(ctx context.Context, path string, opts Options) (*DB, error) {
	replicatedPath := ReplicatedPath(path, opts.ReplicatedPath)
	// REFUSED BEFORE EITHER LOCK. Two exclusive claims on one path do not
	// collide inside this process — the claim is refcounted — so what one
	// file for both estates actually produces is one database carrying two
	// migration sequences, with both appliers writing into the audit log's
	// file. It fails as data rather than as an error.
	if replicatedPath == path && !strings.HasPrefix(path, ":memory:") && path != "" {
		return nil, fmt.Errorf("%w: %s", ErrOneFile, path)
	}
	db, err := openEstate(ctx, EstateNode, path, opts)
	if err != nil {
		return nil, err
	}
	// THE REPLICATED ESTATE SECOND, and its failure closes the first. A
	// handle on one file and not the other is a node that would apply
	// records into a database it has no checkpoint table in, and the
	// caller has no way to ask which half it got.
	replicated, err := openEstate(ctx, EstateReplicated, replicatedPath, opts)
	if err != nil {
		_ = db.Close()
		return nil, err
	}
	// THE PROBE IS NOT REPEATED. It answers a question about the DRIVER —
	// one compiled-in library, in one process — so a second probe asks the
	// same question of the same code and pays a binary search of prepared
	// statements to hear the same answer.
	replicated.caps = db.caps
	db.replicated = replicated
	return db, nil
}

// ReplicatedPath is where the replicated estate lives for a node whose own
// estate is at nodePath.
//
// BESIDE IT, under a name of its own: the two files are one node's, taken
// together by a backup and lost together with the disk, so putting them in one
// directory is what makes "back up the data directory" true. An explicit
// setting wins outright.
//
// An in-memory node estate gets an in-memory peer, which is a DIFFERENT
// anonymous database rather than the same one — exactly as two files are two
// files.
func ReplicatedPath(nodePath, configured string) string {
	if strings.TrimSpace(configured) != "" {
		return configured
	}
	if nodePath == "" || strings.HasPrefix(nodePath, ":memory:") {
		return nodePath
	}
	return filepath.Join(filepath.Dir(nodePath), replicatedFileName)
}

// replicatedFileName is the replicated estate's name beside the node's.
const replicatedFileName = "crewlet-replicated.db"

// openEstate opens one estate: its lock, its pool, its own migration
// sequence, and — for the node estate, which goes first — the capability
// probe both share.
func openEstate(ctx context.Context, estate Estate, path string, opts Options) (*DB, error) {
	opts = opts.forEstate(estate)
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

	db := &DB{sql: pool, path: path, lock: lock, estate: estate}
	// THE PINS ARE THE REPLICATED ESTATE'S. A pinned connection is an
	// applier's, and an applier writes there — so the node estate keeps
	// its four readers and the pool that grows is the one the writers are
	// actually on.
	if estate == EstateReplicated {
		db.pins.declared = opts.PinnedWriters
	}
	// Straight to the field, not through [DB.LearnEmbeddingDim]: that one
	// only ever raises from 0 because it guards a LIVE handle, and this is
	// the open where whatever the caller passed — including 0 — is the
	// answer.
	db.dim.Store(int64(opts.EmbeddingDim))
	applied, err := db.migrate(ctx)
	if err != nil {
		_ = pool.Close()
		return nil, err
	}
	if estate == EstateNode {
		db.caps = probe(ctx, pool)
	}

	log.InfoContext(ctx, "store_opened",
		"estate", string(estate),
		"path", path,
		"engine_version", engineVersion(ctx, pool),
		"migrations_applied", len(applied),
		"vector_functions", db.caps.VectorFunctions,
		"vector_index", db.caps.VectorIndex,
		"full_text_search", db.caps.FullTextSearch,
		"max_variables", db.caps.MaxVariables,
		"page_cache_kib", db.caps.PageCacheKiB,
		"pinned_writers", db.pins.declared,
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
	pool.SetMaxOpenConns(opts.poolSize())
	// Idle capacity matches open capacity: these are file handles on local
	// storage, not sockets to a remote server, so retiring one buys nothing
	// and paying to re-establish it (plus its session pragmas) on the next
	// query costs real latency on the read path.
	pool.SetMaxIdleConns(opts.poolSize())

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
	// THE PEER FIRST, and its error is reported even when this one also
	// fails: a node that closed half its estates and returned the other
	// half's error would leave a lock held with nothing naming it.
	var peer error
	if d.replicated != nil {
		peer = d.replicated.Close()
		d.replicated = nil
	}
	err := d.sql.Close()
	d.lock.release()
	return errors.Join(err, peer)
}

// Replicated is the handle on the estate a state log's appliers write.
//
// Nil on a handle that IS the replicated estate, which is what makes the
// boundary checkable at a glance: there is no chain of peers, and a caller
// holding one estate cannot reach back to the other's peer and lose track of
// which file it is writing.
func (d *DB) Replicated() *DB {
	if d == nil {
		return nil
	}
	return d.replicated
}

// Estate names which of a node's two databases this handle is on.
func (d *DB) Estate() Estate { return d.estate }

// Caps reports what the live driver can do. Probed once at Open — the answers
// are a property of the compiled-in driver version, so nothing re-measures
// them per query.
func (d *DB) Caps() Capabilities { return d.caps }

// Path reports the file this handle owns.
func (d *DB) Path() string { return d.path }

// ReplicatedPath is where the replicated estate lives, whichever estate this
// handle is.
//
// A caller measuring what a snapshot will cost is asking about the replicated
// file — the artefact is a copy of it alone — and it should not have to know
// whether it is holding the node handle or the replicated one to ask.
func (d *DB) ReplicatedPath() string {
	if d.estate == EstateReplicated {
		return d.path
	}
	if d.replicated != nil {
		return d.replicated.path
	}
	return ""
}

// EmbeddingDim reports the configured vector width, or 0 when no embedding
// model is configured. See Options.EmbeddingDim for why this is not in the
// schema.
func (d *DB) EmbeddingDim() int { return int(d.dim.Load()) }

// LearnEmbeddingDim records the width the first time this handle meets one.
//
// # It only ever raises the width from 0, and that is the whole contract
//
// The width describes the vectors this FILE already holds, not what the
// current config asks for, so it is emphatically not a setting an apply may
// change. The engine refuses a revision whose width differs from the one the
// store was opened at, and tells the operator to restart — see
// buildEmbedder. This must never become a way around that guard, and a plain
// setter would be exactly that way in two steps: drop the embeddings provider
// and apply (width falls to 0, guard off), then add it back at a different
// width (guard sees no declared width, lets it through). The recall pool then
// holds rows of two widths and the reader can match neither reliably.
//
// So: zero to non-zero, once. That is the only safe transition, and it is the
// one a node that booted with no active revision needs — it opened at 0
// holding no rows at all, and its first apply is what tells it how wide its
// vectors will be. Every other case is already correct at open, because
// [OpenBackends] passes the active revision's width.
//
// A width of 0 is "no declared width", not a width of zero:
// [DB.EncodeVector] checks nothing against it, which is what leaves the
// dimension guard off on a store that has never been told. See
// TestVectorDimensionUnconfigured.
func (d *DB) LearnEmbeddingDim(width int) {
	if width <= 0 {
		return
	}
	d.dim.CompareAndSwap(0, int64(width))
	// BOTH ESTATES LEARN IT. The width describes the vectors a node holds,
	// and which FILE those rows are in is a question the schema answers
	// rather than the width — so a handle that knew and a peer that did not
	// would leave the dimension guard on for one and off for the other,
	// which is the two-widths state this method exists to prevent.
	if d.replicated != nil {
		d.replicated.LearnEmbeddingDim(width)
	}
}

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
			// FULL is the driver's own default (measured at v0.8.0-pre.7:
			// `PRAGMA synchronous` answers 2 on a fresh connection), pinned
			// here so a driver bump cannot weaken commit durability
			// silently. The obvious alternative — NORMAL, mainline
			// SQLite's usual WAL pairing — trades the last commits before
			// a power cut for one fsync per checkpoint instead of per
			// commit, and this store is the seat's only memory: what it
			// writes has no other copy, and its commit rate (a handful per
			// second on a busy node) never earns the discount.
			"PRAGMA synchronous = FULL",
			// AND ON DARWIN, `FULL` alone is not what it says. Turso
			// moved macOS's FULL from fcntl(F_FULLFSYNC) to a plain
			// fsync() and put F_FULLFSYNC behind this pragma
			// (tursodatabase/turso#4760). A plain fsync() on macOS
			// returns before the drive's own write cache is flushed, so
			// a committed transaction is lost to a power cut on the one
			// platform where FULL reads as strongest. Harmless
			// elsewhere: every other platform ignores it.
			"PRAGMA fullfsync = 1",
			// SQLite defaults foreign keys OFF, which makes a declared
			// constraint look enforced right up until the day it
			// matters. synthesized_skill_versions declares one so that
			// deleting a skill cascades its history rather than
			// orphaning it; this is what makes the declaration true.
			"PRAGMA foreign_keys = ON",
			// 32 MiB per connection, and it is for the B-TREE INTERIOR
			// PAGES rather than for the scans. The hot table here carries
			// twenty-odd indexes and the postings list is read by term,
			// so what a cache this size buys is that a lookup's descent
			// does not go to the file; the big sequential reads are the
			// OS page cache's job, and sizing a per-connection cache for
			// them would be N copies of it.
			//
			// Negative means KiB rather than pages, which is what makes
			// the number mean the same thing whatever page size the file
			// was created with. Read back at Open as
			// [Capabilities.PageCacheKiB], because a driver that ignores
			// this leaves every connection on its own default and the
			// symptom appears nowhere near the cause.
			"PRAGMA cache_size = -32768",
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
	// A CONFLICTED TRANSACTION IS RETRIED, and fn may therefore run more
	// than once. That is safe by construction: the driver reports the
	// conflict on a statement inside the transaction, everything the
	// attempt wrote is rolled back before the next one begins, and fn sees
	// a fresh snapshot each time. A caller whose fn has side effects
	// OUTSIDE the transaction — a publish, a counter in memory — must make
	// them idempotent or move them out, which is what a transaction body
	// should be anyway.
	//
	// The driver's BeginTx ignores its options and always issues a plain
	// BEGIN, so a read-then-write that loses a race does not wait out a
	// busy timeout: it fails IMMEDIATELY with "database snapshot is
	// stale". Without a retry that error reaches the caller as a lost
	// write on a database with no other writer than this process, which is
	// how a conversation entry, a memory row or a config revision went
	// missing under nothing more than two goroutines. internal/learning
	// carried a private copy of this loop for one of its twelve callers;
	// the other eleven had none.
	return retryStale(ctx, func() error { return d.tx(ctx, fn) })
}

// retryStale runs one attempt at a time until it succeeds, fails for a reason
// a retry cannot fix, or exhausts [txAttempts].
//
// ONE LOOP, and that is the whole reason it is a function: [DB.Tx] and
// [Writer.Tx] both need it, and a second copy is how one of them comes to
// classify an error the other retries — which is exactly what the eleven
// callers without internal/learning's private copy paid for.
func retryStale(ctx context.Context, once func() error) error {
	for attempt := 0; ; attempt++ {
		err := once()
		if err == nil || attempt+1 >= txAttempts || !staleSnapshot(err) {
			return err
		}
		log.WarnContext(ctx, "store_tx_retry", "attempt", attempt+1, "error", err.Error(),
			"detail", "the transaction read a snapshot another writer had already "+
				"advanced past; retrying on a fresh one")
		sleepFor(ctx, txRetryBeat(attempt))
	}
}

// txAttempts is how many times a conflicted transaction is retried.
//
// Eight, and the number is measured rather than chosen: four goroutines each
// incrementing one row twelve times — the sharpest contention this store
// sees, since every one of them reads and writes the SAME row — still lost an
// update at three attempts even with a jittered pause. What fails at a budget
// this size is contention no retry loop should absorb silently anyway, and
// the caller gets the error rather than a lost write.
const txAttempts = 8

// txRetryBeat is the jittered, WIDENING pause between attempts.
//
// Two properties, and both were paid for. Jittered because the conflict
// returns with no wait of its own, so retries fired back to back re-collide
// inside the same contention window and spend the whole budget in a few
// microseconds. Widening because with a fixed window every loser of one round
// is a contender in the next at the same density: the window has to grow with
// the number of writers still fighting over the row, and the attempt count is
// the only estimate of that available here.
//
// The base is sized to what is being waited out — one local write transaction
// committing, which is microseconds — so even the last attempt's ceiling is a
// pause a caller never notices.
func txRetryBeat(attempt int) time.Duration {
	const base = 1_000 // microseconds
	spread := base << min(attempt, 5)
	return time.Duration(base+rand.N(spread)) * time.Microsecond
}

// staleSnapshot reports whether an error is the driver's read-then-write
// conflict.
//
// MATCHED ON TEXT, deliberately and with a comment saying so: the driver
// returns a bare error for this with no sentinel and no code to compare
// against, so the alternative to a string match is no retry at all. Kept
// narrow — two spellings, both of which are the driver's own — so an
// unrelated failure is never retried into a second side effect.
func staleSnapshot(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "snapshot is stale") ||
		strings.Contains(msg, "database is locked")
}

// sleepFor waits, or returns early if the context is done.
func sleepFor(ctx context.Context, d time.Duration) {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
	case <-t.C:
	}
}

// tx runs one attempt.
func (d *DB) tx(ctx context.Context, fn func(*sql.Tx) error) (err error) {
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
