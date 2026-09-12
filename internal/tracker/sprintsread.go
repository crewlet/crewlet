package tracker

import (
	"context"
	"database/sql"
	"fmt"
	"math"
	"sort"
	"time"

	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/store"
)

// Reading a project's sprints, and the arithmetic every sprint figure is.
//
// # Every figure is derived from the STAYS, and none of them is stored
//
// `tracker_task_sprints` holds one row per STAY — a task's membership of one
// sprint, opened when it entered and closed when it left. Committed, added,
// removed, remaining and open-after-close are all predicates over that one
// table's two instants, which is what makes a sprint's history exact at any
// age: nothing is snapshotted at the close, so a report run a year later reads
// the same rows and produces the same numbers.
//
// A stored counter could not do it. "Committed" is a statement about what the
// sprint held at its START, and a counter maintained forward is a statement
// about what it holds NOW — the two differ for every task that arrived late,
// which is precisely the number a team wants.
//
// # Done is a SPAN, not a stay
//
// Delivery is not membership: a task can sit in a sprint all the way through
// and never be finished, and it can be finished in a sprint it joined an hour
// before the close. So `done` reads `tracker_status_spans` — the rows a task
// ENTERED a status at — and counts the ones whose own status is delivered
// (§3.7's [Delivered]: a finished group that is not `cancelled`) and whose
// entry falls inside the window.
//
// Both endpoints are the EFFECTIVE instant. The applier writes spans from
// `tracker_history.effective_at` and the stays from the record's own effective
// instant, so a writer whose clock is a minute out cannot put a completion in
// the wrong sprint — which an authored instant would.
//
// # The window ends where the sprint did
//
// A closed sprint's window is `start_at … closed_at`; a sprint still running
// ends at its planned `end_at`. NEITHER READS A CLOCK, so two nodes asked the
// same question answer the same numbers — the one value that genuinely needs
// `now` is `days_remaining`, which the caller supplies.

// SprintFigures is one sprint's arithmetic in the project's own measure.
type SprintFigures struct {
	// Measure names which column every number here is a sum of, because a
	// bare "42" is points to one team and minutes to another.
	Measure SprintMeasure `json:"measure"`

	// Committed is the stays open at the sprint's start — what the team
	// took on, not what it ended up holding.
	Committed float64 `json:"committed"`

	// Added is the stays opened inside the window: work that arrived after
	// the commitment was made.
	Added float64 `json:"added"`

	// Removed is the stays closed inside the window WITHOUT a rollover —
	// work pulled out, as against work carried forward.
	Removed float64 `json:"removed"`

	// Done is the delivered spans entered inside the window. See the file
	// head: it is a span rather than a stay.
	Done float64 `json:"done"`

	// Remaining is the stays still open at the window's end whose task is
	// not delivered.
	Remaining float64 `json:"remaining"`

	// OpenAfterClose is what left the sprint unfinished — stays rolled to
	// another sprint, plus the stays still open on a closed sprint whose
	// spillover nobody has settled.
	OpenAfterClose float64 `json:"open_after_close"`

	// Tasks is how many stays the sprint held in total, so a screen can
	// render "12 tasks · 34 points" without a second question.
	Tasks int `json:"tasks"`

	// Unestimated is how many of them carry NO value in the measure. It is
	// the honesty column: a sprint reporting 8 of 34 points done where
	// half its tasks are unestimated is reporting on half a sprint.
	Unestimated int `json:"unestimated"`
}

// AssigneeFigures is one person's share of a sprint.
type AssigneeFigures struct {
	Handle    string  `json:"handle"`
	Committed float64 `json:"committed"`
	Done      float64 `json:"done"`
	Remaining float64 `json:"remaining"`

	// Total is every stay this person holds in the sprint, whenever it
	// opened and whether or not it has closed — which is the quantity a
	// capacity is about. Committed alone would let somebody take twice
	// their capacity mid-sprint and still read as clear.
	Total float64 `json:"total"`

	Tasks int `json:"tasks"`

	// Capacity is what the project's policy says this person can take in
	// the sprint's measure, and is ABSENT when the policy names nobody —
	// an unset capacity is not a capacity of zero, which would render
	// every assignee permanently over.
	Capacity *float64 `json:"capacity,omitempty"`

	// OverCapacity is set only when a capacity was declared.
	OverCapacity bool `json:"over_capacity,omitempty"`
}

