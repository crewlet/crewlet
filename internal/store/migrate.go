package store

import (
	"context"
	"database/sql"
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"path"
	"slices"
	"strconv"
	"strings"

	turso "turso.tech/database/tursogo"

	"github.com/crewlet/crewlet/internal/words"
)

// schemaFS carries the schema into the binary: one file, no data directory.
// ONE SEQUENCE PER ESTATE, in a directory named for it. `all:` carries a
// directory whose sequence is still empty, so a name that does not resolve is a
// read error at Open rather than a sequence that silently applies nothing.
//
// A NODE MIGRATION NEVER DROPS A COLUMN IN PLACE. The driver's
// `ALTER TABLE … DROP COLUMN` re-parses every trigger, and node 0041's triggers
// name `NEW.rowid`, which it refuses ("no such column: rowid") whatever table.
// So copy the table without the column, drop the old one, rename the copy, as
// node 0045 does.
//
//go:embed all:schema
var schemaFS embed.FS

// Estate names one of a node's two databases.
//
// Two files because a SNAPSHOT copies the replicated estate, and from one file
// that is `VACUUM INTO` then a DELETE of the local tables: Turso has no in-place
// VACUUM, so the free pages (the audit log is the largest) cost transfer,
// checksum and integrity check. The IDENTITY CLAIM a peer verifies is a
// checksum over the replicated tables alone, and the applier's commits would
// share a WAL and fsync queue with every audit insert.
//
// NO TRANSACTION SPANS THE TWO and no read joins across them, so an applier's
// bookkeeping tables live with the rows it writes beside them.
type Estate string

const (
	// EstateNode is the node's own: the audit event log, learning and
	// memory, config revisions, the bootstrap secret store.
	EstateNode Estate = "node"

	// EstateReplicated is everything a state log's applier writes.
	EstateReplicated Estate = "replicated"
)

// Estates are the two, in the order a node brings them up.
var Estates = []Estate{EstateNode, EstateReplicated}

// Valid reports whether e is one of the two. The zero value is not: an
// estate nobody named is not quietly the node's.
func (e Estate) Valid() bool { return e == EstateNode || e == EstateReplicated }

// migrate applies every embedded schema file that has not been applied yet, in
// filename order, and returns the versions it applied.
//
// Forward-only, one transaction per file with the schema_migrations row written
// inside it, so a file is fully applied and recorded or neither. Turso's DDL is
// transactional, so a file goes in as one batch with no ';' splitting.
//
// One run per FILE at a time: the only race is two handles on ONE PATH in this
// binary, one reading a ledger the other has not finished writing and failing
// at CREATE TABLE; a per-file transaction does not close it. So the lock is the
// path's, on this process's claim ([fileLock]). A process-wide mutex would queue
// every store's migrations behind other files' DDL that cannot race its own.
func (d *DB) migrate(ctx context.Context) ([]string, error) {
	running := d.lock.migrations()
	running.Lock()
	defer running.Unlock()

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
	last := 0
	for _, name := range files {
		if slices.Contains(applied, name) {
			last = migrationNumber(name)
			continue
		}
		body, err := schemaFS.ReadFile(path.Join("schema", string(d.estate), name))
		if err != nil {
			return nil, fmt.Errorf("store: read schema %s: %w", name, err)
		}
		if err := d.applyOne(ctx, name, string(body)); err != nil {
			return nil, &MigrationError{Estate: d.estate, Version: name, From: last,
				Failure: migrationFailureOf(err), Err: err}
		}
		last = migrationNumber(name)
		done = append(done, name)
		log.InfoContext(ctx, "schema_applied", "estate", string(d.estate), "version", name)
	}
	return done, nil
}

// MigrationError is a numbered schema file this binary could not apply: which
// one, on which estate, after which, and the driver's class of refusal. Closed
// values beside the driver's own words, which stay in Err, so a report may name
// the failure without carrying a word of the data that caused it.
type MigrationError struct {
	Estate Estate
	// Version is the file's name, as `schema_migrations` keys it.
	Version string
	// From is the number of the last file applied before it, 0 for none.
	From    int
	Failure MigrationFailure
	Err     error
}

func (e *MigrationError) Error() string { return e.Err.Error() }

// Unwrap is the driver's error.
func (e *MigrationError) Unwrap() error { return e.Err }

// MigrationFailure is the driver's class of a refused migration.
type MigrationFailure string

// The classes a refused migration is read as.
const (
	// MigrationConstraint is a constraint the rows already held refused.
	MigrationConstraint MigrationFailure = "constraint"
	// MigrationStatement is a statement the driver refused as written.
	MigrationStatement MigrationFailure = "statement"
	// MigrationMisuse is the driver's API used against its contract.
	MigrationMisuse MigrationFailure = "misuse"
	// MigrationOther is any other refusal of the engine's own.
	MigrationOther MigrationFailure = "other"
	// MigrationEnvironment is the host's or the moment's: a busy or read-only
	// file, a full or corrupt one, a cancelled start. No defect of the engine's.
	MigrationEnvironment MigrationFailure = "environment"
)

