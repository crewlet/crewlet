package worklet_test

import (
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/worklet"
)

// finished seeds one task that finished — done, or cancelled — at an instant.
func finished(h *readHarness, id string, status worklet.Status, at time.Time) {
	h.t.Helper()
	h.seed(id, func(task *worklet.Task) {
		task.Status, task.StatusGroup = status, status.Group()
		if status.Group() == worklet.GroupDone {
			task.DoneAt = &at
		} else {
			task.ClosedAt = &at
		}
	})
}

// askIn is [readHarness.ask] on a company whose clock is in another zone.
func askIn(h *readHarness, loc *time.Location, kv map[string]any) worklet.Answer {
	h.t.Helper()
	q, err := worklet.ParseQuery(worklet.MapParams(kv), wednesday, loc)
	if err != nil {
		h.t.Fatalf("ParseQuery(%v): %v", kv, err)
	}
	q.Level = statelog.ReadStale
	answer, err := h.reader.Tasks(h.t.Context(), q, wednesday)
	if err != nil {
		h.t.Fatalf("Tasks(%v): %v", kv, err)
	}
	return answer
}

// THE BOARD'S RECENT SCOPE IS THE OPEN WORK AND WHAT FINISHED SINCE A DATE.
//
// `closed_since=sow` is a Done lane that holds this week's work and empties
// itself on Monday — and it is the whole finished set that is bounded, so a
// task cancelled this week is on the board exactly as one done this week is,
// while last week's is not. Open work is never bounded by it: an open task has
// not finished, so no date about finishing can exclude it.
func TestClosedSinceIncludesOpenAndRecentlyFinished(t *testing.T) {
	t.Parallel()
	h := newReadHarness(t)
	h.seed("open", nil)
	// Wednesday 16 April 2031 is "now"; the week began on Monday the 14th.
	finished(h, "done-this-week", worklet.StatusDone,
		time.Date(2031, 4, 15, 9, 0, 0, 0, time.UTC))
	finished(h, "cancelled-this-week", worklet.StatusCancelled,
		time.Date(2031, 4, 14, 8, 0, 0, 0, time.UTC))
	finished(h, "done-last-week", worklet.StatusDone,
		time.Date(2031, 4, 10, 9, 0, 0, 0, time.UTC))

	got := ids(h.ask(map[string]any{
		"container": "project:ENG", "closed_since": "sow",
	}))
	slices.Sort(got)
	want := []string{"cancelled-this-week", "done-this-week", "open"}
	if !slices.Equal(got, want) {
		t.Fatalf("closed_since=sow answers %v, want %v — the open work plus "+
			"everything that finished since Monday, cancelled or done", got, want)
	}

	// AN ABSOLUTE DATE IS A DATE TOKEN TOO, and the bound is inclusive: the
	// Done lane of "since the 10th" holds what finished on the 10th.
	got = ids(h.ask(map[string]any{
		"container": "project:ENG", "closed_since": "2031-04-10",
	}))
	if len(got) != 4 {
		t.Errorf("closed_since=2031-04-10 answers %v, want all four — the "+
			"bound is at or after the day's first instant", got)
	}

	// A BOARD GROUPED BY STATUS DRAWS THE FINISHED COLUMNS, because the
	// query now admits finished work — an open-work board never draws
	// Done, and a Recent board that did not would hide the lane the scope
	// exists for.
	board := h.ask(map[string]any{
		"container": "project:ENG", "closed_since": "sow", "group_by": "status",
	})
	var columns []string
	for _, group := range board.Groups {
		columns = append(columns, group.Key)
	}
	if !slices.Contains(columns, string(worklet.StatusDone)) {
		t.Errorf("a closed_since board draws %v and no Done column", columns)
	}

	// AND IT IS ONE ANSWER TO ONE QUESTION: beside show_closed it is
	// refused naming both, never resolved by whichever was read last.
	_, err := worklet.ParseQuery(worklet.MapParams{
		"container": "project:ENG", "closed_since": "sow", "show_closed": "true",
	}, wednesday, berlin)
	if err == nil || !strings.Contains(err.Error(), "show_closed") {
		t.Errorf("closed_since beside show_closed parsed (err %v), want a "+
			"refusal naming the pair", err)
	}
	// And a branch of a disjunction may not carry it: which finished work
	// is in the answer is the answer's shape, not one arm's predicate.
	_, err = worklet.ParseQuery(worklet.MapParams{
		"container": "project:ENG", "any": `[{"closed_since":"sow"},{"assignee":"a"}]`,
	}, wednesday, berlin)
	if err == nil || !strings.Contains(err.Error(), "closed_since") {
		t.Errorf("an any branch carrying closed_since parsed (err %v)", err)
	}
}

// A BOUND AT THE ZERO INSTANT IS NO BOUND, AND IS REFUSED NAMING THE ONE THAT
// SAYS IT. `closed_since` at January of year one is every finished task, which
// `show_closed=true` already asks — and a query holding the zero instant as its
// bound could not cross the estate's wire as itself, since the wire leaves a
// value its type calls zero off: read through a node without data, it arrived
// as no bound at all and left every finished task out. So the parse refuses it,
// and a bound it takes is never the zero instant.
//
// Mutation: drop the parse's refusal of the zero instant and the query parses,
// holding no bound.
func TestAClosedSinceAtTheZeroInstantIsRefused(t *testing.T) {
	t.Parallel()
	_, err := worklet.ParseQuery(worklet.MapParams{
		"container": "project:ENG", "closed_since": "0001-01-01T00:00:00Z",
	}, wednesday, berlin)
	if err == nil || !strings.Contains(err.Error(), "show_closed=true") {
		t.Errorf("closed_since at the zero instant parsed (err %v), want a refusal naming "+
			"show_closed=true", err)
	}
	q, err := worklet.ParseQuery(worklet.MapParams{
		"container": "project:ENG", "closed_since": "2031-04-10",
	}, wednesday, berlin)
	if err != nil || q.ShowClosed.Since.IsZero() || !q.ShowClosed.Finished() {
		t.Errorf("closed_since=2031-04-10 parsed as (%+v, %v), want a bound that admits finished work",
			q.ShowClosed, err)
	}
}

// THE WEEK BEGINS AT THE COMPANY'S MIDNIGHT, NOT UTC'S.
//
// A task finished at 23:30 UTC on Sunday the 13th finished at 01:30 on MONDAY
// in Berlin. For a Berlin company that is this week's work and on the Recent
// board; for a company on UTC it is last week's and off it. A bound resolved on
// any clock but the company's own would move a task between two weeks for the
// two hours a year nobody checks — and disagree with `due=range:sow..eow`,
// which is cut on the company's calendar.
func TestClosedSinceTokenResolvesInCompanyZone(t *testing.T) {
	t.Parallel()
	h := newReadHarness(t)
	finished(h, "late-sunday-utc", worklet.StatusDone,
		time.Date(2031, 4, 13, 23, 30, 0, 0, time.UTC))

	scope := map[string]any{"container": "project:ENG", "closed_since": "sow"}
	if got := ids(askIn(h, berlin, scope)); !slices.Equal(got,
		[]string{"late-sunday-utc"}) {
		t.Errorf("for a Berlin company closed_since=sow answers %v, want the "+
			"task that finished at 01:30 on Monday, Berlin time", got)
	}
	if got := ids(askIn(h, time.UTC, scope)); len(got) != 0 {
		t.Errorf("for a UTC company closed_since=sow answers %v, want nothing "+
			"— it finished on Sunday there", got)
	}
}
