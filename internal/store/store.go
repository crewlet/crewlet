// Package store is the engine's local materialized-index database — the
// audit event log, the learning subsystem's memory, and the durable runtime
// state a turn leaves behind.
//
// # Two files: the node's own, and the replicated estate
//
// A node keeps its OWN estate in one file, which [OpenNode] brings up at the
// path it is given, and the REPLICATED estate — everything a state log's
// applier derives — in a second, which a data node opens on that handle from
// boot ([DB.OpenReplicated]; replicated.go says why on the handle rather than
// with it) and which [ReplicatedPath] names beside the node's own. A node
// without the `data` role never opens the second at all.
//
// The boundary is [Estate], and what rests on it is written there — a snapshot
// is a copy of one file rather than a copy of everything with the rest's pages
// deleted out of it, the identity claim a peer verifies is a checksum with
// nothing in the way, and an applier's write cadence is its own rather than
// shared with every audit insert.
//
// NO TRANSACTION SPANS THE TWO FILES, no read joins across them and nothing
// ATTACHes one to the other, which is a rule a static walk enforces rather
// than a convention: a transaction is one file. That is ADR-0004; what may
// write the replicated estate is ADR-0002, and which estate a new table
// belongs in at all is ADR-0003. The driver this all rests on, and the
// release matrix it bounds, is ADR-0007.
//
// # Two handles on the replicated estate, and who holds which
//
// The replicated estate reaches a caller as [ReplicatedHandle] — which can
// open a write transaction and pin a [Writer], and which the framework, the
// appliers and the runtime that hands them out hold — or as
// [ReplicatedReader], which can only read and which every other holder is
// handed: it has no write method, and the engine refuses a write inside its
// read transaction ([DB.Read]). Both resolve the file per call, so an estate
// closed under its holder answers [ErrNoEstate] rather than a stale pool. Who
// may write is then a property of the type a holder was given, and
// TestOnlyTheApplierWritesTheReplicatedEstate has only the short list of
// those that hold the first to read.
//
// Everything below is true of each file separately.
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
// [OpenNode] and [DB.OpenReplicated] therefore take an advisory OS lock for
// the life of the handle and answer a second PROCESS with [ErrLocked] — before
// any driver work, and naming the pid that holds the file. The claim is
// internal/flock's, whose package doc says why an OS lock rather than a pid
// file; lock.go says why two handles inside one process share it instead.
// That is what makes the rule above true rather than merely stated: the
// secret-store CLIs open this database from a second process as their
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
// in-process mutex per file — held on the claim every handle on that path
// shares, see [DB.migrate] — and the lock protocol simply disappears.
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
// rather than by how its polls line up, which is what lets every domain's
// applier share the replicated estate and each still drain.
//
// A writer whose line never reached the front — the holders ahead of it kept
// the lock through every attempt its budget allows — gives up with [ErrBusy],
// the one failure here a caller clears by trying again a moment later, and
// the reason it is marked rather than left as the driver's words: unmarked, a
// tool read lock contention as a fault of the node and told its caller that
// trying again would not help.
//
// # What a handle promises about its pool, its copies and its absence
//
// Each file's pool is its ordinary readers PLUS one connection held for
// identity work ([Identity]) PLUS one pinned writer per replicated log, every
// term ADDED, an explicit [Options.MaxOpenConns] included: a term taken out of
// the readers is a read burst queueing behind it with nothing naming the loss
// ([Options.poolSize]).
//
// A COPY ([DB.Backup], which a donor's snapshot and a fleet backup both take)
// is one moment holding every commit that returned before it began, and that
// is [DB.vacuumInto]'s doing, not the driver's (backup.go has the measurement
// and the tripwire test). The copy holds its view on a connection of its own,
// outside the pool, so it never needs two readers at once.
//
// The runner applies the migrations THIS binary carries and steps over a
// version it does not (a newer build's, before a rollback), so the node boots;
// [DB.ForeignMigrations] names what it stepped over for the one caller that
// must not read those tables as empty, the object store's collector.
//
// A store that is not there answers, it does not crash. A node without the
// `data` role opens a scratch store: the file is discarded under the store's
// own lock, so a database another process holds is refused rather than
// deleted, and a path that reaches for the replicated estate is told
// [ErrNoEstate] rather than handed an empty database that reads as a company
// with nothing in it. The same answer covers an estate between an adoption's
// rename and its reopen, and after Close; the open handle sits in an atomic
// slot because an adoption swaps it under running readers.
//
// The config history splits what one column used to say (ADR-0057): WHICH
// revision is the company is coord's activation pointer, and `company_config`
// holds only this node's facts about it (companyconfig.go). Anything else the
// COMPANY has to agree on belongs in coord, not here.
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
// Every connection, from [OpenNode], [DB.OpenReplicated] and [Pending], goes
// through one helper (turso.go): a second path skipped the native-library
// preparation and panicked.
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
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/crewlet/crewlet/internal/logging"
	"github.com/crewlet/crewlet/internal/words"

	// The driver registers itself under the name [driverName]. A blank
	// import because nothing here touches its types — the whole point of
	// the database/sql seam is that the engine talks to an interface.
	_ "turso.tech/database/tursogo"
)

// driverName is what the Turso driver registers itself as with database/sql.
//
// Unexported, and nothing selects it: Turso is the one driver because the
// recall paths (learning's diary and the knowledge base's semantic search)
// read through its vector distance functions, which no other SQLite driver
// has. ADR-0007 is the decision.
const driverName = "turso"

