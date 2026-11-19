package worklet

import (
	"cmp"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/store"
)

// HOW LONG WORK STOOD IN EACH FLOW STATE, per seat, over a window: the flow
// record (ADR-0162) keeps each change of an item's state while it is in
// progress, so a state stands from the instant its change gives to the
// item's next change, or to the item leaving progress, clipped to the window.
// It is what the Advisor's staffing classes weigh: the hours work waited for
// a seat (due), and the hours it was held, by why.

// FlowHoursQuery asks for the item-hours of [Since, Until).
type FlowHoursQuery struct {
	Since, Until time.Time
	Level        statelog.ReadLevel
}

// FlowHours is the time one seat's items in progress stood in one state for
// one cause over the window, summed over the items, and how many items did.
type FlowHours struct {
	Seat  string
	State FlowState
	Cause FlowCause
	Stood time.Duration
	Items int
}

// MaxFlowHoursWindow bounds the window one read sums: two weeks, the longest
// the staffing classes read.
const MaxFlowHoursWindow = 14 * 24 * time.Hour

// FlowHours sums how long the items in progress stood in each state over the
// window, by seat (the item's assignee as each change read it), state and
// cause, in that order.
func (r *Reader) FlowHours(ctx context.Context, q FlowHoursQuery) ([]FlowHours, error) {
	spans, err := r.flowSpans(ctx, q)
	if err != nil {
		return nil, err
	}
	return sumFlowHours(spans, q.Since, q.Until), nil
}

// FlowItems counts, per seat, the items in progress that stood in any flow
// state for some of the window: the items each seat had to work.
func (r *Reader) FlowItems(ctx context.Context, q FlowHoursQuery) (map[string]int, error) {
	spans, err := r.flowSpans(ctx, q)
	if err != nil {
		return nil, err
	}
	items := map[string]map[string]bool{}
	for i, s := range spans {
		start, end := s.start, spanEnd(spans, i, q.Until)
		if start.Before(q.Since) {
			start = q.Since
		}
		if s.seat == "" || !end.After(start) {
			continue
		}
		if items[s.seat] == nil {
			items[s.seat] = map[string]bool{}
		}
		items[s.seat][s.task] = true
	}
	out := make(map[string]int, len(items))
	for seat, tasks := range items {
		out[seat] = len(tasks)
	}
	return out, nil
}

// FlowPickups is how long each item a seat picked up in the window had stood
// due first, shortest first: a due change the item's next change ended by
// moving, at an instant in [Since, Until). Due is a start passed with nothing
// of the assignee's on the item since, so it moves only once the assignee
// acted on it (the turn itself is the turns' record and never a flow change).
// The Advisor's weekly report reads the median and 90th percentile as the
// week's pickup wait.
func (r *Reader) FlowPickups(ctx context.Context, q FlowHoursQuery) ([]time.Duration, error) {
	spans, err := r.flowSpans(ctx, q)
	if err != nil {
		return nil, err
	}
	return pickupsOf(spans, q.Since, q.Until), nil
}

// pickupsOf is every pickup spans hold that ended in [since, until).
func pickupsOf(spans []flowSpan, since, until time.Time) []time.Duration {
	out := []time.Duration{}
	for i, s := range spans {
		if s.state != FlowDue || i+1 == len(spans) || spans[i+1].task != s.task {
			continue
		}
		next := spans[i+1]
		if next.state == FlowMoving && !next.start.Before(since) &&
			next.start.Before(until) && next.start.After(s.start) {
			out = append(out, next.start.Sub(s.start))
		}
	}
	slices.Sort(out)
	return out
}

// flowSpans reads the window's flow changes, each item's oldest first.
func (r *Reader) flowSpans(ctx context.Context, q FlowHoursQuery) ([]flowSpan, error) {
	switch {
	case q.Level == "":
		return nil, errors.New("worklet: this flow read names no level")
	case q.Since.IsZero() || !q.Until.After(q.Since) || q.Until.Sub(q.Since) > MaxFlowHoursWindow:
		return nil, invalid("a flow hours window is from before until, at most %s", MaxFlowHoursWindow)
	}
	var out []flowSpan
	_, err := r.log.Read(ctx, statelog.Query{
		Level: q.Level,
		Scope: statelog.ScopeSet{Paths: []string{pathDomain}}.Normalised(),
		Set:   true,
	}, func(tx *sql.Tx) error {
		var err error
		out, err = readFlowSpans(ctx, tx, q.Since)
		return err
	})
	if err != nil {
		return nil, fmt.Errorf("worklet: read the flow hours: %w", err)
	}
	return out, nil
}

