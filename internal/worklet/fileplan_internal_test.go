package worklet

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/objstore"
	"github.com/crewlet/crewlet/internal/store"
	"github.com/crewlet/crewlet/internal/store/storetest"
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
//
// PLANNED OVER A COMPANY'S FILES ([filePlanStore]) and with no statistics, as
// a deployment plans them ([seededPlanStore]).
func TestTheFileIndexesServeTheirQueries(t *testing.T) {
	t.Parallel()
	db := filePlanStore(t)
	key := func(i byte) string {
		return objstore.KeyAt(filesFrom.Add(time.Duration(i) * time.Hour)).String()
	}
	for _, c := range []struct {
		name, index, statement string
		args                   []any
	}{
		{
			"a project's listing", "tracker_files_project_idx",
			`SELECT path, content_type, hash, size, version, created_by,
				created_at, updated_by, updated_at, removed_by, removed_at
			 FROM tracker_files
			 WHERE project_key = ? AND path > ? AND removed_at IS NULL
			 ORDER BY path LIMIT ?`,
			[]any{"P01", "", 201},
		},
		{
			"a folder's listing", "tracker_files_project_idx",
			`SELECT path FROM tracker_files
			 WHERE project_key = ? AND path > ? AND path > ? AND path < ?
			 ORDER BY path LIMIT ?`,
			[]any{"P01", "", "reports/", "reports0", 201},
		},
		{
			// THE DECLARATION'S OWN STATEMENTS, built exactly as the
			// collector builds them, over a batch of three — and a page
			// of the walk the audit and the backup take.
			"a batch's references", "tracker_files_object_idx",
			declared(t, func() (string, error) { return FileObjectReferences.ObjectsAmong(3) }),
			[]any{key(1), key(2), key(3)},
		},
		{
			"a page of every reference", "tracker_files_object_idx",
			declared(t, func() (string, error) { return FileObjectReferences.ReferencesAfter(500) }),
			[]any{key(4)},
		},
	} {
		t.Run(c.name, func(t *testing.T) {
			plan := storetest.Plan(t, db, c.statement, c.args)
			if !storetest.Seeks(plan, c.index) {
				t.Fatalf("this query does not SEEK %s:\n%s", c.index, strings.Join(plan, "\n"))
			}
		})
	}
}

// The files the file reads are planned against.
//
// TWO HUNDRED A PROJECT across the task corpus's thirty, in four folders, so a
// project's listing is a thirtieth of the table and a folder a quarter of that.
// One in [fileRemovedEvery] is removed — stamped, and naming no object, as a
// removal leaves a row — so the listings' `removed_at IS NULL` and the object
// reads' `object IS NOT NULL` each keep most of the table but not all of it.
// The live rows name objects minted a minute apart, across the hours the
// object reads above ask about.
const (
	fileRows         = 200 * projects
	fileRemovedEvery = 8
)

// filesFrom is when the first fixture file was written.
var filesFrom = time.Date(2026, 10, 2, 0, 0, 0, 0, time.UTC)

// filePlanStore is a replicated estate holding [fileRows] files of a
// company's shape.
func filePlanStore(t *testing.T) store.ReplicatedHandle {
	t.Helper()
	return seededPlanStore(t, func(ctx context.Context, tx *sql.Tx, maxVariables int) error {
		return insertAll(ctx, tx, maxVariables, `
			INSERT INTO tracker_files
				(id, project_key, path, content_type, hash, size,
				 created_by, created_at, updated_by, updated_at,
				 removed_by, removed_at, version, document, object)
			VALUES`, `(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`, fileRows, fileRow)
	})
}

// fileRow is the i-th file of the fixture.
func fileRow(i int) []any {
	project := fmt.Sprintf("P%02d", i%projects)
	n := i / projects
	path := fmt.Sprintf("%s/f-%03d.md", []string{"docs", "notes", "reports", "specs"}[n%4], n)
	at := filesFrom.Add(time.Duration(i) * time.Minute)
	removedBy, removedAt, object := "", any(nil), any(objstore.KeyAt(at).String())
	if i%fileRemovedEvery == 0 {
		removedBy, removedAt, object = "ana", store.EncodeTime(at.Add(time.Hour)), nil
	}
	return []any{
		project + "/" + path, project, path, "text/markdown",
		fmt.Sprintf("%064x", i), int64(1024 + i),
		"ana", store.EncodeTime(at), "ana", store.EncodeTime(at),
		removedBy, removedAt, int64(i + 1), []byte(`{}`), object,
	}
}
