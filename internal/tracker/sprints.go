package tracker

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/crewlet/crewlet/internal/statelog"
)

// Minting sprints, and settling what a closed one left behind.
//
// The two TRANSITIONS a sprint has — start and close — are sequences 18 and
// 19 in `sequence.go`, because each is two writes whose order is the argument.
// What is here is everything around them: where the records come from, and
// where the unfinished work goes.
//
// # A mint is a CREATE-ONLY APPEND at expectation zero
//
// So a collision is harmless rather than a race to lose: two nodes minting
// sprint 7 contend at the broker, one wins, and the loser's refusal means the
// sprint it wanted exists — which is what it was asking for. Nothing here
// reads a counter and writes it back.
//
// # A sprint ALWAYS closes at its end; what happens to the work is the policy
//
// The close is not a setting — a sprint that ran past its own window is a
// number nobody can report on, and ClickUp's own "mark sprint as done" is
// permanently enabled for the same reason. `AutoRoll` decides the SPILLOVER,
// and without it the close leaves it PENDING: a state a lead settles, rather
// than an absence somebody has to interpret.

// ErrSprintState reports a transition a sprint's own state refuses.
//
// ITS OWN SENTINEL, because the caller's answer differs: a tool says "that
// sprint is already closed" to a lead, and the duty treats the same fact as
// work another node finished first — neither should say "the write failed".
var ErrSprintState = errors.New("tracker: the sprint is not in that state")

// RolloverTarget is where a closed sprint's unfinished work goes.
type RolloverTarget string

const (
	// RolloverNext is the lowest-numbered future sprint, minted on the
	// spot when the project has none.
	RolloverNext RolloverTarget = "next"

	// RolloverBacklog clears each task's pointer and leaves it in the
	// project — which is what the Backlog IS here: not a container, but
	// the absence of a sprint.
	RolloverBacklog RolloverTarget = "backlog"

	// RolloverClose writes every open task to `cancelled` — abandoned,
	// not delivered, which is exactly what `cancelled` means and why it
	// is a finished group that [Delivered] excludes.
	RolloverClose RolloverTarget = "close"
)

// RolloverTargets are the three named ones; a sprint NUMBER is the fourth form.
var RolloverTargets = []RolloverTarget{
	RolloverNext, RolloverBacklog, RolloverClose,
}

// MaxSprintsAhead bounds how many unstarted sprints a project may hold.
//
// TWELVE, which is half a year at a fortnight and a quarter at a week: a
// project minting further ahead than that is minting records nobody will read
// before the policy that shaped them has changed. It is a CEILING on the
// policy rather than the policy's own value, which is whatever a lead set.
const MaxSprintsAhead = 12

// DefaultSprintDays is a fortnight, which is what a policy naming no length
// means.
//
// Two weeks is the cadence most teams settle on, and the default is here
// rather than in the policy's zero value so a project that declares sprints
// and forgets the length gets a working cadence rather than a sprint of no
// days at all — which would close on the tick it started.
const DefaultSprintDays = 14

// MaxSprintDays bounds what a policy may declare.
//
// NINETY-TWO — one calendar quarter, the longest of the four. Past that the
// window stops being a sprint in every way that matters to this engine: the
// report it produces covers a period nobody can still remember committing to,
// [MaxSprintsAhead] unstarted sprints reach three years into the future, and
// the rollover that settles spillover runs once a quarter on a set of tasks
// large enough to need its own planning. A team that wants a longer horizon
// has one — it is a goal.
const MaxSprintDays = 92

// RolloverBatch is how many tasks one rollover pass moves.
//
// SIXTY-FOUR, which is one indexed range read and sixty-four ordinary task
// commits: a sprint of four hundred unfinished tasks becomes seven passes
// rather than one transaction the broker has to accept whole. The duty runs
// again on its next tick, and a half-finished rollover is an ordinary state —
// the remaining tasks are still in the closed sprint, which is exactly what
// the pass selects on.
const RolloverBatch = 64

