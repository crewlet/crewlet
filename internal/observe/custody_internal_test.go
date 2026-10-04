package observe

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/events"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/queue"
	"github.com/crewlet/crewlet/internal/queue/topics"
)

// broker is the node's queue as custody reaches it: it can refuse a publish,
// or take one and lose the acknowledgement, and it records every batch it was
// handed, duplicates included.
type broker struct {
	mu       sync.Mutex
	ensured  []string
	batches  []*events.Event
	attempts []uuid.UUID
	refuse   error
	// lostAck takes the next publish and answers it as failed.
	lostAck bool
}

func (b *broker) EnsureSubscription(_ context.Context, topic, group string) (bool, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.ensured = append(b.ensured, topic+"/"+group)
	return true, nil
}

func (b *broker) Publish(_ context.Context, topic string, ev *events.Event) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.attempts = append(b.attempts, ev.ID)
	if b.refuse != nil {
		return b.refuse
	}
	if topic != topics.CustodyRecords || ev.Type != (types.CustodyBatch{}).EventType() {
		return fmt.Errorf("custody published a %s on %s", ev.Type, topic)
	}
	if len(b.ensured) == 0 {
		return errors.New("published before the custody group existed: kept by nobody")
	}
	b.batches = append(b.batches, ev)
	if b.lostAck {
		b.lostAck = false
		return errors.New("the acknowledgement was lost")
	}
	return nil
}

func (b *broker) set(change func(*broker)) {
	b.mu.Lock()
	defer b.mu.Unlock()
	change(b)
}

// carried counts each event id across every batch the broker took.
func (b *broker) carried(t *testing.T) map[string]int {
	t.Helper()
	b.mu.Lock()
	defer b.mu.Unlock()
	out := map[string]int{}
	for _, batch := range b.batches {
		data, ok := events.DataAs[*types.CustodyBatch](batch)
		if !ok {
			t.Fatalf("a batch that does not decode: %+v", batch)
		}
		for _, ev := range data.Events {
			out[ev.ID.String()]++
		}
	}
	return out
}

