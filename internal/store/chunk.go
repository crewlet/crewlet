package store

// RowsPerInsert is how many rows a multi-row INSERT may carry when each row
// binds columns parameters.
//
// The bound is probed, not assumed: it is the engine's, not the dialect's
// (SQLite's moved from 999 to 32 766), so [Capabilities.MaxVariables] is
// measured against the live driver at open.
//
// Chunking because one INSERT per row is a round trip and a plan per row (the
// drain benchmark measures it). A row binding no parameters (columns <= 0)
// takes the whole batch in one statement.
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
// A statement's TEXT grows with its rows (a "(?,?,…)" group each), so a
// wide-open chunk builds a six-figure statement to save round trips that were
// never the cost, and holds more memory while assembled. At 1 000 rows the
// round-trip saving has flattened: the next order of magnitude buys 0.09 %.
//
// ON THE PINNED DRIVER IT IS WHAT BINDS, at every width an applier uses:
// [Capabilities.MaxVariables] reports the probe's 32 766 ceiling, which alone
// would allow 4 680 rows at seven columns and 16 383 at two.
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
