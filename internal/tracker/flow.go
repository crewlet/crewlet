package tracker

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"time"

	"github.com/crewlet/crewlet/internal/period"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/store"
)

// The company's work as a series: how many tasks sat in each status group at
// the end of each of the last N days or weeks, and how many were completed in
// each — `work_flow`.
//
// # REPLAYED BACKWARD FROM THE CENSUS, never forward from the beginning
//
// Today's census is exact and cheap: it is the task rows as they stand. The
// past is today minus every change since, so the reader takes the census and
// walks `tracker_history` BACKWARD over the window, undoing each row that
// moved a task between status groups, into or out of existence (created,
// removed, restored, purged) or between projects. A forward replay would need
// the state at the window's start, which is the very thing being computed —
// and replaying from the company's first commit reads a table nothing sweeps.
//
// The walk touches only the rows that MOVE a count, through the partial index
// replicated migration 0029 adds for exactly this predicate
// ([historyMoves]), so its cost is the window's changes rather than the
// company's history.
//
// # The day is the COMPANY's, and the instant is the fleet's
//
// Windows are cut on the company clock (`period`), so "Tuesday" is Tuesday in
// the company's zone on every node; a row belongs to the window its
// `effective_at` falls in — the fleet-agreed instant every duration is
// measured on — so two nodes at one checkpoint draw the same bars.
//
// # What it cannot say, said
//
// A task's BLOCKED state is derived from other tasks' rows (its blockers'
// groups), and the history records no transition of it, so the past cannot be
// replayed: `blocked` is answered for NOW only and the answer carries
// `blocked_history: false` rather than a series that would be today's number
// drawn as a flat line.

// historyMoves is the one predicate over `tracker_history` both company-wide
// readers state — the backward walk here and the feed's tracker page — WORD FOR
// WORD the WHERE of `tracker_history_moves_idx` (replicated 0029), which is what
// lets the planner prove the implication and search the partial index rather
// than scan a table nothing sweeps. `h` qualifies each column for a statement
// that joins (empty for one that does not); the planner matches the columns,
// not the spelling of the table. Edited here, the index must be replaced in a
// new migration: TestTheFlowAndFeedReadsSearchTheirIndex fails the day the
// planner can no longer prove this predicate implies the index's.
func historyMoves(h string) string {
	return h + `subject_kind = 'task' AND (
        ` + h + `kind IN ('created', 'removed', 'restored', 'purged')
        OR json_extract(` + h + `fields_json, '$.status.to') IS NOT NULL
        OR json_extract(` + h + `fields_json, '$.assignee.to') IS NOT NULL
        OR json_extract(` + h + `fields_json, '$.project.to') IS NOT NULL)`
}

// MaxFlowPoints bounds one series.
//
// NINETY: a quarter of days, or most of two years of weeks — the longest span
// a trend chart draws, and the bound on how much history one poll walks. The
// walk is linear in the changes the window holds, so the bound is a bound on
// the read's cost rather than on the answer's size.
const MaxFlowPoints = 90

// FlowQuery asks for the series.
type FlowQuery struct {
	// Project narrows to one project's tasks, following a task that moved
	// between projects to where it was at each instant. Empty is the whole
	// company.
	Project string

	// Bucket is the window each point covers: [period.Day] or
	// [period.Week]. A month series is not offered: ninety months is a
	// span no history this engine keeps is long enough to be worth
	// walking, and a chart of four is a table.
	Bucket period.Period

	// Points is how many windows, the last being the one now falls in:
	// 1..[MaxFlowPoints].
	Points int

	Level       statelog.ReadLevel
	Session     statelog.Position
	MinPosition statelog.Position
	MaxLag      time.Duration
	MaxLagSeq   uint64
}

