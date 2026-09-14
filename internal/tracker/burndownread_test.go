package tracker_test

import (
	"context"
	"database/sql"
	"errors"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/statelog/statelogtest"
	"github.com/crewlet/crewlet/internal/tracker"
)

// The burndown read, end to end over applied records.
//
// `burndown_test.go` covers the ARITHMETIC over values — every shape a sprint
// has, at instants a fixture cannot reach, because the applier stamps a
// record's effective instant from the broker and every record this harness
// writes therefore lands at roughly now. What is left for this file is what
// only real rows can answer: that the two statements read the rows they are
// meant to, and that the two not-found states are distinguishable.

func (r *roundTrip) burndown(q tracker.BurndownQuery, now time.Time) tracker.Burndown {
	r.t.Helper()
	if q.Level == "" {
		q.Level = statelog.ReadStale
	}
	got, err := r.reader.Burndown(r.t.Context(), q, now)
	if err != nil {
		r.t.Fatalf("Burndown(%+v): %v", q, err)
	}
	return got
}

// THE SERIES IS OVER THE SPRINT'S OWN MEMBERS, in the project's own measure.
func TestABurndownScoresTheSprintsOwnMembers(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	base := time.Now().UTC().Truncate(time.Microsecond)
	// A WINDOW THIS TEST'S OWN RECORDS FALL INSIDE — every instant here
	// comes from the broker, so a window has to be placed around now.
	seedSprintWindow(t, r, 1, tracker.SprintActive, base.Add(-2*time.Hour),
		base.Add(12*24*time.Hour), nil)
	one := 1
	pointedTask(t, r, "a", 3, &one, "ada")
	pointedTask(t, r, "b", 5, &one, "bo")
	// AND ONE TASK OUTSIDE THE SPRINT, which must contribute nothing: a
	// read that scoped by project rather than by membership would carry
	// the whole backlog into every sprint's chart.
	pointedTask(t, r, "loose", 21, nil, "ada")

	// READ AT THE PRESENT INSTANT, not at `base`. The series stops at the
	// caller's own clock, and `base` was taken before these records were
	// written — so reading at it would honestly report a sprint that was
	// still empty, which is the behaviour rather than the bug.
	got := r.burndown(tracker.BurndownQuery{Project: "ENG", Sprint: 1},
		time.Now().UTC())
	if got.Measure != tracker.MeasurePoints {
		t.Errorf("the series is in %q, want points — a bare number is points "+
			"to one team and minutes to another", got.Measure)
	}
	if got.Tasks != 2 {
		t.Errorf("the sprint holds %d tasks, want the 2 that are in it", got.Tasks)
	}
	if len(got.Points) == 0 {
		t.Fatal("the series is empty — a sprint that has started has at least " +
			"its own opening point")
	}
	last := got.Points[len(got.Points)-1]
	if last.Scope != 8 || last.Remaining != 8 {
		t.Errorf("the sprint reads %+v, want 8 of 8 still to do — the loose "+
			"task's 21 points are not this sprint's", last)
	}
	if got.Ideal != got.Points[0].Scope {
		t.Errorf("the reference height is %v, want the opening scope %v",
			got.Ideal, got.Points[0].Scope)
	}
}

// DELIVERY REACHES THE SERIES THROUGH THE SPANS, and the spans are reached
// through the MEMBERSHIP rather than through their own sprint column.
//
// A span carries the task's sprint as of the recompute that WROTE it — the
// applier stamps `t.sprint_number` at insert and rewrites a task's whole span
// set on every history row — so a read filtered on that column loses exactly
// the carried-over work a burndown most needs to show. This case is the flat
// version of that: the task's spans are written before and after it is scored,
// and the series has to see them either way.
func TestABurndownSeesADeliveryMadeAfterTheTaskWasFiled(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	base := time.Now().UTC().Truncate(time.Microsecond)
	seedSprintWindow(t, r, 1, tracker.SprintActive, base.Add(-2*time.Hour),
		base.Add(12*24*time.Hour), nil)
	one := 1
	pointedTask(t, r, "ship", 5, &one, "ada")
	pointedTask(t, r, "hold", 3, &one, "bo")

	done := tracker.StatusDone
	if _, err := r.writer.UpdateTask(t.Context(), "op-ship", "ship", "ENG",
		tracker.NoIfMatch, tracker.TaskPatch{Status: &done},
		tracker.ChangeStatus, nil); err != nil {
		t.Fatalf("deliver the task: %v", err)
	}
	r.drain()

	got := r.burndown(tracker.BurndownQuery{Project: "ENG", Sprint: 1},
		time.Now().UTC())
	last := got.Points[len(got.Points)-1]
	if last.Delivered != 5 {
		t.Errorf("delivered is %v, want the 5 points that shipped", last.Delivered)
	}
	if last.Remaining != 3 {
		t.Errorf("remaining is %v, want the 3 points still to do", last.Remaining)
	}
	if last.Scope != 8 {
		t.Errorf("scope is %v, want 8 — delivering work does not take it out "+
			"of the sprint", last.Scope)
	}
}

