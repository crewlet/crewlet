package worklet

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"time"

	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/store"
)

// The flow duty records each change of an item's flow state and tells a
// person when nobody is moving the item (ADR-0162; package doc, "An item in
// progress says why it is not moving"): a fleet singleton every minute, held
// where the live projection is, reading every item in progress a page at a
// time, since nothing written marks a state that changed with time or a seat.

// FlowDueGrace is how long a due item may wait before a person is told, and
// the lead's step after it. BORROWED, not chosen (ADR-0015): it is the inbox's
// acknowledgement window (jetstream.DefaultAckWait, which an engine test holds
// it to), which the engine sizes as a wait plus a worst-case turn, so a lane
// pickup handed at the start has had every turn the engine expects to run first.
const FlowDueGrace = 30 * time.Minute

// FlowDutyInterval is how often the duty reads every item in progress: the
// unit a due item's grace and a held urgent item's notice are late by.
const FlowDutyInterval = time.Minute

// flowDutyPage is how many items one page's statement reads: a read
// transaction holding its snapshot for a few milliseconds of index seeks, so a
// company with thousands of items in progress is read in a handful of pages.
const flowDutyPage = 500

// FlowChart is what the flow duty reads of the org chart, per call: the lead
// a notice goes to, and whether an assignee is a person to tell. An agent's
// notice is its pickup, and a seat the chart no longer holds reads nothing.
type FlowChart interface {
	Leads

	// Person reports whether handle is a person's seat in the chart.
	Person(handle string) bool
}

// FlowDutyDeps is what the flow duty needs that it does not own.
type FlowDutyDeps struct {
	// Reader reads the items in progress with the node's live posture.
	Reader *Reader
	// Writer publishes the flow records, as the engine.
	Writer *Writer
	// Chart resolves leads and agent seats against the current chart.
	Chart FlowChart
	// NodeID goes into every operation id the duty mints.
	NodeID string
	// Logger is where the duty reports; nil is the package's own.
	Logger *slog.Logger
}

// FlowDuty is the duty over its deps, and the position of its own last
// write, which its next tick's reads wait for so it never publishes a fact
// twice because its own record had not yet been applied. Not the same tick's:
// a tick reads each item once, and a page waiting on the records the page
// before it published would wait on the applier for nothing.
type FlowDuty struct {
	deps  FlowDutyDeps
	wrote statelog.Position
}

// NewFlowDuty is the duty, refusing deps it cannot run without.
func NewFlowDuty(d FlowDutyDeps) (*FlowDuty, error) {
	if d.Reader == nil || d.Writer == nil || d.Chart == nil {
		return nil, errors.New("worklet: the flow duty needs a reader, a writer and the chart")
	}
	if d.Logger == nil {
		d.Logger = log
	}
	return &FlowDuty{deps: d}, nil
}

// Live reports whether this node's posture is live now: whether the duty may
// hold its lease here.
func (d *FlowDuty) Live() bool { return d.deps.Reader.flowAt(time.Now()).live() }

// FlowTick is what one tick did.
type FlowTick struct {
	// Live says the tick read a live posture; a tick that did not read
	// nothing and records nothing.
	Live bool
	// Items is how many items in progress it read, Changes the changes of
	// state it recorded and Notices the people it told.
	Items, Changes, Notices int
}

// Tick reads every item in progress at now and publishes what each owes: a
// change of its state, or the notices its state has come due for. One item's
// failure is logged and the rest go on: the items are independent.
func (d *FlowDuty) Tick(ctx context.Context, now time.Time) (FlowTick, error) {
	at := d.deps.Reader.flowAt(now)
	out := FlowTick{Live: at.live()}
	if !out.Live {
		return out, nil
	}
	wrote := d.wrote
	for after := ""; ; {
		page, err := d.deps.Reader.flowPage(ctx, at, after, wrote)
		if err != nil {
			return out, err
		}
		out.Items += len(page)
		for _, item := range page {
			for _, rec := range item.owed(at.now, d.deps.Chart) {
				if err := d.publish(ctx, at.now, rec); err != nil {
					d.deps.Logger.WarnContext(ctx, "tracker_flow_not_recorded",
						"item", rec.Task, "state", string(rec.State),
						"notice", string(rec.Notice), "error", err)
					continue
				}
				if rec.Notice == "" {
					out.Changes++
				} else {
					out.Notices++
				}
			}
		}
		if len(page) < flowDutyPage {
			break
		}
		after = page[len(page)-1].id
	}
	if out.Changes > 0 || out.Notices > 0 {
		d.deps.Logger.InfoContext(ctx, "tracker_flow_recorded", "items", out.Items,
			"changes", out.Changes, "notices", out.Notices)
	}
	return out, nil
}

