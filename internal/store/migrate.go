package store

import (
	"context"
	"embed"
	"fmt"
	"io/fs"
	"path"
	"slices"
	"strings"
	"sync"
)

// schemaFS carries the consolidated schema into the binary, so a deployment is
// one file with no data directory to keep in step with it.
//
// ONE SEQUENCE PER ESTATE, in a directory named for it. `all:` is what carries
// a directory whose sequence is still empty: an estate's directory must be in
// the binary before its first migration is, so a name that does not resolve is
// a read error at Open rather than a sequence that silently applies nothing.
//
//go:embed all:schema
var schemaFS embed.FS

// Estate names which kind of file a handle is on: the node's own, or one of
// the partitions of the replicated estate the node holds.
//
// # Why they are separate files
//
// A node's own estate and the estate its appliers replicate are different
// things under every reading, and the boundary was already drawn in prose
// before it was drawn in the filesystem:
//
//   - A SNAPSHOT is a copy of a partition. Taken from one file with the node's
//     own tables in it, it would be `VACUUM INTO` of everything followed by a
//     DELETE of every local table on the copy — and Turso has no in-place
//     VACUUM, so the copy keeps the deleted pages as free pages and the
//     transfer, the checksum and the integrity check all pay for them. The
//     largest of those tables is the audit event log, with every phase's
//     prompt in its payload.
//   - The IDENTITY CLAIM a peer verifies a snapshot against is a checksum over
//     the replicated tables, and nothing else may be in the way of it.
//   - The APPLIER's write cadence is its own: in one file every audit insert
//     shares a WAL, a checkpoint and an fsync queue with the applier's
//     commits, whatever the driver's conflict granularity turns out to be.
//
// And the replicated estate is itself a file PER PARTITION, for the first
// reason one level down — see partition.go.
//
// NO TRANSACTION SPANS TWO FILES and no read joins across them. A transaction
// is one file, which is also why the tables an applier writes for its own
// bookkeeping live with the rows it writes beside them rather than with the
// node's other local state.
type Estate string

const (
	// EstateNode is the node's own: the audit event log, learning and
	// memory, config revisions, the bootstrap secret store.
	EstateNode Estate = "node"

	// EstatePartition is one partition of the replicated estate: what a
	// state log's appliers write, for the logs that partition carries.
	EstatePartition Estate = "partition"
)

// Estates are the two kinds, in the order a node brings them up.
var Estates = []Estate{EstateNode, EstatePartition}

// Valid reports whether e is one of the two. The zero value is not: an
// estate nobody named is not quietly the node's.
func (e Estate) Valid() bool { return e == EstateNode || e == EstatePartition }

// schemaDir is the directory under schema/ an estate's migration sequence is
// embedded in.
//
// A PARTITION'S IS replicated/, whatever its space: that sequence holds every
// domain's tables and the framework's, and it is the one sequence this build
// carries for a partition file — layout 0's one file has applied it from the
// start, and a partitioned layout's files carry it whole. ONE SEQUENCE FOR
// EVERY PARTITION means `schema_migrations` keys each file's ledger on the
// bare filename, which is what every layout-0 file already holds.
func (e Estate) schemaDir() string {
	if e == EstatePartition {
		return "replicated"
	}
	return string(e)
}

// migrateMu serialises migration runs across every handle in the process.
//
// It replaces a Postgres advisory lock, and the replacement is smaller than
// the original because the problem is: three OS processes could race the DDL
// there (`crewlet run`, `crewlet run api`, `crewlet config import`), and the
// lock had to be a database object for that reason. Here one process owns the
// file — Turso does not support any other arrangement — so the only race left
// is two handles on one path inside this binary, which a package-level mutex
// closes completely. Migrations run once at Open, so the contention is nil.
var migrateMu sync.Mutex

// migrate applies every embedded schema file that has not been applied yet, in
// filename order, and returns the versions it applied.
//
// Forward-only, one transaction per file, with the schema_migrations row
// written inside that transaction — so a file is either fully applied and
// recorded, or neither. Turso makes DDL transactional, which is what
// lets a whole file go in as one statement batch: the Postgres migrator split
// files on ';' and then had to validate its own naive splitter (dollar-quoted
// bodies would be cut in half). Nothing here needs that.
func (d *DB) migrate(ctx context.Context) ([]string, error) {
	migrateMu.Lock()
	defer migrateMu.Unlock()

	if err := d.createMigrationLedger(ctx); err != nil {
		return nil, err
	}

	applied, err := d.appliedVersions(ctx)
	if err != nil {
		return nil, err
	}

	files, err := schemaVersions(d.estate)
	if err != nil {
		return nil, err
	}

	var done []string
	for _, name := range files {
		if slices.Contains(applied, name) {
			continue
		}
		body, err := schemaFS.ReadFile(path.Join("schema", d.estate.schemaDir(), name))
		if err != nil {
			return nil, fmt.Errorf("store: read schema %s: %w", name, err)
		}
		if err := d.applyOne(ctx, name, string(body)); err != nil {
			return nil, err
		}
		done = append(done, name)
		log.InfoContext(ctx, "schema_applied", "estate", string(d.estate), "version", name)
	}
	return done, nil
}

