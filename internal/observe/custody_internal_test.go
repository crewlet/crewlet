package observe

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/events"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/store"
)

// shipper is a data node's event log as custody reaches it: it can refuse,
// and it counts what it took, duplicates included.
type shipper struct {
	mu      sync.Mutex
	taken   []store.EventRecord
	refuse  bool
	batches int
}

func (s *shipper) AppendEvents(_ context.Context, records []store.EventRecord) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.batches++
	if s.refuse {
		return 0, errors.New("no data node answered")
	}
	s.taken = append(s.taken, records...)
	return len(records), nil
}

func (s *shipper) ids() map[string]int {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := map[string]int{}
	for _, r := range s.taken {
		out[r.ID]++
	}
	return out
}

func (s *shipper) set(refuse bool) {
	s.mu.Lock()
	s.refuse = refuse
	s.mu.Unlock()
}

func fastCustody(t *testing.T, s *shipper) *Custody {
	t.Helper()
	c := NewCustody(s)
	c.flushEvery, c.retryBase = 10*time.Millisecond, 5*time.Millisecond
	c.Start(t.Context())
	t.Cleanup(func() { c.Stop(context.Background()) })
	return c
}

func persisted(t *testing.T) *events.Event {
	t.Helper()
	ev := events.New(types.AgentPhaseStarted{RoleName: "CEO"}, events.TraceContext{})
	if _, ok := Record(ev); !ok {
		t.Fatal("the fixture is not a persisted type")
	}
	return ev
}

func waitFor(t *testing.T, what string, ok func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !ok() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// WHAT A NODE WITHOUT A STORE PUBLISHES REACHES A NODE WITH ONE — every record
// exactly once on the ordinary path, which is what makes the audit log of a
// disposable node's turns something a person can still read.
func TestCustodyShipsEveryPersistedRecord(t *testing.T) {
	t.Parallel()
	s := &shipper{}
	c := fastCustody(t, s)
	listen := c.Listen()
	var want []string
	for range 10 {
		ev := persisted(t)
		want = append(want, ev.ID.String())
		listen(t.Context(), "topic", ev)
	}
	waitFor(t, "every record to ship", func() bool { return len(s.ids()) == len(want) })
	for _, id := range want {
		if got := s.ids()[id]; got != 1 {
			t.Errorf("record %s shipped %d times", id, got)
		}
	}
}

// A DATA NODE THAT IS AWAY IS WAITED FOR, not given up on: the records stay
// buffered and ship when it is back, which is a restart riding out as a delay
// rather than as a hole.
func TestCustodyRidesOutAnUnreachableDataNode(t *testing.T) {
	t.Parallel()
	s := &shipper{refuse: true}
	c := fastCustody(t, s)
	ev := persisted(t)
	c.Listen()(t.Context(), "topic", ev)
	waitFor(t, "a failed shipment", func() bool {
		s.mu.Lock()
		defer s.mu.Unlock()
		return s.batches >= 2
	})
	s.set(false)
	waitFor(t, "the record after the node came back", func() bool { return s.ids()[ev.ID.String()] == 1 })
}

// A FULL BUFFER DROPS RATHER THAN HOLDS THE PUBLISH, and the listener returns
// at once however long the data node is away.
func TestAFullCustodyBufferDropsAndNeverBlocks(t *testing.T) {
	t.Parallel()
	s := &shipper{refuse: true}
	c := NewCustody(s)
	c.buffered = 3
	listen := c.Listen()
	start := time.Now()
	for range 10 {
		listen(t.Context(), "topic", persisted(t))
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("ten publishes into a stalled custody took %s", elapsed)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.pending) != 3 || c.dropped != 7 {
		t.Fatalf("pending %d dropped %d, want 3 and 7", len(c.pending), c.dropped)
	}
}

// A TYPE THE EVENT LOG DOES NOT PERSIST IS NOT SHIPPED EITHER — custody moves
// exactly what the local writer would have written.
func TestCustodyShipsOnlyWhatTheWriterWouldPersist(t *testing.T) {
	t.Parallel()
	s := &shipper{}
	c := NewCustody(s)
	c.Listen()(t.Context(), "topic", events.New(types.ToolSkillPageChanged{PageID: "1"}, events.TraceContext{}))
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.pending) != 0 {
		t.Fatalf("an excluded type was buffered: %+v", c.pending)
	}
}

// A STOP FLUSHES what is buffered, so an orderly restart of a disposable node
// loses nothing it had already published.
func TestAStoppedCustodyFlushesWhatItHolds(t *testing.T) {
	t.Parallel()
	s := &shipper{}
	c := NewCustody(s)
	c.flushEvery = time.Hour
	c.Start(t.Context())
	ev := persisted(t)
	c.Listen()(t.Context(), "topic", ev)
	c.Stop(context.Background())
	if s.ids()[ev.ID.String()] != 1 {
		t.Fatal("a record buffered at stop was not shipped")
	}
}
