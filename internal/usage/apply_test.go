package usage_test

import (
	"slices"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/period"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/usage"
)

type statelogRecord = statelog.Record

// seatRecord is a seat-day with the given spend cells, one per model.
func seatRecord(node, day, seat string, models ...string) usage.Record {
	r := usage.Record{
		RecordEnvelope: usage.RecordEnvelope{Writer: node,
			Subject: usage.Subject{Kind: usage.KindSeat, Node: node, Day: day, Seat: seat}},
		Handle: "dev", Role: "Dev",
		Turns: &usage.Turns{Count: int64(len(models))},
	}
	for i, m := range models {
		r.Tokens = append(r.Tokens, usage.Tokens{Phase: "execute", Model: m,
			Input: int64(100 * (i + 1)), Output: 10, Total: int64(100*(i+1)) + 10, Calls: 1})
	}
	return r
}

// spendModels is the models a seat-day's spend rows name, in order.
func spendModels(t *testing.T, rows []usage.SpendRow) []string {
	t.Helper()
	var out []string
	for _, r := range rows {
		out = append(out, r.Model)
	}
	return out
}

// AN APPLY REPLACES THE OBJECT'S ROWS.
//
// A record is the day's cumulative value, republished whole every time it
// moves. Inserted beside what was there, the afternoon's record would count the
// morning a second time, and a model the seat stopped using at noon would keep
// its morning's row — both indistinguishable, to every reader, from real spend.
func TestAnApplyReplacesTheObjectsRows(t *testing.T) {
	t.Parallel()
	db := openStore(t)
	const day = "2026-09-23"
	if err := applyRecord(t.Context(), db, recordAt(t,
		seatRecord("node-a", day, "seat-1", "m-morning", "m-both"), 1)); err != nil {
		t.Fatalf("apply the morning: %v", err)
	}
	if err := applyRecord(t.Context(), db, recordAt(t,
		seatRecord("node-a", day, "seat-1", "m-both"), 2)); err != nil {
		t.Fatalf("apply the afternoon: %v", err)
	}
	rows, err := usage.Spend(t.Context(), db.Replicated(), day, day)
	if err != nil {
		t.Fatalf("read the spend: %v", err)
	}
	if got := spendModels(t, rows); !slices.Equal(got, []string{"m-both"}) {
		t.Fatalf("after the afternoon's record the day holds %v — the record is "+
			"the whole day, so only its own cells may remain", got)
	}
	if rows[0].Input != 100 {
		t.Fatalf("the surviving cell holds input %d, not the afternoon's 100",
			rows[0].Input)
	}
	if n := len(dump(t, db)["usage_turns"]); n != 1 {
		t.Fatalf("the seat-day has %d head rows", n)
	}
}

// AN OLDER RECORD DOES NOT OVERWRITE A NEWER ONE.
//
// A compacted stream is replayed in sequence order, but an adoption, a
// redelivery or a reprocessed deferral can hand the applier a record below
// what it holds. The position is the guard; without it the day would move
// backwards to whatever was replayed last.
func TestAnOlderRecordDoesNotOverwriteANewerOne(t *testing.T) {
	t.Parallel()
	db := openStore(t)
	const day = "2026-09-23"
	newer := recordAt(t, seatRecord("node-a", day, "seat-1", "m-new"), 9)
	older := recordAt(t, seatRecord("node-a", day, "seat-1", "m-old"), 4)
	for _, rec := range []statelogRecord{newer, older} {
		if err := applyRecord(t.Context(), db, rec); err != nil {
			t.Fatal(err)
		}
	}
	rows, err := usage.Spend(t.Context(), db.Replicated(), day, day)
	if err != nil {
		t.Fatal(err)
	}
	if got := spendModels(t, rows); !slices.Equal(got, []string{"m-new"}) {
		t.Fatalf("a record at sequence 4 applied after one at 9 left %v", got)
	}

	// AND A SCHEDULE, whose guard is the newest version among its rows
	// rather than a head row.
	fires := func(at ...int) usage.Record {
		r := usage.Record{RecordEnvelope: usage.RecordEnvelope{Writer: "node-a",
			Subject: usage.Subject{Kind: usage.KindSchedule, Node: "node-a", Day: day,
				ScopeType: "role", ScopeID: "Dev", Schedule: "standup"}}}
		for _, h := range at {
			r.Fires = append(r.Fires, usage.Fire{Outcome: "fired",
				At: time.Date(2026, 9, 23, h, 0, 0, 0, time.UTC)})
		}
		return r
	}
	for _, rec := range []statelogRecord{
		recordAt(t, fires(9, 12), 20), recordAt(t, fires(9), 11),
	} {
		if err := applyRecord(t.Context(), db, rec); err != nil {
			t.Fatal(err)
		}
	}
	if n := len(dump(t, db)["usage_schedule_runs"]); n != 2 {
		t.Fatalf("the schedule-day holds %d fires after an older record with one "+
			"was replayed over a newer one with two", n)
	}
}

// THE HORIZON LEAVES WITH THE RECORD THAT MAKES IT OLD.
//
// Nothing sweeps these tables: a record for day D removes every row older than
// D minus the history in its own transaction, on every node, because that is a
// pure function of the record and a sweep would be a second writer of the
// replicated estate. The day exactly at the horizon stays — a ninety-day window
// compared with the ninety before it, plus the day a moved clock can touch —
// and the day before it goes.
func TestTheHorizonLeavesWithTheRecordThatMakesItOld(t *testing.T) {
	t.Parallel()
	db := openStore(t)
	today, err := period.Parse(period.Day, "2026-09-23", nil)
	if err != nil {
		t.Fatal(err)
	}
	kept := today.Shift(-usage.HorizonDays).Label
	gone := today.Shift(-usage.HorizonDays - 1).Label
	for i, day := range []string{gone, kept} {
		if err := applyRecord(t.Context(), db, recordAt(t,
			seatRecord("node-a", day, "seat-1", "m"), uint64(i+1))); err != nil {
			t.Fatal(err)
		}
	}
	if err := applyRecord(t.Context(), db, recordAt(t,
		seatRecord("node-b", today.Label, "seat-2", "m"), 3)); err != nil {
		t.Fatal(err)
	}
	rows, err := usage.Spend(t.Context(), db.Replicated(), gone, today.Label)
	if err != nil {
		t.Fatal(err)
	}
	var days []string
	for _, r := range rows {
		days = append(days, r.Day)
	}
	if want := []string{kept, today.Label}; !slices.Equal(days, want) {
		t.Fatalf("after a record for %s the rows hold days %v, want %v — %d days "+
			"of history is what every spend window was promised",
			today.Label, days, want, usage.HorizonDays)
	}
	if usage.HorizonDays != 181 {
		t.Fatalf("the horizon is %d days; the offered range is 90 + the previous "+
			"90 + the day a moved clock touches", usage.HorizonDays)
	}
}
