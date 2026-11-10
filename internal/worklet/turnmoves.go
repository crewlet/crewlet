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

// turnsMovedStatement is, of the turns its first argument lists (a JSON
// array), the ones that wrote a history row of a kind its second does not.
const turnsMovedStatement = `SELECT DISTINCT h.turn_id
	FROM json_each(?) AS j
	JOIN tracker_history AS h ON h.turn_id = CAST(j.value AS TEXT)
	WHERE h.turn_id != '' AND h.kind NOT IN (SELECT value FROM json_each(?))`

// stillKinds are the changes that move no work: the conversation, and who
// watches, by the rule a turn's own outcome is read by ([ChangeKind.Moves]).
var stillKinds = func() []ChangeKind {
	var out []ChangeKind
	for _, k := range ChangeKinds {
		if !k.Moves() {
			out = append(out, k)
		}
	}
	return out
}()

// TurnsMoved reports which of q's turns moved work on the tracker: wrote any
// history row but a comment or a change of who watches.
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
	still, err := json.Marshal(stillKinds)
	if err != nil {
		return nil, fmt.Errorf("worklet: encode the still kinds: %w", err)
	}
	_, err = r.log.Read(ctx, statelog.Query{
		Level: q.Level,
		Scope: statelog.ScopeSet{Paths: []string{pathDomain}}.Normalised(),
		Set:   true,
	}, func(tx *sql.Tx) error {
		rows, err := tx.QueryContext(ctx, turnsMovedStatement, string(turns), string(still))
		if err != nil {
			return err
		}
		for rows.Next() {
			var turn string
			if err := rows.Scan(&turn); err != nil {
				_ = rows.Close()
				return err
			}
			out[turn] = true
		}
		return closeRows(rows)
	})
	if err != nil {
		return nil, fmt.Errorf("worklet: read what the turns moved: %w", err)
	}
	return out, nil
}
