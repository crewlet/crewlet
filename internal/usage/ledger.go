package usage

import (
	"context"
	"database/sql"
	"fmt"
)

// TurnLedgerRow is one seat's turns under one wake and one outcome over a range
// of company days, every data node summed: the figures "turns that changed
// nothing, and what they cost" is read from.
type TurnLedgerRow struct {
	AgentID, Handle string
	Wake, Outcome   string

	Turns, Input, Output, CacheRead, CacheWrite, AuxInput, AuxOutput int64
}

// turnLedgerSQL is [TurnLedger]'s statement: a day range of every node and every
// seat, through the key's leading day (replicated 0154).
const turnLedgerSQL = `
	SELECT l.agent_id, COALESCE(MAX(h.handle), ''), l.wake, l.outcome,
	       SUM(l.turns), SUM(l.input), SUM(l.output), SUM(l.cache_read),
	       SUM(l.cache_write), SUM(l.aux_input), SUM(l.aux_output)
	  FROM usage_turn_ledger AS l
	  LEFT JOIN usage_turns AS h
	    ON h.day = l.day AND h.node = l.node AND h.agent_id = l.agent_id
	 WHERE l.day >= ? AND l.day <= ?
	 GROUP BY l.agent_id, l.wake, l.outcome
	 ORDER BY l.agent_id, l.wake, l.outcome`

// TurnLedger reads every seat's turns by (wake, outcome) for the company days
// q.From..q.To, inclusive, summed over every data node. A seat-day past its cap
// counts the rows it dropped ([LedgerPerSeatDay]) on its head row.
func TurnLedger(ctx context.Context, estate Estate, q SpendQuery) ([]TurnLedgerRow, error) {
	if err := q.check(); err != nil {
		return nil, err
	}
	var out []TurnLedgerRow
	err := estate.Read(ctx, func(tx *sql.Tx) error {
		rows, err := tx.QueryContext(ctx, turnLedgerSQL, q.From, q.To)
		if err != nil {
			return err
		}
		defer func() { _ = rows.Close() }()
		for rows.Next() {
			var r TurnLedgerRow
			if err := rows.Scan(&r.AgentID, &r.Handle, &r.Wake, &r.Outcome, &r.Turns, &r.Input,
				&r.Output, &r.CacheRead, &r.CacheWrite, &r.AuxInput, &r.AuxOutput); err != nil {
				return err
			}
			out = append(out, r)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, fmt.Errorf("usage: read the turn ledger for %s..%s: %w", q.From, q.To, err)
	}
	return out, nil
}