// createMigrationLedger creates `schema_migrations`, the table every applied
// file is recorded in, if the database has none yet.
//
// ONE STATEMENT WITH TWO CALLERS: [DB.migrate], and the tests that stand a
// database up at an OLDER schema to prove what a later migration does to rows
// written under it. A second copy of this DDL there would be a ledger the real
// migrator might one day read differently.
func (d *DB) createMigrationLedger(ctx context.Context) error {
	if _, err := d.sql.ExecContext(ctx, `
		CREATE TABLE IF NOT EXISTS schema_migrations (
			version    TEXT    NOT NULL PRIMARY KEY,
			applied_at INTEGER NOT NULL
		)`); err != nil {
		return fmt.Errorf("store: create schema_migrations: %w", err)
	}
	return nil
}

func (d *DB) applyOne(ctx context.Context, version, body string) error {
	tx, err := d.sql.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("store: begin %s: %w", version, err)
	}
	defer func() { _ = tx.Rollback() }()

	if _, err := tx.ExecContext(ctx, body); err != nil {
		return fmt.Errorf("store: apply schema %s: %w", version, err)
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO schema_migrations (version, applied_at) VALUES (?, ?)`,
		version, EncodeTime(now()),
	); err != nil {
		return fmt.Errorf("store: record schema %s: %w", version, err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("store: commit schema %s: %w", version, err)
	}
	return nil
}

// AppliedMigrations lists the schema versions this database has recorded, in
// order. Read-only: it creates nothing, so a readiness probe may call it while
// another handle is mid-migration.
func (d *DB) AppliedMigrations(ctx context.Context) ([]string, error) {
	if d == nil || d.sql == nil {
		return nil, ErrNoEstate
	}
	return d.appliedVersions(ctx)
}

func (d *DB) appliedVersions(ctx context.Context) ([]string, error) {
	rows, err := d.sql.QueryContext(ctx,
		`SELECT version FROM schema_migrations ORDER BY version`)
	if err != nil {
		return nil, fmt.Errorf("store: read schema_migrations: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []string
	for rows.Next() {
		var v string
		if err := rows.Scan(&v); err != nil {
			return nil, fmt.Errorf("store: scan schema_migrations: %w", err)
		}
		out = append(out, v)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: iterate schema_migrations: %w", err)
	}
	return out, nil
}

// SchemaFile returns one embedded migration's bytes.
//
// EXPORTED FOR THE ONE ASSERTION THAT IS ABOUT THE SOURCE rather than about
// what the driver created: an index's trailing comment naming the query it
// serves is not part of the schema the database keeps, so a test that read the
// schema back could never see it. Every other schema assertion in the tree
// reads sqlite_master instead, deliberately — a clause a file carries and the
// driver silently ignored would pass a text scan and fail in production.
func SchemaFile(estate Estate, name string) ([]byte, error) {
	body, err := schemaFS.ReadFile(path.Join("schema", estate.schemaDir(), name))
	if err != nil {
		return nil, fmt.Errorf("store: read the %s estate's %s: %w", estate, name, err)
	}
	return body, nil
}

// schemaVersions lists one estate's embedded schema files in application
// order, which is filename order — the numeric prefix is the ordering, and
// there is no second source of truth for it.
//
// The two sequences are numbered INDEPENDENTLY. schema_migrations keys on the
// base filename and each file has its own table, so `0001` in one estate and
// `0001` in the other are two migrations and neither can mask the other.
func schemaVersions(estate Estate) ([]string, error) {
	if !estate.Valid() {
		return nil, fmt.Errorf("store: %q is not an estate; the estates are %v", estate, Estates)
	}
	entries, err := fs.ReadDir(schemaFS, path.Join("schema", estate.schemaDir()))
	if err != nil {
		return nil, fmt.Errorf("store: read the %s estate's embedded schema: %w", estate, err)
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		// Only .sql: `all:` carries whatever else is in the directory,
		// which for an estate with no migrations yet is the file that
		// makes the directory exist at all.
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".sql") {
			names = append(names, e.Name())
		}
	}
	slices.Sort(names)
	return names, nil
}

// SchemaVersions reports one estate's schema files, in application order.
// Exposed for diagnostics and for the test that asserts a fresh database ends
// up with all of them.
func SchemaVersions(estate Estate) []string {
	names, err := schemaVersions(estate)
	if err != nil {
		// The FS is embedded at build time; a read failure means the
		// binary is malformed, and reporting an empty list would let a
		// caller conclude the schema is empty rather than broken.
		panic(fmt.Sprintf("store: the %s estate's embedded schema is unreadable: %v", estate, err))
	}
	return names
}

// Pending reports the schema files a node's databases have not applied —
// its own file at path, and each named partition's where the node keeps it —
// WITHOUT applying them.
//
// # Why this is not an open followed by a comparison
//
// Opening migrates. That is the right default — every process that touches the
// store gets a current schema without an operator remembering a step — but
// it makes "what would this apply" unanswerable through it: by the time you
// could ask, the answer is none.
//
// So this opens the pool and reads, and creates nothing at all. A database
// with no schema_migrations table has applied nothing, which is what a fresh
// deployment looks like rather than an error: reporting a missing table
// would send an operator to investigate the state every new install starts
// in.
//
// # It takes the lock, and gives it back
//
// Reading is still a second process on a file this engine owns exclusively,
// and the answer to that is [ErrLocked] naming the holder — not an opaque
// driver error, and not a silent read of a file somebody is writing. It held
// no lock at all until this was fixed, so `crewlet migrate` reported the
// schema of a live engine's database and only refused at the point it tried
// to change it: the check that runs first was the one with no guard.
//
// The lock is released before returning, and NOT because an open would
// otherwise be refused — it would not. The claim is refcounted per process
// (see lock.go), so `crewlet migrate` calling Pending and then opening shares
// one claim either way, and a Pending that never released would look
// perfectly fine from inside that command.
//
// It is released because a claim this process no longer needs is a claim it
// must not keep: the lock lives as long as the process, so a leak here would
// leave the file excluded from every OTHER process for the rest of this one's
// life, with nothing to point at. That is why the test asserts the refcount
// rather than a following open — an open that succeeds proves nothing.
func Pending(ctx context.Context, path string, opts Options, partitions ...PartitionFile) ([]Schema, error) {
	out := make([]Schema, 0, 1+len(partitions))
	node, err := pendingOne(ctx, EstateNode, path, opts)
	if err != nil {
		return nil, err
	}
	out = append(out, node)
	for _, f := range partitions {
		if err := f.Validate(); err != nil {
			return nil, err
		}
		one, err := pendingOne(ctx, EstatePartition, partitionPath(path, opts.ReplicatedPath, f), opts)
		if err != nil {
			return nil, err
		}
		one.Partition = f.Name
		out = append(out, one)
	}
	return out, nil
}

// Schema is one file's migration state.
type Schema struct {
	// Estate is which kind of file this describes.
	Estate Estate
	// Partition names the partition a partition file is, and is empty for
	// the node's own.
	Partition string
	// Path is the file it lives in.
	Path string
	// Applied are the versions the database has recorded, in order.
	Applied []string
	// Pending are the versions this binary carries and it has not.
	Pending []string
}

// KnownMigrations is every schema version THIS BINARY carries for an estate,
// in application order.
//
// A recipient adopting a snapshot needs it to answer the one question
// [PendingEstate] cannot: not what the artefact is missing, but what the
// artefact has that this binary does not. A file shaped by code this node does
// not run is a file whose rows it cannot reason about.
func KnownMigrations(estate Estate) ([]string, error) {
	return schemaVersions(estate)
}

// PendingEstate reports ONE estate file's applied and pending migrations,
// without migrating it.
//
// It exists for the one caller that holds a database file which is not a
// node's own: a snapshot being adopted. That file is a partition and nothing
// else, and it must be inspected BEFORE anything migrates it — a recipient
// refuses a donor whose migrations this binary does not carry, and migrating
// first would answer the question by changing it.
func PendingEstate(ctx context.Context, estate Estate, path string, opts Options) (Schema, error) {
	return pendingOne(ctx, estate, path, opts)
}

func pendingOne(ctx context.Context, estate Estate, path string, opts Options) (Schema, error) {
	out := Schema{Estate: estate, Path: path}

	lock, err := lockStore(path)
	if err != nil {
		return Schema{}, err
	}
	defer lock.release()

	pool, err := openPrepared(ctx, path, Options{
		MaxOpenConns: opts.MaxOpenConns, BusyTimeout: opts.BusyTimeout,
		WrapDriver: opts.WrapDriver,
	}, nil)
	if err != nil {
		return Schema{}, err
	}
	defer func() { _ = pool.Close() }()

	db := &DB{sql: pool, path: path, estate: estate,
		busy: opts.busyTimeout(), writes: lock.queue()}
	if out.Applied, err = db.appliedVersions(ctx); err != nil {
		// A database that has never been migrated has no
		// schema_migrations table, and that is the ordinary state of a
		// fresh deployment rather than a fault.
		out.Applied = nil
	}
	have := make(map[string]bool, len(out.Applied))
	for _, v := range out.Applied {
		have[v] = true
	}
	files, err := schemaVersions(estate)
	if err != nil {
		return Schema{}, err
	}
	for _, file := range files {
		if !have[file] {
			out.Pending = append(out.Pending, file)
		}
	}
	return out, nil
}
