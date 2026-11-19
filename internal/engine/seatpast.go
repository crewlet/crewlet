package engine

import (
	"context"
	"slices"
	"time"

	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/store"
	"github.com/crewlet/crewlet/internal/worklet"
)

// The seats' past, as the flow backfill reads it (ADR-0162, worklet.SeatPast):
// when each seat was paused and when it was out of its model limit, from the
// pause and park events every live data node's event store still retains. A
// read whose answer is not whole, because a data node did not answer, covers
// nothing, so each change the backfill derives from it says it read no seat's
// events rather than reading a stopped seat as free on a partial answer.

// seatPastEvents are the events a seat's stops are read from, each paired
// with whether it begins one and the stop it is.
var seatPastEvents = []struct {
	kind  string
	cause worklet.FlowCause
	stops bool
}{
	{types.SeatPaused{}.EventType(), worklet.FlowCausePaused, true},
	{types.SeatResumed{}.EventType(), worklet.FlowCausePaused, false},
	{types.SeatProviderParked{}.EventType(), worklet.FlowCauseLimit, true},
	{types.SeatProviderReleased{}.EventType(), worklet.FlowCauseLimit, false},
}

// seatPastPage is how many rows one page of one kind reads; a seat's pauses
// and parks over the history's thirty days are a few pages at most.
const seatPastPage = 200

// seatPast is the engine's [worklet.SeatPast].
type seatPast struct{ engine *Engine }

// SeatPast reads seat's pauses and model limits up to to over the history the
// fleet's event stores keep: from its floor, which its coverage begins at,
// whatever span is asked, so a stop begun before the span is read whole. It
// covers nothing where this node reads no history.
func (p seatPast) SeatPast(ctx context.Context, seat string, _, to time.Time) (worklet.SeatHistory, error) {
	fleet := p.engine.History()
	agent := memoryAgentID(p.engine.Company(), seat)
	if fleet == nil || agent == "" {
		return worklet.SeatHistory{}, nil
	}
	now := time.Now().UTC()
	floor := now.Add(-store.EventHistory)
	var marks []seatMark
	for _, kind := range seatPastEvents {
		var before *store.Cursor
		for {
			page, coverage, err := fleet.List(ctx, store.ListQuery{Type: kind.kind,
				AgentID: agent, Since: floor, Until: to, Limit: seatPastPage,
				Before: before, At: now})
			if err != nil {
				return worklet.SeatHistory{}, err
			}
			if !coverage.Complete {
				return worklet.SeatHistory{}, nil
			}
			for _, row := range page.Rows {
				marks = append(marks, seatMark{at: row.Time.UTC(), cause: kind.cause, stops: kind.stops})
			}
			if len(page.Rows) < seatPastPage {
				break
			}
			last := page.Rows[len(page.Rows)-1]
			before = &store.Cursor{Time: last.Time, ID: last.ID}
		}
	}
	return worklet.SeatHistory{Covered: floor, Stops: seatStops(marks, floor)}, nil
}

// seatMark is one event that begins or ends a stop.
type seatMark struct {
	at    time.Time
	cause worklet.FlowCause
	stops bool
}

// seatStops folds a seat's marks into its stops: a renewed park, or a pause
// announced again, extends the stop that is open rather than beginning one,
// and an end whose beginning has aged out of the history ends a stop that
// began before the floor.
func seatStops(marks []seatMark, floor time.Time) []worklet.SeatStop {
	marks = slices.Clone(marks)
	slices.SortStableFunc(marks, func(a, b seatMark) int { return a.at.Compare(b.at) })
	var out []worklet.SeatStop
	open := map[worklet.FlowCause]int{}
	for _, m := range marks {
		i, stopped := open[m.cause]
		switch {
		case m.stops && !stopped:
			open[m.cause] = len(out)
			out = append(out, worklet.SeatStop{Cause: m.cause, From: m.at})
		case !m.stops && stopped:
			out[i].To = m.at
			delete(open, m.cause)
		case !m.stops && !slices.ContainsFunc(out, func(s worklet.SeatStop) bool {
			return s.Cause == m.cause
		}):
			out = append(out, worklet.SeatStop{Cause: m.cause, From: floor, To: m.at})
		}
	}
	return out
}
