package doclet

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
// array), the ones that wrote a page history row of a kind its second does not.
const turnsMovedStatement = `SELECT DISTINCT h.turn_id
	FROM json_each(?) AS j
	JOIN pages_history AS h ON h.turn_id = CAST(j.value AS TEXT)
	WHERE h.turn_id != '' AND h.kind NOT IN (SELECT value FROM json_each(?))`

// stillKinds are the page changes that move no work ([ChangeKind.Moves]).
var stillKinds = func() []ChangeKind {
	var out []ChangeKind
	for _, k := range ChangeKinds() {
		if !k.Moves() {
			out = append(out, k)
		}
	}
	return out
}()

// TurnsMovedPages reports which of q's turns moved work on the pages: created,
// saved, renamed, moved, labelled or removed one.
func (r *Reader) TurnsMovedPages(ctx context.Context, q TurnsMovedQuery) (map[string]bool, error) {
	out := map[string]bool{}
	if len(q.Turns) == 0 {
		return out, nil
	}
	if q.Level == "" {
		return nil, invalid("level", "a turns read names a level")
	}
	turns, err := json.Marshal(q.Turns)
	if err != nil {
		return nil, fmt.Errorf("doclet: encode the turns: %w", err)
	}
	still, err := json.Marshal(stillKinds)
	if err != nil {
		return nil, fmt.Errorf("doclet: encode the still kinds: %w", err)
	}
	_, err = r.log.Read(ctx, statelog.Freshness{Level: q.Level}.Query(ReadScope(""), true),
		func(tx *sql.Tx) error {
			rows, qerr := tx.QueryContext(ctx, turnsMovedStatement, string(turns), string(still))
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
		return nil, fmt.Errorf("doclet: read what the turns moved: %w", err)
	}
	return out, nil
}
