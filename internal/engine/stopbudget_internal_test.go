package engine

import (
	"context"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/events"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/queue"
	"github.com/crewlet/crewlet/internal/seat"
)

// A STOP'S LAST EVENTS ARE STEPS OF ITS ALLOWANCE. The node's own
// announcement and each released seat's last lifecycle event are publishes to
// a broker that, on a member without quorum, answers only when the request's
// context ends — and the drain's context has no deadline, since it waits for
// running turns. Each publish is therefore bounded by what is left of the
// stop's one allowance, and a broker that never answers costs the stop that
// allowance once rather than an unbounded wait per event.
func TestAStopsLastEventsAreStepsOfItsAllowance(t *testing.T) {
	t.Parallel()
	e := &Engine{backends: &Backends{Queue: unansweredPublishes{}}}
	e.epoch.current.Store(companyFor(t, "name: Acme\nroles:\n  - name: Lead\n    handle: lead\n"))

	const allowance = 200 * time.Millisecond
	ctx := seat.WithStopBudget(context.Background(), seat.NewStopBudget(allowance))
	finished := make(chan struct{})
	started := time.Now()
	go func() {
		defer close(finished)
		e.publishLifecycle(ctx, events.New(types.OrgStopped{OrgName: "Acme"}, events.NewTrace()))
		e.publishSeatLifecycle(ctx, "lead", types.AgentTerminated{})
	}()
	select {
	case <-finished:
	case <-time.After(10 * time.Second):
		t.Fatal("a stop's publish to a broker that never answers was never " +
			"ended: it is not a step of the stop's allowance")
	}
	if took := time.Since(started); took > allowance+2*time.Second {
		t.Fatalf("the stop's two publishes took %v against an allowance of %v: "+
			"each waited out a bound of its own", took, allowance)
	}
}

// unansweredPublishes is a broker that answers a publish only when the
// request's context ends.
type unansweredPublishes struct{ queue.EventQueue }

func (unansweredPublishes) Publish(ctx context.Context, _ string, _ *events.Event) error {
	<-ctx.Done()
	return ctx.Err()
}
