package store

import (
	"context"
	"database/sql"
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"

	"github.com/crewlet/crewlet/internal/events/types"
)

// The list of turns, which is the one view of a working company that did not
// exist.
//
// # There was no list of turns anywhere
//
// A turn is the unit of work this engine does: a wake, a decision, some tool
// rounds, a reply. Everything else is a projection of one — the spend rollup,
// the seat page, the item's history — and none of them is a list of them. The
// dashboard faked one by paging the raw event feed sixty-one times and folding
// the rows in the browser, which is slow, wrong at the page boundary (a turn
// straddling two pages appeared twice), and capped at whatever the caller gave
// up on.
//
// # It is an aggregate over columns, not over payloads
//
// Migration 0015 promoted a phase's numbers out of its payload — the model,
// the phase, the iteration and the three token counts are columns now — and
// 0018 indexed `turn_id`. So one row per turn is a GROUP BY over narrow
// values, not a fold over documents.
//
// The exception is the three facts that only the COMPLETION event knows: how
// long the turn took, what it concluded, and what it set out to do. Those are
// read with `json_extract` from the one row per turn that carries them, which
// is why the CASE is gated on the event type rather than applied to every row
// — a turn has one completion and sixty phases.

// The two event types this fold is keyed on, taken from the payload types
// themselves rather than spelled here: a literal would be the one place the
// wire name silently stops matching.
var (
	phaseCompleted = types.AgentPhaseCompleted{}.EventType()
	turnCompleted  = types.TurnCompleted{}.EventType()
)

// failedRow is the per-row predicate "this event reports a failure", and it is
// [types.Failed]'s rule written for SQL rather than a fourth answer to the same
// question.
//
// THE TAG IS ONLY HALF OF IT. The writer stamps `failed` from a payload field,
// and the four types that ARE a failure by their very name carry no such field:
// llm_unavailable, budget_exhausted, turn.guard_breach and sandbox_run_failed.
// Those are precisely the records a turn that died BEFORE completing a phase
// leaves behind — a seat refused at the budget gate, a chain whose every model
// was down, a breached guard — so on the tag alone this aggregate answered "not
// failed" for a turn that never ran, listing it as clean with no completion
// record, which the dashboard draws identically to a turn still in flight. The
// event reader already applies the rule ([EventLog] stamps Failed on read) and
// so does the live projection, which is the disagreement types.Failed's own doc
// warns about.
//
// The names come from the map rather than a literal, for the reason the two
// event types above are taken from their payload types: a spelling written here
// is the one place a wire name silently stops matching.
func failedRow() (string, []any) {
	names := slices.Sorted(maps.Keys(types.FailureEventTypes))
	args := make([]any, 0, len(names))
	for _, name := range names {
		args = append(args, name)
	}
	holders := strings.TrimSuffix(strings.Repeat("?,", len(names)), ",")
	return "(json_extract(tags, '$.failed') = 'true' OR event_type IN (" +
		holders + "))", args
}

// Turn is one unit of agent work, as a list row.
type Turn struct {
	// TurnID is ONE RUN of a turn. Two attempts at one trigger are two
	// rows here, and deliberately: they ran at different times, on
	// different models, and one can fail where the next succeeds. Folding
	// them was what made a turn that recovered from an auth failure read
	// as permanently failed, with both attempts' tokens summed. See
	// ADR-0017.
	TurnID string `json:"turn_id"`

	// WorkKey is the unit of work those runs share — what a reader follows
	// to find the other attempts at the same trigger. Empty for a turn
	// with no ledgerable trigger, and for rows written before the two
	// identities were split (schema/0029 backfills those from turn_id).
	WorkKey string `json:"work_key,omitempty"`

	AgentID   string `json:"agent_id,omitempty"`
	AgentRole string `json:"role,omitempty"`

	// StartedAt and EndedAt are the span of the turn's own EVENTS, which
	// is not the same as the duration the completion event reports: the
	// span covers the reflection pass that publishes after the turn ends,
	// and the duration is what the turn itself measured.
	StartedAt time.Time `json:"started_at"`
	EndedAt   time.Time `json:"ended_at"`

	// DurationMS is the turn's OWN measurement, from its completion
	// record, and zero for a turn that has not finished — which is a
	// meaningful zero here, because `Complete` says which.
	DurationMS int `json:"duration_ms"`

	// Complete is whether a completion record exists. A turn with none is
	// either still running or died mid-flight, and those look identical
	// from here: the span's end says which is more likely, and the event
	// list says for certain.
	Complete bool `json:"complete"`

	// Phases counts the phase completions, and Iterations is the highest
	// iteration any of them reached: SELF-ITERATE rounds, the executor →
	// reviewer → executor loop.
	//
	// NOT the tool rounds a phase used — that is `rounds_used`, on each
	// phase's own record, and it is a per-phase figure this aggregate has
	// no column for. Both were called "rounds": a one-iteration turn listed
	// "Rounds 1" directly above phase rows reading "3r" and "1r" for the
	// same turn, and the word was the whole of the contradiction.
	Phases     int `json:"phases"`
	Iterations int `json:"iterations"`

	// Failed is whether ANY event of this turn was a failure, which is a
	// different question from the outcome: a turn can recover from a
	// failed provider call and still end well, and an operator looking for
	// what is going wrong wants both.
	Failed bool `json:"failed"`

	InputTokens  int `json:"input_tokens"`
	OutputTokens int `json:"output_tokens"`
	TotalTokens  int `json:"total_tokens"`

	// Models is every distinct model the turn used, comma-joined by the
	// read because a turn routinely uses two — a cheap one for the
	// extension judge, the seat's own for the work.
	Models string `json:"models,omitempty"`

	// Summary is what the turn set out to do, in the agent's own words.
	Summary string `json:"summary,omitempty"`

	// Trigger is what woke it, from the first phase record.
	Trigger string `json:"trigger,omitempty"`

	TaskID string `json:"task_id,omitempty"`
}