// Defaults for Options.
const (
	// MinReaderConns is the floor under the derived reader budget: EIGHT,
	// two full dashboards. The socket's query channel admits four
	// concurrent queries per socket (internal/api/stream.MaxInFlightQueries)
	// and a company is routinely watched from more than one tab; the two
	// numbers are one decision, so a change to either is a change to both.
	// The floor serves the small host (GOMAXPROCS 1 or 2). Surplus queries
	// wait safely: identity reads run on [identityReserve]'s connection.
	// EXPORTED because it is also the most pages the smallest data node
	// scans at once, and a Gitlet node holds its full reads to it
	// (gitlet/cache.MaxFullRounds), so a change here moves that cap.
	MinReaderConns = 8

	// identityReserve is how many connections ordinary work may NEVER
	// occupy, kept for identity work (resolving who is acting). ONE: a
	// reservation, not headroom, because a burst consumes headroom:
	// database/sql hands a free connection to whoever asks first, so a
	// socket storm of scans would take every one and queue the identity
	// read the waiting reads depend on. One suffices for a keyed lookup.
	//
	// ON BOTH FILES, ADDED to the readers as a pin is. HELD, not merely
	// counted ([DB.reserve]): a connection nobody holds is taken by the
	// first burst.
	identityReserve = 1

	// Half the dashboard's 10 s query timeout. A busy wait longer than the
	// timeout above it turns lock contention into a request that fails with
	// no error to show; half leaves the blocked writer room to finish and
	// report what happened.
	defaultBusyTimeout = 5 * time.Second

	// nodeCacheKiB is the page cache each connection keeps, on either file:
	// 32 MiB, for the B-TREE INTERIOR PAGES rather than scans, so a lookup's
	// descent stays off the file; big sequential reads are the OS page
	// cache's job, and a per-connection cache for them is N copies.
	// In KiB (a negative `PRAGMA cache_size`, see [openPool]) so it means
	// the same whatever the page size. Read back at every open as
	// [Capabilities.PageCacheKiB]: a driver that ignores it leaves each
	// connection on its default, far from the symptom.
	nodeCacheKiB = 32 << 10
)

// defaultReaderConns is how many connections ORDINARY readers get when
// [Options.MaxOpenConns] says nothing.
//
// max(MinReaderConns, GOMAXPROCS): a read is CPU-bound in this process, so a
// connection without a core buys queue depth, not concurrency (readers never
// block the writer under WAL; writers serialise on the file lock). The read
// concurrency of BOTH files, READERS ONLY: the pool adds [identityReserve] and
// one pinned writer per replicated log ([DB.OpenReplicated]). See
// [Options.poolSize].
func defaultReaderConns() int {
	return max(MinReaderConns, runtime.GOMAXPROCS(0))
}

// Options configures [OpenNode]. The zero value is valid.
type Options struct {
	// WrapDriver wraps the driver before any connection is opened. It
	// exists for FAULT INJECTION and nothing else.
	//
	// Every fail-open read has a branch that only runs when a result set
	// fails PART WAY THROUGH, during iteration. It decides between "nothing
	// is known" and a silent PARTIAL answer, and closing the database cannot
	// reach it: that makes the query itself fail, the other branch.
	//
	// It wraps rather than replaces, so a real driver still runs against a
	// real file. Nil in every non-test caller, with no config field. The
	// replicated estate is opened with the same wrapper as the node.
	WrapDriver func(driver.Driver) driver.Driver

	// MaxOpenConns is the ORDINARY READERS of the node's own file's pool and
	// of its replicated estate's — never fewer than two there; 0 means
	// [defaultReaderConns] for both.
	//
	// What ordinary work may not occupy is ADDED to it, never taken out of
	// it, configured or not: [identityReserve] on both files, and one
	// pinned writer per log the replicated estate carries
	// ([DB.OpenReplicated]). A writer or the identity read that took a
	// reader's connection would leave a read burst queueing with nothing
	// naming the loss — and a figure at or below what is held would leave
	// the readers nothing at all. So a handle opened at 1 is one reader,
	// with the reserve kept beside it.
	MaxOpenConns int

	// ReplicatedPath is where the replicated estate lives. Empty derives it
	// from the node's own path; see [ReplicatedPath].
	//
	// Configurable because the estates differ: the replicated one is what a
	// snapshot copies and an adopting node writes at line rate, so it can
	// merit a separate volume. Both stay THIS NODE's, exclusively locked.
	ReplicatedPath string

	// BusyTimeout is how long a statement waits for the file lock before
	// giving up; 0 means defaultBusyTimeout.
	BusyTimeout time.Duration

	// Scratch makes [OpenNode] DISCARD whatever is at the node estate's
	// path before opening it: the file and every sidecar, under the store's
	// own lock, so a database another process or another handle in THIS
	// process holds is refused, never deleted underneath it.
	//
	// It is what a node without the `data` role runs: nothing it writes
	// here has to outlive it, and it holds no replicated estate. Set only by
	// the engine's own open for a running node, never by a tool that opens
	// the same file to read it.
	Scratch bool

	// EmbeddingDim is the width of the vectors the active company config's
	// embedding model produces. A RUNTIME property, deliberately: vector
	// columns are plain BLOBs and only this knows how wide they are, so the
	// migrator needs no two-phase bootstrap to learn it. 0 means no
	// embedding model is configured.
	//
	// Fixed for the life of the handle once non-zero: the vectors in the
	// file are this wide, and the engine refuses a revision that would
	// change it. A store opened at 0 (a node that boots on no revision)
	// learns its width from its first apply via [DB.LearnEmbeddingDim].
	EmbeddingDim int

	// pins is how many connections [DB.Writer] may hold for the life of
	// the handle. Set for the replicated estate's handle from the logs it
	// was opened for ([DB.OpenReplicated]), and zero on every other: the
	// node's own file has no apply loop to pin a writer on it.
	pins int
}

// readers is how many connections ORDINARY work may draw: [Options.MaxOpenConns]
// when it is set, [defaultReaderConns] when it is not.
func (o Options) readers() int {
	if o.MaxOpenConns <= 0 {
		return defaultReaderConns()
	}
	return o.MaxOpenConns
}

