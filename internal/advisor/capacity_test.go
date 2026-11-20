package advisor_test

import (
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/advisor"
)

// CAPACITY IS THE HOURS EACH ENTRY WAS OUT: its seats' park stretches merged,
// split at company midnight, each separate stretch one hit, and each seat's
// own hours beside them, the entry out longest first. Mutation: sum the seats'
// stretches rather than merge them, and two seats parked together count twice.
func TestCapacityIsTheHoursEachEntryWasOut(t *testing.T) {
	t.Parallel()
	day, to := time.Date(2030, 1, 5, 0, 0, 0, 0, time.UTC), time.Date(2030, 1, 7, 18, 0, 0, 0, time.UTC)
	at := func(h float64) time.Time { return day.Add(time.Duration(h * float64(time.Hour))) }
	c := advisor.CapacityOf([]advisor.ParkSpan{
		{Seat: "dev", Entry: "main", From: at(8), To: at(14)},
		{Seat: "qa", Entry: "main", From: at(9), To: at(15)},
		{Seat: "dev", Entry: "main", From: at(22), To: at(26)},
		{Seat: "ops", Entry: "spare", From: at(10), To: at(11)},
	}, to, 7, time.UTC)
	if len(c.Days) != 7 || !c.Days[6].Equal(time.Date(2030, 1, 7, 0, 0, 0, 0, time.UTC)) || len(c.Entries) != 2 || c.Entries[0].Entry != "main" {
		t.Fatalf("capacity = %+v", c)
	}
	main := c.Entries[0]
	if main.Total != 11*time.Hour || main.Hits != 2 || main.Out[4] != 9*time.Hour || main.Out[5] != 2*time.Hour ||
		main.HitsByDay[4] != 2 {
		t.Errorf("main = total %s, hits %d, out %v, hits by day %v; want 11h over two stretches, 9h then 2h",
			main.Total, main.Hits, main.Out, main.HitsByDay)
	}
	if len(main.Seats) != 2 || main.Seats[0].Seat != "dev" || main.Seats[0].Held != 10*time.Hour ||
		main.Seats[1].Held != 6*time.Hour {
		t.Errorf("main's seats = %+v, want dev 10h and qa 6h", main.Seats)
	}
}

// A MODEL ENTRY OUT FOR HOURS IS ONE FINDING WITH ITS SEATS INSIDE: its
// headline carries its worst day's hours and hits, its seats each with their
// hours, and an entry out under the bar raises nothing. Mutation: raise one
// finding per seat, and nine seats on one entry are nine rows again.
func TestAModelEntryOutForHoursIsOneFindingWithItsSeats(t *testing.T) {
	t.Parallel()
	day, to := time.Date(2030, 1, 5, 0, 0, 0, 0, time.UTC), time.Date(2030, 1, 7, 18, 0, 0, 0, time.UTC)
	at := func(h float64) time.Time { return day.Add(time.Duration(h * float64(time.Hour))) }
	c := advisor.CapacityOf([]advisor.ParkSpan{
		{Seat: "dev-a", Entry: "p", From: at(8), To: at(19)},
		{Seat: "dev-b", Entry: "p", From: at(8), To: at(19)},
		{Seat: "rev", Entry: "q", From: at(8), To: at(9)},
	}, to, 7, time.UTC)
	found := staffed(t, teamShape, advisor.Evidence{Capacity: &c}, advisor.ClassModelLimit)
	if len(found) != 1 || found[0].Subject.Kind != advisor.SubjectModel || found[0].Subject.Name != "p" {
		t.Fatalf("raised %+v, want one finding about entry p", found)
	}
	f := found[0]
	if f.Headline != "2 seats sat out 11h on Sat 5 Jan: p hit its usage limit once." {
		t.Errorf("headline = %q", f.Headline)
	}
	if len(f.Seats) != 2 || f.Seats[0].Name != "Dev A" || f.Seats[0].Value != 11 || f.Seats[0].Unit != advisor.UnitHours {
		t.Errorf("seats = %+v, want Dev A and Dev B, 11 hours each", f.Seats)
	}
	if f.Fix.Kind != advisor.FixOpenSetting || !strings.Contains(f.Fix.Sentence, "Dev A, Dev B") {
		t.Errorf("fix = %+v", f.Fix)
	}
}

// A SNAPSHOT DRAWS THE HOURS EACH ENTRY WAS OUT: the seat-hours lost beside
// the range before, and a column of each entry's hours per day, its note
// naming the worst day; an unread park record is named, never drawn as none.
// Mutation: draw entry-hours as the stat, and two seats out together count once.
func TestASnapshotDrawsTheHoursEachEntryWasOut(t *testing.T) {
	t.Parallel()
	day, to := time.Date(2030, 1, 5, 0, 0, 0, 0, time.UTC), time.Date(2030, 1, 7, 18, 0, 0, 0, time.UTC)
	c := advisor.CapacityOf([]advisor.ParkSpan{
		{Seat: "dev", Entry: "main", From: day.Add(8 * time.Hour), To: day.Add(12 * time.Hour)},
		{Seat: "qa", Entry: "main", From: day.Add(8 * time.Hour), To: day.Add(12 * time.Hour)},
	}, to, 7, time.UTC)
	before := advisor.CapacityOf(nil, day, 7, time.UTC)
	s := advisor.ComputeSnapshot(advisor.SnapshotInputs{Capacity: &c, CapacityBefore: &before, LedgerUnread: true}, to)
	stat, ok := statOf(s, "capacity_lost_hours")
	if !ok || stat.Value != 8 || stat.Was == nil || *stat.Was != 0 {
		t.Errorf("capacity lost = %+v (%v), want 8 seat-hours beside none", stat, ok)
	}
	chart, ok := chartOf(s, "capacity_lost")
	if !ok || chart.X.Kind != advisor.AxisDay || len(chart.X.Keys) != 7 || chart.X.Keys[4] != "2030-01-05" ||
		len(chart.Series) != 1 || value(chart.Series[0].Values[4]) != 4 || chart.Note == nil {
		t.Errorf("capacity chart = %+v", chart)
	}
	s = advisor.ComputeSnapshot(advisor.SnapshotInputs{CapacityUnread: true, LedgerUnread: true}, to)
	if _, ok := statOf(s, "capacity_lost_hours"); ok || s.Unread[0] != advisor.UnreadCapacity {
		t.Errorf("an unread park record drew %+v, unread %v", s.Stats, s.Unread)
	}
}
