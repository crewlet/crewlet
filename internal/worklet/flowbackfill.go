package worklet

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/store"
)

// The backfill records the flow an item had before the flow duty first read
// it (ADR-0162, "What is recorded"; package doc, "An item in progress says
// why it is not moving"): the duty's own expression, evaluated at each instant
// the item's past could change, over that past as its history, its turn
// segments and the seats' retained events give it.

// flowBackfillItems is how many items one run of the backfill walks: each is
// a handful of index seeks and, for an item with a past, one evaluation per
// instant its past could change, so a run stays inside the duty's minute.
const flowBackfillItems = 100

// FlowBackfillReach is how far before the duty's first change the backfill
// reads: an item no longer in progress by then is left unrecorded. BORROWED
// (ADR-0015): the event store's read floor, beyond which the seats' events
// are gone and a change could be read from the item's own rows alone.
const FlowBackfillReach = store.EventHistory

// SeatPast is where the backfill reads what the seats' retained events say of
// their past: the live facts an instant of an item's past had, which the
// estate does not keep. Nil reads none, and each change derived without them
// says so ([FlowPast.Seats]).
type SeatPast interface {
	// SeatPast answers seat's stops and parked runs over [from, to), and
	// from when its events cover it.
	SeatPast(ctx context.Context, seat string, from, to time.Time) (SeatHistory, error)
}

// SeatHistory is what one seat's retained events say over a span.
type SeatHistory struct {
	// Covered is the instant from which the seat's events were read: an
	// instant before it, or any where it is zero, reads no live fact.
	Covered time.Time

	// Stops are the spans the seat could not take a turn, and Parks the
	// spans a run of it waited on a person's answer.
	Stops []SeatStop
	Parks []SeatPark
}

// SeatStop is one span a seat could not take a turn.
type SeatStop struct {
	// Cause is a stop reason ([FlowCause.stops]).
	Cause FlowCause
	// From and To bound it; To is zero for a stop not lifted by the span's
	// end. Until is the reset known while it held, zero where none was.
	From, To, Until time.Time
}

// SeatPark is one span a run of the seat waited on a person's answer.
type SeatPark struct {
	// Item is the id of the item the run is on.
	Item     string
	From, To time.Time
}

// covers reports whether the seat's events cover at.
func (h SeatHistory) covers(at time.Time) bool {
	return !h.Covered.IsZero() && !at.Before(h.Covered)
}

// within reports whether at falls in [from, to), a zero to being open.
func within(at, from, to time.Time) bool {
	return !at.Before(from) && (to.IsZero() || at.Before(to))
}

// posture is the seat's live facts at an instant, as the expression binds
// them: its stop, ranked as the live projection ranks stops, and its parked
// run.
func (h SeatHistory) posture(at time.Time) SeatPosture {
	var out SeatPosture
	if !h.covers(at) {
		return out
	}
	for _, cause := range FlowCauses {
		if out.Stop != "" || !cause.stops() {
			continue
		}
		for _, stop := range h.Stops {
			if stop.Cause == cause && within(at, stop.From, stop.To) {
				out.Stop, out.StopSince, out.StopUntil = cause, stop.From, stop.Until
				break
			}
		}
	}
	for _, park := range h.Parks {
		if park.Item != "" && within(at, park.From, park.To) &&
			(out.Parked == "" || park.From.Before(out.ParkedSince)) {
			out.Parked, out.ParkedSince = park.Item, park.From
		}
	}
	return out
}

// edges are the instants a stop or park begins or ends.
func (h SeatHistory) edges() []time.Time {
	var out []time.Time
	for _, s := range h.Stops {
		out = append(out, s.From, s.To)
	}
	for _, p := range h.Parks {
		out = append(out, p.From, p.To)
	}
	return out
}

// FlowBackfill is what one run of the backfill did.
type FlowBackfill struct {
	// Items is how many items it walked, Past how many of them had a past
	// before the duty to read, and Changes the changes it recorded, of
	// which Unseated were read without the seats' events.
	Items, Past, Changes, Unseated int

	// Done says the walk reached the last item: every later run is free.
	Done bool
}

