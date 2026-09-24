package tracker_test

import (
	"database/sql"
	"fmt"
	"testing"

	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/tracker"
)

// A REANCHOR'S VERSION RESET RUNS TO THE END, OVER THE ROWS THE COMPANY HAS.
//
// It walks a list of the tracker's object tables, and a table on the list
// without a `version` column fails the first statement against it — which
// fails the whole transition at its fourth step, on every reanchor of this log,
// before it has moved anything.
//
// Mutation: put `tracker_comments` on the list and the reset fails with no
// such column.
func TestAReanchorsVersionResetRunsOverEveryRow(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	task := newTask("t-1")
	task.Key = ""
	if _, err := r.writer.CreateTask(t.Context(), "op-1", task, nil); err != nil {
		t.Fatalf("CreateTask: %v", err)
	}
	r.drain()

	if err := tracker.ResetVersions(t.Context(), r.db.Replicated(), 2); err != nil {
		t.Fatalf("ResetVersions: %v", err)
	}
	var version int64
	if err := r.db.Replicated().Read(t.Context(), func(tx *sql.Tx) error {
		return tx.QueryRowContext(t.Context(),
			`SELECT version FROM tracker_tasks WHERE id = 't-1'`).Scan(&version)
	}); err != nil {
		t.Fatalf("read the task's version: %v", err)
	}
	if want := int64(2 * statelog.GenerationStride); version != want {
		t.Fatalf("the task's version is %d after the reset, want generation 2's "+
			"floor %d", version, want)
	}
}

// A REANCHOR'S VERSION RESET LEAVES A TASK'S BODY HISTORY ALONE.
//
// `tracker_body_revisions` has a `version` column that is not a position: it
// is the body version a revision holds, and half the table's primary key. A
// reset writes one floor value into every row it reaches, so a task with two
// revisions fails the primary key — and the transition stops at its fourth
// step on every attempt — while a task with one has its revision renumbered to
// a body version it never had.
//
// The revisions arrive the way every row here does, on a record the applier
// applies: a patch carrying the body it replaced.
//
// Mutation: put `tracker_body_revisions` on the reset's list and the reset
// fails on the two-revision task.
func TestAReanchorsVersionResetLeavesBodyRevisionsAlone(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	for i, id := range []string{"t-twice", "t-once"} {
		task := newTask(id)
		task.Key = ""
		task.Body = id + " as it was created"
		if _, err := r.writer.CreateTask(t.Context(), "op-create-"+id, task,
			nil); err != nil {
			t.Fatalf("CreateTask %s: %v", id, err)
		}
		r.drain()
		// ONE REVISION PER EDIT, each holding the body the edit replaced
		// at the version that body had.
		edits := 2 - i
		for version := 1; version <= edits; version++ {
			body := fmt.Sprintf("%s after edit %d", id, version)
			if _, err := r.writer.UpdateTask(t.Context(),
				fmt.Sprintf("op-edit-%s-%d", id, version), id, "ENG", 0,
				tracker.TaskPatch{
					Body: &body,
					BodyRevision: &tracker.BodyRevision{
						Task: id, Version: version, At: wednesday,
						Body: fmt.Sprintf("the body %s held at version %d", id, version),
					},
				}, tracker.ChangeFields, nil); err != nil {
				t.Fatalf("edit %s to body version %d: %v", id, version+1, err)
			}
			r.drain()
		}
	}
	revisions := func() map[string][]int64 {
		t.Helper()
		out := map[string][]int64{}
		if err := r.db.Replicated().Read(t.Context(), func(tx *sql.Tx) error {
			rows, err := tx.QueryContext(t.Context(), `
				SELECT task_id, version FROM tracker_body_revisions
				ORDER BY task_id, version`)
			if err != nil {
				return err
			}
			defer func() { _ = rows.Close() }()
			for rows.Next() {
				var task string
				var version int64
				if err := rows.Scan(&task, &version); err != nil {
					return err
				}
				out[task] = append(out[task], version)
			}
			return rows.Err()
		}); err != nil {
			t.Fatalf("read the body revisions: %v", err)
		}
		return out
	}
	want := map[string][]int64{"t-twice": {1, 2}, "t-once": {1}}
	if got := revisions(); fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("before the reset the revisions are %v, want %v — the case "+
			"is about a history this harness did not build", got, want)
	}

	if err := tracker.ResetVersions(t.Context(), r.db.Replicated(), 2); err != nil {
		t.Fatalf("ResetVersions over a task with two body revisions: %v", err)
	}
	if got := revisions(); fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("after the reset the revisions are %v, want %v unchanged — a "+
			"revision's version is the body version it holds, not a position",
			got, want)
	}
}