// UNESTIMATED WORK IS COUNTED AND SAID, never folded in silently: a series
// over a sprint half of whose tasks carry no points is a series about half a
// sprint, and a reader who cannot see that quotes the number.
func TestABurndownNamesTheWorkItCannotMeasure(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	base := time.Now().UTC().Truncate(time.Microsecond)
	seedSprintWindow(t, r, 1, tracker.SprintActive, base.Add(-2*time.Hour),
		base.Add(12*24*time.Hour), nil)
	one := 1
	pointedTask(t, r, "sized", 8, &one, "ada")
	pointedTask(t, r, "unsized", 0, &one, "bo")

	got := r.burndown(tracker.BurndownQuery{Project: "ENG", Sprint: 1},
		time.Now().UTC())
	if got.Tasks != 2 || got.Unestimated != 1 {
		t.Errorf("the sprint reports %d tasks and %d unestimated, want 2 and 1",
			got.Tasks, got.Unestimated)
	}
}

// THE TWO NOT-FOUND STATES ARE DIFFERENT REPAIRS, so they are different
// sentinels: a mistyped key is a key, and a number nobody has minted is a
// sprint that does not exist yet.
func TestABurndownDistinguishesAnUnknownProjectFromAnUnknownSprint(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	base := time.Now().UTC().Truncate(time.Microsecond)
	seedSprintWindow(t, r, 1, tracker.SprintActive, base.Add(-2*time.Hour),
		base.Add(12*24*time.Hour), nil)

	_, err := r.reader.Burndown(t.Context(), tracker.BurndownQuery{
		Project: "NOPE", Sprint: 1, Level: statelog.ReadStale}, base)
	if !errors.Is(err, tracker.ErrNoProject) {
		t.Errorf("an unknown project answered %v, want ErrNoProject", err)
	}

	_, err = r.reader.Burndown(t.Context(), tracker.BurndownQuery{
		Project: "ENG", Sprint: 99, Level: statelog.ReadStale}, base)
	if !errors.Is(err, tracker.ErrNoSprint) {
		t.Errorf("an unminted sprint answered %v, want ErrNoSprint — an empty "+
			"series would read as a sprint in which nothing happened", err)
	}
}

// retryOnce is a read seam that runs the closure TWICE, as the store does when
// a read transaction loses its snapshot to a writer and is retried on a fresh
// one ([store.txAttempts] is eight, so twice is the mildest real case).
//
// It exists because the hazard it exercises is invisible to every other test
// in this package: the rows, the window and the arithmetic are all correct on
// each attempt, and what goes wrong is only what the closure carried OUT of
// the attempt before.
type retryOnce struct{ inner statelogtest.DB }

func (d retryOnce) Read(ctx context.Context, fn func(*sql.Tx) error) error {
	if err := d.inner.Read(ctx, fn); err != nil {
		return err
	}
	return d.inner.Read(ctx, fn)
}

// A RETRIED READ ANSWERS WHAT ONE ATTEMPT WOULD.
//
// The read closure is not run once. `Unestimated` was COUNTED into a value
// captured from outside it — `out.Unestimated++` — so a sprint with two
// unestimated tasks reported four after one retry and six after two, while
// every other figure stayed correct. That is the worst shape this answer can
// fail in: `Unestimated` is the number a reader consults to decide how much of
// the series to believe, so an inflated one discredits a series that is right.
//
// `Ideal` is the same hazard in the other direction — assigned only when the
// series has points, so an attempt finding none kept the previous attempt's
// height — which is why the fix builds a whole answer per attempt rather than
// guarding the one counter.
func TestARetriedBurndownReadCountsEachTaskOnce(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	base := time.Now().UTC().Truncate(time.Microsecond)
	seedSprintWindow(t, r, 1, tracker.SprintActive, base.Add(-2*time.Hour),
		base.Add(12*24*time.Hour), nil)
	one := 1
	// ONE ESTIMATED MEMBER AND TWO WITHOUT, so the count is a number a
	// second attempt can visibly double rather than a zero it cannot.
	pointedTask(t, r, "a", 3, &one, "ada")
	pointedTask(t, r, "b", 0, &one, "bo")
	pointedTask(t, r, "c", 0, &one, "cy")

	// READ AT THE PRESENT INSTANT, not at `base`: the series stops at the
	// caller's clock, and `base` was taken before these records existed.
	now := time.Now().UTC()
	once := r.burndown(tracker.BurndownQuery{Project: "ENG", Sprint: 1}, now)

	// The same rows, read through a seam that retries exactly once.
	retried, err := statelogtest.LocalReader(tracker.Domain{},
		retryOnce{inner: r.db.Replicated()},
		statelog.Position{Stream: tracker.Domain{}.Stream().Name, Generation: 1})
	if err != nil {
		t.Fatalf("local read authority: %v", err)
	}
	reader, err := tracker.NewReader(r.db, retried)
	if err != nil {
		t.Fatalf("tracker reader: %v", err)
	}
	got, err := reader.Burndown(t.Context(),
		tracker.BurndownQuery{Project: "ENG", Sprint: 1, Level: statelog.ReadStale}, now)
	if err != nil {
		t.Fatalf("Burndown through a retry: %v", err)
	}

	if once.Unestimated != 2 {
		t.Fatalf("a single attempt counted %d unestimated, want 2 — the "+
			"fixture is wrong, so this case proves nothing", once.Unestimated)
	}
	if got.Unestimated != once.Unestimated {
		t.Errorf("unestimated = %d through one retry, want %d: the closure "+
			"counted into a value captured from outside it, so every retry "+
			"added the whole sprint again", got.Unestimated, once.Unestimated)
	}
	if got.Tasks != once.Tasks {
		t.Errorf("tasks = %d through one retry, want %d", got.Tasks, once.Tasks)
	}
	if got.Ideal != once.Ideal {
		t.Errorf("ideal = %v through one retry, want %v", got.Ideal, once.Ideal)
	}
}
