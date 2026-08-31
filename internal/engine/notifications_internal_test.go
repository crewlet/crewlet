package engine

import (
	"context"
	"testing"

	"github.com/crewlet/crewlet/internal/events"
	"github.com/crewlet/crewlet/internal/notify"
	"github.com/crewlet/crewlet/internal/queue"
	"github.com/crewlet/crewlet/internal/queue/memory"
	"github.com/crewlet/crewlet/internal/queue/topics"
)

// THE INGRESS CONSUMER COMES BACK WHEN THE POSTURE DOES.
//
// A deferral has two halves: the delivery returns to the broker AND the
// attachment quiesces. A seat's mailbox has something to undo the second half
// — its own lease admission, on the next successful renew — and the ingress
// topic has no lease and no seat. Without this convergence a node that shed a
// single webhook would keep accepting deliveries and reading none of them for
// the life of the process, while reporting a perfectly healthy config.
func TestTheIngressConsumerResumesWhenThePostureAdmitsAgain(t *testing.T) {
	t.Parallel()
	q := memory.New()
	if err := q.Start(t.Context()); err != nil {
		t.Fatalf("start: %v", err)
	}
	t.Cleanup(func() { _ = q.Stop(context.Background()) })
	if err := q.Subscribe(t.Context(), topics.NotificationsInbound, notify.InboundGroup,
		func(context.Context, *events.Event) queue.Result { return queue.Ack() }); err != nil {
		t.Fatalf("subscribe: %v", err)
	}

	e := &Engine{backends: &Backends{Queue: q}}

	// Nothing quiesced: the tick that changes nothing must be harmless.
	e.resumeInbound(t.Context())

	quiesced, err := q.Quiesce(t.Context(), topics.NotificationsInbound, notify.InboundGroup)
	if err != nil || !quiesced {
		t.Fatalf("Quiesce = (%v, %v), want (true, nil) — the premise", quiesced, err)
	}

	e.resumeInbound(t.Context())

	// If it resumed, there is nothing left for a second Unquiesce to do.
	resumed, err := q.Unquiesce(t.Context(), topics.NotificationsInbound, notify.InboundGroup)
	if err != nil {
		t.Fatalf("Unquiesce: %v", err)
	}
	if resumed {
		t.Error("the ingress consumer was still quiesced after a posture that admits work")
	}
}

// A NODE WITH NO BROKER DOES NOT PANIC ON THE TICK. The reconcile loop calls
// this on every pass, including on a node whose backends never opened.
func TestResumingTheIngressConsumerWithoutABrokerIsHarmless(t *testing.T) {
	t.Parallel()
	(&Engine{}).resumeInbound(t.Context())
	(&Engine{backends: &Backends{}}).resumeInbound(t.Context())
}
