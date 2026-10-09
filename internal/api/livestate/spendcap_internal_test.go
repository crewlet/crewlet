package livestate

import (
	"fmt"
	"runtime"
	"slices"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/tokens"
)

// A RECORD ARRIVING AT THE CAP COSTS WHAT ONE RECORD COSTS.
//
// Once a company's day passes [SpendRecordLimit], every spend record that
// arrives drops the oldest — under the projection's lock, which every /agents
// request and every socket snapshot waits on. That trim built a fresh slice of
// the whole window per arrival, a copy of 24 000 entries for each record the
// company published. It is a reslice now, with the backing array replaced only
// by append's own growth, so the bytes allocated per arrival stay a small
// multiple of one entry's rather than the window's.
//
// Mutation: rebuild the window per trim and the arrivals allocate megabytes
// each.
func TestARecordArrivingAtTheCapCostsOneRecord(t *testing.T) {
	t.Parallel()
	s := New()
	fresh := time.Now().UTC().Format(time.RFC3339Nano)
	for i := range SpendRecordLimit {
		s.foldSpend(Envelope{ID: fmt.Sprintf("w%d", i), Timestamp: fresh},
			map[string]any{"total_tokens": 1})
	}
	const arrivals = 200
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	for i := range arrivals {
		s.foldSpend(Envelope{ID: fmt.Sprintf("a%d", i), Timestamp: fresh},
			map[string]any{"total_tokens": 1})
	}
	runtime.ReadMemStats(&after)
	if len(s.spend) != SpendRecordLimit || len(s.spendIDs) != SpendRecordLimit {
		t.Fatalf("holding %d records and %d ids, want the cap's %d of each",
			len(s.spend), len(s.spendIDs), SpendRecordLimit)
	}
	if s.spend[len(s.spend)-1].EventID != fmt.Sprintf("a%d", arrivals-1) ||
		s.spend[0].EventID != fmt.Sprintf("w%d", arrivals) {
		t.Fatalf("the window runs %s … %s, want the oldest %d dropped and the newest kept",
			s.spend[0].EventID, s.spend[len(s.spend)-1].EventID, arrivals)
	}
	// One entry is a few hundred bytes; a growth step of the backing array
	// is a quarter of the window, spread over a quarter of the window's
	// arrivals. A rebuild per arrival is the whole window each time.
	window := uint64(SpendRecordLimit) * 400
	if per := (after.TotalAlloc - before.TotalAlloc) / arrivals; per > window/16 {
		t.Fatalf("each arrival at the cap allocated %d bytes, against a window of about "+
			"%d — the trim is copying the window", per, window)
	}
}