// FlowPoint is one window of the series.
//
// THE CENSUS IS AT THE WINDOW'S END — what the board would have shown at the
// last instant of that day or week — and at NOW for the window now falls in,
// whose end has not happened. Completed is every change in the window that
// took a task from not-delivered to delivered (see [Delivered]), so a task
// cancelled is not a completion and a task moved from done to closed is not a
// second one.
type FlowPoint struct {
	// Window is the period's label (`2026-09-23`, `2026-W39`) and Start
	// and End its half-open interval, in UTC on the wire.
	Window string    `json:"window"`
	Start  time.Time `json:"start"`
	End    time.Time `json:"end"`

	NotStarted int `json:"not_started"`
	Active     int `json:"active"`
	Done       int `json:"done"`
	Closed     int `json:"closed"`
	Completed  int `json:"completed"`
}

// FlowNow is the census now, with the two shapes of trouble only the present
// can answer.
type FlowNow struct {
	NotStarted int `json:"not_started"`
	Active     int `json:"active"`

	// Blocked is open work with an open blocker; Overdue is open work
	// whose due instant is before the start of the company's today — the
	// cut every board's overdue mark uses.
	Blocked int `json:"blocked"`
	Overdue int `json:"overdue"`
}

// FlowAnswer is the series, oldest window first.
type FlowAnswer struct {
	Bucket  period.Period `json:"bucket"`
	Project string        `json:"project,omitempty"`
	Points  []FlowPoint   `json:"points"`
	Now     FlowNow       `json:"now"`

	// BlockedHistory is always false: see the file's head. Carried so a
	// chart that wants a blocked line reads that there is none rather
	// than drawing zeros.
	BlockedHistory bool `json:"blocked_history"`

	Level          statelog.ReadLevel `json:"read_level,omitempty"`
	LogSeq         uint64             `json:"log_seq,omitempty"`
	AppliedThrough uint64             `json:"applied_through,omitempty"`
	LogLag         *uint64            `json:"log_lag,omitempty"`
	Complete       bool               `json:"complete"`
	Incomplete     *Incomplete        `json:"incomplete,omitempty"`
}

// Flow answers the series ending in the window `now` falls in, on the clock
// `loc`.
func (r *Reader) Flow(ctx context.Context, q FlowQuery, now time.Time,
	loc *time.Location) (FlowAnswer, error) {

	switch {
	case q.Level == "":
		return FlowAnswer{}, fmt.Errorf("tracker: this flow read names no " +
			"level — a surface resolves an absent read_level to its own " +
			"default before it reads")
	case q.Bucket != period.Day && q.Bucket != period.Week:
		return FlowAnswer{}, invalid("tracker: a flow bucket is %q or %q, not %q",
			period.Day, period.Week, q.Bucket)
	case q.Points < 1 || q.Points > MaxFlowPoints:
		return FlowAnswer{}, invalid("tracker: a flow series is 1 to %d points, "+
			"not %d", MaxFlowPoints, q.Points)
	}
	if loc == nil {
		loc = time.UTC
	}
	out := FlowAnswer{Bucket: q.Bucket, Project: q.Project, BlockedHistory: false}
	served, err := r.log.Read(ctx, statelog.Query{
		Level: q.Level,
		// THE DOMAIN, for [Reader.Workload]'s reason: a series over the
		// company reads every container, and a narrower closure would
		// certify the answer complete while a deferred record held a
		// change it should have undone.
		Scope:       statelog.ScopeSet{Paths: []string{pathDomain}}.Normalised(),
		Session:     q.Session,
		MinPosition: q.MinPosition,
		MaxLag:      q.MaxLag,
		MaxLagSeq:   q.MaxLagSeq,
		Set:         true,
	}, func(tx *sql.Tx) error {
		return readFlow(ctx, tx, q, now, loc, &out)
	})
	if err != nil {
		return FlowAnswer{}, err
	}
	out.Level = served.Level
	out.Complete = served.Complete
	out.LogLag = served.Lag
	if served.Incomplete != nil {
		out.Incomplete = incompleteFrom(served.Incomplete)
	}
	return out, nil
}

