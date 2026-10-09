package storetest

import (
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/store"
)

// WriteEvents writes recs into log in ONE transaction and leaves them as
// ordinary rows, for a fixture that seeds more rows than one at a time is
// worth: [store.EventLog.Append] commits each record on its own, which is
// right for a log written as events happen and is one fsync'd commit per row
// for a fixture — the store contract's 20,001-row spend window spent 69 s of
// the package's wall clock appending.
//
// IT IS THE PRODUCTION WRITE, not a test-only one. The batch goes through
// [store.EventLog.WriteCustody], which builds and inserts each record with the
// same code Append does, so every derived column a reader folds — the phase,
// the spend — is what Append would have stored. The batch is then settled as
// KEPT ([store.EventLog.SettleCustody]), which deletes only its unsettled
// record: an unsettled batch is a state of its own, which a reader of the
// custody table would see, so it is never left behind.
//
// One difference from a loop of Appends is deliberate: a record the log
// refuses refuses the whole batch, and fails the test naming it, rather than
// leaving the rest written.
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
