package worklet

import (
	"context"
	"database/sql"
	"fmt"
	"slices"
)

// A question's `answered_by` is a derived column: the first answer the log
// carries after the question, stamped by the answering remark's apply
// ([writeComment]), guarded `answered_by IS NULL` and only from a post that
// wrote its row. No record states it, so the rule is this build's and
// [rederiveAnswers] re-derives it ([statelog.Deriver]).
//
// THE ORDER IS THE LOG'S: a comment's `record_id` is the history row its
// posting record wrote ([historyID]), whose `log_seq` places it. A comment row
// holds no position, and its history row names it only when it woke
// somebody, so that id is the one join reaching every remark's place.

// answersStatement is [rederiveAnswers]' read, named for the plan test: every
// answer beside the remark it names on the same task, at its post's log
// position. AN ANSWER POSTED BEFORE ITS QUESTION is left out, as the apply met
// no row to stamp.
const answersStatement = `
		SELECT q.id, a.id, ha.log_seq
		FROM tracker_comments a
		JOIN tracker_comments q ON q.id = a.answers AND q.task_id = a.task_id
		JOIN tracker_history ha ON ha.id = a.record_id
		JOIN tracker_history hq ON hq.id = q.record_id
		WHERE a.answers IS NOT NULL AND a.answers <> '' AND ha.log_seq > hq.log_seq`

// answerStampsStatement reads every stamp the rows hold now, and
// stampAnswerStatement writes one.
const (
	answerStampsStatement = `
		SELECT id, answered_by FROM tracker_comments WHERE answered_by IS NOT NULL`
	stampAnswerStatement = `UPDATE tracker_comments SET answered_by = ? WHERE id = ?`
)

// rederiveAnswers brings every question's `answered_by` to the rule the apply
// maintains: the first answer, in log order, posted after it.
//
// IT WRITES ONLY WHAT DIFFERS, so rows this build maintained report nothing.
// The reads finish before any write: a statement cannot be issued on a
// transaction while a query on it is open.
func rederiveAnswers(ctx context.Context, tx *sql.Tx) (int, error) {
	type answer struct {
		id  string
		seq int64
	}
	want := map[string]answer{}
	rows, err := tx.QueryContext(ctx, answersStatement)
	if err != nil {
		return 0, fmt.Errorf("worklet: read every answer to re-derive what closed each question: %w", err)
	}
	for rows.Next() {
		var question string
		var one answer
		if err = rows.Scan(&question, &one.id, &one.seq); err != nil {
			_ = rows.Close()
			return 0, fmt.Errorf("worklet: scan an answer: %w", err)
		}
		held, ok := want[question]
		if !ok || one.seq < held.seq || (one.seq == held.seq && one.id < held.id) {
			want[question] = one
		}
	}
	if err = rows.Err(); err != nil {
		_ = rows.Close()
		return 0, fmt.Errorf("worklet: read every answer to re-derive what closed each question: %w", err)
	}
	if err = rows.Close(); err != nil {
		return 0, err
	}

	stamped := map[string]string{}
	rows, err = tx.QueryContext(ctx, answerStampsStatement)
	if err != nil {
		return 0, fmt.Errorf("worklet: read every question's answer: %w", err)
	}
	for rows.Next() {
		var id, by string
		if err = rows.Scan(&id, &by); err != nil {
			_ = rows.Close()
			return 0, fmt.Errorf("worklet: scan a question's answer: %w", err)
		}
		stamped[id] = by
	}
	if err = rows.Err(); err != nil {
		_ = rows.Close()
		return 0, fmt.Errorf("worklet: read every question's answer: %w", err)
	}
	if err = rows.Close(); err != nil {
		return 0, err
	}

	fix := map[string]*string{}
	for id, by := range stamped {
		if held, ok := want[id]; !ok {
			fix[id] = nil
		} else if held.id != by {
			fix[id] = &held.id
		}
	}
	for id, held := range want {
		if _, ok := stamped[id]; !ok {
			fix[id] = &held.id
		}
	}
	ids := make([]string, 0, len(fix))
	for id := range fix {
		ids = append(ids, id)
	}
	// SORTED for [rederiveReopens]' reason: the result does not depend on
	// it, a failure that reproduces does.
	slices.Sort(ids)
	for _, id := range ids {
		if _, err = tx.ExecContext(ctx, stampAnswerStatement, fix[id], id); err != nil {
			return 0, fmt.Errorf("worklet: write the re-derived answer of comment %s: %w", id, err)
		}
	}
	return len(ids), nil
}
