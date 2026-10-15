package tracingtest_test

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/propagation"

	"github.com/crewlet/crewlet/internal/events"
	"github.com/crewlet/crewlet/internal/tracing"
	"github.com/crewlet/crewlet/internal/tracing/tracingtest"
)

// wakeProbe is a payload no build registers: a hop needs only the envelope,
// and an unknown type carries its envelope like any other.
type wakeProbe struct{}

func (*wakeProbe) EventType() string { return "tracingtest_wake_probe" }

// hop is the queue between two nodes: the wake's JSON out and back, so what
// the turn restores is what the envelope carried and nothing a context held.
func hop(t *testing.T, tc events.TraceContext) events.TraceContext {
	t.Helper()
	wire, err := json.Marshal(events.New(wakeProbe{}, tc))
	if err != nil {
		t.Fatalf("marshal the wake: %v", err)
	}
	var arrived events.Event
	if err := json.Unmarshal(wire, &arrived); err != nil {
		t.Fatalf("unmarshal the wake: %v", err)
	}
	return arrived.Trace()
}

// A TURN A DELIVERY WAKES IS THE DELIVERY'S CHILD — the edge a trace exists
// to draw, from "a message arrived" to "here is what the seat did about it".
//
// The chain is the production one, seam for seam: `webhook.receive` opens
// beneath whatever traceparent the request carried (a gateway's span here,
// which is also how the delivery's sampling decision is set), the delivery is
// published from inside it, the notification service publishes the wake
// BENEATH it — a relay that opens no span, so the wake names the webhook span
// as its parent and no span of its own — the wake crosses the queue, and the
// dispatcher restores it and opens `agent.turn`.
//
// `agent.turn` was a SECOND ROOT of the webhook's trace: the restore kept the
// trace id and a zero span id, so the SDK gave the turn no parent at all, at
// a backend and in every event the turn published. Both halves of the
// decision are run, because the parent has to be the webhook span whether or
// not anybody exports it — the events the turn publishes name it either way,
// and that is what the dashboard's tree is drawn from.
//
// Mutation: have WithRemote ignore a span-less trace's ParentSpanID (restore
// a zero span id, as it did) and every case goes red.
func TestATurnADeliveryWakesIsTheDeliverysChild(t *testing.T) {
	t.Parallel()
	for _, sampled := range []bool{true, false} {
		t.Run(fmt.Sprintf("sampled=%v", sampled), func(t *testing.T) {
			t.Parallel()
			gateway := events.NewTrace()
			flags := "-00"
			if sampled {
				flags = "-01"
			}
			const gatewaySpan = "b7ad6b7169203331"
			tracingtest.Watch(t, gateway.TraceID)
			inbound := otel.GetTextMapPropagator().Extract(context.Background(),
				propagation.MapCarrier{"traceparent": "00-" + gateway.TraceID + "-" + gatewaySpan + flags})

			// THE WEBHOOK EDGE: a span, and the delivery published inside it.
			edge, receive := tracing.Start(inbound, "test", "webhook.receive")
			delivery := tracing.TraceOf(edge)
			receive.End()
			webhookSpan := receive.SpanContext().SpanID().String()
			if delivery.SpanID != webhookSpan || delivery.Unsampled == sampled {
				t.Fatalf("the delivery published in webhook.receive carries %+v — the premise", delivery)
			}

			// THE NOTIFICATION SERVICE: the wake, beneath the delivery's span.
			wake := hop(t, delivery.Beneath())

			// THE DISPATCHER: restore, then the turn.
			ctx, turn := tracing.Start(tracing.WithRemote(context.Background(), wake),
				"test", "agent.turn")
			published, recording := tracing.TraceOf(ctx), turn.IsRecording()
			turn.End()

			if got := turn.SpanContext().TraceID().String(); got != gateway.TraceID {
				t.Errorf("the turn opened in trace %s, want the delivery's %s", got, gateway.TraceID)
			}
			if published.ParentSpanID != webhookSpan {
				t.Errorf("the turn's events name parent %q, want webhook.receive's %s",
					published.ParentSpanID, webhookSpan)
			}
			if published.Unsampled == sampled || recording != sampled {
				t.Errorf("the turn continued unsampled=%v recording=%v, want the "+
					"delivery's sampled=%v", published.Unsampled, recording, sampled)
			}
			if !sampled {
				// Nothing of an unsampled trace is recorded, so there is no
				// exported span to read: the parent its events name, above,
				// is the whole of what it says.
				return
			}
			got := tracingtest.Only(t, gateway.TraceID, "agent.turn")
			if parent := got.Parent(); parent.SpanID().String() != webhookSpan || !parent.IsRemote() {
				t.Errorf("agent.turn's exported parent is %s (remote=%v), want "+
					"webhook.receive's %s — a second root of the delivery's trace",
					parent.SpanID(), parent.IsRemote(), webhookSpan)
			}
		})
	}
}

// A WAKE WHOSE TRACE NOBODY SPANNED STARTS ITS TURN AS THE TRACE'S ROOT, in
// that trace and as it was decided — and names no parent, because there is
// none: the trace was minted where no span was open (a committed record's
// change-feed wake, an event published outside any span), so no span ever had
// an id for the turn to hang under.
//
// A minted root used to carry a random span id, which no span had and no
// backend ever received. Once the restore honours a wake's parent, that id
// became the turn's parent — a "missing parent" at every backend.
//
// The unsampled case is the minted shape a ratio of 0 produces, built by hand
// because this binary runs the default root rule; it is what `continued` keeps.
//
// Mutations: give a minted root a span id again (both cases name it as the
// turn's parent); let `continued` hand every root to the rule (the unsampled
// case records).
func TestAMintedRootsWakeStartsItsTurnWithNoParent(t *testing.T) {
	t.Parallel()
	minted := tracing.TraceOf(context.Background())
	if minted.SpanID != "" || minted.ParentSpanID != "" {
		t.Fatalf("a root minted with no span open claims span %q, parent %q — "+
			"ids no span ever had", minted.SpanID, minted.ParentSpanID)
	}
	unsampled := events.NewTrace()
	unsampled.Unsampled = true
	for name, root := range map[string]events.TraceContext{"sampled": minted, "unsampled": unsampled} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			tracingtest.Watch(t, root.TraceID)
			wake := hop(t, root.Beneath())
			ctx, turn := tracing.Start(tracing.WithRemote(context.Background(), wake),
				"test", "agent.turn")
			published, recording := tracing.TraceOf(ctx), turn.IsRecording()
			turn.End()

			if published.TraceID != root.TraceID {
				t.Errorf("the turn opened in trace %s, want the wake's %s", published.TraceID, root.TraceID)
			}
			if published.ParentSpanID != "" {
				t.Errorf("the turn names parent %q, a span that never existed", published.ParentSpanID)
			}
			if published.Unsampled != root.Unsampled || recording == root.Unsampled {
				t.Errorf("the turn continued unsampled=%v recording=%v, want the root's unsampled=%v",
					published.Unsampled, recording, root.Unsampled)
			}
			if root.Unsampled {
				return
			}
			if parent := tracingtest.Only(t, root.TraceID, "agent.turn").Parent(); parent.SpanID().IsValid() {
				t.Errorf("agent.turn was exported under parent %s, a span that never existed",
					parent.SpanID())
			}
		})
	}
}
