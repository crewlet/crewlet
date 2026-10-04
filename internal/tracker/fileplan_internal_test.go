package tracker

import (
	"slices"
	"strings"
	"testing"
)

// chunksAmong is the statement the object store's collector asks which of a
// batch of chunks any file names, built from this package's own declaration.
func chunksAmong(t *testing.T, n int) string {
	t.Helper()
	statement, err := FileChunkReferences.ChunksAmong(n)
	if err != nil {
		t.Fatalf("the declaration builds no statement: %v", err)
	}
	return statement
}

// THE TWO FILE INDEXES SERVE THE TWO QUERIES THEY NAME, read off the planner
// for [TestEveryIndexServesARegisteredQuery]'s reason: a comment naming a
// reader is a claim, and the plan is what checks it. The chunk read is the one
// that matters most — the object store's collector runs it once per batch of
// the store's listing, and without its index each run is every chunk row of
// every file in the company.
func TestTheFileIndexesServeTheirQueries(t *testing.T) {
	t.Parallel()
	db := planStore(t)
	for _, c := range []struct {
		name, table, index, statement string
		args                          []any
	}{
		{
			"a project's listing", "tracker_files", "tracker_files_project_idx",
			`SELECT path, content_type, hash, size, version, created_by,
				created_at, updated_by, updated_at, removed_by, removed_at
			 FROM tracker_files
			 WHERE project_key = ? AND path > ? AND removed_at IS NULL
			 ORDER BY path LIMIT ?`,
			[]any{"ENG", "", 201},
		},
		{
			"a folder's listing", "tracker_files", "tracker_files_project_idx",
			`SELECT path FROM tracker_files
			 WHERE project_key = ? AND path > ? AND path > ? AND path < ?
			 ORDER BY path LIMIT ?`,
			[]any{"ENG", "", "reports/", "reports0", 201},
		},
		{
			// THE DECLARATION'S OWN STATEMENT, built exactly as the
			// collector builds it, over a batch of three.
			"a batch's references", "tracker_file_chunks", "tracker_file_chunks_chunk_idx",
			chunksAmong(t, 3), []any{
				"0000000000000000000000000000000000000000000000000000000000000001",
				"8000000000000000000000000000000000000000000000000000000000000000",
				"ffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff",
			},
		},
	} {
		t.Run(c.name, func(t *testing.T) {
			plan := explain(t, db, c.statement, c.args)
			if !seeks(plan, c.table, c.index) {
				t.Fatalf("this query does not SEEK %s:\n%s", c.index, strings.Join(plan, "\n"))
			}
		})
	}
}

// seeks reports a plan that SEARCHES table through index — a seek on the
// index's leading columns.
//
// REACHING THE INDEX IS NOT ENOUGH, which is what this used to check: every
// query here is covered by its index, so an index whose columns lead with the
// wrong one is still "used" — as a SCAN of every entry, which is every chunk
// row in the company read in index order rather than heap order, and no
// cheaper. `SEARCH` is the planner saying it will seek.
func seeks(plan []string, table, index string) bool {
	for _, line := range plan {
		if strings.HasPrefix(line, "SEARCH "+table+" ") &&
			slices.Contains(indexesIn([]string{line}), index) {
			return true
		}
	}
	return false
}
