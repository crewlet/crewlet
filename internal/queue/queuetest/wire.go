package queuetest

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/events"
	"github.com/crewlet/crewlet/internal/queue"
)

// carried is a registered payload, so a delivery can be asked what Go type it
// arrived as. Everything else in this suite publishes UNREGISTERED types on
// purpose (see newEvent) — this group is the one that needs the other half.
type carried struct {
	Work  string `json:"work"`
	Count int    `json:"count"`
}

func (carried) EventType() string { return "queuetest.carried" }

func init() { events.Register[carried]() }

// runWire covers the property that a broker is a SERIALIZATION BOUNDARY.
//
// It is a group of its own because it is the one thing an in-process twin is
// tempted to skip, and skipping it does not merely fail to catch bugs — it
// certifies them. Every case here failed on this repo's own memory twin, which
// handed consumers the publisher's pointer, and passed on the embedded broker
// with no change: engine code that read its payload correctly against a real
// broker read nothing at all against the twin, and the twin was what every
// unit test ran on.
//
// What this group does NOT assert: that a backend uses JSON, or any particular
// encoding. It asserts only what a consumer can observe — an event that
// survives the trip intact, decoded into this build's types, in a copy nobody
// else holds.
func (s *suite) runWire(t *testing.T) {
	ctx := t.Context()

	t.Run("a_typed_payload_arrives_as_the_pointer_form", func(t *testing.T) {
		t.Parallel()
		q := s.start(ctx, t)
		got := make(chan *carried, 1)
		subscribe(ctx, t, q, "wire.typed", "g", func(_ context.Context, ev *events.Event) queue.Result {
			p, ok := events.DataAs[*carried](ev)
			if !ok {
				// Not Fatalf: this runs on a dispatch goroutine.
				t.Errorf("delivered payload is %T, want *carried", ev.Data)
				got <- nil
				return queue.Ack()
			}
			got <- p
			return queue.Ack()
		})

		publish(ctx, t, q, "wire.typed", events.New(carried{Work: "w", Count: 3}, events.TraceContext{}))

		select {
		case p := <-got:
			if p == nil {
				return
			}
			if p.Work != "w" || p.Count != 3 {
				t.Errorf("payload arrived as %+v, want {Work:w Count:3}", *p)
			}
		case <-t.Context().Done():
			t.Fatal("no delivery")
		}
	})

	t.Run("a_free_form_payload_value_survives_whatever_type_it_lands_as", func(t *testing.T) {
		t.Parallel()
		// Event.Payload is the UNTYPED bag beside the registered body, and
		// it is where a wire boundary shows first. Measured on this repo's
		// twin: int -> float64, []string -> []any, string -> string. So a
		// caller writing Payload["replicas"].(int) reads correctly on the
		// publishing node and panics on every consumer.
		//
		// This asserts the VALUE survives, never the Go type it lands as.
		// The contract leaves the encoding to the backend, so requiring
		// float64 would forbid what it permits. Comparison is the
		// canonical JSON of the map, under which int 3 and float64 3 are
		// the same value and []string{"a"} and []any{"a"} are the same
		// list — which is exactly the equivalence a caller is entitled to
		// rely on, and nothing more.
		//
		// It exists because every other payload in this suite is a string,
		// which survives any codec unchanged. A suite whose fixtures are
		// already in the shape its backend produces is not testing the
		// boundary, it is arranging not to look at it: a backend that
		// silently DROPS a value it cannot encode passed every case here
		// before this one.
		q := s.start(ctx, t)
		seen := make(chan *events.Event, 1)
		subscribe(ctx, t, q, "wire.freeform", "g", func(_ context.Context, ev *events.Event) queue.Result {
			seen <- ev
			return queue.Ack()
		})

		sent := newEvent("freeform")
		sent.Payload = map[string]any{
			"replicas": 3,
			"roles":    []string{"lead", "reviewer"},
			"ratio":    0.5,
			"enabled":  true,
			"nested":   map[string]any{"depth": 2},
			"conv":     "c1",
		}
		want := canonicalJSON(t, sent.Payload)
		publish(ctx, t, q, "wire.freeform", sent)

		select {
		case got := <-seen:
			if have := canonicalJSON(t, got.Payload); have != want {
				t.Errorf("free-form payload arrived as %s, want %s", have, want)
			}
		case <-t.Context().Done():
			t.Fatal("no delivery")
		}
	})

	t.Run("the_envelope_survives_the_trip", func(t *testing.T) {
		t.Parallel()
		q := s.start(ctx, t)
		seen := make(chan *events.Event, 1)
		subscribe(ctx, t, q, "wire.envelope", "g", func(_ context.Context, ev *events.Event) queue.Result {
			seen <- ev
			return queue.Ack()
		})

		sent := newEvent("wire_probe")
		sent.Source = "someone"
		sent.TraceID, sent.SpanID = "trace-1", "span-1"
		sent.DelegationDepth = 2
		sent.DelegationChain = []string{"ceo", "cto"}
		sent.Payload = map[string]any{"conv": "c1"}
		publish(ctx, t, q, "wire.envelope", sent)

		select {
		case got := <-seen:
			// Identity above all: the completion ledger keys on the id,
			// so a backend that reissues one turns an idempotent replay
			// into a duplicate turn.
			if got.ID != sent.ID {
				t.Errorf("id arrived as %s, want %s", got.ID, sent.ID)
			}
			if got.Type != sent.Type || got.Source != sent.Source {
				t.Errorf("type/source arrived as %q/%q, want %q/%q",
					got.Type, got.Source, sent.Type, sent.Source)
			}
			if got.TraceID != sent.TraceID || got.SpanID != sent.SpanID {
				t.Errorf("trace linkage arrived as %q/%q, want %q/%q",
					got.TraceID, got.SpanID, sent.TraceID, sent.SpanID)
			}
			if got.DelegationDepth != sent.DelegationDepth {
				t.Errorf("delegation depth arrived as %d, want %d",
					got.DelegationDepth, sent.DelegationDepth)
			}
			if convKey(got) != "c1" {
				t.Errorf("payload map arrived as %v, want conv=c1", got.Payload)
			}
			if !got.Timestamp.Equal(sent.Timestamp) {
				t.Errorf("timestamp arrived as %s, want %s", got.Timestamp, sent.Timestamp)
			}
		case <-t.Context().Done():
			t.Fatal("no delivery")
		}
	})

	t.Run("each_group_gets_its_own_copy", func(t *testing.T) {
		t.Parallel()
		q := s.start(ctx, t)

		// Two groups on one topic, the first of which mutates what it was
		// handed. A backend that fans out one pointer lets the first
		// handler rewrite what the second is about to read — and the
		// second's failure then depends on dispatch order, which is the
		// worst kind of bug to be handed.
		var mu sync.Mutex
		var first, second *carried
		done := make(chan struct{}, 2)

		subscribe(ctx, t, q, "wire.shared", "g1", func(_ context.Context, ev *events.Event) queue.Result {
			if p, ok := events.DataAs[*carried](ev); ok {
				p.Work = "mutated-by-g1"
				ev.Source = "mutated-by-g1"
				mu.Lock()
				first = p
				mu.Unlock()
			}
			done <- struct{}{}
			return queue.Ack()
		})
		subscribe(ctx, t, q, "wire.shared", "g2", func(_ context.Context, ev *events.Event) queue.Result {
			if p, ok := events.DataAs[*carried](ev); ok {
				mu.Lock()
				second = p
				mu.Unlock()
			}
			if ev.Source != "publisher" {
				t.Errorf("g2 saw source %q — another group's handler rewrote it", ev.Source)
			}
			done <- struct{}{}
			return queue.Ack()
		})

		ev := events.New(carried{Work: "original"}, events.TraceContext{})
		ev.Source = "publisher"
		publish(ctx, t, q, "wire.shared", ev)

		for range 2 {
			select {
			case <-done:
			case <-t.Context().Done():
				t.Fatal("both groups did not deliver")
			}
		}

		mu.Lock()
		defer mu.Unlock()
		if first == nil || second == nil {
			t.Fatalf("a group decoded no payload (g1=%v g2=%v)", first, second)
		}
		if first == second {
			t.Error("both groups were handed the SAME payload pointer")
		}
		if second.Work != "original" {
			t.Errorf("g2's payload reads %q — g1's mutation reached it", second.Work)
		}
	})

	t.Run("publish_stamps_the_publishing_node", func(t *testing.T) {
		t.Parallel()
		// THE ORIGIN IS THE QUEUE'S TO NAME, on every copy of the event
		// alike. The event store is written by a publish listener on the
		// publishing node, a seat's mailbox by the broker, a dashboard by
		// the broadcast — and a reader holding any one of them has only the
		// envelope's `node` to find the node that holds the rest. So all
		// three must carry it, and must carry the SAME value: a listener
		// handed the publisher's unstamped struct while the wire copy was
		// stamped would write every row with no origin at all.
		const node = "node-a"
		q := s.start(ctx, t, queue.WithNode(node))
		const topic = "crewlet.events.agent_phase_started"
		listened := make(chan string, 1)
		q.AddPublishListener(func(_ context.Context, _ string, ev *events.Event) {
			if ev != nil && labelOf(ev) == "stamped" {
				listened <- ev.Node
			}
		})
		delivered := make(chan string, 1)
		subscribe(ctx, t, q, topic, "grp", func(_ context.Context, ev *events.Event) queue.Result {
			delivered <- ev.Node
			return queue.Ack()
		})
		broadcast := make(chan string, 1)
		if _, err := q.SubscribeStream(ctx, "crewlet.events.>",
			func(_ context.Context, _ string, ev *events.Event) {
				if labelOf(ev) == "stamped" {
					broadcast <- ev.Node
				}
			}); err != nil {
			t.Fatalf("SubscribeStream: %v", err)
		}

		sent := newEvent("stamped")
		publish(ctx, t, q, topic, sent)
		for name, seen := range map[string]chan string{
			"publish listener": listened, "durable consumer": delivered, "broadcast": broadcast,
		} {
			select {
			case got := <-seen:
				if got != node {
					t.Errorf("the %s saw the event from node %q, want %q", name, got, node)
				}
			case <-time.After(settleFor):
				t.Errorf("the %s never saw the event", name)
			}
		}
		// Publish READS its argument. One event published to two topics
		// from two goroutines is legitimate, so a stamp written into the
		// caller's struct is a data race in every such caller.
		if sent.Node != "" {
			t.Errorf("Publish wrote node %q into the caller's own event", sent.Node)
		}

		// AND A CLIENT THAT NAMES NO NODE INVENTS NONE: an origin nobody
		// declared is worse than an absent one, because a reader follows it.
		anonymous := s.start(ctx, t)
		unnamed := make(chan string, 1)
		subscribe(ctx, t, anonymous, topic, "grp", func(_ context.Context, ev *events.Event) queue.Result {
			unnamed <- ev.Node
			return queue.Ack()
		})
		publish(ctx, t, anonymous, topic, newEvent("unstamped"))
		select {
		case got := <-unnamed:
			if got != "" {
				t.Errorf("a client built with no node stamped %q", got)
			}
		case <-time.After(settleFor):
			t.Fatal("no delivery from the client built with no node")
		}
	})

	t.Run("a_republish_keeps_its_origin_node", func(t *testing.T) {
		t.Parallel()
		// ONLY AN EMPTY ORIGIN IS STAMPED. A node hands another node's
		// event back to the broker as a matter of course — a delivery it
		// parked, a dead letter — and that event's row, the work it
		// describes and the rest of its turn are on the node that FIRST
		// published it. Restamped on the way through, it would send every
		// reader to a node whose store holds none of that.
		//
		// Two separate queues rather than two clients of one broker, which
		// is all the case needs and so needs no capability: what b is
		// handed is exactly what a consumer of a received — the decoded
		// copy, origin and all.
		a := s.start(ctx, t, queue.WithNode("node-a"))
		b := s.start(ctx, t, queue.WithNode("node-b"))
		const topic = "crewlet.agent.relay.inbox"

		received := make(chan *events.Event, 1)
		subscribe(ctx, t, a, topic, "agent-relay", func(_ context.Context, ev *events.Event) queue.Result {
			received <- ev
			return queue.Ack()
		})
		publish(ctx, t, a, topic, newEvent("relayed"))
		var handed *events.Event
		select {
		case handed = <-received:
		case <-time.After(settleFor):
			t.Fatal("no delivery on the origin node")
		}
		if handed.Node != "node-a" {
			t.Fatalf("the origin node's own delivery names node %q, want node-a", handed.Node)
		}

		listened := make(chan string, 1)
		b.AddPublishListener(func(_ context.Context, _ string, ev *events.Event) {
			if ev != nil && labelOf(ev) == "relayed" {
				listened <- ev.Node
			}
		})
		redelivered := make(chan string, 1)
		subscribe(ctx, t, b, topic, "agent-relay", func(_ context.Context, ev *events.Event) queue.Result {
			redelivered <- ev.Node
			return queue.Ack()
		})
		publish(ctx, t, b, topic, handed)
		for name, seen := range map[string]chan string{
			"relaying node's listener": listened, "relaying node's consumer": redelivered,
		} {
			select {
			case got := <-seen:
				if got != "node-a" {
					t.Errorf("after node-b handed it on, the %s saw origin %q, want node-a",
						name, got)
				}
			case <-time.After(settleFor):
				t.Errorf("the %s never saw the relayed event", name)
			}
		}
	})

	t.Run("the_publishers_event_is_not_the_delivered_one", func(t *testing.T) {
		t.Parallel()
		q := s.start(ctx, t)
		seen := make(chan *events.Event, 1)
		subscribe(ctx, t, q, "wire.isolation", "g", func(_ context.Context, ev *events.Event) queue.Result {
			seen <- ev
			return queue.Ack()
		})

		sent := events.New(carried{Work: "sent"}, events.TraceContext{})
		publish(ctx, t, q, "wire.isolation", sent)

		select {
		case got := <-seen:
			if got == sent {
				t.Fatal("the handler was handed the publisher's own event")
			}
			// A publisher that keeps writing to its struct after
			// publishing — a loop reusing one variable is the usual
			// shape — must not reach back into what was delivered.
			if p, ok := events.DataAs[*carried](sent); ok {
				p.Work = "changed-after-publish"
			}
			if p, ok := events.DataAs[*carried](got); !ok {
				t.Errorf("delivered payload is %T", got.Data)
			} else if p.Work != "sent" {
				t.Errorf("delivered payload reads %q — it shares the publisher's struct", p.Work)
			}
		case <-t.Context().Done():
			t.Fatal("no delivery")
		}
	})
}
