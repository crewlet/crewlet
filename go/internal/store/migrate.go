package store

import (
	"context"
	"embed"
	"fmt"
	"io/fs"
	"path"
	"slices"
	"sync"
)

// schemaFS carries the consolidated schema into the binary, so a deployment is
// one file with no data directory to keep in step with it.
//
//go:embed schema/*.sql
var schemaFS embed.FS

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
// recorded, or neither. Both drivers make DDL transactional, which is what
// lets a whole file go in as one statement batch: the Postgres migrator split
// files on ';' and then had to validate its own naive splitter (dollar-quoted
// bodies would be cut in half). Nothing here needs that.
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

	files, err := schemaVersions()
	if err != nil {
		return nil, err
	}

	var done []string
	for _, name := range files {
		if slices.Contains(applied, name) {
			continue
		}
		body, err := schemaFS.ReadFile(path.Join("schema", name))
		if err != nil {
			return nil, fmt.Errorf("store: read schema %s: %w", name, err)
		}
		if err := d.applyOne(ctx, name, string(body)); err != nil {
			return nil, err
		}
		done = append(done, name)
		log.InfoContext(ctx, "schema_applied", "version", name)
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

// schemaVersions lists the embedded schema files in application order, which
// is filename order — the numeric prefix is the ordering, and there is no
// second source of truth for it.
func schemaVersions() ([]string, error) {
	entries, err := fs.ReadDir(schemaFS, "schema")
	if err != nil {
		return nil, fmt.Errorf("store: read embedded schema: %w", err)
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		if !e.IsDir() {
			names = append(names, e.Name())
		}
	}
	slices.Sort(names)
	return names, nil
}

// SchemaVersions reports the schema files this build carries, in application
// order. Exposed for diagnostics and for the test that asserts a fresh
// database ends up with all of them.
func SchemaVersions() []string {
	names, err := schemaVersions()
	if err != nil {
		// The FS is embedded at build time; a read failure means the
		// binary is malformed, and reporting an empty list would let a
		// caller conclude the schema is empty rather than broken.
		panic(fmt.Sprintf("store: embedded schema unreadable: %v", err))
	}
	return names
}
