package store

import (
	"context"
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"os"
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

// Estate names one of a node's two databases.
//
// # Why there are two
//
// A node's own estate and the estate its appliers replicate are different
// things under every reading, and the boundary was already drawn in prose
// before it was drawn in the filesystem:
//
//   - A SNAPSHOT is a copy of the replicated estate. Taken from one file it
//     is `VACUUM INTO` of everything followed by a DELETE of every local
//     table on the copy — and Turso has no in-place VACUUM, so the copy keeps
//     the deleted pages as free pages and the transfer, the checksum and the
//     integrity check all pay for them. The largest of those tables is the
//     audit event log, with every phase's prompt in its payload.
//   - The IDENTITY CLAIM a peer verifies a snapshot against is a checksum
//     over the replicated tables, and nothing else may be in the way of it.
//   - The APPLIER's write cadence is its own: in one file every audit insert
//     shares a WAL, a checkpoint and an fsync queue with the applier's
//     commits, whatever the driver's conflict granularity turns out to be.
//
// NO TRANSACTION SPANS THE TWO and no read joins across them. A transaction
// is one file, which is also why the tables an applier writes for its own
// bookkeeping live with the rows it writes beside them rather than with the
// node's other local state.
type Estate string

const (
	// EstateNode is the node's own: the audit event log, learning and
	// memory, config revisions, the bootstrap secret store.
	EstateNode Estate = "node"

	// EstateReplicated is everything a state log's applier writes.
	EstateReplicated Estate = "replicated"
)

// Estates are the two, in the order [Open] brings them up.
var Estates = []Estate{EstateNode, EstateReplicated}

// migrateMu serialises migration runs across every handle in the process.
//
// The store's file lock admits one process per file (see lock.go), so no
// other process can race this one's DDL and the migrator needs no lock of its
// own inside the database. What is left is two handles on one path inside
// this binary, which share the process's claim, and a package-level mutex
// closes that completely. Migrations run once at Open, so the contention is
// nil.
var migrateMu sync.Mutex

// migrate applies every embedded schema file that has not been applied yet, in
// filename order, and returns the versions it applied.
//
// Forward-only, one transaction per file, with the schema_migrations row
// written inside that transaction — so a file is either fully applied and
// recorded, or neither. Turso makes DDL transactional, which is what lets a
// whole file go in as one statement batch, with nothing splitting it on ';'.
func (d *DB) migrate(ctx context.Context) ([]string, error) {
	migrateMu.Lock()
	defer migrateMu.Unlock()

	if _, err := d.sql.ExecContext(ctx, `
		CREATE TABLE IF NOT EXISTS schema_migrations (
			version    TEXT    NOT NULL PRIMARY KEY,
			applied_at INTEGER NOT NULL
		)`); err != nil {
		return nil, fmt.Errorf("store: create schema_migrations: %w", err)
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
		body, err := schemaFS.ReadFile(path.Join("schema", string(d.estate), name))
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
	body, err := schemaFS.ReadFile(path.Join("schema", string(estate), name))
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
// base filename and each estate has its own table, so `0001` in one estate and
// `0001` in the other are two migrations and neither can mask the other.
func schemaVersions(estate Estate) ([]string, error) {
	entries, err := fs.ReadDir(schemaFS, path.Join("schema", string(estate)))
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

// Pending reports, for EACH of a node's two estates, the schema files its
// database has not applied — WITHOUT applying them.
//
// # Why this is not Open followed by a comparison
//
// Open migrates. That is the right default — every process that touches the
// store gets a current schema without an operator remembering a step — but
// it makes "what would this apply" unanswerable through it: by the time you
// could ask, the answer is none.
//
// # It makes no database
//
// A database that does not exist has applied nothing, and is reported so
// without being made: no database file, no -wal and no lock sidecar is
// created on a path the check was only pointed at. The deploy gate this
// answers for runs before the node does, possibly as another user, and every
// file it made would be one the node then meets as somebody else's — or a
// database at a path nobody meant.
//
// A database that exists but has no schema_migrations table has applied
// nothing too, which is what a fresh deployment looks like rather than an
// error. One whose table is there and cannot be read is an error like any
// other read — never an answer of "nothing applied", which would report a
// database that has applied everything as one that has applied nothing.
//
// Reading a database that exists leaves beside it what any open of it does:
// its lock's sidecar if it had none, which the lock's release leaves in place
// (see [fileLock.release]), and its -wal, made owner-only before the driver
// opens it (see filemode.go).
//
// # It takes the lock, and gives it back
//
// Reading is still a second process on a file this engine owns exclusively,
// and the answer to that is [ErrLocked] naming the holder — not an opaque
// driver error, and not a silent read of a file somebody is writing. Without
// it `crewlet migrate -check` would report the schema of a live engine's
// database and refuse only where it tried to change it, and the check that
// runs first would be the one with no guard. It is asked even where there is
// no database to read, of a sidecar already standing: a process that holds it
// is an engine on this path, which can make the database the moment this
// reports it absent.
//
// The lock is released before returning, and NOT because [Open] would
// otherwise be refused — it would not. The claim is refcounted per process
// (see lock.go), so `crewlet migrate` calling Pending and then Open shares one
// claim either way, and a Pending that never released would look perfectly
// fine from inside that command.
//
// It is released because a claim this process no longer needs is a claim it
// must not keep: the lock lives as long as the process, so a leak here would
// leave the file excluded from every OTHER process for the rest of this one's
// life, with nothing to point at. That is why the test asserts the refcount
// rather than a following Open — an Open that succeeds proves nothing.
func Pending(ctx context.Context, path string, opts Options) ([]Schema, error) {
	out := make([]Schema, 0, len(Estates))
	for _, estate := range Estates {
		file := path
		if estate == EstateReplicated {
			file = ReplicatedPath(path, opts.ReplicatedPath)
		}
		one, err := pendingOne(ctx, estate, file, opts)
		if err != nil {
			return nil, err
		}
		out = append(out, one)
	}
	return out, nil
}

// Schema is one estate's migration state.
type Schema struct {
	// Estate is which of a node's two databases this describes.
	Estate Estate
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
// node's own: a snapshot being adopted. That file is a replicated estate and
// nothing else, and it must be inspected BEFORE anything migrates it — a
// recipient refuses a donor whose migrations this binary does not carry, and
// migrating first would answer the question by changing it.
func PendingEstate(ctx context.Context, estate Estate, path string, opts Options) (Schema, error) {
	return pendingOne(ctx, estate, path, opts)
}

func pendingOne(ctx context.Context, estate Estate, path string, opts Options) (Schema, error) {
	out := Schema{Estate: estate, Path: path}
	files, err := schemaVersions(estate)
	if err != nil {
		return Schema{}, err
	}

	lock, err := lockExisting(path)
	if err != nil {
		return Schema{}, err
	}
	// A CLOSURE, because the claim below can replace lock: `defer
	// lock.release()` would bind the nil a database with no sidecar gets here,
	// and the claim that made the sidecar would never be given back.
	defer func() { lock.release() }()

	switch _, statErr := os.Stat(path); {
	case errors.Is(statErr, os.ErrNotExist):
		out.Pending = files
		return out, nil
	case statErr != nil:
		return Schema{}, fmt.Errorf("store: read the %s estate at %s: %w", estate, path, statErr)
	}
	if lock == nil {
		// The database is there and its sidecar is not: the lock is taken
		// the way every opener takes it, which makes the sidecar.
		if lock, err = lockStore(path); err != nil {
			return Schema{}, err
		}
	}

	pool, err := openPrepared(ctx, path, opts.forEstate(estate))
	if err != nil {
		return Schema{}, err
	}
	defer func() { _ = pool.Close() }()

	db := &DB{sql: pool, path: path, estate: estate,
		busy: opts.busyTimeout(), writes: lock.queue()}
	if out.Applied, err = db.recordedVersions(ctx); err != nil {
		return Schema{}, err
	}
	have := make(map[string]bool, len(out.Applied))
	for _, v := range out.Applied {
		have[v] = true
	}
	for _, file := range files {
		if !have[file] {
			out.Pending = append(out.Pending, file)
		}
	}
	return out, nil
}

// recordedVersions is [DB.appliedVersions] for a database that may never have
// been migrated: one with no schema_migrations table has applied nothing and
// answers so, while a table that is there and cannot be read is an error like
// any other read.
//
// ASKED OF THE SCHEMA, not inferred from the read failing: a failed read is
// every fault a database can have, and "no such table" is only one of them.
func (d *DB) recordedVersions(ctx context.Context) ([]string, error) {
	var tables int
	if err := d.sql.QueryRowContext(ctx, `SELECT count(*) FROM sqlite_master
		WHERE type = 'table' AND name = 'schema_migrations'`).Scan(&tables); err != nil {
		return nil, fmt.Errorf("store: look for schema_migrations in %s: %w", d.path, err)
	}
	if tables == 0 {
		return nil, nil
	}
	return d.appliedVersions(ctx)
}
