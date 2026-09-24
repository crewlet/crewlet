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
// That is ADR-0004; what may write the replicated one is ADR-0002, and which
// estate a new table belongs in at all is ADR-0003. The driver this all rests
// on, and the release matrix it bounds, is ADR-0007.
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
// every `crewlet` command that opens the store's files does so from its own
// OS process, and the lock is what refuses one run beside a live engine.
//
// The files are the owner's alone on disk as well: every open makes a
// database and its -wal owner-only before the driver sees them, and takes
// every permission beyond the owner's from one it finds wider. See
// filemode.go for why the driver cannot be left to it.
//
// Everything that genuinely needs cross-process coordination — seat leases,
// config activations, the completion ledger, dedupe and rate valves — lives in
// the KV layer instead, and that separation is why nothing
// here has to be safe against a peer.
//
// It also means the migrator needs no lock inside the database: with one
// process per file, the only race left is two handles in this one, which an
// in-process mutex closes (see migrateMu).
//
// # One writer, and the begin that makes it safe
//
// Turso here is a SINGLE-WRITER database with a database-level write lock and
// no MVCC, and its own BeginTx discards the options database/sql passes it and
// issues a plain DEFERRED begin. Under that begin the lock is taken by a
// transaction's FIRST WRITE, which means a transaction that READS and then
// writes — the shape of every state-log applier — is aborted by any commit
// landing anywhere in the file in between, including into a table it never
// names.
//
// begin.go is what fixes that: [DB.Tx] and [Writer.Tx] take BEGIN IMMEDIATE,
// so the lock is held from the start and nobody else's commit can reach the
// window; [DB.Read] keeps the deferred begin, so a multi-statement read still
// excludes nobody. What follows from it is that a contended writer waits at
// BEGIN having done nothing, rather than discovering the loss at its first
// write and replaying everything above it.
//
// writequeue.go is the other half, and it answers the question the begin mode
// leaves open: WHO GETS THE LOCK NEXT. The driver's lock is a try-lock with no
// queue, polled by a busy handler, so a writer committing back to back starves
// a waiter until that waiter's busy timeout — measured, and the reason an
// applier's transaction ran four times against a writer that committed 2338
// times beside it. Every write transaction this process begins therefore takes
// its place in one FIFO line per file, handed from each holder to the writer
// that asked next. A writer's wait is then bounded by the work in front of it
// rather than by how its polls line up, which is what lets three domains'
// appliers share the replicated estate and each still drain.
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
	//
	// ATOMIC for the reason [DB.dim] is, and it is the same shape: an
	// adoption CLOSES the peer, renames the file underneath it and opens
	// it again, all while this node's own readers are running — so the
	// pointer is written while it is being read, which a plain field makes
	// a data race on every join.
	replicated atomic.Pointer[DB]

	// dim is [Options.EmbeddingDim], re-stated by every config apply. ATOMIC
	// because the applying goroutine writes it while turns are reading it to
	// validate their vectors — a plain int here is a data race the detector
	// finds on any company that writes memory during a reconcile.
	dim atomic.Int64

	// busy is this handle's configured busy timeout, kept so the retry
	// budget can anchor its pause to it — see [lockRetryBeat]. A number
	// derived from a setting has to travel with the handle the setting was
	// applied to, or an operator lowering it leaves a pause sized for a
	// timeout that no longer exists.
	busy time.Duration

	// lock is this process's exclusive claim on path, held for the life of
	// the handle and released by Close — or by the kernel, if this process
	// does not get to run Close. Nil for an in-memory database, which has
	// no file to exclude anyone from. See lock.go.
	lock *fileLock

	// swapClaim is a share of this process's claim on the REPLICATED
	// estate's path, held by the node handle from [DB.CloseReplicated]
	// until [DB.ReopenReplicated] succeeds, and nil at every other moment.
	// The peer's own share goes with its Close, and the install that runs
	// between the two replaces the file at that path — so without this
	// share the path would be free for another process to lock for exactly
	// that window, and the reopen would be refused. ATOMIC for the reason
	// replicated is: the adoption writes it while Close may be reading it.
	swapClaim atomic.Pointer[fileLock]

	// writes is the line this handle's write transactions take their place
	// in, shared with every other handle this process has open on the same
	// file. Taken from [fileLock.queue] at open, so it is never nil on a
	// handle a caller can reach. See writequeue.go.
	writes *writeQueue

	// opened is the Options this handle was opened with, kept so
	// [DB.ReopenReplicated] can bring the peer back up IDENTICALLY. A
	// second Options assembled at the reopen is a second place to decide
	// the pool bounds, the pin count and the embedding width — and the
	// one thing a node must not do after adopting a peer's database is
	// come back up configured differently from how it went down.
	opened Options

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

