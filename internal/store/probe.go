package store

// probed splits a read of one row past a page into the page and whether that
// row was there.
//
// THE EVIDENCE ROW. A read that stops at its limit answers the same rows from a
// set of exactly that many and from a set of a thousand, so a caller counting
// the page reports the page as the set. A read that uses this asks the database
// for `limit+1` rows, and this is where the extra one stops being an answer and
// becomes a fact: it is dropped from the page and reported as "there is more".
//
// One helper rather than the three lines written out at each read, because the
// three lines are where the idiom goes wrong: a read that forgets to drop the
// probe hands a caller `limit+1` rows, and one that compares with `>=` reports a
// set of exactly `limit` as cut.
func probed[T any](rows []T, limit int) ([]T, bool) {
	if len(rows) > limit {
		return rows[:limit], true
	}
	return rows, false
}