// publish writes one record under an operation id derived from the tick, so a
// retry inside it is one operation; the record's own key lands it once across
// ticks.
func (d *FlowDuty) publish(ctx context.Context, now time.Time, rec FlowRecord) error {
	tick := now.Truncate(time.Second)
	op := statelog.DeriveOpID(tick, "flow-"+rec.Task, "crewlet.tracker.flow",
		d.deps.NodeID, rec.ID, strconv.FormatInt(tick.Unix(), 10))
	res, err := d.deps.Writer.RecordFlow(ctx, op, rec)
	if err != nil {
		return err
	}
	if res.Position.Seq > d.wrote.Seq || res.Position.Stream != d.wrote.Stream {
		d.wrote = res.Position
	}
	return nil
}

// flowItem is one item in progress as the duty reads it.
type flowItem struct {
	id, project, unit, assignee string
	urgent                      bool
	flow                        *Flow

	// dueGraced and quiet are what the item would be were it not held: its
	// start passed the grace ago with nothing of the assignee's since, and
	// nothing changed on it for its project's window.
	dueGraced, quiet bool
	window           time.Duration

	// last is the item's latest recorded change of state, nil for none, and
	// told the roles told since it.
	last *flowChange
	told []FlowRole
}

// flowChange is a recorded change of an item's state.
type flowChange struct {
	id       string
	state    FlowState
	cause    FlowCause
	since    *time.Time
	recorded time.Time
}

// owed is what the duty owes the item at now: a change of state where its
// flow left the recorded one, else every notice its state has come due for and
// nobody was given. A change is recorded before any notice of it, so a notice
// is keyed on a change every node holds; its notices come a tick later.
func (it flowItem) owed(now time.Time, chart FlowChart) []FlowRecord {
	f := it.flow
	if f == nil || f.State == FlowWorking {
		// Working time is the turns' own record and is never copied.
		return nil
	}
	record := FlowRecord{Task: it.id, State: f.State, Cause: f.Cause, Since: f.Since,
		Until: f.Until, Seat: it.assignee}
	if it.changed() {
		previous := ""
		if it.last != nil {
			previous = it.last.id
		}
		record.ID = FlowChangeID(it.id, f.State, f.Cause, f.Since, previous)
		return []FlowRecord{record}
	}
	entered := it.last.recorded
	if it.last.since != nil {
		entered = *it.last.since
	}
	var out []FlowRecord
	for _, step := range it.steps(entered, chart) {
		if now.Before(step.at) || it.toldAs(step.role) {
			continue
		}
		notice := record
		notice.Notice, notice.Told = step.role, step.who
		notice.ID = FlowNoticeID(it.id, f.State, entered, step.who)
		out = append(out, notice)
	}
	return out
}

// changed reports a flow that left the recorded one: another state or cause,
// or, for a state whose instant is a fact, another instant. A moving item's
// instant is its last change, which every edit moves without it leaving the
// state, and a blocked item's is the recording's own.
func (it flowItem) changed() bool {
	last, f := it.last, it.flow
	if last == nil || last.state != f.State || last.cause != f.Cause {
		return true
	}
	switch f.State {
	case FlowDue, FlowStuck, FlowHeld, FlowWaiting:
		return !sameInstant(last.since, f.Since)
	}
	return false
}

func sameInstant(a, b *time.Time) bool {
	if a == nil || b == nil {
		return a == b
	}
	return a.Equal(*b)
}

// flowStep is one notice an item's state owes: to whom, in which role, from
// when.
type flowStep struct {
	role FlowRole
	who  string
	at   time.Time
}

// steps are the notices the item's state owes since it entered it, in order.
func (it flowItem) steps(entered time.Time, chart FlowChart) []flowStep {
	lead := chart.ProjectLead(it.project)
	if lead == "" {
		lead = chart.UnitLead(it.unit)
	}
	if lead == it.assignee {
		// A lead told about its own item hears nothing its own read lacks.
		lead = ""
	}
	var first, second time.Time
	switch it.flow.State {
	case FlowDue:
		first, second = entered.Add(FlowDueGrace), entered.Add(2*FlowDueGrace)
	case FlowStuck:
		// Entered is the last change, so the first step is the moment the
		// item became stuck.
		first, second = entered.Add(it.window), entered.Add(2*it.window)
	case FlowHeld:
		if !it.holdCosts() || lead == "" {
			return nil
		}
		return []flowStep{{role: FlowToldLead, who: lead, at: entered}}
	default:
		return nil
	}
	var out []flowStep
	if it.assignee != "" && chart.Person(it.assignee) {
		out = append(out, flowStep{role: FlowToldAssignee, who: it.assignee, at: first})
		first = second
	}
	if lead != "" {
		out = append(out, flowStep{role: FlowToldLead, who: lead, at: first})
	}
	return out
}

// holdCosts reports a hold somebody should act on: the item is urgent, or it
// would be due past the grace or quiet past its window were it not held. A
// hold that ends on its own on an item nothing waits for asks nobody to act.
func (it flowItem) holdCosts() bool { return it.urgent || it.dueGraced || it.quiet }

// toldAs reports whether the item's current state was already told in role.
func (it flowItem) toldAs(role FlowRole) bool {
	for _, told := range it.told {
		if told == role {
			return true
		}
	}
	return false
}

