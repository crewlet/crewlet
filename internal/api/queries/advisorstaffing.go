package queries

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/crewlet/crewlet/internal/advisor"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/org"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/store"
	"github.com/crewlet/crewlet/internal/tokens"
	"github.com/crewlet/crewlet/internal/usage"
	"github.com/crewlet/crewlet/internal/worklet"
)

// THE STAFFING CLASSES' READ (internal/advisor's Staffing), over whole company
// days ending where today begins: each agent seat's exact busy time from its
// days' usage, its stopped time from its own records (its pause, the pause and
// model-limit events the fleet's event stores keep, and its budget refusals
// with their reset), how many items it had, how long its work stood in each
// flow state, and every company document active over the window. A read that
// cannot be whole is an error, so the classes say nothing rather than judge a
// partial week.

// The reads the staffing read makes past the evidence's, each optional: a node
// without one reads no staffing.
type (
	flowItemsReads interface {
		FlowItems(ctx context.Context, q worklet.FlowHoursQuery) (map[string]int, error)
	}
)

// stopEvents are the events a seat's pauses and model limits are read from,
// each with the stop it is and whether it begins one.
var stopEvents = []struct {
	kind   string
	cause  string
	begins bool
}{
	{types.SeatPaused{}.EventType(), "paused", true},
	{types.SeatResumed{}.EventType(), "paused", false},
	{types.SeatProviderParked{}.EventType(), "limit", true},
	{types.SeatProviderReleased{}.EventType(), "limit", false},
}

// span is one stretch a seat could not take a turn.
type span struct{ from, to time.Time }

// advisorStaffing reads the staffing classes' window of days whole company days
// before the one now falls in; nil for a company whose tracker keeps no flow
// record, which is not judged.
func (s Sources) advisorStaffing(ctx context.Context, company *config.Company, now time.Time,
	days int) (*advisor.Staffing, error) {
	if company == nil {
		return nil, nil
	}
	switch kept, err := keptHere(s.Work); {
	case err != nil:
		return nil, err
	case !kept:
		return nil, nil
	}
	flows, ok := s.Work.(flowReads)
	items, itemsOK := s.Work.(flowItemsReads)
	if !ok || !itemsOK || s.Config == nil || s.Usage == nil || s.Events == nil {
		return nil, errors.New("queries: this node reads no seat time to judge staffing on")
	}
	zone := s.zone()
	local := now.In(zone)
	to := time.Date(local.Year(), local.Month(), local.Day(), 0, 0, 0, 0, zone)
	from := to.AddDate(0, 0, -days)
	out := &advisor.Staffing{From: from, To: to, Seats: map[string]advisor.SeatTime{}}
	var err error
	if out.Documents, err = s.Config.CompaniesActiveSince(ctx, from); err != nil {
		return nil, fmt.Errorf("queries: the documents active since %s: %w", from, err)
	}
	window := worklet.FlowHoursQuery{Since: from, Until: to, Level: statelog.ReadStale}
	if out.Flow, err = flows.FlowHours(ctx, window); err != nil {
		return nil, err
	}
	if out.Items, err = items.FlowItems(ctx, window); err != nil {
		return nil, err
	}
	if days == advisor.AddSeatDays {
		for d := from; d.Before(to); d = d.AddDate(0, 0, 1) {
			hours, err := flows.FlowHours(ctx, worklet.FlowHoursQuery{Since: d, Until: d.AddDate(0, 0, 1),
				Level: statelog.ReadStale})
			if err != nil {
				return nil, err
			}
			due := map[string]time.Duration{}
			for _, h := range hours {
				if h.State == worklet.FlowDue {
					due[h.Seat] += h.Stood
				}
			}
			out.DueByDay = append(out.DueByDay, due)
		}
	}
	busy, err := s.seatBusy(ctx, from, to, zone)
	if err != nil {
		return nil, err
	}
	stops, err := s.seatStops(ctx, from, to, now)
	if err != nil {
		return nil, err
	}
	for role := range company.EachRole() {
		if role.Kind == org.KindHuman {
			continue
		}
		handle := role.Seat().Handle()
		derived, ok := org.DeriveAgentID(company.Name, handle)
		if !ok {
			continue
		}
		id := derived.String()
		b, worked := busy[id]
		if !worked {
			b.measured = true
		}
		stopped, whole := stoppedIn(stops[id], from, to)
		out.Seats[handle] = advisor.SeatTime{Busy: b.busy, Stopped: stopped, Measured: b.measured && whole}
	}
	return out, nil
}

// seatBusy is each seat's busy time over [from, to), by agent id, and whether
// every day it took turns measured it.
type busyOf struct {
	busy     time.Duration
	measured bool
}

