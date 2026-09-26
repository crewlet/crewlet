package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
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
// Migration 0015 promoted a phase's numbers out of its payload — the model,
// the phase, the iteration and the three token counts are columns now — and
// 0018 indexed `turn_id`. So one row per turn is a GROUP BY over narrow
// values, not a fold over documents.
//
// The exception is the facts that only the COMPLETION event knows: how long
// the turn took, what it set out to do, the item it was charged to, and
// whether it ENDED. Those are read with `json_extract` from the completion
// rows, which is why every such CASE is gated on the event type rather than
// applied to every row — a turn has a completion per segment and sixty
// phases.
//
// # A completion is not always an end
//
// A turn whose executor launches a detached coding run PARKS: the segment
// that launched it publishes `turn_completed` with `suspended: true`, and the
// same turn id completes again — possibly on another node, possibly after a
// restart — when the run is collected and the loop resumes. So one turn can
// hold several completion records, and "a completion exists" is not "the turn
// finished". Read that way, this list called a parked turn complete the
// moment it parked and reported the first segment's duration as the whole
// turn's. The NEWEST completion decides what the turn is now — finished, or
// parked waiting for its run — and the duration is the sum of every segment's
// own measurement, which is the time the turn spent working rather than the
// time it spent waiting. A completion that predates the flag names no
// `suspended` and folds as it always did: an end.
//
// # A lost run ends the turn that was waiting for it
//
// A parked turn resumes only when its run is collected, and a run that is LOST
// — its box unreachable, its claim stranded, its conversation gone — is settled
// like any other lost turn, announced by `sandbox_run_failed` and never
// resumed. Read by the completions alone that turn was parked for good: the
// failure is therefore an end, dated when it was announced, and the newest end
// still decides — a run that failed while still launching is followed by its
// turn's own completion, which is newer.