// SprintRow is one sprint as a report renders it.
type SprintRow struct {
	Project string      `json:"project"`
	Number  int         `json:"number"`
	Name    string      `json:"name"`
	Goal    string      `json:"goal,omitempty"`
	State   SprintState `json:"state"`

	StartAt  time.Time  `json:"start_at"`
	EndAt    time.Time  `json:"end_at"`
	ClosedAt *time.Time `json:"closed_at,omitempty"`
	ClosedBy string     `json:"closed_by,omitempty"`

	// DaysRemaining is whole days from the caller's own instant to the
	// end, and is ABSENT on anything but an active sprint — a future
	// sprint has not started and a closed one has no remainder.
	DaysRemaining *int `json:"days_remaining,omitempty"`

	Figures    SprintFigures     `json:"figures"`
	ByAssignee []AssigneeFigures `json:"by_assignee,omitempty"`

	// RolloverPending is a CLOSED sprint whose spillover nobody has
	// decided — the state `AutoRoll: false` leaves behind, and the one
	// thing on this answer a lead has to act on.
	RolloverPending bool `json:"rollover_pending,omitempty"`
	RolloverTo      *int `json:"rollover_to,omitempty"`

	Archived bool   `json:"archived,omitempty"`
	Version  uint64 `json:"version"`
}

// SprintListing is the answer.
type SprintListing struct {
	Project string      `json:"project"`
	Sprints []SprintRow `json:"sprints"`

	// VelocityAvg is the mean `done` over the CLOSED sprints in this
	// answer, and is absent when there are none: a team that has not
	// finished a sprint has no velocity, and rendering zero would read as
	// a team that delivers nothing.
	VelocityAvg *float64 `json:"velocity_avg,omitempty"`

	// EarlierSprintsDropped is how many closed sprints exist below the
	// window this answer covers. NAMED rather than silently truncated, for
	// the reason every bound in this package is named.
	EarlierSprintsDropped int `json:"earlier_sprints_dropped,omitempty"`

	Measure SprintMeasure `json:"measure"`

	Level          statelog.ReadLevel `json:"read_level"`
	LogSeq         uint64             `json:"log_seq"`
	AppliedThrough uint64             `json:"applied_through"`
	LogLag         *uint64            `json:"log_lag,omitempty"`
	Complete       bool               `json:"complete"`
	Incomplete     *Incomplete        `json:"incomplete,omitempty"`
}

// SprintWindowDefault is how many sprints a report covers when nobody says.
//
// FIVE, because that is the span a velocity average is worth taking over: two
// sprints is noise and twenty spans a quarter in which the team, the scope and
// the estimate scale have all changed. It is also what `describe_project`
// carries inline, so the two surfaces agree by construction.
const SprintWindowDefault = 5

// SprintWindowMin and SprintWindowMax bound what a caller may ask for.
//
// The floor is 3 because a mean of two is a midpoint; the ceiling is 10
// because each sprint costs two indexed scans of its stays and one of its
// spans, and ten is where `describe_project`'s own 200 KiB answer ceiling
// starts to bind before the query does.
const (
	SprintWindowMin = 3
	SprintWindowMax = 10
)

// MaxSprintAssignees is how many people one sprint's breakdown names.
//
// SIXTY-FOUR, the same bound §7 puts on every enumerated list in an answer a
// model reads. A sprint with more assignees than that is not a sprint, and the
// rows are ordered by committed descending so the ones dropped are the ones
// carrying least.
const MaxSprintAssignees = 64

// SprintQuery asks for a project's sprints.
type SprintQuery struct {
	// Project is required: sprints are a project's, and a company-wide
	// sprint report would be summing two teams' unrelated cadences.
	Project string

	// Number asks for exactly one sprint. Zero lists the window.
	Number int

	// Sprints is how many to cover, clamped into [SprintWindowMin,
	// SprintWindowMax]; zero takes [SprintWindowDefault].
	Sprints int

	// Archived includes the archived sprint records.
	Archived bool

	Level       statelog.ReadLevel
	Session     statelog.Position
	MinPosition statelog.Position
	MaxLag      time.Duration
}

