package worklet_test

import (
	"database/sql"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/worklet"
)

// overtakenOf is each ask's `overtaken`, by comment id.
func (r *roundTrip) overtakenOf(ids ...string) map[string]string {
	r.t.Helper()
	out := map[string]string{}
	if err := r.db.Tx(r.t.Context(), func(tx *sql.Tx) error {
		for _, id := range ids {
			var v string
			if err := tx.QueryRowContext(r.t.Context(),
				`SELECT overtaken FROM tracker_comments WHERE id = ?`, id).Scan(&v); err != nil {
				return err
			}
			out[id] = v
		}
		return nil
	}); err != nil {
		r.t.Fatalf("read overtaken: %v", err)
	}
	return out
}

// AN ASK THE FACTS OVERTOOK NO LONGER WAITS ON ITS PERSON: its asker asking the
// same person again on the same item supersedes it, its item finishing makes it
// moot and reopening frees it; another asker's ask stands, an answer to an
// overtaken ask is still recorded, and a re-derivation agrees with the apply.
func TestAnAskTheFactsOvertookNoLongerWaits(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	filedTask(t, r, "w")
	at := time.Date(2026, 3, 4, 9, 0, 0, 0, time.UTC)
	comment := func(id, author string, ask string, answers *string, minute int) {
		t.Helper()
		c := worklet.Comment{ID: id, Task: "w", Author: author, AuthorKind: worklet.AuthorHuman,
			Body: "which way?", Ask: ask, Answers: answers, CreatedAt: at.Add(time.Duration(minute) * time.Minute)}
		if _, err := r.writer.As(author, worklet.AuthorHuman, worklet.Provenance{}).UpdateTask(t.Context(),
			"op-"+id, "w", "ENG", worklet.NoIfMatch, worklet.TaskPatch{Comment: &c}, worklet.ChangeComment,
			nil); err != nil {
			t.Fatalf("comment %s: %v", id, err)
		}
		r.drain()
	}
	move := func(op string, status worklet.Status) {
		t.Helper()
		if _, err := r.writer.UpdateTask(t.Context(), op, "w", "ENG", worklet.NoIfMatch,
			worklet.TaskPatch{Status: &status}, worklet.ChangeStatus, nil); err != nil {
			t.Fatalf("move to %s: %v", status, err)
		}
		r.drain()
	}
	waiting := func() int { return r.decisions("ana").Total.Total }
	check := func(when string, want map[string]string, open int) {
		t.Helper()
		ids := make([]string, 0, len(want))
		for id := range want {
			ids = append(ids, id)
		}
		got := r.overtakenOf(ids...)
		for id, w := range want {
			if got[id] != w {
				t.Errorf("%s: ask %s is overtaken %q, want %q", when, id, got[id], w)
			}
		}
		if n := waiting(); n != open {
			t.Errorf("%s: %d asks wait on ana, want %d", when, n, open)
		}
	}

	comment("c-1", "pm", "ana", nil, 1)
	comment("c-2", "pm", "ana", nil, 2)
	comment("c-3", "qa", "ana", nil, 3)
	check("pm asked twice and qa once", map[string]string{"c-1": "superseded", "c-2": "", "c-3": ""}, 2)

	answered := "c-1"
	comment("c-4", "ana", "", &answered, 4)
	check("ana answered the superseded ask", map[string]string{"c-1": "", "c-2": "", "c-3": ""}, 2)

	move("op-done", worklet.StatusDone)
	check("the item is done", map[string]string{"c-2": "moot", "c-3": "moot"}, 0)

	move("op-reopen", worklet.StatusTodo)
	check("the item is reopened", map[string]string{"c-2": "", "c-3": ""}, 2)

	// ROWS ANOTHER RULE SET DERIVED: every value wrong, then re-derived.
	move("op-done-again", worklet.StatusDone)
	if err := r.db.Tx(t.Context(), func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(t.Context(), `UPDATE tracker_comments SET overtaken = 'stale'`); err != nil {
			return err
		}
		_, err := r.applier.Rederive(t.Context(), tx, statelog.ApplyOptions{})
		return err
	}); err != nil {
		t.Fatalf("re-derive: %v", err)
	}
	check("re-derived", map[string]string{"c-1": "", "c-2": "moot", "c-3": "moot", "c-4": ""}, 0)
}
