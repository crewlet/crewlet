package worklet

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/crewlet/crewlet/internal/period"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/store"
)

// Who is carrying how much, across every project at once, in one read and one
// transaction, so no surface fans out a read per project and sums the rows.
// It counts every OPEN assigned task whatever its dates: "is this person
// overloaded" is about their whole queue.

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
}

// WorkloadAnswer is the whole company's load.
type WorkloadAnswer struct {
	Rows []WorkloadRow `json:"rows"`

	// Truncated says the answer stopped at [MaxWorkloadHandles].
	Truncated bool `json:"truncated,omitempty"`

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
	var out WorkloadAnswer
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
		return readWorkload(ctx, tx, q, now, loc, &out)
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
// units, counted against the day that begins at dayStart.
func workloadStatement(units []string, dayStart int64) (string, []any) {
	where := "t.removed_at IS NULL AND t.archived = 0 AND t.assignee <> '' " +
		"AND t.status_group IN (" + openGroupsSQL + ")"
	args := []any{dayStart}
	if len(units) > 0 {
		where += " AND EXISTS (SELECT 1 FROM tracker_projects p " +
			"WHERE p.key = t.project_key AND p.unit IN (" +
			placeholders(len(units)) + "))"
		args = append(args, anyOf(units)...)
	}
	return `
		SELECT t.assignee,
		       COUNT(*),
		       COALESCE(SUM(t.points), 0),
		       COALESCE(SUM(t.estimate_min), 0),
		       COALESCE(SUM(CASE WHEN EXISTS (
		           SELECT 1 FROM tracker_task_deps d
		           WHERE d.task_id = t.id AND d.blocker_open = 1)
		         THEN 1 ELSE 0 END), 0),
		       COALESCE(SUM(CASE WHEN t.due_at IS NOT NULL AND t.due_at < ?
		         THEN 1 ELSE 0 END), 0),
		       COALESCE(SUM(CASE WHEN t.start_at IS NULL AND t.due_at IS NULL
		         THEN 1 ELSE 0 END), 0)
		FROM tracker_tasks t
		WHERE ` + where + `
		GROUP BY t.assignee
		ORDER BY 2 DESC, t.assignee
		LIMIT ?`, append(args, MaxWorkloadHandles+1)
}

func readWorkload(ctx context.Context, tx *sql.Tx, q WorkloadQuery,
	now time.Time, loc *time.Location, out *WorkloadAnswer) error {

	// The company's own midnight, resolved as [readMyWork] does, so the
	// overdue count matches what a board marks overdue.
	anchor, err := period.Resolve("today", now, loc)
	if err != nil {
		return err
	}
	dayStart := store.EncodeTime(anchor.At)

	// Through the chart ([unitKeys]): the unit came from another screen,
	// so its id and its name both have to work.
	statement, args := workloadStatement(unitKeys(q.Units, []string{q.Unit}), dayStart)
	rows, err := tx.QueryContext(ctx, statement, args...)
	if err != nil {
		return fmt.Errorf("worklet: read the company's workload: %w", err)
	}
	defer func() { _ = rows.Close() }()

	out.Rows = []WorkloadRow{}
	for rows.Next() {
		var row WorkloadRow
		if err = rows.Scan(&row.Handle, &row.Open, &row.Points,
			&row.EstimateMinutes, &row.Blocked, &row.Overdue,
			&row.Unscheduled); err != nil {
			return fmt.Errorf("worklet: scan a workload row: %w", err)
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
