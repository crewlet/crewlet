package engine_test

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/agent/inbox"
	"github.com/crewlet/crewlet/internal/agent/phase"
	"github.com/crewlet/crewlet/internal/agent/turn"
	"github.com/crewlet/crewlet/internal/engine"
	"github.com/crewlet/crewlet/internal/events"
	"github.com/crewlet/crewlet/internal/queue"
)

// A FAILURE OF THIS NODE'S IS NEVER THE MAIL'S OWN. A seat's mail that fails
// is NAK'd for a retry wherever it fails: its tail's requeue when the
// partition would not merge, its requeue behind a pause hold, and its turn.
// Where the failure is one the queue tells as this node's (queue.NodeFault: a
// broker that did not answer, a queue no longer live, a delivery whose context
// this node's stop or drain ended), the NAK is the node's (queue.NakNode), so a
// delivery whose budget it spends is kept with the group's unhandled
// deliveries and never with its poison. Any other failure stays the mail's.
//
// Mutation: NAK any one of the three as the message's (queue.Nak), and its
// rows for a broker, a queue and an ended context count the failure against
// the mail.
func TestAFailureOfThisNodesIsNeverTheMailsOwn(t *testing.T) {
	t.Parallel()
	live := t.Context()
	ended, end := context.WithCancel(live)
	end()
	failures := map[string]struct {
		err   error
		ended bool
		nodes bool
	}{
		"a broker that did not answer": {
			err: fmt.Errorf("%w: nats: no responders available for request", queue.ErrUnavailable), nodes: true},
		"a queue no longer live":    {err: fmt.Errorf("%w: draining", queue.ErrNotLive), nodes: true},
		"a context this node ended": {err: context.Canceled, ended: true, nodes: true},
		"the mail's own":            {err: errors.New("the payload is larger than the broker takes")},
	}
	sites := map[string]func(t *testing.T, failed error) (*engine.Dispatcher, []*events.Event){
		// The tail of a partition that would not merge, requeued before
		// the head runs.
		"the requeue of a partition's tail": func(t *testing.T, failed error) (*engine.Dispatcher, []*events.Event) {
			d := dispatcher(t, &recorder{result: turn.Result{Decision: phase.Done}})
			d.Park = func(context.Context, string, []*events.Event) error { return failed }
			opaque := &events.Event{ID: uuid.New(), Type: notificationType, Timestamp: clock}
			notifyStamp(opaque, "slack:C1", "slack:C1")
			return d, []*events.Event{said("ana", "first", clock), opaque}
		},
		// A delivery requeued behind the hold a company with no model
		// takes on the seat.
		"a park behind a pause hold": func(t *testing.T, failed error) (*engine.Dispatcher, []*events.Event) {
			d := dispatcher(t, &recorder{})
			d.Conditions = func(string) inbox.Conditions { return inbox.Conditions{Owned: true} }
			d.Park = func(context.Context, string, []*events.Event) error { return failed }
			return d, []*events.Event{ev("notification")}
		},
		// A turn that failed without acting, which a retry runs cleanly.
		"a turn": func(t *testing.T, failed error) (*engine.Dispatcher, []*events.Event) {
			return dispatcher(t, &recorder{err: failed}), []*events.Event{ev("notification")}
		},
	}
	for site, build := range sites {
		for name, tc := range failures {
			t.Run(site+"/"+name, func(t *testing.T) {
				t.Parallel()
				ctx := live
				if tc.ended {
					ctx = ended
				}
				d, evs := build(t, tc.err)
				got := d.Dispatch(ctx, "ceo", evs)
				if got.Outcome != queue.OutcomeNak {
					t.Fatalf("outcome = %s (%s), want a NAK for a retry", got.Outcome, got.Reason)
				}
				if !errors.Is(got.Err, tc.err) {
					t.Fatalf("the NAK carries %v, want the failure the site met (%v)", got.Err, tc.err)
				}
				if got.FailsMessage() == tc.nodes {
					t.Errorf("FailsMessage() = %v for %v; want %v", got.FailsMessage(), got.Err, !tc.nodes)
				}
			})
		}
	}
}
