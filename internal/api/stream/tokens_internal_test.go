package stream

import (
	"sync/atomic"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/api/livestate"
	"github.com/crewlet/crewlet/internal/tokens"
)

// A TAB CONNECTING DOES NOT TAKE THE TICK'S PUSH FROM THE TABS ALREADY OPEN.
//
// The tick re-pushes the spend rollup when a record has aged out of the live
// window, and it learns that from the projection's expiry. A snapshot folds
// the window too, for the tab that connects — and when that read dropped the
// aged records itself, the tick that followed found nothing to report, so
// every tab already open kept a figure the window no longer held. Driven
// through the tick's own fold rather than a running ticker, so the order of
// the connect and the tick is the case's and never the scheduler's.
//
// Mutation: drop the aged records inside the projection's read, and the tick
// pushes nothing.
func TestAConnectingTabLeavesTheTicksPushToTheOpenOnes(t *testing.T) {
	t.Parallel()
	start := time.Date(2026, 6, 14, 12, 0, 0, 0, time.UTC)
	var now atomic.Int64
	now.Store(start.UnixNano())
	clock := func() time.Time { return time.Unix(0, now.Load()).UTC() }
	s, err := NewService(livestate.New(livestate.WithClock(clock)), Options{
		Health:    func() any { return nil },
		Handles:   func() map[string]string { return map[string]string{} },
		Roster:    func() []map[string]any { return nil },
		Org:       func() any { return nil },
		Tools:     func() []map[string]any { return nil },
		Schedules: func() any { return nil },
		Placement: func() (map[string]bool, error) { return map[string]bool{}, nil },
		Now:       clock,
	})
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	t.Cleanup(s.Stop)
	open := NewClient()
	s.Hub().Register(open)

	s.Ingest(livestate.Envelope{
		ID: "p1", Type: "agent_phase_completed", Timestamp: start.Format(time.RFC3339Nano),
		Category: "agent", Payload: map[string]any{"role": "Lead", "phase": "plan", "total_tokens": 10},
	})
	s.flushTokens()
	if got := pushedRollups(open); len(got) != 1 || got[0].Totals.TotalTokens != 10 {
		t.Fatalf("pushed %+v, want one rollup of the phase", got)
	}

	now.Store(start.Add(livestate.LiveSpendWindow + time.Minute).UnixNano())
	if snap := s.Snapshot()["tokens"].(tokens.Rollup); snap.Totals.TotalTokens != 0 {
		t.Fatalf("the connecting tab's rollup is %+v, want the emptied window", snap.Totals)
	}
	s.flushTokens()
	got := pushedRollups(open)
	if len(got) != 1 || got[0].Totals.TotalTokens != 0 {
		t.Errorf("the open tab was pushed %+v after its only record aged out, want one "+
			"empty rollup: the connect took the expiry the tick reports", got)
	}
}

// pushedRollups drains c and keeps the spend rollups it was pushed.
func pushedRollups(c *Client) []tokens.Rollup {
	var out []tokens.Rollup
	for {
		select {
		case env := <-c.Out():
			if rollup, ok := env.Data.(tokens.Rollup); ok && env.Kind == KindTokens {
				out = append(out, rollup)
			}
		default:
			return out
		}
	}
}
