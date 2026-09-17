package tracker

import (
	"context"
	"database/sql"
	"fmt"
	"sort"
	"time"

	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/store"
)

// Who is carrying how much, across every project at once.
//
// # Why this is its own reader rather than a screen adding some up
//
// The two halves of the question live in different places and neither is
// reachable from the other. What somebody HOLDS is a group-by over
// `tracker_tasks`, which the ordinary query grammar can already answer. What
// somebody CAN hold is a project's sprint policy, and that is per project:
// `Reader.Sprints` answers one project at a time, deliberately, because a
// sprint is numbered per project and a report is about one of them.
//
// So a caller that wanted "is anybody over-committed" had to fan out one read
// per project and sum the capacities itself — N round trips to answer one
// question, with the arithmetic written once per surface and the chance to get
// the three-valued capacity wrong every time. This is one read, in one
// transaction, over the same rows.
//
// # The capacity is a SUM, and it is three-valued
//
// A person working across two projects has a capacity in each, and their
// capacity is the total — that is what the number means. Summing them here
// rather than picking one is the only reading that does not silently answer
// about part of somebody's week.
//
// But an UNDECLARED capacity is not a capacity of zero. A policy that names
// nobody would otherwise put every single person permanently over, which is
// the one failure that makes the whole screen useless — so [WorkloadRow] has a
// POINTER and a surface renders its absence as an em dash. A person with a
// capacity in one project and none in another has the one they have: the
// absence contributes nothing rather than poisoning the sum, and
// `CapacityFrom` says how many projects the number came from, so "5 points,
// from one of the two projects they work in" is a fact a reader can see rather
// than a total they would have read as covering both.
//
// # It counts OPEN work, not a sprint's commitment
//
// `Reader.Sprints` answers what somebody took on inside one sprint's window.
// This answers what they are holding NOW — every open task assigned to them,
// in or out of a sprint — because "is this person overloaded" is a question
// about their whole queue and a sprint is only part of it. The two numbers are
// deliberately different and both are worth having.

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
	// in, both carried rather than one chosen: which one a company uses is
	// `sprint_measure` per PROJECT, so a company-wide answer that picked
	// one would be wrong for every project running the other.
	Points          float64 `json:"points"`
	EstimateMinutes int     `json:"estimate_min"`

	// Blocked, Overdue and Unscheduled are the three shapes of "this is
	// not simply work in progress", carried beside the totals because a
	// person at capacity whose whole queue is blocked has a different
	// problem from one who is simply busy.
	Blocked     int `json:"blocked"`
	Overdue     int `json:"overdue"`
	Unscheduled int `json:"unscheduled"`

	// Capacity is the sum of what every ACTIVE sprint policy says this
	// person can take, in that project's own measure — and is ABSENT when
	// no policy names them. An unset capacity is not a capacity of zero,
	// which would render everybody permanently over.
	Capacity *float64 `json:"capacity,omitempty"`

	// CapacityFrom is how many projects contributed to it, so a reader can
	// tell a whole-week number from part of one.
	CapacityFrom int `json:"capacity_from,omitempty"`

	// CapacityMeasure is what the capacity counts. It is absent with the
	// capacity, and it is a single value because a person whose projects
	// size in DIFFERENT measures has no summable capacity at all —
	// [Workload] leaves that person's capacity unset rather than adding
	// points to minutes.
	CapacityMeasure SprintMeasure `json:"capacity_measure,omitempty"`

	// OverCapacity is set only when a capacity was declared.
	OverCapacity bool `json:"over_capacity,omitempty"`

	// MixedMeasures marks the person whose capacity could not be summed
	// because their projects size in different units. It is the reason the
	// capacity is absent, said rather than left to look like nobody
	// declared one — those are different facts and only one of them is
	// somebody's to fix.
	MixedMeasures bool `json:"mixed_measures,omitempty"`
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

	declared, err := activeCapacities(ctx, tx, q.Unit)
	if err != nil {
		return err
	}
	for i := range out.Rows {
		applyCapacity(&out.Rows[i], declared[out.Rows[i].Handle])
	}

	position, applied, err := readCheckpoint(ctx, tx)
	if err != nil {
		return err
	}
	out.LogSeq, out.AppliedThrough = position, applied
	return nil
}

