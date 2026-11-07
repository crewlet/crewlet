package engine_test

import (
	"context"
	"slices"
	"sync/atomic"
	"testing"

	"github.com/crewlet/crewlet/internal/agent/inbox"
	"github.com/crewlet/crewlet/internal/agent/phase"
	"github.com/crewlet/crewlet/internal/agent/turn"
	"github.com/crewlet/crewlet/internal/engine"
	"github.com/crewlet/crewlet/internal/events"
	"github.com/crewlet/crewlet/internal/queue"
)

// A DELIVERY THE DISPATCH FINISHES WITH TAKES ITS QUEUED LINES DOWN, AND ONE
// THAT COMES BACK KEEPS THEM.
//
// A person's queued line stands for their message waiting; it comes down when
// a turn picks the message up or the message is settled without one. A
// delivery handed back (a seat an executor holds, a node that does not own the
// seat) is still waiting, and its person must go on seeing that.
//
// Mutation: settle on every outcome, and a person whose message was handed
// back while an executor held the seat loses their queued line while the
// message still waits.
func TestOnlyAFinishedDeliverySettlesItsQueuedLines(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		conds   inbox.Conditions
		outcome queue.Outcome
		settled bool
	}{
		"a turn ran on it": {
			inbox.Conditions{Owned: true, TurnEngineReady: true, AdmitsTriggers: true},
			queue.OutcomeAck, true},
		"an executor holds the seat": {
			inbox.Conditions{Owned: true, TurnEngineReady: true, AdmitsTriggers: true,
				SeatHeldBySandbox: true},
			queue.OutcomeDefer, false},
		"another node owns the seat": {
			inbox.Conditions{}, queue.OutcomeDefer, false},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			r := &recorder{result: turn.Result{Decision: phase.Done}}
			d := dispatcher(t, r)
			d.Conditions = func(string) inbox.Conditions { return tc.conds }
			var settled []string
			d.Settled = func(_ context.Context, handle string, wakes []string) {
				if handle != "pm" {
					t.Errorf("settled for %q, want the seat dispatched", handle)
				}
				settled = append(settled, wakes...)
			}
			delivered := ev("notification")
			got := d.Dispatch(context.Background(), "pm", []*events.Event{delivered})
			if got.Outcome != tc.outcome {
				t.Fatalf("outcome = %v, want %v", got.Outcome, tc.outcome)
			}
			if was := slices.Contains(settled, delivered.ID.String()); was != tc.settled {
				t.Fatalf("the delivery's queued line settled = %v, want %v", was, tc.settled)
			}
		})
	}
}

// A SEAT IS BUSY WHILE ONE OF ITS DELIVERIES IS BEING DISPATCHED, and only
// then: that is what decides whether a person's message reaching it meanwhile
// is shown as queued.
func TestASeatIsRunningOnlyWhileItsDeliveryIsDispatched(t *testing.T) {
	t.Parallel()
	r := &recorder{result: turn.Result{Decision: phase.Done}}
	d := dispatcher(t, r)
	var during bool
	d.Turn = func(ctx context.Context, req engine.Request) (turn.Result, error) {
		during = d.Running("pm") && !d.Running("qa")
		return r.run(ctx, req)
	}
	if d.Running("pm") {
		t.Fatal("an idle seat reads as running")
	}
	d.Dispatch(context.Background(), "pm", []*events.Event{ev("notification")})
	if !during {
		t.Fatal("the seat did not read as running while its turn ran, or another seat did")
	}
	if d.Running("pm") {
		t.Fatal("the seat still reads as running after its dispatch returned")
	}
}

// A HAND-BACK THAT DEAD-LETTERS A WAKE ENDS ITS QUEUED LINE. The backend
// retires a message whose deliveries are spent rather than return it, so no
// turn would ever pick it up; one that still has deliveries left comes back,
// and its person is still waiting.
func TestAWakeAHandBackDeadLettersSettlesItsQueuedLine(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		left    int
		settled bool
	}{
		"its deliveries are spent":        {0, true},
		"it has deliveries left to retry": {3, false},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			d := dispatcher(t, &recorder{result: turn.Result{Decision: phase.Done}})
			d.Conditions = func(string) inbox.Conditions { return inbox.Conditions{} }
			var settled []string
			d.Settled = func(_ context.Context, _ string, wakes []string) {
				settled = append(settled, wakes...)
			}
			delivered := ev("notification")
			ctx := queue.WithHeadroom(context.Background(),
				[]queue.Headroom{{ID: delivered.ID, Left: tc.left}})
			if got := d.Dispatch(ctx, "pm", []*events.Event{delivered}); got.Outcome == queue.OutcomeAck {
				t.Fatalf("outcome = %v, want the delivery handed back", got.Outcome)
			}
			if was := slices.Contains(settled, delivered.ID.String()); was != tc.settled {
				t.Fatalf("the queued line settled = %v, want %v", was, tc.settled)
			}
		})
	}
}

// THE DISPATCHER TELLS THE REPORTER AT THE BOUNDARY, on the outcome it decided:
// a delivery handed back with nothing left, and neither one with deliveries in
// hand nor one a turn finished.
func TestTheDispatcherTellsOfADeadLetter(t *testing.T) {
	t.Parallel()
	for name, c := range map[string]struct {
		left     int
		finishes bool
		want     int32
	}{
		"its deliveries are spent": {left: 0, want: 1},
		"it has deliveries left":   {left: 3},
		"a turn finished it":       {left: 0, finishes: true},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			d := dispatcher(t, &recorder{result: turn.Result{Decision: phase.Done}})
			if !c.finishes {
				d.Conditions = func(string) inbox.Conditions { return inbox.Conditions{} }
			}
			var told atomic.Int32
			d.DeadLettered = func(context.Context, string, queue.Result) { told.Add(1) }
			delivered := ev("notification")
			ctx := queue.WithHeadroom(context.Background(), []queue.Headroom{{ID: delivered.ID, Left: c.left}})
			d.Dispatch(ctx, "pm", []*events.Event{delivered})
			if got := told.Load(); got != c.want {
				t.Errorf("told %d times, want %d", got, c.want)
			}
		})
	}
}