// ErrNoEstate is a read or write issued through a handle that is not open.
//
// IT IS A STATE, NOT A BUG IN THE CALLER. [DB.Replicated] answers nil while an
// adoption holds the peer closed between its rename and its reopen, and again
// after [DB.Close] — both documented and deliberate — so a goroutine that was
// already in flight when one of those happened reaches here legitimately. What
// it must NOT reach is a nil dereference: a maintenance tick racing a shutdown
// panicked the engine, where every other late read in the same shutdown logged
// "sql: database is closed" and moved on.
var ErrNoEstate = errors.New("store: this estate is not open")

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
	db, err := openEstate(ctx, EstateNode, path, opts, nil)
	if err != nil {
		return nil, err
	}
	// THE REPLICATED ESTATE SECOND, and its failure closes the first. A
	// handle on one file and not the other is a node that would apply
	// records into a database it has no checkpoint table in, and the
	// caller has no way to ask which half it got.
	// THE PROBE IS NOT REPEATED — see openEstate's own doc for why it is
	// handed over rather than copied on afterwards.
	replicated, err := openEstate(ctx, EstateReplicated, replicatedPath, opts, &db.caps)
	if err != nil {
		_ = db.Close()
		return nil, err
	}
	db.replicated.Store(replicated)
	db.opened = opts
	return db, nil
}