// poolSize is a handle's whole bound:
//
//	pool = readers + identityReserve + pins
//
// with the pins held by their writers ([DB.Writer]) and [identityReserve] held
// by the handle itself ([DB.reserve]). EVERY TERM IS ADDED, configured or not:
// a connection taken out of the readers is a read burst queueing with nothing
// naming the loss.
//
// Holding connections, not an admission semaphore below the pool, keeps
// `sql.DBStats.WaitCount` counting, the sole input of the `pool_starved` alarm.
// A method so [OpenNode] and [Pending] cannot drift on the bound.
func (o Options) poolSize() int {
	return o.readers() + identityReserve + o.pins
}

// identityKey marks a context as identity work. Its own unexported type, as
// the context package requires, so no other package's key can collide with it.
type identityKey struct{}

// Identity marks ctx as IDENTITY WORK: resolving who is acting, before a
// route, a query or a tool call can decide anything. A READ on such a context
// runs on the connection [DB.reserve] holds back, the only way to spend it.
//
// A WRITE is not covered: every write takes the same place in the same queue
// (see writequeue.go) whatever connection it began on.
//
// IT IS NOT A PRIORITY FLAG: marking ordinary work makes it SLOWER (identity
// reads serialise on one connection) and removes the reserve's guarantee; a
// login that hangs under load then appears far from the call that took the
// connection. Only "who is this" lookups belong here.
func Identity(ctx context.Context) context.Context {
	return context.WithValue(ctx, identityKey{}, true)
}

// isIdentity reports whether ctx was marked by [Identity].
func isIdentity(ctx context.Context) bool {
	marked, _ := ctx.Value(identityKey{}).(bool)
	return marked
}

// busyTimeout is the lock wait with the default applied. See poolSize.
func (o Options) busyTimeout() time.Duration {
	if o.BusyTimeout <= 0 {
		return defaultBusyTimeout
	}
	return o.BusyTimeout
}

// DB is an open handle on the local store: a connection pool, the schema it
// has applied, and the capability answers probed against the live driver.
//
// A NODE's own handle ([OpenNode]) also holds the node's replicated estate
// once it is opened ([DB.OpenReplicated]); the replicated estate's own handle,
// and a copy opened with [OpenEstate], hold nothing but their own file.
type DB struct {
	sql    *sql.DB
	path   string
	caps   Capabilities
	estate Estate

	// connector is what sql opens its connections with, kept for the one
	// connection this handle opens OUTSIDE its pool: the view a copy holds
	// while it runs ([DB.holdViewFor]). The same connector rather than a second
	// one built from the same settings, so that connection carries the same
	// session state, the same begin mode and the same [Options.WrapDriver]
	// instance as every pooled one — a fault injector armed on the handle
	// reaches it too.
	connector *connector

	// copies is what this handle knows of the copies taken of its file: the
	// ones holding its write-ahead log now, and what the last to finish held
	// back — see copies.go.
	copies copies

	// slot is the replicated estate this node holds open, on a node's own
	// handle, and nil on every other. The node handle owns its lifetime:
	// its Close takes it down, because a process holding the replicated
	// file's lock with no handle left to release it is a state no caller
	// asked for and none could recover from.
	slot *replicatedSlot

	// owner is the node that holds this replicated estate open, on the
	// replicated estate's handle, and nil on every other. It is how an
	// estate closed through its OWN handle leaves its node's slot — see
	// [DB.Close].
	owner *DB

	// closed is set by the first Close, and it is what makes a handle
	// closed UNDER a caller answer [ErrNoEstate] like one that was never
	// open: the replicated estate's database is taken for one operation,
	// and an adoption may close it while that operation runs — the pool is
	// then closed but not nil, and database/sql's own "database is closed"
	// would reach a caller whose branch for an estate that is not open
	// never sees it. It also makes Close idempotent, which the shared lock
	// depends on: a second release of one handle's claim would drop
	// ANOTHER handle's share of it, and the file would be unlocked while
	// that handle still had it open.
	closed atomic.Bool

	// eventSeats resolves a handle an event names to the AGENT id the party
	// index is keyed on — see [DB.SetEventSeats]. ATOMIC for dim's reason:
	// the engine installs it while a publish listener may already be
	// appending on another goroutine.
	eventSeats atomic.Pointer[SeatResolver]

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

	// pool is this handle's connection bound — [Options.poolSize], the
	// readers, the identity reserve and the pins together — kept for the
	// same reason busy is: [dirtyAttempts] is a function of it, and a
	// retry budget sized for a pool other than the one the handle opened
	// is a number nobody can re-derive.
	pool int

	// lock is this process's exclusive claim on path, held for the life of
	// the handle and released by Close — or by the kernel, if this process
	// does not get to run Close. Nil for an in-memory database, which has
	// no file to exclude anyone from. See lock.go.
	lock *fileLock

	// writes is the line this handle's write transactions take their place
	// in, shared with every other handle this process has open on the same
	// file. Taken from [fileLock.queue] at open, so it is never nil on a
	// handle a caller can reach. See writequeue.go.
	writes *writeQueue

	// opened is the Options a node's handle was opened with, kept so its
	// replicated estate — reopened after an adoption's install included — is
	// opened with the SAME wrapper, lock wait and paths. A
	// second Options assembled at each open is a second place to decide
	// them, and the one thing a node must not do after adopting a peer's
	// file is come back up configured differently from how it went down.
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

	// reserve is [identityReserve]'s connection, drawn at open and HELD for
	// the life of the handle; [Identity] spends it. HELD RATHER THAN COUNTED,
	// as a pin is (see [Writer]): a spare nobody holds is taken by the first
	// burst. HELD RATHER THAN GATED: a semaphore would bound ordinary work
	// below [Options.poolSize], so database/sql would never reach its limit
	// and the `pool_starved` alarm, whose input is `sql.DBStats.WaitCount`,
	// would go quiet.
	//
	// conn is nil while a replacement is drawn, after a failed draw, and
	// after Close. The mutex serialises identity readers on it. held is true
	// from the open until Close, so a re-draw is allowed while set and
	// nothing is drawn from a pool Close is taking down.
	reserve struct {
		mu     sync.Mutex
		held   bool
		conn   *sql.Conn
		retire func()
	}
}

