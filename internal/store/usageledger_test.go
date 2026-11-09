package store_test

import (
	"reflect"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/store"
)

// THE LEDGER IS READ OFF THE RECORDS THAT END TURNS: each turn counted once
// under its wake and its outcome, its tokens summed, a turn whose end was
// recorded twice counted once, and a turn whose end states no wake or outcome
// (a build before the ledger) in no row.
func TestTheTurnLedgerIsReadOffTheRecordsThatEndTurns(t *testing.T) {
	t.Parallel()
	db := open(t)
	log := db.Events()
	base := time.Now().UTC().Truncate(time.Second).Add(-2 * time.Hour)
	at := func(min int) time.Time { return base.Add(time.Duration(min) * time.Minute) }
	words := types.TurnTally{DurationMS: 1_000, Wake: "work_item:mention", Outcome: types.OutcomeWords,
		Tokens: types.TurnTokens{Input: 100, Output: 10, AuxInput: 5}}
	moved := types.TurnTally{DurationMS: 2_000, Wake: "work_pickup", Outcome: types.OutcomeMoved,
		Tokens: types.TurnTokens{Input: 900, Output: 90, CacheRead: 300}}
	for _, rec := range []store.EventRecord{
		endedTurn(t, "e1", at(10), "seat-1", "t-1", words),
		endedTurn(t, "e2", at(20), "seat-1", "t-2", words),
		endedTurn(t, "e2-again", at(21), "seat-1", "t-2", words),
		endedTurn(t, "e3", at(30), "seat-1", "t-3", moved),
		endedTurn(t, "e4", at(40), "seat-1", "t-4", types.TurnTally{DurationMS: 500}),
	} {
		if err := log.Append(t.Context(), rec); err != nil {
			t.Fatalf("append %s: %v", rec.ID, err)
		}
	}
	day, err := db.UsageForDay(t.Context(), usageDayOf(base, base.Add(2*time.Hour)))
	if err != nil {
		t.Fatalf("derive the day: %v", err)
	}
	if len(day.Seats) != 1 {
		t.Fatalf("derived %d seats, want seat-1 alone", len(day.Seats))
	}
	want := []store.UsageLedger{
		{Wake: "work_item:mention", Outcome: "words", Turns: 2,
			Tokens: types.TurnTokens{Input: 200, Output: 20, AuxInput: 10}},
		{Wake: "work_pickup", Outcome: "moved", Turns: 1,
			Tokens: types.TurnTokens{Input: 900, Output: 90, CacheRead: 300}},
	}
	if got := day.Seats[0].Turns.Ledger; !reflect.DeepEqual(got, want) {
		t.Errorf("the ledger is\n%+v\nwant\n%+v", got, want)
	}
	if got := day.Seats[0].Turns.Count; got != 4 {
		t.Errorf("counted %d turns, want 4: the untallied one still counts as a turn", got)
	}
}