// OpenEstate opens ONE estate's file, alone.
//
// [Open] opens a NODE: two files, two migration sequences, and the pairing
// between them. This opens one — which is the shape a COPY of a single estate
// has, and a snapshot artefact and a backup member are both exactly that.
//
// Opening such a copy with [Open] is not an error a caller sees: the migrator
// applies the OTHER estate's whole sequence to it, creates that estate's
// tables inside it, records them as applied, and opens a second file beside it
// for the estate the caller thought it had. The artefact is then no longer a
// copy of anything, and the only thing that ever notices is a table name the
// two estates happen to share.
func OpenEstate(ctx context.Context, estate Estate, path string, opts Options) (*DB, error) {
	// NIL, so this handle probes for itself. It has no sibling to inherit
	// from, and a zero MaxVariables here is not a missing log line but a
	// writer silently degraded to one statement per row.
	return openEstate(ctx, estate, path, opts, nil)
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

// openEstate opens one estate's file: its lock, its pool, its own migration
// sequence, and — for the node estate, which goes first — the capability
// probe both share.
//
// inherited is the DRIVER-level probe a sibling estate already paid for, or
// nil to probe this pool. It exists because the probe answers a question about
// the DRIVER — one compiled-in library, in one process — so a node's second
// estate would pay a binary search of prepared statements to hear the answer
// its first one already has.
//
// IT IS A PARAMETER RATHER THAN AN ASSIGNMENT AFTER THE FACT, because the caps
// are read the moment the handle exists: this function's own `store_opened`
// line reports them, and the state-log applier hands MaxVariables to every
// domain's apply, where [InsertRows] reads 0 as "one row per statement". Caps
// assigned once this returned would be logged as zeros for a handle that has
// them, and a standalone [OpenEstate] handle, which nothing else assigns caps
// to, would write the slow shape.
func openEstate(ctx context.Context, estate Estate, path string, opts Options,
	inherited *Capabilities) (*DB, error) {
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

	db := &DB{sql: pool, path: path, lock: lock, estate: estate,
		busy: opts.busyTimeout(), writes: lock.queue()}
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
	if inherited != nil {
		// The driver's answers carry over; the PAGE CACHE does not. It is
		// read from `PRAGMA cache_size` and `PRAGMA page_size`, which are a
		// connection's setting and a FILE's geometry — so the sibling's
		// number describes the sibling's file, not this one.
		db.caps = *inherited
		db.caps.PageCacheKiB = probePageCache(ctx, pool)
	} else {
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
		"without_rowid", db.caps.WithoutRowid,
		// EMPTY IS THE HEALTHY READING. A name here is a capability the
		// database engine has and this engine's connections cannot reach,
		// which is a decision waiting to be taken rather than a fault.
		"gated", db.caps.Gated,
		"max_variables", db.caps.MaxVariables,
		"page_cache_kib", db.caps.PageCacheKiB,
		"pinned_writers", db.pins.declared,
	)
	return db, nil
}

// openPrepared readies the native library and the database's files, and
// returns a live pool with this package's bounds and session state applied.
//
// ONE PATH TO A CONNECTION, and that is the whole reason it exists: every pool
// this package opens on a database file is built here, so none reaches the
// driver's own loader unprepared — whose answer to a half-written cache is a
// PANIC inside a sync.Once (see turso.go) — runs on an unbounded pool, or
// opens files the umask made (see filemode.go). A second path is how one of
// those gets skipped.
//
// It does NOT lock. Whether the file is claimed, and for how long, is the
// caller's to decide: [Open] holds the claim for the life of the handle,
// [Pending] only across its read, and a copy this process made has no peer to
// exclude.
func openPrepared(ctx context.Context, path string, opts Options) (*sql.DB, error) {
	// BEFORE THE POOL, because the driver loads its native library on the
	// first connection and PANICS if the shared cache it loads from is
	// half-written. Preparing it here turns a process that dies on its
	// first query into an error a caller can read, and stops two engines
	// starting at once from corrupting that cache at all.
	if err := prepareTursoLibrary(); err != nil {
		return nil, err
	}
	// THE FILES BEFORE THE DRIVER, because the driver makes whatever it does
	// not find from the process umask: see filemode.go.
	if err := ownerOnly(path); err != nil {
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
	if peerDB := d.replicated.Swap(nil); peerDB != nil {
		peer = peerDB.Close()
	}
	// A node closed while an adoption holds its peer closed still holds the
	// peer's path through the swap's share, which nothing else gives back.
	d.swapClaim.Swap(nil).release()
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
	return d.replicated.Load()
}

// Estate names which of a node's two databases this handle is on.
//
// The empty estate on a handle that is not open, which is the meaningful zero:
// a caller asking which file it is holding while the peer is closed is asking
// about no file. See [ErrNoEstate] for why nil is a state rather than a bug.
func (d *DB) Estate() Estate {
	if d == nil {
		return ""
	}
	return d.estate
}

// Caps reports what the live driver can do. Probed once at Open — the answers
// are a property of the compiled-in driver version, so nothing re-measures
// them per query.
func (d *DB) Caps() Capabilities {
	if d == nil {
		return Capabilities{}
	}
	return d.caps
}

// Path reports the file this handle owns, and the empty string on a handle
// that is not open — the same meaningful zero [DB.Estate] answers with.
func (d *DB) Path() string {
	if d == nil {
		return ""
	}
	return d.path
}

// ReplicatedPath is where the replicated estate lives, whichever estate this
// handle is.
//
// A caller measuring what a snapshot will cost is asking about the replicated
// file — the artefact is a copy of it alone — and it should not have to know
// whether it is holding the node handle or the replicated one to ask.
func (d *DB) ReplicatedPath() string {
	if d == nil {
		return ""
	}
	if d.estate == EstateReplicated {
		return d.path
	}
	if peer := d.Replicated(); peer != nil {
		return peer.path
	}
	return ""
}

// EmbeddingDim reports the configured vector width, or 0 when no embedding
// model is configured. See Options.EmbeddingDim for why this is not in the
// schema.
func (d *DB) EmbeddingDim() int {
	if d == nil {
		return 0
	}
	return int(d.dim.Load())
}

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
	// A HANDLE THAT IS NOT OPEN LEARNS NOTHING, and says so by doing nothing:
	// this reports no error, so the only honest answer on a closed handle is
	// the no-op. See [ErrNoEstate] — an apply in flight when an adoption nils
	// the peer reaches here legitimately.
	if d == nil {
		return
	}
	if width <= 0 {
		return
	}
	d.dim.CompareAndSwap(0, int64(width))
	// BOTH ESTATES LEARN IT. The width describes the vectors a node holds,
	// and which FILE those rows are in is a question the schema answers
	// rather than the width — so a handle that knew and a peer that did not
	// would leave the dimension guard on for one and off for the other,
	// which is the two-widths state this method exists to prevent.
	if peer := d.Replicated(); peer != nil {
		peer.LearnEmbeddingDim(width)
	}
}

// SQL exposes the pooled handle for store implementations built on this
// database. Application code goes through a typed store instead — a caller
// that reaches for raw SQL is writing a query nobody can find later.
func (d *DB) SQL() *sql.DB {
	if d == nil {
		return nil
	}
	return d.sql
}

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
	// THE BEGIN MODE GOES ON FIRST, so WrapDriver wraps IT rather than the
	// other way round — see [beginModeDriver] for why a fault injector
	// installed underneath would silently stop injecting.
	drv = &beginModeDriver{inner: drv}
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
// IT HOLDS THE FILE'S WRITE LOCK FROM ITS BEGIN, having queued for it behind
// every write transaction on this database that asked first (see
// writequeue.go). So fn reads a snapshot no other writer can advance, and a
// burst of writers is served in the order it arrived rather than in the order
// their busy handlers happen to poll. A transaction that only reads belongs in
// [DB.Read], which takes the deferred begin and queues behind nothing.
//
// A PANIC rolls back, hands the connection back and re-panics rather than
// leaving the transaction open. Both halves are load-bearing on a
// single-writer database: a connection returned to the pool with an
// uncommitted transaction on it refuses the next caller's BEGIN, and a
// connection never returned at all is one the pool has lost for good. Either
// way one bug in one handler wedges the whole process, the second way
// permanently.
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
	// WHAT IS LEFT TO RETRY is narrower than it was. This transaction now
	// takes the write lock at BEGIN ([beginModeDriver]), so a foreign
	// commit can no longer abort it between a read and a write — the
	// "database snapshot is stale" that used to reach a caller as a lost
	// write, on a database with no writer but this process, cannot be
	// produced by that shape any more. The loop stays because the OTHER
	// two reasons are unchanged: losing the race for the lock, which is
	// now an honest wait at BEGIN, and a connection the pool handed back
	// dirty. internal/learning carried a private copy of this loop for one
	// of its twelve callers; the other eleven had none.
	// THE NIL GUARD IS HERE AND NOT ONLY IN [DB.txOpts], which is where it
	// was and where it could never run: `budget(d.busy)` is an ARGUMENT,
	// evaluated before the call it guards, so a nil handle dereferenced on the
	// way in and the guard three frames down never saw it. See [ErrNoEstate].
	if d == nil || d.sql == nil {
		return ErrNoEstate
	}
	return retryTransient(ctx, budget(d.busy), func() error { return d.tx(ctx, fn) })
}