// TurnQuery selects a page of turns.
type TurnQuery struct {
	// SinceDays is the window in whole days back from now. Zero takes
	// [DefaultTurnDays].
	SinceDays int

	// AgentRole and AgentID each narrow to one seat; a caller passes
	// whichever it holds, for [EventLog.AgentPhases]' own reason.
	AgentRole string
	AgentID   string

	// Model narrows to turns that used one.
	Model string

	// WorkKey narrows to every RUN of one unit of work — the attempts at a
	// trigger that was redelivered. It is the one question the turns list
	// could not ask while a turn id WAS the work key, because then the two
	// were the same query. See ADR-0017.
	WorkKey string

	// Failed narrows to turns that carried a failure, or to those that did
	// not. Nil is both, which is not the same as false.
	Failed *bool

	// Before is an exclusive cursor on the turn's START, which is what the
	// listing is ordered by.
	Before time.Time

	Limit int
}

const (
	// DefaultTurnDays is the window a caller that names none gets — the
	// spend rollup's, so an unparameterised turns list and an
	// unparameterised cost breakdown describe the same week.
	DefaultTurnDays = DefaultPhaseTokenDays

	// MaxTurnDays bounds what a caller may ask for, at the same retention
	// horizon: asking for more cannot return more.
	MaxTurnDays = MaxPhaseTokenDays

	// DefaultTurnPage is one page for a caller that names no size.
	//
	// FIFTY. A turn row is narrow — no payload, no prompts — so the page is
	// sized to what a person scans rather than to what the wire can carry.
	DefaultTurnPage = 50

	// MaxTurnPage is the ceiling, at TWO HUNDRED: past it a list stops
	// being one and becomes a report.
	MaxTurnPage = 200
)

