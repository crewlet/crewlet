package sandbox

import (
	"math"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/executorkeep"
)

// A SEAT'S WIDEST SAVE LEDGER FITS ITS BOUND, as its charge writes it: the
// charge that fills the ledger to saveLedgerCap entries, each at its widest
// instant and at the most a save is counted at (executorkeep.KeptSealedMaxBytes,
// countedFor's ceiling), and again at an int64's count of bytes.
//
// Mutation: raise saveLedgerCap to 600, or lower coord.MaxSaveLedgerBytes
// below the widest, and this is red.
func TestASeatsWidestSaveLedgerFitsItsBound(t *testing.T) {
	t.Parallel()
	until := time.Date(2026, 11, 22, 10, 0, 0, 123456789, time.UTC)
	for _, bytes := range []int64{executorkeep.KeptSealedMaxBytes, math.MaxInt64 / saveLedgerCap} {
		raw := ledgerEntries(t, saveLedgerCap-1, bytes, until)
		value, class, why := chargedLedger(raw, savedEntry{ID: uuid.New(), Until: until, Bytes: bytes},
			math.MaxInt64, until.Add(-time.Hour))
		if value == nil {
			t.Fatalf("premise: the charge filling the ledger to its cap came to %q (%s), want it written", class, why)
		}
		if len(value) > coord.MaxSaveLedgerBytes {
			t.Errorf("a seat's widest save ledger, at %d bytes a save, is stored as %d bytes, past its class's %d",
				bytes, len(value), coord.MaxSaveLedgerBytes)
		}
		t.Logf("a seat's widest save ledger, at %d bytes a save, is %d bytes", bytes, len(value))
	}
}
