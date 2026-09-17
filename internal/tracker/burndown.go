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

// The burndown: what a sprint was carrying, and how much of it was still to do,
// at every day of its own window.
//
// # It is derived, like every other sprint figure, and stored nowhere
//
// [Reader.Sprints] already derives committed, added, removed and done from the
// STAYS and the SPANS rather than from counters, because "what did this sprint
// hold at its start" is a statement about a past instant and a counter is a
// statement about now. A burndown is that same reading taken at N instants
// instead of two, so it is the same two tables and no new state at all — which
// is why a sprint that closed a year ago still draws, and draws identically on
// every node.
//
// The schema was built for exactly this and had no reader:
// `tracker_status_spans_sprint_idx` carries the comment "the burndown and the
// burnup", and migration 0002's own header says the index that serves an
// aggregate arrives with the step that adds the aggregate's reader. This is
// that step.
//
// # REMAINING IS AN OPEN STATUS GROUP, not the negation of delivery
//
// The two differ on exactly one status and the difference is the whole point.
// `cancelled` is a FINISHED group that is not [Delivered] — that is what makes
// abandoned work invisible to velocity without a second field — so a remaining
// line written as "not delivered" would keep counting work the team had
// deliberately dropped, and the burndown of a sprint that was descoped would
// run flat to its end while everyone involved knew the work was gone.
//
// So remaining asks the question a burndown is actually about: is this still
// to do. Cancelling drops a task out of remaining and does NOT add to done —
// which is visible on the chart as a fall in the line with no matching rise in
// delivery, and is exactly the shape "we descoped" should have.
//
// # SCOPE IS THE SECOND LINE, and without it the first one lies
//
// A burndown drawn alone cannot tell "we finished eight points" from "somebody
// added eight points and we finished sixteen". Scope — everything the sprint
// was carrying at that instant, delivered and not — makes both visible: the
// gap between the two lines is what has left the remaining pile, and a step UP
// in scope is work that arrived after the start.
//
// # The walk is in memory, and that is a bound rather than a shortcut
//
// Two indexed reads and a pass per day. A sprint is at most [MaxSprintDays]
// long because the policy refuses a longer one, so the point count is bounded
// by construction and the alternative — a statement per day — would be up to
// ninety-two round trips over the same two index ranges.
const (
	// BurndownMaxPoints is the ceiling on the series a burndown returns.
	//
	// A sprint is bounded at 92 days by its own policy, so this is that
	// plus the start point, the final partial day, and slack. It exists so
	// that a sprint record whose dates were written by an older build —
	// or by a clock that ran away — cannot ask this walk for an unbounded
	// series.
	BurndownMaxPoints = 128
)

// BurndownPoint is one instant of the series.
type BurndownPoint struct {
	At time.Time `json:"at"`

	// Scope is everything the sprint was carrying at this instant, in the
	// project's own measure — delivered, abandoned and outstanding alike.
	Scope float64 `json:"scope"`

	// Remaining is the part of it still to do: a task whose status at this
	// instant was in an OPEN group. See the file head for why this is not
	// the negation of [Delivered].
	Remaining float64 `json:"remaining"`

	// Delivered is the part that had landed — [Delivered], so `cancelled`
	// is in neither this nor Remaining, and the three sum to Scope only
	// when nothing was abandoned. That gap IS the abandoned work, and it
	// is the one quantity a two-line burndown cannot show.
	Delivered float64 `json:"delivered"`
}

// Burndown is one sprint's series with everything needed to read it.
type Burndown struct {
	Project string `json:"project"`
	Sprint  int    `json:"sprint"`
	Name    string `json:"name,omitempty"`

	// Measure is on the answer because a bare number is points to one team
	// and minutes to another — the same reason [SprintFigures] carries it.
	Measure SprintMeasure `json:"measure"`

	StartAt time.Time `json:"start_at"`
	EndAt   time.Time `json:"end_at"`

	// Points run from the start to whichever of the sprint's end and the
	// caller's own instant comes first: a running sprint draws to today
	// and stops, rather than drawing a flat line into its own future.
	Points []BurndownPoint `json:"points"`

	// Ideal is the scope at the start, which is the height the reference
	// line falls from. The line itself is the renderer's — two points and
	// a straight edge is not something to send over a wire.
	Ideal float64 `json:"ideal"`

	// Tasks and Unestimated are the honesty pair the sprint figures
	// already carry: a series over a sprint half of whose tasks carry no
	// value in the measure is a series about half a sprint.
	Tasks       int `json:"tasks"`
	Unestimated int `json:"unestimated"`

	Level          statelog.ReadLevel `json:"read_level"`
	LogSeq         uint64             `json:"log_seq"`
	AppliedThrough uint64             `json:"applied_through"`
	LogLag         *uint64            `json:"log_lag,omitempty"`
	Complete       bool               `json:"complete"`
	Incomplete     *Incomplete        `json:"incomplete,omitempty"`
}

