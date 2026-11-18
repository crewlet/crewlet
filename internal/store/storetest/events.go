package storetest

import (
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/store"
)

// WriteEvents writes recs into log in ONE transaction and leaves them as
// ordinary rows, for a fixture that seeds more rows than one at a time is
// worth: [store.EventLog.Append] commits each record on its own, one fsync per
// row (the 20,001-row spend window took 69 s).
//
// IT IS THE PRODUCTION WRITE: [store.EventLog.WriteCustody] builds each record
// with Append's code, so every derived column is what Append would store, and
// the batch is settled as KEPT ([store.EventLog.SettleCustody]) since a reader
// of the custody table would see it unsettled. Unlike a loop of Appends, a
// record the log refuses refuses the whole batch and fails the test naming it.
func WriteEvents(t testing.TB, log *store.EventLog, recs []store.EventRecord) {
	t.Helper()
	if len(recs) == 0 {
		return
	}
	batch := "storetest-" + uuid.NewString()
	if err := log.WriteCustody(t.Context(), store.CustodyBatch{
		ID: batch, Origin: "storetest", Records: recs,
	}, time.Now()); err != nil {
		t.Fatalf("storetest: write %d events: %v", len(recs), err)
	}
	if err := log.SettleCustody(t.Context(), batch, true); err != nil {
		t.Fatalf("storetest: settle the %d events written: %v", len(recs), err)
	}
}
