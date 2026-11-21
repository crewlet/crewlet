package engine

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/nats-io/nats-server/v2/server"

	"github.com/crewlet/crewlet/internal/events"
	"github.com/crewlet/crewlet/internal/queue"
	"github.com/crewlet/crewlet/internal/queue/jetstream/linktest"
)

// handBackRate is the case's fair shared link: 128 KiB a second, so the
// upload ([outlastingBytes]) holds the data link for some twelve seconds.
const handBackRate = 128 << 10

// awaited waits for ch within a liveness bound.
func awaited[T any](t *testing.T, ch <-chan T, what string) T {
	t.Helper()
	bound := time.NewTimer(2 * time.Minute) //fixedwait:liveness
	defer bound.Stop()
	select {
	case v := <-ch:
		return v
	case <-bound.C:
		t.Fatalf("waited two minutes for %s", what)
	}
	var none T
	return none
}

// A HANDED-BACK DELIVERY REACHES THE PEER ONCE, AFTER ITS NAK (T6): a leaf
// hands back a seat's delivery while its upload holds the data link, so the
// Nak crosses behind the upload's bytes, and detaches; the member's node then
// attaches the same group and is delivered the message once the Nak has
// arrived, decided by the link's count of the upload when it is, and only
// once. A delivery that waited out AckWait (thirty minutes) would never come
// inside the case's bound (design §8.1: a Nak arrives inside its ack window).
//
// Mutant: the hand-back acks in place of its Nak — the peer is never
// delivered the message.
func TestAHandedBackDeliveryReachesThePeerOnceAfterItsNak(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	f := openLeafAcross(t, 0, func(t testing.TB, target string) *linktest.Link {
		return linktest.SharedEachWay(t, target, handBackRate)
	})
	const topic, group = "handback.peer", "seat"
	if _, err := f.member.Queue.EnsureSubscription(ctx, topic, group); err != nil {
		t.Fatalf("subscribe the seat's group: %v", err)
	}
	handedBack := &events.Event{ID: uuid.New(), Type: "handed-back", Timestamp: time.Now().UTC(),
		Source: "engine-test"}

	taken := make(chan struct{}, 1)
	giveBack := make(chan struct{})
	release := sync.OnceFunc(func() { close(giveBack) })
	t.Cleanup(release)
	if err := f.leaf.Queue.Subscribe(ctx, topic, group, func(context.Context, *events.Event) queue.Result {
		select {
		case taken <- struct{}{}:
		default:
		}
		<-giveBack
		return queue.Defer(queue.DeferSeatMoved, "the seat moved to its peer")
	}); err != nil {
		t.Fatalf("attach the leaf: %v", err)
	}
	if err := f.member.Queue.Publish(ctx, topic, handedBack); err != nil {
		t.Fatalf("publish: %v", err)
	}
	awaited(t, taken, "the leaf to be delivered the seat's message")

	carried := func() int64 {
		return f.link.Count(server.DEFAULT_GLOBAL_ACCOUNT, linktest.ToBroker).Carried
	}
	start := carried()
	uploaded := f.bulkOnTheDataLink(t)
	release()
	if _, err := f.leaf.Queue.Detach(ctx, topic, group); err != nil {
		t.Fatalf("detach the leaf: %v", err)
	}

	var deliveries atomic.Int32
	reached := make(chan int64, 1)
	later := make(chan struct{}, 1)
	if err := f.member.Queue.Subscribe(ctx, topic, group, func(_ context.Context, ev *events.Event) queue.Result {
		switch ev.ID {
		case handedBack.ID:
			if deliveries.Add(1) == 1 {
				reached <- carried()
			}
		default:
			select {
			case later <- struct{}{}:
			default:
			}
		}
		return queue.Ack()
	}); err != nil {
		t.Fatalf("attach the peer: %v", err)
	}
	at := awaited(t, reached, "the peer to be delivered the handed-back message")
	if got := at - start; got < outlastingBytes {
		t.Errorf("the peer was delivered the message with %d bytes of the %d-byte upload carried: "+
			"before the Nak behind it could arrive", got, outlastingBytes)
	}
	if err := awaited(t, uploaded, "the upload"); err != nil {
		t.Fatalf("the upload: %v", err)
	}

	// ONCE: a message published after it reaches the peer with the handed-back
	// one delivered to it a single time.
	if err := f.member.Queue.Publish(ctx, topic, &events.Event{ID: uuid.New(), Type: "later",
		Timestamp: time.Now().UTC(), Source: "engine-test"}); err != nil {
		t.Fatalf("publish the later message: %v", err)
	}
	awaited(t, later, "the peer to be delivered the later message")
	if n := deliveries.Load(); n != 1 {
		t.Errorf("the peer was delivered the handed-back message %d times, want once", n)
	}
}