// BurndownQuery asks for one sprint's series.
type BurndownQuery struct {
	// Project and Sprint are both required: a sprint is numbered per
	// project, so a number with no key names as many sprints as the
	// company has teams.
	Project string
	Sprint  int

	Level       statelog.ReadLevel
	Session     statelog.Position
	MinPosition statelog.Position
	MaxLag      time.Duration
	MaxLagSeq   uint64
}

// stay is one task's membership of the sprint, as the walk reads it.
type stay struct {
	from int64
	to   sql.NullInt64
}

// span is one interval a task spent in one status.
type span struct {
	group   StatusGroup
	status  Status
	entered int64
	left    sql.NullInt64
}

// burnTask is everything the walk needs about one task.
//
// `measures` is a HISTORY rather than a number, which is the whole of this
// chart's correctness: the walk asks what each member was worth AT EACH
// INSTANT, and a single value read before the walk made a chart of change in
// time out of a quantity that had none. A task re-estimated from 3 to 8 on day
// 5 was drawn as 8 on day 1, so a sprint that delivered exactly what it took
// on rendered as one handed more work and finished it — and the shape of a
// closed sprint moved whenever somebody tidied an estimate months later.
type burnTask struct {
	measures []measureSpan
	stays    []stay
	spans    []span
}

// measureSpan is one span of a task's size, in the project's own measure.
//
// ONE VALUE RATHER THAN BOTH, because the read already knows which measure the
// project scores in and selecting it in SQL keeps the choice in one place —
// see [SprintMeasure.SpanColumn].
type measureSpan struct {
	from  int64
	to    sql.NullInt64
	value float64
}

// Burndown answers one sprint's series.
func (r *Reader) Burndown(ctx context.Context, q BurndownQuery, now time.Time) (
	Burndown, error) {

	if q.Level == "" {
		return Burndown{}, fmt.Errorf("tracker: this burndown read names no " +
			"level — a surface resolves an absent read_level to its own " +
			"default before it reads")
	}
	project := ProjectKey(q.Project)
	if project == "" {
		return Burndown{}, fmt.Errorf("tracker: a burndown names a project — " +
			"sprints are numbered per project, so a number alone names as " +
			"many sprints as the company has teams")
	}
	if q.Sprint <= 0 {
		return Burndown{}, fmt.Errorf("tracker: a burndown names a sprint "+
			"number and %d is not one — `sprint_report` lists the numbers a "+
			"project has", q.Sprint)
	}

	out := Burndown{Project: project, Sprint: q.Sprint}
	served, err := r.log.Read(ctx, statelog.Query{
		Level: q.Level,
		// THE PROJECT'S CONTAINER, exactly as [Reader.Sprints] scopes
		// itself and for the same reason: every figure here is an
		// aggregate over the project's task rows, and a closure naming
		// only the sprint record would certify the series complete
		// while a deferred task record was holding a point.
		Scope:       sprintReadScope(project),
		Session:     q.Session,
		MinPosition: q.MinPosition,
		MaxLag:      q.MaxLag,
		MaxLagSeq:   q.MaxLagSeq,
		Set:         true,
	}, func(tx *sql.Tx) error {
		// A FRESH ANSWER PER ATTEMPT, assigned to `out` as the last
		// statement. This closure is not run once: a read transaction
		// that loses its snapshot to a writer is retried up to
		// [store.txAttempts] times on a new one, so anything it
		// ACCUMULATES into a value captured from outside is added to
		// again on every attempt. `Unestimated` was counted that way —
		// a sprint with two unestimated tasks reported four after one
		// retry — and `Ideal` was the same hazard in the other
		// direction, assigned only when the series has points, so an
		// attempt that found none kept the previous attempt's height.
		// Neither is visible in the answer: every other figure is
		// correct, and `Unestimated` is precisely the number a reader
		// consults to decide how much of the series to believe.
		b := Burndown{Project: project, Sprint: q.Sprint}

		p, found, err := readProject(ctx, tx, project)
		if err != nil {
			return err
		}
		if !found {
			return fmt.Errorf("tracker: no project %q — %w", project, ErrNoProject)
		}
		b.Measure = measureOf(p)

		sprint, found, err := readSprintWindow(ctx, tx, project, q.Sprint)
		if err != nil {
			return err
		}
		if !found {
			return fmt.Errorf("tracker: %s has no sprint %d — %w",
				project, q.Sprint, ErrNoSprint)
		}
		b.Name = sprint.name
		b.StartAt = store.DecodeTime(sprint.start)
		b.EndAt = store.DecodeTime(sprint.end)

		tasks, err := readBurndownTasks(ctx, tx, project, q.Sprint, b.Measure)
		if err != nil {
			return err
		}
		b.Tasks = len(tasks)
		for _, task := range tasks {
			// THE OPEN SPAN, because this figure is present tense.
			// It is the honesty pair on the chart as a whole — "this
			// many of these tasks carry no value, so the series
			// understates" — and what a reader would go and fix is
			// the estimate the task has NOW. Valuing it at the
			// sprint's start instead would report a task estimated
			// on day 2 as unestimated for ever.
			if measureNow(task.measures) == 0 {
				b.Unestimated++
			}
		}
		b.Points = burndownSeries(tasks,
			burndownInstants(b.StartAt, b.EndAt, sprint.closed, now))
		if len(b.Points) > 0 {
			b.Ideal = b.Points[0].Scope
		}

		position, applied, err := readCheckpoint(ctx, tx)
		if err != nil {
			return err
		}
		b.LogSeq, b.AppliedThrough = position, applied

		out = b
		return nil
	})
	if err != nil {
		return Burndown{}, err
	}
	out.Level = served.Level
	out.Complete = served.Complete
	out.LogLag = served.Lag
	if served.Incomplete != nil {
		out.Incomplete = incompleteFrom(served.Incomplete)
	}
	return out, nil
}

