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
	s, open := nearlyFull(t, fleet)
	err := s.reserve(t.Context(), hold{Upload: uuid.NewString(), Principal: "ada.lovelace", Bytes: 20,
		Until: s.now().Add(time.Hour), Expires: s.now().Add(time.Hour)}, 0)
	var outstanding *UploadsOutstanding
	if !errors.As(err, &outstanding) {
		t.Fatalf("an intent past the pending bytes beside 85 held answered %v, want it refused", err)
	}
	if after := storedLedger(t, fleet); after.holdOf(open) >= 0 || len(after.Holds) != 1 {
		t.Errorf("after the refused intent the ledger holds %+v, want the released upload gone", after.Holds)
	}
}

// nearlyFull is a service whose principal may hold 100 pending bytes, over a
// ledger holding 85 live beside 10 of an abandoned upload, which it answers.
func nearlyFull(t *testing.T, fleet *memory.Fleet) (*Service, string) {
	t.Helper()
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
	return s, open
}

// unwritable is the fleet's reservations whose ledger writes all fail, or,
// where lose is set, all lose their race.
type unwritable struct {
	*memory.Fleet
	lose bool
}

func (u unwritable) SwapUploadLedger(context.Context, []byte, uint64) (uint64, error) {
	if u.lose {
		return 0, nil
	}
	return 0, errors.New("the store did not answer")
}

// A REFUSED RESERVATION IS ANSWERED HOWEVER ITS RELEASE'S WRITE ENDS: an
// intent past its budget is refused as outstanding even where the write of
// the early release it made on its way fails or loses every round. That write
// is best effort, and the refusal was decided before it.
//
// Mutation: answer the write's error, or errLostLedger, in changeLedger, and
// the intent reads as a store outage.
func TestARefusedReservationIsAnsweredHoweverItsReleasesWriteEnds(t *testing.T) {
	t.Parallel()
	for _, lose := range []bool{false, true} {
		fleet := memory.NewFleet()
		s, _ := nearlyFull(t, fleet)
		s.res = unwritable{Fleet: fleet, lose: lose}
		err := s.reserve(t.Context(), hold{Upload: uuid.NewString(), Principal: "ada.lovelace", Bytes: 20,
			Until: s.now().Add(time.Hour), Expires: s.now().Add(time.Hour)}, 0)
		var outstanding *UploadsOutstanding
		if !errors.As(err, &outstanding) {
			t.Errorf("an intent past its budget whose release's write fails (lost every round: %v) answered %v, "+
				"want it refused as outstanding", lose, err)
		}
	}
}

// interleaved is the fleet's reservations whose first ledger write runs
// between first, before it is sent: another node's change landing between
// this one's read and its write.
type interleaved struct {
	*memory.Fleet
	between func()
	once    sync.Once
}

func (i *interleaved) SwapUploadLedger(ctx context.Context, value []byte, revision uint64) (uint64, error) {
	i.once.Do(i.between)
	return i.Fleet.SwapUploadLedger(ctx, value, revision)
}

// A RELEASE LEAVES A HOLD A RETRIED INTENT TOOK UP: a change releases an
// abandoned upload, deleting its record, and before its write lands a retry
// of that upload's intent (one id under one operation key) finds no record
// and reserves again, taking up the hold. The change loses its round, and its
// next drops the hold only were it the one it released, so the retried
// intent's bytes stay held.
//
// Mutation: drop every released upload's hold in a later round, unchecked,
// and the retried intent's reservation is gone.
func TestAReleaseLeavesAHoldARetriedIntentTookUp(t *testing.T) {
	t.Parallel()
	fleet := memory.NewFleet()
	now := time.Date(2026, 11, 22, 10, 0, 0, 0, time.UTC)
	retry := &Service{res: fleet, now: func() time.Time { return now.Add(time.Second) },
		limits: func() artifacts.Limits { return artifacts.Limits{PendingBytes: 100, CompanyBytes: 1 << 40} }}
	res := &interleaved{Fleet: fleet}
	s := &Service{res: res, now: func() time.Time { return now }}
	abandoned := abandonedBehind(t, s, fleet, 0, 10)
	res.between = func() {
		later := now.Add(time.Second)
		if err := retry.reserve(t.Context(), hold{Upload: abandoned, Principal: "ada.lovelace", Bytes: 10,
			Until: later.Add(time.Hour), Expires: later.Add(10 * time.Minute)}, 0); err != nil {
			t.Errorf("the retried intent's reservation: %v", err)
		}
	}
	if err := s.changeLedger(t.Context(), func(*ledger, time.Time) error { return nil }); err != nil {
		t.Fatalf("change the ledger: %v", err)
	}
	after := storedLedger(t, fleet)
	if i := after.holdOf(abandoned); i < 0 || after.Holds[i].Bytes != 10 {
		t.Errorf("after the release lost its round the ledger holds %+v, want the retried intent's 10 bytes",
			after.Holds)
	}
}

