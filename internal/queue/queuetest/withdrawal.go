package queuetest

import (
	"context"
	"testing"

	"github.com/crewlet/crewlet/internal/events"
	"github.com/crewlet/crewlet/internal/queue"
)

// runWithdrawal holds every registration a queue hands back a
// [queue.Unsubscribe] for to the rule that type states for all of them: it
// may be called again, and a later call answers nil. A caller that withdraws
// defensively, a deferred withdrawal beside an explicit one or a teardown
// that runs twice, must hear the same from every backend. JetStream's
// answerer alone failed the second call with the broker's "invalid
// subscription", while its listener, its stream subscription and every
// registration of the twin answered nil (#777).
func (s *suite) runWithdrawal(t *testing.T) {
	ctx := t.Context()
	for _, r := range []struct {
		name     string
		register func(q queue.EventQueue, t *testing.T) (queue.Unsubscribe, error)
	}{
		{"a_stream_subscription", func(q queue.EventQueue, _ *testing.T) (queue.Unsubscribe, error) {
			return q.SubscribeStream(ctx, "crewlet.events.>", func(context.Context, string, *events.Event) {})
		}},
		{"an_answerer", func(q queue.EventQueue, t *testing.T) (queue.Unsubscribe, error) {
			return q.Serve(ctx, ns(t)+".ask", queue.ConcurrentAnswers, echo("unasked"))
		}},
		{"a_listener", func(q queue.EventQueue, t *testing.T) (queue.Unsubscribe, error) {
			return q.Listen(ctx, ns(t)+".room", func(string, []byte) {})
		}},
	} {
		t.Run(r.name+"_may_be_withdrawn_twice", func(t *testing.T) {
			t.Parallel()
			q := s.start(ctx, t)
			stop, err := r.register(q, t)
			if err != nil {
				t.Fatalf("register: %v", err)
			}
			if err := stop(ctx); err != nil {
				t.Fatalf("the first withdrawal: %v", err)
			}
			if err := stop(ctx); err != nil {
				t.Fatalf("a second withdrawal answered %v, want nil: the registration is "+
					"withdrawn, which is all a later call asks", err)
			}
		})
	}
}