// sprintWindow is the one sprint row a burndown needs.
type sprintWindow struct {
	name   string
	start  int64
	end    int64
	closed sql.NullInt64
}

func readSprintWindow(ctx context.Context, tx *sql.Tx, project string, number int) (
	sprintWindow, bool, error) {

	var w sprintWindow
	err := tx.QueryRowContext(ctx, `
		SELECT s.name, s.start_at, s.end_at, s.closed_at
		FROM tracker_sprints s
		WHERE s.project_key = ? AND s.number = ?`,
		project, number).Scan(&w.name, &w.start, &w.end, &w.closed)
	if err == sql.ErrNoRows {
		return sprintWindow{}, false, nil
	}
	if err != nil {
		return sprintWindow{}, false, fmt.Errorf(
			"tracker: read sprint %d of %s: %w", number, project, err)
	}
	return w, true, nil
}

// readBurndownTasks reads every task that was ever in the sprint, with its
// stays and its status spans.
//
// THE SPANS ARE REACHED THROUGH THE STAYS, never by their own sprint column.
// A span carries the task's sprint AS OF THE RECOMPUTE THAT WROTE IT — the
// applier stamps `t.sprint_number` at insert and rewrites a task's whole span
// set on every history row — so a task that has since moved to the next sprint
// carries spans labelled with that next sprint, and a read filtered on
// `sprint_number` would lose exactly the carried-over work a burndown most
// needs to show. The membership table is the authority on who was in the
// sprint; the spans are only asked what a status was and when.
func readBurndownTasks(ctx context.Context, tx *sql.Tx, project string,
	number int, measure SprintMeasure) (map[string]*burnTask, error) {

	tasks := map[string]*burnTask{}

	// A REMOVED TASK IS NOT COUNTED, the same rule every sprint figure
	// takes: a removal hides work, and a burndown that kept counting it
	// would climb when somebody tidied up.
	rows, err := tx.QueryContext(ctx, `
		SELECT m.task_id, m.from_at, m.to_at
		FROM tracker_task_sprints m
		JOIN tracker_tasks t ON t.id = m.task_id
		WHERE m.project_key = ? AND m.sprint = ? AND t.removed_at IS NULL`,
		project, number)
	if err != nil {
		return nil, fmt.Errorf("tracker: read the stays of sprint %d of %s: %w",
			number, project, err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var id string
		var s stay
		//nolint:govet // shadow: scoped to this block; see .golangci.yml
		if err := rows.Scan(&id, &s.from, &s.to); err != nil {
			return nil, fmt.Errorf("tracker: scan a stay of sprint %d of %s: %w",
				number, project, err)
		}
		task, held := tasks[id]
		if !held {
			task = &burnTask{}
			tasks[id] = task
		}
		task.stays = append(task.stays, s)
	}
	//nolint:govet // shadow: scoped to this block; see .golangci.yml
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("tracker: read the stays of sprint %d of %s: %w",
			number, project, err)
	}
	if len(tasks) == 0 {
		return tasks, nil
	}

	// THE SIZE HISTORY, one row per span, for the same set of tasks. Read
	// separately rather than joined onto the stays because the two are
	// independent intervals over one task — a re-estimate inside a stay
	// makes two measure spans and one stay, and a carry-over makes two
	// stays and one measure span — so a join would multiply them out and
	// the walk would count the product.
	values, err := tx.QueryContext(ctx, `
		SELECT v.task_id, v.from_at, v.to_at, `+measure.SpanColumn()+`
		FROM tracker_measure_spans v
		WHERE v.task_id IN (
			SELECT m.task_id FROM tracker_task_sprints m
			JOIN tracker_tasks t ON t.id = m.task_id
			WHERE m.project_key = ? AND m.sprint = ? AND t.removed_at IS NULL)`,
		project, number)
	if err != nil {
		return nil, fmt.Errorf("tracker: read the sizes of sprint %d of %s: %w",
			number, project, err)
	}
	for values.Next() {
		var id string
		var v measureSpan
		if err = values.Scan(&id, &v.from, &v.to, &v.value); err != nil {
			_ = values.Close()
			return nil, fmt.Errorf("tracker: scan a size of sprint %d of %s: %w",
				number, project, err)
		}
		if task, held := tasks[id]; held {
			task.measures = append(task.measures, v)
		}
	}
	if err = values.Err(); err != nil {
		_ = values.Close()
		return nil, fmt.Errorf("tracker: read the sizes of sprint %d of %s: %w",
			number, project, err)
	}
	if err = values.Close(); err != nil {
		return nil, fmt.Errorf("tracker: close the sizes of sprint %d of %s: %w",
			number, project, err)
	}

	spans, err := tx.QueryContext(ctx, `
		SELECT s.task_id, s.status, s.grp, s.entered_at, s.left_at
		FROM tracker_status_spans s
		WHERE s.task_id IN (
			SELECT m.task_id FROM tracker_task_sprints m
			JOIN tracker_tasks t ON t.id = m.task_id
			WHERE m.project_key = ? AND m.sprint = ? AND t.removed_at IS NULL)`,
		project, number)
	if err != nil {
		return nil, fmt.Errorf("tracker: read the spans of sprint %d of %s: %w",
			number, project, err)
	}
	defer func() { _ = spans.Close() }()
	for spans.Next() {
		var id, status, group string
		var sp span
		if err := spans.Scan(&id, &status, &group, &sp.entered, &sp.left); err != nil {
			return nil, fmt.Errorf("tracker: scan a span of sprint %d of %s: %w",
				number, project, err)
		}
		task, held := tasks[id]
		if !held {
			continue
		}
		sp.status, sp.group = Status(status), StatusGroup(group)
		task.spans = append(task.spans, sp)
	}
	if err := spans.Err(); err != nil {
		return nil, fmt.Errorf("tracker: read the spans of sprint %d of %s: %w",
			number, project, err)
	}
	for _, task := range tasks {
		sort.Slice(task.spans, func(i, j int) bool {
			return task.spans[i].entered < task.spans[j].entered
		})
		// OLDEST FIRST, which is what [measureAt]'s fallback to the
		// first span depends on: SQL returned these in no order the
		// read asked for.
		sort.Slice(task.measures, func(i, j int) bool {
			return task.measures[i].from < task.measures[j].from
		})
	}
	return tasks, nil
}

