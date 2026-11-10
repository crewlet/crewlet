package code

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

// turnsMovedSQL is, of the turns its argument lists (a JSON array), the ones
// that pushed, opened a change or gave a review.
const turnsMovedSQL = `SELECT CAST(j.value AS TEXT) FROM json_each(?) AS j
	WHERE EXISTS (SELECT 1 FROM code_pushes AS p WHERE p.turn_id = CAST(j.value AS TEXT) AND p.turn_id != '')
	OR EXISTS (SELECT 1 FROM code_changes AS c WHERE c.created_turn = CAST(j.value AS TEXT) AND c.created_turn != '')
	OR EXISTS (SELECT 1 FROM code_reviews AS r WHERE r.turn_id = CAST(j.value AS TEXT) AND r.turn_id != '')`

// TurnsMovedCode reports which of q's turns moved work in code: pushed, opened a
// change or gave a review.
func (r *Reader) TurnsMovedCode(ctx context.Context, q TurnsMovedQuery) (map[string]bool, error) {
	out := map[string]bool{}
	if len(q.Turns) == 0 {
		return out, nil
	}
	if q.Level == "" {
		return nil, fmt.Errorf("%w: a turns read names a level", ErrInvalid)
	}
	turns, err := json.Marshal(q.Turns)
	if err != nil {
		return nil, fmt.Errorf("code: encode the turns: %w", err)
	}
	query := statelog.Freshness{Level: q.Level}.Query(statelog.ScopeSet{Paths: []string{DomainPath()}}, true)
	_, err = r.log.Read(ctx, query, func(tx *sql.Tx) error {
		rows, qerr := tx.QueryContext(ctx, turnsMovedSQL, string(turns))
		if qerr != nil {
			return qerr
		}
		defer func() { _ = rows.Close() }()
		for rows.Next() {
			var turn string
			if serr := rows.Scan(&turn); serr != nil {
				return serr
			}
			out[turn] = true
		}
		return rows.Err()
	})
	if err != nil {
		return nil, fmt.Errorf("code: read what the turns moved: %w", err)
	}
	return out, nil
}
