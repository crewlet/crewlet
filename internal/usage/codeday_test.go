package usage_test

import (
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/store"
	"github.com/crewlet/crewlet/internal/usage"
)

// A SEAT'S CODE DAY IS VERSION 3, AND ONLY WHERE IT COUNTS SOMETHING: a seat's
// day carrying its changes and reviews is stamped 3, so a build at 1 or 2
// retains it rather than applying it with them dropped — which would replace
// the day every upgraded node holds with one counting none — while a seat's
// day with none of them stays the base format every build applies. A code day
// rides a seat's record alone, never with a figure below zero, and never all
// zeros, which would hold older builds back from a day that counts nothing.
//
// Mutation: drop the Record.Code row from versionedFields — the code day is
// stamped 1, red; publish a zero code day (Code: &CodeDay{}) — the empty day
// is stamped 3, red.
func TestASeatsCodeDayIsVersionThree(t *testing.T) {
	t.Parallel()
	stamp := func(r usage.Record) (int, error) {
		t.Helper()
		body, err := r.Encode()
		if err != nil {
			return 0, err
		}
		env, err := usage.DecodeEnvelope(body)
		if err != nil {
			t.Fatalf("decode %s: %v", r.Subject, err)
		}
		return env.V, nil
	}
	coded := seatRecord("node-a", "2026-09-23", "seat-1", "m")
	coded.Code = &usage.CodeDay{ChangesOpened: 1, ReviewsGiven: 2, FindingsReceived: 3}
	if v, err := stamp(coded); err != nil || v != 3 {
		t.Errorf("a seat's day counting its changes and reviews is stamped %d (%v), want 3", v, err)
	}
	if v, err := stamp(seatRecord("node-a", "2026-09-23", "seat-1", "m")); err != nil || v != 1 {
		t.Errorf("a seat's day with no change or review is stamped %d (%v), want the base format's 1", v, err)
	}
	if usage.RecordVersion != 3 {
		t.Errorf("this build reads version %d, want 3", usage.RecordVersion)
	}

	person := usage.Record{RecordEnvelope: usage.RecordEnvelope{Writer: "node-a",
		Subject: usage.Subject{Kind: usage.KindPerson, Node: "node-a", Day: "2026-09-23", Person: "founder"}},
		Tokens: []usage.Tokens{{Phase: "auxiliary", Model: "m", Input: 1, Total: 1, Calls: 1}},
		Code:   &usage.CodeDay{ChangesOpened: 1}}
	zeros := seatRecord("node-a", "2026-09-23", "seat-1", "m")
	zeros.Code = &usage.CodeDay{}
	negative := seatRecord("node-a", "2026-09-23", "seat-1", "m")
	negative.Code = &usage.CodeDay{FindingsReceived: -1}
	for name, bad := range map[string]usage.Record{"a person's": person, "all zeros": zeros,
		"a negative figure": negative} {
		if _, err := bad.Encode(); err == nil {
			t.Errorf("a code day on %s record encoded", name)
		}
	}
}

// A SEAT'S CODE DAY IS ITS HEAD ROW'S, REPLACED WITH IT: applied, a seat's day
// writes its changes opened, reviews given and findings received on the head
// row, which the seat-days read answers; a later record of the day that counts
// none writes zeros rather than keeping the earlier figures; and the join with
// what the fleet holds raises each figure to the larger, as every counter of a
// day joins.
//
// Mutation: leave the columns out of the upsert's update — the later record's
// day still counts the morning's, red; join by the derivation alone — the
// fleet's larger review count is lost, red.
func TestASeatsCodeDayIsAppliedOnItsHeadRow(t *testing.T) {
	t.Parallel()
	db := openStore(t)
	const day = "2026-09-23"
	read := func() usage.CodeDay {
		t.Helper()
		days, err := usage.SeatDays(t.Context(), db.Replicated(), usage.SpendQuery{From: day, To: day})
		if err != nil || len(days) != 1 {
			t.Fatalf("read the seat days: %+v, %v", days, err)
		}
		return days[0].Code
	}
	morning := seatRecord("node-a", day, "seat-1", "m")
	morning.Code = &usage.CodeDay{ChangesOpened: 2, ReviewsGiven: 1, FindingsReceived: 4}
	if err := applyRecord(t.Context(), db, recordAt(t, morning, 1)); err != nil {
		t.Fatalf("apply the morning: %v", err)
	}
	if got := read(); got != *morning.Code {
		t.Fatalf("the seat's day reads %+v, want the morning's %+v", got, *morning.Code)
	}
	applied, held, err := usage.Applied(t.Context(), db.Replicated(), morning.Subject)
	if err != nil || !held || applied.Code == nil || *applied.Code != *morning.Code {
		t.Fatalf("what the fleet holds of the day is %+v (%v, %v), want the morning's code day", applied.Code,
			held, err)
	}

	derived := seatRecord("node-a", day, "seat-1", "m")
	derived.Code = &usage.CodeDay{ChangesOpened: 3}
	if got := usage.Join(derived, applied).Code; got == nil || *got != (usage.CodeDay{ChangesOpened: 3,
		ReviewsGiven: 1, FindingsReceived: 4}) {
		t.Fatalf("the join's code day is %+v, want each figure the larger of the two", got)
	}

	if err := applyRecord(t.Context(), db, recordAt(t, seatRecord("node-a", day, "seat-1", "m"), 2)); err != nil {
		t.Fatalf("apply the afternoon: %v", err)
	}
	if got := read(); got != (usage.CodeDay{}) {
		t.Fatalf("after a record counting none the seat's day reads %+v, want zeros — a record replaces the day",
			got)
	}
}

// THE PUBLISHER CARRIES A SEAT'S CODE DAY, and none for a day with none: the
// store's counts of a seat's changes and reviews become its record's code day.
func TestThePublisherCarriesASeatsCodeDay(t *testing.T) {
	t.Parallel()
	into := openStore(t)
	p, err := usage.NewPublisher(usage.PublisherDeps{
		Store: oneSeatDay{}, Log: &loopback{t: t, into: into, node: "node-a"}, Estate: into.Replicated(),
		NodeID: "node-a", Caught: caught, Zone: func() *time.Location { return time.UTC },
		FireHistory: fireHistory,
	})
	if err != nil {
		t.Fatal(err)
	}
	recs, err := p.Records("2026-09-23", store.UsageDay{Seats: []store.UsageSeat{
		{AgentID: "seat-1", Code: store.UsageCode{ChangesOpened: 1, FindingsReceived: 2}},
		{AgentID: "seat-2"},
	}})
	if err != nil {
		t.Fatal(err)
	}
	if len(recs) != 2 || recs[0].Code == nil || *recs[0].Code != (usage.CodeDay{ChangesOpened: 1,
		FindingsReceived: 2}) || recs[1].Code != nil {
		t.Fatalf("the records carry %+v and %+v, want seat-1's code day and none for seat-2", recs[0].Code,
			recs[1].Code)
	}
}
