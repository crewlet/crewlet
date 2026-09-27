package store

import (
	"context"
	"database/sql"
	"fmt"
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
// A spend record's numbers are columns — its model, phase, iteration and three
// token counts (schema/0015) and its per-model split (schema/0032) — written
// for every spend record ([spendEventTypes]), a phase's and an auxiliary
// call's alike, and schema/0018 indexes `turn_id`. So one row per turn is a
// GROUP BY that parses no spend record's payload, and the models a page's
// turns used are a second read of the same rows ([turnModelsSQL]).
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

	// InputTokens, OutputTokens and TotalTokens are the turn's spend
	// records summed: its phase records — a coding run's own spend among
	// them, on the record of the phase that collected it — and the
	// auxiliary calls made for it, which name the turn. That is the set
	// the spend rollup's per-turn row sums (internal/tokens), so the two
	// agree on what a turn cost.
	InputTokens  int `json:"input_tokens"`
	OutputTokens int `json:"output_tokens"`
	TotalTokens  int `json:"total_tokens"`

	// Models is every distinct model name those spend records carry, sorted
	// — a turn routinely has several: a cheap one for the extension judge,
	// the seat's own for the work, the auxiliary model for the calls made
	// for it. A record carries names in two places, and both count: its
	// `model`, the one its first completion reported (or the provider key
	// it ran on where it names none), and every entry of its per-model
	// split — a fallback that took over mid-phase, the models a collected
	// coding run reported ([turnModelsSQL] reads both). An entry reported under
	// no name adds nothing, so the rollup's `unknown` and `unmeasured` rows,
	// which are spend without a model's name, are never here.
	//
	// A LIST OF NAMES, not an account: which of these the tokens above are
	// counted under is the spend rollup's by-model breakdown, which ignores
	// a split whose figures do not fit inside its record.
	Models []string `json:"models,omitempty"`

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

	// Model narrows to turns that used one: whose spend records carry the
	// name in either place [Turn.Models] reads.
	Model string

	// WorkKey narrows to every RUN of one unit of work — the attempts at a
	// trigger that was redelivered. It is the one question the turns list
	// could not ask while a turn id WAS the work key, because then the two
	// were the same query. See ADR-0017.
	WorkKey string

	// Failed narrows to turns that carried a failure, or to those that did
	// not. Nil is both, which is not the same as false.
	Failed *bool

	// Before is an exclusive cursor: the last row of the previous page, as
	// its START and its TURN ID. Nil starts at the newest turn.
	//
	// ON THE START, because that is what the listing is ordered by — a
	// keyset on any one event would page a turn twice. AND ON THE ID, because
	// a start is not unique: it is an event's time, which [Cursor] says is not
	// unique either, and a cursor on the start alone steps over every other
	// turn that began at the boundary's instant, silently. Both halves are
	// required, and a cursor without the id is refused with [ErrHalfCursor].
	Before *Cursor

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

// Turns lists one page of turns, newest first, and whether the window holds
// more past it.
//
// THE SECOND RETURN IS READ, NOT INFERRED: one turn past the limit is read as
// the evidence and dropped (see [probed]). A page that merely FILLED says
// nothing — a window of exactly `limit` turns and one of ten thousand answer
// with the same rows — so a caller drawing anything from the page (a count, an
// axis) must say it is a page when this is true. The rest is the next page:
// the last row's start and id handed back as [TurnQuery.Before].
func (l *EventLog) Turns(ctx context.Context, q TurnQuery) ([]Turn, bool, error) {
	if q.Before != nil && q.Before.ID == "" {
		return nil, false, fmt.Errorf("%w: resume after the last row's turn_id, not its start alone",
			ErrHalfCursor)
	}
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

	// THE ROWS a turn is folded from. Every term here is on the ROW, and the
	// models read after the page takes the same terms, so a row's list of
	// models describes the rows its figures were summed from.
	rowTerms := []string{"turn_id != ''", "event_time >= ?"}
	rowArgs := []any{EncodeTime(floor)}
	// ONLY THE IDENTIFIERS THE CALLER HOLDS — binding an empty one matches
	// every row that carries none, which is every non-agent event in the
	// window. See [seatClause].
	if clause, ids := seatClause(q.AgentID, q.AgentRole); clause != "" {
		rowTerms = append(rowTerms, strings.TrimPrefix(clause, " AND "))
		rowArgs = append(rowArgs, ids...)
	}
	if q.WorkKey != "" {
		rowTerms = append(rowTerms, "work_key = ?")
		rowArgs = append(rowArgs, q.WorkKey)
	}
	where := slices.Clone(rowTerms)
	args := slices.Clone(rowArgs)
	if q.Model != "" {
		// ON THE TURN, not on the row: a turn is selected when ANY of its
		// spend records carries the model — as its own `model` or in its
		// per-model split, the two places [Turn.Models] reads — which is
		// what a reader means by "turns on the cheap model". Over the
		// window alone, as the page's own rows are.
		where = append(where, "turn_id IN ("+turnsUsingSQL("turn_id != '' AND event_time >= ?")+")")
		args = append(args, EncodeTime(floor), q.Model, q.Model)
	}

	having := []string{}
	if q.Before != nil {
		// THE PAIR, compared as the ORDER BY below sorts it, so every turn
		// strictly after the cursor's position in that order is on the next
		// page — including one that began at the cursor's own instant.
		having = append(having, "(MIN(event_time), turn_id) < (?, ?)")
		args = append(args, EncodeTime(q.Before.Time), q.Before.ID)
	}
	if q.Failed != nil {
		if *q.Failed {
			having = append(having, "MAX(CASE WHEN json_extract(tags, '$.failed') = 'true' THEN 1 ELSE 0 END) = 1")
		} else {
			having = append(having, "MAX(CASE WHEN json_extract(tags, '$.failed') = 'true' THEN 1 ELSE 0 END) = 0")
		}
	}
	havingSQL := ""
	if len(having) > 0 {
		havingSQL = " HAVING " + strings.Join(having, " AND ")
	}
	args = append(args, limit+1)

	rows, err := l.db.sql.QueryContext(ctx, `
		SELECT turn_id,
		       MAX(work_key),
		       MAX(agent_id), MAX(agent_role),
		       MIN(event_time), MAX(event_time),
		       SUM(CASE WHEN event_type = ? THEN 1 ELSE 0 END),
		       MAX(iteration),
		       MAX(CASE WHEN json_extract(tags, '$.failed') = 'true' THEN 1 ELSE 0 END),
		       SUM(input_tokens), SUM(output_tokens), SUM(total_tokens),
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
		 ORDER BY MIN(event_time) DESC, turn_id DESC
		 LIMIT ?`,
		append([]any{phaseCompleted, turnCompleted, turnCompleted,
			turnCompleted, turnCompleted}, args...)...)
	if err != nil {
		return nil, false, fmt.Errorf("store: list turns: %w", err)
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
			summary           sql.NullString
			taskID, trigger   sql.NullString
			in, outTok, total sql.NullInt64
			duration          sql.NullInt64
		)
		if err := rows.Scan(&t.TurnID, &workKey, &agentID, &role, &started, &ended,
			&t.Phases, &t.Iterations, &failed, &in, &outTok, &total,
			&complete, &duration, &summary, &taskID, &trigger); err != nil {

			return nil, false, fmt.Errorf("store: scan a turn: %w", err)
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
		t.Summary = summary.String
		t.TaskID, t.Trigger = taskID.String, trigger.String
		out = append(out, t)
	}
	if err := rows.Err(); err != nil {
		return nil, false, fmt.Errorf("store: list turns: %w", err)
	}
	out, more := probed(out, limit)
	if err := l.nameTurnModels(ctx, out, rowTerms, rowArgs); err != nil {
		return nil, false, err
	}
	return out, more, nil
}

// splitEntryModel is one split entry's `model`, as json_each walks a row's
// `models` column (the row aliased `e`) one entry at a time (aliased `entry`).
// Addressed by the entry's path into the column rather than read out of the
// entry's own value, so an entry that is not an object answers NULL instead of
// failing the statement: json_extract parses its first argument, and a string
// entry's value is the bare text.
const splitEntryModel = `json_extract(e.models, entry.fullkey || '.model')`

// turnModelsSQL is the (turn_id, model) pairs the rows `rowTerms` selects carry,
// in the two places [Turn.Models] names: a row's own `model` where it is not
// empty, and the `model` of each entry of its per-model split that is a
// non-empty string. Its placeholders are the rows' own, once for each half
// ([turnModelArgs]).
//
// THE SPLIT IS EXPANDED IN SQL, with json_each over a column that holds a JSON
// array on every row (schema/0032), in the same terms [turnsUsingSQL] filters
// on, so a turn found by a model lists it.
func turnModelsSQL(rowTerms string) string {
	return `SELECT e.turn_id, e.model FROM crewlet_events e
	 WHERE ` + rowTerms + ` AND e.model <> ''
	UNION
	SELECT e.turn_id, ` + splitEntryModel + ` FROM crewlet_events e, json_each(e.models) entry
	 WHERE ` + rowTerms + ` AND e.models <> '[]'
	   AND json_type(e.models, entry.fullkey || '.model') = 'text' AND ` + splitEntryModel + ` <> ''`
}

// turnModelArgs is [turnModelsSQL]'s arguments: the rows' own, once for each
// half.
func turnModelArgs(rowArgs []any) []any {
	return append(slices.Clone(rowArgs), rowArgs...)
}

// turnsUsingSQL is the turns among the rows `rowTerms` selects that carry one
// model, in either place [turnModelsSQL] reads; its placeholders are the rows'
// own and then the name, twice.
//
// ONE PASS over the rows rather than a pass per place, because the rows are
// every row in the window and a row costs what it weighs to read, a phase
// record's whole payload included: two passes measured twice the time of one.
// And the name a caller filters on is never empty, so `= ?` matches exactly
// the names [turnModelsSQL] lists — no empty one, and nothing json_extract
// answers for an entry whose `model` is not a string.
func turnsUsingSQL(rowTerms string) string {
	return `SELECT e.turn_id FROM crewlet_events e
	 WHERE ` + rowTerms + ` AND (e.model = ? OR (e.models <> '[]' AND EXISTS (
	     SELECT 1 FROM json_each(e.models) entry WHERE ` + splitEntryModel + ` = ?)))`
}

// nameTurnModels fills in [Turn.Models] for a page of turns, from the same rows
// their figures were folded from.
//
// A SECOND READ, of the page's own turns through the turn index, rather than a
// column of the fold: the fold groups every turn in the window before the
// page is cut, and naming each one's models there would expand the splits of
// every spend record in the window to label the few turns a page shows.
func (l *EventLog) nameTurnModels(ctx context.Context, page []Turn, rowTerms []string, rowArgs []any) error {
	if len(page) == 0 {
		return nil
	}
	ids := make([]any, len(page))
	for i, t := range page {
		ids[i] = t.TurnID
	}
	terms := strings.Join(append(slices.Clone(rowTerms),
		"turn_id IN (?"+strings.Repeat(", ?", len(ids)-1)+")"), " AND ")
	rows, err := l.db.sql.QueryContext(ctx, turnModelsSQL(terms),
		turnModelArgs(append(slices.Clone(rowArgs), ids...))...)
	if err != nil {
		return fmt.Errorf("store: name the models of a page of turns: %w", err)
	}
	defer func() { _ = rows.Close() }()
	named := make(map[string][]string, len(page))
	for rows.Next() {
		var turnID, model string
		if err := rows.Scan(&turnID, &model); err != nil {
			return fmt.Errorf("store: name the models of a page of turns: scan: %w", err)
		}
		named[turnID] = append(named[turnID], model)
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("store: name the models of a page of turns: %w", err)
	}
	for i := range page {
		// SORTED, since the UNION already made each pair distinct and has no
		// order of its own: one turn read twice lists its models the same way.
		models := named[page[i].TurnID]
		slices.Sort(models)
		page[i].Models = models
	}
	return nil
}
