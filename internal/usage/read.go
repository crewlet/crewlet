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

// SpendQuery is a range of company days, both inclusive, and optionally one
// seat.
type SpendQuery struct {
	// From and To are company day labels (`2026-09-23`).
	From, To string

	// AgentID narrows the read to one seat, by the id every node derives
	// alike; empty reads every seat.
	AgentID string
}

func (q SpendQuery) check() error {
	for _, label := range []string{q.From, q.To} {
		if _, err := period.Parse(period.Day, label, nil); err != nil {
			return fmt.Errorf("usage: a spend range is two company days: %w", err)
		}
	}
	if q.From > q.To {
		return fmt.Errorf("usage: the spend range %s..%s ends before it starts", q.From, q.To)
	}
	return nil
}

// Spend reads every node's spend for the company days q.From..q.To,
// inclusive, in (day, node, seat, cell) order.
//
// EVERY NODE'S ROWS, which is the whole point of the domain: the answer is the
// same on whichever node is asked, and it still includes a node that has left
// the fleet. The labels are the company's own (`2026-09-23`), which sort as
// strings — so the range is a seek on the leading column of every key.
func Spend(ctx context.Context, estate Estate, q SpendQuery) ([]SpendRow, error) {
	if err := q.check(); err != nil {
		return nil, err
	}
	var out []SpendRow
	err := estate.Read(ctx, func(tx *sql.Tx) error {
		// The seat filter is an `= ?` OR an empty argument, rather than
		// two query texts: one statement, and a narrowed read seeks the
		// same day range and drops the other seats' rows on the way.
		rows, err := tx.QueryContext(ctx, `
			SELECT t.day, t.node, t.agent_id,
			       COALESCE(h.handle, ''), COALESCE(h.role, ''),
			       t.phase, t.worker, t.model, t.provider_key,
			       t.input, t.output, t.cache_read, t.cache_write, t.total, t.calls
			  FROM usage_tokens t
			  LEFT JOIN usage_turns h
			    ON h.day = t.day AND h.node = t.node AND h.agent_id = t.agent_id
			 WHERE t.day >= ? AND t.day <= ? AND (? = '' OR t.agent_id = ?)
			 ORDER BY t.day, t.node, t.agent_id, t.phase, t.worker, t.model, t.provider_key`,
			q.From, q.To, q.AgentID, q.AgentID)
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
		return nil, fmt.Errorf("usage: read the spend for %s..%s: %w", q.From, q.To, err)
	}
	return out, nil
}

// TurnsRow is one node's head row for one seat on one day: its name as that
// day's record named it, and how many of its turns ended and failed.
type TurnsRow struct {
	Day, Node, AgentID string
	Handle, Role       string
	Turns, Failed      int64
}

// SeatTurns reads every node's turn counts for the company days
// q.From..q.To, inclusive, in (day, node, seat) order — the half of a spend
// answer that says how many turns the tokens were spent over.
//
// A HEAD ROW WITH NO SPEND IS STILL READ: a seat that ended a turn without a
// model call (a turn that failed before its first round) ended a turn, and a
// per-seat table that dropped it would say the seat did nothing.
func SeatTurns(ctx context.Context, estate Estate, q SpendQuery) ([]TurnsRow, error) {
	if err := q.check(); err != nil {
		return nil, err
	}
	var out []TurnsRow
	err := estate.Read(ctx, func(tx *sql.Tx) error {
		rows, err := tx.QueryContext(ctx, `
			SELECT day, node, agent_id, handle, role, turns, failed
			  FROM usage_turns
			 WHERE day >= ? AND day <= ? AND (? = '' OR agent_id = ?)
			 ORDER BY day, node, agent_id`,
			q.From, q.To, q.AgentID, q.AgentID)
		if err != nil {
			return err
		}
		defer func() { _ = rows.Close() }()
		for rows.Next() {
			var r TurnsRow
			if err := rows.Scan(&r.Day, &r.Node, &r.AgentID, &r.Handle, &r.Role,
				&r.Turns, &r.Failed); err != nil {
				return err
			}
			out = append(out, r)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, fmt.Errorf("usage: read the turns for %s..%s: %w", q.From, q.To, err)
	}
	return out, nil
}
