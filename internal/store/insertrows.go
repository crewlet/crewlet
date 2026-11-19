package store

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
)

// Execer is the one method [InsertRows] needs of whatever it writes through.
//
// It is declared here, in the package that CALLS it, rather than taking a
// *sql.Tx: an applier hands it a transaction, but the drain benchmark hands it
// a counter, and the statement count is the load-independent half of what
// BenchmarkLogApplyDrain claims. *sql.Tx, *sql.DB and *sql.Conn all satisfy
// it already.
type Execer interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
}

// InsertRows writes n rows through as few multi-row INSERTs as the parameter
// limit allows, and is THE way an applier writes a collection.
//
// The statement comes in three parts: the PREFIX (through VALUES), ONE
// parenthesised row template, and the SUFFIX after the value list, normally an
// ON CONFLICT clause. Parsing SQL for the value list breaks on a CASE, subquery
// or string literal in the template. The bind width comes from args(0), not
// from counting '?'; a row binding another width is a programming error and
// says so. n <= 0 writes nothing and asks the database nothing.
func InsertRows(ctx context.Context, tx Execer, maxVariables int,
	prefix, row, suffix string, n int, args func(i int) []any) (int, error) {

	return insertChunks(maxVariables, prefix, row, suffix, n, args, func(stmt string, binds []any) (int, error) {
		res, err := tx.ExecContext(ctx, stmt, binds...)
		if err != nil {
			return 0, err
		}
		rows, err := res.RowsAffected()
		if err != nil {
			return 0, fmt.Errorf("store: read the effect of %s: %w", prefix, err)
		}
		return int(rows), nil
	})
}

// Querier is the one method [InsertRowsReturning] needs of whatever it writes
// through, declared here for [Execer]'s reason. *sql.Tx, *sql.DB and
// *sql.Conn all satisfy it.
type Querier interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
}

// InsertRowsReturning is [InsertRows] for a statement whose suffix ends in a
// RETURNING clause: each chunk is QUERIED rather than executed, scan is handed
// every row a chunk answers, and it answers how many it handed.
//
// It serves the writer that must know WHICH rows it wrote: under ON CONFLICT DO
// NOTHING a chunk's count says how many were skipped, never which. One
// statement a row would answer that but costs too much against
// statelog.ApplyTxTimeBudget (3 904 rows took 240 ms of its 250).
//
// scan reads the current row of rows and must not advance it; an error from
// it ends the insert, the chunk's rows closed.
func InsertRowsReturning(ctx context.Context, tx Querier, maxVariables int,
	prefix, row, suffix string, n int, args func(i int) []any, scan func(*sql.Rows) error) (int, error) {

	return insertChunks(maxVariables, prefix, row, suffix, n, args, func(stmt string, binds []any) (int, error) {
		rows, err := tx.QueryContext(ctx, stmt, binds...)
		if err != nil {
			return 0, err
		}
		handed := 0
		for rows.Next() {
			if err := scan(rows); err != nil {
				_ = rows.Close()
				return 0, err
			}
			handed++
		}
		if err := rows.Err(); err != nil {
			_ = rows.Close()
			return 0, err
		}
		return handed, rows.Close()
	})
}

// insertChunks builds the multi-row statements of n rows — prefix, row
// repeated as many times as the engine's parameter limit allows, suffix — and
// hands each chunk's statement and binds to run, answering the sum of what
// run answers.
func insertChunks(maxVariables int, prefix, row, suffix string, n int, args func(i int) []any,
	run func(stmt string, binds []any) (int, error)) (int, error) {

	if n <= 0 {
		return 0, nil
	}
	first := args(0)
	columns := len(first)
	if columns == 0 {
		return 0, fmt.Errorf("store: %s binds no parameters per row", prefix)
	}

	// The statement text depends only on how many rows the chunk carries,
	// and every chunk but the last carries the same number — so at most two
	// distinct statements are built for a collection of any size.
	built := make(map[int]string, 2)
	statement := func(rows int) string {
		if s, ok := built[rows]; ok {
			return s
		}
		s := InsertStatement(prefix, row, suffix, rows)
		built[rows] = s
		return s
	}

	written := 0
	binds := make([]any, 0, RowsPerInsert(maxVariables, columns)*columns)
	for start, end := range Chunks(n, maxVariables, columns) {
		binds = binds[:0]
		for i := start; i < end; i++ {
			a := first
			if i > 0 {
				a = args(i)
			}
			if len(a) != columns {
				return 0, fmt.Errorf(
					"store: row %d of %s binds %d parameters, row 0 binds %d: "+
						"every row of a multi-row insert has to bind the same width",
					i, prefix, len(a), columns)
			}
			binds = append(binds, a...)
		}
		rows, err := run(statement(end-start), binds)
		if err != nil {
			return 0, err
		}
		written += rows
	}
	return written, nil
}

// InsertStatement is the statement [InsertRows] issues for a chunk of rows:
// the prefix, the row template that many times, and the suffix. It is
// exported so a caller's plan test can explain the statement its collection
// write actually issues rather than a copy assembled beside it.
func InsertStatement(prefix, row, suffix string, rows int) string {
	var b strings.Builder
	b.Grow(len(prefix) + rows*(len(row)+1) + len(suffix) + 1)
	b.WriteString(prefix)
	for i := range rows {
		if i > 0 {
			b.WriteString(",")
		}
		b.WriteString(row)
	}
	if suffix != "" {
		b.WriteString(" ")
		b.WriteString(suffix)
	}
	return b.String()
}