var log = logging.Get("store")

// ErrOneFile reports a node configured to keep its own estate and the
// replicated estate in one file.
//
// Its own sentinel because it is a CONFIGURATION mistake with an obvious
// remedy, and because it is the one failure here that would otherwise succeed:
// nothing crashes, the schema sequences interleave in one database and the
// appliers write beside the audit log.
var ErrOneFile = errors.New("store: the node's own estate and the replicated estate cannot be the same file")

// ErrNoEstate is a read or write issued through a handle that is not open.
//
// IT IS A STATE, NOT A BUG IN THE CALLER. [DB.ReplicatedDB] answers it on a
// node without `data`; while an adoption holds the file closed between its
// rename and its reopen; and after [DB.Close], so a goroutine already in
// flight reaches it legitimately. What it must NOT reach is a nil dereference:
// a maintenance tick racing a shutdown must not panic the engine.
var ErrNoEstate = errors.New("store: this estate is not open")

// ErrBusy is a write transaction that gave up because this node's store never
// granted it the file's write lock through every attempt [lockAttempts] allows
// (two busy timeouts, ten seconds at the default). A CONDITION, NOT A FAULT:
// the lock frees when the writers ahead commit, so the same write a moment
// later lands. Unmarked, "database is locked" reached a tool as
// `internal_error`, "trying again does not fix it".
//
// ONLY THE LOCK THAT WAS NEVER TAKEN: it is taken at BEGIN ([beginModeDriver]),
// so such a transaction never ran its body. A statement that failed with the
// lock held (constraint, malformed query, refused write) is never marked.
//
// Wrapped beside the driver's own error ("%w: %w"), so a log still reads it.
var ErrBusy = errors.New("store: this node's store could not take its write lock in time")

// OpenNode opens (creating if absent) a node's OWN database, applies any
// pending schema to it, and reads its capabilities: the driver's, which the
// process measures once and every later open reuses, and the file's page
// cache, which is read on every open (see [openEstate]). A data node's
// replicated estate is opened on the handle it returns ([DB.OpenReplicated]).
//
// This process takes an EXCLUSIVE lock on the file for the life of the handle
// — see the package doc for why, and lock.go for how. A second crewlet process
// opening it gets [ErrLocked] rather than a database the two of them corrupt
// between them.
func OpenNode(ctx context.Context, path string, opts Options) (*DB, error) {
	if opts.Scratch {
		if err := discard(path, removeDatabaseFiles); err != nil {
			return nil, err
		}
	}
	db, err := openEstate(ctx, EstateNode, path, opts)
	if err != nil {
		return nil, err
	}
	db.opened = opts
	db.slot = &replicatedSlot{}
	return db, nil
}

// OpenEstate opens ONE estate's file, alone.
//
// [OpenNode] opens a NODE, whose replicated estate it goes on to hold. This
// opens one file of either estate by itself — which is the shape a COPY has,
// and a snapshot artefact and a backup member are both exactly that.
//
// Opening a replicated copy with [OpenNode] is not an error a caller sees:
// the migrator applies the node estate's whole sequence to it, creates those
// tables inside it and records them as applied. The artefact is then no longer
// a copy of anything, and the only thing that ever notices is a table name the
// two estates happen to share.
func OpenEstate(ctx context.Context, estate Estate, path string, opts Options) (*DB, error) {
	if !estate.Valid() {
		return nil, fmt.Errorf("store: open %s: %q is not an estate; the estates are %s",
			path, estate, words.And(Estates))
	}
	opts.pins = 0
	return openEstate(ctx, estate, path, opts)
}

// ReplicatedPath is where the replicated estate lives for a node whose own
// estate is at nodePath.
//
// BESIDE IT, under a name of its own: the files are one node's, taken
// together by a backup and lost together with the disk, so putting them in one
// directory is what makes "back up the data directory" true. An explicit
// setting — store.replicated_path — wins outright.
//
// An in-memory node estate gets an in-memory replicated estate, which is a
// DIFFERENT anonymous database rather than the same one — exactly as two files
// are two files.
func ReplicatedPath(nodePath, configured string) string {
	if strings.TrimSpace(configured) != "" {
		return configured
	}
	if nodePath == "" || strings.HasPrefix(nodePath, ":memory:") {
		return nodePath
	}
	return filepath.Join(filepath.Dir(nodePath), replicatedFileName)
}

// replicatedFileName is the replicated estate's file name beside the node's.
const replicatedFileName = "crewlet-replicated.db"