// spanEnd is when spans[i] stopped standing: the item's next change, else when
// the item left progress, else until.
func spanEnd(spans []flowSpan, i int, until time.Time) time.Time {
	s := spans[i]
	switch {
	case i+1 < len(spans) && spans[i+1].task == s.task:
		return spans[i+1].start
	case !s.left.IsZero() && s.left.Before(until):
		return s.left
	}
	return until
}

// flowSpan is one change of an item's flow as the window reads it.
type flowSpan struct {
	task, seat string
	state      FlowState
	cause      FlowCause
	start      time.Time
	// left is when the item left progress, zero while it is in it.
	left time.Time
}

// flowHoursStatement reads the flow changes of every item that was in
// progress at any instant since since (in progress now, or whose status or
// removal moved since), each item's newest first: the order of the flow's own
// index, so the pass over the record sorts nothing.
func flowHoursStatement(since time.Time) (string, []any) {
	return `
		SELECT f.task_id, f.seat, f.state, f.cause, COALESCE(f.since, f.recorded_at),
		       t.status = ? AND t.removed_at IS NULL, t.status_entered_at, COALESCE(t.removed_at, 0)
		FROM tracker_flow f JOIN tracker_tasks t ON t.id = f.task_id
		WHERE f.notice = '' AND (
			(t.status = ? AND t.removed_at IS NULL) OR t.status_entered_at >= ? OR t.removed_at >= ?)
		ORDER BY f.task_id, f.log_seq DESC`,
		[]any{string(StatusInProgress), string(StatusInProgress), store.EncodeTime(since), store.EncodeTime(since)}
}

// readFlowSpans reads the window's flow changes, each item's oldest first.
func readFlowSpans(ctx context.Context, tx *sql.Tx, since time.Time) ([]flowSpan, error) {
	statement, args := flowHoursStatement(since)
	rows, err := tx.QueryContext(ctx, statement, args...)
	if err != nil {
		return nil, err
	}
	var spans []flowSpan
	first := 0
	for rows.Next() {
		var (
			s                    flowSpan
			state, cause         string
			start, entered, gone int64
			inProgress           bool
		)
		if err := rows.Scan(&s.task, &s.seat, &state, &cause, &start, &inProgress, &entered, &gone); err != nil {
			_ = rows.Close()
			return nil, err
		}
		s.state, s.cause, s.start = FlowState(state), FlowCause(cause), store.DecodeTime(start)
		if !inProgress {
			s.left = store.DecodeTime(max(entered, gone))
		}
		// EACH ITEM'S CHANGES OLDEST FIRST, as the sum reads them: the
		// statement answers them newest first.
		if len(spans) > 0 && spans[len(spans)-1].task != s.task {
			slices.Reverse(spans[first:])
			first = len(spans)
		}
		spans = append(spans, s)
	}
	slices.Reverse(spans[first:])
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return nil, err
	}
	return spans, nil
}

// sumFlowHours sums spans, grouped by task in log order: each change stands
// until the task's next one, or until the task left progress, or until.
func sumFlowHours(spans []flowSpan, since, until time.Time) []FlowHours {
	type key struct {
		seat  string
		state FlowState
		cause FlowCause
	}
	sums := map[key]*FlowHours{}
	items := map[key]map[string]bool{}
	for i, s := range spans {
		end := spanEnd(spans, i, until)
		start := s.start
		if start.Before(since) {
			start = since
		}
		if end.After(until) {
			end = until
		}
		if !end.After(start) {
			continue
		}
		k := key{s.seat, s.state, s.cause}
		h, ok := sums[k]
		if !ok {
			h = &FlowHours{Seat: s.seat, State: s.state, Cause: s.cause}
			sums[k], items[k] = h, map[string]bool{}
		}
		h.Stood += end.Sub(start)
		items[k][s.task] = true
	}
	out := make([]FlowHours, 0, len(sums))
	for k, h := range sums {
		h.Items = len(items[k])
		out = append(out, *h)
	}
	slices.SortFunc(out, func(a, b FlowHours) int {
		return cmp.Or(cmp.Compare(a.Seat, b.Seat), cmp.Compare(slices.Index(FlowStates, a.State),
			slices.Index(FlowStates, b.State)), cmp.Compare(a.Cause, b.Cause))
	})
	return out
}