// flowState is one task as the backward walk holds it: where it stood just
// AFTER the row being undone.
type flowState struct {
	exists  bool
	removed bool
	status  Status
	project string
}

// counted reports the task in the census a query draws.
func (s flowState) counted(project string) bool {
	return s.exists && !s.removed && (project == "" || s.project == project)
}

// census is one count per status group.
type census [4]int

func (c *census) add(s flowState, project string, n int) {
	if !s.counted(project) {
		return
	}
	switch s.status.Group() {
	case GroupNotStarted:
		c[0] += n
	case GroupActive:
		c[1] += n
	case GroupDone:
		c[2] += n
	case GroupClosed:
		c[3] += n
	}
}

// flowRow is one history row the walk undoes.
type flowRow struct {
	subject   string
	kind      ChangeKind
	effective time.Time
	fields    map[string]Delta
}

// undo is the state before a row, given the state after it.
//
// A purge is the one row this cannot undo from the row alone — the task's
// rows are gone — so its prior state is the forward replay of the task's own
// history below the purge, which [readFlow] supplies.
func (s flowState) undo(row flowRow) flowState {
	if moved, ok := row.fields["status"]; ok {
		s.status = Status(moved.From)
	}
	if moved, ok := row.fields["project"]; ok {
		s.project = moved.From
	}
	switch row.kind {
	case ChangeCreated:
		s.exists = false
	case ChangeRemoved:
		s.removed = false
	case ChangeRestored:
		s.removed = true
	}
	return s
}

// completes reports a row that took a task from not-delivered to delivered.
func (row flowRow) completes() bool {
	moved, ok := row.fields["status"]
	return ok && !Delivered(Status(moved.From)) && Delivered(Status(moved.To))
}

func readFlow(ctx context.Context, tx *sql.Tx, q FlowQuery, now time.Time,
	loc *time.Location, out *FlowAnswer) error {

	// THE WINDOWS, oldest first, the last holding `now`.
	last := period.At(q.Bucket, now, loc)
	windows := make([]period.Window, q.Points)
	for i := range windows {
		windows[i] = last.Shift(i - (q.Points - 1))
		if windows[i].Label == "" {
			return invalid("tracker: a %d-%s series ending %s reaches past the "+
				"calendar a label can spell", q.Points, q.Bucket, last.Label)
		}
	}

	// TODAY'S CENSUS, exact, from the rows as they stand.
	var current census
	censusWhere, censusArgs := "removed_at IS NULL", []any{}
	if q.Project != "" {
		censusWhere += " AND project_key = ?"
		censusArgs = append(censusArgs, q.Project)
	}
	rows, err := tx.QueryContext(ctx, `
		SELECT status, COUNT(*) FROM tracker_tasks
		WHERE `+censusWhere+` GROUP BY status`, censusArgs...)
	if err != nil {
		return fmt.Errorf("tracker: count today's census: %w", err)
	}
	for rows.Next() {
		var status string
		var n int
		if err = rows.Scan(&status, &n); err != nil {
			_ = rows.Close()
			return fmt.Errorf("tracker: scan the census: %w", err)
		}
		current.add(flowState{exists: true, status: Status(status), project: q.Project},
			q.Project, n)
	}
	if err = rows.Close(); err != nil {
		return fmt.Errorf("tracker: read the census: %w", err)
	}
	if err = readFlowNow(ctx, tx, q.Project, now, loc, current, &out.Now); err != nil {
		return err
	}

	// THE ROWS TO UNDO, newest first, back to the first window's start.
	walk, err := readFlowRows(ctx, tx, windows[0].Start)
	if err != nil {
		return err
	}
	states := map[string]flowState{}
	state := func(id string) (flowState, error) {
		if s, held := states[id]; held {
			return s, nil
		}
		s, readErr := taskStateNow(ctx, tx, id)
		states[id] = s
		return s, readErr
	}

	out.Points = make([]FlowPoint, len(windows))
	next := 0
	for i := len(windows) - 1; i >= 0; i-- {
		w := windows[i]
		// UNDO EVERYTHING AT OR AFTER THIS WINDOW'S END, which leaves the
		// census as it stood at its last instant. The newest window's
		// end is in the future, so nothing is undone for it and its
		// census is today's.
		for ; next < len(walk) && !walk[next].effective.Before(w.End); next++ {
			if err = undoRow(ctx, tx, walk[next], state, states, q.Project, &current); err != nil {
				return err
			}
		}
		point := FlowPoint{
			Window: w.Label, Start: w.Start.UTC(), End: w.End.UTC(),
			NotStarted: current[0], Active: current[1],
			Done: current[2], Closed: current[3],
		}
		// AND COUNT THE COMPLETIONS INSIDE IT while undoing them, so the
		// next (older) window's census starts from this one's start.
		for ; next < len(walk) && !walk[next].effective.Before(w.Start); next++ {
			row := walk[next]
			after, stateErr := state(row.subject)
			if stateErr != nil {
				return stateErr
			}
			if row.completes() && after.counted(q.Project) {
				point.Completed++
			}
			if err = undoRow(ctx, tx, row, state, states, q.Project, &current); err != nil {
				return err
			}
		}
		out.Points[i] = point
	}

	position, applied, err := readCheckpoint(ctx, tx)
	if err != nil {
		return err
	}
	out.LogSeq, out.AppliedThrough = position, applied
	return nil
}

