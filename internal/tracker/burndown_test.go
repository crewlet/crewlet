package tracker

import (
	"database/sql"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/store"
)

// The burndown's arithmetic, over values.
//
// PURE, and tested without a database for the reason `textindex`'s ranking and
// `coerce.go`'s table are: a rule that can only be exercised through a store is
// a rule nobody re-measures, and every case below is a shape a real sprint has
// — a task that arrived late, one that was abandoned, one carried out — that
// would take a fixture of applied records to reach through SQL and would then
// be testing the applier rather than the reading.

func at(day int) time.Time {
	return time.Date(2031, 4, 14, 9, 0, 0, 0, time.UTC).AddDate(0, 0, day)
}

func stamp(day int) int64 { return store.EncodeTime(at(day)) }

func open(from int) stay { return stay{from: stamp(from)} }

func closedStay(from, to int) stay {
	return stay{from: stamp(from), to: sql.NullInt64{Int64: stamp(to), Valid: true}}
}

func inStatus(status Status, from int) span {
	return span{status: status, group: status.Group(), entered: stamp(from)}
}

func inStatusUntil(status Status, from, to int) span {
	s := inStatus(status, from)
	s.left = sql.NullInt64{Int64: stamp(to), Valid: true}
	return s
}

// THE SERIES STOPS AT TODAY, not at the sprint's planned end.
//
// A running sprint drawn to its end is a flat line into its own future, which
// reads as a team that has stopped working — the one shape a burndown must not
// invent.
func TestARunningSprintsSeriesStopsAtTheCallersOwnInstant(t *testing.T) {
	t.Parallel()
	got := burndownInstants(at(0), at(14), sql.NullInt64{}, at(3).Add(4*time.Hour))
	if len(got) != 5 {
		t.Fatalf("the series has %d points, want 5 — day 0 through day 3 and "+
			"the caller's own instant", len(got))
	}
	if !got[0].Equal(at(0)) {
		t.Errorf("the series opens at %v, want the sprint's start %v", got[0], at(0))
	}
	// THE LAST POINT IS THE INSTANT ITSELF, not the day boundary below it:
	// a sprint read at four in the afternoon has burned down whatever
	// landed since nine, and a series that stopped at nine would hide it.
	if want := at(3).Add(4 * time.Hour); !got[len(got)-1].Equal(want) {
		t.Errorf("the series ends at %v, want the caller's instant %v",
			got[len(got)-1], want)
	}
}

// A CLOSED SPRINT ENDS WHEN IT CLOSED. Its planned end is in the future of its
// own history, and drawing to it appends days in which, by construction,
// nothing could have happened.
func TestAClosedSprintsSeriesEndsAtItsClose(t *testing.T) {
	t.Parallel()
	closed := sql.NullInt64{Int64: stamp(5), Valid: true}
	got := burndownInstants(at(0), at(14), closed, at(40))
	if want := at(5); !got[len(got)-1].Equal(want) {
		t.Fatalf("the series ends at %v, want the close %v", got[len(got)-1], want)
	}
	if len(got) != 6 {
		t.Errorf("the series has %d points, want 6 — day 0 through the close", len(got))
	}
}

// A SPRINT THAT HAS NOT STARTED DRAWS ONE POINT rather than none: "there is
// nothing to draw yet" and "this read could not be answered" are different
// states, and a caller tells them apart by the point count.
func TestAFutureSprintDrawsItsStartAndNothingElse(t *testing.T) {
	t.Parallel()
	got := burndownInstants(at(10), at(24), sql.NullInt64{}, at(3))
	if len(got) != 1 || !got[0].Equal(at(10)) {
		t.Fatalf("a future sprint draws %v, want its start alone", got)
	}
}

// MEMBERSHIP IS HALF-OPEN, like every other interval in this package: a task
// that left at exactly this instant is not in the sprint at it, which is what
// keeps a hand-off between two sprints from counting in both.
func TestAStayIsHalfOpen(t *testing.T) {
	t.Parallel()
	stays := []stay{closedStay(0, 4)}
	if !inSprintAt(stays, stamp(0)) {
		t.Error("the task is out of the sprint at the instant it joined")
	}
	if !inSprintAt(stays, stamp(3)) {
		t.Error("the task is out of the sprint in the middle of its stay")
	}
	if inSprintAt(stays, stamp(4)) {
		t.Error("the task is still in the sprint at the instant it left")
	}
}

// A TASK WITH NO SPAN COVERING AN INSTANT HAS NO STATUS THERE, and the reader
// says so rather than answering `todo`.
//
// The third value is not decoration: a task created after the instant, or one
// whose spans this build could not decode, holds no status at it, and
// inventing one is the difference between reporting a fact and making one up.
func TestAnInstantBeforeTheFirstSpanHasNoStatus(t *testing.T) {
	t.Parallel()
	spans := []span{inStatus(StatusTodo, 2)}
	if _, _, known := statusAt(spans, stamp(1)); known {
		t.Error("a task reports a status before its first span")
	}
	status, group, known := statusAt(spans, stamp(2))
	if !known || status != StatusTodo || group != GroupNotStarted {
		t.Errorf("at its first span the task is %q/%q (known %v), want todo",
			status, group, known)
	}
}

