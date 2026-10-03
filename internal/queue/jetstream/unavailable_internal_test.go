package jetstream

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"

	"github.com/crewlet/crewlet/internal/queue"
)

// WHAT A FAILED CALL SAYS ABOUT THE BROKER, judged on a connection that is up:
// silence is the contract's condition, a broker's own answer is not, and a
// caller's own deadline stays the caller's.
//
// The conformance suite certifies the connection-down half on both backends
// (runReachability); what it cannot reach is a broker that is up and still
// does not answer — a stream with no leader, a request the client timed out —
// so the words a connected client uses for that are pinned here, one by one.
//
// Mutation: drop jsprovision.Unanswered from brokerFailed, and the timeout,
// no-responders and client-deadline rows fail; drop ErrNoStreamResponse, and
// its row does; mark every error, and the broker-answer and caller-deadline
// rows do.
func TestABrokerThatDidNotAnswerIsUnavailableAndOneThatDidIsNot(t *testing.T) {
	t.Parallel()
	q := openForTest(t, Config{})
	live := t.Context()
	over, cancel := context.WithCancel(t.Context())
	cancel()

	for _, c := range []struct {
		name   string
		ctx    context.Context
		err    error
		marked bool
	}{
		{"a request the client timed out", live, nats.ErrTimeout, true},
		{"a request nobody was serving", live, nats.ErrNoResponders, true},
		{"a publish no stream leader acknowledged", live, jetstream.ErrNoStreamResponse, true},
		{"a deadline the client imposed while its caller waited", live,
			context.DeadlineExceeded, true},
		{"a reconnect buffer that filled", live, nats.ErrReconnectBufExceeded, true},
		{"a connection the client found stale", live, nats.ErrStaleConnection, true},
		{"a stream that does not exist", live, jetstream.ErrStreamNotFound, false},
		{"a deadline the caller's own context set", over, context.DeadlineExceeded, false},
		{"a caller that gave up", over, context.Canceled, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			wrapped := fmt.Errorf("publish x: %w", c.err)
			got := q.brokerFailed(c.ctx, wrapped)
			if marked := errors.Is(got, queue.ErrUnavailable); marked != c.marked {
				t.Errorf("brokerFailed(%v) = %v; marked unavailable = %v, want %v",
					c.err, got, marked, c.marked)
			}
			if errors.Is(got, queue.ErrNotLive) {
				t.Errorf("brokerFailed(%v) = %v, which is ErrNotLive on a "+
					"connection that is up", c.err, got)
			}
			if !errors.Is(got, c.err) {
				t.Errorf("brokerFailed(%v) = %v: the client's own error is not "+
					"beneath the mark", c.err, got)
			}
		})
	}
}

// A CONNECTION NATS CLOSED FOR GOOD IS NOT LIVE, and a queue its own Stop
// closed is not live as its Stop says. Both are what a seat release reads as
// proof that nothing is consumed — and both are reached here the way
// production reaches them: the connection closed under a queue that never
// stopped, and then the stop.
//
// Mutation: answer a closed connection ErrUnavailable, and the first half
// fails — a node stopping for a lost connection would KEEP every lease it
// could not hand back; drop the isClosed arm, and the second does.
func TestAConnectionClosedForGoodIsNotLive(t *testing.T) {
	t.Parallel()
	q := openForTest(t, Config{})
	ctx := t.Context()

	// CLOSED BY THE CLIENT, NOT BY Stop: the queue still believes itself
	// open, which is the state a connection the client gave up on leaves.
	q.nc.Close()
	err := q.Publish(ctx, "crewlet.events.lost", ev(1))
	if !errors.Is(err, ErrConnectionLost) || !errors.Is(err, queue.ErrNotLive) {
		t.Errorf("a publish over a connection closed for good answered %v, want "+
			"ErrConnectionLost, which is queue.ErrNotLive", err)
	}
	if errors.Is(err, queue.ErrUnavailable) {
		t.Errorf("a publish over a connection closed for good answered %v, marked "+
			"unavailable: no wait reopens it", err)
	}
	if err := q.reachable(); !errors.Is(err, ErrConnectionLost) {
		t.Errorf("an ask's reachability check over a closed connection answered %v, "+
			"want ErrConnectionLost", err)
	}

	if err := q.Stop(context.WithoutCancel(ctx)); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if got := q.brokerFailed(ctx, nats.ErrConnectionClosed); !errors.Is(got, ErrClosed) {
		t.Errorf("a call that raced this queue's own Stop answered %v, want ErrClosed: "+
			"it says the stop, which is what the caller has to read", got)
	}
}
