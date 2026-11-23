package artifactupload

import (
	"context"
	"encoding/json"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/coord/memory"
)

// lossyLedger is the fleet's reservations, counting the upload records read
// and losing the ledger's first writes, as a contended ledger does.
type lossyLedger struct {
	*memory.Fleet

	mu    sync.Mutex
	reads int
	lose  int
}

func (l *lossyLedger) UploadRecord(ctx context.Context, upload string) ([]byte, uint64, error) {
	l.mu.Lock()
	l.reads++
	l.mu.Unlock()
	return l.Fleet.UploadRecord(ctx, upload)
}

func (l *lossyLedger) SwapUploadLedger(ctx context.Context, value []byte, revision uint64) (uint64, error) {
	l.mu.Lock()
	lost := l.lose > 0
	l.lose--
	l.mu.Unlock()
	if lost {
		return 0, nil
	}
	return l.Fleet.SwapUploadLedger(ctx, value, revision)
}

// A LEDGER CHANGE READS AT MOST earlyReads RECORDS, across all its rounds,
// to release abandoned uploads: each read is a record's crossing inside the
// request that happened to find them. Twenty abandoned holds whose records a
// publish has claimed release nothing, and a change that loses two rounds
// still reads four records, not four a round or twenty.
//
// Mutation: count releases rather than reads in releaseAbandoned, and the
// change reads every abandoned hold's record.
func TestALedgerChangeReadsABoundedNumberOfRecords(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	res := &lossyLedger{Fleet: memory.NewFleet(), lose: 2}
	now := time.Date(2026, 11, 22, 10, 0, 0, 0, time.UTC)
	s := &Service{res: res, now: func() time.Time { return now }}
	l := ledger{V: ledgerVersion}
	for range 20 {
		r := &record{ID: uuid.NewString(), Principal: "ada.lovelace", Purpose: purposePublish, Created: now,
			Expires: now.Add(-time.Minute), State: stateFinalizing, FinalizeKey: "a-publish"}
		if wrote, err := s.writeRecord(ctx, r, 0); err != nil || wrote == 0 {
			t.Fatalf("premise: write an abandoned upload's record = %d, %v", wrote, err)
		}
		l.Holds = append(l.Holds, hold{Upload: r.ID, Principal: r.Principal, Bytes: 1,
			Until: now.Add(time.Hour), Expires: r.Expires})
	}
	raw, err := json.Marshal(l)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := res.Fleet.SwapUploadLedger(ctx, raw, 0); err != nil {
		t.Fatal(err)
	}
	res.reads = 0
	if err := s.changeLedger(ctx, func(*ledger, time.Time) error { return nil }); err != nil {
		t.Fatalf("change the ledger: %v", err)
	}
	if res.reads != earlyReads {
		t.Errorf("a change over 20 abandoned holds read %d records across its rounds, want %d", res.reads,
			earlyReads)
	}
}
