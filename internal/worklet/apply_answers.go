package worklet

import (
	"context"
	"database/sql"
	"fmt"
	"slices"
)

// A question's `answered_by` is the fifth derived column: the remark that
// answered it, stamped onto the question's row by the ANSWERING remark's
// apply ([writeComment]) — the first answer the log carries after the
// question was posted, since the stamp is guarded `answered_by IS NULL` and
// lands only from a post that actually wrote its row.
//
// It is derived rather than carried because no record about the question
// states it: the question's own records never move it, and an answer's record
// names the question it answers, not whether that question was still open. So
// the RULE — which answer holds a question — is this build's, and a rule is
// what [statelog.Deriver] versions: a node holding rows another rule set
// wrote, a reanchored cursor's or an adopted artefact's, has them brought to
// this one by [rederiveAnswers] before it maintains them further.
//
// THE ORDER THE RULE READS IS THE LOG'S, and the rows keep it: each comment's
// `record_id` is the id of the history row its posting record wrote
// ([historyID]), and that row's `log_seq` is where the record sits in the log.
// A comment row holds no position of its own, and its history row names the
// comment only when its record woke somebody, so the posting record's id is
// the one join that reaches every remark's place in the log.

// answersStatement is [rederiveAnswers]' read: every answering remark beside
// the remark it names on the same task, each at the log position its post
// landed at, kept only where the answer was posted after the question — named
// for the plan test.
//
// AN ANSWER POSTED BEFORE ITS QUESTION closes nothing in the apply, which met
// no row to stamp, so it is left out here as well.
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
// IT WRITES ONLY WHAT DIFFERS, so it reports the rows a different rule set had
// left wrong and nothing for rows this build maintained. The rows are read
// whole before anything is written, because a statement cannot be issued on a
// transaction while a query on it is still being read.
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