// flowFill is the backfill's place in its walk on this node.
type flowFill struct {
	// started says the walk resumed from its cursor: the highest item it
	// recorded, read again inclusive, since a run can stop between two of
	// an item's changes.
	started bool
	after   string
	from    bool // the next page includes after

	horizon *flowHorizon
	done    bool
}

// flowHorizon is the duty's first change read as it happened: the instant and
// position before which an item's past is the backfill's to read.
type flowHorizon struct {
	seq int64
	at  time.Time
}

// Backfill runs one bounded step of the backfill at now, after the tick that
// recorded the present, gated on its cursor: the items after it, a page at a
// time in id order, each item's past before the duty first read it recorded
// as the duty would have, and nothing told. Each change lands once by the key
// the duty derives, so a run that reads an item again, here or on another
// node after a hand-over, publishes nothing. A run that fails is logged and
// left for the next: the cursor stays before the item it failed on.
func (d *FlowDuty) Backfill(ctx context.Context, now time.Time) FlowBackfill {
	was := d.fill.done
	out, err := d.backfill(ctx, now)
	switch {
	case err != nil:
		d.deps.Logger.WarnContext(ctx, "tracker_flow_backfill_failed", "error", err)
	case out.Changes > 0:
		d.deps.Logger.InfoContext(ctx, "tracker_flow_backfilled", "items", out.Items,
			"past", out.Past, "changes", out.Changes, "without_seats", out.Unseated)
	}
	if out.Done && !was {
		d.deps.Logger.InfoContext(ctx, "tracker_flow_backfill_done", "detail", "every "+
			"item's flow before the duty first read it is recorded")
	}
	return out
}

func (d *FlowDuty) backfill(ctx context.Context, now time.Time) (FlowBackfill, error) {
	fill := &d.fill
	if fill.done {
		return FlowBackfill{Done: true}, nil
	}
	if fill.horizon == nil || !fill.started {
		horizon, cursor, err := d.deps.Reader.flowFillStart(ctx, d.wrote)
		if err != nil {
			return FlowBackfill{}, err
		}
		if horizon == nil {
			// The duty has read nothing yet, so nothing is before it.
			return FlowBackfill{}, nil
		}
		fill.horizon = horizon
		if !fill.started {
			fill.started, fill.after, fill.from = true, cursor, cursor != ""
		}
	}
	page, err := d.deps.Reader.flowFillPage(ctx, fill.after, fill.from, d.wrote)
	if err != nil {
		return FlowBackfill{}, err
	}
	out := FlowBackfill{Items: len(page)}
	for _, id := range page {
		past, err := d.deps.Reader.readPast(ctx, id, *fill.horizon, now, d.wrote)
		if err != nil {
			return out, err
		}
		if past != nil {
			out.Past++
			records, err := d.deps.Reader.pastFlow(ctx, past, d.deps.Past, d.wrote)
			if err != nil {
				return out, err
			}
			for _, rec := range records {
				if past.recorded[rec.ID] {
					continue
				}
				if err := d.publishPast(ctx, rec); err != nil {
					// The cursor stays before the item, which the next run
					// reads again; what landed of it is not published twice.
					return out, err
				}
				out.Changes++
				if !rec.Past.Seats {
					out.Unseated++
				}
			}
		}
		fill.after, fill.from = id, false
	}
	if len(page) < flowBackfillItems {
		fill.done, out.Done = true, true
	}
	return out, nil
}

// publishPast writes one backfilled change under an operation id derived from
// the change's own key and instant, so a retry is the same operation.
func (d *FlowDuty) publishPast(ctx context.Context, rec FlowRecord) error {
	op := statelog.DeriveOpID(rec.Past.At, "flow-past-"+rec.Task, "crewlet.tracker.flow",
		rec.ID)
	res, err := d.deps.Writer.RecordFlow(ctx, op, rec)
	if err != nil {
		return fmt.Errorf("worklet: record item %s's past flow: %w", rec.Task, err)
	}
	if res.Position.Seq > d.wrote.Seq || res.Position.Stream != d.wrote.Stream {
		d.wrote = res.Position
	}
	return nil
}