// THE CAP DROPS THE OLDEST STAMPED, the undateable first — never simply the
// first to arrive.
//
// A record that lost a cross-topic race, or was stamped by a node whose clock
// runs behind, arrives AFTER records stamped later than it. Dropping by arrival
// would keep it past newer spend; the window is held in stamp order and the
// cap takes its front. A record whose stamp does not parse goes before any of
// them, because it is the one record the window cannot age out on time and the
// cap is what bounds it ([stamp.chronological]).
func TestTheCapDropsTheOldestStampedAndTheUndateableFirst(t *testing.T) {
	t.Parallel()
	base := time.Date(2026, 6, 14, 12, 0, 0, 0, time.UTC)
	s := New(WithClock(func() time.Time { return base.Add(time.Hour) }))
	fold := func(id string, at time.Time) {
		s.foldSpend(Envelope{ID: id, Timestamp: at.Format(time.RFC3339Nano)},
			map[string]any{"total_tokens": 1})
	}
	s.foldSpend(Envelope{ID: "undateable", Timestamp: "not-a-timestamp"},
		map[string]any{"total_tokens": 1})
	for i := range SpendRecordLimit - 3 {
		fold(fmt.Sprintf("w%d", i), base.Add(time.Duration(i+10)*time.Millisecond))
	}
	// The two OLDEST stamps, arriving LAST — behind everything above.
	fold("late-oldest", base)
	fold("late-older", base.Add(time.Millisecond))
	if held := len(s.spend) + len(s.undatedSpend); held != SpendRecordLimit {
		t.Fatalf("holding %d records, want the window exactly full at %d", held, SpendRecordLimit)
	}

	fold("new-1", base.Add(30*time.Minute))
	if ids := s.spendHeld(); ids[0] != "late-oldest" || slices.Contains(ids, "undateable") {
		t.Fatalf("the window starts %v after one arrival past the cap, want the "+
			"undateable record dropped first", ids[:3])
	}
	fold("new-2", base.Add(31*time.Minute))
	fold("new-3", base.Add(32*time.Minute))
	ids := s.spendHeld()
	if slices.Contains(ids, "late-oldest") || slices.Contains(ids, "late-older") {
		t.Errorf("the window starts %v, want the two oldest STAMPED dropped although "+
			"they arrived last", ids[:3])
	}
	if ids[0] != "w0" || ids[len(ids)-1] != "new-3" {
		t.Errorf("the window runs %s … %s, want w0 … new-3", ids[0], ids[len(ids)-1])
	}
	if len(s.spendIDs) != SpendRecordLimit {
		t.Errorf("the index holds %d ids for %d records", len(s.spendIDs), SpendRecordLimit)
	}
}

// THE CAP TAKES A RECORD STAMPED AHEAD IN ITS ARRIVAL'S PLACE.
//
// Held by its own stamp, a record from a node whose clock is years fast sorted
// NEWEST, so the cap — which drops from the front — reached it only after every
// other record in the window, and a busy company kept it for good. Aged from its
// arrival, it is as old as the records that arrived beside it, and the cap
// drops it when it drops them.
//
// Mutation: age a record by its stamp alone, and the far record outlives the
// oldest record that arrived after it.
func TestTheCapTakesARecordStampedAheadInItsArrivalsPlace(t *testing.T) {
	t.Parallel()
	base := time.Date(2026, 6, 14, 12, 0, 0, 0, time.UTC)
	now := base
	s := New(WithClock(func() time.Time { return now }))
	s.foldSpend(Envelope{ID: "far", Timestamp: "2099-01-01T00:00:00Z"},
		map[string]any{"total_tokens": 1})
	// The window's worth of records, each arriving a millisecond after the
	// last and stamped on arrival: the last of them is one past the cap.
	for i := range SpendRecordLimit {
		now = base.Add(time.Duration(i+1) * time.Millisecond)
		s.foldSpend(Envelope{ID: fmt.Sprintf("w%d", i), Timestamp: now.Format(time.RFC3339Nano)},
			map[string]any{"total_tokens": 1})
	}
	ids := s.spendHeld()
	if slices.Contains(ids, "far") {
		t.Errorf("the cap kept the record stamped ahead and dropped one that arrived after "+
			"it: the window starts at %s", ids[0])
	}
	if len(ids) != SpendRecordLimit || ids[0] != "w0" {
		t.Errorf("the window holds %d records from %s, want the cap's %d from w0",
			len(ids), ids[0], SpendRecordLimit)
	}
}

