package tracker

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/store"
)

// Who is carrying how much, across every project at once.
//
// # Why this is its own reader rather than a screen adding some up
//
// A caller that wanted "who is carrying the most" over the whole company had
// to fan out one grouped read per project and sum the rows itself — N round
// trips to answer one question, with the arithmetic written once per surface.
// This is one read, in one transaction, over the same rows.
//
// # It counts OPEN work
//
// Every open task assigned to somebody, whatever its dates, because "is this
// person overloaded" is a question about their whole queue. The three shapes
// of trouble ride beside the totals rather than being folded into them: a
// person whose whole queue is blocked has a different problem from one who is
// simply busy.

// WorkloadQuery asks who is carrying how much.
type WorkloadQuery struct {
	// Unit narrows to the people whose open work sits in projects owned by
	// this unit. Empty is the whole company.
	Unit string

	Level       statelog.ReadLevel
	Session     statelog.Position
	MinPosition statelog.Position
	MaxLag      time.Duration
	MaxLagSeq   uint64
}

// MaxWorkloadHandles is how many people one answer names.
//
// Two hundred and fifty-six: an order of magnitude above the seat count any
// configuration in this tree describes, and a bound at all, because the axis
// is "everybody with open work" and that set is not knowable before the read.
// A company past it is one whose workload is not a screen anyway.
const MaxWorkloadHandles = 256

// WorkloadRow is one person's load.
type WorkloadRow struct {
	Handle string `json:"handle"`

	// Open is how many open tasks they hold.
	Open int `json:"open"`

	// Points and EstimateMinutes are the two measures a company may size
	// in, both carried rather than one chosen: which one a company uses
	// differs by team, so a company-wide answer that picked one would be
	// wrong for everybody sizing in the other.
	Points          float64 `json:"points"`
	EstimateMinutes int     `json:"estimate_min"`

	// Blocked, Overdue and Unscheduled are the three shapes of "this is
	// not simply work in progress", carried beside the totals because a
	// person whose whole queue is blocked has a different problem from one
	// who is simply busy.
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
func (r *Reader) Workload(ctx context.Context, q WorkloadQuery, now time.Time) (
	WorkloadAnswer, error) {

	if q.Level == "" {
		return WorkloadAnswer{}, fmt.Errorf("tracker: this workload read " +
			"names no level — a surface resolves an absent read_level to " +
			"its own default before it reads")
	}
	var out WorkloadAnswer
	served, err := r.log.Read(ctx, statelog.Query{
		Level: q.Level,
		// THE DOMAIN, for [Reader.MyWork]'s reason: this question is
		// about every container at once, and a narrower closure would
		// certify the answer complete while a deferred record in some
		// project held work that belongs in it.
		Scope:       statelog.ScopeSet{Paths: []string{pathDomain}}.Normalised(),
		Session:     q.Session,
		MinPosition: q.MinPosition,
		MaxLag:      q.MaxLag,
		MaxLagSeq:   q.MaxLagSeq,
		Set:         true,
	}, func(tx *sql.Tx) error {
		return readWorkload(ctx, tx, q, now, &out)
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

func readWorkload(ctx context.Context, tx *sql.Tx, q WorkloadQuery,
	now time.Time, out *WorkloadAnswer) error {

	// THE DAY BOUNDARY the overdue count is against, resolved exactly as
	// [readMyWork] resolves it and for the same reason: a compound answer
	// has no caller-supplied zone, and a boundary read from the process's
	// own clock would put one node's counts a day out from another's.
	anchor, err := ResolveDate("today", now, time.UTC)
	if err != nil {
		return err
	}
	dayStart := store.EncodeTime(anchor.At)

	where := "t.removed_at IS NULL AND t.archived = 0 AND t.assignee <> '' " +
		"AND t.status_group IN ('not_started','active')"
	args := []any{dayStart}
	if q.Unit != "" {
		where += " AND EXISTS (SELECT 1 FROM tracker_projects p " +
			"WHERE p.key = t.project_key AND p.unit = ?)"
		args = append(args, q.Unit)
	}
	args = append(args, MaxWorkloadHandles+1)

	rows, err := tx.QueryContext(ctx, `
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
		WHERE `+where+`
		GROUP BY t.assignee
		ORDER BY 2 DESC, t.assignee
		LIMIT ?`, args...)
	if err != nil {
		return fmt.Errorf("tracker: read the company's workload: %w", err)
	}
	defer func() { _ = rows.Close() }()

	out.Rows = []WorkloadRow{}
	for rows.Next() {
		var row WorkloadRow
		if err = rows.Scan(&row.Handle, &row.Open, &row.Points,
			&row.EstimateMinutes, &row.Blocked, &row.Overdue,
			&row.Unscheduled); err != nil {
			return fmt.Errorf("tracker: scan a workload row: %w", err)
		}
		out.Rows = append(out.Rows, row)
	}
	if err = rows.Err(); err != nil {
		return fmt.Errorf("tracker: walk the workload rows: %w", err)
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
