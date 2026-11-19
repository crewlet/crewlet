package worklet

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"

	"github.com/crewlet/crewlet/internal/period"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/store"
)

// Who is carrying how much, across every project at once, in one read and one
// transaction, so no surface fans out a read per project and sums the rows.
// It counts every OPEN assigned task whatever its dates: "is this person
// overloaded" is about their whole queue. Beside it, each in-progress item
// by its flow ([FlowState]), and the load: the items that want the seat now.

// WorkloadQuery asks who is carrying how much.
type WorkloadQuery struct {
	// Unit narrows to open work in projects this unit owns, named by its id
	// or its name. Empty is the whole company.
	Unit string

	// Units resolves that reference — see [Units]. Nil matches it
	// literally, the honest answer for a surface with no chart.
	//
	// NOT SERIALISED: JSON writes an interface as `{}` and cannot decode it
	// back, so the far side of a wire attaches ITS OWN chart (internal/estate).
	Units Units `json:"-"`

	Level     statelog.ReadLevel
	Session   statelog.Position
	Floors    []statelog.Position
	MaxLag    time.Duration
	MaxLagSeq uint64
}

// MaxWorkloadHandles is how many people one answer names.
//
// 256 is an order of magnitude above the seat count of any configuration in
// this tree; a bound is needed at all because "everybody with open work" is
// not knowable before the read.
const MaxWorkloadHandles = 256

// WorkloadRow is one person's load.
type WorkloadRow struct {
	Handle string `json:"handle"`

	// Open is how many open tasks they hold.
	Open int `json:"open"`

	// Points and EstimateMinutes are both carried, never one chosen: teams
	// size in different measures, and picking one is wrong for the other.
	Points          float64 `json:"points"`
	EstimateMinutes int     `json:"estimate_min"`

	// Blocked, Overdue and Unscheduled ride beside the totals, not folded
	// in: a person whose whole queue is blocked has a different problem
	// from one who is simply busy.
	Blocked     int `json:"blocked"`
	Overdue     int `json:"overdue"`
	Unscheduled int `json:"unscheduled"`

	// Flow counts their items in progress by flow state, read with the
	// expression every row reads ([Flow]).
	Flow FlowCounts `json:"flow"`

	// Load is their items in progress that want the seat now
	// ([FlowState.Loads]): an item parked until tomorrow, blocked or held
	// is open work and no load.
	Load int `json:"load"`
}

// FlowCounts is one seat's items in progress, a count per [FlowState].
// Nested rather than beside the totals: [WorkloadRow.Blocked] already counts
// every open item with an open blocker, to do and in review included.
type FlowCounts struct {
	Working int `json:"working"`
	Blocked int `json:"blocked"`
	Waiting int `json:"waiting"`
	Held    int `json:"held"`
	Due     int `json:"due"`
	Stuck   int `json:"stuck"`
	Moving  int `json:"moving"`
}

// counter is where a census of state counts into.
func (c *FlowCounts) counter(state FlowState) *int {
	switch state {
	case FlowWorking:
		return &c.Working
	case FlowBlocked:
		return &c.Blocked
	case FlowWaiting:
		return &c.Waiting
	case FlowHeld:
		return &c.Held
	case FlowDue:
		return &c.Due
	case FlowStuck:
		return &c.Stuck
	case FlowMoving:
		return &c.Moving
	}
	return nil
}

// WorkloadAnswer is the whole company's load.
type WorkloadAnswer struct {
	Rows []WorkloadRow `json:"rows"`

	// Truncated says the answer stopped at [MaxWorkloadHandles].
	Truncated bool `json:"truncated,omitempty"`

	// FlowLive is [Answer.FlowLive] for every row's flow counts.
	FlowLive bool `json:"flow_live"`

	Level          statelog.ReadLevel `json:"read_level,omitempty"`
	LogSeq         uint64             `json:"log_seq,omitempty"`
	AppliedThrough uint64             `json:"applied_through,omitempty"`
	LogLag         *uint64            `json:"log_lag,omitempty"`
	Complete       bool               `json:"complete"`
	Incomplete     *Incomplete        `json:"incomplete,omitempty"`
}

// Workload answers who is carrying how much, against what they can take.
//
// `now` and `loc` are the instant and the company's clock the overdue count is
// cut on — see [Reader.MyWork], which cuts the same day.
func (r *Reader) Workload(ctx context.Context, q WorkloadQuery, now time.Time,
	loc *time.Location) (WorkloadAnswer, error) {

	if q.Level == "" {
		return WorkloadAnswer{}, fmt.Errorf("worklet: this workload read " +
			"names no level — a surface resolves an absent read_level to " +
			"its own default before it reads")
	}
	flow := r.flowAt(now)
	out := WorkloadAnswer{FlowLive: flow.live()}
	served, err := r.log.Read(ctx, statelog.Query{
		Level: q.Level,
		// THE DOMAIN, as in [Reader.MyWork]: a narrower closure would
		// certify the answer complete while a deferred record in some
		// project held work that belongs in it.
		Scope:     statelog.ScopeSet{Paths: []string{pathDomain}}.Normalised(),
		Session:   q.Session,
		Floors:    q.Floors,
		MaxLag:    q.MaxLag,
		MaxLagSeq: q.MaxLagSeq,
		Set:       true,
	}, func(tx *sql.Tx) error {
		return readWorkload(ctx, tx, q, flow, loc, &out)
	})
	if err != nil {
		return WorkloadAnswer{}, err
	}
	out.Level = served.Level
	out.Complete = served.Complete
	out.LogLag = served.Lag
	if served.Incomplete != nil {
		out.Incomplete = incompleteFrom(served.Incomplete)
	}
	return out, nil
}

