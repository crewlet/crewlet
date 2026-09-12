package tracker

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"time"

	"github.com/crewlet/crewlet/internal/statelog"
)

// Reading goals, and computing the number nothing stores.
//
// # Progress is derived, on every read
//
// A goal is at what its targets say. A `tasks` target is the fraction of the
// tasks it names that are FINISHED — read from the tasks' own status group,
// which is the same value every other rule in this package is written at — and
// a numeric one is the fraction of the distance from its start to its goal. A
// binary target is one or zero.
//
// The goal's own progress is the MEAN of its targets, unweighted. Weighting
// would be a second set of numbers somebody has to maintain and nobody would,
// and an unweighted mean is the one aggregation whose wrongness is obvious
// rather than hidden: two targets, one enormous and one trivial, read as half
// done when the trivial one lands, and a person looking at the goal sees
// exactly which target it was.
//
// A goal with NO targets has no progress at all — not zero. "Nothing has
// happened" and "there is nothing to measure" are different facts, and a goal
// rendered at 0% because nobody set a target is one somebody escalates.

// GoalTargetRow is one target with what it is at.
type GoalTargetRow struct {
	GoalTarget

	// Progress is 0..1, or nil when the target measures nothing.
	Progress *float64 `json:"progress,omitempty"`

	// Finished and Total are the task counts behind a `tasks` target, so
	// a screen can render "7 of 12" rather than a bare percentage.
	Finished int `json:"finished_tasks,omitempty"`
	Total    int `json:"total_tasks,omitempty"`
}

