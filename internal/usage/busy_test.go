package usage_test

import (
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/store"
	"github.com/crewlet/crewlet/internal/usage"
)

// A SEAT'S DAY KEEPS ITS EXACT BUSY TIME: applied, the sum of its turns'
// durations is on its head row and the seat-days read answers it to the
// millisecond; the fleet's join keeps the larger of two values of the day; a
// day whose record did not carry it reads as unmeasured, never as idle.
// Mutation: leave busy_ms out of the upsert, or read a missing value as zero
// measured, and a check fails.
func TestASeatsDayKeepsItsExactBusyTime(t *testing.T) {
	t.Parallel()
	db := openStore(t)
	const day = "2026-09-23"
	read := func() usage.SeatDay {
		t.Helper()
		days, err := usage.SeatDays(t.Context(), db.Replicated(), usage.SpendQuery{From: day, To: day})
		if err != nil || len(days) != 1 {
			t.Fatalf("read the seat days: %+v, %v", days, err)
		}
		return days[0]
	}
	unmeasured := seatRecord("node-a", day, "seat-1", "m")
	if err := applyRecord(t.Context(), db, recordAt(t, unmeasured, 1)); err != nil {
		t.Fatalf("apply the unmeasured day: %v", err)
	}
	if got := read(); got.BusyMeasured || got.Busy != 0 {
		t.Errorf("a day whose record carried no busy time reads %s measured %v, want unmeasured", got.Busy, got.BusyMeasured)
	}
	busy := int64(5_400_123)
	measured := seatRecord("node-a", day, "seat-1", "m")
	measured.Turns.BusyMS = &busy
	if err := applyRecord(t.Context(), db, recordAt(t, measured, 2)); err != nil {
		t.Fatalf("apply the measured day: %v", err)
	}
	if got := read(); !got.BusyMeasured || got.Busy != 5_400_123*time.Millisecond {
		t.Errorf("the measured day reads %s measured %v, want 1h30m0.123s", got.Busy, got.BusyMeasured)
	}
	applied, held, err := usage.Applied(t.Context(), db.Replicated(), measured.Subject)
	if err != nil || !held || applied.Turns == nil || applied.Turns.BusyMS == nil || *applied.Turns.BusyMS != busy {
		t.Fatalf("the applied day holds %+v (%v, %v), want its busy time", applied.Turns, held, err)
	}
}

// A SEAT'S BUSY TIME IS THE SUM OF ITS TURNS' OWN DURATIONS, to the
// millisecond, and a day with no turn carries none, staying the format every
// build reads. Mutation: read it off the histogram, and the sum is off by up
// to half a bin.
func TestASeatsBusyTimeIsTheSumOfItsTurns(t *testing.T) {
	t.Parallel()
	into := openStore(t)
	p, err := usage.NewPublisher(usage.PublisherDeps{
		Store: oneSeatDay{}, Log: &loopback{t: t, into: into, node: "node-a"}, Estate: into.Replicated(),
		NodeID: "node-a", Caught: caught, Generations: unseen,
		Zone: func() *time.Location { return time.UTC }, FireHistory: fireHistory,
	})
	if err != nil {
		t.Fatal(err)
	}
	recs, err := p.Records("2026-09-23", store.UsageDay{Seats: []store.UsageSeat{
		{AgentID: "seat-1", Turns: store.UsageTurns{Count: 3,
			Durations: []time.Duration{41*time.Second + 7*time.Millisecond, 90 * time.Minute, 333 * time.Millisecond}}},
		{AgentID: "seat-2"},
	}})
	if err != nil {
		t.Fatal(err)
	}
	want := (41*time.Second + 7*time.Millisecond + 90*time.Minute + 333*time.Millisecond).Milliseconds()
	if len(recs) != 2 || recs[0].Turns.BusyMS == nil || *recs[0].Turns.BusyMS != want || recs[1].Turns.BusyMS != nil {
		t.Fatalf("the records carry %v and %v, want seat-1's %d ms and none for seat-2", recs[0].Turns.BusyMS,
			recs[1].Turns.BusyMS, want)
	}
}