// MintSprints tops a project's future sprints up to its policy's `Ahead`.
//
// ONE READ, THEN N APPENDS — never a read between each. The appends go to the
// LOG and this node's rows are written by an applier that has not run yet, so
// a loop that re-read after each mint would see the same deficit every time
// and try to mint the same number for ever. The numbers are therefore computed
// together, from one snapshot, and each is published independently.
//
// A COLLISION IS HARMLESS rather than a race to lose: two nodes minting sprint
// 7 contend at the broker, one wins, and the loser's refusal means the sprint
// it wanted EXISTS — which is what it was asking for.
func (w *Writer) MintSprints(ctx context.Context, opID, project string,
	zone *time.Location) ([]int, error) {

	project = ProjectKey(project)
	numbers, policy, err := w.sprintsToMint(ctx, project)
	if err != nil || len(numbers) == 0 {
		return nil, err
	}
	var minted []int
	for _, number := range numbers {
		sprint := sprintFor(project, number, policy, w.Now(), zone)
		_, err := w.WriteDocument(ctx, stepID(opID, fmt.Sprintf("m%d", number)),
			SprintSubject(project, number), "", sprint, nil)
		switch {
		case errors.Is(err, statelog.ErrExists),
			errors.Is(err, statelog.ErrConflict):
			continue
		case err != nil:
			// THE ONES ALREADY MINTED ARE REPORTED, because they
			// landed: a caller told "none" would mint them again on
			// its next tick and collide with its own work.
			return minted, err
		}
		minted = append(minted, number)
	}
	return minted, nil
}

// sprintsToMint is the numbers a project is short of, from ONE snapshot.
func (w *Writer) sprintsToMint(ctx context.Context, project string) (
	[]int, SprintPolicy, error) {

	var numbers []int
	var policy SprintPolicy
	err := w.db.Replicated().Read(ctx, func(tx *sql.Tx) error {
		p, held, err := readProject(ctx, tx, project)
		if err != nil {
			return err
		}
		if !held || p.Sprints == nil || p.Sprints.Ahead <= 0 {
			return nil
		}
		policy = *p.Sprints
		var unstarted, highest int
		if err := tx.QueryRowContext(ctx, `
			SELECT
				COALESCE(SUM(CASE WHEN state = ? THEN 1 ELSE 0 END), 0),
				COALESCE(MAX(number), 0)
			FROM tracker_sprints WHERE project_key = ?`,
			string(SprintFuture), project).Scan(&unstarted, &highest); err != nil {
			return fmt.Errorf("tracker: count the future sprints of %s: %w",
				project, err)
		}
		deficit := min(policy.Ahead, MaxSprintsAhead) - unstarted
		// ABOVE EVERY NUMBER THE PROJECT HAS, not the policy's own
		// counter: a number is immutable, a sprint is never deleted, and
		// the closed ones' stays are still read — so reusing one would
		// put two windows on one set of rows.
		next := max(highest, policy.Next-1) + 1
		for i := range deficit {
			numbers = append(numbers, next+i)
		}
		return nil
	})
	return numbers, policy, err
}

// sprintFor builds one future sprint from a project's policy.
//
// THE WINDOW IS A CALENDAR BOUNDARY, not an offset from now: `StartMinutes`
// past midnight on the policy's `StartWeekday`, in the company's ONE zone —
// which is why the policy stores a weekday and a minute count rather than an
// instant. A sprint minted at 3am and one minted at 5pm start at the same
// moment, because the boundary is what a team plans around.
func sprintFor(project string, number int, policy SprintPolicy, now time.Time,
	zone *time.Location) Sprint {

	if zone == nil {
		zone = time.UTC
	}
	length := policy.LengthDays
	if length <= 0 {
		length = DefaultSprintDays
	}
	start := nextWeekday(now.In(zone), policy.StartWeekday, policy.StartMinutes)
	// EACH SPRINT AFTER THE FIRST STARTS WHERE THE LAST ENDED, so a
	// cadence stays a cadence: computed from `now` alone, every sprint
	// minted in one tick would start on the same day.
	if ahead := number - max(policy.Next, 1); ahead > 0 {
		start = start.AddDate(0, 0, length*ahead)
	}
	return Sprint{
		V: DocumentVersion, Project: project, Number: number,
		Name:      sprintName(policy.NameFormat, project, number),
		State:     SprintFuture,
		StartAt:   start.UTC(),
		EndAt:     start.AddDate(0, 0, length).UTC(),
		CreatedBy: "system",
		CreatedAt: now.UTC(),
		UpdatedAt: now.UTC(),
	}
}

