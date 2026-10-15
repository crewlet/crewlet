package engine_test

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"

	"github.com/crewlet/crewlet/internal/agent/turn"
	"github.com/crewlet/crewlet/internal/engine"
	"github.com/crewlet/crewlet/internal/events"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/queue"
	"github.com/crewlet/crewlet/internal/tracing"
)

// A TURN CONTINUES ITS TRIGGER'S SAMPLING DECISION, end to end through the
// dispatcher: a wake that crossed the queue as JSON, restored onto the
// context the turn runs under, and the turn's own span opened beneath it the
// way the turn engine opens it.
//
// The bug this closes: the dispatcher restored the trigger's ids and nothing
// else, and the restore hardcoded the sampled flag — so every turn was
// exported, and every event it published claimed sampled, whatever the
// operator's sampler had decided at the webhook. This process runs the
// default sampler, which samples every root, so an unsampled trigger can only
// produce an unsampled turn by the decision having crossed the hop.
//
// BOTH SHAPES OF WAKE: one published from inside a span, and the notification
// service's, published BENEATH the webhook's span with no span id of its own —
// the commonest trigger there is. AND IN BOTH THE TURN IS THAT SPAN'S CHILD:
// the span-less wake was restored with no span at all, so the turn it woke
// was a second root of the webhook's trace, naming no parent in any event it
// published.
//
// Mutations: have triggerTrace copy the three ids by hand, as it did, and the
// unsampled cases go red; have WithRemote ignore a span-less wake's parent and
// the span-less cases do.
func TestATurnContinuesItsTriggersSamplingDecision(t *testing.T) {
	t.Parallel()
	const traceID, spanID = "4bf92f3577b34da6a3ce929d0e0e4736", "00f067aa0ba902b7"
	for _, unsampled := range []bool{false, true} {
		for _, beneath := range []bool{false, true} {
			name := fmt.Sprintf("unsampled=%v,span-less=%v", unsampled, beneath)
			t.Run(name, func(t *testing.T) {
				t.Parallel()
				published := events.TraceContext{
					TraceID: traceID, SpanID: spanID, Unsampled: unsampled,
				}
				if beneath {
					published = published.Beneath()
				}
				wire, err := json.Marshal(events.New(types.ExternalNotification{
					NotificationSource: "slack", SourceEventType: "message",
					Sender: "ana", Subject: "a message", Body: "hello",
				}, published))
				if err != nil {
					t.Fatalf("marshal: %v", err)
				}
				var wake events.Event
				if err := json.Unmarshal(wire, &wake); err != nil {
					t.Fatalf("unmarshal: %v", err)
				}

				var (
					ran      bool
					recorded bool
					trace    events.TraceContext
				)
				d := &engine.Dispatcher{
					Turn: func(ctx context.Context, _ engine.Request) (turn.Result, error) {
						ctx, span := tracing.Start(ctx, "engine", "agent.turn")
						defer span.End()
						ran, recorded, trace = true, span.IsRecording(), tracing.TraceOf(ctx)
						return turn.Result{}, nil
					},
				}
				if got := d.Dispatch(t.Context(), "ceo", []*events.Event{&wake}); got.Outcome != queue.OutcomeAck {
					t.Fatalf("outcome = %v, want an ack", got.Outcome)
				}
				if !ran {
					t.Fatal("the turn never ran — the premise")
				}
				if trace.TraceID != traceID {
					t.Errorf("the turn is in trace %s, want its trigger's %s", trace.TraceID, traceID)
				}
				if trace.ParentSpanID != spanID {
					t.Errorf("the turn's parent is %q, want the span its trigger was "+
						"published from %s", trace.ParentSpanID, spanID)
				}
				if recorded == unsampled {
					t.Errorf("a trigger decided unsampled=%v ran a turn recording=%v",
						unsampled, recorded)
				}
				if trace.Unsampled != unsampled {
					t.Errorf("the turn's events carry unsampled=%v, want the trigger's %v",
						trace.Unsampled, unsampled)
				}
			})
		}
	}
}
