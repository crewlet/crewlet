package ledgerstore_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/agent/ledger/ledgerstore"
	coordmemory "github.com/crewlet/crewlet/internal/coord/memory"
)

var base = time.Date(2026, 8, 20, 9, 0, 0, 0, time.UTC)

func completionImpls(t *testing.T) map[string]ledgerstore.Completions {
	t.Helper()
	// The SQL implementation is GONE, not omitted: the completion ledger
	// has to be agreed across the fleet, so it moved to the coordination
	// store — see internal/coord/fleet.go. The twin here and the fleet
	// implementation over the memory backend are the two this contract has,
	// and internal/coord/coordtest certifies the backends underneath.
	return map[string]ledgerstore.Completions{
		"memory": ledgerstore.NewMemoryCompletions(),
		"fleet":  ledgerstore.NewFleetCompletions(coordmemory.NewFleet()),
	}
}

func TestARecordedTriggerReadsAsWorked(t *testing.T) {
	t.Parallel()
	for name, s := range completionImpls(t) {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			if err := s.Record(ctx, "ceo", "wk-1", "turn-1", base); err != nil {
				t.Fatalf("Record: %v", err)
			}
			got := s.Worked(ctx, "ceo", []string{"wk-1", "wk-2"})
			if !got["wk-1"] {
				t.Error("a recorded key did not read as worked")
			}
			if got["wk-2"] {
				t.Error("an unrecorded key read as worked")
			}
		})
	}
}

func TestTheLedgerIsPerSeat(t *testing.T) {
	t.Parallel()
	// Two seats answering the same trigger is two turns, not one. Sharing
	// the key space would make whichever seat ran first silence the other.
	for name, s := range completionImpls(t) {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			if err := s.Record(ctx, "ceo", "wk-1", "t1", base); err != nil {
				t.Fatalf("Record: %v", err)
			}
			if s.Worked(ctx, "cto", []string{"wk-1"})["wk-1"] {
				t.Error("one seat's completion silenced another's")
			}
		})
	}
}

func TestAnEmptyKeyIsNeverRecordedOrMatched(t *testing.T) {
	t.Parallel()
	// '' is the documented "a turn with no ledgerable trigger": it skips
	// the guard because there is nothing to collapse. Recording it would
	// make the first such turn silence every other one.
	for name, s := range completionImpls(t) {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			if err := s.Record(ctx, "ceo", "", "t1", base); err != nil {
				t.Fatalf("Record: %v", err)
			}
			if s.Worked(ctx, "ceo", []string{""})[""] {
				t.Error("an empty key read as worked")
			}
			// And it must not poison a lookup that also asks about a real
			// key alongside it.
			if err := s.Record(ctx, "ceo", "wk-1", "t2", base); err != nil {
				t.Fatalf("Record: %v", err)
			}
			got := s.Worked(ctx, "ceo", []string{"", "wk-1"})
			if !got["wk-1"] {
				t.Error("a real key was lost when queried beside an empty one")
			}
		})
	}
}

func TestRecordingTwiceIsNotAnError(t *testing.T) {
	t.Parallel()
	// Two nodes completing one trigger is the case the ledger EXISTS to
	// collapse, so losing the write is the ordinary outcome and not a
	// failure to report. The record itself is first-writer-wins, which
	// internal/coord/coordtest asserts against every backend — including
	// that the second write does not move the record's age past the
	// retention the bucket enforces.
	for name, s := range completionImpls(t) {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			if err := s.Record(ctx, "ceo", "wk-1", "t1", base); err != nil {
				t.Fatalf("Record: %v", err)
			}
			if err := s.Record(ctx, "ceo", "wk-1", "t2", base.Add(10*time.Hour)); err != nil {
				t.Fatalf("second Record: %v", err)
			}
			if !s.Worked(ctx, "ceo", []string{"wk-1"})["wk-1"] {
				t.Error("the key stopped being worked after a second record")
			}
			// AND WHEN IT WAS WORKED is the first record's instant, which
			// the work pickup spaces a recheck from.
			if at, ok := s.WorkedAt(ctx, "ceo", []string{"wk-1", "wk-2"})["wk-1"]; !ok || !at.Equal(base) {
				t.Errorf("the key reads worked at %s (%v), want the first record's %s", at, ok, base)
			}
			if _, ok := s.WorkedAt(ctx, "ceo", []string{"wk-2"})["wk-2"]; ok {
				t.Error("an unrecorded key reads as worked")
			}
		})
	}
}

