package engine_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"syscall"
	"testing"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/agent/inbox"
	"github.com/crewlet/crewlet/internal/agent/phase"
	"github.com/crewlet/crewlet/internal/agent/turn"
	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/engine"
	"github.com/crewlet/crewlet/internal/events"
	"github.com/crewlet/crewlet/internal/queue"
	"github.com/crewlet/crewlet/internal/sandbox"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/store"
)

// A FAILURE OF THIS NODE'S IS NEVER THE MAIL'S OWN. A seat's mail that fails
// is NAK'd for a retry wherever it fails: its tail's requeue when the
// partition would not merge, its requeue behind a pause hold, and its turn.
// Where the failure is one the queue tells as this node's (queue.NodeFault: a
// broker that did not answer, a queue no longer live, a delivery whose context
// this node's stop or drain ended), or a condition of the infrastructure the
// engine runs on (builtin.InfrastructureKind: a coordination store that did
// not answer, a busy store, a full disk, a sandbox provider that did not
// answer), the NAK is the node's
// (queue.NakNode), so a delivery whose budget it spends is kept with the
// group's unhandled deliveries and never with its poison. Any other failure
// stays the mail's, a deadline, a write the state log refused for what it is
// and a sandbox provider refusing the company's credentials among them.
//
// THE SAME ANSWER THE SEAT'S CONTROL TOPIC NAKS BY (nodesFailure), so the
// sandbox coordinator's marks for a store that did not answer it
// (sandbox.ErrStoreSilent, sandbox.ErrUnannounced) are the node's here too.
//
// Mutation: NAK any one of the three as the message's (queue.Nak), and its
// rows for a broker, a queue, an ended context and the infrastructure count
// the failure against the mail; drop the infrastructure's conditions from
// nodesFailure and the rows for a coordination store, a busy store and a
// full disk do; drop the coordinator's marks and theirs do; read every
// condition as the node's and the deadline's and the sandbox's rows count it
// against the node.
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
		"a coordination store that did not answer": {
			err: fmt.Errorf("read the rebase: %w: nats: timeout", coord.ErrUnavailable), nodes: true},
		"a store busy with other writes": {
			err: fmt.Errorf("worklet: %w: database is locked", store.ErrBusy), nodes: true},
		"a full disk": {err: fmt.Errorf("write the notes: %w",
			&os.PathError{Op: "write", Path: "/var/lib/crewlet/notes", Err: syscall.ENOSPC}), nodes: true},
		"a step the coordination store was silent on": {err: fmt.Errorf("runner: launching the "+
			"executor's run: %w: sandbox: read run t1: nats: timeout", sandbox.ErrStoreSilent), nodes: true},
		"a hand-back no waiter announces": {err: fmt.Errorf("runner: resuming t1: %w: its run is not "+
			"settled", sandbox.ErrUnannounced), nodes: true},
		"a state log at its byte ceiling": {err: fmt.Errorf("worklet: decide: %w",
			&statelog.Unavailable{Reason: statelog.ReasonLogFull, OpID: "op-1"}), nodes: true},
		"a write the state log refused for what it is": {err: fmt.Errorf("worklet: decide: %w",
			&statelog.Unavailable{Reason: statelog.ReasonRecordTooLarge, OpID: "op-1"})},
		"a deadline the turn ran past": {
			err: fmt.Errorf("runner: the harness's run: %w", context.DeadlineExceeded)},
		"a sandbox provider that did not answer": {err: fmt.Errorf("runner: launching the executor's "+
			"run: %w", sandbox.Because(sandbox.WhyProviderUnavailable, errors.New("e2b: 503"))), nodes: true},
		"a sandbox provider refusing the company's key": {err: fmt.Errorf("runner: launching the executor's run: %w",
			sandbox.Because(sandbox.WhyProviderRefused, errors.New("e2b: 401")))},
		"the mail's own": {err: errors.New("the payload is larger than the broker takes")},
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
