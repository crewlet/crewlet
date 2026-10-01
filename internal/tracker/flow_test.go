package tracker_test

import (
	"database/sql"
	"errors"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/period"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/store"
	"github.com/crewlet/crewlet/internal/tracker"
)

// losAngeles is a zone far enough from UTC that a day or a week cut in UTC
// lands on the wrong side of every boundary these cases put a change near.
func losAngeles(t *testing.T) *time.Location {
	t.Helper()
	loc, err := time.LoadLocation("America/Los_Angeles")
	if err != nil {
		t.Fatalf("load the zone: %v", err)
	}
	return loc
}

// flowClock stamps the fleet-agreed instant of every history row a case's
// last write produced. The harness's broker stamps the real clock; a series
// is about WHEN changes took effect, so each case says when.
type flowClock struct {
	r    *roundTrip
	seen int64
}

func (c *flowClock) at(when time.Time) {
	c.r.t.Helper()
	c.r.drain()
	if err := c.r.db.Replicated().Tx(c.r.t.Context(), func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(c.r.t.Context(),
			`UPDATE tracker_history SET effective_at = ? WHERE log_seq > ?`,
			store.EncodeTime(when), c.seen); err != nil {
			return err
		}
		return tx.QueryRowContext(c.r.t.Context(),
			`SELECT COALESCE(MAX(log_seq), 0) FROM tracker_history`).Scan(&c.seen)
	}); err != nil {
		c.r.t.Fatalf("stamp the history at %v: %v", when, err)
	}
}

func (r *roundTrip) flow(q tracker.FlowQuery, now time.Time, loc *time.Location) tracker.FlowAnswer {
	r.t.Helper()
	if q.Level == "" {
		q.Level = statelog.ReadStale
	}
	answer, err := r.reader.Flow(r.t.Context(), q, now, loc)
	if err != nil {
		r.t.Fatalf("Flow: %v", err)
	}
	return answer
}

func (r *roundTrip) setStatus(id string, status tracker.Status) {
	r.t.Helper()
	if _, err := r.writer.UpdateTask(r.t.Context(), "op-"+id+"-"+string(status), id,
		"ENG", tracker.NoIfMatch, tracker.TaskPatch{Status: &status},
		tracker.ChangeStatus, nil); err != nil {
		r.t.Fatalf("move %s to %s: %v", id, status, err)
	}
	r.drain()
}

func (r *roundTrip) file(id string) {
	r.t.Helper()
	task := newTask(id)
	task.Key = "ENG-" + id
	if _, err := r.writer.CreateTask(r.t.Context(), "op-"+id, task, nil); err != nil {
		r.t.Fatalf("create %s: %v", id, err)
	}
	r.drain()
}

func wantPoint(t *testing.T, got tracker.FlowPoint, label string,
	notStarted, active, done, completed int) {

	t.Helper()
	if got.Window != label || got.NotStarted != notStarted || got.Active != active ||
		got.Done != done || got.Completed != completed {
		t.Errorf("point %s = %+v, want window %s with not_started %d, active %d, "+
			"done %d and %d completed", got.Window, got, label, notStarted, active,
			done, completed)
	}
}

// THREE WEEKS, REPLAYED BACKWARD FROM TODAY, ON THE COMPANY'S CLOCK.
//
// Every change a count depends on is here: a create, a start, a delivery, a
// removal. Each week's census is what the board held at that week's last
// instant, and each completion lands in the week it took effect in — in the
// company's zone, which is seven hours from the UTC a naive cut would use.
func TestTheFlowReplaysThreeWeeksOnTheCompanyClock(t *testing.T) {
	t.Parallel()
	loc := losAngeles(t)
	r := newRoundTrip(t)
	clock := &flowClock{r: r}
	clock.at(time.Date(2031, 3, 20, 9, 0, 0, 0, loc)) // the project, long before

	// WEEK 14: two tasks filed.
	r.file("a")
	r.file("b")
	clock.at(time.Date(2031, 4, 1, 10, 0, 0, 0, loc))
	// WEEK 15: a started, b delivered, c filed.
	r.setStatus("a", tracker.StatusInProgress)
	r.setStatus("b", tracker.StatusDone)
	r.file("c")
	clock.at(time.Date(2031, 4, 8, 10, 0, 0, 0, loc))
	// WEEK 16 — the one now falls in: a delivered, c removed.
	r.setStatus("a", tracker.StatusDone)
	if _, err := r.writer.RemoveTask(t.Context(), "op-rm-c", "c", "ENG", false, nil); err != nil {
		t.Fatalf("remove c: %v", err)
	}
	r.drain()
	clock.at(time.Date(2031, 4, 14, 10, 0, 0, 0, loc))

	now := time.Date(2031, 4, 16, 7, 30, 0, 0, loc)
	answer := r.flow(tracker.FlowQuery{Bucket: period.Week, Points: 3}, now, loc)
	if len(answer.Points) != 3 {
		t.Fatalf("the series has %d points, want 3", len(answer.Points))
	}
	wantPoint(t, answer.Points[0], "2031-W14", 2, 0, 0, 0)
	wantPoint(t, answer.Points[1], "2031-W15", 1, 1, 1, 1)
	wantPoint(t, answer.Points[2], "2031-W16", 0, 0, 2, 1)
	if answer.BlockedHistory {
		t.Error("the answer claims a blocked history it cannot replay")
	}
	if answer.Now.NotStarted != 0 || answer.Now.Active != 0 {
		t.Errorf("now = %+v, want nothing open", answer.Now)
	}
}

