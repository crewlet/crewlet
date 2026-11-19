package queries

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/eventfan"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/store"
	"github.com/crewlet/crewlet/internal/usage"
)

// stopLog is a fleet's event stores holding the records given, by type, every
// node answering.
type stopLog struct {
	FleetEvents
	rows map[string][]store.EventRecord
}

func (l stopLog) List(_ context.Context, q store.ListQuery) (eventfan.Listing, eventfan.Coverage, error) {
	return eventfan.Listing{Rows: l.rows[q.Type]}, eventfan.Coverage{Complete: true}, nil
}

// A SEAT'S STOPS ARE READ FROM ITS OWN RECORDS: a pause from its event to its
// resume, a model limit still standing to the window's end, an end whose start
// aged out of the history from the history's floor, and a pause older than the
// history from its record. Mutation: drop the pause record, and a seat paused
// for a month reads as free.
func TestASeatsStopsAreReadFromItsOwnRecords(t *testing.T) {
	t.Parallel()
	now := time.Date(2030, 1, 14, 9, 0, 0, 0, time.UTC)
	to := time.Date(2030, 1, 14, 0, 0, 0, 0, time.UTC)
	from := to.AddDate(0, 0, -7)
	a, b := uuid.New(), uuid.New()
	at := func(d time.Duration) time.Time { return from.Add(d) }
	event := func(agent uuid.UUID, when time.Time) store.EventRecord {
		return store.EventRecord{ID: uuid.NewString(), AgentID: agent.String(), Time: when}
	}
	s := Sources{Events: stopLog{rows: map[string][]store.EventRecord{
		types.SeatPaused{}.EventType():           {event(a, at(time.Hour))},
		types.SeatResumed{}.EventType():          {event(a, at(3 * time.Hour))},
		types.SeatProviderParked{}.EventType():   {event(a, at(5 * 24 * time.Hour))},
		types.SeatProviderReleased{}.EventType(): {event(b, at(-40 * time.Hour))},
	}}, Pauses: pauseList{pauses: []coord.SeatPause{{Seat: b, At: now.Add(-40 * 24 * time.Hour)}}}}
	stops, err := s.seatStops(t.Context(), from, to, now)
	if err != nil {
		t.Fatal(err)
	}
	gotA, wholeA := stoppedIn(stops[a.String()], from, to)
	if want := 2*time.Hour + 2*24*time.Hour; gotA != want || !wholeA {
		t.Errorf("a stopped %s (whole %v), want two hours paused and two days at its limit", gotA, wholeA)
	}
	if gotB, _ := stoppedIn(stops[b.String()], from, to); gotB != 7*24*time.Hour {
		t.Errorf("b stopped %s, want the whole week: its pause is older than the history", gotB)
	}
}

// STOPS THAT OVERLAP ARE COUNTED ONCE, clipped to the window, and one whose end
// its records do not give leaves the seat unmeasured. Mutation: sum the spans,
// and the overlap counts twice.
func TestStopsThatOverlapAreCountedOnce(t *testing.T) {
	t.Parallel()
	from := time.Date(2030, 1, 7, 0, 0, 0, 0, time.UTC)
	to := from.AddDate(0, 0, 7)
	got, whole := stoppedIn([]span{{from.Add(-time.Hour), from.Add(2 * time.Hour)},
		{from.Add(time.Hour), from.Add(3 * time.Hour)}, {from.Add(10 * time.Hour), from.Add(11 * time.Hour)}}, from, to)
	if got != 4*time.Hour || !whole {
		t.Errorf("stopped %s (whole %v), want four hours", got, whole)
	}
	if _, whole := stoppedIn([]span{{from, time.Time{}}}, from, to); whole {
		t.Error("a stop with no known end left the seat measured")
	}
}

// busyDays answers the seats' days given.
type busyDays struct {
	UsageReads
	rows []usage.SeatDay
}

func (b busyDays) SeatDays(context.Context, usage.SpendQuery) ([]usage.SeatDay, error) { return b.rows, nil }

// A SEAT'S BUSY TIME IS ITS DAYS' EXACT SUM ACROSS NODES, and a day with turns
// whose busy time was not measured leaves the seat unmeasured. Mutation: read
// an unmeasured day as zero, and b is judged on a week it was busy.
func TestASeatsBusyTimeIsItsDaysExactSum(t *testing.T) {
	t.Parallel()
	from := time.Date(2030, 1, 7, 0, 0, 0, 0, time.UTC)
	s := Sources{Usage: busyDays{rows: []usage.SeatDay{
		{AgentID: "a", Node: "n1", Turns: 3, Busy: time.Hour, BusyMeasured: true},
		{AgentID: "a", Node: "n2", Turns: 1, Busy: 30 * time.Minute, BusyMeasured: true},
		{AgentID: "b", Node: "n1", Turns: 2},
		{AgentID: "c", Node: "n1"},
	}}}
	got, err := s.seatBusy(t.Context(), from, from.AddDate(0, 0, 7), time.UTC)
	if err != nil {
		t.Fatal(err)
	}
	if got["a"] != (busyOf{busy: 90 * time.Minute, measured: true}) || got["b"].measured || !got["c"].measured {
		t.Errorf("busy %+v, want a's 1h30m measured, b unmeasured, c measured idle", got)
	}
}
