package store_test

import (
	"reflect"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/store"
)

// THE LEDGER IS READ OFF THE RECORDS THAT END TURNS AND THE HELD WAKES: each
// turn counted once under its wake and its outcome, its tokens and refused calls
// summed, a turn whose end was recorded twice counted once, a turn whose end
// states no wake or outcome (a build before the ledger) counted as untallied in
// no row, and each held wake under the key a woken turn from it would take.
func TestTheTurnLedgerIsReadOffTheRecordsThatEndTurns(t *testing.T) {
	t.Parallel()
	db := open(t)
	log := db.Events()
	base := time.Now().UTC().Truncate(time.Second).Add(-2 * time.Hour)
	at := func(min int) time.Time { return base.Add(time.Duration(min) * time.Minute) }
	words := types.TurnTally{DurationMS: 1_000, Wake: "work_item:mention", Outcome: types.OutcomeWords,
		Tokens: types.TurnTokens{Input: 100, Output: 10, AuxInput: 5}}
	moved := types.TurnTally{DurationMS: 2_000, Wake: "work_pickup", Outcome: types.OutcomeMoved,
		Tokens:   types.TurnTokens{Input: 900, Output: 90, CacheRead: 300},
		Refusals: []types.RefusalTally{{Tool: "merge_change", Class: "forbidden", Calls: 2}}}
	held := func(id string, at time.Time) store.EventRecord {
		return usageRecord(t, id, "wake_held", at, "seat-1", types.WakeHeld{Agent: "seat-1",
			AgentHandle: "dev", NotificationSource: "work",
			Origin: types.TriggerOrigin{Kind: types.OriginWorkItem, ID: "W-1", Reason: "mention"}})
	}
	for _, rec := range []store.EventRecord{
		endedTurn(t, "e1", at(10), "seat-1", "t-1", words),
		endedTurn(t, "e2", at(20), "seat-1", "t-2", words),
		endedTurn(t, "e2-again", at(21), "seat-1", "t-2", words),
		endedTurn(t, "e3", at(30), "seat-1", "t-3", moved),
		endedTurn(t, "e4", at(40), "seat-1", "t-4", types.TurnTally{DurationMS: 500}),
		held("h1", at(50)), held("h2", at(51)),
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
		{Wake: "work_item:mention", Disposition: "held", Turns: 2},
		{Wake: "work_item:mention", Disposition: "started", Outcome: "words", Turns: 2,
			Tokens: types.TurnTokens{Input: 200, Output: 20, AuxInput: 10}},
		{Wake: "work_pickup", Disposition: "started", Outcome: "moved", Turns: 1,
			Tokens: types.TurnTokens{Input: 900, Output: 90, CacheRead: 300}},
	}
	turns := day.Seats[0].Turns
	if !reflect.DeepEqual(turns.Ledger, want) {
		t.Errorf("the ledger is\n%+v\nwant\n%+v", turns.Ledger, want)
	}
	if turns.Count != 4 || turns.Untallied != 1 {
		t.Errorf("counted %d turns, %d untallied; want 4, the untallied one in no row but counted", turns.Count,
			turns.Untallied)
	}
	if want := []types.RefusalTally{{Tool: "merge_change", Class: "forbidden", Calls: 2}}; !reflect.DeepEqual(turns.Refusals, want) {
		t.Errorf("refusals %+v, want %+v", turns.Refusals, want)
	}
}

// A HELD WAKE MOVES ITS DAY'S FINGERPRINT: it is counted into the ledger, so a
// day whose only change is a held wake landing must be derived again.
func TestAHeldWakeMovesTheDaysFingerprint(t *testing.T) {
	t.Parallel()
	db := open(t)
	day := time.Now().UTC().Truncate(24 * time.Hour).Add(-24 * time.Hour)
	w := usageDayOf(day, day.Add(24*time.Hour))
	before, err := db.UsageMark(t.Context(), w)
	if err != nil {
		t.Fatalf("fingerprint the empty day: %v", err)
	}
	rec := usageRecord(t, "h1", "wake_held", day.Add(time.Hour), "seat-1", types.WakeHeld{Agent: "seat-1",
		AgentHandle: "dev", NotificationSource: "chat",
		Origin: types.TriggerOrigin{Kind: types.OriginChatMessage, ID: "m-1", Reason: "quiet"}})
	if err := db.Events().Append(t.Context(), rec); err != nil {
		t.Fatalf("append the held wake: %v", err)
	}
	after, err := db.UsageMark(t.Context(), w)
	if err != nil {
		t.Fatalf("fingerprint the day: %v", err)
	}
	if after == before {
		t.Error("a held wake landed and the day's fingerprint did not move, so the day is never derived again")
	}
}
