package worklet

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/objstore"
	"github.com/crewlet/crewlet/internal/objstore/collect"
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
// once per batch of the store's listing, the audit and the backup page
// through every named key, and the backup asks which of the keys written
// while it ran the copy names — and without the key's index each is every
// file in the company.
//
// AT EVERY WIDTH THEIR CALLERS BUILD THEM AT, from the callers' own
// constants: each width is a statement text of its own — n placeholders, a
// LIMIT of n — which the driver plans on its own, so a question planned at
// three keys certifies nothing about the five hundred the collector asks, and
// a driver that traded five hundred seeks for a scan of the table would pass
// it. A question of a list of keys is planned at its widest, where a planner
// would first make that trade, and at the one key a short last batch narrows
// it to, the form a planner may read as a plain equality. The declaration's
// fourth statement, a backup's page of the records its copy deferred, reads
// the log's deferred records by their position rather than a file index, so
// it is not this test's.
//
// PLANNED OVER A COMPANY'S FILES ([filePlanStore]) and with no statistics, as
// a deployment plans them ([seededPlanStore]).
func TestTheFileIndexesServeTheirQueries(t *testing.T) {
	t.Parallel()
	db := filePlanStore(t)
	key := func(i int) string {
		return objstore.KeyAt(filesFrom.Add(time.Duration(i) * time.Minute)).String()
	}
	// keys is a question's n keys, one for each of its placeholders.
	keys := func(n int) []any {
		out := make([]any, n)
		for i := range out {
			out[i] = key(i + 1)
		}
		return out
	}
	type fileRead struct {
		name, index, statement string
		args                   []any
	}
	reads := []fileRead{
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
			// A PAGE OF THE WALK the audit and the backup take, whose
			// LIMIT is always the whole page.
			"a page of every reference", "tracker_files_object_idx",
			declared(t, func() (string, error) {
				return FileObjectReferences.ReferencesAfter(objstore.WalkPage)
			}),
			[]any{key(4)},
		},
	}
	// THE DECLARATION'S OWN QUESTIONS OF A LIST OF KEYS, built exactly as
	// their callers build them: the collector's of a batch of its listing,
	// and the backup's of the keys written while it ran, a walk's page of
	// them at a time.
	for _, n := range []int{1, collect.JudgeBatch} {
		reads = append(reads, fileRead{
			fmt.Sprintf("the collector's batch of %d", n), "tracker_files_object_idx",
			declared(t, func() (string, error) { return FileObjectReferences.ObjectsAmong(n) }),
			keys(n),
		})
	}
	for _, n := range []int{1, objstore.WalkPage} {
		reads = append(reads, fileRead{
			fmt.Sprintf("a backup's question of %d", n), "tracker_files_object_idx",
			declared(t, func() (string, error) { return FileObjectReferences.ReferencesAmong(n) }),
			keys(n),
		})
	}
	for _, c := range reads {
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
