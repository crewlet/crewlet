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

// AN ABANDONED UPLOAD IS RELEASED BEHIND HOLDS THAT CANNOT BE: earlyReads
// expired holds whose records a publish has claimed lead the ledger, and one
// open upload whose window closed follows them. Each change reads at most
// earlyReads records and moves every hold it could not release on by
// earlyRecheck, so the second change reads and releases the open one rather
// than reading the same claimed records again.
//
// Mutation: leave a kept hold's Expires where it was, and the open upload's
// hold is never read.
func TestAnAbandonedUploadIsReleasedBehindHoldsThatCannotBe(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	fleet := memory.NewFleet()
	now := time.Date(2026, 11, 22, 10, 0, 0, 0, time.UTC)
	s := &Service{res: fleet, now: func() time.Time { return now }}
	l := ledger{V: ledgerVersion}
	for i := range earlyReads + 1 {
		r := &record{ID: uuid.NewString(), Principal: "ada.lovelace", Purpose: purposePublish, Created: now,
			Expires: now.Add(-time.Minute), State: stateFinalizing, FinalizeKey: "a-publish"}
		if i == earlyReads {
			r.State, r.FinalizeKey = stateOpen, ""
		}
		if wrote, err := s.writeRecord(ctx, r, 0); err != nil || wrote == 0 {
			t.Fatalf("premise: write an upload's record = %d, %v", wrote, err)
		}
		l.Holds = append(l.Holds, hold{Upload: r.ID, Principal: r.Principal, Bytes: 1,
			Until: now.Add(time.Hour), Expires: r.Expires})
	}
	raw, err := json.Marshal(l)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fleet.SwapUploadLedger(ctx, raw, 0); err != nil {
		t.Fatal(err)
	}
	open := l.Holds[earlyReads].Upload
	for change := 1; change <= 2; change++ {
		if err := s.changeLedger(ctx, func(*ledger, time.Time) error { return nil }); err != nil {
			t.Fatalf("change %d of the ledger: %v", change, err)
		}
	}
	raw, _, err = fleet.UploadLedger(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var after ledger
	if err := json.Unmarshal(raw, &after); err != nil {
		t.Fatal(err)
	}
	if after.holdOf(open) >= 0 {
		t.Errorf("after two changes the abandoned upload %s is still held behind %d it cannot release",
			open, earlyReads)
	}
	if len(after.Holds) != earlyReads {
		t.Errorf("after two changes the ledger holds %d, want the %d claimed uploads'", len(after.Holds), earlyReads)
	}
}
