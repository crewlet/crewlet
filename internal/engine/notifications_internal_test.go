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

// THE INGRESS CONSUMER COMES BACK WHEN THE POSTURE DOES. A deferral returns
// the delivery to the broker AND quiesces the attachment; the ingress topic
// has no lease admission to resume it, so without [Engine.resumeInbound] a
// node that shed one webhook would read nothing for the life of the process.
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
// A [mattermost.SeatConfig] field the fingerprint does not write is a change
// [Engine.reconcileMattermost] reads as "nothing to do", leaving the socket
// on the old value until a restart; the write list is positional, so a new
// struct field compiles silently. Conversely, a field the transport does not
// read must not be on the struct, or changing it drops and reopens every
// seat's websocket for nothing.
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

	// LENGTH-PREFIXED, not joined: with a separator, seat "a" holding token
	// "b:c" and seat "a:b" holding "c" hash the same.
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
