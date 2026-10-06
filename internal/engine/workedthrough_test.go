package engine_test

import (
	"context"
	"errors"
	"testing"

	"github.com/crewlet/crewlet/internal/agent/ledger/ledgerstore"
	"github.com/crewlet/crewlet/internal/agent/phase"
	"github.com/crewlet/crewlet/internal/agent/turn"
	"github.com/crewlet/crewlet/internal/events"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/notify"
	"github.com/crewlet/crewlet/internal/queue"
	"github.com/crewlet/crewlet/internal/workkey"
)

// threadMessage is a chat message in thread 1.0 of channel C1, with its own
// backend id — the `ts` the thread block knows it by.
func threadMessage(ts, body string) *events.Event {
	salient := body
	e := events.New(types.ExternalNotification{
		NotificationSource: "slack", SourceEventType: "message",
		Sender: "ana", Body: body, SalientBody: &salient,
		Metadata: map[string]string{
			notify.TransportField: "slack", "channel": "C1",
			"thread_ts": "1.0", "ts": ts,
		},
	}, events.TraceContext{})
	e.Source = "notify.slack"
	e.Timestamp = clock
	notifyStamp(e, "slack:C1:1.0", "slack:C1:1.0")
	return e
}

// workedThroughKey is the ledger key a turn records for a thread message it
// was shown waiting — the same derivation the dispatch drops on.
func workedThroughKey(ts string) string {
	return workkey.Derive([]string{"chat-message", "slack", "C1", ts})
}

// THE WORKED-THROUGH RECORD, end to end through the dispatcher.
//
// M1's turn fails before anything leaves the engine and is NAKed, which on the
// only broker this engine ships returns it BEHIND the conversation's newer
// mail. So M2 runs first, and its turn is shown M1 waiting in the thread and
// answers the thread as it stands — which the turn reports, and the dispatch
// records as worked through. When M1 comes round it is dropped as worked,
// with a record saying a later turn answered it, rather than run as a turn
// that answers it a second time, out of order.
func TestAMessageALaterTurnAnsweredInItsThreadIsNotRunAgain(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	completions := ledgerstore.NewMemoryCompletions()
	m1, m2 := threadMessage("1.1", "it loops on /login"), threadMessage("1.2", "and on /logout")

	r := &recorder{err: errors.New("the provider did not answer")}
	d := dispatcher(t, r)
	d.Completions = completions
	var seen []*events.Event
	d.Observe = func(_ context.Context, e *events.Event) { seen = append(seen, e) }

	if got := d.Dispatch(ctx, "swe", []*events.Event{m1}); got.Outcome != queue.OutcomeNak {
		t.Fatalf("M1 = %v, want a NAK: nothing left the engine, so it is retried", got.Outcome)
	}

	// M2's turn completes, reporting the waiting message it was shown.
	r.err = nil
	r.result = turn.Result{Decision: phase.Done, WorkedThrough: []string{workedThroughKey("1.1")}}
	if got := d.Dispatch(ctx, "swe", []*events.Event{m2}); got.Outcome != queue.OutcomeAck {
		t.Fatalf("M2 = %v, want an ack", got.Outcome)
	}

	// M1 comes round behind it.
	if got := d.Dispatch(ctx, "swe", []*events.Event{m1}); got.Outcome != queue.OutcomeAck {
		t.Fatalf("M1's retry = %v, want an ack", got.Outcome)
	}
	if len(r.reqs) != 2 {
		t.Fatalf("%d turns ran, want 2 (M1's failed one and M2's): M1 was answered in M2's "+
			"thread and its retry ran a turn that answers it again", len(r.reqs))
	}
	var skipped *types.TurnTriggerSkipped
	for _, e := range seen {
		if s, ok := events.DataAs[*types.TurnTriggerSkipped](e); ok && s.TriggerID == m1.ID.String() {
			skipped = s
		}
	}
	if skipped == nil {
		t.Fatal("nothing recorded that M1 was dropped")
	}
	if skipped.Reason != "a later turn was shown this message waiting in its thread and answered it there" {
		t.Errorf("the skip says %q, which does not tell a reader a LATER turn answered it",
			skipped.Reason)
	}
}

// AND ONLY WHAT THE LATER TURN REPORTS. A message its thread block did not show
// it waiting — a top-level message with no thread block, one a bound dropped,
// or one the turn never reached — is still owed an answer, and its retry runs.
func TestAMessageNoLaterTurnWasShownStillRuns(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	completions := ledgerstore.NewMemoryCompletions()
	m1, m2 := threadMessage("1.1", "it loops on /login"), threadMessage("1.2", "and on /logout")

	r := &recorder{err: errors.New("the provider did not answer")}
	d := dispatcher(t, r)
	d.Completions = completions
	d.Dispatch(ctx, "swe", []*events.Event{m1})

	r.err = nil
	r.result = turn.Result{Decision: phase.Done} // shown nothing waiting
	d.Dispatch(ctx, "swe", []*events.Event{m2})

	if got := d.Dispatch(ctx, "swe", []*events.Event{m1}); got.Outcome != queue.OutcomeAck {
		t.Fatalf("M1's retry = %v", got.Outcome)
	}
	if len(r.reqs) != 3 {
		t.Fatalf("%d turns ran, want 3: nothing answered M1, so its retry is owed a turn",
			len(r.reqs))
	}
}

// AND A FAILED TURN ANSWERED NOTHING, so what it was shown is not recorded.
func TestAFailedTurnRecordsNothingAsWorkedThrough(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	completions := ledgerstore.NewMemoryCompletions()
	r := &recorder{
		err:    errors.New("the provider did not answer"),
		result: turn.Result{WorkedThrough: []string{workedThroughKey("1.1")}},
	}
	d := dispatcher(t, r)
	d.Completions = completions
	d.Dispatch(ctx, "swe", []*events.Event{threadMessage("1.2", "and on /logout")})
	key := workedThroughKey("1.1")
	if completions.Worked(ctx, "swe", []string{key})[key] {
		t.Fatal("a turn that failed recorded a message it was shown as answered")
	}
}