// The backfill's statements, named for the plan test.
const (
	// flowHorizonStatement is the duty's first change, through the partial
	// index over the changes it read.
	flowHorizonStatement = `SELECT log_seq, recorded_at FROM tracker_flow
		INDEXED BY tracker_flow_read_idx WHERE backfilled = 0 ORDER BY log_seq LIMIT 1`
	// flowCursorStatement is the highest item the backfill recorded, through
	// the partial index over its rows.
	flowCursorStatement = `SELECT task_id FROM tracker_flow
		INDEXED BY tracker_flow_backfilled_idx WHERE backfilled = 1
		ORDER BY task_id DESC LIMIT 1`
	// flowFillPageStatement is a page of items in id order after the cursor.
	flowFillPageStatement = `SELECT id FROM tracker_tasks
		WHERE id > ? AND removed_at IS NULL ORDER BY id LIMIT ?`
	flowFillFromStatement = `SELECT id FROM tracker_tasks
		WHERE id >= ? AND removed_at IS NULL ORDER BY id LIMIT ?`
	// pastHistoryStatement is an item's history, and a blocker's, in log
	// order: what moved and when.
	pastHistoryStatement = `SELECT log_seq, effective_at, fields_json FROM tracker_history
		WHERE subject_id = ? ORDER BY log_seq`
	// pastTurnsStatement is the item's turn segments, each its seat, its end
	// and how long it ran.
	pastTurnsStatement = `SELECT seat, effective_at, wall_ms FROM tracker_turns
		WHERE task_id = ? ORDER BY effective_at`
	// pastAsksStatement is the instants the item's questions opened and
	// closed.
	pastAsksStatement = `SELECT c.created_at, c.resolved_at,
		(SELECT a.created_at FROM tracker_comments a WHERE a.id = c.answered_by),
		CASE WHEN c.removed = 1 THEN c.updated_at END
		FROM tracker_comments c WHERE c.task_id = ? AND c.ask <> ''`
	// pastRecordedStatement is every key recorded of the item, and the
	// instant of the first change the duty read of it.
	pastRecordedStatement = `SELECT id, backfilled, notice, recorded_at FROM tracker_flow
		WHERE task_id = ? ORDER BY log_seq`
	// pastNowStatement is the item's row as it stands, for what its history
	// spells less exactly than its columns.
	pastNowStatement = `SELECT project_key, start_at FROM tracker_tasks WHERE id = ?`
	// pastDepStatement is an edge's openness as it stands, for a blocker
	// that has no history of its own.
	pastDepStatement = `SELECT blocker_open FROM tracker_task_deps
		WHERE blocker_id = ? AND task_id = ?`
)

// flowFillStart reads the duty's first change and the backfill's cursor.
func (r *Reader) flowFillStart(ctx context.Context, wrote statelog.Position) (*flowHorizon, string, error) {
	var horizon *flowHorizon
	var cursor string
	err := r.readFill(ctx, wrote, func(tx *sql.Tx) error {
		var seq, at int64
		err := tx.QueryRowContext(ctx, flowHorizonStatement).Scan(&seq, &at)
		switch {
		case errors.Is(err, sql.ErrNoRows):
			return nil
		case err != nil:
			return fmt.Errorf("worklet: read the flow duty's first change: %w", err)
		}
		horizon = &flowHorizon{seq: seq, at: store.DecodeTime(at)}
		err = tx.QueryRowContext(ctx, flowCursorStatement).Scan(&cursor)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("worklet: read the flow backfill's cursor: %w", err)
		}
		return nil
	})
	return horizon, cursor, err
}

