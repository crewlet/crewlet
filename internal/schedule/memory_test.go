package schedule_test

import (
	"testing"

	"github.com/crewlet/crewlet/internal/schedule"
	"github.com/crewlet/crewlet/internal/schedule/scheduletest"
)

// TestMemoryLedgerContract certifies the in-memory twin against the shared
// suite. The SQL backend runs the SAME suite from
// internal/schedule/sqlledger — a divergence between them is a failing case
// there or here, never a production surprise on whichever node runs the other.
func TestMemoryLedgerContract(t *testing.T) {
	scheduletest.Run(t, func(t *testing.T) schedule.Ledger {
		return schedule.NewMemoryLedger()
	})
}

// TestTheZeroMemoryLedgerIsUsable pins the zero value, which no contract case
// can reach: the suite is handed a constructed ledger. A &MemoryLedger{} with
// a nil map must claim rather than panic, because a struct field left unset is
// exactly how one arrives.
func TestTheZeroMemoryLedgerIsUsable(t *testing.T) {
	t.Parallel()
	var l schedule.MemoryLedger
	ok, err := l.Claim(t.Context(), schedule.Run{FireKey: schedule.FireKey{ScopeID: "qa"}})
	if err != nil || !ok {
		t.Fatalf("Claim on a zero MemoryLedger = %v, %v; want true, nil", ok, err)
	}
	rows, err := l.Recent(t.Context(), 10)
	if err != nil || len(rows) != 1 {
		t.Fatalf("Recent = %d rows, %v; want 1, nil", len(rows), err)
	}
}