// Sprints answers a project's sprints with every figure computed.
//
// ONE READ TRANSACTION for the project, its sprint records, their stays and
// the spans behind `done` — so committed and done describe one instant.
// Assembled across reads a report could show a sprint at 12 committed and 13
// done, which is a number somebody screenshots.
func (r *Reader) Sprints(ctx context.Context, q SprintQuery, now time.Time) (
	SprintListing, error) {

	if q.Level == "" {
		return SprintListing{}, fmt.Errorf("tracker: this sprint read names " +
			"no level — a surface resolves an absent read_level to its own " +
			"default before it reads")
	}
	project := ProjectKey(q.Project)
	if project == "" {
		return SprintListing{}, fmt.Errorf("tracker: a sprint report names a " +
			"project — sprints belong to one, and a company-wide report would " +
			"sum two teams' unrelated cadences")
	}
	if q.Sprints != 0 && (q.Sprints < SprintWindowMin || q.Sprints > SprintWindowMax) {
		return SprintListing{}, fmt.Errorf("tracker: sprints=%d is outside "+
			"%d..%d — ask for a window inside that range, or omit it for %d",
			q.Sprints, SprintWindowMin, SprintWindowMax, SprintWindowDefault)
	}
	window := q.Sprints
	if window == 0 {
		window = SprintWindowDefault
	}

	listing := SprintListing{Project: project}
	served, err := r.log.Read(ctx, statelog.Query{
		Level:       q.Level,
		Scope:       sprintReadScope(project),
		Session:     q.Session,
		MinPosition: q.MinPosition,
		MaxLag:      q.MaxLag,
		Set:         true,
	}, func(tx *sql.Tx) error {
		p, found, err := readProject(ctx, tx, project)
		if err != nil {
			return err
		}
		if !found {
			return fmt.Errorf("tracker: no project %q — %w", project, ErrNoProject)
		}
		listing.Measure = measureOf(p)
		rows, dropped, err := readSprintRows(ctx, tx, p, q, window, now)
		if err != nil {
			return err
		}
		listing.Sprints, listing.EarlierSprintsDropped = rows, dropped
		listing.VelocityAvg = velocityOf(rows)
		position, applied, err := readCheckpoint(ctx, tx)
		if err != nil {
			return err
		}
		listing.LogSeq, listing.AppliedThrough = position, applied
		return nil
	})
	if err != nil {
		return SprintListing{}, err
	}
	listing.Level = served.Level
	listing.Complete = served.Complete
	listing.LogLag = served.Lag
	if served.Incomplete != nil {
		listing.Incomplete = incompleteFrom(served.Incomplete)
	}
	return listing, nil
}

// sprintReadScope is the project's CONTAINER.
//
// The container rather than the sprint objects, because every figure here is
// an aggregate over the project's TASK rows — their stays and their spans —
// and a closure naming only the sprint records would certify a report complete
// while a deferred task record in the project was holding the row that moves
// `done`. Enumerating the tasks is not tractable, which is exactly the case
// §5's rule reserves a container term for.
func sprintReadScope(project string) statelog.ScopeSet {
	return statelog.ScopeSet{Paths: []string{
		ScopeTerm{Kind: TermContainer, ID: project}.Path(),
	}}.Normalised()
}