// txBudget is how many attempts each cause gets, and how long to wait between
// them. [pooled] and [pinned] are the two that exist.
type txBudget struct {
	attempts func(txCause) int
	beat     func(txCause, int) time.Duration
}

// retryTransient runs one attempt at a time until it succeeds, fails for a
// reason a retry cannot fix, or exhausts the budget for its cause.
//
// ONE LOOP, and that is the whole reason it is a function: [DB.Tx],
// [DB.Read] and [Writer.Tx] all need it, and a second copy is how one of them
// comes to classify an error the other retries — which is exactly what the
// eleven callers without internal/learning's private copy paid for.
func retryTransient(ctx context.Context, b txBudget, once func() error) error {
	for attempt := 0; ; attempt++ {
		err := once()
		cause := classify(err)
		if err == nil || cause == causeFatal || attempt+1 >= b.attempts(cause) {
			return err
		}
		log.WarnContext(ctx, "store_tx_retry", "attempt", attempt+1,
			"cause", causeName(cause), "error", err.Error())
		sleepFor(ctx, b.beat(cause, attempt))
	}
}

func causeName(c txCause) string {
	switch c {
	case causeStaleSnapshot:
		return "stale_snapshot"
	case causeLockTimeout:
		return "lock_timeout"
	case causeDirtyConn:
		return "dirty_connection"
	default:
		return "fatal"
	}
}