// burndownInstants is the series' own x axis.
//
// # A DAY IS MEASURED FROM THE SPRINT'S OWN START, not from midnight
//
// Stepping by 24 hours from `start_at` needs no timezone at all, which removes
// the one thing two nodes could disagree about: a company zone read at read
// time is a value the answer would depend on, and a burndown that drew
// differently in two browsers would be a burndown nobody trusted. A sprint
// starts at a fixed clock time by policy (`StartMinutes`), so the ticks land
// at the same wall-clock time every day anyway — and what they actually mean,
// day 1 / day 2 / day 3 of the sprint, is what a reader wants a burndown's
// ticks to mean.
//
// The series stops at the sprint's end or at the caller's own instant,
// whichever is first, so a running sprint draws to today rather than flat into
// its own future — and a closed one stops at its close.
func burndownInstants(start, end time.Time, closedAt sql.NullInt64,
	now time.Time) []time.Time {

	last := end
	if closedAt.Valid {
		// A SPRINT THAT CLOSED EARLY ENDS WHEN IT CLOSED. Its planned
		// end is in the future of its own history, and drawing to it
		// would append days in which, by construction, nothing could
		// have happened.
		if at := store.DecodeTime(closedAt.Int64); at.Before(last) {
			last = at
		}
	} else if now.Before(last) {
		last = now
	}
	if last.Before(start) {
		// A sprint that has not started yet, or one whose dates were
		// written backwards. One point rather than none: a chart with
		// an empty series and a chart that could not be read are
		// different states, and the caller distinguishes them by the
		// point count.
		return []time.Time{start}
	}
	out := []time.Time{start}
	for at := start.AddDate(0, 0, 1); at.Before(last); at = at.AddDate(0, 0, 1) {
		if len(out) >= BurndownMaxPoints-1 {
			break
		}
		out = append(out, at)
	}
	if tail := out[len(out)-1]; tail.Before(last) {
		out = append(out, last)
	}
	return out
}

