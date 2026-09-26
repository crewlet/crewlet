package tracker

import (
	"slices"
	"strings"
	"testing"
)

// THE TWO FILE INDEXES SERVE THE TWO QUERIES THEY NAME, read off the planner
// for [TestEveryIndexServesARegisteredQuery]'s reason: a comment naming a
// reader is a claim, and the plan is what checks it. The chunk read is the one
// that matters most — the object store's passes run it once per placement
// group per pass on every data node, and without its index each run is every
// chunk row of every file in the company.
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
			"one group's references", "tracker_file_chunks", "tracker_file_chunks_pg_idx",
			`SELECT DISTINCT chunk FROM tracker_file_chunks WHERE pg = ?`,
			[]any{17},
		},
	} {
		t.Run(c.name, func(t *testing.T) {
			plan := explain(t, db, c.statement, c.args)
			if !slices.Contains(indexesIn(plan), c.index) || scansHeap(plan, c.table) {
				t.Fatalf("this query does not reach %s:\n%s", c.index, strings.Join(plan, "\n"))
			}
		})
	}
}