// undoRow moves one task back across one row and adjusts the census.
func undoRow(ctx context.Context, tx *sql.Tx, row flowRow,
	state func(string) (flowState, error), states map[string]flowState,
	project string, current *census) error {

	after, err := state(row.subject)
	if err != nil {
		return err
	}
	before := after.undo(row)
	if row.kind == ChangePurged {
		if before, err = stateBeforePurge(ctx, tx, row.subject); err != nil {
			return err
		}
	}
	current.add(after, project, -1)
	current.add(before, project, 1)
	states[row.subject] = before
	return nil
}

// flowRowsStatement is the walk's one statement, named so the plan test
// explains exactly what the reader issues.
var flowRowsStatement = `
		SELECT subject_id, kind, fields_json, effective_at FROM tracker_history
		WHERE ` + historyMoves("") + ` AND effective_at >= ?
		ORDER BY effective_at DESC, log_seq DESC`

// readFlowRows is every row that moves a count, at or after `from`, newest
// first — through `tracker_history_moves_idx`.
func readFlowRows(ctx context.Context, tx *sql.Tx, from time.Time) ([]flowRow, error) {
	rows, err := tx.QueryContext(ctx, flowRowsStatement, store.EncodeTime(from))
	if err != nil {
		return nil, fmt.Errorf("tracker: read the history the flow walks: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []flowRow
	for rows.Next() {
		var row flowRow
		var kind, fields string
		var at int64
		if err = rows.Scan(&row.subject, &kind, &fields, &at); err != nil {
			return nil, fmt.Errorf("tracker: scan a flow row: %w", err)
		}
		row.kind = ChangeKind(kind)
		row.effective = store.DecodeTime(at)
		row.fields = deltasOf(fields)
		out = append(out, row)
	}
	if err = rows.Err(); err != nil {
		return nil, fmt.Errorf("tracker: walk the flow rows: %w", err)
	}
	return out, nil
}

// deltasOf decodes a history row's `fields_json`.
//
// A COLUMN THAT DOES NOT DECODE MOVED NOTHING this walk can undo, rather than
// failing the read: the column is the applier's own JSON, and one malformed row
// must not take a company's chart down with it.
func deltasOf(fields string) map[string]Delta {
	var out map[string]Delta
	if err := json.Unmarshal([]byte(fields), &out); err != nil {
		return nil
	}
	return out
}

// taskStateNow is one task as its row stands, or — for a task with no row,
// which only a purge leaves — a task that does not exist.
func taskStateNow(ctx context.Context, tx *sql.Tx, id string) (flowState, error) {
	var status, project string
	var removed int
	err := tx.QueryRowContext(ctx, `
		SELECT status, project_key, removed_at IS NOT NULL
		FROM tracker_tasks WHERE id = ?`, id).Scan(&status, &project, &removed)
	switch {
	case err == sql.ErrNoRows:
		return flowState{}, nil
	case err != nil:
		return flowState{}, fmt.Errorf("tracker: read task %s for the flow: %w", id, err)
	}
	return flowState{exists: true, removed: removed != 0, status: Status(status),
		project: project}, nil
}

// stateBeforePurge is a purged task as it stood just before its purge: the
// forward replay of its own rows below the purge row.
//
// The purge deleted the task's rows but not its history, and the history is
// the only account left of where it stood — whether it was still counted (a
// purge does not require a removal first) and in which group.
func stateBeforePurge(ctx context.Context, tx *sql.Tx, id string) (flowState, error) {
	rows, err := tx.QueryContext(ctx, `
		SELECT kind, fields_json FROM tracker_history
		WHERE subject_id = ? ORDER BY log_seq`, id)
	if err != nil {
		return flowState{}, fmt.Errorf("tracker: read purged task %s's history: %w", id, err)
	}
	defer func() { _ = rows.Close() }()
	var s flowState
	for rows.Next() {
		var kind, fields string
		if err = rows.Scan(&kind, &fields); err != nil {
			return flowState{}, fmt.Errorf("tracker: scan purged task %s's history: %w", id, err)
		}
		if ChangeKind(kind) == ChangePurged {
			break
		}
		moved := deltasOf(fields)
		if d, ok := moved["status"]; ok {
			s.status = Status(d.To)
		}
		if d, ok := moved["project"]; ok {
			s.project = d.To
		}
		switch ChangeKind(kind) {
		case ChangeCreated:
			s.exists = true
		case ChangeRemoved:
			s.removed = true
		case ChangeRestored:
			s.removed = false
		}
	}
	if err = rows.Err(); err != nil {
		return flowState{}, fmt.Errorf("tracker: walk purged task %s's history: %w", id, err)
	}
	return s, nil
}

// readFlowNow fills the present-tense half: the open census it already has,
// and blocked and overdue, which only the present can answer.
func readFlowNow(ctx context.Context, tx *sql.Tx, project string, now time.Time,
	loc *time.Location, current census, out *FlowNow) error {

	out.NotStarted, out.Active = current[0], current[1]
	// THE DAY BOUNDARY every board's overdue mark is cut on — see
	// [readWorkload].
	anchor, err := ResolveDate("today", now, loc)
	if err != nil {
		return err
	}
	where := "t.removed_at IS NULL AND t.status_group IN (" + openGroupsSQL + ")"
	args := []any{store.EncodeTime(anchor.At)}
	if project != "" {
		where += " AND t.project_key = ?"
		args = append(args, project)
	}
	err = tx.QueryRowContext(ctx, `
		SELECT
		  COALESCE(SUM(CASE WHEN EXISTS (
		      SELECT 1 FROM tracker_task_deps d
		      WHERE d.task_id = t.id AND d.blocker_open = 1)
		    THEN 1 ELSE 0 END), 0),
		  COALESCE(SUM(CASE WHEN t.due_at IS NOT NULL AND t.due_at < ?
		    THEN 1 ELSE 0 END), 0)
		FROM tracker_tasks t WHERE `+where, args...).Scan(&out.Blocked, &out.Overdue)
	if err != nil {
		return fmt.Errorf("tracker: count blocked and overdue work: %w", err)
	}
	return nil
}
