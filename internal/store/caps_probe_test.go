package store

import (
	"path/filepath"
	"strings"
	"testing"
)

// THE PARAMETER LIMIT IS MEASURED, and both halves of the answer are asserted:
// that it clears the floor every engine in this family guarantees, and that a
// statement binding the reported number really does prepare while one binding
// one more does not.
//
// The second half is what makes this a measurement rather than a constant with
// a probe-shaped comment, and it is asked of the statement the number SIZES —
// a multi-row INSERT, one parameter per row of a one-column table — never of
// the probe's own. A probe held only to its own statement passes whatever
// bound that statement happens to meet: `SELECT ?,?,…` met the result-set
// width at 2 000 columns, the probe reported that as the parameter limit, this
// case asked the same statement whether 2 001 prepared, and every applier
// chunked to a sixteenth of the parameters the probe can report, with nothing
// failing.
//
// Mutation: probe with a statement returning a column per parameter again,
// and an insert binding one more than the reported limit prepares.
func TestMaxVariablesIsMeasuredNotAssumed(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	db, err := OpenNode(ctx, filepath.Join(t.TempDir(), "vars.db"), Options{})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer func() { _ = db.Close() }()

	got := db.Caps().MaxVariables
	if got < conservativeMaxVariables {
		t.Fatalf("MaxVariables = %d, want at least the %d every engine in this "+
			"family accepts", got, conservativeMaxVariables)
	}

	if _, err := db.SQL().ExecContext(ctx, `CREATE TABLE vars_probe (v INTEGER)`); err != nil {
		t.Fatalf("create the table the inserts bind into: %v", err)
	}
	binds := func(n int) error {
		stmt, err := db.SQL().PrepareContext(ctx, "INSERT INTO vars_probe (v) VALUES "+
			strings.TrimSuffix(strings.Repeat("(?),", n), ","))
		if err != nil {
			return err
		}
		return stmt.Close()
	}
	if err := binds(got); err != nil {
		t.Errorf("an insert binding the reported %d parameters does not prepare: "+
			"the probe is reporting a limit the driver does not have: %v", got, err)
	}
	// One past the limit must be refused — UNLESS the probe hit its own
	// ceiling, where "one more" says nothing about the driver.
	if got < 32766 && binds(got+1) == nil {
		t.Errorf("an insert binding %d parameters prepares but the probe reported "+
			"%d as the limit: it measured some other bound, and every batch "+
			"built from it is smaller than it needs to be", got+1, got)
	}
	t.Logf("driver parameter limit: %d", got)
}

// THE PAGE CACHE IS READ BACK, because asking for it is not getting it.
func TestThePageCacheIsAppliedOrReported(t *testing.T) {
	t.Parallel()
	db, err := OpenNode(t.Context(), filepath.Join(t.TempDir(), "cache.db"), Options{})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer func() { _ = db.Close() }()

	// 32 MiB is what the session list asks for. A driver that ignored the
	// pragma answers with its own default, which is what this catches.
	if got := db.Caps().PageCacheKiB; got != 32768 {
		t.Errorf("cache_size reads back as %d KiB, want the 32768 the session "+
			"list sets: a driver that ignores the pragma leaves every "+
			"connection on its own default and nothing else would say so", got)
	}
}

// THE CONVERSION IS A PURE FUNCTION, so the rule is asserted without a driver.
//
// Both spellings of cache_size mean a size and they are in different units; a
// caller that compared the raw number against a byte budget would be out by
// the page size in one of the two cases.
func TestPageCacheConversion(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		raw, pageSize, want int
	}{
		"negative is already KiB":          {-32768, 4096, 32768},
		"negative ignores the page size":   {-2048, 8192, 2048},
		"positive is pages":                {100, 4096, 400},
		"positive at a wider page":         {100, 8192, 800},
		"an unreadable page size gives up": {100, 0, 0},
	} {
		t.Run(name, func(t *testing.T) {
			if got := pageCacheKiB(tc.raw, tc.pageSize); got != tc.want {
				t.Errorf("pageCacheKiB(%d, %d) = %d, want %d",
					tc.raw, tc.pageSize, got, tc.want)
			}
		})
	}
}
