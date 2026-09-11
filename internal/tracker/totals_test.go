package tracker_test

import (
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/tracker"
)

func totalOf(t *testing.T, answer tracker.Answer, key string) tracker.Total {
	t.Helper()
	for _, total := range answer.Totals {
		if total.Key == key {
			return total
		}
	}
	t.Fatalf("the answer carries no total %q, only %v", key, answer.Totals)
	return tracker.Total{}
}

// A TOTAL IS OVER THE WHOLE SET, never over the page.
//
// A total is what the question adds up to, and a page is fifty rows of it. One
// computed over the page changes as somebody scrolls, which is the one thing a
// number on a header must not do.
func TestATotalCountsTheWholeSetRatherThanThePage(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)

	for i, points := range []float64{1, 2, 3, 5, 8} {
		task := newTask("t-" + itoa(i))
		task.Points = points
		task.EstimateMinutes = (i + 1) * 30
		if _, err := r.writer.CreateTask(t.Context(), "op-"+itoa(i), task, nil); err != nil {
			t.Fatalf("CreateTask: %v", err)
		}
		r.drain()
	}

	// A PAGE OF TWO over a set of five: the totals must describe five.
	answer := r.ask(map[string]any{
		"container": "project:ENG", "limit": 2,
		"totals": "points:sum,estimate_min:sum,points:max,points:avg,tasks:count",
	})
	if len(answer.Rows) != 2 {
		t.Fatalf("the page is %d rows, want the 2 that were asked for",
			len(answer.Rows))
	}
	for key, want := range map[string]float64{
		"points:sum":       19,
		"estimate_min:sum": 450,
		"points:max":       8,
		"points:avg":       3.8,
		"tasks:count":      5,
	} {
		got := totalOf(t, answer, key)
		if got.Value == nil {
			t.Fatalf("%s came back absent, want %v", key, want)
		}
		if *got.Value != want {
			t.Fatalf("%s is %v over a 2-row page, want %v over the whole set "+
				"of 5", key, *got.Value, want)
		}
	}
}

// A TOTAL SHARES THE ANSWER'S OWN PREDICATE.
//
// A header that added up more than the board shows is a number nobody can
// reconcile with what is in front of them.
func TestATotalAddsUpOnlyWhatTheFilterMatched(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)

	for i, spec := range []struct {
		points float64
		status tracker.Status
	}{{1, tracker.StatusTodo}, {2, tracker.StatusTodo}, {100, tracker.StatusDone}} {
		task := newTask("t-" + itoa(i))
		task.Points = spec.points
		task.Status, task.StatusGroup = spec.status, spec.status.Group()
		if _, err := r.writer.CreateTask(t.Context(), "op-"+itoa(i), task, nil); err != nil {
			t.Fatalf("CreateTask: %v", err)
		}
		r.drain()
	}

	open := totalOf(t, r.ask(map[string]any{
		"container": "project:ENG", "status": "todo", "totals": "points:sum",
	}), "points:sum")
	if open.Value == nil || *open.Value != 3 {
		t.Fatalf("the filtered total is %v, want 3 — the done task's 100 must "+
			"not be in a sum the board does not show", open.Value)
	}
}

// NO ROW CONTRIBUTING IS ABSENT, NOT ZERO.
//
// "Nothing is estimated" and "everything is estimated at nothing" are
// different facts, and a header rendering the second for the first is how a
// sprint reads as free.
func TestATotalOverNothingIsAbsentRatherThanZero(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)

	// A task with no due date, which is the NULLABLE case: `estimate_min`
	// is NOT NULL DEFAULT 0 in the store, so "no estimate" and "estimated
	// at nothing" really are one value there — a fact about the schema
	// rather than about this aggregate.
	if _, err := r.writer.CreateTask(t.Context(), "op-1", newTask("t-1"), nil); err != nil {
		t.Fatalf("CreateTask: %v", err)
	}
	r.drain()

	got := totalOf(t, r.ask(map[string]any{
		"container": "project:ENG", "totals": "due_at:max",
	}), "due_at:max")
	if got.At != nil || got.Value != nil {
		t.Fatalf("a max over a column no row set answered %v/%v, want no "+
			"answer at all", got.At, got.Value)
	}

	// AND OVER AN EMPTY SET, which is the other way a total has nothing
	// to say: a filter that matched no row at all.
	empty := totalOf(t, r.ask(map[string]any{
		"container": "project:ENG", "status": "done", "totals": "points:sum",
	}), "points:sum")
	if empty.Value != nil {
		t.Fatalf("a sum over no rows answered %v, want no answer — zero would "+
			"read as work that adds up to nothing", *empty.Value)
	}

	// AND A COUNT IS STILL A NUMBER, because "none" is a count somebody
	// asked for rather than a value nothing produced.
	count := totalOf(t, r.ask(map[string]any{
		"container": "project:ENG", "status": "done", "totals": "tasks:count",
	}), "tasks:count")
	if count.Value == nil || *count.Value != 0 {
		t.Fatalf("a count over an empty set is %v, want 0", count.Value)
	}
}