// readSprintRows reads the sprints in the window and scores each one.
func readSprintRows(ctx context.Context, tx *sql.Tx, p Project, q SprintQuery,
	window int, now time.Time) ([]SprintRow, int, error) {

	where := []string{"s.project_key = ?"}
	args := []any{p.Key}
	if q.Number != 0 {
		where = append(where, "s.number = ?")
		args = append(args, q.Number)
	}
	if !q.Archived {
		where = append(where, "s.archived = 0")
	}
	// NEWEST FIRST and then reversed, because the window is "the last N"
	// and a LIMIT can only take a prefix — so the prefix has to be the end.
	args = append(args, window)
	rows, err := tx.QueryContext(ctx, `
		SELECT s.number, s.name, s.goal, s.start_at, s.end_at, s.state,
		       s.closed_at, s.closed_by, s.rollover_to, s.rollover_done,
		       s.archived, s.version
		FROM tracker_sprints s
		WHERE `+joinAnd(where)+`
		ORDER BY s.number DESC
		LIMIT ?`, args...)
	if err != nil {
		return nil, 0, fmt.Errorf("tracker: read the sprints of %s: %w", p.Key, err)
	}
	defer func() { _ = rows.Close() }()

	var out []SprintRow
	for rows.Next() {
		var row SprintRow
		var start, end int64
		var closedAt sql.NullInt64
		var closedBy sql.NullString
		var rolloverTo sql.NullInt64
		var rolloverDone, archived int
		var state string
		if err := rows.Scan(&row.Number, &row.Name, &row.Goal, &start, &end,
			&state, &closedAt, &closedBy, &rolloverTo, &rolloverDone,
			&archived, &row.Version); err != nil {
			return nil, 0, fmt.Errorf("tracker: scan a sprint of %s: %w", p.Key, err)
		}
		row.Project = p.Key
		row.State = SprintState(state)
		row.StartAt = store.DecodeTime(start)
		row.EndAt = store.DecodeTime(end)
		if closedAt.Valid {
			at := store.DecodeTime(closedAt.Int64)
			row.ClosedAt = &at
		}
		row.ClosedBy = closedBy.String
		if rolloverTo.Valid {
			to := int(rolloverTo.Int64)
			row.RolloverTo = &to
		}
		// PENDING IS AN ABSENT POINTER ON A CLOSED SPRINT — the state
		// `AutoRoll: false` leaves behind. `rollover_done` says the
		// walk finished; a nil pointer says nobody has decided yet, and
		// the two are different facts.
		row.RolloverPending = row.State == SprintClosed && !rolloverTo.Valid
		row.Archived = archived != 0
		out = append(out, row)
	}
	if err := rows.Err(); err != nil {
		return nil, 0, fmt.Errorf("tracker: read the sprints of %s: %w", p.Key, err)
	}
	// Back into sprint order, oldest first, which is how every burndown,
	// velocity chart and sprint list reads.
	sort.Slice(out, func(i, j int) bool { return out[i].Number < out[j].Number })

	measure := measureOf(p)
	for i := range out {
		if err := scoreSprint(ctx, tx, p, &out[i], measure, now); err != nil {
			return nil, 0, err
		}
	}
	dropped, err := earlierSprints(ctx, tx, p.Key, q, out)
	if err != nil {
		return nil, 0, err
	}
	return out, dropped, nil
}

