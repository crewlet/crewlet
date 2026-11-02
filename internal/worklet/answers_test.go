package worklet_test

import (
	"database/sql"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/store"
	"github.com/crewlet/crewlet/internal/worklet"
)

// stampsOf reads every comment's `answered_by`, NULL as the empty string.
func stampsOf(t *testing.T, db store.ReplicatedHandle) map[string]string {
	t.Helper()
	out := map[string]string{}
	if err := db.Read(t.Context(), func(tx *sql.Tx) error {
		rows, err := tx.QueryContext(t.Context(),
			`SELECT id, COALESCE(answered_by, '') FROM tracker_comments`)
		if err != nil {
			return err
		}
		defer func() { _ = rows.Close() }()
		for rows.Next() {
			var id, by string
			if err := rows.Scan(&id, &by); err != nil {
				return err
			}
			out[id] = by
		}
		return rows.Err()
	}); err != nil {
		t.Fatalf("read the answer stamps: %v", err)
	}
	return out
}

// rederiveOn runs the applier's re-derivation once and reports the rows it
// wrote.
func rederiveOn(t *testing.T, db store.ReplicatedHandle, applier *worklet.Applier) int {
	t.Helper()
	var n int
	if err := db.Tx(t.Context(), func(tx *sql.Tx) error {
		var err error
		n, err = applier.Rederive(t.Context(), tx, statelog.ApplyOptions{})
		return err
	}); err != nil {
		t.Fatalf("re-derive: %v", err)
	}
	return n
}

// THE BACKFILL EQUALS THE STAMP THE APPLY MAINTAINED, for which remark answered
// a question as for every derived column: rows a different rule set wrote — a
// reanchored cursor's, an adopted artefact's — re-derive to exactly what
// applying the records reached, and rows this build maintained re-derive to
// themselves.
func TestTheAnswerBackfillEqualsTheMaintainedStamp(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	rollback := r.createTask("who owns the rollback")
	pager := r.createTask("who holds the pager")
	post := func(op, task string, comment worklet.Comment) {
		t.Helper()
		comment.Task, comment.AuthorKind, comment.CreatedAt = task, worklet.AuthorHuman, wednesday
		if _, err := r.writer.UpdateTask(t.Context(), op, task, "ENG", worklet.NoIfMatch,
			worklet.TaskPatch{Comment: &comment}, worklet.ChangeComment, nil); err != nil {
			t.Fatalf("%s: %v", op, err)
		}
		r.drain()
	}
	answers := func(id string) *string { return &id }
	post("op-q1", rollback.ID, worklet.Comment{ID: "cm-1", Author: "ana",
		Body: "who owns the rollback?", Ask: "bob"})
	post("op-a1", rollback.ID, worklet.Comment{ID: "cm-2", Author: "bob",
		Body: "platform does", Answers: answers("cm-1")})
	post("op-q2", rollback.ID, worklet.Comment{ID: "cm-3", Author: "ana",
		Body: "and the runbook?", Ask: "cy"})
	post("op-q3", pager.ID, worklet.Comment{ID: "cm-4", Author: "ana",
		Body: "who holds the pager?", Ask: "bob"})
	post("op-a3", pager.ID, worklet.Comment{ID: "cm-5", Author: "bob",
		Body: "I do", Answers: answers("cm-4")})
	if _, err := r.writer.EditComment(t.Context(), "op-edit", pager.ID, "ENG",
		"cm-4", "who holds the pager this week?", nil); err != nil {
		t.Fatalf("edit: %v", err)
	}
	r.drain()

	maintained := stampsOf(t, r.db)
	if maintained["cm-1"] != "cm-2" || maintained["cm-4"] != "cm-5" || maintained["cm-3"] != "" {
		t.Fatalf("the apply maintained %v, want cm-1 by cm-2, cm-4 by cm-5 and "+
			"cm-3 open", maintained)
	}
	if n := rederiveOn(t, r.db, r.applier); n != 0 {
		t.Errorf("re-deriving rows this build maintained rewrote %d of them", n)
	}

	// ROWS A DIFFERENT RULE SET DERIVED: both answers lost, and an open
	// question stamped with somebody else's answer.
	if err := r.db.Tx(t.Context(), func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(t.Context(),
			`UPDATE tracker_comments SET answered_by = NULL`); err != nil {
			return err
		}
		_, err := tx.ExecContext(t.Context(),
			`UPDATE tracker_comments SET answered_by = 'cm-2' WHERE id = 'cm-3'`)
		return err
	}); err != nil {
		t.Fatalf("age the rows: %v", err)
	}
	if n := rederiveOn(t, r.db, r.applier); n != 3 {
		t.Errorf("re-deriving three wrong stamps rewrote %d rows", n)
	}
	if got := stampsOf(t, r.db); !equalStamps(got, maintained) {
		t.Errorf("the rows re-derive to %v and the apply maintained %v — a "+
			"re-derived node would disagree with one that applied the records",
			got, maintained)
	}
}