// workloadStatement is [readWorkload]'s one statement, named for the plan
// test: every assignee's open work, across the company or in the projects of
// units, counted against the day that begins at dayStart, with each item's
// flow at `at` read once per row inside it and counted per state.
func workloadStatement(units []string, dayStart int64, at flowAt) (string, []any) {
	flow, args := flowExpr(at, flowStateOnly)
	args = append(args, dayStart)
	where := "t.removed_at IS NULL AND t.archived = 0 AND t.assignee <> '' " +
		"AND t.status_group IN (" + openGroupsSQL + ")"
	if len(units) > 0 {
		where += " AND EXISTS (SELECT 1 FROM tracker_projects p " +
			"WHERE p.key = t.project_key AND p.unit IN (" +
			placeholders(len(units)) + "))"
		args = append(args, anyOf(units)...)
	}
	var buckets strings.Builder
	for _, state := range FlowStates {
		fmt.Fprintf(&buckets, ",\n\t\t       COALESCE(SUM(CASE WHEN w.flow = '%s' THEN 1 ELSE 0 END), 0)", state)
	}
	return `
		SELECT w.assignee,
		       COUNT(*),
		       COALESCE(SUM(w.points), 0),
		       COALESCE(SUM(w.estimate_min), 0),
		       COALESCE(SUM(w.blocked), 0),
		       COALESCE(SUM(w.overdue), 0),
		       COALESCE(SUM(w.unscheduled), 0)` + buckets.String() + `
		FROM (SELECT t.assignee, t.points, t.estimate_min,
		             ` + flow + ` AS flow,
		             EXISTS (SELECT 1 FROM tracker_task_deps d
		                     WHERE d.task_id = t.id AND d.blocker_open = 1) AS blocked,
		             CASE WHEN t.due_at IS NOT NULL AND t.due_at < ?
		               THEN 1 ELSE 0 END AS overdue,
		             CASE WHEN t.start_at IS NULL AND t.due_at IS NULL
		               THEN 1 ELSE 0 END AS unscheduled
		      FROM tracker_tasks t
		      WHERE ` + where + `) w
		GROUP BY w.assignee
		ORDER BY 2 DESC, w.assignee
		LIMIT ?`, append(args, MaxWorkloadHandles+1)
}

func readWorkload(ctx context.Context, tx *sql.Tx, q WorkloadQuery,
	flow flowAt, loc *time.Location, out *WorkloadAnswer) error {

	// The company's own midnight, resolved as [readMyWork] does, so the
	// overdue count matches what a board marks overdue.
	anchor, err := period.Resolve("today", flow.now, loc)
	if err != nil {
		return err
	}
	dayStart := store.EncodeTime(anchor.At)

	// Through the chart ([unitKeys]): the unit came from another screen,
	// so its id and its name both have to work.
	statement, args := workloadStatement(unitKeys(q.Units, []string{q.Unit}), dayStart, flow)
	rows, err := tx.QueryContext(ctx, statement, args...)
	if err != nil {
		return fmt.Errorf("worklet: read the company's workload: %w", err)
	}
	defer func() { _ = rows.Close() }()

	out.Rows = []WorkloadRow{}
	for rows.Next() {
		var row WorkloadRow
		targets := []any{&row.Handle, &row.Open, &row.Points,
			&row.EstimateMinutes, &row.Blocked, &row.Overdue, &row.Unscheduled}
		for _, state := range FlowStates {
			targets = append(targets, row.Flow.counter(state))
		}
		if err = rows.Scan(targets...); err != nil {
			return fmt.Errorf("worklet: scan a workload row: %w", err)
		}
		for _, state := range FlowStates {
			if state.Loads() {
				row.Load += *row.Flow.counter(state)
			}
		}
		out.Rows = append(out.Rows, row)
	}
	if err = rows.Err(); err != nil {
		return fmt.Errorf("worklet: walk the workload rows: %w", err)
	}
	if len(out.Rows) > MaxWorkloadHandles {
		out.Rows = out.Rows[:MaxWorkloadHandles]
		out.Truncated = true
	}

	position, applied, err := readCheckpoint(ctx, tx)
	if err != nil {
		return err
	}
	out.LogSeq, out.AppliedThrough = position, applied
	return nil
}