// Turns lists one row per turn, newest first.
func (l *EventLog) Turns(ctx context.Context, q TurnQuery) ([]Turn, error) {
	days := q.SinceDays
	switch {
	case days <= 0:
		days = DefaultTurnDays
	case days > MaxTurnDays:
		days = MaxTurnDays
	}
	limit := q.Limit
	switch {
	case limit <= 0:
		limit = DefaultTurnPage
	case limit > MaxTurnPage:
		limit = MaxTurnPage
	}

	// THE FLOOR IS THE HISTORY WINDOW as well as the caller's, for the
	// reason `agentPhaseSQL`'s own comment gives: a read without it answers
	// below the retention horizon on a node whose sweep is behind, and a
	// turns list showing turns the spend rollup excludes is the exact
	// comparison an operator makes to decide whether a seat went quiet.
	floor := now().Add(-time.Duration(days) * 24 * time.Hour)
	if history := now().Add(-EventHistory); history.After(floor) {
		floor = history
	}

	where := []string{"turn_id != ''", "event_time >= ?"}
	args := []any{EncodeTime(floor)}
	// ONLY THE IDENTIFIERS THE CALLER HOLDS — binding an empty one matches
	// every row that carries none, which is every non-agent event in the
	// window. See [seatClause].
	if clause, ids := seatClause(q.AgentID, q.AgentRole); clause != "" {
		where = append(where, strings.TrimPrefix(clause, " AND "))
		args = append(args, ids...)
	}
	if q.WorkKey != "" {
		where = append(where, "work_key = ?")
		args = append(args, q.WorkKey)
	}
	if q.Model != "" {
		// ON THE TURN, not on the row: a turn is selected when ANY of
		// its phases used the model, which is what a reader means by
		// "turns on the cheap model".
		where = append(where,
			"turn_id IN (SELECT turn_id FROM crewlet_events "+
				"WHERE model = ? AND event_time >= ? AND turn_id != '')")
		args = append(args, q.Model, EncodeTime(floor))
	}

	having := []string{}
	if q.Before.IsZero() {
		// no cursor
	} else {
		having = append(having, "MIN(event_time) < ?")
		args = append(args, EncodeTime(q.Before))
	}
	// ONE EXPRESSION FOR THE COLUMN AND THE FILTER. Spelled twice, a turn
	// could be selected by `failed=true` and then render without the mark,
	// which is the same defect one layer down from the one [failedRow]
	// describes.
	failedExpr, failedArgs := failedRow()
	failedAgg := "MAX(CASE WHEN " + failedExpr + " THEN 1 ELSE 0 END)"
	if q.Failed != nil {
		want := "0"
		if *q.Failed {
			want = "1"
		}
		having = append(having, failedAgg+" = "+want)
		args = append(args, failedArgs...)
	}
	havingSQL := ""
	if len(having) > 0 {
		havingSQL = " HAVING " + strings.Join(having, " AND ")
	}
	args = append(args, limit)

	// NULLIF ON THE MODEL, because `model` is `TEXT NOT NULL DEFAULT ''`
	// and only a phase record carries one. Every turn's group also holds
	// its `turn_completed` row, so GROUP_CONCAT — which skips NULLs but
	// not empty strings — joined one model as ",claude-opus-5" and handed
	// every consumer splitting on the comma a nameless band in its legend
	// and a nameless row in its breakdown.
	rows, err := l.db.sql.QueryContext(ctx, `
		SELECT turn_id,
		       MAX(work_key),
		       MAX(agent_id), MAX(agent_role),
		       MIN(event_time), MAX(event_time),
		       SUM(CASE WHEN event_type = ? THEN 1 ELSE 0 END),
		       MAX(iteration),
		       `+failedAgg+`,
		       SUM(input_tokens), SUM(output_tokens), SUM(total_tokens),
		       GROUP_CONCAT(DISTINCT NULLIF(model, '')),
		       MAX(CASE WHEN event_type = ? THEN 1 ELSE 0 END),
		       MAX(CASE WHEN event_type = ?
		                THEN COALESCE(json_extract(payload, '$.duration_ms'), 0) END),
		       MAX(CASE WHEN event_type = ?
		                THEN COALESCE(json_extract(payload, '$.plan_summary'), '') END),
		       MAX(CASE WHEN event_type = ?
		                THEN COALESCE(json_extract(payload, '$.task_id'), '') END),
		       MAX(COALESCE(json_extract(tags, '$.trigger'), ''))
		  FROM crewlet_events
		 WHERE `+strings.Join(where, " AND ")+`
		 GROUP BY turn_id`+havingSQL+`
		 ORDER BY MIN(event_time) DESC
		 LIMIT ?`,
		// The SELECT list's own placeholders, in the order they appear
		// in it: the phase count, then the failure predicate, then the
		// four reads off the completion row.
		slices.Concat([]any{phaseCompleted}, failedArgs,
			[]any{turnCompleted, turnCompleted, turnCompleted, turnCompleted},
			args)...)
	if err != nil {
		return nil, fmt.Errorf("store: list turns: %w", err)
	}
	defer func() { _ = rows.Close() }()

	out := []Turn{}
	for rows.Next() {
		var (
			t                 Turn
			workKey           sql.NullString
			agentID, role     sql.NullString
			started, ended    int64
			failed, complete  int
			models, summary   sql.NullString
			taskID, trigger   sql.NullString
			in, outTok, total sql.NullInt64
			duration          sql.NullInt64
		)
		if err := rows.Scan(&t.TurnID, &workKey, &agentID, &role, &started, &ended,
			&t.Phases, &t.Iterations, &failed, &in, &outTok, &total, &models,
			&complete, &duration, &summary, &taskID, &trigger); err != nil {

			return nil, fmt.Errorf("store: scan a turn: %w", err)
		}
		t.WorkKey = workKey.String
		t.AgentID, t.AgentRole = agentID.String, role.String
		t.StartedAt, t.EndedAt = DecodeTime(started), DecodeTime(ended)
		t.Failed = failed != 0
		t.Complete = complete != 0
		t.DurationMS = int(duration.Int64)
		t.InputTokens = int(in.Int64)
		t.OutputTokens = int(outTok.Int64)
		t.TotalTokens = int(total.Int64)
		t.Models, t.Summary = models.String, summary.String
		t.TaskID, t.Trigger = taskID.String, trigger.String
		out = append(out, t)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: list turns: %w", err)
	}
	return out, nil
}
