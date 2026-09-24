package usage

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/crewlet/crewlet/internal/period"
)

// Estate is the replicated estate as a reader of this domain's rows needs it:
// a read transaction and nothing else. Satisfied by the replicated handle of a
// [store.DB].
type Estate interface {
	Read(ctx context.Context, fn func(*sql.Tx) error) error
}

// SpendRow is one spend cell of one node's day, with the seat named as that
// day's record named it.
type SpendRow struct {
	Day, Node, AgentID string
	Handle, Role       string
	Tokens
}

// Spend reads every node's spend for the company days from..to, inclusive, in
// (day, node, seat, cell) order.
//
// EVERY NODE'S ROWS, which is the whole point of the domain: the answer is the
// same on whichever node is asked, and it still includes a node that has left
// the fleet. The labels are the company's own (`2026-09-23`), which sort as
// strings — so the range is a seek on the leading column of every key.
func Spend(ctx context.Context, estate Estate, from, to string) ([]SpendRow, error) {
	for _, label := range []string{from, to} {
		if _, err := period.Parse(period.Day, label, nil); err != nil {
			return nil, fmt.Errorf("usage: a spend range is two company days: %w", err)
		}
	}
	if from > to {
		return nil, fmt.Errorf("usage: the spend range %s..%s ends before it starts", from, to)
	}
	var out []SpendRow
	err := estate.Read(ctx, func(tx *sql.Tx) error {
		rows, err := tx.QueryContext(ctx, `
			SELECT t.day, t.node, t.agent_id,
			       COALESCE(h.handle, ''), COALESCE(h.role, ''),
			       t.phase, t.worker, t.model, t.provider_key,
			       t.input, t.output, t.cache_read, t.cache_write, t.total, t.calls
			  FROM usage_tokens t
			  LEFT JOIN usage_turns h
			    ON h.day = t.day AND h.node = t.node AND h.agent_id = t.agent_id
			 WHERE t.day >= ? AND t.day <= ?
			 ORDER BY t.day, t.node, t.agent_id, t.phase, t.worker, t.model, t.provider_key`,
			from, to)
		if err != nil {
			return err
		}
		defer func() { _ = rows.Close() }()
		for rows.Next() {
			var r SpendRow
			if err := rows.Scan(&r.Day, &r.Node, &r.AgentID, &r.Handle, &r.Role,
				&r.Phase, &r.Worker, &r.Model, &r.ProviderKey, &r.Input, &r.Output,
				&r.CacheRead, &r.CacheWrite, &r.Total, &r.Calls); err != nil {
				return err
			}
			out = append(out, r)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, fmt.Errorf("usage: read the spend for %s..%s: %w", from, to, err)
	}
	return out, nil
}