// A RETRIED INTENT TAKES UP THE HOLD ITS FIRST ATTEMPT LEFT: an intent whose
// first attempt wrote its hold and no record reserves again, and the hold
// becomes the retry's, its window and lapse and a new attempt, so the record
// the retry writes is covered for as long as its window runs and a release
// that read the old hold tells the two apart.
//
// Mutation: answer nil from reserve on any hold of the upload, or keep the
// first attempt's Attempt, and the old hold stands.
func TestARetriedIntentTakesUpTheHoldItsFirstAttemptLeft(t *testing.T) {
	t.Parallel()
	fleet := memory.NewFleet()
	now := time.Date(2026, 11, 22, 10, 0, 0, 0, time.UTC)
	s := &Service{res: fleet, now: func() time.Time { return now },
		limits: func() artifacts.Limits { return artifacts.Limits{PendingBytes: 100, CompanyBytes: 1 << 40} }}
	upload := uuid.NewString()
	first := hold{Upload: upload, Principal: "ada.lovelace", Bytes: 10, Until: now.Add(time.Hour),
		Expires: now.Add(5 * time.Minute)}
	retried := first
	retried.Until, retried.Expires = now.Add(2*time.Hour), now.Add(30*time.Minute)
	var attempts []uint32
	for _, h := range []hold{first, retried} {
		if err := s.reserve(t.Context(), h, 0); err != nil {
			t.Fatalf("reserve %+v: %v", h, err)
		}
		after := storedLedger(t, fleet)
		i := after.holdOf(upload)
		if i < 0 || len(after.Holds) != 1 {
			t.Fatalf("after reserving %+v the ledger holds %+v, want the one hold", h, after.Holds)
		}
		attempts = append(attempts, after.Holds[i].Attempt)
		if got := after.Holds[i]; !got.Until.Equal(h.Until) || !got.Expires.Equal(h.Expires) {
			t.Errorf("after reserving %+v the hold is %+v, want its window and lapse", h, got)
		}
	}
	if attempts[0] == attempts[1] {
		t.Errorf("the retried intent kept the first attempt's %d, want an attempt of its own", attempts[0])
	}
}

// A TAKEN-UP HOLD IS NEVER WEAKENED: a first attempt reserves a direct hold
// and a retry of the same intent from an origin the bucket is not handed to
// takes it up. Either attempt's record may be the one written, and a direct
// put may still land, so the hold stays direct, keeps the later window and
// lapse, and is never released early.
//
// Mutation: take up the retry's Direct alone in reserve, and the hold is
// released with the first attempt's direct record.
func TestATakenUpHoldIsNeverWeakened(t *testing.T) {
	t.Parallel()
	fleet := memory.NewFleet()
	now := time.Date(2026, 11, 22, 10, 0, 0, 0, time.UTC)
	s := &Service{res: fleet, now: func() time.Time { return now },
		limits: func() artifacts.Limits { return artifacts.Limits{PendingBytes: 100, CompanyBytes: 1 << 40} }}
	r := &record{ID: uuid.NewString(), Principal: "ada.lovelace", Purpose: purposePublish, Created: now,
		Expires: now.Add(5 * time.Minute), State: stateOpen}
	if wrote, err := s.writeRecord(t.Context(), r, 0); err != nil || wrote == 0 {
		t.Fatalf("premise: write the first attempt's record = %d, %v", wrote, err)
	}
	first := hold{Upload: r.ID, Principal: r.Principal, Bytes: 10, Until: now.Add(2 * time.Hour),
		Expires: r.Expires, Direct: true}
	retry := first
	retry.Direct, retry.Until, retry.Expires = false, now.Add(time.Hour), now.Add(10*time.Minute)
	for _, h := range []hold{first, retry} {
		if err := s.reserve(t.Context(), h, 0); err != nil {
			t.Fatalf("reserve %+v: %v", h, err)
		}
	}
	after := storedLedger(t, fleet)
	if i := after.holdOf(r.ID); i < 0 || !after.Holds[i].Direct || !after.Holds[i].Until.Equal(first.Until) ||
		!after.Holds[i].Expires.Equal(retry.Expires) {
		t.Fatalf("after the retry the ledger holds %+v, want one direct hold to %v, its window to %v",
			after.Holds, first.Until, retry.Expires)
	}
	later := now.Add(time.Hour)
	s.now = func() time.Time { return later }
	if err := s.changeLedger(t.Context(), func(*ledger, time.Time) error { return nil }); err != nil {
		t.Fatalf("change the ledger past the window: %v", err)
	}
	held := storedLedger(t, fleet)
	if kept, _, err := s.readRecord(t.Context(), r.ID); err != nil || kept == nil || held.holdOf(r.ID) < 0 {
		t.Errorf("past the window the direct upload's record is %v (%v) and its holds %+v; want both kept",
			kept, err, held.Holds)
	}
}

// A RELEASE DROPS A HOLD ANOTHER NODE ONLY RECHECKED: a change releases an
// abandoned upload, deleting its record, and before its write lands another
// node's change reads the deleted record and moves the hold's window on. The
// release loses its round and drops the hold in the next all the same: it is
// still the reservation it released, which only a retry's reserve replaces,
// so the deleted bytes are not charged for a day.
//
// Mutation: compare the hold's window in hold.same, and the released hold
// stays.
func TestAReleaseDropsAHoldAnotherNodeOnlyRechecked(t *testing.T) {
	t.Parallel()
	fleet := memory.NewFleet()
	now := time.Date(2026, 11, 22, 10, 0, 0, 0, time.UTC)
	other := &Service{res: fleet, now: func() time.Time { return now }}
	res := &interleaved{Fleet: fleet}
	s := &Service{res: res, now: func() time.Time { return now }}
	abandoned := abandonedBehind(t, s, fleet, 0, 10)
	res.between = func() {
		if err := other.changeLedger(t.Context(), func(*ledger, time.Time) error { return nil }); err != nil {
			t.Errorf("another node's change: %v", err)
		}
	}
	if err := s.changeLedger(t.Context(), func(*ledger, time.Time) error { return nil }); err != nil {
		t.Fatalf("change the ledger: %v", err)
	}
	if after := storedLedger(t, fleet); after.holdOf(abandoned) >= 0 {
		t.Errorf("after the release lost its round to a recheck the ledger holds %+v, want the released hold gone",
			after.Holds)
	}
}