// AN ANSWER POSTED BEFORE ITS QUESTION CLOSES NOTHING, in the apply and in the
// re-derivation alike: the apply met no row to stamp, and a re-post of the
// answer once the question exists posts nothing, so it closes nothing either.
// A re-derivation that read only which remark names which would stamp it, and
// the node re-deriving would hold a question answered that every node applying
// the records holds open.
func TestAnAnswerPostedBeforeItsQuestionClosesNothing(t *testing.T) {
	t.Parallel()
	h := newApplyHarness(t)
	at := time.Unix(1_700_000_100, 0).UTC()
	if _, err := h.apply(taskRecord("t-1", worklet.OpCreate, newTask("t-1"), nil), at); err != nil {
		t.Fatalf("create: %v", err)
	}
	post := func(op string, comment worklet.Comment) {
		t.Helper()
		comment.Task, comment.AuthorKind, comment.CreatedAt = "t-1", worklet.AuthorHuman, at
		rec := taskRecord("t-1", worklet.OpPatch, worklet.TaskPatch{Comment: &comment}, nil)
		rec.Kind, rec.OpID = worklet.ChangeComment, op
		if _, err := h.apply(rec, at); err != nil {
			t.Fatalf("%s: %v", op, err)
		}
	}
	answers := "cm-1"
	answer := worklet.Comment{ID: "cm-2", Author: "bob", Body: "platform does",
		Answers: &answers}
	post("op-answer", answer)
	post("op-ask", worklet.Comment{ID: "cm-1", Author: "ana",
		Body: "who owns the rollback?", Ask: "bob"})
	if got := stampsOf(t, h.db)["cm-1"]; got != "" {
		t.Fatalf("a question posted after its answer reads answered by %q", got)
	}
	post("op-answer-again", answer)
	if got := stampsOf(t, h.db)["cm-1"]; got != "" {
		t.Errorf("a re-post of an answer that found its row closed the question "+
			"it names (answered by %q) — the post wrote nothing", got)
	}
	if n := rederiveOn(t, h.db, h.applier); n != 0 {
		t.Errorf("the re-derivation rewrote %d rows the apply maintained", n)
	}
	if got := stampsOf(t, h.db)["cm-1"]; got != "" {
		t.Errorf("the re-derivation stamped the question answered by %q, which "+
			"was posted before it", got)
	}
}

// THE FIRST ANSWER THE LOG CARRIES HOLDS THE QUESTION, in the re-derivation as
// in the apply: the apply's stamp is guarded on the question being open, so a
// second answer that reached the log — which the writer refuses, but a restore
// or a retained record can leave — never takes it over, and a re-derivation
// that picked any answer but the earliest would close the question on one
// node with a remark every other node holds as a mere reply.
func TestTheFirstAnswerTheLogCarriesHoldsTheQuestion(t *testing.T) {
	t.Parallel()
	h := newApplyHarness(t)
	at := time.Unix(1_700_000_100, 0).UTC()
	if _, err := h.apply(taskRecord("t-1", worklet.OpCreate, newTask("t-1"), nil), at); err != nil {
		t.Fatalf("create: %v", err)
	}
	post := func(op string, comment worklet.Comment) {
		t.Helper()
		comment.Task, comment.AuthorKind, comment.CreatedAt = "t-1", worklet.AuthorHuman, at
		rec := taskRecord("t-1", worklet.OpPatch, worklet.TaskPatch{Comment: &comment}, nil)
		rec.Kind, rec.OpID = worklet.ChangeComment, op
		if _, err := h.apply(rec, at); err != nil {
			t.Fatalf("%s: %v", op, err)
		}
	}
	answers := "cm-1"
	post("op-ask", worklet.Comment{ID: "cm-1", Author: "ana",
		Body: "who owns the rollback?", Ask: "bob"})
	post("op-first", worklet.Comment{ID: "cm-2", Author: "bob",
		Body: "platform does", Answers: &answers})
	post("op-second", worklet.Comment{ID: "cm-3", Author: "cy",
		Body: "no, release does", Answers: &answers})
	if got := stampsOf(t, h.db)["cm-1"]; got != "cm-2" {
		t.Fatalf("the apply holds the question answered by %q, want the first "+
			"answer cm-2", got)
	}
	if n := rederiveOn(t, h.db, h.applier); n != 0 {
		t.Errorf("re-deriving rows this build maintained rewrote %d of them", n)
	}
	for name, planted := range map[string]any{"lost": nil, "the later answer": "cm-3"} {
		if err := h.db.Tx(t.Context(), func(tx *sql.Tx) error {
			_, err := tx.ExecContext(t.Context(),
				`UPDATE tracker_comments SET answered_by = ? WHERE id = 'cm-1'`, planted)
			return err
		}); err != nil {
			t.Fatalf("plant %s: %v", name, err)
		}
		if n := rederiveOn(t, h.db, h.applier); n != 1 {
			t.Errorf("re-deriving a stamp %s rewrote %d rows, want 1", name, n)
		}
		if got := stampsOf(t, h.db)["cm-1"]; got != "cm-2" {
			t.Errorf("with the stamp %s the question re-derives answered by %q, "+
				"want the first answer cm-2", name, got)
		}
	}
}

func equalStamps(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for id, by := range a {
		if b[id] != by {
			return false
		}
	}
	return true
}