// nextWeekday is the next boundary at or after t.
func nextWeekday(t time.Time, weekday time.Weekday, minutes int) time.Time {
	day := time.Date(t.Year(), t.Month(), t.Day(), 0, minutes, 0, 0, t.Location())
	day = day.AddDate(0, 0, (int(weekday)-int(day.Weekday())+7)%7)
	if day.Before(t) {
		// TODAY'S BOUNDARY HAS PASSED, so the next one is a week out —
		// which is what "the next Monday" means on a Monday afternoon.
		day = day.AddDate(0, 0, 7)
	}
	return day
}

// sprintName renders a sprint's name from the policy's format.
//
// TWO PLACEHOLDERS AND NO TEMPLATE ENGINE: a name is read by people on a board
// and typed into a filter, and a format that could compute would be one
// somebody puts a loop in.
func sprintName(format, project string, number int) string {
	if strings.TrimSpace(format) == "" {
		return fmt.Sprintf("%s Sprint %d", project, number)
	}
	name := strings.ReplaceAll(format, "{project}", project)
	return strings.ReplaceAll(name, "{number}", fmt.Sprint(number))
}

// RolloverSprint settles what a closed sprint left unfinished.
//
// ONE BATCH PER CALL — see [RolloverBatch] — and the pass is IDEMPOTENT by
// selection rather than by a marker: it moves the open tasks still pointing at
// the closed sprint, so a re-run moves what is left and a finished rollover
// moves nothing. `Done` on the sprint record is what stops the duty looking.
func (w *Writer) RolloverSprint(ctx context.Context, opID, project string,
	number int, target RolloverTarget) (moved int, done bool, err error) {

	project = ProjectKey(project)
	sprint, held, err := w.readSprintRow(ctx, project, number)
	switch {
	case err != nil:
		return 0, false, err
	case !held:
		return 0, false, fmt.Errorf("tracker: %s has no sprint %d: %w",
			project, number, ErrSprintState)
	case sprint.State != SprintClosed:
		return 0, false, fmt.Errorf("tracker: sprint %d of %s is %s, and only "+
			"a closed sprint has a spillover to settle: %w",
			number, project, sprint.State, ErrSprintState)
	}

	into, err := w.resolveRollover(ctx, opID, project, number, target)
	if err != nil {
		return 0, false, err
	}
	tasks, err := w.openTasksInSprint(ctx, project, number, RolloverBatch)
	if err != nil {
		return 0, false, err
	}
	for _, task := range tasks {
		if err := w.rollOne(ctx, stepID(opID, task.ID), task, target, into); err != nil {
			return moved, false, err
		}
		moved++
	}
	if len(tasks) == RolloverBatch {
		// A FULL BATCH MEANS THERE IS PROBABLY MORE. Saying so rather
		// than probing again: the duty's next tick re-selects, and one
		// extra pass that finds nothing is cheaper than a second count.
		return moved, false, nil
	}
	// AND THE RECORD RECORDS WHERE IT WENT, which is what turns a PENDING
	// spillover into a settled one — the state `work_sprints` reports and
	// a lead acts on.
	return moved, true, w.settleRollover(ctx, stepID(opID, "settle"), project,
		number, target, into)
}

// resolveRollover turns a target into the sprint number a task moves to.
//
// `next` MINTS ONE WHERE THERE IS NONE, because a policy that rolls forward
// with nothing to roll into would leave the work in a closed sprint for ever —
// which is the state the straggler sweep exists to prevent rather than to
// create.
func (w *Writer) resolveRollover(ctx context.Context, opID, project string,
	number int, target RolloverTarget) (*int, error) {

	switch target {
	case RolloverBacklog, RolloverClose:
		return nil, nil
	case RolloverNext:
		into, found, err := w.lowestFuture(ctx, project)
		if err != nil {
			return nil, err
		}
		if found {
			return &into, nil
		}
		minted, err := w.MintSprints(ctx, stepID(opID, "mint"), project, nil)
		if err != nil || len(minted) == 0 {
			return nil, fmt.Errorf("tracker: %s has no future sprint to roll "+
				"sprint %d into and none could be minted — give it a sprint "+
				"policy with `ahead` above zero, or roll to `backlog`: %w",
				project, number, err)
		}
		return &minted[0], nil
	}
	explicit, err := rolloverNumber(target)
	if err != nil {
		return nil, err
	}
	if explicit == number {
		return nil, fmt.Errorf("tracker: sprint %d of %s cannot roll into "+
			"itself", number, project)
	}
	sprint, held, err := w.readSprintRow(ctx, project, explicit)
	switch {
	case err != nil:
		return nil, err
	case !held:
		return nil, fmt.Errorf("tracker: %s has no sprint %d: %w",
			project, explicit, ErrSprintState)
	case sprint.State == SprintClosed:
		return nil, fmt.Errorf("tracker: sprint %d of %s is closed, and work "+
			"cannot be rolled into a closed sprint — name a future or active "+
			"one, or roll to `backlog`: %w", explicit, project, ErrSprintState)
	}
	return &explicit, nil
}