// GoalRow is one goal as a listing renders it.
type GoalRow struct {
	ID          string     `json:"id"`
	Name        string     `json:"name"`
	Description string     `json:"description,omitempty"`
	Owners      []string   `json:"owners"`
	Members     []string   `json:"members,omitempty"`
	Group       string     `json:"group,omitempty"`
	Health      GoalHealth `json:"health,omitempty"`
	StartAt     *time.Time `json:"start_at,omitempty"`
	DueAt       *time.Time `json:"due_at,omitempty"`
	Archived    bool       `json:"archived,omitempty"`

	Targets []GoalTargetRow `json:"targets,omitempty"`

	// Updates is the health history, newest LAST as it was written.
	//
	// ON THE ROW because a `goal_updated` wake tells its owners to read
	// the updates written against the goal — and for as long as this
	// field was absent, the only verb a seat has for reading a goal
	// returned everything about it EXCEPT the part somebody wrote in
	// their own words, which is the part the wake was about.
	Updates []GoalUpdate `json:"updates,omitempty"`

	// Progress is the mean of the targets', and ABSENT when there are
	// none: a goal with nothing to measure is not a goal at zero.
	Progress *float64 `json:"progress,omitempty"`

	Version   uint64    `json:"version"`
	CreatedBy string    `json:"created_by,omitempty"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// GoalQuery asks for a company's goals.
type GoalQuery struct {
	// Owner narrows to the goals one handle owns or is a member of.
	Owner string

	// Group is the free-text label goals are filed under.
	Group string

	// Archived includes the archived ones; absent excludes them, which is
	// what a goals page means.
	Archived bool

	// ID asks for exactly one, with its targets. Empty lists.
	ID string

	Level       statelog.ReadLevel
	Session     statelog.Position
	MinPosition statelog.Position
	MaxLag      time.Duration

	// MaxLagSeq is the same bound counted in RECORDS, which is what
	// the broker actually answers — the duration above is derived
	// from it through this node's own drain rate. Both may be set
	// and the read refuses past whichever is reached first.
	MaxLagSeq uint64
}

// GoalListing is the answer, with the framework's own verdict on the read.
type GoalListing struct {
	Goals []GoalRow `json:"goals"`

	Level          statelog.ReadLevel `json:"read_level"`
	LogSeq         uint64             `json:"log_seq"`
	AppliedThrough uint64             `json:"applied_through"`
	LogLag         *uint64            `json:"log_lag,omitempty"`
	Complete       bool               `json:"complete"`
	Incomplete     *Incomplete        `json:"incomplete,omitempty"`
}

// Goals answers the company's goals, with each one's computed progress.
//
// ONE READ TRANSACTION for the goals, their targets, the targets' references
// and the status of every task those references name — so a percentage
// describes one instant. Assembled across reads it could report a target at
// 6/12 whose twelfth task had just closed, which is a number somebody quotes.
func (r *Reader) Goals(ctx context.Context, q GoalQuery) (GoalListing, error) {
	if q.Level == "" {
		return GoalListing{}, fmt.Errorf("tracker: this goal read names no " +
			"level — a surface resolves an absent read_level to its own " +
			"default before it reads")
	}

	var listing GoalListing
	served, err := r.log.Read(ctx, statelog.Query{
		Level:           q.Level,
		Scope:           goalReadScope(),
		Session:         q.Session,
		MinPosition:     q.MinPosition,
		MaxLag:          q.MaxLag,
		MaxLagPositions: q.MaxLagSeq,
		Set:             true,
	}, func(tx *sql.Tx) error {
		rows, err := readGoalRows(ctx, tx, q)
		if err != nil {
			return err
		}
		listing.Goals = rows
		position, applied, err := readCheckpoint(ctx, tx)
		if err != nil {
			return err
		}
		listing.LogSeq, listing.AppliedThrough = position, applied
		return nil
	})
	if err != nil {
		return GoalListing{}, err
	}
	listing.Level = served.Level
	listing.Complete = served.Complete
	listing.LogLag = served.Lag
	if served.Incomplete != nil {
		listing.Incomplete = incompleteFrom(served.Incomplete)
	}
	return listing, nil
}

// goalReadScope is the DOMAIN, and honestly so.
//
// A goal's targets reach tasks anywhere in the company, so a goal listing's
// answer depends on rows in every container — and a narrower closure would
// report a listing complete while a deferred record in some project was
// holding the task that moves a target.
func goalReadScope() statelog.ScopeSet {
	return statelog.ScopeSet{Paths: []string{pathDomain}}.Normalised()
}

// readGoalRows reads the goals a query names and computes their progress.
func readGoalRows(ctx context.Context, tx *sql.Tx, q GoalQuery) ([]GoalRow, error) {
	where := []string{}
	var args []any
	if q.ID != "" {
		where = append(where, "g.id = ?")
		args = append(args, q.ID)
	}
	if q.Group != "" {
		where = append(where, "g.group_label = ?")
		args = append(args, q.Group)
	}
	if !q.Archived {
		where = append(where, "g.archived = 0")
	}
	if q.Owner != "" {
		// OWNERS AND MEMBERS TOGETHER, because "my goals" means both: a
		// person contributing to an outcome wants it on their page, and
		// the `member` column is what tells a report which they are.
		where = append(where, "EXISTS (SELECT 1 FROM tracker_goal_owners o "+
			"WHERE o.goal_id = g.id AND o.handle = ?)")
		args = append(args, q.Owner)
	}
	clause := ""
	if len(where) > 0 {
		clause = " WHERE " + joinAnd(where)
	}
	rows, err := tx.QueryContext(ctx, `
		SELECT g.document, g.version FROM tracker_goals g`+clause+`
		ORDER BY g.group_label, g.due_at, g.name`, args...)
	if err != nil {
		return nil, fmt.Errorf("tracker: read the goals: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []GoalRow
	for rows.Next() {
		var document []byte
		var version uint64
		if err := rows.Scan(&document, &version); err != nil {
			return nil, fmt.Errorf("tracker: scan a goal: %w", err)
		}
		var goal Goal
		if err := json.Unmarshal(document, &goal); err != nil {
			return nil, fmt.Errorf("tracker: decode a goal: %w", err)
		}
		goal.Version = version
		out = append(out, GoalRow{
			ID: goal.ID, Name: goal.Name, Description: goal.Description,
			Owners: goal.Owners, Members: goal.Members, Group: goal.Group,
			Health: GoalHealth(goal.Health), StartAt: goal.StartAt,
			DueAt: goal.DueAt, Archived: goal.Archived, Version: version,
			CreatedBy: goal.CreatedBy, CreatedAt: goal.CreatedAt,
			UpdatedAt: goal.UpdatedAt, Updates: goal.Updates,
			Targets: make([]GoalTargetRow, 0, len(goal.Targets)),
		})
		row := &out[len(out)-1]
		for _, target := range goal.Targets {
			row.Targets = append(row.Targets, GoalTargetRow{GoalTarget: target})
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("tracker: read the goals: %w", err)
	}
	for i := range out {
		if err := scoreGoal(ctx, tx, &out[i]); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// scoreGoal fills in what every target is at, and the goal's own mean.
func scoreGoal(ctx context.Context, tx *sql.Tx, goal *GoalRow) error {
	var sum float64
	measured := 0
	for i := range goal.Targets {
		target := &goal.Targets[i]
		progress, err := scoreTarget(ctx, tx, goal.ID, target)
		if err != nil {
			return err
		}
		if progress == nil {
			continue
		}
		target.Progress = progress
		sum += *progress
		measured++
	}
	if measured == 0 {
		// NO PROGRESS AT ALL, not zero — see the file head.
		return nil
	}
	mean := sum / float64(measured)
	goal.Progress = &mean
	return nil
}

// scoreTarget is one target's arithmetic, and it is four of them.
func scoreTarget(ctx context.Context, tx *sql.Tx, goalID string,
	target *GoalTargetRow) (*float64, error) {

	switch target.Type {
	case TargetBinary:
		return ratio(boolFloat(target.Done), 1), nil
	case TargetNumber, TargetPercent:
		span := target.Goal - target.Start
		if span == 0 {
			// A TARGET THAT CANNOT MOVE MEASURES NOTHING. The write
			// refuses this shape for `number`, so what reaches here is
			// a `percent` target left at its defaults or a row an
			// older build wrote.
			return nil, nil
		}
		return ratio(target.Current-target.Start, span), nil
	case TargetTasks:
		finished, total, err := targetTasks(ctx, tx, goalID, target.ID)
		if err != nil {
			return nil, err
		}
		target.Finished, target.Total = finished, total
		if total == 0 {
			// A TARGET WHOSE TASKS ARE ALL GONE, which a purge or a
			// project that was never created both produce. Reporting
			// it at 100% would be a goal that completed itself.
			return nil, nil
		}
		return ratio(float64(finished), float64(total)), nil
	}
	return nil, nil
}

// targetTasks counts the tasks one target reaches, and how many are finished.
//
// ONE STATEMENT over both reference kinds, because a target names tasks AND
// projects and a task reached both ways must be counted once — which a UNION
// of two counts could not do.
//
// A REMOVED TASK IS NOT COUNTED, on either side: a removal hides work, and
// counting hidden work as unfinished would make a goal go backwards when
// somebody tidied up.
func targetTasks(ctx context.Context, tx *sql.Tx, goalID, targetID string) (
	finished, total int, err error) {

	// DELIVERED, NOT FINISHED, and the difference is the whole measurement:
	// `cancelled` is a FINISHED group and is not delivery, so counting the
	// finished groups scored a team that cancelled its remaining work at a
	// hundred per cent. [Delivered]'s own doc names a goal's task targets
	// as one of its readers — this is that reader, and it is the clause
	// rather than a second spelling of it.
	// THE ORDER IS THE STATEMENT'S, and the delivery clause's arguments
	// come first because its placeholders are in the SELECT rather than
	// the WHERE.
	delivered, deliveredArgs := deliveredClause("t.status_group", "t.status")
	args := append(append([]any{}, deliveredArgs...),
		goalID, targetID, goalID, targetID)
	err = tx.QueryRowContext(ctx, `
		SELECT
			COUNT(*),
			COALESCE(SUM(CASE WHEN `+delivered+` THEN 1 ELSE 0 END), 0)
		FROM tracker_tasks t
		WHERE t.removed_at IS NULL AND (
			EXISTS (SELECT 1 FROM tracker_goal_target_refs r
				WHERE r.goal_id = ? AND r.target_id = ?
				  AND r.kind = 'task' AND r.ref = t.id)
			OR EXISTS (SELECT 1 FROM tracker_goal_target_refs r
				WHERE r.goal_id = ? AND r.target_id = ?
				  AND r.kind = 'project' AND r.ref = t.project_key))`,
		args...).Scan(&total, &finished)
	if err != nil {
		return 0, 0, fmt.Errorf("tracker: count target %s of goal %s: %w",
			targetID, goalID, err)
	}
	return finished, total, nil
}

// ratio clamps a fraction into 0..1.
//
// CLAMPED rather than reported raw, because a numeric target routinely
// overshoots — somebody sets a goal of 100 and reaches 130 — and a progress
// bar at 130% is a rendering bug in every screen that draws one. The number
// behind it is on the target itself, so nothing is lost.
func ratio(part, whole float64) *float64 {
	if whole == 0 {
		return nil
	}
	value := part / whole
	switch {
	case value < 0:
		value = 0
	case value > 1:
		value = 1
	}
	return &value
}

func boolFloat(v bool) float64 {
	if v {
		return 1
	}
	return 0
}

func joinAnd(clauses []string) string {
	out := clauses[0]
	for _, clause := range clauses[1:] {
		out += " AND " + clause
	}
	return out
}

// finishedGroups is the status groups where work has stopped.
func finishedGroups() []any {
	out := make([]any, 0, len(StatusGroups))
	for _, group := range StatusGroups {
		if group.Finished() {
			out = append(out, string(group))
		}
	}
	return out
}

// deliveredClause is [Delivered] as SQL, over a row's two status columns.
//
// ONE SPELLING, because the Go predicate and a hand-written SQL copy are two
// answers to "did this land" and the copy is the one that stops matching — it
// already had: a goal's task targets counted `status_group IN (finished)` and
// scored a CANCELLED task as delivered, so a team that cancelled its remaining
// work drove its goal to a hundred per cent.
//
// The groups are DERIVED rather than typed for the same reason [Delivered] is
// derived: a group added to [StatusGroups] must reach every reader, and a
// literal list is the reader it would not reach.
func deliveredClause(statusGroup, status string) (string, []any) {
	groups := finishedGroups()
	args := append(append([]any{}, groups...), string(StatusCancelled))
	return statusGroup + " IN (" + placeholders(len(groups)) + ") AND " +
		status + " <> ?", args
}
