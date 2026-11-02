package usage

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"

	"github.com/crewlet/crewlet/internal/store"
)

// A server's calls per tool, as the fleet holds them: every data node's
// seat-days summed over a range of company days, per (tool, seat).
//
// THE COUNTS ARE THE DOMAIN'S, the calls are not. Which call ran when, for
// whom and how it ended is turn detail, read from the event logs of the nodes
// that ran it (internal/eventfan, ADR-0021); what is replicated here is how
// many, so the counts outlive the node that made the calls and a range of
// months is answered from rows rather than from thirty days of a log.

// ServerToolCallsQuery is a range of company days, both inclusive, and the server
// whose tools are counted.
type ServerToolCallsQuery struct {
	// From and To are company day labels (`2026-09-23`).
	From, To string

	// Server is the bare MCP server name.
	Server string
}

// ServerToolCallsRow is one seat's calls to one of the server's tools over the
// range, summed over every data node and every day.
type ServerToolCallsRow struct {
	Tool, AgentID string

	// Handle is the seat's handle as its days' head rows named it: what a
	// reader shows for a seat the company no longer holds. A handle never
	// changes for a seat (ADR-0013), so any day's names it.
	Handle string

	Calls, Failed, Unavailable, Refused, TotalMS int64

	// LastAt is the newest call that ran, zero when none did.
	LastAt time.Time
}

// serverToolCallsSQL is [ServerToolCalls]' statement: the server's rows over the
// range, through `usage_tool_calls_server_idx` (replicated 0074), summed per
// (tool, seat), each row's seat named by the head row of its own seat-day,
// which every seat record writes (a probe of that row's key).
const serverToolCallsSQL = `
	SELECT t.tool, t.agent_id, COALESCE(MAX(h.handle), ''),
	       SUM(t.calls), SUM(t.failed), SUM(t.unavailable), SUM(t.refused),
	       SUM(t.total_ms), MAX(t.last_at)
	  FROM usage_tool_calls AS t
	  LEFT JOIN usage_turns AS h
	    ON h.day = t.day AND h.node = t.node AND h.agent_id = t.agent_id
	 WHERE t.server = ? AND t.day >= ? AND t.day <= ?
	 GROUP BY t.tool, t.agent_id
	 ORDER BY t.tool, t.agent_id`

// ServerToolCalls reads every data node's calls to q.Server's tools over the
// company days q.From..q.To, inclusive, one row per (tool, seat) in that
// order.
func ServerToolCalls(ctx context.Context, estate Estate, q ServerToolCallsQuery) ([]ServerToolCallsRow, error) {
	if err := (SpendQuery{From: q.From, To: q.To}).check(); err != nil {
		return nil, err
	}
	if strings.TrimSpace(q.Server) == "" {
		return nil, fmt.Errorf("usage: a server's tool calls name the server")
	}
	var out []ServerToolCallsRow
	err := estate.Read(ctx, func(tx *sql.Tx) error {
		rows, err := tx.QueryContext(ctx, serverToolCallsSQL, q.Server, q.From, q.To)
		if err != nil {
			return err
		}
		defer func() { _ = rows.Close() }()
		for rows.Next() {
			var (
				r    ServerToolCallsRow
				last int64
			)
			if err := rows.Scan(&r.Tool, &r.AgentID, &r.Handle, &r.Calls, &r.Failed,
				&r.Unavailable, &r.Refused, &r.TotalMS, &last); err != nil {
				return err
			}
			if last != 0 {
				r.LastAt = store.DecodeTime(last)
			}
			out = append(out, r)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, fmt.Errorf("usage: read the calls to %s's tools for %s..%s: %w",
			q.Server, q.From, q.To, err)
	}
	return out, nil
}

// ToolsElided is how many (server, tool) entries the seat-days of the
// company days from..to, inclusive, dropped at their cap ([ToolsPerSeatDay]),
// summed over every data node: 0 when every count in the range is whole. A
// seat-day's cap is not per server, so a nonzero answer says the counts of a
// range MAY be short, never which server's are.
func ToolsElided(ctx context.Context, estate Estate, from, to string) (int64, error) {
	if err := (SpendQuery{From: from, To: to}).check(); err != nil {
		return 0, err
	}
	var total int64
	err := estate.Read(ctx, func(tx *sql.Tx) error {
		return tx.QueryRowContext(ctx, `
			SELECT COALESCE(SUM(tools_elided), 0) FROM usage_turns
			 WHERE day >= ? AND day <= ? AND tools_elided > 0`,
			from, to).Scan(&total)
	})
	if err != nil {
		return 0, fmt.Errorf("usage: count the elided tool calls for %s..%s: %w", from, to, err)
	}
	return total, nil
}