// openEstate opens one estate's file: its lock, its pool, its own migration
// sequence, and its capabilities.
//
// THE DRIVER'S CAPABILITIES ARE THE PROCESS'S ([driverMeasurement]): every file
// opened after the first reuses its answer, and only the page cache, a property
// of the file, is read per open. A pool on a WRAPPED driver probes for itself.
//
// THEY ARE SET BEFORE THE `store_opened` LINE, every field, on every handle:
// [InsertRows] reads MaxVariables 0 as "one row per statement", so unassigned
// caps write the slow shape and that line would misreport it.
func openEstate(ctx context.Context, estate Estate, path string, opts Options) (*DB, error) {
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

	db := &DB{path: path, lock: lock, estate: estate,
		busy: opts.busyTimeout(), pool: opts.poolSize(), writes: lock.queue()}
	// DECLARED, and only the replicated estate's handle declares any: a
	// pinned connection is an apply loop's, and every apply loop writes the
	// replicated estate, so the node's own file keeps its readers and
	// nothing else.
	db.pins.declared = opts.pins
	if db.sql, db.connector, err = openConnected(ctx, path, opts); err != nil {
		return nil, err
	}
	// Straight to the field, not through [DB.LearnEmbeddingDim]: that one
	// only ever raises from 0 because it guards a LIVE handle, and this is
	// the open where whatever the caller passed — including 0 — is the
	// answer.
	db.dim.Store(int64(opts.EmbeddingDim))
	applied, err := db.migrate(ctx)
	if err != nil {
		_ = db.sql.Close()
		return nil, err
	}
	// THE IDENTITY RESERVE, drawn AFTER the migrator: a migration runs a
	// sequence of transactions on this same pool, and holding a connection
	// back across it would be a connection the bring-up could not use for
	// the one thing that has to finish before anything else runs.
	db.holdIdentityReserve(ctx)
	var unanswered error
	db.caps, unanswered = measuredDriver.capabilities(ctx, db.sql, opts.WrapDriver != nil)
	// The driver's answers carry over; the PAGE CACHE does not. It is read
	// from `PRAGMA cache_size` and `PRAGMA page_size`, which are a
	// connection's setting and a FILE's geometry — so another file's number
	// describes that file, not this one.
	db.caps.PageCacheKiB = probePageCache(ctx, db.sql)
	if unanswered != nil {
		// THE ONE PLACE A DEGRADED HANDLE SAYS WHY. Its `store_opened` line
		// below carries the conservative answers — a max_variables of 999 —
		// and without this nothing would tell that from a driver that
		// really has them.
		log.WarnContext(ctx, "store_capability_probe_unanswered",
			"estate", string(estate), "path", path, "error", unanswered.Error(),
			"detail", "the driver did not answer every capability question, so "+
				"this handle carries the conservative answer to each it missed; "+
				"the next open asks again")
	}

	log.InfoContext(ctx, "store_opened",
		"estate", string(estate),
		"path", path,
		"engine_version", engineVersion(ctx, db.sql),
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
		"pool", opts.poolSize(),
		"pinned_writers", db.pins.declared,
	)
	return db, nil
}

// openPrepared readies the native library and returns a live pool with this
// package's bounds and session state applied.
//
// ONE PATH TO A CONNECTION: [OpenNode] and [Pending] both need a pool, and a
// second path skipped preparing the native library, so the driver's own loader
// met a half-written cache and PANICKED inside a sync.Once (see turso.go), and
// it ran on an unbounded pool.
//
// It does NOT lock. The claim belongs to the caller, because callers want
// opposite things: an open holds it for the life of the handle, and Pending
// takes it only across its read.
func openPrepared(ctx context.Context, path string, opts Options) (*sql.DB, error) {
	pool, _, err := openConnected(ctx, path, opts)
	return pool, err
}

// openConnected is [openPrepared] answering the connector the pool draws from
// as well, for the handle that opens a connection outside its pool
// ([DB.connector]).
func openConnected(ctx context.Context, path string, opts Options) (*sql.DB, *connector, error) {
	// BEFORE THE POOL, because the driver loads its native library on the
	// first connection and PANICS if the shared cache it loads from is
	// half-written. Preparing it here turns a process that dies on its
	// first query into an error a caller can read, and stops two engines
	// starting at once from corrupting that cache at all.
	if err := prepareTursoLibrary(); err != nil {
		return nil, nil, err
	}
	pool, conns, err := openPool(path, opts.busyTimeout(), opts.WrapDriver)
	if err != nil {
		return nil, nil, err
	}
	pool.SetMaxOpenConns(opts.poolSize())
	// Idle capacity matches open capacity: these are file handles on local
	// storage, not sockets to a remote server, so retiring one buys nothing
	// and paying to re-establish it (plus its session pragmas) on the next
	// query costs real latency on the read path.
	pool.SetMaxIdleConns(opts.poolSize())

	if err := pool.PingContext(ctx); err != nil {
		_ = pool.Close()
		return nil, nil, fmt.Errorf("store: open %s: %w", path, err)
	}
	return pool, conns, nil
}

// engineVersion is what the database engine calls itself, for the one log line
// an operator reads when a store behaves unlike it did before a driver bump.
// Worth having because the driver is pre-1.0 and pinned, so "what changed" is
// a real support question with no other answer inside the process.
//
// TWO NUMBERS, because turso_version() and sqlite_version() disagree and
// neither is the driver's module version. It reports what was asked, not a
// guess at which is the release and which a compatibility level; the
// unambiguous identifier is the pin in go.mod.
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
//
// THE REPLICATED ESTATE'S HANDLE LEAVES ITS NODE'S SLOT FIRST, exactly as
// [DB.CloseReplicated] takes it out: the estate is held by its node, and one
// closed behind the node's back would stay in the slot as open — answered to
// every lookup, answered again to an open of the same file, and never reopened
// by the runtime, which asks the slot whether it has lost it.
func (d *DB) Close() error {
	if d != nil && d.owner != nil {
		d.owner.forget(d)
	}
	return d.close()
}

// close is [DB.Close] without the node's bookkeeping, for the node's own paths,
// which have already taken the handle out of the set under its lock.
func (d *DB) close() error {
	if d == nil || d.sql == nil || !d.closed.CompareAndSwap(false, true) {
		return nil
	}
	// THE REPLICATED ESTATE FIRST, and its error is reported even when
	// this one also fails: a node that closed one of its files and returned
	// the other's error would leave a lock held with nothing naming it.
	//
	// The node is marked closed above BEFORE the slot's lock is taken here,
	// which is what [DB.stillOpen] relies on to refuse an open that would
	// land after the slot was emptied.
	var errs []error
	if d.slot != nil {
		d.slot.mu.Lock()
		held := d.slot.open.Swap(nil)
		d.slot.mu.Unlock()
		if held != nil {
			if err := held.close(); err != nil {
				errs = append(errs, fmt.Errorf("store: close the replicated estate: %w", err))
			}
		}
	}
	// THE RESERVE BEFORE THE POOL. database/sql will not finish closing
	// while a connection is still checked out, so a held reserve would
	// turn every Close into a wait for a connection nobody is going to
	// hand back.
	d.releaseIdentityReserve()
	errs = append(errs, d.sql.Close())
	d.lock.release()
	return errors.Join(errs...)
}