// flowFillPage reads the next page of item ids.
func (r *Reader) flowFillPage(ctx context.Context, after string, from bool,
	wrote statelog.Position) ([]string, error) {

	statement := flowFillPageStatement
	if from {
		statement = flowFillFromStatement
	}
	var out []string
	err := r.readFill(ctx, wrote, func(tx *sql.Tx) error {
		out = out[:0]
		rows, err := tx.QueryContext(ctx, statement, after, flowBackfillItems)
		if err != nil {
			return fmt.Errorf("worklet: read the backfill's page of items: %w", err)
		}
		defer func() { _ = rows.Close() }()
		for rows.Next() {
			var id string
			if err := rows.Scan(&id); err != nil {
				return err
			}
			out = append(out, id)
		}
		return rows.Err()
	})
	return out, err
}

// readFill is one read of the backfill, no older than the duty's own write.
func (r *Reader) readFill(ctx context.Context, wrote statelog.Position, fn func(*sql.Tx) error) error {
	_, err := r.log.Read(ctx, statelog.Query{
		Level:   statelog.ReadSession,
		Scope:   statelog.ScopeSet{Paths: []string{pathDomain}}.Normalised(),
		Session: wrote,
		Set:     true,
	}, fn)
	return err
}

// itemPast is one item's past as the backfill reads it.
type itemPast struct {
	id      string
	project string
	start   *time.Time // the start as its column holds it, exact

	history []pastRow
	turns   []pastTurn
	asks    []time.Time

	// blockers is each blocker's status over time, by id; a blocker with
	// none is open as its edge stands now (open[id]).
	blockers map[string][]pastStatus
	open     map[string]bool

	// recorded is every key already recorded of the item; until is the
	// instant the backfill reads it to (the duty's first change of it, or
	// now), and horizon the duty's first change of any item.
	recorded map[string]bool
	until    time.Time
	horizon  time.Time
}

// pastRow is one history row: when it took effect and what it moved.
type pastRow struct {
	seq    int64
	at     time.Time
	deltas map[string]Delta
}

// pastTurn is one turn segment: its seat and the span it ran.
type pastTurn struct {
	seat     string
	from, to time.Time
}

// pastStatus is a blocker's status from an instant.
type pastStatus struct {
	at     time.Time
	status Status
}

// readPast reads an item's past, nil for one with nothing before the duty to
// record: never in progress before the horizon, or no longer in progress
// within the reach before it.
func (r *Reader) readPast(ctx context.Context, id string, horizon flowHorizon, now time.Time,
	wrote statelog.Position) (*itemPast, error) {

	past := &itemPast{id: id, open: map[string]bool{}, blockers: map[string][]pastStatus{},
		recorded: map[string]bool{}, until: now, horizon: horizon.at}
	keep := false
	err := r.readFill(ctx, wrote, func(tx *sql.Tx) error {
		var err error
		if past.history, err = readPastRows(ctx, tx, id); err != nil {
			return err
		}
		if !past.enteredBefore(horizon) {
			return nil
		}
		keep = true
		var start sql.NullInt64
		if err := tx.QueryRowContext(ctx, pastNowStatement, id).Scan(&past.project, &start); err != nil {
			return fmt.Errorf("worklet: read item %s: %w", id, err)
		}
		if start.Valid {
			at := store.DecodeTime(start.Int64)
			past.start = &at
		}
		if err := past.readRecorded(ctx, tx); err != nil {
			return err
		}
		if err := past.readTurns(ctx, tx); err != nil {
			return err
		}
		if err := past.readAsks(ctx, tx); err != nil {
			return err
		}
		return past.readBlockers(ctx, tx)
	})
	if err != nil || !keep {
		return nil, err
	}
	if !past.inProgressSince(horizon.at.Add(-FlowBackfillReach)) {
		return nil, nil
	}
	return past, nil
}

