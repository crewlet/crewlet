package usage

import (
	"context"
	"database/sql"
	"fmt"
)

// TurnLedgerRow is one seat's wakes under one wake, disposition and outcome over
// a range of company days, every data node summed: the figures "turns that
// changed nothing, and what they cost" and "wakes held" are read from.
type TurnLedgerRow struct {
	AgentID, Handle            string
	Wake, Disposition, Outcome string

	Turns, Input, Output, CacheRead, CacheWrite, AuxInput, AuxOutput int64
}

// Tokens is what the row's turns spent: input, output and the auxiliary calls'.
// The cache columns are a breakdown of Input, never an addition to it.
func (r TurnLedgerRow) Tokens() int64 { return r.Input + r.Output + r.AuxInput + r.AuxOutput }

// turnLedgerSQL is [TurnLedger]'s statement: a day range of every node and every
// seat, through the key's leading day (replicated 0155).
const turnLedgerSQL = `
	SELECT l.agent_id, COALESCE(MAX(h.handle), ''), l.wake, l.disposition, l.outcome,
	       SUM(l.turns), SUM(l.input), SUM(l.output), SUM(l.cache_read),
	       SUM(l.cache_write), SUM(l.aux_input), SUM(l.aux_output)
	  FROM usage_turn_ledger AS l
	  LEFT JOIN usage_turns AS h
	    ON h.day = l.day AND h.node = l.node AND h.agent_id = l.agent_id
	 WHERE l.day >= ? AND l.day <= ?
	 GROUP BY l.agent_id, l.wake, l.disposition, l.outcome
	 ORDER BY l.agent_id, l.wake, l.disposition, l.outcome`

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
			if err := rows.Scan(&r.AgentID, &r.Handle, &r.Wake, &r.Disposition, &r.Outcome, &r.Turns, &r.Input,
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

// ToolRefusalRow is one seat's refused calls to one tool under one class over a
// range of company days, every data node summed.
type ToolRefusalRow struct {
	AgentID, Handle     string
	Server, Tool, Class string
	Calls               int64
}

// toolRefusalsSQL is [ToolRefusals]' statement, through the key's leading day
// (replicated 0155).
const toolRefusalsSQL = `
	SELECT r.agent_id, COALESCE(MAX(h.handle), ''), r.server, r.tool, r.class, SUM(r.calls)
	  FROM usage_tool_refusals AS r
	  LEFT JOIN usage_turns AS h
	    ON h.day = r.day AND h.node = r.node AND h.agent_id = r.agent_id
	 WHERE r.day >= ? AND r.day <= ?
	 GROUP BY r.agent_id, r.server, r.tool, r.class
	 ORDER BY r.agent_id, r.server, r.tool, r.class`

// ToolRefusals reads every seat's refused calls per (server, tool, class) for
// the company days q.From..q.To, inclusive, summed over every data node.
func ToolRefusals(ctx context.Context, estate Estate, q SpendQuery) ([]ToolRefusalRow, error) {
	if err := q.check(); err != nil {
		return nil, err
	}
	var out []ToolRefusalRow
	err := estate.Read(ctx, func(tx *sql.Tx) error {
		rows, err := tx.QueryContext(ctx, toolRefusalsSQL, q.From, q.To)
		if err != nil {
			return err
		}
		defer func() { _ = rows.Close() }()
		for rows.Next() {
			var r ToolRefusalRow
			if err := rows.Scan(&r.AgentID, &r.Handle, &r.Server, &r.Tool, &r.Class, &r.Calls); err != nil {
				return err
			}
			out = append(out, r)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, fmt.Errorf("usage: read the refused calls for %s..%s: %w", q.From, q.To, err)
	}
	return out, nil
}
