package store

import (
	"cmp"
	"context"
	"database/sql"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/crewlet/crewlet/internal/mcp"
)

// A server's recent calls, read out of the phase records that made them: turn
// detail, not a table of its own. Each call is already on the phase record that
// made it (`tool_executions`, types.ToolExecution), in the event log of the
// node that ran the phase, so it is read there at query time
// (internal/eventfan, ADR-0021); only the count is replicated
// (`usage_tool_calls`). A listing never selects the payload, so rows are
// narrowed by [ExtractTags]' `mcp_servers` before any tool log is walked.
//
// Never an argument, never a result: those are a model's and a server's words
// and can hold a credential or customer data. A row is the engine's own
// account: tool, seat, instant, outcome, class of a refusal, duration.

// ToolCallQuery asks for one server's newest calls.
type ToolCallQuery struct {
	// Server is the bare MCP server name.
	Server string

	// Since is the oldest record read, inclusive; floored at the history
	// this log answers for.
	Since time.Time

	// Limit is how many calls; at most [MaxToolCalls].
	Limit int

	// At is the asker's instant, which the history is floored at and the
	// window ends before ([ListQuery.At]'s reason). Zero is now.
	At time.Time
}

// MaxToolCalls is the most calls one read answers.
//
// FIFTY: a server's recent calls are a table on its page, and fifty rows are
// two screens of it, which is what a person scans for a pattern before they
// open a turn. An older call is that turn's detail, reached through the turns
// that called the tool (`GET /turns?server=&tool=`).
const MaxToolCalls = 50

// ToolCall is one call to a server's tool, as its phase record states it.
type ToolCall struct {
	// At is when the call was handed to the tool, or, for a call nothing
	// timed (one refused before it ran), when its phase was recorded.
	At time.Time `json:"at"`

	// AgentID is the seat that made the call, and TurnID the run it was
	// made in.
	AgentID string `json:"agent_id"`
	TurnID  string `json:"turn_id"`

	// Tool is the name the call used, as the seat was offered it.
	Tool string `json:"tool"`

	// Outcome is which of the four the call is in ([mcp.OutcomeOf]), and
	// Refusal the class of a failure the engine classified, "" otherwise.
	Outcome mcp.CallOutcome `json:"outcome"`
	Refusal mcp.Refusal     `json:"refusal,omitempty"`

	// DurationMS is how long the call took, nil for a call nothing timed:
	// absent means "not measured", never "instant".
	DurationMS *int64 `json:"duration_ms,omitempty"`

	// EventID and Index place the call: its phase record, and its place in
	// that record's tool log. They break a tie between two calls at one
	// instant, the same way on every node.
	EventID string `json:"event_id"`
	Index   int    `json:"index"`
}

// Before reports whether c lists after o, newest first: a later instant
// first, and within one instant the later record and the later call.
func (c ToolCall) Before(o ToolCall) bool { return compareToolCalls(c, o) < 0 }

// compareToolCalls is the listing's order, newest first.
func compareToolCalls(a, b ToolCall) int {
	return cmp.Or(b.At.Compare(a.At), cmp.Compare(b.EventID, a.EventID), cmp.Compare(b.Index, a.Index))
}

// SortToolCalls puts calls in the listing's order, newest first.
func SortToolCalls(calls []ToolCall) { slices.SortFunc(calls, compareToolCalls) }

// toolCallsSQL is [EventLog.ToolCalls]' statement and its arguments: the phase
// records in the window whose calls named the server, newest first through the
// (type, time) index, each record's calls to the server's tools through
// `json_each`, and only the rows this log keeps ([keptRowSQL]), so a custody
// batch two data nodes hold is listed by whichever keeps it.
//
// THE TAG IS READ BEFORE THE PAYLOAD: it names only the record, so it is
// tested on the record's own row, and the tool log is walked for the records
// it passes.
func toolCallsSQL(server string, since, at time.Time) (string, []any) {
	return `
		SELECT e.event_id, e.event_time, e.agent_id, e.turn_id, x.key,
		       COALESCE(json_extract(x.value, '$.name'), ''),
		       COALESCE(json_extract(x.value, '$.origin'), ''),
		       COALESCE(json_extract(x.value, '$.refusal'), ''),
		       COALESCE(json_extract(x.value, '$.success'), 0),
		       COALESCE(json_extract(x.value, '$.started_at'), ''),
		       json_extract(x.value, '$.duration_ms')
		  FROM crewlet_events AS e, json_each(e.payload, '$.tool_executions') AS x
		 WHERE e.event_type = ? AND e.event_time >= ? AND e.event_time < ?
		   AND instr(COALESCE(json_extract(e.tags, '$.` + mcpServersTagKey + `'), ''), ?) > 0
		   AND ` + keptRowSQL + `
		   AND json_extract(x.value, '$.source') = ?
		 ORDER BY e.event_time DESC, e.event_id DESC`,
		[]any{phaseCompleted, EncodeTime(since), EncodeTime(at),
			mcpServerTagNeedle(server), mcp.Origin(server)}
}

// ToolCalls answers q.Server's newest calls in this log, at most q.Limit,
// newest first ([ToolCall.Before]).
//
// IT STOPS READING once nothing older can be newer than what it holds: a
// call is handed to its tool before its phase is recorded, so once the
// records reach an instant older than the q.Limit-th newest call held, no
// call in any older record can list above it. A busy server is answered from
// its newest records alone; a quiet one walks the window, reading the payload
// only of the records its tag names.
func (l *EventLog) ToolCalls(ctx context.Context, q ToolCallQuery) ([]ToolCall, error) {
	if strings.TrimSpace(q.Server) == "" {
		return nil, fmt.Errorf("store: a server's calls name the server")
	}
	limit := q.Limit
	if limit <= 0 || limit > MaxToolCalls {
		limit = MaxToolCalls
	}
	at := askedAt(q.At)
	since := q.Since.UTC()
	if floor := at.Add(-EventHistory); since.Before(floor) {
		since = floor
	}
	var out []ToolCall
	err := l.db.Read(ctx, func(tx *sql.Tx) error {
		query, args := toolCallsSQL(q.Server, since, at)
		rows, err := tx.QueryContext(ctx, query, args...)
		if err != nil {
			return err
		}
		defer func() { _ = rows.Close() }()
		for rows.Next() {
			var (
				c                      ToolCall
				recorded               int64
				origin, refusal, start string
				success                bool
				duration               sql.NullInt64
			)
			if err := rows.Scan(&c.EventID, &recorded, &c.AgentID, &c.TurnID, &c.Index,
				&c.Tool, &origin, &refusal, &success, &start, &duration); err != nil {
				return err
			}
			phaseAt := DecodeTime(recorded)
			if len(out) >= limit && phaseAt.Before(out[limit-1].At) {
				// Every call left was handed to its tool no later than
				// its record, which is older than the newest `limit`.
				break
			}
			c.At = phaseAt
			if start != "" {
				if t, err := time.Parse(time.RFC3339Nano, start); err == nil {
					c.At = t.UTC()
				}
			}
			c.Refusal = mcp.Refusal(refusal)
			c.Outcome = mcp.OutcomeOf(origin, c.Refusal, !success)
			if duration.Valid {
				ms := duration.Int64
				c.DurationMS = &ms
			}
			out = append(out, c)
			SortToolCalls(out)
			if len(out) > limit {
				out = out[:limit]
			}
		}
		return rows.Err()
	})
	if err != nil {
		return nil, fmt.Errorf("store: read %s's recent calls: %w", q.Server, err)
	}
	return out, nil
}
