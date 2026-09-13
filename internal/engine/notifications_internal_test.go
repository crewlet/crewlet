package engine

import (
	"context"
	"reflect"
	"testing"

	"github.com/crewlet/crewlet/internal/events"
	"github.com/crewlet/crewlet/internal/mattermost"
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

// THE REBUILD FINGERPRINT COVERS EVERY INPUT THE TRANSPORT IS BUILT FROM.
//
// A field on [mattermost.SeatConfig] that the fingerprint does not write is a
// change [Engine.reconcileMattermost] reads as "nothing to do": the revision
// lands, the socket keeps running on the old value, and the only way back is a
// process restart. Nothing but this ties the two together — the write list is
// positional, so adding a field to the struct compiles and says nothing.
//
// The inverse cost is what made this worth writing down. `Channel` was on that
// struct and in this list while no part of the transport read it, so renaming a
// seat's channel — a provisioning input, joined by the reconcile off the live
// org — dropped and re-opened every seat's websocket and replayed each one's
// backfill window, to rebuild a transport that could not tell the difference.
func TestTheChatFingerprintCoversEverySeatInput(t *testing.T) {
	t.Parallel()
	const url, team, status = "https://chat.example.com", "acme", "always"
	base := mattermost.SeatConfig{Handle: "swe", Token: "tok-swe", Username: "agent-swe"}
	was := chatFingerprint(url, team, status, []mattermost.SeatConfig{base})

	fields := reflect.TypeFor[mattermost.SeatConfig]()
	for i := range fields.NumField() {
		field := fields.Field(i)
		if field.Type.Kind() != reflect.String {
			t.Fatalf("%s is not a string, so this case cannot vary it", field.Name)
		}
		moved := base
		reflect.ValueOf(&moved).Elem().Field(i).SetString("moved")
		if got := chatFingerprint(url, team, status, []mattermost.SeatConfig{moved}); got == was {
			t.Errorf("a change to %s leaves the fingerprint identical, so the "+
				"reconcile reads it as nothing to do", field.Name)
		}
	}

	// LENGTH-PREFIXED, which is the whole reason the writer is not a join:
	// with a separator, a seat named "a" holding token "b:c" and one named
	// "a:b" holding "c" hash the same, and a token rotation that happened to
	// land on that shape would be read as no change at all.
	straddle := chatFingerprint(url, team, status, []mattermost.SeatConfig{
		{Handle: "a", Token: "b:c", Username: "u"},
	})
	shifted := chatFingerprint(url, team, status, []mattermost.SeatConfig{
		{Handle: "a:b", Token: "c", Username: "u"},
	})
	if straddle == shifted {
		t.Error("two different seat configurations render as one fingerprint")
	}
}
