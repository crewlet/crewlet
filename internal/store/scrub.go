package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"strings"
)

// ErrScrubTable reports a table a scrub was asked to empty and could not
// account for.
var ErrScrubTable = errors.New("store: unscrubbable table")

// ScrubFile empties the named tables in a database file this process does not
// otherwise hold open, and reports what it emptied.
//
// # What it is for
//
// A node that donates a snapshot ships a copy of its own database, and some
// of what that database holds is THIS NODE'S rather than the fleet's: its
// audit event log, its scheduled-run history, its own retry ledger, its record
// of what it last adopted. None of that is state a recipient should inherit,
// and some of it — the event log, with every phase's prompt in its payload —
// is the largest thing in the file.
//
// THE DONOR SCRUBS, NOT THE RECIPIENT, and the difference is the whole
// argument for the transfer being safe at all: an offered artefact carries no
// authority a peer does not already hold only once every table still in it is
// fleet-visible. Scrubbing after the transfer means the donor shipped it.
//
// # The list is a DECLARATION, never a literal here
//
// Every caller derives it from what the registered domains say about their own
// tables. A hardcoded list would "silently omit whatever a deployment actually
// has", which is this tree's own words for the failure — so this function
// takes the list and REFUSES a name it cannot find, rather than skipping it.
// A table that was renamed and left on somebody's list is a table that travels
// with the artefact and is reported as scrubbed.
//
// # It does not reclaim the pages
//
// The rows go and the pages stay, as free space in the file. That is
// deliberate and it is what the two-estate split buys: the replicated estate
// holds almost nothing local, so the free pages are a rounding error rather
// than the event log's whole footprint. There is no in-place VACUUM on this
// engine to reclaim them with, and copying the copy to reclaim them would cost
// a second full pass over the artefact.
func ScrubFile(ctx context.Context, path string, tables []string) ([]string, error) {
	if path == "" {
		return nil, errors.New("store: scrub: no path")
	}
	for _, name := range tables {
		if !plainIdentifier(name) {
			return nil, fmt.Errorf("%w: %q is not a plain table name, and a "+
				"scrub interpolates it into a statement rather than binding it",
				ErrScrubTable, name)
		}
	}
	pool, err := openPrepared(ctx, path, Options{})
	if err != nil {
		return nil, fmt.Errorf("store: scrub: open %s: %w", path, err)
	}
	defer func() { _ = pool.Close() }()

	present, err := tableNames(ctx, pool)
	if err != nil {
		return nil, err
	}
	scrubbed := make([]string, 0, len(tables))
	for _, name := range tables {
		if !slices.Contains(present, name) {
			// REFUSED RATHER THAN SKIPPED. A name nobody can find is
			// either a table that was renamed — in which case the real
			// one is travelling, and the manifest would claim it was
			// scrubbed — or a list that has drifted from the schema.
			// Both are the failure this list exists to prevent.
			return nil, fmt.Errorf("%w: %s names %q, which this database does "+
				"not have — the artefact would be offered claiming a table was "+
				"emptied that was never there", ErrScrubTable, path, name)
		}
		if _, err := pool.ExecContext(ctx, `DELETE FROM `+name); err != nil {
			return nil, fmt.Errorf("store: scrub %s in %s: %w", name, path, err)
		}
		scrubbed = append(scrubbed, name)
	}
	slices.Sort(scrubbed)

	// A TRUNCATING CHECKPOINT, so the deletions are in the file itself
	// rather than in a sidecar that will not travel with it. The next
	// thing that happens to this path is a checksum and an offer.
	if _, err := pool.ExecContext(ctx, `PRAGMA wal_checkpoint(TRUNCATE)`); err != nil {
		return nil, fmt.Errorf("store: scrub: checkpoint %s: %w", path, err)
	}
	if err := pool.Close(); err != nil {
		return nil, fmt.Errorf("store: scrub: close %s: %w", path, err)
	}
	if err := removeSidecars(path); err != nil {
		return nil, fmt.Errorf("store: scrub: %w", err)
	}
	return scrubbed, nil
}

// EmptyTables reports which of the named tables hold no rows, so a recipient
// can VERIFY a donor's claim rather than trust it.
//
// The verification is a list rather than a memory: the artefact says what it
// emptied, and this answers whether it did.
func EmptyTables(ctx context.Context, path string, tables []string) ([]string, error) {
	if path == "" {
		return nil, errors.New("store: verify scrub: no path")
	}
	for _, name := range tables {
		if !plainIdentifier(name) {
			return nil, fmt.Errorf("%w: %q", ErrScrubTable, name)
		}
	}
	pool, err := openPrepared(ctx, path, Options{})
	if err != nil {
		return nil, fmt.Errorf("store: verify scrub: open %s: %w", path, err)
	}
	defer func() { _ = pool.Close() }()

	present, err := tableNames(ctx, pool)
	if err != nil {
		return nil, err
	}
	var empty []string
	for _, name := range tables {
		if !slices.Contains(present, name) {
			// A table the artefact does not have is empty in every
			// sense the recipient cares about.
			empty = append(empty, name)
			continue
		}
		var n int64
		if err := pool.QueryRowContext(ctx,
			`SELECT COUNT(*) FROM `+name).Scan(&n); err != nil {
			return nil, fmt.Errorf("store: verify scrub: count %s: %w", name, err)
		}
		if n == 0 {
			empty = append(empty, name)
		}
	}
	slices.Sort(empty)
	return empty, nil
}

// tableNames lists the tables a database file actually has.
func tableNames(ctx context.Context, pool *sql.DB) ([]string, error) {
	rows, err := pool.QueryContext(ctx,
		`SELECT name FROM sqlite_master WHERE type = 'table'`)
	if err != nil {
		return nil, fmt.Errorf("store: list tables: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, fmt.Errorf("store: scan a table name: %w", err)
		}
		out = append(out, name)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: iterate the table list: %w", err)
	}
	return out, nil
}

// plainIdentifier is what a table name must be to be interpolated into a
// statement. Deliberately narrower than the engine accepts: the set is what is
// unambiguous rather than what the parser tolerates.
func plainIdentifier(name string) bool {
	if name == "" || len(name) > 64 {
		return false
	}
	for i, r := range name {
		switch {
		case r >= 'a' && r <= 'z', r == '_':
		case r >= '0' && r <= '9' && i > 0:
		default:
			return false
		}
	}
	return !strings.HasPrefix(name, "sqlite_")
}