// burndownSeries walks the tasks once per instant.
func burndownSeries(tasks map[string]*burnTask, instants []time.Time) []BurndownPoint {
	out := make([]BurndownPoint, 0, len(instants))
	for _, at := range instants {
		t := store.EncodeTime(at)
		point := BurndownPoint{At: at}
		for _, task := range tasks {
			if !inSprintAt(task.stays, t) {
				continue
			}
			// WHAT IT WAS WORTH THEN, which is the point of the
			// series: a re-estimate moves this instant's scope and
			// every one after it, and leaves the instants before it
			// as they were reported.
			value := measureAt(task.measures, t)
			point.Scope += value
			status, group, known := statusAt(task.spans, t)
			switch {
			case !known || !group.Finished():
				// A TASK WITH NO SPAN COVERING THIS INSTANT COUNTS AS
				// REMAINING. It is in the sprint — the membership row
				// says so — and the honest reading of a gap in its own
				// history is "still to do": the alternative silently
				// burns work down for a record this build could not
				// read, which is the one direction that flatters.
				point.Remaining += value
			case Delivered(status):
				point.Delivered += value
			}
		}
		out = append(out, point)
	}
	return out
}

// inSprintAt reports whether any stay covers the instant.
//
// HALF-OPEN, like every other interval in this package: a task that left at
// exactly `t` is not in the sprint at `t`, which is what keeps a hand-off
// between two sprints from counting in both.
func inSprintAt(stays []stay, at int64) bool {
	for _, s := range stays {
		if s.from <= at && (!s.to.Valid || s.to.Int64 > at) {
			return true
		}
	}
	return false
}

// measureAt is what a task was worth at one instant.
//
// THE SPAN COVERING IT, and before the first span the FIRST span's value
// rather than zero — the same rule [MeasureAt] takes on the document, and for
// the same reason. A sprint's start can be older than the history the per-task
// cap kept, and answering zero there would say the task was unestimated when
// what is true is that this node no longer knows what it was worth. The oldest
// value it does know is the honest answer, and it degrades towards the old
// behaviour rather than towards a burndown that starts below its own scope.
//
// Deliberately NOT the `known` three-valued shape [statusAt] takes. A status
// this build cannot read has a real consequence — it decides which band the
// work counts in — so inventing `todo` there would be inventing a fact. A
// size has no bands: an unknown one is a quantity, and the quantity nearest
// to true is the one it last held.
func measureAt(spans []measureSpan, at int64) float64 {
	if len(spans) == 0 {
		return 0
	}
	value := spans[0].value
	for _, v := range spans {
		if v.from <= at {
			value = v.value
			continue
		}
		break
	}
	return value
}

// measureNow is what a task is worth today: the value of its OPEN span.
//
// The spans are oldest-first and at most one is open, so it is the last —
// which is also what a task with a capped history still answers correctly,
// since the cap only ever drops closed spans.
func measureNow(spans []measureSpan) float64 {
	if len(spans) == 0 {
		return 0
	}
	return spans[len(spans)-1].value
}

// statusAt is the status a task held at one instant, and whether it held one.
//
// The `known` third value is not decoration: a task created after this instant,
// or one whose spans this build could not decode, has no status here — and
// answering `todo` for it would be inventing a fact rather than reporting one.
func statusAt(spans []span, at int64) (Status, StatusGroup, bool) {
	for _, sp := range spans {
		if sp.entered <= at && (!sp.left.Valid || sp.left.Int64 > at) {
			return sp.status, sp.group, true
		}
	}
	return "", "", false
}
