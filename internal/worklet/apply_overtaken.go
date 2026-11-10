package worklet

import (
	"context"
	"database/sql"
	"fmt"
)

// The values of a question's `overtaken` (ADR-0023): the facts settled it before
// anybody answered, so it no longer waits on its person.
const (
	OvertakenSuperseded = "superseded"
	OvertakenMoot       = "moot"
)

// overtakenRule is the one rule, over the rows a node holds: an open ask its
// asker put to the same person again, later, on the same item is superseded;
// else an open ask on a finished item is moot; any other comment is neither.
const overtakenRule = `CASE
	WHEN c.ask = '' OR c.resolved = 1 OR c.answered_by IS NOT NULL OR c.removed = 1 THEN ''
	WHEN EXISTS (SELECT 1 FROM tracker_comments n
	              WHERE n.task_id = c.task_id AND n.ask = c.ask AND n.author = c.author
	                AND n.removed = 0
	                AND (n.created_at > c.created_at OR (n.created_at = c.created_at AND n.id > c.id)))
	  THEN '` + OvertakenSuperseded + `'
	WHEN EXISTS (SELECT 1 FROM tracker_tasks t
	              WHERE t.id = c.task_id AND t.status_group IN ('` + string(GroupDone) + `', '` + string(GroupClosed) + `'))
	  THEN '` + OvertakenMoot + `'
	ELSE '' END`

// overtakenStatement is [stampOvertaken]'s write for one task's comments, or
// every comment when id is empty, touching only the rows the rule moves.
func overtakenStatement(id string) (string, []any) {
	scope, args := "", []any{}
	if id != "" {
		scope, args = "c.task_id = ? AND ", append(args, id)
	}
	return `UPDATE tracker_comments AS c SET overtaken = ` + overtakenRule + `
		WHERE ` + scope + `c.overtaken <> ` + overtakenRule, args
}

// stampOvertaken brings the asks of one task, or of every task when id is
// empty, to [overtakenRule].
func stampOvertaken(ctx context.Context, tx *sql.Tx, id string) (int, error) {
	query, args := overtakenStatement(id)
	res, err := tx.ExecContext(ctx, query, args...)
	if err != nil {
		return 0, fmt.Errorf("worklet: stamp which asks the facts overtook: %w", err)
	}
	return affected(res)
}