// An UNKEYED turn writes nothing at all.
//
// Worked already refuses to MATCH an empty key, so a stored one would be
// invisible on the read path — which is exactly why the write guard needs its
// own assertion. The observation is the CALL, not a side effect: the guard is
// in the adapter, and a counting ledger underneath it sees whether the write
// was attempted at all.
//
// Found by mutation: removing the write guard changed no read.
func TestAnUnkeyedTurnLeavesNoRowBehind(t *testing.T) {
	t.Parallel()
	counter := &countingLedger{Fleet: coordmemory.NewFleet()}
	s := ledgerstore.NewFleetCompletions(counter)
	ctx := context.Background()

	if err := s.Record(ctx, "ceo", "", "t1", base); err != nil {
		t.Fatalf("an unkeyed turn was an error: %v", err)
	}
	if err := s.Record(ctx, "", "wk-1", "t1", base); err != nil {
		t.Fatalf("a seatless turn was an error: %v", err)
	}
	if counter.writes != 0 {
		t.Fatalf("%d writes reached the ledger, want none", counter.writes)
	}

	// THE CONTROL. Without it this passes for an adapter that writes
	// nothing at all.
	if err := s.Record(ctx, "ceo", "wk-1", "t2", base); err != nil {
		t.Fatalf("Record: %v", err)
	}
	if counter.writes != 1 {
		t.Fatalf("a keyed turn made %d writes, want 1", counter.writes)
	}
}

// countingLedger counts the writes that reach it.
type countingLedger struct {
	*coordmemory.Fleet
	writes int
}

func (c *countingLedger) Record(ctx context.Context, scope, key, detail string, at time.Time) error {
	c.writes++
	return c.Fleet.Record(ctx, scope, key, detail, at)
}

// The ledger FAILS OPEN on a read it could not complete.
//
// Not knowing whether work was done has one safe answer and it is the
// pre-ledger one — do the work. A read that failed closed would make a
// coordination-store blip look like a company that had already answered
// everything, and every seat would go quiet with nothing in the log to say
// why.
//
// The failure is injected at the CONTRACT rather than in a driver: the
// coordination store raises the error (so the decision belongs to whoever is
// about to do the work) and this is where that decision is made.
func TestAFailedCompletionReadAnswersNOTHINGRatherThanAPartialSet(t *testing.T) {
	t.Parallel()
	ledger := &faultyLedger{Fleet: coordmemory.NewFleet()}
	s := ledgerstore.NewFleetCompletions(ledger)
	ctx := context.Background()
	for _, k := range []string{"wk-1", "wk-2", "wk-3"} {
		if err := s.Record(ctx, "ceo", k, "t", base); err != nil {
			t.Fatalf("Record: %v", err)
		}
	}
	// THE CONTROL. Without it every assertion below passes for a ledger
	// that never finds anything.
	if got := s.Worked(ctx, "ceo", []string{"wk-1", "wk-2", "wk-3"}); len(got) != 3 {
		t.Fatalf("healthy read found %v, want all three", got)
	}

	ledger.fail = errors.New("the coordination store is unreachable")
	got := s.Worked(ctx, "ceo", []string{"wk-1", "wk-2", "wk-3"})
	if len(got) != 0 {
		t.Errorf("a failed read answered %v, want nothing known — a partial answer "+
			"marks some triggers worked and leaves the rest unknown, with no way "+
			"for the caller to tell", got)
	}
	if got := s.WorkedAt(ctx, "ceo", []string{"wk-1", "wk-2", "wk-3"}); len(got) != 0 {
		t.Errorf("a failed read of when keys were worked answered %v, want nothing known", got)
	}
	ledger.fail = nil
	if got := s.Worked(ctx, "ceo", []string{"wk-1"}); len(got) != 1 {
		t.Errorf("the ledger did not recover: %v", got)
	}
}

// faultyLedger is a coordination ledger whose reads can be made to fail.
type faultyLedger struct {
	*coordmemory.Fleet
	fail error
}

func (f *faultyLedger) Worked(ctx context.Context, scope string, keys []string) (map[string]bool, error) {
	if f.fail != nil {
		return nil, f.fail
	}
	return f.Fleet.Worked(ctx, scope, keys)
}

func (f *faultyLedger) WorkedAt(ctx context.Context, scope string, keys []string) (map[string]time.Time, error) {
	if f.fail != nil {
		return nil, f.fail
	}
	return f.Fleet.WorkedAt(ctx, scope, keys)
}
