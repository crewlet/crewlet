package stream

import (
	"sync/atomic"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/api/auth"
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
		Org:       func(auth.Reach) any { return nil },
		Tools:     func() []map[string]any { return nil },
		Schedules: func() any { return nil },
		Placement: func() (map[string]bool, error) { return map[string]bool{}, nil },
		Now:       clock,
	})
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	t.Cleanup(s.Stop)
	open := NewClient(auth.ReachAdmin)
	s.Hub().Register(open)

	s.Ingest(livestate.Envelope{
		ID: "p1", Type: "agent_phase_completed", Timestamp: start.Format(time.RFC3339Nano),
		Category: "agent", Payload: map[string]any{"role": "Lead", "agent_handle": "Lead", "phase": "plan", "total_tokens": 10},
	})
	s.flushTokens()
	if got := pushedRollups(open); len(got) != 1 || got[0].Totals.TotalTokens != 10 {
		t.Fatalf("pushed %+v, want one rollup of the phase", got)
	}

	now.Store(start.Add(livestate.LiveSpendWindow + time.Minute).UnixNano())
	if snap := s.Snapshot(auth.ReachAdmin)["tokens"].(tokens.Rollup); snap.Totals.TotalTokens != 0 {
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

// THE SPEND IS FOLDED ONLY FOR AN ADMIN, and waits for one.
//
// The rollup is spend, which no other reach is pushed (ADR-0031): with only a
// member connected the tick folds nothing and pushes nothing, and keeps the
// window marked as moved — so the first tick after an admin connects pushes it
// as it then stands, rather than the admin waiting for the next phase.
//
// Mutation: clear the flag whether or not the fold ran, and the admin is
// pushed nothing.
func TestTheSpendIsFoldedOnlyForAnAdmin(t *testing.T) {
	t.Parallel()
	start := time.Date(2026, 6, 14, 12, 0, 0, 0, time.UTC)
	clock := func() time.Time { return start }
	s, err := NewService(livestate.New(livestate.WithClock(clock)), Options{
		Health:    func() any { return nil },
		Handles:   func() map[string]string { return map[string]string{} },
		Roster:    func() []map[string]any { return nil },
		Org:       func(auth.Reach) any { return nil },
		Tools:     func() []map[string]any { return nil },
		Schedules: func() any { return nil },
		Placement: func() (map[string]bool, error) { return map[string]bool{}, nil },
		Now:       clock,
	})
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	t.Cleanup(s.Stop)
	member := NewClient(auth.ReachMember)
	s.Hub().Register(member)

	s.Ingest(livestate.Envelope{
		ID: "p1", Type: "agent_phase_completed", Timestamp: start.Format(time.RFC3339Nano),
		Category: "agent", Payload: map[string]any{"role": "Lead", "agent_handle": "lead", "phase": "execute", "total_tokens": 10},
	})
	s.flushTokens()
	if got := pushedRollups(member); len(got) != 0 {
		t.Fatalf("a member was pushed the spend rollup %+v", got)
	}
	if !s.tokensDirty.Load() {
		t.Fatal("a fold nobody was sent cleared the mark, so the next admin waits for a phase")
	}

	admin := NewClient(auth.ReachAdmin)
	s.Hub().Register(admin)
	s.flushTokens()
	if got := pushedRollups(admin); len(got) != 1 || got[0].Totals.TotalTokens != 10 {
		t.Errorf("the admin was pushed %+v, want the window as it stands", got)
	}
	if s.tokensDirty.Load() {
		t.Error("the mark outlived the fold it asked for")
	}
}
