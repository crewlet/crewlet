package store_test

import (
	"context"
	"database/sql"
	"path/filepath"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/store"
)

// countingExec counts the statements an insert actually issues, which is the
// load-independent half of what the chunker claims.
type countingExec struct {
	inner store.Execer
	n     int
	sizes []int
}

func (c *countingExec) ExecContext(ctx context.Context, query string,
	args ...any) (sql.Result, error) {

	c.n++
	c.sizes = append(c.sizes, strings.Count(query, "("))
	return c.inner.ExecContext(ctx, query, args...)
}

const rowsTable = `CREATE TABLE ins_rows (
	a TEXT NOT NULL,
	b TEXT NOT NULL,
	c INTEGER NOT NULL,
	PRIMARY KEY (a, b)
)`

func insertRowsStore(t *testing.T) (*store.DB, *store.Writer) {
	t.Helper()
	db, w := openApplierStore(t, filepath.Join(t.TempDir(), "insrows.db"))
	t.Cleanup(func() { _ = w.Close() })
	t.Cleanup(func() { _ = db.Close() })
	rep := replicatedOf(t, db)
	if err := w.Tx(t.Context(), func(tx *sql.Tx) error {
		_, err := tx.ExecContext(t.Context(), rowsTable)
		return err
	}); err != nil {
		t.Fatalf("create the table: %v", err)
	}
	return rep, w
}

// A COLLECTION BECOMES ceil(n/size) STATEMENTS, NOT n OF THEM — which is the
// whole of what the chunker buys and the only part of it that a loaded machine
// cannot move. The wall clock is measured by BenchmarkLogApplyDrain; this is
// the invariant.
func TestInsertRowsIssuesOneStatementPerChunk(t *testing.T) {
	t.Parallel()
	_, w := insertRowsStore(t)
	ctx := t.Context()

	// A deliberately small limit, so the chunk boundary is exercised by a
	// row count a test can read rather than by the 1 000 rows a statement
	// the row cap allows at the probed limit.
	const limit, columns, rows = 10, 3, 17
	size := store.RowsPerInsert(limit, columns) // 3
	if size != 3 {
		t.Fatalf("RowsPerInsert(%d, %d) = %d, want 3", limit, columns, size)
	}
	wantStatements := (rows + size - 1) / size // 6

	var count *countingExec
	if err := w.Tx(ctx, func(tx *sql.Tx) error {
		count = &countingExec{inner: tx}
		written, err := store.InsertRows(ctx, count, limit,
			`INSERT INTO ins_rows (a, b, c) VALUES`, `(?,?,?)`, "",
			rows, func(i int) []any { return []any{"k", string(rune('a' + i)), i} })
		if err != nil {
			return err
		}
		if written != rows {
			t.Errorf("wrote %d rows, want %d", written, rows)
		}
		return nil
	}); err != nil {
		t.Fatalf("insert: %v", err)
	}
	if count.n != wantStatements {
		t.Errorf("%d rows at %d per statement took %d statements, want %d — "+
			"the chunker is not chunking, and every read-latency bound in the "+
			"design is derived from this number", rows, size, count.n, wantStatements)
	}
}

// NOTHING IS ASKED OF THE DATABASE FOR AN EMPTY COLLECTION, which is what makes
// "write the child rows" safe to call on a record that has none.
func TestInsertRowsWritesNothingForNoRows(t *testing.T) {
	t.Parallel()
	_, w := insertRowsStore(t)
	ctx := t.Context()
	for _, n := range []int{0, -1} {
		var count *countingExec
		if err := w.Tx(ctx, func(tx *sql.Tx) error {
			count = &countingExec{inner: tx}
			written, err := store.InsertRows(ctx, count, 2000,
				`INSERT INTO ins_rows (a, b, c) VALUES`, `(?,?,?)`, "",
				n, func(int) []any { t.Fatal("args called for an empty collection"); return nil })
			if written != 0 {
				t.Errorf("n=%d wrote %d rows, want 0", n, written)
			}
			return err
		}); err != nil {
			t.Fatalf("n=%d: %v", n, err)
		}
		if count.n != 0 {
			t.Errorf("n=%d issued %d statements, want 0", n, count.n)
		}
	}
}