// The two event types this fold is keyed on, taken from the payload types
// themselves rather than spelled here: a literal would be the one place the
// wire name silently stops matching.
var (
	phaseCompleted = types.AgentPhaseCompleted{}.EventType()
	turnCompleted  = types.TurnCompleted{}.EventType()
	runLost        = types.SandboxRunFailed{}.EventType()
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
// The names come from the catalogue rather than a literal, for the reason the
// two event types above are taken from their payload types: a spelling written
// here is the one place a wire name silently stops matching. Through
// [types.FailureEventNames], which is the accessor that exists because this is
// the caller that has to ENUMERATE the set rather than test one value against
// it — the map behind it is unexported, so there is no second way in.
func failedRow() (string, []any) {
	names := types.FailureEventNames()
	args := make([]any, 0, len(names))
	for _, name := range names {
		args = append(args, name)
	}
	holders := strings.TrimSuffix(strings.Repeat("?,", len(names)), ",")
	return "(json_extract(tags, '$.failed') = 'true' OR event_type IN (" +
		holders + "))", args
}

// suspendedExpr is 1 for a completion record that PARKED its turn and 0 for
// every other row, a completion that predates the flag included — the flag is
// `omitempty` on the wire, so its absence is the ordinary end.
const suspendedExpr = "COALESCE(json_extract(payload, '$.suspended'), 0)"

// segmentState is what a turn is now, from the time of its newest completion
// that ENDED it and of its newest one that PARKED it (either absent).
//
// THE NEWEST DECIDES, so the two answers are exclusive: a turn that parked and
// then finished is complete, and one that parked again after a resume is
// parked. A turn with neither record is neither — running, or died mid-flight.
func segmentState(ended, parked *time.Time) (complete, isParked bool) {
	switch {
	case ended != nil && (parked == nil || !ended.Before(*parked)):
		return true, false
	case parked != nil:
		return false, true
	}
	return false, false
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

	// DurationMS is the turn's OWN measurement: the sum of what every one
	// of its completion records measured, so a turn that parked and resumed
	// counts both segments and not the wait between them. Zero for a turn
	// that has completed no segment — a meaningful zero here, because
	// `Complete` and `Parked` say which.
	DurationMS int `json:"duration_ms"`

	// Complete is whether the turn ENDED: its newest completion record is
	// not a suspension. A turn with no completion is either still running
	// or died mid-flight, and those look identical from here: the span's
	// end says which is more likely, and the event list says for certain.
	Complete bool `json:"complete"`

	// Parked is whether the turn is waiting on a detached coding run: its
	// newest completion record is a suspension, so it will complete again
	// when the run is collected. Never true beside Complete — the newest
	// completion is one or the other.
	Parked bool `json:"parked"`

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

	// CacheRead and CacheWrite are the share of InputTokens the provider's
	// prompt cache served and stored, summed over the turn's phases off the
	// columns schema/0030 promoted — a BREAKDOWN of the input, never an
	// addition to it (see tokens.Bucket), so TotalTokens is unchanged by
	// them.
	CacheRead  int `json:"cache_read_tokens"`
	CacheWrite int `json:"cache_write_tokens"`

	// Models is every distinct model the turn used, comma-joined by the
	// read because a turn routinely uses two — a cheap one for the
	// extension judge, the seat's own for the work.
	Models string `json:"models,omitempty"`

	// Summary is what the turn set out to do, in the agent's own words.
	Summary string `json:"summary,omitempty"`

	// Trigger is what woke it, from the first phase record.
	Trigger string `json:"trigger,omitempty"`

	// WorkItem is the one work item the turn is charged to, read off its
	// completion record, and absent for a turn on nothing — including one
	// still running, since a sole write names its item only at the end.
	//
	// It replaces a `task_id` that read a completion field no build ever
	// assigned, so every row listed no item; `task_id` means a delegated
	// worker's own task or a schedule fire's run, and is never joined to a
	// tracker item.
	WorkItem *types.WorkItem `json:"work_item,omitempty"`
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

	// WorkItem narrows to the turns on one work item, by its identity
	// across trackers — `<backend>:<id>`, [types.WorkItem.Ref]. ON THE TURN,
	// like Model: a turn is selected when any of its records names the item,
	// and every record it has is folded, so the row is the same row the
	// unfiltered list shows. See schema/0031 for the column and its index.
	WorkItem string

	// Failed narrows to turns that carried a failure, or to those that did
	// not. Nil is both, which is not the same as false.
	Failed *bool

	// Before is an exclusive cursor on the turn's START, which is what the
	// listing is ordered by. Meaningless under [TurnSortTokens], which is a
	// ranking rather than a walk and has nothing to resume from.
	Before time.Time

	// Sort is the order the page is cut in. The zero value is
	// [TurnSortStarted], newest first, which is what every caller that names
	// none has always had.
	Sort TurnSort

	// IDs, when set, asks for this log's SHARE of exactly these turns
	// rather than for a page — see [EventLog.TurnPartials]. Capped by the
	// caller at [MaxTurnPage] per share: it is the ids of pages somebody
	// else already cut.
	IDs []string

	Limit int
}

// TurnSort is the order a turns page is cut in.
//
// A CLOSED SET with a Valid method, for the reason every enum here is one: a
// value off the wire that this build does not know is refused naming the ones
// it does, never read as the default — a page asked for "the costliest" and
// answered newest-first is a page of the wrong turns under the right heading.
type TurnSort string

const (
	// TurnSortStarted is newest START first, and the only order a cursor
	// can walk: the start is what [TurnQuery.Before] is a position on.
	TurnSortStarted TurnSort = "-started"

	// TurnSortTokens is the most TOTAL TOKENS first — the spend screen's
	// "which turns cost the most" drill-down, per turn rather than per day,
	// which is the one question the daily usage rows cannot answer. Ties
	// break newest first, so two equal turns come back in a stable order.
	TurnSortTokens TurnSort = "-tokens"
)

// TurnSorts is the closed set, in the order a screen offers them.
var TurnSorts = []TurnSort{TurnSortStarted, TurnSortTokens}

// Valid reports whether s is an order this build cuts pages in. The zero
// value is valid and is [TurnSortStarted].
func (s TurnSort) Valid() bool {
	return s == "" || slices.Contains(TurnSorts, s)
}

// orderSQL is the ORDER BY the sort compiles to. The expressions are this
// file's own constants, never the caller's text.
func (s TurnSort) orderSQL() string {
	if s == TurnSortTokens {
		return "SUM(total_tokens) DESC, MIN(event_time) DESC"
	}
	return "MIN(event_time) DESC"
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
	parts, err := l.TurnPartials(ctx, q)
	if err != nil {
		return nil, err
	}
	out := make([]Turn, 0, len(parts))
	for _, p := range parts {
		out = append(out, p.Turn())
	}
	return out, nil
}

// TurnPartial is what ONE event log holds of one turn: the aggregate [Turn] is
// folded from, before it is folded.
//
// # Why a turn has partials at all
//
// A turn's events are written to the event log of the node that PUBLISHED
// them, and a turn is not always published by one node: a turn parked on a
// detached coding run resumes wherever its seat is held when the run is
// collected, which after a restart or a placement move is another node. So
// the fleet's row for that turn is spread across two logs, and folding each
// half into a finished [Turn] loses exactly what the whole needs — whether a
// turn is complete is decided by which of its NEWEST end and its NEWEST park
// is later, and two booleans computed apart cannot be compared.
//
// So a partial keeps every aggregate in the form its SQL computed it — sums,
// minimums, maximums and the two instants — and [CombineTurnPartials] applies
// the same aggregates across logs. It is the wire shape `internal/eventfan`
// carries between nodes, which is why every field is tagged: two builds read
// it, and it evolves additively.
type TurnPartial struct {
	TurnID    string    `json:"turn_id"`
	WorkKey   string    `json:"work_key,omitempty"`
	AgentID   string    `json:"agent_id,omitempty"`
	AgentRole string    `json:"role,omitempty"`
	StartedAt time.Time `json:"started_at"`
	EndedAt   time.Time `json:"ended_at"`

	Phases     int  `json:"phases"`
	Iterations int  `json:"iterations"`
	Failed     bool `json:"failed"`

	InputTokens  int `json:"input_tokens"`
	OutputTokens int `json:"output_tokens"`
	TotalTokens  int `json:"total_tokens"`
	CacheRead    int `json:"cache_read_tokens"`
	CacheWrite   int `json:"cache_write_tokens"`

	// Models is every distinct model, in the order the read produced them.
	Models []string `json:"models,omitempty"`

	// LastEnded and LastParked are the instants of the newest completion
	// that ENDED the turn and the newest that PARKED it, or nil. What
	// [segmentState] decides from — and the reason this type exists.
	LastEnded  *time.Time `json:"last_ended,omitempty"`
	LastParked *time.Time `json:"last_parked,omitempty"`

	// DurationMS is the SUM of the completions' own measurements.
	DurationMS int `json:"duration_ms"`

	// Summary, WorkItem and Trigger are the MAXIMUM of their values, as
	// the SQL takes them; WorkItem is the completion's work item AS
	// STORED — JSON text — and is decoded only when the turn is finished,
	// because the maximum is a maximum of that text.
	Summary  string `json:"summary,omitempty"`
	WorkItem string `json:"work_item,omitempty"`
	Trigger  string `json:"trigger,omitempty"`
}

// Turn finishes a partial into the row a listing serves.
func (p TurnPartial) Turn() Turn {
	t := Turn{
		TurnID: p.TurnID, WorkKey: p.WorkKey,
		AgentID: p.AgentID, AgentRole: p.AgentRole,
		StartedAt: p.StartedAt, EndedAt: p.EndedAt,
		DurationMS: p.DurationMS,
		Phases:     p.Phases, Iterations: p.Iterations, Failed: p.Failed,
		InputTokens: p.InputTokens, OutputTokens: p.OutputTokens,
		TotalTokens: p.TotalTokens,
		CacheRead:   p.CacheRead, CacheWrite: p.CacheWrite,
		Models:  strings.Join(p.Models, ","),
		Summary: p.Summary, Trigger: p.Trigger,
	}
	t.Complete, t.Parked = segmentState(p.LastEnded, p.LastParked)
	if p.WorkItem != "" {
		// A record whose item does not decode is a row with no item,
		// not a failed listing: the item is a label on the row, and
		// one unreadable completion must not cost the page.
		var named types.WorkItem
		if json.Unmarshal([]byte(p.WorkItem), &named) == nil && named.ID != "" {
			t.WorkItem = &named
		}
	}
	return t
}

// CombineTurnPartials folds several logs' partials of ONE turn into the
// partial one log holding every event would have produced.
//
// THE SQL'S OWN AGGREGATES, written for values: a sum is a sum, a MIN of the
// start is a min, a MAX of a string is the byte-wise greatest (SQLite's
// BINARY collation), and the distinct models are a union. Beside the query it
// mirrors, because a rule applied in two places is the rule that stops
// matching — an aggregate added to the SELECT and not here would be a column
// the fleet's row reads as its first node's value.
//
// The partials are assumed to be of one turn; a caller grouping by turn id is
// what guarantees it. Zero partials is the zero partial.
func CombineTurnPartials(parts ...TurnPartial) TurnPartial {
	if len(parts) == 0 {
		return TurnPartial{}
	}
	out := parts[0]
	out.Models = slices.Clone(out.Models)
	for _, p := range parts[1:] {
		out.WorkKey = max(out.WorkKey, p.WorkKey)
		out.AgentID = max(out.AgentID, p.AgentID)
		out.AgentRole = max(out.AgentRole, p.AgentRole)
		if p.StartedAt.Before(out.StartedAt) {
			out.StartedAt = p.StartedAt
		}
		if p.EndedAt.After(out.EndedAt) {
			out.EndedAt = p.EndedAt
		}
		out.Phases += p.Phases
		out.Iterations = max(out.Iterations, p.Iterations)
		out.Failed = out.Failed || p.Failed
		out.InputTokens += p.InputTokens
		out.OutputTokens += p.OutputTokens
		out.TotalTokens += p.TotalTokens
		out.CacheRead += p.CacheRead
		out.CacheWrite += p.CacheWrite
		for _, m := range p.Models {
			if !slices.Contains(out.Models, m) {
				out.Models = append(out.Models, m)
			}
		}
		out.LastEnded = laterOf(out.LastEnded, p.LastEnded)
		out.LastParked = laterOf(out.LastParked, p.LastParked)
		out.DurationMS += p.DurationMS
		out.Summary = max(out.Summary, p.Summary)
		out.WorkItem = max(out.WorkItem, p.WorkItem)
		out.Trigger = max(out.Trigger, p.Trigger)
	}
	return out
}

// laterOf is MAX over two instants either of which may be absent.
func laterOf(a, b *time.Time) *time.Time {
	switch {
	case a == nil:
		return b
	case b == nil:
		return a
	case b.After(*a):
		return b
	}
	return a
}

// TurnPartials is [EventLog.Turns] before the fold: this log's share of every
// turn the query selects.
//
// With [TurnQuery.IDs] set it is the OTHER question a fleet asks — "your share
// of exactly these turns" — which is how a turn selected on one node is made
// whole from every other: the turn-level narrowing (a model, a work item, a
// failure, the cursor) was already decided where the turn was selected, and
// applying it again here would drop the half of the turn that did not match
// it. The ROW-level filters — the seat and the work key — still apply,
// because they narrow which records are folded, and a node answering a share
// without them would fold rows the selecting node did not.
func (l *EventLog) TurnPartials(ctx context.Context, q TurnQuery) ([]TurnPartial, error) {
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
	if !q.Sort.Valid() {
		return nil, fmt.Errorf("%w: sort %q is not one of %v", ErrTurnSort, q.Sort, TurnSorts)
	}
	shares := len(q.IDs) > 0
	if shares {
		// EVERY TURN NAMED, whatever the page size: the caller chose the
		// turns and is asking for all of them.
		limit = len(q.IDs)
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

	where, args := q.turnWhere(floor, shares)

	having := []string{}
	if !q.Before.IsZero() && !shares && q.Sort != TurnSortTokens {
		having = append(having, "MIN(event_time) < ?")
		args = append(args, EncodeTime(q.Before))
	}
	// ONE EXPRESSION FOR THE COLUMN AND THE FILTER. Spelled twice, a turn
	// could be selected by `failed=true` and then render without the mark,
	// which is the same defect one layer down from the one [failedRow]
	// describes.
	failedExpr, failedArgs := failedRow()
	failedAgg := "MAX(CASE WHEN " + failedExpr + " THEN 1 ELSE 0 END)"
	if q.Failed != nil && !shares {
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
		       SUM(cache_read_tokens), SUM(cache_write_tokens),
		       GROUP_CONCAT(DISTINCT NULLIF(model, '')),
		       MAX(CASE WHEN (event_type = ? AND `+suspendedExpr+` = 0)
		                  OR event_type = ?
		                THEN event_time END),
		       MAX(CASE WHEN event_type = ? AND `+suspendedExpr+` = 1
		                THEN event_time END),
		       SUM(CASE WHEN event_type = ?
		                THEN COALESCE(json_extract(payload, '$.duration_ms'), 0) END),
		       MAX(CASE WHEN event_type = ?
		                THEN COALESCE(json_extract(payload, '$.plan_summary'), '') END),
		       MAX(CASE WHEN event_type = ?
		                THEN json_extract(payload, '$.work_item') END),
		       MAX(COALESCE(json_extract(tags, '$.trigger'), ''))
		  FROM crewlet_events
		 WHERE `+strings.Join(where, " AND ")+`
		 GROUP BY turn_id`+havingSQL+`
		 ORDER BY `+q.Sort.orderSQL()+`
		 LIMIT ?`,
		// The SELECT list's own placeholders, in the order they appear
		// in it: the phase count, then the failure predicate, then the
		// five reads off the completion rows — the first of them, the
		// newest end, also reading a lost run as one.
		slices.Concat([]any{phaseCompleted}, failedArgs,
			[]any{turnCompleted, runLost, turnCompleted, turnCompleted, turnCompleted,
				turnCompleted},
			args)...)
	if err != nil {
		return nil, fmt.Errorf("store: list turns: %w", err)
	}
	defer func() { _ = rows.Close() }()

	out := []TurnPartial{}
	for rows.Next() {
		var (
			p                 TurnPartial
			workKey           sql.NullString
			agentID, role     sql.NullString
			started, ended    int64
			failed            int
			models, summary   sql.NullString
			item, trigger     sql.NullString
			in, outTok, total sql.NullInt64
			cacheR, cacheW    sql.NullInt64
			ended1, parked1   sql.NullInt64
			duration          sql.NullInt64
		)
		if err := rows.Scan(&p.TurnID, &workKey, &agentID, &role, &started, &ended,
			&p.Phases, &p.Iterations, &failed, &in, &outTok, &total,
			&cacheR, &cacheW, &models,
			&ended1, &parked1, &duration, &summary, &item, &trigger); err != nil {

			return nil, fmt.Errorf("store: scan a turn: %w", err)
		}
		p.WorkKey = workKey.String
		p.AgentID, p.AgentRole = agentID.String, role.String
		p.StartedAt, p.EndedAt = DecodeTime(started), DecodeTime(ended)
		p.Failed = failed != 0
		p.LastEnded, p.LastParked = instantOf(ended1), instantOf(parked1)
		p.DurationMS = int(duration.Int64)
		p.InputTokens = int(in.Int64)
		p.OutputTokens = int(outTok.Int64)
		p.TotalTokens = int(total.Int64)
		p.CacheRead, p.CacheWrite = int(cacheR.Int64), int(cacheW.Int64)
		if models.String != "" {
			p.Models = strings.Split(models.String, ",")
		}
		p.Summary, p.Trigger = summary.String, trigger.String
		p.WorkItem = item.String
		out = append(out, p)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: list turns: %w", err)
	}
	return out, nil
}

// instantOf decodes a nullable stored instant.
func instantOf(v sql.NullInt64) *time.Time {
	if !v.Valid {
		return nil
	}
	at := DecodeTime(v.Int64)
	return &at
}

// ErrTurnSort is returned for a sort this build does not know, naming the
// ones it does.
var ErrTurnSort = errors.New("store: unknown turn sort")

// turnWhere is the row-level WHERE of [EventLog.TurnPartials] and its
// arguments — a function of its own so the plan every filter gets can be read
// back for the terms that run (see TestEveryTurnFilterSeeksItsIndex).
func (q TurnQuery) turnWhere(floor time.Time, shares bool) ([]string, []any) {
	where := []string{"turn_id != ''", "event_time >= ?"}
	args := []any{EncodeTime(floor)}
	if shares {
		where = append(where, "turn_id IN (?"+strings.Repeat(",?", len(q.IDs)-1)+")")
		for _, id := range q.IDs {
			args = append(args, id)
		}
	}
	// ONLY THE IDENTIFIERS THE CALLER HOLDS — binding an empty one matches
	// every row that carries none, which is every non-agent event in the
	// window. See [seatClause].
	if clause, ids := seatClause(q.AgentID, q.AgentRole); clause != "" {
		where = append(where, strings.TrimPrefix(clause, " AND "))
		args = append(args, ids...)
	}
	if q.WorkKey != "" {
		// WITH THE PARTIAL INDEX'S OWN PREDICATE, which is what lets the
		// planner use schema/0029's index — see [ListQuery.predicate].
		where = append(where, "work_key = ?", "work_key <> ''")
		args = append(args, q.WorkKey)
	}
	if q.Model != "" && !shares {
		// ON THE TURN, not on the row: a turn is selected when ANY of
		// its phases used the model, which is what a reader means by
		// "turns on the cheap model".
		where = append(where,
			"turn_id IN (SELECT turn_id FROM crewlet_events "+
				"WHERE model = ? AND event_time >= ? AND turn_id != '')")
		args = append(args, q.Model, EncodeTime(floor))
	}
	if q.WorkItem != "" && !shares {
		// ON THE TURN, not on the row, for Model's reason and one more:
		// filtering rows would fold only the records that carry the item,
		// and a parked segment resolved before its sole write — or any
		// record that names no item — would drop out of the sums.
		where = append(where,
			"turn_id IN (SELECT turn_id FROM crewlet_events "+
				"WHERE work_item = ? AND work_item <> '' AND event_time >= ? AND turn_id != '')")
		args = append(args, q.WorkItem, EncodeTime(floor))
	}
	return where, args
}
