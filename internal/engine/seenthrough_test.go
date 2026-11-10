package engine_test

import (
	"context"
	"slices"
	"testing"

	"github.com/crewlet/crewlet/internal/agent/ledger/ledgerstore"
	"github.com/crewlet/crewlet/internal/agent/phase"
	"github.com/crewlet/crewlet/internal/agent/turn"
	"github.com/crewlet/crewlet/internal/events"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/notify"
	"github.com/crewlet/crewlet/internal/queue"
	"github.com/crewlet/crewlet/internal/statelog"
)

// itemWake is a tracker notice on item at position seq of its log, addressed
// to the seat or not.
func itemWake(item string, seq uint64, addressed bool) *events.Event {
	e := events.New(types.ExternalNotification{NotificationSource: "worklet", SourceEventType: "comment",
		Sender: "ana", Subject: item, Body: "a comment", Addressed: addressed}, events.TraceContext{})
	e.Source, e.Timestamp = "notify.worklet", clock
	e.Payload = map[string]any{notify.PartitionField: "worklet:" + item,
		notify.TriggerField: statelog.Position{Stream: "worklet", Generation: 1, Seq: seq}.String()}
	return e
}

// A WAKE A COMPLETED TURN ALREADY READ PAST, WHICH ASKS NOTHING, IS ABSORBED
// (ADR-0120): a turn woken at position 40 of an item's conversation marks it,
// a later notice there at 38 starts no turn and is counted absorbed, while one
// that is addressed, one past the mark, one in another conversation and a
// vendor's notice with no position all run.
func TestAWakeACompletedTurnReadPastIsAbsorbed(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	r := &recorder{result: turn.Result{Decision: phase.Done}}
	d := dispatcher(t, r)
	d.Completions = ledgerstore.NewMemoryCompletions()
	d.SeenThrough = ledgerstore.NewMemorySeenThrough()
	d.Identify = func(string) (string, string) { return "pm", "id-pm" }
	var skipped []types.TurnTriggerSkipped
	d.Observe = func(_ context.Context, e *events.Event) {
		if s, ok := events.DataAs[*types.TurnTriggerSkipped](e); ok {
			skipped = append(skipped, *s)
		}
	}
	ran := func(evs ...*events.Event) []string {
		t.Helper()
		before := len(r.reqs)
		if got := d.Dispatch(ctx, "pm", evs); got.Outcome != queue.OutcomeAck {
			t.Fatalf("outcome = %v", got.Outcome)
		}
		if len(r.reqs) == before {
			return nil
		}
		return eventIDs(r.reqs[len(r.reqs)-1].Events)
	}

	if got := ran(itemWake("W-1", 40, false)); len(got) != 1 {
		t.Fatalf("the first notice ran %v, want a turn", got)
	}
	if mark := d.SeenThrough.Mark(ctx, "pm", "worklet:W-1", "worklet"); mark.Seq != 40 {
		t.Fatalf("the completed turn marked %v, want position 40", mark)
	}
	if got := ran(itemWake("W-1", 38, false)); got != nil {
		t.Errorf("a notice the turn already read past ran %v", got)
	}
	if len(skipped) != 1 || skipped[0].Disposition != types.DispositionAbsorbed ||
		skipped[0].Agent != "id-pm" || skipped[0].Wake == "" {
		t.Errorf("the absorbed notice was recorded as %+v, want one skip counted absorbed for the seat", skipped)
	}

	// THREE IN ONE DELIVERY: the two at or below the mark are absorbed and
	// the one past it is the follow-up.
	low, mid, high := itemWake("W-1", 37, false), itemWake("W-1", 40, false), itemWake("W-1", 41, false)
	if got := ran(low, mid, high); !slices.Equal(got, []string{high.ID.String()}) {
		t.Errorf("the delivery ran %v, want only the notice past the mark", got)
	}
	// TWO LOGS IN ONE CONVERSATION keep a mark each: a turn woken on the
	// other log leaves this log's mark where it was.
	other := itemWake("W-1", 5, false)
	other.Payload[notify.TriggerField] = statelog.Position{Stream: "chatlet", Generation: 1, Seq: 5}.String()
	if got := ran(other); len(got) != 1 {
		t.Errorf("the first notice on another log ran %v, want a turn", got)
	}
	if got := ran(itemWake("W-1", 39, false)); got != nil {
		t.Errorf("after a turn on another log, a notice this log's mark covers ran %v", got)
	}
	for name, wake := range map[string]*events.Event{
		"an addressed notice the turn read past": itemWake("W-1", 39, true),
		"a notice in another conversation":       itemWake("W-2", 12, false),
		"a vendor's notice with no position":     ev("notification"),
	} {
		if got := ran(wake); len(got) != 1 {
			t.Errorf("%s ran %v, want a turn", name, got)
		}
	}
}