// AN EXPIRY READS THE FRONT OF THE WINDOW AND STOPS.
//
// This asserts the expiry's COST rather than its result, because the result is
// the same either way: the window is held in stamp order, so whatever it has
// aged past is a prefix, and an expiry that read past the first record still
// inside the window would be a pass over every record — on every spend record
// the company published, under the projection's lock, which is the quadratic
// fill this replaced (80 s a test under the race detector at the cap). A
// record planted where the order never puts one tells the two apart: read from
// the front, it is never reached.
//
// Mutation: scan the window for aged records (slices.ContainsFunc and
// DeleteFunc, as it used to) and the planted record is dropped.
func TestAnExpiryReadsOnlyTheAgedFrontOfTheWindow(t *testing.T) {
	t.Parallel()
	base := time.Date(2026, 6, 14, 12, 0, 0, 0, time.UTC)
	s := New(WithClock(func() time.Time { return base.Add(time.Hour) }))
	for i := range 100 {
		s.foldSpend(Envelope{
			ID:        fmt.Sprintf("w%d", i),
			Timestamp: base.Add(time.Duration(i) * time.Second).Format(time.RFC3339Nano),
		}, map[string]any{"total_tokens": 1})
	}
	planted := spendEntry{
		at:     newStamp(base.Add(-2 * LiveSpendWindow).Format(time.RFC3339Nano)),
		Record: tokens.Record{EventID: "planted"},
	}
	s.spend = append(s.spend, planted)
	s.spendIDs[planted.EventID] = struct{}{}

	if s.expireSpend(base.Add(time.Hour)) {
		t.Error("an expiry with nothing aged at the front reported dropping something")
	}
	if ids := s.spendHeld(); ids[len(ids)-1] != "planted" {
		t.Fatal("the expiry dropped a record from the back of the window: it read past " +
			"the first record still inside it, which is a pass over every record")
	}

	// w0 … w49 have aged.
	if !s.expireSpend(base.Add(LiveSpendWindow + 50*time.Second)) {
		t.Fatal("the expiry dropped nothing from an aged front")
	}
	if ids := s.spendHeld(); ids[0] != "w50" || ids[len(ids)-1] != "planted" {
		t.Errorf("the window runs %s … %s, want w50 … planted", ids[0], ids[len(ids)-1])
	}
}

// AN ARRIVAL IN STAMP ORDER MOVES NOTHING THE WINDOW HOLDS.
//
// Nearly every arrival is in stamp order, and placing one is an append — the
// insertion that keeps the window ordered costs only the records stamped AFTER
// an arrival, which for these is none. A record planted out of order in the
// middle tells that apart from the two ways of keeping order that cost the
// window instead: re-sorting it on an arrival moves the planted record, and
// searching for the arrival's place from the front stops at the planted record
// and inserts the arrival there.
//
// Mutation: append and re-sort the window per arrival, or search for the
// insertion point from the front, and the arrival does not land last or the
// planted record moves.
func TestAnInOrderArrivalIsAnAppend(t *testing.T) {
	t.Parallel()
	base := time.Date(2026, 6, 14, 12, 0, 0, 0, time.UTC)
	s := New(WithClock(func() time.Time { return base.Add(time.Hour) }))
	const held = 100
	for i := range held {
		s.foldSpend(Envelope{
			ID:        fmt.Sprintf("w%d", i),
			Timestamp: base.Add(time.Duration(i) * time.Second).Format(time.RFC3339Nano),
		}, map[string]any{"total_tokens": 1})
	}
	s.spend[held/2] = spendEntry{
		at:     newStamp(base.Add(10 * time.Hour).Format(time.RFC3339Nano)),
		Record: tokens.Record{EventID: "planted"},
	}

	s.foldSpend(Envelope{
		ID:        "arrival",
		Timestamp: base.Add(held * time.Second).Format(time.RFC3339Nano),
	}, map[string]any{"total_tokens": 1})
	ids := s.spendHeld()
	if ids[len(ids)-1] != "arrival" {
		t.Errorf("the arrival landed at %d of %d: its place was searched for from the "+
			"front rather than taken at the back", slices.Index(ids, "arrival"), len(ids))
	}
	if ids[held/2] != "planted" {
		t.Errorf("the planted record moved to %d: the arrival re-sorted the window",
			slices.Index(ids, "planted"))
	}
}

// spendHeld lists the ids of every record the window holds, in its order.
func (s *LiveState) spendHeld() []string {
	ids := make([]string, 0, len(s.undatedSpend)+len(s.spend))
	for i := range s.undatedSpend {
		ids = append(ids, s.undatedSpend[i].EventID)
	}
	for i := range s.spend {
		ids = append(ids, s.spend[i].EventID)
	}
	return ids
}