// ABANDONED WORK LEAVES THE REMAINING LINE AND JOINS NEITHER DELIVERY NOR IT.
//
// THIS IS THE CASE THE WHOLE SHAPE TURNS ON. `cancelled` is a FINISHED group
// that is not [Delivered] — which is what keeps it out of velocity — so a
// remaining line written as "not delivered" would keep counting work the team
// deliberately dropped, and a descoped sprint would run flat to its end while
// everyone involved knew the work was gone. Written as "an open group", the
// line falls with no matching rise in delivery, which is exactly the shape
// descoping should have.
func TestCancelledWorkLeavesRemainingWithoutBeingDelivered(t *testing.T) {
	t.Parallel()
	tasks := map[string]*burnTask{
		"done": {
			measure: 5,
			stays:   []stay{open(0)},
			spans: []span{
				inStatusUntil(StatusInProgress, 0, 2),
				inStatus(StatusDone, 2),
			},
		},
		"dropped": {
			measure: 3,
			stays:   []stay{open(0)},
			spans: []span{
				inStatusUntil(StatusTodo, 0, 2),
				inStatus(StatusCancelled, 2),
			},
		},
	}
	got := burndownSeries(tasks, []time.Time{at(0), at(3)})

	before, after := got[0], got[1]
	if before.Scope != 8 || before.Remaining != 8 || before.Delivered != 0 {
		t.Fatalf("before anything moved the sprint reads %+v, want 8 of 8 "+
			"remaining and nothing delivered", before)
	}
	if after.Scope != 8 {
		t.Errorf("scope after is %v, want 8 — neither task left the sprint",
			after.Scope)
	}
	if after.Remaining != 0 {
		t.Errorf("remaining after is %v, want 0 — one task was delivered and "+
			"the other was abandoned, and neither is still to do", after.Remaining)
	}
	// AND THE ABANDONED WORK IS NOT DELIVERY. A reader that counted the
	// finished GROUP here would report five points of velocity as eight.
	if after.Delivered != 5 {
		t.Errorf("delivered after is %v, want 5 — the cancelled three points "+
			"were abandoned rather than delivered", after.Delivered)
	}
}

// SCOPE IS THE SECOND LINE, and without it the first one lies: a burndown
// drawn alone cannot tell "we finished eight points" from "somebody added
// eight and we finished sixteen".
func TestWorkThatArrivesLateStepsScopeUp(t *testing.T) {
	t.Parallel()
	tasks := map[string]*burnTask{
		"early": {measure: 5, stays: []stay{open(0)}, spans: []span{inStatus(StatusTodo, 0)}},
		"late":  {measure: 8, stays: []stay{open(2)}, spans: []span{inStatus(StatusTodo, 2)}},
	}
	got := burndownSeries(tasks, []time.Time{at(0), at(3)})
	if got[0].Scope != 5 {
		t.Errorf("scope at the start is %v, want the 5 points that were in the "+
			"sprint then", got[0].Scope)
	}
	if got[1].Scope != 13 || got[1].Remaining != 13 {
		t.Errorf("after the arrival the sprint reads %+v, want 13 of 13 — a "+
			"burndown that held scope flat would report the team as having "+
			"fallen behind rather than as having been given more", got[1])
	}
}

// WORK CARRIED OUT OF THE SPRINT LEAVES IT, and the scope falls: a rollover
// moves the task to the next sprint, and counting it here for ever would make
// every carried-over point permanent debt on the sprint it started in.
func TestWorkCarriedOutLeavesTheScope(t *testing.T) {
	t.Parallel()
	tasks := map[string]*burnTask{
		"carried": {
			measure: 4,
			stays:   []stay{closedStay(0, 3)},
			spans:   []span{inStatus(StatusTodo, 0)},
		},
	}
	got := burndownSeries(tasks, []time.Time{at(0), at(4)})
	if got[0].Scope != 4 {
		t.Errorf("scope while the task was in the sprint is %v, want 4", got[0].Scope)
	}
	if got[1].Scope != 0 || got[1].Remaining != 0 {
		t.Errorf("after the carry-over the sprint reads %+v, want nothing — "+
			"the task is the next sprint's now", got[1])
	}
}

// A TASK WHOSE OWN HISTORY THIS BUILD CANNOT READ COUNTS AS REMAINING.
//
// The membership row says it is in the sprint, and the honest reading of a gap
// in its spans is "still to do": the alternative silently burns work down for
// a record nobody could decode, which is the one direction that flatters.
func TestATaskWithNoSpansCountsAsStillToDo(t *testing.T) {
	t.Parallel()
	tasks := map[string]*burnTask{
		"opaque": {measure: 7, stays: []stay{open(0)}},
	}
	got := burndownSeries(tasks, []time.Time{at(1)})
	if got[0].Remaining != 7 || got[0].Delivered != 0 {
		t.Fatalf("a task with no readable history reads %+v, want its 7 points "+
			"as still to do", got[0])
	}
}