// earlierSprints counts the sprints below the window this answer covers.
func earlierSprints(ctx context.Context, tx *sql.Tx, project string,
	q SprintQuery, covered []SprintRow) (int, error) {

	if q.Number != 0 || len(covered) == 0 {
		// A single-sprint report drops nothing, and an empty window has
		// nothing below it.
		return 0, nil
	}
	where := []string{"s.project_key = ?", "s.number < ?"}
	args := []any{project, covered[0].Number}
	if !q.Archived {
		where = append(where, "s.archived = 0")
	}
	var n int
	if err := tx.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM tracker_sprints s WHERE `+joinAnd(where),
		args...).Scan(&n); err != nil {
		return 0, fmt.Errorf("tracker: count the earlier sprints of %s: %w",
			project, err)
	}
	return n, nil
}

// scoreSprint computes one sprint's figures and its per-assignee breakdown.
func scoreSprint(ctx context.Context, tx *sql.Tx, p Project, row *SprintRow,
	measure SprintMeasure, now time.Time) error {

	row.Figures.Measure = measure
	start := store.EncodeTime(row.StartAt)
	end := store.EncodeTime(sprintWindowEnd(*row))

	if row.State == SprintActive {
		days := int(math.Ceil(row.EndAt.Sub(now).Hours() / 24))
		if days < 0 {
			// A SPRINT PAST ITS END BUT NOT YET CLOSED — the duty has
			// not run. Zero rather than a negative, because "-2 days
			// remaining" is a rendering nobody wants and the state
			// already says the sprint is running late.
			days = 0
		}
		row.DaysRemaining = &days
	}

	column := measure.Column()
	delivered, deliveredArgs := deliveredClause("t.status_group", "t.status")

	// ONE STATEMENT over the stays, because every figure but `done` is a
	// predicate over the same two instants and five separate counts would
	// be five scans of one index range.
	//
	// A REMOVED TASK IS NOT COUNTED. A removal hides work, and counting
	// hidden work as committed-but-unfinished would make a sprint go
	// backwards when somebody tidied up — the same rule a goal's task
	// targets take.
	args := []any{
		start, start, // committed
		start, end, // added
		start, end, // removed
	}
	args = append(args, deliveredArgs...) // remaining
	args = append(args, p.Key, row.Number)
	err := tx.QueryRowContext(ctx, `
		SELECT
			COALESCE(SUM(CASE WHEN m.from_at <= ?
			                   AND (m.to_at IS NULL OR m.to_at > ?)
			                  THEN `+column+` ELSE 0 END), 0),
			COALESCE(SUM(CASE WHEN m.from_at > ? AND m.from_at <= ?
			                  THEN `+column+` ELSE 0 END), 0),
			COALESCE(SUM(CASE WHEN m.to_at IS NOT NULL AND m.to_at > ?
			                   AND m.to_at <= ? AND m.rolled_to IS NULL
			                  THEN `+column+` ELSE 0 END), 0),
			COALESCE(SUM(CASE WHEN m.to_at IS NULL AND NOT (`+delivered+`)
			                  THEN `+column+` ELSE 0 END), 0),
			COALESCE(SUM(CASE WHEN m.rolled_to IS NOT NULL THEN `+column+`
			                  ELSE 0 END), 0),
			COUNT(*),
			COALESCE(SUM(CASE WHEN `+column+` = 0 THEN 1 ELSE 0 END), 0)
		FROM tracker_task_sprints m
		JOIN tracker_tasks t ON t.id = m.task_id
		WHERE m.project_key = ? AND m.sprint = ? AND t.removed_at IS NULL`,
		args...).Scan(&row.Figures.Committed, &row.Figures.Added,
		&row.Figures.Removed, &row.Figures.Remaining,
		&row.Figures.OpenAfterClose, &row.Figures.Tasks,
		&row.Figures.Unestimated)
	if err != nil {
		return fmt.Errorf("tracker: score sprint %d of %s: %w",
			row.Number, p.Key, err)
	}
	// AN UNSETTLED SPILLOVER'S OPEN STAYS ARE ALSO OPEN AFTER THE CLOSE.
	// They carry no `rolled_to` because nobody has decided where they go,
	// and leaving them out would report a closed sprint whose remaining
	// work vanished.
	if row.RolloverPending {
		row.Figures.OpenAfterClose += row.Figures.Remaining
	}

	if err := scoreSprintDone(ctx, tx, p, row, column); err != nil {
		return err
	}
	return scoreSprintAssignees(ctx, tx, p, row, column, start, end)
}

// deliveredInWindow is "this task was delivered inside this sprint's window".
//
// ONE SPELLING, shared by the sprint total and by every assignee's share,
// because the two are rendered beside each other and a screen whose
// `by_assignee` does not sum to `done` is a screen somebody files a bug
// against. It is a SPAN predicate rather than a status one — see the file
// head — and it is an EXISTS rather than a join so a task that bounced
// between two delivered statuses counts once.
func deliveredInWindow(project string, number int, from, to int64) (string, []any) {
	delivered, args := deliveredClause("s.grp", "s.status")
	return `EXISTS (SELECT 1 FROM tracker_status_spans s
			WHERE s.task_id = t.id AND ` + delivered + `
			  AND s.project_key = ? AND s.sprint_number = ?
			  AND s.entered_at >= ? AND s.entered_at <= ?)`,
		append(args, project, number, from, to)
}

// scoreSprintDone sums the delivered tasks of the sprint's window.
func scoreSprintDone(ctx context.Context, tx *sql.Tx, p Project,
	row *SprintRow, column string) error {

	done, args := deliveredInWindow(p.Key, row.Number,
		store.EncodeTime(row.StartAt), store.EncodeTime(sprintWindowEnd(*row)))
	args = append(args, p.Key, row.Number)
	err := tx.QueryRowContext(ctx, `
		SELECT COALESCE(SUM(`+column+`), 0)
		FROM tracker_tasks t
		WHERE t.removed_at IS NULL AND `+done+`
		  AND EXISTS (SELECT 1 FROM tracker_task_sprints m
			WHERE m.task_id = t.id AND m.project_key = ? AND m.sprint = ?)`,
		args...).Scan(&row.Figures.Done)
	if err != nil {
		return fmt.Errorf("tracker: score the delivery of sprint %d of %s: %w",
			row.Number, p.Key, err)
	}
	return nil
}

// scoreSprintAssignees breaks the sprint down by who holds the work.
//
// THE SAME FOUR PREDICATES the sprint's own figures use, so the breakdown sums
// to the total. It is one GROUP BY rather than a query per person because a
// sprint's assignees are not knowable before the read.
func scoreSprintAssignees(ctx context.Context, tx *sql.Tx, p Project,
	row *SprintRow, column string, start, end int64) error {

	done, doneArgs := deliveredInWindow(p.Key, row.Number, start, end)
	notDelivered, notDeliveredArgs := deliveredClause("t.status_group", "t.status")

	args := []any{start, start}
	args = append(args, doneArgs...)
	args = append(args, notDeliveredArgs...)
	args = append(args, p.Key, row.Number, MaxSprintAssignees)
	rows, err := tx.QueryContext(ctx, `
		SELECT t.assignee,
			COALESCE(SUM(CASE WHEN m.from_at <= ?
			                   AND (m.to_at IS NULL OR m.to_at > ?)
			                  THEN `+column+` ELSE 0 END), 0),
			COALESCE(SUM(CASE WHEN `+done+` THEN `+column+` ELSE 0 END), 0),
			COALESCE(SUM(CASE WHEN m.to_at IS NULL AND NOT (`+notDelivered+`)
			                  THEN `+column+` ELSE 0 END), 0),
			COALESCE(SUM(`+column+`), 0),
			COUNT(*)
		FROM tracker_task_sprints m
		JOIN tracker_tasks t ON t.id = m.task_id
		WHERE m.project_key = ? AND m.sprint = ? AND t.removed_at IS NULL
		  AND t.assignee <> ''
		GROUP BY t.assignee
		ORDER BY 5 DESC, t.assignee
		LIMIT ?`, args...)
	if err != nil {
		return fmt.Errorf("tracker: break sprint %d of %s down by assignee: %w",
			row.Number, p.Key, err)
	}
	defer func() { _ = rows.Close() }()

	capacities := sprintCapacities(p, row.Figures.Measure)
	for rows.Next() {
		var a AssigneeFigures
		if err := rows.Scan(&a.Handle, &a.Committed, &a.Done, &a.Remaining,
			&a.Total, &a.Tasks); err != nil {
			return fmt.Errorf("tracker: scan an assignee of sprint %d of %s: %w",
				row.Number, p.Key, err)
		}
		if capacity, declared := capacities[a.Handle]; declared {
			a.Capacity = &capacity
			// AGAINST WHAT THEY HOLD, not against what they took:
			// somebody who finished half their sprint and picked up
			// twice as much again is over capacity now, which is the
			// moment the number is worth knowing.
			a.OverCapacity = a.Total > capacity
		}
		row.ByAssignee = append(row.ByAssignee, a)
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("tracker: break sprint %d of %s down by assignee: %w",
			row.Number, p.Key, err)
	}
	return nil
}

// sprintCapacities is the project policy's capacities in the sprint's measure.
//
// An empty map means the policy declares none, which is what makes an absent
// `capacity` on a row an absence rather than a zero.
func sprintCapacities(p Project, measure SprintMeasure) map[string]float64 {
	if p.Sprints == nil || len(p.Sprints.Capacity) == 0 {
		return nil
	}
	out := make(map[string]float64, len(p.Sprints.Capacity))
	for handle, c := range p.Sprints.Capacity {
		if measure.Or() == MeasureEstimate {
			if c.EstimateMin == 0 {
				continue
			}
			out[handle] = float64(c.EstimateMin)
			continue
		}
		if c.Points == 0 {
			continue
		}
		out[handle] = c.Points
	}
	return out
}

// sprintWindowEnd is where a sprint's arithmetic stops.
//
// The CLOSE for a closed sprint and the planned end for every other, and
// NEITHER READS A CLOCK — see the file head. A sprint running past its end
// therefore reports its planned window rather than a window that grows on
// every poll, which is what makes two nodes agree.
func sprintWindowEnd(row SprintRow) time.Time {
	if row.ClosedAt != nil {
		return *row.ClosedAt
	}
	return row.EndAt
}

// velocityOf is the mean `done` over the CLOSED sprints in an answer.
//
// Closed only, because an active sprint's `done` is a partial total and
// averaging it in drags every team's velocity down by however far through the
// sprint they happen to be. NIL rather than zero when none has closed — see
// [SprintListing.VelocityAvg].
func velocityOf(rows []SprintRow) *float64 {
	var sum float64
	closed := 0
	for _, row := range rows {
		if row.State != SprintClosed {
			continue
		}
		sum += row.Figures.Done
		closed++
	}
	if closed == 0 {
		return nil
	}
	mean := sum / float64(closed)
	return &mean
}