func readPastRows(ctx context.Context, tx *sql.Tx, subject string) ([]pastRow, error) {
	rows, err := tx.QueryContext(ctx, pastHistoryStatement, subject)
	if err != nil {
		return nil, fmt.Errorf("worklet: read the history of %s: %w", subject, err)
	}
	defer func() { _ = rows.Close() }()
	var out []pastRow
	for rows.Next() {
		var row pastRow
		var at int64
		var fields string
		if err := rows.Scan(&row.seq, &at, &fields); err != nil {
			return nil, err
		}
		row.at = store.DecodeTime(at)
		if err := json.Unmarshal([]byte(fields), &row.deltas); err != nil {
			return nil, fmt.Errorf("worklet: read a history row of %s: %w", subject, err)
		}
		out = append(out, row)
	}
	return out, rows.Err()
}

// enteredBefore reports an entry into in progress below the horizon.
func (p *itemPast) enteredBefore(horizon flowHorizon) bool {
	for _, row := range p.history {
		if row.seq >= horizon.seq {
			break
		}
		if d, ok := row.deltas["status"]; ok && d.To == string(StatusInProgress) {
			return true
		}
	}
	return false
}

// inProgressSince reports whether the item was in progress at or after an
// instant, before the backfill's end.
func (p *itemPast) inProgressSince(from time.Time) bool {
	var status string
	for _, row := range p.history {
		if d, ok := row.deltas["status"]; ok {
			if status == string(StatusInProgress) && !row.at.Before(from) {
				return true
			}
			status = d.To
		}
	}
	return status == string(StatusInProgress)
}

func (p *itemPast) readRecorded(ctx context.Context, tx *sql.Tx) error {
	rows, err := tx.QueryContext(ctx, pastRecordedStatement, p.id)
	if err != nil {
		return fmt.Errorf("worklet: read item %s's recorded flow: %w", p.id, err)
	}
	defer func() { _ = rows.Close() }()
	read := false
	for rows.Next() {
		var id, notice string
		var backfilled int
		var at int64
		if err := rows.Scan(&id, &backfilled, &notice, &at); err != nil {
			return err
		}
		p.recorded[id] = true
		if !read && backfilled == 0 && notice == "" {
			read = true
			if first := store.DecodeTime(at); first.Before(p.until) {
				p.until = first
			}
		}
	}
	return rows.Err()
}

