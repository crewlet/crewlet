package worklet

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/crewlet/crewlet/internal/statelog"
)

// A TYPED ANSWER to a work item is a record the engine can act on (ADR-0122):
// a change to one of the item's fields (its status, its assignee, its content,
// its relations, a wait until a date), or a comment that puts a question to
// somebody with `ask`. A comment that asks nothing, its edit, resolution or
// removal, and a change of who watches and nothing else are words or nothing:
// the engine cannot tell when the condition such words describe is met. A
// watch written beside a change of the item is that change, whatever kind its
// row names ([movedBeyondWatch], #953).

// conversationKinds is [ChangeKind.Conversation]'s set, as SQL.
const conversationKinds = `('` + string(ChangeComment) + `', '` + string(ChangeCommentEdited) +
	`', '` + string(ChangeCommentResolved) + `', '` + string(ChangeCommentRemoved) + `')`

// movedBeyondWatch is whether a history row `h`'s deltas name a field beyond
// who watches the item and who of them is muted: the SQL form of what
// [Notify.watchOnly] asks of a record's card, for the reads that decide on the
// row itself. Read off the deltas and never the kind, because
// `update_work_item` files a watch written beside a priority, a date or a
// custom value under `watchers` wherever nothing else named a kind.
const movedBeyondWatch = `json_remove(h.fields_json, '$.` + watchersField + `', '$.` +
	mutedField + `') <> '{}'`

// typedWrite is whether a history row `h` is a typed answer to its item, an
// ask found by the record that wrote it (the row's id). ONE EXPRESSION, read
// by the ready read's recheck ([ReadyItem.Quiet]) and by the check of a
// pickup's answer ([Reader.TypedAnswer]), so the two cannot disagree.
const typedWrite = `(h.kind NOT IN ` + conversationKinds + ` AND ` + movedBeyondWatch + `
	OR h.kind = '` + string(ChangeComment) + `' AND EXISTS (SELECT 1 FROM tracker_comments c
		WHERE c.task_id = h.subject_id AND c.ask <> '' AND c.record_id = h.id))`

// TypedAnswerQuery asks whether one turn answered one item with a typed answer.
type TypedAnswerQuery struct {
	// Turn is the turn's id, which every history row its writes made
	// carries; Item the item's id.
	Turn string
	Item string

	// Level is the read level: the check reads the turn's own writes, so it
	// reads at one that has them.
	Level statelog.ReadLevel
}

// typedAnswerStatement is whether the turn wrote a typed answer on the item.
const typedAnswerStatement = `SELECT EXISTS (SELECT 1 FROM tracker_history h
	WHERE h.turn_id = ? AND h.subject_id = ? AND ` + typedWrite + `)`

// TypedAnswer reports whether q's turn answered q's item with a typed answer:
// the delivery a work pickup's turn owes (ADR-0122).
func (r *Reader) TypedAnswer(ctx context.Context, q TypedAnswerQuery) (bool, error) {
	if q.Turn == "" || q.Item == "" || q.Level == "" {
		return false, invalid("a typed-answer read names a turn, an item and a level, "+
			"not %q, %q at %q", q.Turn, q.Item, q.Level)
	}
	var typed bool
	_, err := r.log.Read(ctx, statelog.Query{
		Level: q.Level,
		Scope: statelog.ScopeSet{Paths: []string{pathDomain}}.Normalised(),
		Set:   true,
	}, func(tx *sql.Tx) error {
		return tx.QueryRowContext(ctx, typedAnswerStatement, q.Turn, q.Item).Scan(&typed)
	})
	if err != nil {
		return false, fmt.Errorf("worklet: read whether turn %s answered %s: %w", q.Turn, q.Item, err)
	}
	return typed, nil
}