// A DELIVERY AT 23:59 LOCAL IS THAT DAY'S, not the next — which is where a UTC
// cut would put it, seven hours into tomorrow.
func TestACompletionAtOneMinuteToMidnightIsThatDays(t *testing.T) {
	t.Parallel()
	loc := losAngeles(t)
	r := newRoundTrip(t)
	clock := &flowClock{r: r}
	r.file("a")
	clock.at(time.Date(2031, 4, 13, 9, 0, 0, 0, loc))
	r.setStatus("a", tracker.StatusDone)
	clock.at(time.Date(2031, 4, 14, 23, 59, 0, 0, loc))

	now := time.Date(2031, 4, 16, 7, 30, 0, 0, loc)
	answer := r.flow(tracker.FlowQuery{Bucket: period.Day, Points: 4}, now, loc)
	wantPoint(t, answer.Points[0], "2031-04-13", 1, 0, 0, 0)
	wantPoint(t, answer.Points[1], "2031-04-14", 0, 0, 1, 1)
	wantPoint(t, answer.Points[2], "2031-04-15", 0, 0, 1, 0)
	wantPoint(t, answer.Points[3], "2031-04-16", 0, 0, 1, 0)
}

// A CANCELLATION AND A CLOSE ARE NOT COMPLETIONS, and a purge takes a task out
// of the census only from the instant it happened.
func TestOnlyADeliveryCompletesAndAPurgeIsUndoneFromItsHistory(t *testing.T) {
	t.Parallel()
	loc := time.UTC
	r := newRoundTrip(t)
	clock := &flowClock{r: r}
	r.file("a")
	r.file("b")
	r.file("gone")
	clock.at(time.Date(2031, 4, 14, 9, 0, 0, 0, loc))
	r.setStatus("a", tracker.StatusCancelled) // finished, not delivered
	r.setStatus("b", tracker.StatusDone)
	r.setStatus("gone", tracker.StatusInProgress)
	clock.at(time.Date(2031, 4, 15, 9, 0, 0, 0, loc))
	r.setStatus("b", tracker.StatusClosed) // already delivered
	operator := r.writer.As("founder", tracker.AuthorOperator, tracker.Provenance{})
	if _, err := operator.PurgeTask(t.Context(), "op-purge", "gone", "ENG", "test"); err != nil {
		t.Fatalf("purge: %v", err)
	}
	r.drain()
	clock.at(time.Date(2031, 4, 16, 9, 0, 0, 0, loc))

	answer := r.flow(tracker.FlowQuery{Bucket: period.Day, Points: 3},
		time.Date(2031, 4, 16, 12, 0, 0, 0, loc), loc)
	wantPoint(t, answer.Points[0], "2031-04-14", 3, 0, 0, 0)
	// a cancelled (done group, not delivered), b delivered: one completion.
	wantPoint(t, answer.Points[1], "2031-04-15", 0, 1, 2, 1)
	// b closed and the purged task gone: nothing completed.
	wantPoint(t, answer.Points[2], "2031-04-16", 0, 0, 1, 0)
	if answer.Points[2].Closed != 1 {
		t.Errorf("today's closed count is %d, want b", answer.Points[2].Closed)
	}
}

// OVERDUE IS CUT AT THE COMPANY'S MIDNIGHT, the cut every board's mark uses.
func TestTheFlowsOverdueIsCutAtTheCompanysMidnight(t *testing.T) {
	t.Parallel()
	loc := losAngeles(t)
	r := newRoundTrip(t)
	now := time.Date(2031, 4, 16, 7, 30, 0, 0, loc)
	// DUE LATE YESTERDAY LOCAL — after UTC's midnight, so a UTC cut would
	// not call it overdue — and due early today, which is not.
	for id, due := range map[string]time.Time{
		"late":  time.Date(2031, 4, 15, 23, 0, 0, 0, loc),
		"today": time.Date(2031, 4, 16, 0, 30, 0, 0, loc),
	} {
		task := newTask(id)
		task.Key = "ENG-" + id
		task.DueAt = &due
		if _, err := r.writer.CreateTask(t.Context(), "op-"+id, task, nil); err != nil {
			t.Fatalf("create %s: %v", id, err)
		}
		r.drain()
	}
	answer := r.flow(tracker.FlowQuery{Bucket: period.Day, Points: 1}, now, loc)
	if answer.Now.Overdue != 1 {
		t.Errorf("now.overdue = %d, want the one due before today began in the "+
			"company's zone", answer.Now.Overdue)
	}
}

// NINETY POINTS AT MOST, and a bucket is a day or a week.
func TestTheFlowRefusesAnUnboundedSeries(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	for _, q := range []tracker.FlowQuery{
		{Bucket: period.Day, Points: tracker.MaxFlowPoints + 1},
		{Bucket: period.Day, Points: 0},
		{Bucket: period.Month, Points: 3},
	} {
		q.Level = statelog.ReadStale
		_, err := r.reader.Flow(t.Context(), q, wednesday, time.UTC)
		if !errors.Is(err, tracker.ErrInvalid) {
			t.Errorf("Flow(%+v) = %v, want a refusal", q, err)
		}
	}
	answer := r.flow(tracker.FlowQuery{Bucket: period.Day, Points: tracker.MaxFlowPoints},
		wednesday, time.UTC)
	if len(answer.Points) != tracker.MaxFlowPoints {
		t.Errorf("a %d-point series answered %d", tracker.MaxFlowPoints, len(answer.Points))
	}
}