// budget is the retry budget every transaction in this package takes.
//
// ONE OF THEM, for pooled and pinned alike. There were two: a [Writer]'s
// refused to retry [causeDirtyConn], because the retry is the fix only when
// the next attempt draws a DIFFERENT connection and a pin had no other to
// draw. That is no longer true — an attempt that leaves its transaction
// possibly open retires the connection it ran on, and a pinned writer pins a
// fresh one in its place (see [Writer.replace]) — so the next attempt draws a
// different connection on both paths and a second budget would be a policy
// with nothing left to justify it.
func budget(busy time.Duration) txBudget {
	return txBudget{
		attempts: func(c txCause) int {
			switch c {
			case causeDirtyConn:
				return txAttempts
			case causeLockTimeout:
				return lockAttempts
			default:
				// causeStaleSnapshot AMONG THEM, and see its own
				// comment: no transaction this package begins can
				// meet one any more, and the one way left to
				// reach it is a body that writes inside a read —
				// where re-running it repeats a write its caller
				// declared to be a read.
				return 0
			}
		},
		beat: func(c txCause, attempt int) time.Duration {
			if c == causeLockTimeout {
				return lockRetryBeat(busy)
			}
			return txRetryBeat(attempt)
		},
	}
}

// lockAttempts is how many times a writer that lost the race for the write
// lock tries again.
//
// TWO — one retry — and the number is anchored rather than picked. This error
// only EXISTS after the full [Options.busyTimeout] has already elapsed, so
// attempt one has already given the holder five seconds and one retry gives
// it ten: that is the dashboard's own query timeout, the quantity
// [defaultBusyTimeout] is already defined as half of. A writer that has not
// released in ten seconds is not "another writer holds it and will not for
// long", it is a stuck batch, and the honest answer is the error.
//
// It was [txAttempts] — eight — until the begin mode moved contention from
// the first write to the BEGIN and made this cause common. Eight attempts of
// a five-second wait is forty seconds of stall with eight full replays of a
// four-thousand-row apply, which is the fleet-wide stall the applier's
// occupancy model exists to bound. Measured at 40.7s.
const lockAttempts = 2

// lockRetryBeat is the pause after losing the race for the write lock.
//
// JITTER ONLY, and no widening: the retry re-enters a fresh busy wait that is
// itself seconds long, so the only job of this pause is to de-synchronise two
// writers that timed out together. Drawn from a tenth of the configured busy
// timeout so that an operator lowering store.busy_timeout_seconds lowers both
// halves together rather than leaving a pause sized for a timeout that no
// longer exists.
func lockRetryBeat(busy time.Duration) time.Duration {
	if busy <= 0 {
		busy = defaultBusyTimeout
	}
	return time.Duration(rand.N(int64(busy / 10)))
}

// txAttempts is how many times a transaction that drew a dirty connection is
// retried.
//
// Eight, and the anchor moved with the reason. It was measured against four
// goroutines each incrementing one row twelve times — the sharpest contention
// this store sees — which still lost an update at three attempts even with a
// jittered pause. That race cannot happen now: a write transaction takes the
// lock at BEGIN and queues for it, so the four serialise and each body runs
// once (TestWriterAndTxShareOneWritePath asserts exactly that), and a number
// justified by a measurement of something that no longer occurs is a number
// nobody can re-derive.
//
// What it governs is [causeDirtyConn], and the bound is the POOL: each
// attempt RETIRES the connection it drew, so the worst case is drawing every
// dirty connection the pool can be holding before reaching a clean one. That
// is [defaultReaderConns] plus the pins a node declares — four plus three
// state-log domains today — and eight is the first round number above it.
// Every attempt of it costs a reconnect and no wait, so the budget is spent
// in milliseconds rather than in seconds; it is [lockAttempts] that bounds
// the seconds.
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