// A DATE TOTAL ANSWERS AN INSTANT, and only min and max may be asked for one.
//
// A sum of dates is a number of microseconds since 1970 — a value nothing
// renders and nobody meant.
func TestADateTotalAnswersAnInstant(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)

	early := wednesday.Add(-48 * time.Hour)
	late := wednesday.Add(72 * time.Hour)
	for i, due := range []time.Time{early, late} {
		task := newTask("t-" + itoa(i))
		task.DueAt = &[]time.Time{due}[0]
		if _, err := r.writer.CreateTask(t.Context(), "op-"+itoa(i), task, nil); err != nil {
			t.Fatalf("CreateTask: %v", err)
		}
		r.drain()
	}

	answer := r.ask(map[string]any{
		"container": "project:ENG", "totals": "due_at:min,due_at:max",
	})
	first := totalOf(t, answer, "due_at:min")
	if first.At == nil || !first.At.Equal(early) {
		t.Fatalf("the earliest due is %v, want %v", first.At, early)
	}
	if first.Value != nil {
		t.Fatalf("a date total also answered a number %v, so a renderer has "+
			"two values for one fact", *first.Value)
	}
	last := totalOf(t, answer, "due_at:max")
	if last.At == nil || !last.At.Equal(late) {
		t.Fatalf("the latest due is %v, want %v", last.At, late)
	}
}

// A TOTAL NOTHING COULD MEAN IS REFUSED, naming what is wrong.
func TestATotalThatMeansNothingIsRefused(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	seedFields(t, r)

	for name, tc := range map[string]struct{ totals, want string }{
		"an op that is not one": {"points:median", "not an aggregate"},
		"a column with no meaning summed": {"status:sum",
			"not a column a total may be taken over"},
		"a sum of dates":           {"due_at:sum", "number of microseconds"},
		"a field nothing resolves": {"f.nonesuch:sum", "names no field"},
		"an aggregate over prose":  {"f.owner:sum", "number with no meaning"},
	} {
		t.Run(name, func(t *testing.T) {
			q, err := tracker.ParseQuery(tracker.MapParams(map[string]any{
				"container": "project:ENG", "totals": tc.totals,
			}), wednesday, berlin)
			if err != nil {
				// SOME ARE REFUSED AT THE PARSE and some at the
				// compile — what matters is that none is answered.
				if !strings.Contains(err.Error(), tc.want) {
					t.Fatalf("the refusal %q does not say %q", err, tc.want)
				}
				return
			}
			q.Level = statelog.ReadStale
			_, err = r.reader.Tasks(t.Context(), q, wednesday)
			if err == nil {
				t.Fatal("a total that means nothing was answered")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("the refusal %q does not say %q", err, tc.want)
			}
		})
	}
}

// A CUSTOM FIELD'S TOTAL READS ITS OWN TABLE.
//
// A declared field's values are rows keyed by field id, typed per column — so
// an aggregate over one is a correlated read rather than a column of the task.
func TestACustomFieldsTotalAddsUpItsValues(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	seedFields(t, r)

	seedWithFields(t, r, "t-1", map[string]any{"f-effort": 8})
	seedWithFields(t, r, "t-2", map[string]any{"f-effort": 2})
	seedWithFields(t, r, "t-3", nil)

	answer := r.ask(map[string]any{
		"container": "project:ENG",
		"totals":    "f.effort:sum,f.effort:max,f.effort:count",
	})
	for key, want := range map[string]float64{
		"f.effort:sum": 10, "f.effort:max": 8,
		// THREE TASKS AND TWO SET IT: a count over a field is how many
		// tasks have one, never how many value rows it produced.
		"f.effort:count": 2,
	} {
		got := totalOf(t, answer, key)
		if got.Value == nil || *got.Value != want {
			t.Fatalf("%s is %v, want %v", key, got.Value, want)
		}
	}
}