// MigrationFailures are the classes a migration of the engine's own failed by,
// the environment's left out.
func MigrationFailures() []MigrationFailure {
	return []MigrationFailure{MigrationConstraint, MigrationStatement, MigrationMisuse, MigrationOther}
}

// migrationFailureOf reads a refusal by the driver's own marks, never its words.
func migrationFailureOf(err error) MigrationFailure {
	switch {
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded),
		errors.Is(err, turso.ErrTursoBusy), errors.Is(err, turso.ErrTursoInterrupt),
		errors.Is(err, turso.ErrTursoReadOnly), errors.Is(err, ErrFull),
		errors.Is(err, turso.ErrTursoNotADb), errors.Is(err, turso.ErrTursoCorrupt),
		errors.Is(err, turso.ErrTursoConnClosed), errors.Is(err, sql.ErrConnDone):
		return MigrationEnvironment
	case errors.Is(err, turso.ErrTursoConstraint):
		return MigrationConstraint
	case errors.Is(err, turso.ErrTursoMisuse):
		return MigrationMisuse
	case errors.Is(err, turso.ErrTursoGeneric):
		return MigrationStatement
	}
	return MigrationOther
}

// migrationNumber is a schema file's leading number, 0 for a name with none.
func migrationNumber(name string) int {
	digits, _, _ := strings.Cut(name, "_")
	n, err := strconv.Atoi(digits)
	if err != nil {
		return 0
	}
	return n
}

// createMigrationLedger creates `schema_migrations`, the table every applied
// file is recorded in, if the database has none yet.
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
	if !d.isOpen() {
		return nil, ErrNoEstate
	}
	return d.appliedVersions(ctx)
}

// ForeignMigrations is every schema version this database has recorded that
// this binary does not carry, in order: a newer build's, applied before this
// node was rolled back to an older one. The migration runner steps over them
// (it applies what this binary carries and nothing else), so the node boots,
// and what it cannot do is read the tables they made — which may hold rows
// that matter to a caller about to act on "nothing names this", the object
// store's collector above all.
func (d *DB) ForeignMigrations(ctx context.Context) ([]string, error) {
	applied, err := d.AppliedMigrations(ctx)
	if err != nil {
		return nil, err
	}
	carried, err := schemaVersions(d.estate)
	if err != nil {
		return nil, err
	}
	var foreign []string
	for _, v := range applied {
		if !slices.Contains(carried, v) {
			foreign = append(foreign, v)
		}
	}
	return foreign, nil
}

// appliedVersions reads the ledger through ONE CONNECTION DRAWN FROM THIS
// HANDLE ([DB.conn]), for [DB.vacuumInto]'s reason: a replicated estate closed
// after [DB.AppliedMigrations]'s guard answers [ErrNoEstate] rather than
// database/sql's "database is closed".
func (d *DB) appliedVersions(ctx context.Context) ([]string, error) {
	conn, err := d.conn(ctx)
	if err != nil {
		return nil, fmt.Errorf("store: read schema_migrations: %w", err)
	}
	defer func() { _ = conn.Close() }()
	rows, err := conn.QueryContext(ctx,
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
// base filename and each file has its own table, so `0001` in one estate and
// `0001` in the other are two migrations and neither can mask the other.
func schemaVersions(estate Estate) ([]string, error) {
	if !estate.Valid() {
		return nil, fmt.Errorf("store: %q is not an estate; the estates are %s", estate, words.And(Estates))
	}
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

// Pending reports the schema files a node's two databases have not applied —
// its own file at path, and its replicated estate where [ReplicatedPath] puts
// it for opts — WITHOUT applying them.
//
// Opening would migrate, so this opens the pool and reads, creating nothing. No
// schema_migrations table means nothing applied (a fresh deployment), not an
// error. It takes the lock, [ErrLocked] naming the holder, and releases it
// before returning: the claim lives as long as the process, so a leak would
// exclude every OTHER process, and the test asserts the refcount (a following
// open proves nothing). Both files, always, or a deploy gate passes while the
// other is behind; its caller refuses a scratch store.
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

// Schema is one file's migration state.
type Schema struct {
	// Estate is which of the node's two files this describes.
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
// nothing else, and it must be inspected BEFORE anything migrates it — a recipient
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

	pool, conns, err := openConnected(ctx, path, Options{
		MaxOpenConns: opts.MaxOpenConns, BusyTimeout: opts.BusyTimeout,
		WrapDriver: opts.WrapDriver,
	})
	if err != nil {
		return Schema{}, err
	}
	defer func() { _ = pool.Close() }()

	db := &DB{sql: pool, connector: conns, path: path, estate: estate,
		busy: opts.busyTimeout(), pool: opts.poolSize(),
		writes: lock.queue()}
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