func (s Sources) seatBusy(ctx context.Context, from, to time.Time, zone *time.Location) (map[string]busyOf, error) {
	days := tokens.LastDays(int(to.Sub(from).Hours()/24+0.5), to.Add(-time.Nanosecond), zone)
	rows, err := s.Usage.SeatDays(ctx, usage.SpendQuery{From: days.First.Label, To: days.Last.Label})
	if err != nil {
		return nil, fmt.Errorf("queries: the seats' days: %w", err)
	}
	out := map[string]busyOf{}
	for _, r := range rows {
		b, seen := out[r.AgentID]
		if !seen {
			b.measured = true
		}
		b.busy += r.Busy
		if r.Turns > 0 && !r.BusyMeasured {
			b.measured = false
		}
		out[r.AgentID] = b
	}
	return out, nil
}

// seatStops is each seat's stops by agent id, read from its own records: its
// pause, open since before the event history where it is; its pause and model
// limit events over the history; and its budget refusals, each until its
// window reset. A stop whose end its records do not give has a zero end, which
// leaves the seat unmeasured.
func (s Sources) seatStops(ctx context.Context, from, to, now time.Time) (map[string][]span, error) {
	floor := now.Add(-store.EventHistory)
	type mark struct {
		at     time.Time
		cause  string
		begins bool
	}
	marks := map[string][]mark{}
	for _, kind := range stopEvents {
		rows, err := s.spanOf(ctx, floor, to, kind.kind, nil)
		if err != nil {
			return nil, err
		}
		for _, r := range rows {
			marks[r.AgentID] = append(marks[r.AgentID], mark{at: r.Time.UTC(), cause: kind.cause, begins: kind.begins})
		}
	}
	out := map[string][]span{}
	for agent, ms := range marks {
		slices.SortStableFunc(ms, func(a, b mark) int { return a.at.Compare(b.at) })
		open := map[string]time.Time{}
		for _, m := range ms {
			began, isOpen := open[m.cause]
			switch {
			case m.begins && !isOpen:
				open[m.cause] = m.at
			case !m.begins && isOpen:
				out[agent] = append(out[agent], span{began, m.at})
				delete(open, m.cause)
			case !m.begins:
				// AN END WHOSE BEGINNING HAS AGED OUT of the history: the
				// stop stood from before the history's floor.
				out[agent] = append(out[agent], span{floor, m.at})
			}
		}
		for _, began := range open {
			out[agent] = append(out[agent], span{began, to})
		}
	}
	// A PAUSE OLDER THAN THE HISTORY has no event left, only its record.
	if s.Pauses != nil {
		pauses, err := s.Pauses.ListSeatPauses(ctx)
		if err != nil {
			return nil, fmt.Errorf("queries: list the seats' pauses: %w", err)
		}
		for _, p := range pauses {
			agent := p.Seat.String()
			if p.At.Before(floor) {
				out[agent] = append(out[agent], span{p.At, to})
			}
		}
	}
	refusals, err := s.budgetStopsIn(ctx, maxTime(from.AddDate(0, -1, 0), floor), to)
	if err != nil {
		return nil, err
	}
	chart := s.chartHandles()
	byHandle := map[string]string{}
	for id, seat := range chart {
		byHandle[seat.handle] = id
	}
	for _, r := range refusals {
		if agent, ok := byHandle[r.Seat]; ok {
			out[agent] = append(out[agent], span{r.At, r.Resets})
		}
	}
	return out, nil
}

func maxTime(a, b time.Time) time.Time {
	if a.After(b) {
		return a
	}
	return b
}

// stoppedIn is the union of spans clipped to [from, to), and whether every
// span's end is known.
func stoppedIn(spans []span, from, to time.Time) (time.Duration, bool) {
	whole := true
	var clipped []span
	for _, sp := range spans {
		if sp.to.IsZero() {
			whole = false
			continue
		}
		f, t := maxTime(sp.from, from), sp.to
		if t.After(to) {
			t = to
		}
		if t.After(f) {
			clipped = append(clipped, span{f, t})
		}
	}
	slices.SortFunc(clipped, func(a, b span) int { return cmp.Or(a.from.Compare(b.from), a.to.Compare(b.to)) })
	var total time.Duration
	var cur span
	for i, sp := range clipped {
		switch {
		case i == 0:
			cur = sp
		case !sp.from.After(cur.to):
			if sp.to.After(cur.to) {
				cur.to = sp.to
			}
		default:
			total += cur.to.Sub(cur.from)
			cur = sp
		}
	}
	if len(clipped) > 0 {
		total += cur.to.Sub(cur.from)
	}
	return total, whole
}
