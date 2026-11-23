package artifactupload

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/artifacts"
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

// abandonedBehind writes a ledger of claims expired holds whose records a
// publish has claimed, then one of bytes whose upload is open and whose window
// closed, each of the claims holding a byte, and answers that upload.
func abandonedBehind(t *testing.T, s *Service, fleet *memory.Fleet, claims int, bytes int64) string {
	t.Helper()
	ctx, now := t.Context(), s.now()
	l := ledger{V: ledgerVersion}
	for i := range claims + 1 {
		r := &record{ID: uuid.NewString(), Principal: "ada.lovelace", Purpose: purposePublish, Created: now,
			Expires: now.Add(-time.Minute), State: stateFinalizing, FinalizeKey: "a-publish"}
		held := int64(1)
		if i == claims {
			r.State, r.FinalizeKey, held = stateOpen, "", bytes
		}
		if wrote, err := s.writeRecord(ctx, r, 0); err != nil || wrote == 0 {
			t.Fatalf("premise: write an upload's record = %d, %v", wrote, err)
		}
		l.Holds = append(l.Holds, hold{Upload: r.ID, Principal: r.Principal, Bytes: held,
			Until: now.Add(time.Hour), Expires: r.Expires})
	}
	raw, err := json.Marshal(l)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fleet.SwapUploadLedger(ctx, raw, 0); err != nil {
		t.Fatal(err)
	}
	return l.Holds[claims].Upload
}

// storedLedger is the ledger as the store holds it.
func storedLedger(t *testing.T, fleet *memory.Fleet) ledger {
	t.Helper()
	raw, _, err := fleet.UploadLedger(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	var l ledger
	if err := json.Unmarshal(raw, &l); err != nil {
		t.Fatal(err)
	}
	return l
}

// AN ABANDONED UPLOAD IS RELEASED BEHIND HOLDS THAT CANNOT BE: earlyReads
// expired holds whose records a publish has claimed lead the ledger, and one
// open upload whose window closed follows them. Each change reads at most
// earlyReads records and moves every hold it could not release on by
// earlyRecheck, so the second change reads and releases the open one rather
// than reading the same claimed records again, whether the changes write or
// refuse.
//
// Mutation: leave a kept hold's Expires where it was, and the open upload's
// hold is never read; write nothing on a refused change, and the refusing
// case keeps it.
func TestAnAbandonedUploadIsReleasedBehindHoldsThatCannotBe(t *testing.T) {
	t.Parallel()
	errFull := errors.New("the budget is full")
	for _, c := range []struct {
		name   string
		change func(*ledger, time.Time) error
	}{
		{"changes that write", func(*ledger, time.Time) error { return nil }},
		{"changes that refuse", func(*ledger, time.Time) error { return errFull }},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			fleet := memory.NewFleet()
			now := time.Date(2026, 11, 22, 10, 0, 0, 0, time.UTC)
			s := &Service{res: fleet, now: func() time.Time { return now }}
			open := abandonedBehind(t, s, fleet, earlyReads, 1)
			for change := 1; change <= 2; change++ {
				if err := s.changeLedger(t.Context(), c.change); err != nil && !errors.Is(err, errFull) {
					t.Fatalf("change %d of the ledger: %v", change, err)
				}
			}
			after := storedLedger(t, fleet)
			if after.holdOf(open) >= 0 {
				t.Errorf("after two changes the abandoned upload %s is still held behind %d it cannot release",
					open, earlyReads)
			}
			if len(after.Holds) != earlyReads {
				t.Errorf("after two changes the ledger holds %d, want the %d claimed uploads'", len(after.Holds),
					earlyReads)
			}
		})
	}
}

// A REFUSED RESERVATION KEEPS THE RELEASE IT MADE: an intent past the
// principal's pending bytes releases an abandoned upload on its way, deleting
// its record, and the room freed is too little, so the intent is refused. The
// stored ledger no longer holds the released upload, which would otherwise
// count against the budget for a day with nothing left to release it.
//
// Mutation: write nothing on a refused change, and the released hold stays.
func TestARefusedReservationKeepsTheReleaseItMade(t *testing.T) {
	t.Parallel()
	fleet := memory.NewFleet()
	now := time.Date(2026, 11, 22, 10, 0, 0, 0, time.UTC)
	s := &Service{res: fleet, now: func() time.Time { return now },
		limits: func() artifacts.Limits { return artifacts.Limits{PendingBytes: 100, CompanyBytes: 1 << 40} }}
	open := abandonedBehind(t, s, fleet, 0, 10)
	l := storedLedger(t, fleet)
	l.Holds = append(l.Holds, hold{Upload: uuid.NewString(), Principal: "ada.lovelace", Bytes: 85,
		Until: now.Add(time.Hour), Expires: now.Add(time.Hour)})
	raw, err := json.Marshal(l)
	if err != nil {
		t.Fatal(err)
	}
	_, rev, err := fleet.UploadLedger(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fleet.SwapUploadLedger(t.Context(), raw, rev); err != nil {
		t.Fatal(err)
	}
	err = s.reserve(t.Context(), hold{Upload: uuid.NewString(), Principal: "ada.lovelace", Bytes: 20,
		Until: now.Add(time.Hour), Expires: now.Add(time.Hour)}, 0)
	var outstanding *UploadsOutstanding
	if !errors.As(err, &outstanding) {
		t.Fatalf("an intent past the pending bytes beside 85 held answered %v, want it refused", err)
	}
	if after := storedLedger(t, fleet); after.holdOf(open) >= 0 || len(after.Holds) != 1 {
		t.Errorf("after the refused intent the ledger holds %+v, want the released upload gone", after.Holds)
	}
}