// txCause is why an attempt failed, in the three kinds that need three
// different answers.
//
// It replaces a bool, for the reason CLAUDE.md gives in general and this
// package paid for in particular: "conflicted", "starved for the lock" and
// "the pool handed back a dirty connection" are three different facts, and
// collapsing them into one retryable/not answer meant a FIVE-SECOND lock wait
// was retried eight times on a one-millisecond backoff. That is forty seconds
// of stall with eight full replays of the batch, and it was measured here
// rather than reasoned about: a test that forced the contention took 40.7s to
// fail.
type txCause int

const (
	// causeFatal is an error a retry cannot fix.
	causeFatal txCause = iota
	// causeStaleSnapshot is the driver's read-then-write conflict — a
	// commit landed in the file between this transaction's read and its
	// write.
	//
	// NAMED BUT NOT RETRIED. Since [beginModeDriver] it can only reach a
	// DEFERRED begin, which is [DB.Read]'s, and a read that only reads
	// never upgrades its snapshot and so never meets one. The single way
	// left to produce it is a body that WRITES inside [DB.Read], and
	// re-running that repeats a write its caller declared to be a read —
	// a silent double effect where the error is an accurate report of a
	// caller's own mistake. It keeps its name so the log line says which
	// of the four it was rather than "fatal".
	causeStaleSnapshot
	// causeLockTimeout is the busy timeout expiring on the write lock.
	causeLockTimeout
	// causeDirtyConn is a connection the pool handed back with a
	// transaction still open, reported on the next BEGIN over it.
	causeDirtyConn
)

