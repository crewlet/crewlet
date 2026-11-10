package worklet_test

import (
	"database/sql"
	"slices"
	"testing"

	"github.com/crewlet/crewlet/internal/worklet"
)

// recipients is every handle a notice row names on the task, sorted.
func (r *roundTrip) recipients(task string) []string {
	r.t.Helper()
	var out []string
	if err := r.db.Read(r.t.Context(), func(tx *sql.Tx) error {
		rows, err := tx.QueryContext(r.t.Context(),
			`SELECT DISTINCT recipient FROM tracker_notifications WHERE task_id = ? ORDER BY recipient`, task)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var h string
			if err := rows.Scan(&h); err != nil {
				return err
			}
			out = append(out, h)
		}
		return rows.Err()
	}); err != nil {
		r.t.Fatalf("read the notice rows: %v", err)
	}
	return out
}

// A CREDENTIAL IS NEVER A NOTICE'S RECIPIENT. A token that files an item and
// watches it is its reporter and its watcher, and has no Inbox and no turn, so
// a notice to it is read by nobody: every change on the item tells its
// assignee and its other watchers, and no row names the token, whatever the
// reason it would have heard under. The item still names the token as its
// reporter.
//
// Mutation: drop the writer's filter, or let any recipient field through, and
// this goes red.
func TestACredentialIsNeverANoticesRecipient(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	const token = "token:ci"
	filer := r.writer.As(token, worklet.AuthorOperator, worklet.Provenance{})
	task := newTask("t-1")
	task.Assignee, task.Reporter = "bo", token
	task.Watchers = []string{token, "cy"}
	if _, err := filer.CreateTask(t.Context(), "op-create", task, nil); err != nil {
		t.Fatalf("create as the token: %v", err)
	}
	r.drain()
	ana := r.writer.As("ana", worklet.AuthorHuman, worklet.Provenance{})
	comment := worklet.Comment{ID: "c-1", Task: "t-1", Author: "ana", AuthorKind: worklet.AuthorHuman,
		Body: "where are we with this?", CreatedAt: wednesday}
	if _, err := ana.UpdateTask(t.Context(), "op-comment", "t-1", "ENG", worklet.NoIfMatch,
		worklet.TaskPatch{Comment: &comment}, worklet.ChangeComment,
		worklet.Wake{Kind: worklet.ChangeComment, Before: task, After: task, Comment: &comment}.Notify(nil),
	); err != nil {
		t.Fatalf("comment: %v", err)
	}
	r.drain()
	done := task
	done.Status, done.StatusGroup = worklet.StatusDone, worklet.GroupDone
	if _, err := ana.UpdateTask(t.Context(), "op-done", "t-1", "ENG", worklet.NoIfMatch,
		worklet.TaskPatch{Status: &done.Status}, worklet.ChangeStatus,
		worklet.Wake{Kind: worklet.ChangeStatus, Before: task, After: done}.Notify(nil),
	); err != nil {
		t.Fatalf("finish: %v", err)
	}
	r.drain()

	got := r.recipients("t-1")
	if slices.Contains(got, token) {
		t.Fatalf("the notice rows name the token: %v", got)
	}
	for _, want := range []string{"bo", "cy"} {
		if !slices.Contains(got, want) {
			t.Errorf("the notice rows %v leave out %s", got, want)
		}
	}
}