// rolloverNumber reads the fourth form of a target.
func rolloverNumber(target RolloverTarget) (int, error) {
	var n int
	if _, err := fmt.Sscanf(string(target), "%d", &n); err != nil || n < 1 {
		return 0, fmt.Errorf("tracker: %q is not a rollover target — the four "+
			"are %s, or a sprint number", target, joinTargets())
	}
	return n, nil
}

func joinTargets() string {
	out := make([]string, 0, len(RolloverTargets))
	for _, t := range RolloverTargets {
		out = append(out, string(t))
	}
	return strings.Join(out, ", ")
}

// rollOne moves one task out of a closed sprint.
//
// `Notify == nil` ON EVERY ONE, because a rollover is ONE thing that happened
// and a sprint of forty unfinished tasks would otherwise send forty wakes for
// it. The sprint's own close is the wake that says what was done.
func (w *Writer) rollOne(ctx context.Context, opID string, task rollTask,
	target RolloverTarget, into *int) error {

	// ZERO IS HOW A PATCH SPELLS "CLEAR IT" — see [applyPatch]: an absent
	// field means "leave it alone", so the clear has to be a value, and a
	// sprint is numbered from one.
	clear := 0
	sprint := into
	if sprint == nil {
		sprint = &clear
	}
	patch := TaskPatch{Sprint: sprint}
	if target == RolloverClose {
		// ABANDONED, NOT DELIVERED. `cancelled` is a finished group that
		// [Delivered] excludes for exactly this reason: a team that
		// cancelled its remaining work has not shipped it. And the
		// pointer is cleared with it, because a cancelled task sitting
		// in a closed sprint is what the straggler sweep would keep
		// finding.
		cancelled := StatusCancelled
		patch.Status, patch.Sprint = &cancelled, &clear
	}
	_, err := w.UpdateTask(ctx, opID, task.ID, task.Project, NoIfMatch, patch, nil)
	if errors.Is(err, statelog.ErrConflict) {
		// ANOTHER WRITER MOVED IT FIRST, which on a rollover is another
		// node's pass or the task's own assignee. The selection is what
		// makes this idempotent, so the next pass simply will not see
		// it.
		return nil
	}
	return err
}

// settleRollover stamps where a sprint's spillover went.
func (w *Writer) settleRollover(ctx context.Context, opID, project string,
	number int, target RolloverTarget, into *int) error {

	subject := SprintSubject(project, number)
	scope := ScopeSet{Subject: true, Container: project}
	at := w.Now()
	_, err := w.published(ctx, statelog.Request{
		Subject:  wire(subject),
		Scope:    scope.Resolve(subject),
		OpID:     opID,
		MintedAt: at,
		Pattern:  statelog.PatternArbitrated,
		Decide: func(tx *sql.Tx) (statelog.Decision, error) {
			current, held, err := readSprint(ctx, tx, project, number)
			if err != nil {
				return statelog.Decision{}, err
			}
			if !held {
				return statelog.Decision{}, fmt.Errorf("tracker: %s has no "+
					"sprint %d: %w", project, number, ErrSprintState)
			}
			if current.RolloverDone {
				// ANOTHER NODE SETTLED IT. An empty decision is a
				// legitimate outcome, and re-stamping would be a
				// second record saying what the first said.
				return statelog.Decision{}, nil
			}
			next := current
			next.RolloverTo = rolloverLabel(target, into)
			next.RolloverDone = true
			next.UpdatedAt = at
			decision, err := w.decide(subject, OpPatch, scope, opID, next,
				nil, at)
			if err != nil {
				return statelog.Decision{}, err
			}
			decision.Version = int64(current.Version)
			return decision, nil
		},
	})
	return err
}

// rolloverLabel is what the record says the spillover became.
//
// A STRING RATHER THAN A NUMBER, because three of the four answers are not
// numbers: `backlog` and `close` name no sprint, and an empty field is already
// taken — it is what PENDING means.
func rolloverLabel(target RolloverTarget, into *int) string {
	if into != nil {
		return fmt.Sprint(*into)
	}
	return string(target)
}