// declaredCapacity is one project's capacity for one person, with the measure
// it is counted in.
//
// THE MEASURE TRAVELS WITH THE NUMBER because it is a property of the PROJECT
// rather than of the company: two projects may size in different units, and a
// sum that dropped the measure would add points to minutes and answer a number
// that means nothing.
type declaredCapacity struct {
	value   float64
	measure SprintMeasure
}

// activeCapacities is what every project with a RUNNING sprint says its people
// can take, keyed on handle.
//
// A SPRINT MUST BE RUNNING. A capacity is a statement about a fortnight, so a
// project between sprints declares nothing this answer can use — and counting
// a dormant project's policy would hold somebody to a number nobody is
// currently working to.
func activeCapacities(ctx context.Context, tx *sql.Tx, unit string) (
	map[string][]declaredCapacity, error) {

	where := "sprint_policy_json IS NOT NULL AND active_sprint IS NOT NULL " +
		"AND archived = 0"
	var args []any
	if unit != "" {
		where += " AND unit = ?"
		args = append(args, unit)
	}
	rows, err := tx.QueryContext(ctx,
		`SELECT key FROM tracker_projects WHERE `+where+` ORDER BY key`, args...)
	if err != nil {
		return nil, fmt.Errorf("tracker: read the projects running a sprint: %w", err)
	}
	keys := []string{}
	for rows.Next() {
		var key string
		if err := rows.Scan(&key); err != nil {
			_ = rows.Close()
			return nil, fmt.Errorf("tracker: scan a sprinting project: %w", err)
		}
		keys = append(keys, key)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return nil, fmt.Errorf("tracker: walk the sprinting projects: %w", err)
	}
	_ = rows.Close()

	out := map[string][]declaredCapacity{}
	for _, key := range keys {
		// THE DOCUMENT, not the column: `sprint_policy_json` is the
		// applier's own copy for the duty's selection, and the policy a
		// capacity is read from is the project's — one decoder, in the
		// place every other reader of a policy already uses.
		project, held, err := readProject(ctx, tx, key)
		if err != nil {
			return nil, err
		}
		if !held || project.Sprints == nil {
			continue
		}
		measure := project.Sprints.Measure.Or()
		for handle, value := range sprintCapacities(project, measure) {
			out[handle] = append(out[handle],
				declaredCapacity{value: value, measure: measure})
		}
	}
	for handle := range out {
		sort.Slice(out[handle], func(i, j int) bool {
			return out[handle][i].value < out[handle][j].value
		})
	}
	return out, nil
}

// applyCapacity sums one person's declarations onto their row.
//
// THREE OUTCOMES, not two. Nobody declared one, which leaves the pointer nil
// and says nothing about whether this person is overloaded. Somebody declared
// one in a single measure, which sums. Or their projects size in DIFFERENT
// measures, which does not sum at all and is marked as its own fact —
// `MixedMeasures` rather than an absent capacity, because "nobody said" and
// "it cannot be added up" send a reader to two different places.
func applyCapacity(row *WorkloadRow, declared []declaredCapacity) {
	if len(declared) == 0 {
		return
	}
	measure := declared[0].measure
	total := 0.0
	for _, one := range declared {
		if one.measure != measure {
			row.MixedMeasures = true
			return
		}
		total += one.value
	}
	row.Capacity = &total
	row.CapacityFrom = len(declared)
	row.CapacityMeasure = measure
	// AGAINST WHAT THEY HOLD IN THAT MEASURE, which is the only comparison
	// that means anything: a capacity in points compared against a sum of
	// minutes is two numbers about different things.
	held := row.Points
	if measure == MeasureEstimate {
		held = float64(row.EstimateMinutes)
	}
	row.OverCapacity = held > total
}