// Estate names which of a node's two files this handle is on: the node's own,
// or the replicated estate.
//
// The empty estate on a handle that is not open, which is the meaningful zero:
// a caller asking which file it is holding while there is none is asking about
// no file. See [ErrNoEstate] for why nil is a state rather than a bug.
func (d *DB) Estate() Estate {
	if d == nil {
		return ""
	}
	return d.estate
}

// Caps reports what the live driver can do, as this handle's open read it: the
// driver's answers, measured once per process because they are a property of
// the compiled-in driver version ([driverMeasurement]), and the file's page
// cache, read at this open. Nothing re-measures them per query.
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

// Scratch reports whether this handle's file is DISCARDED at every open: a
// node's own database opened with [Options.Scratch], which nothing written
// here outlives. It is how the handle was opened, and stays so after
// [DB.Close]; false on a nil handle and on every handle [OpenNode] did not
// open — a replicated estate is never discarded.
//
// THE HANDLE'S OWN ANSWER, because it is the one reading that cannot disagree
// with what the open did: a caller that needs to know whether what it writes
// here survives a restart — the reconciler, which tells the fleet what this
// node would boot on — reads it here rather than deciding it again from Tier A,
// where a second reading is how one of them comes to believe a node keeps what
// it writes.
func (d *DB) Scratch() bool {
	if d == nil {
		return false
	}
	return d.opened.Scratch
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

// LearnEmbeddingDim records the width the first time this handle meets one. It
// only ever raises the width from 0: the width describes the vectors this FILE
// holds, so no apply may change it (the engine refuses a revision of another
// width, see buildEmbedder). A plain setter would defeat that guard in two
// steps: drop the embeddings provider and apply (width 0, guard off), then add
// it back at another width, leaving two widths in the recall pool.
//
// Zero to non-zero, once, is what a node that booted on no revision needs: its
// first apply says how wide its vectors will be. Every other case is correct at
// open ([OpenBackends]). A width of 0 is "no declared width": [DB.EncodeVector]
// checks nothing against it. See TestVectorDimensionUnconfigured.
func (d *DB) LearnEmbeddingDim(width int) {
	// A HANDLE THAT IS NOT OPEN LEARNS NOTHING: this reports no error, so the
	// no-op is the answer. See [ErrNoEstate]: an apply in flight when an
	// adoption closes the replicated estate reaches here legitimately.
	if d == nil {
		return
	}
	if width <= 0 {
		return
	}
	d.dim.CompareAndSwap(0, int64(width))
	// BOTH FILES THE NODE HOLDS LEARN IT: a node that knew with an estate
	// that did not would leave the guard on for one and off for the other,
	// the two-widths state this method prevents. An estate opened later
	// gets the node's width ([DB.replicatedOptions]).
	//
	// THE SLOT IS READ UNDER ITS LOCK, because [DB.OpenReplicated] reads the
	// width and publishes the handle under it: otherwise an open that took
	// the width before the store above and published after this read found
	// the slot empty stays at width 0, guard off for good. The call is made
	// outside the lock; it only raises an atomic.
	if d.slot != nil {
		d.slot.mu.Lock()
		held := d.slot.open.Load()
		d.slot.mu.Unlock()
		if held != nil {
			held.LearnEmbeddingDim(width)
		}
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
//
// It answers that connector beside the pool, for the one connection a handle
// opens outside it ([DB.connector]).
func openPool(path string, busy time.Duration,
	wrap func(driver.Driver) driver.Driver,
) (*sql.DB, *connector, error) {
	// sql.Open is lazy — it validates the driver name and nothing else — so
	// this costs no I/O and exists only to reach the registered driver
	// value, which database/sql offers no other accessor for.
	probeHandle, err := sql.Open(driverName, path)
	if err != nil {
		return nil, nil, fmt.Errorf("store: driver %q: %w", driverName, err)
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

	conns := &connector{
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
			// THE SAME FIGURE ON EVERY CONNECTION OF EITHER FILE, the
			// replicated estate's pinned writers included: a session
			// pragma is the one place every connection passes, and a
			// pragma sent through the pool would reach whichever
			// connection answered and no other. The figure, and what the
			// cache is for, are [nodeCacheKiB]'s; negative is KiB.
			fmt.Sprintf("PRAGMA cache_size = %d", -nodeCacheKiB),
			// TEMPORARY STORAGE IS MEMORY: the default is a per-connection
			// temp FILE, made at the first BEGIN IMMEDIATE (every write,
			// begin.go) and removed only when its connection closes, so exits
			// that close nothing leaked one per connection and a missing
			// TMPDIR refused every write. Nothing here declares a TEMP
			// object. Sorts, hash joins and ephemeral tables past the buffer
			// now stay in memory; the plan gates keep constant walks from
			// sorting (storetest.Sorts). The driver reads the location from
			// the environment alone (TURSO_TMPDIR, SQLITE_TMPDIR, TMPDIR), so
			// no per-store directory exists. VACUUM INTO (backup, state-log
			// snapshot) still writes there: [tempDirRefused] names the variable.
			"PRAGMA temp_store = MEMORY",
			fmt.Sprintf("PRAGMA busy_timeout = %d", busy.Milliseconds()),
		},
	}
	return sql.OpenDB(conns), conns, nil
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
// IT HOLDS THE FILE'S WRITE LOCK FROM ITS BEGIN, queued behind every earlier
// write transaction (see writequeue.go), so fn reads a snapshot no other writer
// can advance and writers are served in arrival order. A transaction that only
// reads belongs in [DB.Read], which queues behind nothing.
//
// A PANIC rolls back, hands the connection back and re-panics: a connection
// returned mid-transaction refuses the next BEGIN, and one never returned is
// lost to the pool, so either way one bug wedges the process. The rollback
// error is discarded: fn's error is what the caller needs.
func (d *DB) Tx(ctx context.Context, fn func(*sql.Tx) error) (err error) {
	// A CONFLICTED TRANSACTION IS RETRIED, so fn may run more than once: each
	// attempt is rolled back first and fn sees a fresh snapshot. A fn with
	// side effects OUTSIDE the transaction (a publish, an in-memory counter)
	// must make them idempotent or move them out.
	//
	// What is left to retry, since BEGIN takes the write lock
	// ([beginModeDriver]), is losing the race for the lock (an honest wait at
	// BEGIN) and a connection the pool handed back dirty.
	//
	// THE NIL GUARD IS HERE, NOT ONLY IN [DB.txOpts]: `budget(d.busy, d.pool)`
	// is an argument, evaluated before the call it guards, so a nil handle
	// dereferenced before that guard ran. See [ErrNoEstate].
	if !d.isOpen() {
		return ErrNoEstate
	}
	return retryTransient(ctx, budget(d.busy, d.pool), func() error { return d.tx(ctx, fn) })
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
// ONE LOOP: [DB.Tx], [DB.Read] and [Writer.Tx] all need it, and a second copy
// is how one comes to classify an error another retries.
//
// A LOCK IT NEVER TOOK IS ANSWERED AS [ErrBusy] once its budget is spent: the
// one cause whose exhaustion is still a wait that clears. A dirty connection
// that survived every attempt (a pool with no clean one) and a stale snapshot
// (never retried) stay the driver's error.
func retryTransient(ctx context.Context, b txBudget, once func() error) error {
	for attempt := 0; ; attempt++ {
		err := once()
		cause := classify(err)
		if err == nil || cause == causeFatal || attempt+1 >= b.attempts(cause) {
			if cause == causeLockTimeout {
				return fmt.Errorf("%w: %w", ErrBusy, err)
			}
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
func budget(busy time.Duration, pool int) txBudget {
	return txBudget{
		attempts: func(c txCause) int {
			switch c {
			case causeDirtyConn:
				return dirtyAttempts(pool)
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
// TWO, one retry, anchored rather than picked: this error only EXISTS after a
// full [Options.busyTimeout] has elapsed, so one retry gives the holder ten
// seconds, the dashboard's query timeout that [defaultBusyTimeout] is half of.
// A writer unreleased in ten seconds is a stuck batch, and the honest answer is
// the error. More attempts stall: eight five-second waits with eight replays of
// a four-thousand-row apply is the fleet-wide stall the applier's occupancy
// model exists to bound.
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

// dirtyAttempts is how many times a transaction that drew a dirty connection
// is tried, on a handle whose pool holds pool connections.
//
// THE BOUND IS THE POOL, DERIVED rather than written down: each attempt
// RETIRES the connection it drew, so the worst case is every dirty connection
// the pool can hold, plus one for the fresh one after all are retired. A
// constant justified by a count is wrong the day the count changes.
//
// pool is [Options.poolSize]. Over-counting the pinned writers' and reserve's
// connections, which never sit idle, costs an attempt, not a write. Each
// attempt costs a reconnect and a [txRetryBeat] pause; [lockAttempts] bounds
// the seconds.
func dirtyAttempts(pool int) int {
	return max(pool, 1) + 1
}

// txRetryBeat is the jittered, WIDENING pause between attempts.
//
// Jittered because the conflict returns with no wait of its own, so back to back
// retries re-collide and spend the budget in microseconds. Widening because
// with a fixed window every loser of a round contends again at the same
// density; the attempt count is the only estimate of writers still fighting.
// The base is one local write transaction committing (microseconds), so even
// the last ceiling is a pause a caller never notices.
func txRetryBeat(attempt int) time.Duration {
	const base = 1_000 // microseconds
	spread := base << min(attempt, 5)
	return time.Duration(base+rand.N(spread)) * time.Microsecond
}

// txCause is why an attempt failed, in the three kinds that need three
// different answers.
//
// It replaces a bool: "conflicted", "starved for the lock" and "handed back a
// dirty connection" are three facts, and one retryable/not answer retried a
// FIVE-SECOND lock wait eight times on a one-millisecond backoff, forty seconds
// of stall with eight full replays of the batch.
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
		// Retryable because the attempt that met it RETIRED the connection
		// it drew ([giveBack]), so the next draws a different one.
		// database/sql hands out the connection it freed LAST, so merely
		// returning the dirty one drew it straight back every time.
		//
		// This package leaves no such connection behind, so this clause
		// meets somebody else's raw transaction on [DB.SQL] left open, or,
		// on a [Writer], its own previous attempt's retired one.
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
// A HANDLE THAT IS NOT OPEN IS AN ERROR, NOT A CRASH, and it is a state a
// caller can legitimately be holding: [DB.ReplicatedDB] answers none while the
// replicated estate is not open, and its database taken for one operation is
// closed under its taker when an adoption closes it between its rename and its
// reopen, or when [DB.Close] or [DB.CloseReplicated] runs. Both are
// documented and deliberate — so the honest answer to a read issued
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
	conn, err := d.conn(ctx)
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

// holdIdentityReserve draws the connection [identityReserve] keeps back.
//
// EVERY HANDLE HOLDS ONE: the reserve is added to the pool ([Options.poolSize]),
// so no bound a caller sets is too small to keep it.
//
// A DRAW THAT FAILS IS NOT AN OPEN THAT FAILS. The reserve is a guarantee under
// load, not a precondition for serving: until drawn, identity reads queue with
// everybody else, and the first one draws it again ([DB.identityConn]). It is
// logged, not returned, so a node does not go down for a reason that costs
// nobody an answer.
func (d *DB) holdIdentityReserve(ctx context.Context) {
	// UNDER THE LOCK although nothing else can reach this handle yet: the
	// field is read under it everywhere else, and a write outside it is
	// the kind of "safe because nobody is looking" the race detector
	// cannot check and a later caller cannot rely on.
	d.reserve.mu.Lock()
	defer d.reserve.mu.Unlock()
	d.reserve.held = true
	conn, err := d.sql.Conn(ctx)
	if err != nil {
		log.WarnContext(ctx, "store_identity_reserve_unheld",
			"estate", string(d.estate), "error", err,
			"hint", "identity reads will queue with every other reader on "+
				"this node until the reserved connection can be drawn")
		return
	}
	d.reserve.conn, d.reserve.retire = conn, retireSwitch(conn)
}

// identityConn is the reserved connection, drawn afresh if the last was
// retired or the draw at the open failed, or nil when no draw succeeds or
// Close has released the reserve.
//
// Caller holds d.reserve.mu.
func (d *DB) identityConn(ctx context.Context) *sql.Conn {
	if d.reserve.conn != nil {
		return d.reserve.conn
	}
	if !d.reserve.held || d.closed.Load() {
		return nil
	}
	// BOUNDED BY THE BUSY TIMEOUT and stripped of the caller's
	// cancellation, exactly as [Writer.replace] is and for the same
	// reason: re-drawing is cleanup after a failure that is often the
	// cancellation itself, and a context with the cancellation taken off
	// has no deadline either.
	bounded, stop := context.WithTimeout(context.WithoutCancel(ctx), d.busy)
	defer stop()
	conn, err := d.sql.Conn(bounded)
	if err != nil {
		return nil
	}
	d.reserve.conn, d.reserve.retire = conn, retireSwitch(conn)
	return conn
}

// releaseIdentityReserve closes the held connection, for [DB.Close].
func (d *DB) releaseIdentityReserve() {
	d.reserve.mu.Lock()
	defer d.reserve.mu.Unlock()
	d.reserve.held = false
	if d.reserve.conn == nil {
		return
	}
	_ = d.reserve.conn.Close()
	d.reserve.conn, d.reserve.retire = nil, nil
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
// One body for every transaction, pooled or pinned; [DB.Read] is the only
// caller passing non-nil opts. nil is the WRITE mode, as in
// [beginModeConn.Begin]: a default that took the deferred begin would put the
// read-then-write abort back on whichever path forgot to ask.
// It reports whether the connection is still FIT, false whenever the
// transaction may still be open on it: a refused BEGIN, a failed rollback, a
// failed commit (the driver keeps it open, as SQLite does for a deferred
// constraint). Telling harmless cases apart would be a text match over driver
// errors; retiring a clean connection costs one reconnect.
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
		// A CONTEXT THAT ENDED MID-TRANSACTION leaves database/sql's own rollback
		// closing this connection on its own time, so it is never handed out again.
		return rollback(ctx, tx) && ctx.Err() == nil, err
	}
	if err := tx.Commit(); err != nil {
		// A CONTEXT THAT ENDED is the cause: database/sql rolls the
		// transaction back when it does, and the commit then reports only
		// sql.ErrTxDone, which no caller can tell from a fault.
		if cause := ctx.Err(); cause != nil {
			return false, fmt.Errorf("store: commit: %w", cause)
		}
		return false, fmt.Errorf("store: commit: %w", err)
	}
	return true, nil
}

// rollback undoes an attempt and reports whether it did.
//
// A discarded rollback error is not harmless: database/sql pools the
// connection when the transaction ends, so after a failed rollback the next
// caller's BEGIN fails with "cannot start a transaction within a transaction".
// The driver does not answer ErrBadConn, so nothing retires the connection; on
// a PINNED one every later attempt failed and the domain stopped applying.
//
// So the answer is the CALLER'S to act on ([giveBack] repairs it), and the
// failure is still logged at WARN for the operator.
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
// TAKEN AT THE DRAW, which is why this is safe. [sql.Conn.Raw] races a
// concurrent close of the same connection (see [beginModeConn.IsValid]), and
// the one moment nothing can be closing it is before any transaction has begun
// on it: the only closer is a transaction's awaitDone goroutine, and this
// goroutine alone holds the handle. So the switch is captured here and thrown
// later, never reached for later.
//
// A NIL SWITCH IS A DRIVER THAT CANNOT BE RETIRED (a wrapper that hid the
// interface): the caller just hands the connection back.
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
// The switch was captured at the draw ([retireSwitch]); throwing it is an
// atomic store, and the discard happens inside the Close below, where
// database/sql asks the driver's connection whether it is still valid.
//
// THE HAND-BACK IS LOAD-BEARING ON ITS OWN, and is called FROM A DEFER at
// every call site: a body that panicked unwinds past the return path, and a
// pooled connection never handed back is lost to database/sql for the life of
// the process, so enough panicking bodies wedge the whole pool.
func giveBack(conn *sql.Conn, retire func(), fit bool) {
	if !fit && retire != nil {
		retire()
	}
	_ = conn.Close()
}

// isOpen reports a handle a statement may be issued through: one that was
// opened and has not been closed. See [ErrNoEstate] for why the other answer
// is a state rather than a fault.
func (d *DB) isOpen() bool { return d != nil && d.sql != nil && !d.closed.Load() }

// conn draws one connection, and answers [ErrNoEstate] when the draw failed
// because the handle was closed while the caller was on its way in — the race
// [DB.closed] exists for, which the guard at the top of every entry point
// cannot close by itself: the handle can be closed between that check and
// this draw.
func (d *DB) conn(ctx context.Context) (*sql.Conn, error) {
	conn, err := d.sql.Conn(ctx)
	if err != nil && d.closed.Load() {
		return nil, fmt.Errorf("%w: %s was closed while this call was in flight", ErrNoEstate, d.path)
	}
	return conn, err
}
