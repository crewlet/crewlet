package livestate_test

import (
	"sync"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/api/livestate"
)

// waitingReport is one node's inbox report: its incarnation, its sequence and
// the seats it holds, each with its two counts.
func waitingReport(reporter string, seq int, seats ...map[string]any) map[string]any {
	rows := make([]any, 0, len(seats))
	for _, seat := range seats {
		rows = append(rows, seat)
	}
	return map[string]any{"reporter_id": reporter, "seq": seq, "seats": rows}
}

func seatWaiting(agentID string, priority, background int) map[string]any {
	return map[string]any{"agent_id": agentID, "priority": priority, "background": background}
}

// testClock is a clock a case moves by hand.
type testClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *testClock) read() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *testClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

func waitingOf(t *testing.T, s *livestate.LiveState, agentID string) *livestate.Waiting {
	t.Helper()
	return overlayOf(t, s, agentID).Waiting
}

// A REPORT LANDS ON EVERY SEAT IT LISTS, and a later report from the same
// node replaces it; an older one arriving late does not walk it back.
func TestAnInboxReportLandsOnTheSeatsItLists(t *testing.T) {
	t.Parallel()
	s := livestate.New()
	s.Apply(env("seat_inbox_waiting", waitingReport("node-a:1", 2,
		seatWaiting("Lead", 1, 4), seatWaiting("Dev", 0, 0)), streamOnly))
	if got := waitingOf(t, s, "Lead"); got == nil || *got != (livestate.Waiting{Priority: 1, Background: 4}) {
		t.Fatalf("Lead waiting = %+v, want 1 priority and 4 background", got)
	}
	s.Apply(env("seat_inbox_waiting", waitingReport("node-a:1", 1, seatWaiting("Lead", 9, 9)), streamOnly))
	if got := waitingOf(t, s, "Lead"); *got != (livestate.Waiting{Priority: 1, Background: 4}) {
		t.Fatalf("a late, older report walked Lead back to %+v", got)
	}
}

// A SEAT ITS REPORTER STOPS LISTING IS CLEARED, but only by the reporter its
// count came from: a seat that moved to another node keeps the count its new
// holder reported, whatever the old holder stops saying.
func TestASeatIsClearedOnlyByTheReporterItsCountCameFrom(t *testing.T) {
	t.Parallel()
	s := livestate.New()
	s.Apply(env("seat_inbox_waiting", waitingReport("node-a:1", 1,
		seatWaiting("Lead", 0, 3), seatWaiting("Dev", 0, 2)), streamOnly))
	s.Apply(env("seat_inbox_waiting", waitingReport("node-b:1", 1, seatWaiting("Dev", 1, 2)), streamOnly))
	s.Apply(env("seat_inbox_waiting", waitingReport("node-a:1", 2), streamOnly))

	if got := waitingOf(t, s, "Lead"); got != nil {
		t.Errorf("Lead keeps %+v after the node holding it stopped listing it", got)
	}
	if got := waitingOf(t, s, "Dev"); got == nil || *got != (livestate.Waiting{Priority: 1, Background: 2}) {
		t.Errorf("Dev = %+v, want its new holder's count: the old holder cleared a seat it no "+
			"longer reports", got)
	}
}

// A NODE THAT WENT AWAY STOPS COUNTING. It publishes nothing that could clear
// its seats, so its counts are forgotten three heartbeats after it was last
// heard, and not before: one late report never blanks a seat.
func TestTheCountsOfANodeThatWentAwayExpire(t *testing.T) {
	t.Parallel()
	clock := &testClock{now: time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)}
	s := livestate.New(livestate.WithClock(clock.read))
	s.Apply(env("seat_inbox_waiting", waitingReport("node-a:1", 1, seatWaiting("Lead", 0, 3)), streamOnly))

	clock.advance(60 * time.Second)
	if change := s.ExpireWaiting(); len(change.Agents) != 0 || waitingOf(t, s, "Lead") == nil {
		t.Fatalf("the count expired after two heartbeats (moved %v); it is held for three",
			change.Agents)
	}
	clock.advance(31 * time.Second)
	change := s.ExpireWaiting()
	if _, moved := change.Agents["Lead"]; !moved {
		t.Fatalf("expiry moved %v, want Lead pushed with its count gone", change.Agents)
	}
	if got := waitingOf(t, s, "Lead"); got != nil {
		t.Fatalf("Lead still shows %+v from a node nobody has heard from for 91 s", got)
	}
}
