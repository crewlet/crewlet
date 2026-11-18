package engine_test

import (
	"context"
	"slices"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/agent/inbox"
	"github.com/crewlet/crewlet/internal/agent/ledger/ledgerstore"
	"github.com/crewlet/crewlet/internal/agent/phase"
	"github.com/crewlet/crewlet/internal/agent/turn"
	"github.com/crewlet/crewlet/internal/engine"
	"github.com/crewlet/crewlet/internal/events"
	"github.com/crewlet/crewlet/internal/queue"
	"github.com/crewlet/crewlet/internal/workkey"
)

// A DELIVERY THE DISPATCH FINISHES WITH TAKES ITS QUEUED LINES DOWN, AND ONE
// THAT COMES BACK KEEPS THEM: a delivery handed back (an executor holds the
// seat, another node owns it) is still waiting.
//
// Mutation: settle on every outcome, and the handed-back case goes red.
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
// then, which decides whether a person's message is shown as queued.
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

// A HAND-BACK THAT DEAD-LETTERS A WAKE ENDS ITS QUEUED LINE, since no turn
// will pick it up; one with deliveries left comes back and keeps it.
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

// A DELIVERY IS GONE WHEN IT WILL NOT COME BACK, AND ONLY THEN: what a seat's
// park recorded for a launch refused below its floor is read by every try and
// dropped once the delivery is gone (acknowledged, run or not, or
// dead-lettered). One handed back with deliveries left, requeued behind a
// pause, or the tail of a partition that would not merge comes back with the
// same ids though the original is acknowledged, and told gone would lose the
// wait it was owed.
//
// Mutation: tell every outcome, tell a requeued delivery or tail, or tell no
// dead-lettered one, and a case goes red.
func TestOnlyADeliveryThatWillNotComeBackIsGone(t *testing.T) {
	t.Parallel()
	ready := inbox.Conditions{Owned: true, TurnEngineReady: true, AdmitsTriggers: true}
	paused := ready
	paused.Paused = true
	for name, tc := range map[string]struct {
		conds inbox.Conditions
		// left is the delivery's deliveries left, none stated where negative.
		left int
		gone bool
	}{
		"a turn ran on it":                 {conds: ready, left: -1, gone: true},
		"requeued behind a person's pause": {conds: paused, left: -1},
		"handed back with deliveries left": {conds: inbox.Conditions{}, left: 3},
		"handed back with none left":       {conds: inbox.Conditions{}, left: 0, gone: true},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			d := dispatcher(t, &recorder{result: turn.Result{Decision: phase.Done}})
			d.Conditions = func(string) inbox.Conditions { return tc.conds }
			var gone []*events.Event
			d.Gone = func(_ context.Context, handle string, evs []*events.Event) {
				if handle != "pm" {
					t.Errorf("gone for %q, want the seat dispatched", handle)
				}
				gone = append(gone, evs...)
			}
			delivered := ev("notification")
			ctx := context.Background()
			if tc.left >= 0 {
				ctx = queue.WithHeadroom(ctx, []queue.Headroom{{ID: delivered.ID, Left: tc.left}})
			}
			d.Dispatch(ctx, "pm", []*events.Event{delivered})
			if was := slices.Contains(gone, delivered); was != tc.gone {
				t.Fatalf("the delivery is gone = %v, want %v", was, tc.gone)
			}
		})
	}
	// ACKNOWLEDGED WITH NO TURN RUN, the ledger having it as worked: it is
	// gone though the dispatch no longer holds it, or its park record would
	// sit in the seat's map until the seat is released.
	t.Run("acknowledged unrun, the ledger had it as worked", func(t *testing.T) {
		t.Parallel()
		r := &recorder{result: turn.Result{Decision: phase.Done}}
		d := dispatcher(t, r)
		completions := ledgerstore.NewMemoryCompletions()
		d.Completions = completions
		var gone []*events.Event
		d.Gone = func(_ context.Context, _ string, evs []*events.Event) { gone = append(gone, evs...) }
		delivered := ev("notification")
		ctx := context.Background()
		if err := completions.Record(ctx, "pm", workkey.Derive([]string{delivered.ID.String()}), "", clock); err != nil {
			t.Fatalf("Record: %v", err)
		}
		if got := d.Dispatch(ctx, "pm", []*events.Event{delivered}); got.Outcome != queue.OutcomeAck {
			t.Fatalf("outcome = %v, want the worked delivery acknowledged", got.Outcome)
		}
		if len(r.reqs) != 0 {
			t.Fatalf("a turn ran %d times on a delivery the ledger had as worked", len(r.reqs))
		}
		if !slices.Contains(gone, delivered) {
			t.Fatal("a delivery acknowledged unrun was not told gone")
		}
	})
	t.Run("a partition that would not merge", func(t *testing.T) {
		t.Parallel()
		d := dispatcher(t, &recorder{result: turn.Result{Decision: phase.Done}})
		var gone []*events.Event
		d.Gone = func(_ context.Context, _ string, evs []*events.Event) { gone = append(gone, evs...) }
		head := said("ana", "first", clock)
		tail := &events.Event{ID: uuid.New(), Type: notificationType, Timestamp: clock.Add(time.Minute)}
		notifyStamp(tail, "slack:C1", "slack:C1")
		if got := d.Dispatch(context.Background(), "ceo", []*events.Event{head, tail}); got.Outcome != queue.OutcomeAck {
			t.Fatalf("outcome = %v, want the head's turn acknowledged", got.Outcome)
		}
		if !slices.Contains(gone, head) || slices.Contains(gone, tail) {
			t.Fatalf("gone: the head %v and the requeued tail %v, want the head alone",
				slices.Contains(gone, head), slices.Contains(gone, tail))
		}
	})
}
