package tracker

import (
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/objstore"
)

// declared is a statement the object store builds from this package's own
// declaration, exactly as the collector, its audit and the backup build it.
func declared(t *testing.T, build func() (string, error)) string {
	t.Helper()
	statement, err := build()
	if err != nil {
		t.Fatalf("the declaration builds no statement: %v", err)
	}
	return statement
}

// THE FILE INDEXES SERVE THE QUERIES THEY NAME, read off the planner for
// [TestEveryIndexServesARegisteredQuery]'s reason: a comment naming a reader is
// a claim, and the plan is what checks it. The object reads are the ones that
// matter most — the collector asks which of a batch of keys any file names
// once per batch of the store's listing, and the audit and the backup page
// through every named key — and without the key's index each is every file in
// the company.
func TestTheFileIndexesServeTheirQueries(t *testing.T) {
	t.Parallel()
	db := planStore(t)
	key := func(i byte) string {
		return objstore.KeyAt(time.Date(2026, 10, 6, 0, 0, 0, 0, time.UTC).Add(time.Duration(i) * time.Hour)).String()
	}
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
			// THE DECLARATION'S OWN STATEMENTS, built exactly as the
			// collector builds them, over a batch of three — and a page
			// of the walk the audit and the backup take.
			"a batch's references", "tracker_files", "tracker_files_object_idx",
			declared(t, func() (string, error) { return FileObjectReferences.ObjectsAmong(3) }),
			[]any{key(1), key(2), key(3)},
		},
		{
			"a page of every reference", "tracker_files", "tracker_files_object_idx",
			declared(t, func() (string, error) { return FileObjectReferences.ReferencesAfter(500) }),
			[]any{key(4)},
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
// wrong one is still "used" — as a SCAN of every entry, which is every file
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
