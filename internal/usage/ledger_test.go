package usage_test

import (
	"reflect"
	"testing"

	"github.com/crewlet/crewlet/internal/usage"
)

func seatWithLedger(node, day, seat string, rows ...usage.LedgerRow) usage.Record {
	r := seatRecord(node, day, seat, "m-1")
	r.Ledger = rows
	return r
}

// A SEAT-DAY'S TURN LEDGER IS REPLACED BY ITS NEWER RECORD AND SUMMED ACROSS
// NODES: the read answers what the company's turns did over the days, each
// node's day counting the turns that ended on it.
func TestASeatDaysTurnLedgerIsReplacedAndSummedAcrossNodes(t *testing.T) {
	t.Parallel()
	db := openStore(t)
	const day = "2026-09-23"
	morning := seatWithLedger("node-a", day, "seat-1",
		usage.LedgerRow{Wake: "work_item:mention", Outcome: "words", Turns: 1, Input: 100})
	afternoon := seatWithLedger("node-a", day, "seat-1",
		usage.LedgerRow{Wake: "work_item:mention", Outcome: "words", Turns: 4, Input: 400, AuxInput: 40},
		usage.LedgerRow{Wake: "work_pickup", Outcome: "moved", Turns: 1, Input: 900, Output: 90})
	other := seatWithLedger("node-b", day, "seat-1",
		usage.LedgerRow{Wake: "work_item:mention", Outcome: "words", Turns: 2, Input: 200})
	for i, r := range []usage.Record{morning, afternoon, other} {
		if err := applyRecord(t.Context(), db, recordAt(t, r, uint64(i+1))); err != nil {
			t.Fatalf("apply record %d: %v", i, err)
		}
	}
	rows, err := usage.TurnLedger(t.Context(), db.Replicated(), usage.SpendQuery{From: day, To: day})
	if err != nil {
		t.Fatalf("read the ledger: %v", err)
	}
	want := []usage.TurnLedgerRow{
		{AgentID: "seat-1", Handle: "dev", Wake: "work_item:mention", Outcome: "words", Turns: 6,
			Input: 600, AuxInput: 40},
		{AgentID: "seat-1", Handle: "dev", Wake: "work_pickup", Outcome: "moved", Turns: 1,
			Input: 900, Output: 90},
	}
	if !reflect.DeepEqual(rows, want) {
		t.Errorf("the ledger read\n%+v\nwant node-a's afternoon and node-b's day summed:\n%+v", rows, want)
	}
}

// WHAT THE FLEET HOLDS OF A SEAT'S LEDGER JOINS FIGURE BY FIGURE: the larger of
// each, a row only the fleet holds kept, the cap's count carried.
func TestWhatTheFleetHoldsOfASeatsLedgerJoinsFigureByFigure(t *testing.T) {
	t.Parallel()
	db := openStore(t)
	const day = "2026-09-23"
	held := seatWithLedger("node-a", day, "seat-1",
		usage.LedgerRow{Wake: "chat_message:dm", Outcome: "words", Turns: 3, Input: 300},
		usage.LedgerRow{Wake: "schedule:standup", Outcome: "nothing", Turns: 1, Input: 50})
	held.LedgerElided = 2
	if err := applyRecord(t.Context(), db, recordAt(t, held, 1)); err != nil {
		t.Fatalf("apply: %v", err)
	}
	applied, ok, err := usage.Applied(t.Context(), db.Replicated(), held.Subject)
	if err != nil || !ok {
		t.Fatalf("read what the fleet holds: %v, %v", ok, err)
	}
	if !reflect.DeepEqual(applied.Ledger, held.Ledger) || applied.LedgerElided != 2 {
		t.Fatalf("the fleet holds %+v (elided %d), want the record's %+v (elided 2)",
			applied.Ledger, applied.LedgerElided, held.Ledger)
	}
	derived := seatWithLedger("node-a", day, "seat-1",
		usage.LedgerRow{Wake: "chat_message:dm", Outcome: "words", Turns: 2, Input: 400})
	got := usage.Join(derived, applied)
	want := []usage.LedgerRow{
		{Wake: "chat_message:dm", Outcome: "words", Turns: 3, Input: 400},
		{Wake: "schedule:standup", Outcome: "nothing", Turns: 1, Input: 50},
	}
	if !reflect.DeepEqual(got.Ledger, want) || got.LedgerElided != 2 {
		t.Errorf("joined %+v (elided %d), want %+v (elided 2)", got.Ledger, got.LedgerElided, want)
	}
}

// A LEDGER ROW NAMES ITS WAKE AND A KNOWN OUTCOME, ONCE, AND COUNTS NOTHING
// NEGATIVE: a record that does not is refused before it is published.
func TestALedgerRowIsHeldToItsShape(t *testing.T) {
	t.Parallel()
	for name, rows := range map[string][]usage.LedgerRow{
		"no wake":         {{Outcome: "moved", Turns: 1}},
		"unknown outcome": {{Wake: "work_pickup", Outcome: "busy", Turns: 1}},
		"twice": {{Wake: "work_pickup", Outcome: "moved", Turns: 1},
			{Wake: "work_pickup", Outcome: "moved", Turns: 2}},
		"negative": {{Wake: "work_pickup", Outcome: "moved", Turns: -1}},
	} {
		r := seatWithLedger("node-a", "2026-09-23", "seat-1", rows...)
		if _, err := r.Encode(); err == nil {
			t.Errorf("%s: a ledger %+v was encoded, want it refused", name, rows)
		}
	}
}
