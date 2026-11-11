package worklet

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"

	"github.com/crewlet/crewlet/internal/statelog"
)

// TurnsMovedQuery is a set of turns, by id, read at a level.
type TurnsMovedQuery struct {
	Turns []string
	Level statelog.ReadLevel
}

// turnsMovedStatement is, of the turns its argument lists (a JSON array),
// every history row they wrote but the conversation's ([conversationKinds],
// whose words are the change): each row's turn, kind and deltas, which
// [TurnsMoved] reads a move of work from.
const turnsMovedStatement = `SELECT h.turn_id, h.kind, h.fields_json
	FROM json_each(?) AS j
	JOIN tracker_history AS h ON h.turn_id = CAST(j.value AS TEXT)
	WHERE h.turn_id != '' AND h.kind NOT IN ` + conversationKinds

// TurnsMoved reports which of q's turns moved work on the tracker: wrote any
// history row but a comment or a change of who watches and nothing else.
//
// BY THE ROW'S DELTAS, NEVER BY ITS KIND ALONE, and by the rule a turn's own
// outcome is read by ([Notify.watchOnly], [committedChange]): a row records
// one kind, and `update_work_item` files a watch written with a due date, a
// priority or a custom value under `watchers`, so read by its kind such a turn
// moved nothing. Decided here rather than in the statement, so the rule is
// that one function over the applier's own deltas, which are whole (#953).
func (r *Reader) TurnsMoved(ctx context.Context, q TurnsMovedQuery) (map[string]bool, error) {
	out := map[string]bool{}
	if len(q.Turns) == 0 {
		return out, nil
	}
	if q.Level == "" {
		return nil, invalid("a turns read names a level")
	}
	turns, err := json.Marshal(q.Turns)
	if err != nil {
		return nil, fmt.Errorf("worklet: encode the turns: %w", err)
	}
	_, err = r.log.Read(ctx, statelog.Query{
		Level: q.Level,
		Scope: statelog.ScopeSet{Paths: []string{pathDomain}}.Normalised(),
		Set:   true,
	}, func(tx *sql.Tx) error {
		rows, qerr := tx.QueryContext(ctx, turnsMovedStatement, string(turns))
		if qerr != nil {
			return qerr
		}
		for rows.Next() {
			var turn, kind string
			var deltas sql.NullString
			if serr := rows.Scan(&turn, &kind, &deltas); serr != nil {
				_ = rows.Close()
				return serr
			}
			var fields map[string]Delta
			if deltas.Valid && deltas.String != "" {
				if derr := json.Unmarshal([]byte(deltas.String), &fields); derr != nil {
					_ = rows.Close()
					return fmt.Errorf("decode turn %s's %s row's deltas: %w", turn, kind, derr)
				}
			}
			if !(&Notify{Kind: ChangeKind(kind), Fields: fields}).watchOnly() {
				out[turn] = true
			}
		}
		return closeRows(rows)
	})
	if err != nil {
		return nil, fmt.Errorf("worklet: read what the turns moved: %w", err)
	}
	return out, nil
}
