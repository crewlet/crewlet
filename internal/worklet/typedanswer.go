package worklet

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/store"
)

// A TYPED ANSWER to a work item is a record the engine can act on (ADR-0122):
// a change to one of its fields, or a comment that asks somebody with `ask`.
// Other comment rows and a watch-only change are words or nothing; a watch
// written beside a change is that change, whatever its kind ([movedBeyondWatch]).
//
// The seat's own rows on the item since its pickup's turn began answer it,
// whichever turn or none wrote them: a merge queue closes an item as the
// enqueuing seat, outside any turn. An item removed, finished or reassigned by
// somebody else since makes the pickup moot.

// conversationKinds is [ChangeKind.Conversation]'s set, as SQL.
const conversationKinds = `('` + string(ChangeComment) + `', '` + string(ChangeCommentEdited) +
	`', '` + string(ChangeCommentResolved) + `', '` + string(ChangeCommentRemoved) + `')`

// movedBeyondWatch is whether a history row `h`'s deltas name a field beyond
// who watches the item and who of them is muted: the SQL form of
// [Notify.watchOnly]. Read off the deltas, never the kind, because
// `update_work_item` files a watch beside a priority, a date or a custom value
// under `watchers` when nothing else named a kind.
const movedBeyondWatch = `json_remove(h.fields_json, '$.` + watchersField + `', '$.` +
	mutedField + `') <> '{}'`

// typedWrite is whether a history row `h` is a typed answer to its item, an
// ask found by the record that wrote it (the row's id): the ONE EXPRESSION
// that decides a pickup's answer ([Reader.PickupAnswer]), for the delivery
// checks and the advisor's ledger alike.
const typedWrite = `(h.kind NOT IN ` + conversationKinds + ` AND ` + movedBeyondWatch + `
	OR h.kind = '` + string(ChangeComment) + `' AND EXISTS (SELECT 1 FROM tracker_comments c
		WHERE c.task_id = h.subject_id AND c.ask <> '' AND c.record_id = h.id))`

// PickupAnswerQuery asks how a seat's turn left the item a work pickup handed
// it.
type PickupAnswerQuery struct {
	// Seat is the seat's handle and Item the item's id.
	Seat string
	Item string

	// Since is when the pickup's turn began: the seat's rows on the item
	// from then on answer it.
	Since time.Time

	// Level is the read level: the check reads the turn's own writes, so it
	// reads at one that has them.
	Level statelog.ReadLevel
}

// pickupTypedStatement is whether the seat wrote a typed answer on the item
// since the turn began.
const pickupTypedStatement = `SELECT EXISTS (SELECT 1 FROM tracker_history h
	WHERE h.subject_id = ? AND h.actor = ? AND h.created_at >= ? AND ` + typedWrite + `)`

// pickupItemStatement is where the item stands for its holder now.
const pickupItemStatement = `SELECT t.removed_at IS NOT NULL, t.status_group, t.assignee
	FROM tracker_tasks t WHERE t.id = ?`

// PickupAnswer reads how q's seat left q's item since q.Since: typed, moot
// (removed, finished or reassigned by somebody else), or unanswered (ADR-0122).
func (r *Reader) PickupAnswer(ctx context.Context, q PickupAnswerQuery) (types.PickupAnswer, error) {
	if q.Seat == "" || q.Item == "" || q.Since.IsZero() || q.Level == "" {
		return "", invalid("a pickup's answer is read for a seat, an item, the instant its turn "+
			"began and a level, not %q, %q, %s at %q", q.Seat, q.Item, q.Since, q.Level)
	}
	var answer types.PickupAnswer
	_, err := r.log.Read(ctx, statelog.Query{
		Level: q.Level,
		Scope: statelog.ScopeSet{Paths: []string{pathDomain}}.Normalised(),
		Set:   true,
	}, func(tx *sql.Tx) (err error) {
		answer, err = readPickupAnswer(ctx, tx, q)
		return err
	})
	if err != nil {
		return "", fmt.Errorf("worklet: read how %s answered %s: %w", q.Seat, q.Item, err)
	}
	return answer, nil
}

func readPickupAnswer(ctx context.Context, tx *sql.Tx, q PickupAnswerQuery) (types.PickupAnswer, error) {
	var typed bool
	if err := tx.QueryRowContext(ctx, pickupTypedStatement, q.Item, q.Seat,
		store.EncodeTime(q.Since)).Scan(&typed); err != nil {
		return "", err
	}
	if typed {
		return types.PickupTyped, nil
	}
	var removed bool
	var group StatusGroup
	var assignee string
	err := tx.QueryRowContext(ctx, pickupItemStatement, q.Item).Scan(&removed, &group, &assignee)
	switch {
	case errors.Is(err, sql.ErrNoRows), err == nil && removed:
		// PURGED OR IN THE TRASH: no write to it can land.
		return types.PickupRemoved, nil
	case err != nil:
		return "", err
	case group.Finished():
		return types.PickupFinished, nil
	case assignee != q.Seat:
		return types.PickupReassigned, nil
	}
	return types.PickupUnanswered, nil
}