// rollTask is one task a rollover pass moves.
type rollTask struct{ ID, Project string }

// openTasksInSprint reads what a sprint still holds unfinished.
func (w *Writer) openTasksInSprint(ctx context.Context, project string,
	number, limit int) ([]rollTask, error) {

	open := openGroups()
	var out []rollTask
	err := w.db.Replicated().Read(ctx, func(tx *sql.Tx) error {
		args := append([]any{project, number}, open...)
		args = append(args, limit)
		rows, err := tx.QueryContext(ctx, `
			SELECT t.id, t.project_key FROM tracker_tasks t
			WHERE t.project_key = ? AND t.sprint_number = ?
			  AND t.removed_at IS NULL
			  AND t.status_group IN (`+placeholders(len(open))+`)
			ORDER BY t.id
			LIMIT ?`, args...)
		if err != nil {
			return err
		}
		defer func() { _ = rows.Close() }()
		for rows.Next() {
			var task rollTask
			if err := rows.Scan(&task.ID, &task.Project); err != nil {
				return err
			}
			out = append(out, task)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, fmt.Errorf("tracker: read the open work of sprint %d of "+
			"%s: %w", number, project, err)
	}
	return out, nil
}

// lowestFuture is the next sprint a project has to roll into.
func (w *Writer) lowestFuture(ctx context.Context, project string) (
	number int, found bool, err error) {

	err = w.db.Replicated().Read(ctx, func(tx *sql.Tx) error {
		var lowest sql.NullInt64
		if err := tx.QueryRowContext(ctx, `
			SELECT MIN(number) FROM tracker_sprints
			WHERE project_key = ? AND state = ? AND archived = 0`,
			project, string(SprintFuture)).Scan(&lowest); err != nil {
			return err
		}
		number, found = int(lowest.Int64), lowest.Valid
		return nil
	})
	if err != nil {
		return 0, false, fmt.Errorf("tracker: read the next sprint of %s: %w",
			project, err)
	}
	return number, found, nil
}

// readSprintRow reads one sprint outside a write's own transaction.
func (w *Writer) readSprintRow(ctx context.Context, project string, number int) (
	Sprint, bool, error) {

	var sprint Sprint
	var held bool
	err := w.db.Replicated().Read(ctx, func(tx *sql.Tx) error {
		var err error
		sprint, held, err = readSprint(ctx, tx, project, number)
		return err
	})
	return sprint, held, err
}

// ArchiveSprint hides a settled sprint from the sprint screens.
//
// IT TOUCHES NO TASK. An archived sprint's stays are still read — `sprint=<an
// archived number>` answers from them — so this is about what a screen offers
// rather than about what a report can reach: a project three years into a
// weekly cadence has a hundred and fifty sprints, and a picker listing all of
// them is a picker nobody uses.
func (w *Writer) ArchiveSprint(ctx context.Context, opID, project string,
	number int) (WriteResult, error) {

	project = ProjectKey(project)
	subject := SprintSubject(project, number)
	scope := ScopeSet{Subject: true, Container: project}
	at := w.Now()
	return w.published(ctx, statelog.Request{
		Subject:  wire(subject),
		Scope:    scope.Resolve(subject),
		OpID:     opID,
		MintedAt: at,
		Pattern:  statelog.PatternArbitrated,
		Decide: func(tx *sql.Tx) (statelog.Decision, error) {
			current, held, err := readSprint(ctx, tx, project, number)
			if err != nil {
				return statelog.Decision{}, err
			}
			switch {
			case !held:
				return statelog.Decision{}, fmt.Errorf("tracker: %s has no "+
					"sprint %d: %w", project, number, ErrSprintState)
			case current.Archived:
				return statelog.Decision{}, nil
			case current.State != SprintClosed:
				return statelog.Decision{}, fmt.Errorf("tracker: sprint %d of "+
					"%s is %s, and only a closed sprint is archived: %w",
					number, project, current.State, ErrSprintState)
			}
			next := current
			next.Archived = true
			next.UpdatedAt = at
			decision, err := w.decide(subject, OpPatch, scope, opID, next,
				nil, at)
			if err != nil {
				return statelog.Decision{}, err
			}
			decision.Version = int64(current.Version)
			return decision, nil
		},
	})
}
