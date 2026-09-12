package store

// RowsPerInsert is how many rows a multi-row INSERT may carry when each row
// binds columns parameters.
//
// # Why the bound is probed rather than assumed
//
// A statement's parameters are bounded by the engine, and the number is the
// engine's rather than the dialect's: SQLite's default has been 32 766 since
// 3.32 and was 999 before it, and Turso's is a third answer neither of those
// predicts. [Capabilities.MaxVariables] is measured against the live driver at
// open, which is the only way to be right on all three.
//
// # Why chunking at all
//
// The tables an applier fills — a closure, exploded child rows, dependency
// edges, a candidate fan-out — take many rows per record, and one INSERT per
// row is one round trip and one plan per row. A multi-row INSERT is one of
// each for the whole chunk, and it is the difference the drain benchmark
// measures.
//
// A row that binds no parameters at all takes the whole batch in one
// statement, and columns below zero is treated the same way: there is nothing
// to bound.
func RowsPerInsert(maxVariables, columns int) int {
	if columns <= 0 {
		return maxBatchRows
	}
	rows := maxVariables / columns
	if rows < 1 {
		// A SINGLE ROW EVEN WHEN IT DOES NOT FIT, because the alternative
		// is returning zero and having the caller loop forever writing
		// nothing. A row wider than the engine's parameter limit is
		// refused by the engine, naming the statement — which is a
		// better failure than a silent stall.
		return 1
	}
	if rows > maxBatchRows {
		return maxBatchRows
	}
	return rows
}

// maxBatchRows caps a chunk regardless of how many parameters would fit.
//
// A statement's TEXT grows with its rows — each is a "(?,?,…)" group the
// engine parses — so a 32 000-row insert of a two-column table builds a
// 160 KB statement to save round trips that were never the cost. It also
// bounds the memory one chunk holds while it is being assembled. 1 000 rows
// is where the round-trip saving has already flattened: it is 1/1000th of the
// per-row overhead, and the next order of magnitude buys a further 0.09 %.
const maxBatchRows = 1000

// Chunks splits n rows into contiguous [start, end) ranges of at most
// RowsPerInsert(maxVariables, columns) rows.
//
// It yields nothing for n <= 0, which is what makes "write the child rows"
// safe to call on a record that has none.
func Chunks(n, maxVariables, columns int) func(func(int, int) bool) {
	size := RowsPerInsert(maxVariables, columns)
	return func(yield func(int, int) bool) {
		for start := 0; start < n; start += size {
			end := min(start+size, n)
			if !yield(start, end) {
				return
			}
		}
	}
}
