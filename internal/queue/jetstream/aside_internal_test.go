package jetstream

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/events"
	"github.com/crewlet/crewlet/internal/queue"
)

// AN ASIDE DELIVERY IS KEPT PAST THE ACK WINDOW, and spends none of it: a busy
// seat's mail waits aside for its run's hours, many windows long, and is handed
// over again only by the Readmit, with the count it was first delivered at.
//
// Whether a window ran out is the broker's to say, so the case asks it through
// controls, as TestAHandlerSignallingProgressKeepsItsDeliveryPastTheAckWindow
// does: a later message held in its handler WITHOUT a signal is redelivered
// once its window runs out, and two of them in turn hold the aside delivery
// past two windows. Any member may take it, so every member is readmitted.
//
// Mutation: start no keeper ([attachment.keepHeld]), or let it signal nothing,
// and the aside delivery is redelivered before the Readmit.
func TestAnAsideDeliveryIsKeptPastTheAckWindow(t *testing.T) {
	t.Parallel()
	const window = 2 * time.Second
	srv := startTestServer(t, testTimings(Config{AckWait: window}))
	ctx := t.Context()
	topic, group := seatInbox("ines"), seatGroup("ines")
	members := []*Queue{clientUnderTest(t, srv, srv.Server), clientUnderTest(t, srv, srv.Server),
		clientUnderTest(t, srv, srv.Server)}
	if _, err := members[0].EnsureSubscription(ctx, topic, group); err != nil {
		t.Fatalf("EnsureSubscription: %v", err)
	}

	type control struct {
		ev                        *events.Event
		taken, redelivered, letGo chan struct{}
	}
	kept := ev(1)
	controls := map[uuid.UUID]*control{}
	var order []*control
	for _, n := range []int{2, 3} {
		c := &control{ev: ev(n), taken: make(chan struct{}),
			redelivered: make(chan struct{}), letGo: make(chan struct{})}
		controls[c.ev.ID] = c
		order = append(order, c)
	}
	keptAside, keptBack := make(chan struct{}), make(chan struct{})
	var (
		mu     sync.Mutex
		handed = map[uuid.UUID]int{}
		left   []int
	)
	handler := func(ctx context.Context, evs []*events.Event) queue.Result {
		e := evs[0]
		mu.Lock()
		handed[e.ID]++
		n := handed[e.ID]
		if e.ID == kept.ID {
			l, _ := queue.DeliveriesLeftFor(ctx, e.ID)
			left = append(left, l)
		}
		mu.Unlock()
		c := controls[e.ID]
		switch {
		case e.ID == kept.ID && n == 1:
			close(keptAside)
			return queue.Aside("k", "the case keeps it aside")
		case e.ID == kept.ID && n == 2:
			close(keptBack)
		case c != nil && n == 1:
			close(c.taken)
			select {
			case <-c.letGo:
			case <-ctx.Done():
			}
		case c != nil && n == 2:
			close(c.redelivered)
		}
		return queue.Ack()
	}
	key := func(*events.Event) string { return "one" }
	for _, m := range members {
		if err := m.SubscribeBatch(ctx, topic, group, handler, key, queue.NewBatchOptions(0, 1)); err != nil {
			t.Fatalf("SubscribeBatch: %v", err)
		}
	}
	await := func(ch <-chan struct{}, what string) {
		t.Helper()
		select {
		case <-ch:
		case <-time.After(time.Minute): //fixedwait:liveness
			t.Fatalf("%s within a minute", what)
		}
	}

	if err := members[0].Publish(ctx, topic, kept); err != nil {
		t.Fatalf("Publish: %v", err)
	}
	await(keptAside, "the kept message was not set aside")
	for i, c := range order {
		if err := members[0].Publish(ctx, topic, c.ev); err != nil {
			t.Fatalf("Publish control %d: %v", i+1, err)
		}
		await(c.taken, "a control was not taken")
		await(c.redelivered, "a control held without a signal was not redelivered")
		close(c.letGo)
	}
	mu.Lock()
	before := handed[kept.ID]
	mu.Unlock()
	if before != 1 {
		t.Fatalf("the aside message was handed over %d times before its Readmit, want once", before)
	}
	readmitted := 0
	for _, m := range members {
		n, err := m.Readmit(ctx, topic, group, "k")
		if err != nil {
			t.Fatalf("Readmit: %v", err)
		}
		readmitted += n
	}
	if readmitted != 1 {
		t.Fatalf("Readmit handed back %d deliveries, want the one aside", readmitted)
	}
	await(keptBack, "the readmitted message was not handed over")
	mu.Lock()
	defer mu.Unlock()
	if len(left) != 2 || left[0] != left[1] {
		t.Fatalf("the aside message had %v deliveries left, want the same both times", left)
	}
}