func (p *itemPast) readTurns(ctx context.Context, tx *sql.Tx) error {
	rows, err := tx.QueryContext(ctx, pastTurnsStatement, p.id)
	if err != nil {
		return fmt.Errorf("worklet: read item %s's turns: %w", p.id, err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var turn pastTurn
		var end, wall int64
		if err := rows.Scan(&turn.seat, &end, &wall); err != nil {
			return err
		}
		turn.to = store.DecodeTime(end)
		turn.from = turn.to.Add(-time.Duration(wall) * time.Millisecond)
		p.turns = append(p.turns, turn)
	}
	return rows.Err()
}

func (p *itemPast) readAsks(ctx context.Context, tx *sql.Tx) error {
	rows, err := tx.QueryContext(ctx, pastAsksStatement, p.id)
	if err != nil {
		return fmt.Errorf("worklet: read item %s's questions: %w", p.id, err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var at [4]sql.NullInt64
		if err := rows.Scan(&at[0], &at[1], &at[2], &at[3]); err != nil {
			return err
		}
		for _, v := range at {
			if v.Valid {
				p.asks = append(p.asks, store.DecodeTime(v.Int64))
			}
		}
	}
	return rows.Err()
}

// readBlockers reads the status over time of every item the item ever waited
// on.
func (p *itemPast) readBlockers(ctx context.Context, tx *sql.Tx) error {
	seen := map[string]bool{}
	for _, row := range p.history {
		for _, id := range row.deltas[string(RelationWaitingOn)].Added {
			seen[id] = true
		}
	}
	for _, id := range sortedSet(seen) {
		rows, err := readPastRows(ctx, tx, id)
		if err != nil {
			return err
		}
		for _, row := range rows {
			if d, ok := row.deltas["status"]; ok {
				p.blockers[id] = append(p.blockers[id], pastStatus{at: row.at, status: Status(d.To)})
			}
		}
		if len(p.blockers[id]) > 0 {
			continue
		}
		var open int
		err = tx.QueryRowContext(ctx, pastDepStatement, id, p.id).Scan(&open)
		missing := errors.Is(err, sql.ErrNoRows)
		if err != nil && !missing {
			return fmt.Errorf("worklet: read item %s's edge to %s: %w", p.id, id, err)
		}
		p.open[id] = missing || open == 1
	}
	return nil
}

// pastRowAt is the item's row as it stood at an instant, as its history
// spells it.
type pastRowAt struct {
	status, assignee, project, reason string
	start                             *time.Time
	updated                           time.Time
	waiting                           []string
	// entered is when its present span in progress began.
	entered time.Time
}

// rowAt folds the item's history up to at.
func (p *itemPast) rowAt(at time.Time) pastRowAt {
	var out pastRowAt
	waiting := map[string]bool{}
	for _, row := range p.history {
		if row.at.After(at) {
			break
		}
		out.updated = row.at
		for field, d := range row.deltas {
			switch field {
			case "status":
				if d.To == string(StatusInProgress) && out.status != d.To {
					out.entered = row.at
				}
				out.status = d.To
			case "assignee":
				out.assignee = d.To
			case "project":
				out.project = d.To
			case "wait_reason":
				out.reason = d.To
			case "start":
				out.start = p.startOf(d.To)
			case string(RelationWaitingOn):
				for _, id := range d.Added {
					waiting[id] = true
				}
				for _, id := range d.Removed {
					delete(waiting, id)
				}
			}
		}
	}
	if out.project == "" {
		out.project = p.project
	}
	out.waiting = sortedSet(waiting)
	return out
}

// startOf reads a start as a history row spells it, to the second, taking
// the exact instant the item's column holds where the two agree.
func (p *itemPast) startOf(text string) *time.Time {
	if text == "" {
		return nil
	}
	if p.start != nil && instantText(p.start) == text {
		at := *p.start
		return &at
	}
	at, err := time.Parse(time.RFC3339, text)
	if err != nil {
		return nil
	}
	at = at.UTC()
	return &at
}

// blockerOpen reports whether a blocker was open at an instant: not finished
// by its own history, or open as its edge stands where it has none.
func (p *itemPast) blockerOpen(id string, at time.Time) bool {
	statuses, ok := p.blockers[id]
	if !ok {
		return p.open[id]
	}
	open := true
	for _, s := range statuses {
		if s.at.After(at) {
			break
		}
		open = !s.status.Group().Finished()
	}
	return open
}

// instants are every instant the item's flow could change at, ascending:
// each change it took, each turn's start and end, each start it waited for,
// each instant it would read stuck, each blocker's change and each question
// opening or closing, and each seat's stop or park beginning or ending.
func (p *itemPast) instants(window time.Duration, seats []time.Time) []time.Time {
	var out []time.Time
	for _, row := range p.history {
		out = append(out, row.at, row.at.Add(window))
		if d, ok := row.deltas["start"]; ok {
			if start := p.startOf(d.To); start != nil {
				out = append(out, *start)
			}
		}
	}
	for _, turn := range p.turns {
		out = append(out, turn.from, turn.to)
	}
	for _, statuses := range p.blockers {
		for _, s := range statuses {
			out = append(out, s.at)
		}
	}
	out = append(out, p.asks...)
	out = append(out, seats...)
	slices.SortFunc(out, func(a, b time.Time) int { return a.Compare(b) })
	return slices.CompactFunc(out, func(a, b time.Time) bool { return a.Equal(b) })
}

// pastFlow is every change of the item's flow its past gives before the
// backfill's end, as the duty would have recorded each: the duty's expression
// evaluated at each instant the past could change, a change recorded where
// the flow left the one before it ([flowItem.changed]) under the duty's key,
// working never, and a span in progress the duty already read never.
func (r *Reader) pastFlow(ctx context.Context, past *itemPast, seats SeatPast,
	wrote statelog.Position) ([]FlowRecord, error) {

	var first time.Time
	assignees := map[string]bool{}
	for _, row := range past.history {
		if first.IsZero() {
			first = row.at
		}
		if d, ok := row.deltas["assignee"]; ok && d.To != "" {
			assignees[d.To] = true
		}
	}
	histories := map[string]SeatHistory{}
	var edges []time.Time
	if seats != nil {
		for _, seat := range sortedSet(assignees) {
			h, err := seats.SeatPast(ctx, seat, first, past.until)
			if err != nil {
				return nil, fmt.Errorf("worklet: read seat %s's past: %w", seat, err)
			}
			histories[seat] = h
			edges = append(edges, h.edges()...)
		}
	}
	var out []FlowRecord
	err := r.readFill(ctx, wrote, func(tx *sql.Tx) error {
		out = out[:0]
		window, err := stuckWindow(ctx, tx, past.project)
		if err != nil {
			return err
		}
		var last *flowChange
		for _, at := range past.instants(window, edges) {
			if at.IsZero() || !at.Before(past.until) {
				continue
			}
			row := past.rowAt(at)
			if row.status != string(StatusInProgress) || !row.entered.Before(past.horizon) {
				continue
			}
			history, seated := histories[row.assignee], false
			posture := Posture{Live: true, Seats: map[string]SeatPosture{}}
			if row.assignee != "" {
				seat := history.posture(at)
				seated = history.covers(at)
				for _, turn := range past.turns {
					if turn.seat == row.assignee && within(at, turn.from, turn.to) {
						seat.On = append(seat.On, PostureItem{Item: past.id,
							Cause: FlowCauseTurn, Since: turn.from})
					}
				}
				posture.Seats[row.assignee] = seat
			}
			flow, err := past.evaluate(ctx, tx, flowAt{now: at.UTC(), posture: posture}, row, last)
			if err != nil {
				return err
			}
			if flow == nil || flow.State == FlowWorking {
				continue
			}
			item := flowItem{id: past.id, assignee: row.assignee, flow: flow, last: last}
			if !item.changed() {
				continue
			}
			previous := ""
			if last != nil {
				previous = last.id
			}
			id := FlowChangeID(past.id, flow.State, flow.Cause, flow.Since, previous)
			out = append(out, FlowRecord{ID: id, Task: past.id, State: flow.State,
				Cause: flow.Cause, Since: flow.Since, Until: flow.Until, Seat: row.assignee,
				Backfilled: true, Past: &FlowPast{At: at.UTC(), Seats: seated}})
			last = &flowChange{id: id, state: flow.State, cause: flow.Cause,
				since: flow.Since, recorded: at.UTC()}
		}
		return nil
	})
	return out, err
}

// stuckWindow is a project's stuck window as it stands: a project keeps no
// history of it.
func stuckWindow(ctx context.Context, tx *sql.Tx, project string) (time.Duration, error) {
	var hours sql.NullInt64
	err := tx.QueryRowContext(ctx, `SELECT stuck_after_hours FROM tracker_projects WHERE key = ?`,
		project).Scan(&hours)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return 0, fmt.Errorf("worklet: read project %s's stuck window: %w", project, err)
	}
	if !hours.Valid || hours.Int64 == 0 {
		return DefaultStuckAfterHours * time.Hour, nil
	}
	return time.Duration(hours.Int64) * time.Hour, nil
}

// evaluate reads THE flow expression over the item as it stood at the
// instant: [pastFlowStatement]'s tables as of then.
func (p *itemPast) evaluate(ctx context.Context, tx *sql.Tx, at flowAt, row pastRowAt,
	last *flowChange) (*Flow, error) {

	statement, args := pastFlowStatement(at, p, row, last)
	var packed []byte
	if err := tx.QueryRowContext(ctx, statement, args...).Scan(&packed); err != nil {
		return nil, fmt.Errorf("worklet: read item %s's flow at %s: %w", p.id, at.now, err)
	}
	return decodeFlow(packed)
}

// pastFlowStatement is [flowExpr] over an item as it stood at an instant: a
// statement whose common tables, named as the tables the expression reads,
// shadow each with what it held then. The row is the item's as its history
// spells it; the turns and history rows are those that took effect by then;
// a question is open where it was asked and neither answered, resolved nor
// removed by then; an edge is open where its blocker was unfinished then; the
// latest change is the backfill's own; and no roster is read, since one keeps
// no history and a seat unknown is never departed. A column the expression
// comes to read that these do not carry fails the statement, never reads as
// absent.
func pastFlowStatement(at flowAt, p *itemPast, row pastRowAt, last *flowChange) (string, []any) {
	now := store.EncodeTime(at.now)
	var args []any
	var b strings.Builder
	b.WriteString("WITH tracker_task_deps(task_id, blocker_open) AS (")
	if len(row.waiting) == 0 {
		b.WriteString("SELECT NULL, NULL WHERE 0")
	} else {
		b.WriteString("VALUES ")
		for i, id := range row.waiting {
			if i > 0 {
				b.WriteString(", ")
			}
			b.WriteString("(?, ?)")
			args = append(args, p.id, boolInt(p.blockerOpen(id, at.now)))
		}
	}
	b.WriteString(`),
		tracker_comments(task_id, author, ask, resolved, answered_by, removed) AS (
			SELECT c.task_id, c.author, c.ask,
				CASE WHEN c.resolved = 1 AND COALESCE(c.resolved_at, 0) <= ? THEN 1 ELSE 0 END,
				(SELECT a.id FROM main.tracker_comments a WHERE a.id = c.answered_by
				 AND a.created_at <= ?),
				CASE WHEN c.removed = 1 AND c.updated_at <= ? THEN 1 ELSE 0 END
			FROM main.tracker_comments c WHERE c.task_id = ? AND c.created_at <= ?),
		tracker_turns(task_id, seat, effective_at) AS (
			SELECT task_id, seat, effective_at FROM main.tracker_turns
			WHERE task_id = ? AND effective_at <= ?),
		tracker_history(subject_id, actor, effective_at) AS (
			SELECT subject_id, actor, effective_at FROM main.tracker_history
			WHERE subject_id = ? AND effective_at <= ?),
		tracker_rosters(name) AS (SELECT NULL WHERE 0),
		tracker_seat_places(handle) AS (SELECT NULL WHERE 0),
		tracker_flow(task_id, state, notice, backfilled, log_seq, recorded_at) AS (`)
	args = append(args, now, now, now, p.id, now, p.id, now, p.id, now)
	if last == nil {
		b.WriteString("SELECT NULL, NULL, NULL, NULL, NULL, NULL WHERE 0")
	} else {
		b.WriteString("VALUES (?, ?, '', 0, 1, ?)")
		args = append(args, p.id, string(last.state), store.EncodeTime(last.recorded))
	}
	b.WriteString(`),
		flow_past(id, status, start_at, assignee, updated_at, document, project_key) AS (
			VALUES (?, ?, ?, ?, ?, json_object('wait_reason', ?), ?))
		SELECT `)
	var start any
	if row.start != nil {
		start = store.EncodeTime(*row.start)
	}
	args = append(args, p.id, row.status, start, row.assignee, store.EncodeTime(row.updated),
		row.reason, row.project)
	expr, exprArgs := flowExpr(at, flowWhole)
	b.WriteString(expr)
	b.WriteString(" FROM flow_past t")
	return b.String(), append(args, exprArgs...)
}

// sortedSet is a set's members in order.
func sortedSet(set map[string]bool) []string {
	out := make([]string, 0, len(set))
	for key := range set {
		out = append(out, key)
	}
	slices.Sort(out)
	return out
}