// THE CONFLICT CLAUSE SURVIVES THE EXPANSION, including when the two rows that
// collide are in the SAME statement — which a per-row loop could never stage
// and is therefore the case chunking introduces.
func TestInsertRowsAppliesTheConflictClauseWithinOneChunk(t *testing.T) {
	t.Parallel()
	rep, w := insertRowsStore(t)
	ctx := t.Context()

	type row struct {
		b string
		c int
	}
	// Two rows collide on (a, b) inside one chunk; the later one wins.
	rows := []row{{"x", 1}, {"y", 2}, {"x", 3}}
	if err := w.Tx(ctx, func(tx *sql.Tx) error {
		_, err := store.InsertRows(ctx, tx, 2000,
			`INSERT INTO ins_rows (a, b, c) VALUES`, `(?,?,?)`,
			`ON CONFLICT (a, b) DO UPDATE SET c = excluded.c`,
			len(rows), func(i int) []any { return []any{"k", rows[i].b, rows[i].c} })
		return err
	}); err != nil {
		t.Fatalf("insert: %v", err)
	}

	var got int
	if err := rep.Read(ctx, func(tx *sql.Tx) error {
		return tx.QueryRowContext(ctx,
			`SELECT c FROM ins_rows WHERE a = 'k' AND b = 'x'`).Scan(&got)
	}); err != nil {
		t.Fatalf("read back: %v", err)
	}
	if got != 3 {
		t.Errorf("c = %d, want 3 — the last write of a colliding pair inside one "+
			"chunk has to win, exactly as it does one statement per row", got)
	}
}

// countingQuery counts the statements a returning insert issues.
type countingQuery struct {
	inner store.Querier
	n     int
}

func (c *countingQuery) QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error) {
	c.n++
	return c.inner.QueryContext(ctx, query, args...)
}

// A RETURNING INSERT HANDS OVER ONLY THE ROWS IT WROTE, one statement a chunk:
// under ON CONFLICT DO NOTHING a row that collides with one already stored is
// skipped and never handed to scan, the rows of every chunk are handed in
// the order given, and the count is of the rows handed — which is what a
// writer that announces each row it wrote (internal/code's notices) needs and
// a chunk's RowsAffected cannot say.
//
// Mutation: execute each chunk rather than query it, or hand over every row
// given rather than every row answered, and the rows handed are not these.
func TestInsertRowsReturningHandsOverOnlyTheRowsItWrote(t *testing.T) {
	t.Parallel()
	_, w := insertRowsStore(t)
	ctx := t.Context()
	given := []string{"a", "b", "c", "d", "e"}
	var handed []string
	count := 0
	statements := 0
	if err := w.Tx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `INSERT INTO ins_rows (a, b, c) VALUES ('k', 'b', 0)`); err != nil {
			return err
		}
		q := &countingQuery{inner: tx}
		// SIX VARIABLES of three columns a row: two rows a chunk, three
		// chunks for five rows, the stored one's collision in the first.
		n, err := store.InsertRowsReturning(ctx, q, 6,
			`INSERT INTO ins_rows (a, b, c) VALUES`, `(?,?,?)`, `ON CONFLICT (a, b) DO NOTHING RETURNING b`,
			len(given), func(i int) []any { return []any{"k", given[i], i + 1} },
			func(rows *sql.Rows) error {
				var b string
				if err := rows.Scan(&b); err != nil {
					return err
				}
				handed = append(handed, b)
				return nil
			})
		count, statements = n, q.n
		return err
	}); err != nil {
		t.Fatalf("insert: %v", err)
	}
	if want := []string{"a", "c", "d", "e"}; strings.Join(handed, ",") != strings.Join(want, ",") || count != 4 {
		t.Errorf("the insert handed %v and counted %d, want %v and 4: the stored row's collision is "+
			"skipped, and every row written is handed once", handed, count, want)
	}
	if statements != 3 {
		t.Errorf("five rows of three columns under six variables issued %d statements, want 3", statements)
	}
}

// A RAGGED BIND WIDTH IS REFUSED NAMING THE ROW, because the alternative is
// every later row's arguments silently shifted by one.
func TestInsertRowsRefusesARaggedRow(t *testing.T) {
	t.Parallel()
	_, w := insertRowsStore(t)
	ctx := t.Context()
	err := w.Tx(ctx, func(tx *sql.Tx) error {
		_, err := store.InsertRows(ctx, tx, 2000,
			`INSERT INTO ins_rows (a, b, c) VALUES`, `(?,?,?)`, "",
			3, func(i int) []any {
				if i == 2 {
					return []any{"k", "short"}
				}
				return []any{"k", string(rune('a' + i)), i}
			})
		return err
	})
	if err == nil {
		t.Fatal("a row binding the wrong width was accepted")
	}
	for _, want := range []string{"row 2", "binds 2 parameters", "row 0 binds 3"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not name %q", err, want)
		}
	}
}
