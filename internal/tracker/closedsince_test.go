package tracker_test

import (
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/tracker"
)

// finished seeds one task that finished — done, or cancelled — at an instant.
func finished(h *readHarness, id string, status tracker.Status, at time.Time) {
	h.t.Helper()
	h.seed(id, func(task *tracker.Task) {
		task.Status, task.StatusGroup = status, status.Group()
		if status.Group() == tracker.GroupDone {
			task.DoneAt = &at
		} else {
			task.ClosedAt = &at
		}
	})
}

// askIn is [readHarness.ask] on a company whose clock is in another zone.
func askIn(h *readHarness, loc *time.Location, kv map[string]any) tracker.Answer {
	h.t.Helper()
	q, err := tracker.ParseQuery(tracker.MapParams(kv), wednesday, loc)
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
	finished(h, "done-this-week", tracker.StatusDone,
		time.Date(2031, 4, 15, 9, 0, 0, 0, time.UTC))
	finished(h, "cancelled-this-week", tracker.StatusCancelled,
		time.Date(2031, 4, 14, 8, 0, 0, 0, time.UTC))
	finished(h, "done-last-week", tracker.StatusDone,
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
	if !slices.Contains(columns, string(tracker.StatusDone)) {
		t.Errorf("a closed_since board draws %v and no Done column", columns)
	}

	// AND IT IS ONE ANSWER TO ONE QUESTION: beside show_closed it is
	// refused naming both, never resolved by whichever was read last.
	_, err := tracker.ParseQuery(tracker.MapParams{
		"container": "project:ENG", "closed_since": "sow", "show_closed": "true",
	}, wednesday, berlin)
	if err == nil || !strings.Contains(err.Error(), "show_closed") {
		t.Errorf("closed_since beside show_closed parsed (err %v), want a "+
			"refusal naming the pair", err)
	}
	// And a branch of a disjunction may not carry it: which finished work
	// is in the answer is the answer's shape, not one arm's predicate.
	_, err = tracker.ParseQuery(tracker.MapParams{
		"container": "project:ENG", "any": `[{"closed_since":"sow"},{"assignee":"a"}]`,
	}, wednesday, berlin)
	if err == nil || !strings.Contains(err.Error(), "closed_since") {
		t.Errorf("an any branch carrying closed_since parsed (err %v)", err)
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
	finished(h, "late-sunday-utc", tracker.StatusDone,
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