// classify names why an attempt failed.
//
// MATCHED ON TEXT, deliberately and with a comment saying so: the driver
// returns bare errors for all three with no sentinel and no code to compare
// against, so the alternative to a string match is no retry at all. Kept
// narrow — every spelling here is the driver's own — so an unrelated failure
// is never retried into a second side effect.
func classify(err error) txCause {
	if err == nil {
		return causeFatal
	}
	// THE QUEUE'S OWN TIMEOUT IS THE SAME CAUSE as the driver's, and it is
	// matched on the sentinel rather than on text because it is this
	// package's own error. Both are the wait for one file's write lock
	// ended by one knob, so they take the same budget: [lockAttempts] gives
	// the holder two busy timeouts and then answers honestly, whether the
	// waiting was done in this process's line or in the driver's poll.
	if errors.Is(err, errWritersQueued) {
		return causeLockTimeout
	}
	msg := strings.ToLower(err.Error())
	switch {
	case strings.Contains(msg, "snapshot is stale"):
		return causeStaleSnapshot
	case strings.Contains(msg, "database is locked"):
		return causeLockTimeout
	case strings.Contains(msg, "transaction within a transaction"):
		// It is retryable because the attempt that met it RETIRED the
		// connection it drew ([giveBack]), so the next attempt draws a
		// different one. That is what makes the retry work at all:
		// database/sql hands out the connection it freed LAST, so a
		// retry that merely returned the dirty connection drew it
		// straight back, eight times — the projector's boot reconcile
		// did exactly that, restarting every two seconds against the
		// same connection and never hydrating, with the failure visible
		// only as a WARN nobody was watching.
		//
		// This package no longer leaves such a connection behind, so
		// what this clause meets is one somebody else's raw transaction
		// on [DB.SQL] left open — or, on a [Writer], the one its own
		// previous attempt retired and replaced.
		return causeDirtyConn
	}
	return causeFatal
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
//
// A NIL HANDLE IS AN ERROR, NOT A CRASH, and it is a state a caller can
// legitimately be holding: [DB.Replicated] answers nil while an adoption has
// the peer closed between its rename and its reopen, and again after [DB.Close].
// Both are documented and deliberate — so the honest answer to a read issued
// through one is the same shape every other late read already gets ("this
// estate is not open"), rather than a segfault that takes the process with it.
// Measured: a maintenance tick racing a shutdown panicked the whole engine.
func (d *DB) tx(ctx context.Context, fn func(*sql.Tx) error) (err error) {
	if d == nil || d.sql == nil {
		return ErrNoEstate
	}
	// THE CONNECTION BEFORE THE QUEUE, and the order is the whole reason
	// this draws one explicitly rather than beginning on the pool. Drawn
	// the other way round, a writer at the FRONT of the queue could be
	// waiting on the pool, behind readers that are waiting on the very
	// commit a pinned applier queued behind it is about to make — the
	// feedback loop [Writer] exists to break, rebuilt one layer up. A
	// writer waiting here holds a pooled connection instead, which is
	// exactly what one polling the driver's busy handler already held.
	conn, err := d.sql.Conn(ctx)
	if err != nil {
		return fmt.Errorf("store: begin: %w", err)
	}
	// FROM A DEFER, because [txOn] re-panics rather than returning: a
	// hand-back written on the return path alone never runs, and a pooled
	// connection never handed back is one database/sql has lost for the
	// life of the process. The pool is four plus the pins, so the fourth
	// panicking body would wedge the handle.
	retire := retireSwitch(conn)
	// UNFIT WHILE THE ATTEMPT IS IN FLIGHT, because a panic leaves nobody
	// to say whether its rollback happened.
	fit := false
	defer func() { giveBack(conn, retire, fit) }()
	fit, err = d.writeOn(ctx, conn, fn)
	return err
}

// writeOn is one write attempt on conn: its place in this file's queue, then
// the IMMEDIATE begin, then fn.
//
// [DB.Tx] and [Writer.Tx] differ only in where conn comes from, which is why
// both reach the lock through here. A second copy is how one of them comes to
// take the write lock in a way the other does not.
// It reports, as [txOn] does, whether conn is still fit to be used again.
func (d *DB) writeOn(ctx context.Context, conn *sql.Conn, fn func(*sql.Tx) error) (bool, error) {
	if err := d.writes.acquire(ctx, d.busy); err != nil {
		// NOTHING WAS BEGUN, so the connection is untouched: a writer
		// that never reached the front of the line is the one failure
		// here that costs nobody a reconnect.
		return true, err
	}
	// HELD FOR THE WHOLE TRANSACTION, panic included: a queue slot dropped
	// while its holder unwinds stalls every writer behind it until each
	// one's busy timeout, and then the next one again.
	defer d.writes.release()
	return txOn(ctx, conn.BeginTx, nil, fn)
}

// txOn runs ONE transaction on whatever begin it is given: begin with opts,
// run fn, then commit or roll back.
//
// One body for every transaction this package runs, pooled or pinned, read or
// write — [DB.Read] is the only caller that passes anything but nil for opts.
// nil is the WRITE mode, which is the default for the same reason
// [beginModeConn.Begin] takes it: everything through [DB.Tx] is a write until
// a caller says otherwise, and a default that quietly took the deferred begin
// would put the read-then-write abort back on whichever path forgot to ask.
// It reports whether the connection underneath is still FIT, which is false
// whenever the transaction may still be open on it: a BEGIN the driver
// refused (the usual reason is a transaction already open on that
// connection), a rollback that failed, and a commit that failed, since the
// driver keeps a transaction open when a commit is refused as SQLite does for
// a deferred constraint. Telling the harmless cases of those apart would be
// another text match over driver errors, and the cost of retiring a
// connection that was in fact clean is one reconnect on a path that is
// already failing.
func txOn(ctx context.Context,
	begin func(context.Context, *sql.TxOptions) (*sql.Tx, error),
	opts *sql.TxOptions, fn func(*sql.Tx) error,
) (fit bool, err error) {
	tx, err := begin(ctx, opts)
	if err != nil {
		return false, fmt.Errorf("store: begin: %w", err)
	}
	defer func() {
		if p := recover(); p != nil {
			rollback(ctx, tx)
			panic(p)
		}
	}()
	if err := fn(tx); err != nil {
		return rollback(ctx, tx), err
	}
	if err := tx.Commit(); err != nil {
		return false, fmt.Errorf("store: commit: %w", err)
	}
	return true, nil
}

// rollback undoes an attempt and reports whether it did.
//
// # Why a discarded rollback error is not harmless
//
// database/sql returns the connection to the pool when the transaction ends.
// If the rollback failed, the transaction is still open on that connection and
// the pool does not know: the next caller to draw it gets "cannot start a
// transaction within a transaction" from its own BEGIN, on a connection it did
// nothing to. The driver reports the failure and does not answer ErrBadConn, so
// nothing retires the connection either. On a PINNED connection that was every
// subsequent attempt, and the domain stopped applying for good.
//
// So the answer is the CALLER'S to act on — see [giveBack], which is what
// finally repairs it — and the failure is logged at WARN all the same: what
// forced a connection out of the pool is worth an operator's eye even though
// nothing downstream fails on it any more. Discarding it made a poisoned pool
// entry into a mystery that surfaced somewhere else entirely, as a subsystem
// that had been failing every two seconds for as long as the process had been
// up.
func rollback(ctx context.Context, tx *sql.Tx) bool {
	// ErrTxDone is the ORDINARY case and not a failure: the driver ends a
	// transaction itself when a statement inside it aborts, so a rollback
	// after one has nothing left to undo.
	err := tx.Rollback()
	if err == nil || errors.Is(err, sql.ErrTxDone) {
		return true
	}
	log.WarnContext(ctx, "store_rollback_failed", "error", err.Error(),
		"detail", "the transaction may still be open on its connection, so "+
			"the connection is retired rather than handed to another caller")
	return false
}

// retirer is a driver connection that can be told its transaction may still
// be open. [beginModeConn] is the implementation; a fault injector wrapping
// it forwards the two methods explicitly, as it does every other optional
// interface.
type retirer interface{ RetireSwitch() func() }

// retireSwitch reaches the driver connection under conn and returns the
// switch that retires it.
//
// TAKEN AT THE DRAW, which is the whole of why this is safe. [sql.Conn.Raw]
// races a concurrent close of the same connection — see
// [beginModeConn.IsValid] for the segfault that cost — and the one moment
// nothing can be closing this connection is before any transaction has been
// begun on it: the only closer is the awaitDone goroutine of a transaction
// that does not exist yet, and this goroutine is the only one holding the
// handle. So the switch is captured here and thrown later, rather than the
// connection being reached for later.
//
// A NIL SWITCH IS A DRIVER THAT CANNOT BE RETIRED, which is a wrapper that
// hid the interface rather than a state the engine reaches: the caller falls
// back to handing the connection back, which is what happened before any of
// this existed.
func retireSwitch(conn *sql.Conn) func() {
	var retire func()
	_ = conn.Raw(func(dc any) error {
		if r, ok := dc.(retirer); ok {
			retire = r.RetireSwitch()
		}
		return nil
	})
	return retire
}

// giveBack hands conn back, retiring it first when the attempt reported that
// its transaction may still be open.
//
// The switch was captured at the draw ([retireSwitch]); throwing it here is
// a store to an atomic and touches database/sql not at all, and the discard
// happens inside the Close below, where database/sql asks the driver's
// connection whether it is still valid.
//
// THE HAND-BACK IS LOAD-BEARING ON ITS OWN, and it is called FROM A DEFER at
// every call site rather than on the return path: an attempt whose body
// panicked unwinds past the return, and a pooled connection never handed
// back is one database/sql has lost for the life of the process. The pool is
// four plus the declared pins, so the fourth panicking body wedged the
// handle — measured, a fifth store.Tx blocked until the test binary's own
// ten minute timeout killed it.
func giveBack(conn *sql.Conn, retire func(), fit bool) {
	if !fit && retire != nil {
		retire()
	}
	_ = conn.Close()
}