// flowLastChange is an item's latest recorded change of state, packed, and
// flowToldSince the roles told since it, each one seek of
// tracker_flow_task_idx per item.
const (
	flowLastChange = `SELECT json_array(fl.id, fl.state, fl.cause, fl.since, fl.recorded_at)
		FROM tracker_flow fl WHERE fl.task_id = t.id AND fl.notice = ''
		ORDER BY fl.log_seq DESC LIMIT 1`
	flowToldSince = `SELECT group_concat(fn.notice, ',') FROM tracker_flow fn
		WHERE fn.task_id = t.id AND fn.notice <> '' AND fn.log_seq > COALESCE(
			(SELECT fc.log_seq FROM tracker_flow fc WHERE fc.task_id = t.id AND fc.notice = ''
			 ORDER BY fc.log_seq DESC LIMIT 1), 0)`
)

// flowPageStatement is the duty's one statement per page, named for the plan
// test: the items in progress after an id, through the partial index over
// them, each with its flow at `at`, what it would read were it not held, its
// project's window and what was recorded of it.
func flowPageStatement(at flowAt) (string, []any) {
	flow, args := flowExpr(at, flowWhole)
	query := `SELECT t.id, t.project_key, t.routing_unit, t.assignee,
		t.priority = '` + string(PriorityUrgent) + `', ` + flow + `,
		CASE WHEN t.start_at <= ? AND ` + flowNothingSinceStart + ` THEN 1 ELSE 0 END,
		CASE WHEN ` + flowStuck + ` THEN 1 ELSE 0 END,
		COALESCE((SELECT fw.stuck_after_hours FROM tracker_projects fw
		          WHERE fw.key = t.project_key), ?),
		(` + flowLastChange + `), (` + flowToldSince + `)
		FROM tracker_tasks t INDEXED BY tracker_tasks_in_progress_idx
		WHERE t.status = '` + string(StatusInProgress) + `' AND t.removed_at IS NULL AND t.id > ?
		ORDER BY t.id LIMIT ?`
	return query, append(args, store.EncodeTime(at.now.Add(-FlowDueGrace)),
		store.EncodeTime(at.now), DefaultStuckAfterHours, DefaultStuckAfterHours)
}

// flowPage reads one page of the items in progress after an id, no older than
// the duty's own last write.
func (r *Reader) flowPage(ctx context.Context, at flowAt, after string,
	wrote statelog.Position) ([]flowItem, error) {

	statement, args := flowPageStatement(at)
	args = append(args, after, flowDutyPage)
	var out []flowItem
	_, err := r.log.Read(ctx, statelog.Query{
		Level:   statelog.ReadSession,
		Scope:   statelog.ScopeSet{Paths: []string{pathDomain}}.Normalised(),
		Session: wrote,
		Set:     true,
	}, func(tx *sql.Tx) error {
		out = out[:0]
		rows, err := tx.QueryContext(ctx, statement, args...)
		if err != nil {
			return fmt.Errorf("worklet: read the items in progress: %w", err)
		}
		defer func() { _ = rows.Close() }()
		for rows.Next() {
			item, err := scanFlowItem(rows)
			if err != nil {
				return err
			}
			out = append(out, item)
		}
		return rows.Err()
	})
	return out, err
}

// scanFlowItem reads one row of [flowPageStatement].
func scanFlowItem(rows *sql.Rows) (flowItem, error) {
	var item flowItem
	var flow []byte
	var urgent, dueGraced, quiet int
	var hours int64
	var last, told sql.NullString
	if err := rows.Scan(&item.id, &item.project, &item.unit, &item.assignee, &urgent,
		&flow, &dueGraced, &quiet, &hours, &last, &told); err != nil {
		return flowItem{}, fmt.Errorf("worklet: read an item in progress: %w", err)
	}
	var err error
	if item.flow, err = decodeFlow(flow); err != nil {
		return flowItem{}, err
	}
	item.urgent, item.dueGraced, item.quiet = urgent == 1, dueGraced == 1, quiet == 1
	item.window = time.Duration(hours) * time.Hour
	if last.Valid {
		if item.last, err = decodeFlowChange(last.String); err != nil {
			return flowItem{}, fmt.Errorf("worklet: read item %s's recorded flow: %w", item.id, err)
		}
	}
	if told.Valid && told.String != "" {
		for _, role := range strings.Split(told.String, ",") {
			item.told = append(item.told, FlowRole(role))
		}
	}
	return item, nil
}

// decodeFlowChange reads [flowLastChange]'s packed row.
func decodeFlowChange(packed string) (*flowChange, error) {
	var parts []json.RawMessage
	if err := json.Unmarshal([]byte(packed), &parts); err != nil || len(parts) != 5 {
		return nil, fmt.Errorf("a recorded change packs five facts, not %q", packed)
	}
	var out flowChange
	var since *int64
	var recorded int64
	for i, target := range []any{&out.id, &out.state, &out.cause, &since, &recorded} {
		if err := json.Unmarshal(parts[i], target); err != nil {
			return nil, fmt.Errorf("decode %q: %w", packed, err)
		}
	}
	out.since, out.recorded = decodedInstant(since), store.DecodeTime(recorded)
	return &out, nil
}