func fastCustody(t *testing.T, b *broker) *Custody {
	t.Helper()
	c := NewCustody(b)
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

// WHAT A NODE WITHOUT A STORE PUBLISHES IS CARRIED — every event once on the
// ordinary path, onto the custody topic, after the group that keeps the
// stream's messages exists.
func TestCustodyCarriesEveryPersistedEvent(t *testing.T) {
	t.Parallel()
	b := &broker{}
	c := fastCustody(t, b)
	want := map[string]int{}
	for range 600 {
		ev := persisted(t)
		want[ev.ID.String()] = 1
		c.Listen()(t.Context(), "crewlet.events.x", ev)
	}
	waitFor(t, "every event carried", func() bool { return len(b.carried(t)) == len(want) })
	got := b.carried(t)
	for id := range want {
		if got[id] != 1 {
			t.Errorf("event %s carried %d times", id, got[id])
		}
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if !slices.Equal(b.ensured[:1], []string{topics.CustodyRecords + "/" + CustodyGroup}) {
		t.Errorf("the group ensured was %v, want the custody group", b.ensured)
	}
	for _, batch := range b.batches {
		data, _ := events.DataAs[*types.CustodyBatch](batch)
		if len(data.Events) > custodyBatch {
			t.Errorf("a batch of %d events, over the bound of %d", len(data.Events), custodyBatch)
		}
	}
}

// A BATCH THE BROKER WOULD NOT TAKE IS SENT AGAIN UNDER ITS OWN ID, and one
// whose acknowledgement was lost too: the second copy is the same batch, which
// a keeper holding the first writes over and claims to the same answer —
// under a new id it would be a second batch, kept by a second node.
func TestARetriedBatchKeepsItsIdentity(t *testing.T) {
	t.Parallel()
	b := &broker{refuse: errors.New("no member answered")}
	c := fastCustody(t, b)
	ev := persisted(t)
	c.Listen()(t.Context(), "crewlet.events.x", ev)
	waitFor(t, "a refused attempt", func() bool {
		b.mu.Lock()
		defer b.mu.Unlock()
		return len(b.attempts) >= 2
	})
	b.set(func(b *broker) { b.refuse, b.lostAck = nil, true })
	waitFor(t, "the batch taken twice", func() bool {
		b.mu.Lock()
		defer b.mu.Unlock()
		return len(b.batches) == 2
	})
	b.mu.Lock()
	defer b.mu.Unlock()
	for _, id := range b.attempts {
		if id != b.attempts[0] {
			t.Fatalf("the batch was attempted under %v, want one id throughout", b.attempts)
		}
	}
	if b.batches[0].ID != b.batches[1].ID {
		t.Fatal("the batch whose acknowledgement was lost came again under a new id")
	}
}

// A FULL BUFFER DROPS AND NEVER BLOCKS: a turn must not wait on its own audit
// trail, and what was dropped is counted rather than silent.
func TestAFullCustodyBufferDropsAndNeverBlocks(t *testing.T) {
	t.Parallel()
	b := &broker{refuse: errors.New("no member answered")}
	c := NewCustody(b)
	c.buffered = 8
	started := time.Now()
	for range 100 {
		c.Listen()(t.Context(), "crewlet.events.x", persisted(t))
	}
	if took := time.Since(started); took > time.Second {
		t.Fatalf("buffering 100 events took %v with the broker away", took)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.pending) != 8 || c.dropped != 92 {
		t.Fatalf("buffered %d and dropped %d, want 8 and 92", len(c.pending), c.dropped)
	}
}

// ONLY WHAT THE WRITER WOULD PERSIST IS CARRIED — and never a custody batch,
// so this node's own carriers do not feed back into custody.
func TestCustodyCarriesOnlyWhatTheWriterWouldPersist(t *testing.T) {
	t.Parallel()
	c := NewCustody(&broker{})
	for _, ev := range []*events.Event{
		events.New(types.AgentTurnProgress{}, events.TraceContext{}),
		events.New(types.CustodyBatch{}, events.TraceContext{}),
	} {
		c.Listen()(t.Context(), "crewlet.events.x", ev)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.pending) != 0 {
		t.Fatalf("buffered %d event(s) no event log keeps", len(c.pending))
	}
}

// AN ORDERLY STOP PUBLISHES WHAT IS STILL BUFFERED.
func TestAStoppedCustodyFlushesWhatItHolds(t *testing.T) {
	t.Parallel()
	b := &broker{}
	c := NewCustody(b)
	c.flushEvery = time.Hour
	c.Start(t.Context())
	ev := persisted(t)
	c.Listen()(t.Context(), "crewlet.events.x", ev)
	c.Stop(context.Background())
	if got := b.carried(t); got[ev.ID.String()] != 1 {
		t.Fatalf("the buffered event was carried %d times at stop, want once", got[ev.ID.String()])
	}
}

// AN EVENT THE BROKER WILL NEVER CARRY IS LET GO, and the events behind it are
// carried: retried, it would hold the whole audit trail behind one event for
// ever.
func TestAnEventTooLargeToCarryDoesNotHoldTheRest(t *testing.T) {
	t.Parallel()
	b := &broker{refuse: fmt.Errorf("publish: %w", queue.ErrTooLarge)}
	c := fastCustody(t, b)
	c.Listen()(t.Context(), "crewlet.events.x", persisted(t))
	waitFor(t, "the oversized batch attempted", func() bool {
		b.mu.Lock()
		defer b.mu.Unlock()
		return len(b.attempts) >= 1
	})
	b.set(func(b *broker) { b.refuse = nil })
	next := persisted(t)
	c.Listen()(t.Context(), "crewlet.events.x", next)
	waitFor(t, "the next event carried", func() bool { return b.carried(t)[next.ID.String()] == 1 })
}

// A BATCH AT ITS BOUND FITS THE BROKER'S MESSAGE: cut at a mebibyte of events,
// the carrier must stay under queue.MaxPayloadBytes with its envelope, or a
// full batch is one the broker refuses.
func TestABatchAtItsBoundFitsTheBroker(t *testing.T) {
	t.Parallel()
	body := make([]byte, custodyBatchBytes/custodyBatch)
	for i := range body {
		body[i] = 'x'
	}
	evs := make([]*events.Event, custodyBatch)
	for i := range evs {
		evs[i] = events.New(types.AgentPhaseStarted{RoleName: string(body)}, events.TraceContext{})
	}
	carrier := events.New(types.CustodyBatch{Events: evs}, events.TraceContext{})
	raw := encode(carrier)
	if raw == nil || len(raw) >= queue.MaxPayloadBytes {
		t.Fatalf("a full batch encodes to %d bytes, against the broker's %d",
			len(raw), queue.MaxPayloadBytes)
	}
}
