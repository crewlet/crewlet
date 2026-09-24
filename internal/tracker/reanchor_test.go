package tracker_test

import (
	"database/sql"
	"testing"

	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/tracker"
)

// A REANCHOR'S VERSION RESET RUNS TO THE END, OVER THE ROWS THE COMPANY HAS.
//
// It walks a list of the tracker's object tables, and a table on the list
// without a `version` column fails the first statement against it — which
// fails the whole transition at its fourth step, on every reanchor of this log,
// before it has moved anything. Three such tables were listed.
//
// Mutation: put `tracker_comments` back on the list and the reset fails with
// no such column.
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
