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

// InsertRows writes n rows through as few multi-row INSERT statements as the
// engine's parameter limit allows, and it is THE way an applier writes a
// collection.
//
// # Why this exists rather than a loop at each call site
//
// [RowsPerInsert] and [Chunks] are the arithmetic, and for a long time they
// were the WHOLE of it: nothing in the engine called them. Every applier in
// the tree wrote its child rows one ExecContext per row — the shape
// BenchmarkLogApplyDrain names "unprepared" and measures as the SLOWEST of
// the three — while [Writer.Conn]'s doc comment, the drain benchmark and
// docs/guides/replication.md all described the multi-row shape as the one the
// engine ships. It was not shipped anywhere. This is the function that makes
// those three statements true, and it lives here because the chunker does:
// written once per applier it would be five copies of one rule, which is the
// arrangement [textcut] and [whsec] exist to record the cost of.
//
// # The statement is given in three parts, not one
//
// A caller hands the PREFIX (through the VALUES keyword), ONE parenthesised
// row template, and whatever SUFFIX follows the value list — normally an
// ON CONFLICT clause. The parts are separate because the row template is what
// gets repeated, and the alternative is parsing SQL to find the value list:
// a row template here carries a CASE, a scalar subquery and string literals,
// so "find the parentheses after VALUES" is a rule that works until the first
// applier that needs one of those.
//
// The bind width is taken from args(0) rather than by counting '?' in the
// template, because the argument builder is authoritative and a '?' inside a
// string literal is not a parameter. Every row must bind the same width; one
// that does not is a programming error and says so rather than silently
// shifting every later row's arguments by one.
//
// n <= 0 writes nothing and asks the database nothing, which is what makes
// "write the child rows" safe to call on a record that has none.
func InsertRows(ctx context.Context, tx Execer, maxVariables int,
	prefix, row, suffix string, n int, args func(i int) []any) (int, error) {

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
		s := b.String()
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
		res, err := tx.ExecContext(ctx, statement(end-start), binds...)
		if err != nil {
			return 0, err
		}
		rows, err := res.RowsAffected()
		if err != nil {
			return 0, fmt.Errorf("store: read the effect of %s: %w", prefix, err)
		}
		written += int(rows)
	}
	return written, nil
}
