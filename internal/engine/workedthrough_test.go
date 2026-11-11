package engine_test

import (
	"context"
	"errors"
	"testing"

	"github.com/crewlet/crewlet/internal/agent/ledger/ledgerstore"
	"github.com/crewlet/crewlet/internal/agent/phase"
	"github.com/crewlet/crewlet/internal/agent/turn"
	"github.com/crewlet/crewlet/internal/chatlet"
	"github.com/crewlet/crewlet/internal/events"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/notify"
	"github.com/crewlet/crewlet/internal/queue"
	"github.com/crewlet/crewlet/internal/statelog"
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

// chatWake is bea's Chatlet message in thread msg-0 of room-1, written for
// whom, as Chatlet's parser states it: its record's op as the event type, and
// the position the record was committed at as its trigger.
func chatWake(id string, op chatlet.OpKind, trigger string, whom *types.WorkingFor) *events.Event {
	e := events.New(types.ExternalNotification{
		NotificationSource: chatlet.Source, SourceEventType: string(op),
		Sender: "bea", Body: "the plan",
		Writer: notify.FirstPartyWriter(iam.ActorAgent, "bea", nil, whom),
		Metadata: map[string]string{
			notify.TransportField: chatlet.Source, notify.ChannelField: "room-1",
			notify.ThreadField: "msg-0", notify.MessageIDField: id,
		},
	}, events.TraceContext{})
	e.Source = "notify." + chatlet.Source
	e.Timestamp = clock
	notifyStamp(e, "chat:room-1:msg-0", "chat:room-1:msg-0")
	if trigger != "" {
		e.Payload[notify.TriggerField] = trigger
	}
	return e
}

// AN EDIT'S WAKE IS DROPPED ONLY WHERE A TURN FOR ITS REQUESTER WAS SHOWN THAT
// EDIT, end to end through the dispatcher.
//
// A turn woken by bea's message msg-3, written for ada, was shown bea's earlier
// msg-1 waiting at its edit e1, and reports it: its post's key, and the key of
// version e1 for ada. A mark of the message alone used to drop every later
// wake of it, an edit made after the read included, whose words no turn saw.
// Now the redelivery of msg-1's post is dropped, and so is e1's wake made for
// ada, with a reason that names the edit. But e1's wake made for omar runs,
// owed a turn worked for omar; so does a later edit e2; and so does an edit's
// wake that carries no trigger position, since nothing says which version it
// is.
//
// Mutation: key an edit's wake by the message's key, as main did, or give the
// edit's skip the post's reason, and a row goes red.
func TestAnEditsWakeIsDroppedOnlyWhereATurnForItsRequesterWasShownThatEdit(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	ada := &types.WorkingFor{Kind: types.WorkingForSeat, Seat: "ada"}
	omar := &types.WorkingFor{Kind: types.WorkingForSeat, Seat: "omar"}
	at := func(seq uint64) string {
		return statelog.Position{Stream: "chat", Generation: 1, Seq: seq}.String()
	}
	e1, e2 := at(41), at(57)
	// What the marking turn reports, by the derivation the dispatch drops on
	// (TestAMarkDropsOnlyTheEditItsTurnWasShownForTheSameWhom holds the two
	// sides of it to each other).
	post := workkey.Derive([]string{"chat-message", chatlet.Source, "room-1", "msg-1"})
	edit := workkey.Derive([]string{"chat-edit", chatlet.Source, "room-1", "msg-1", e1,
		notify.WorkingForKey(ada)})

	r := &recorder{result: turn.Result{Decision: phase.Done, WorkedThrough: []string{post, edit}}}
	d := dispatcher(t, r)
	d.Completions = ledgerstore.NewMemoryCompletions()
	var seen []*events.Event
	d.Observe = func(_ context.Context, e *events.Event) { seen = append(seen, e) }
	marking := chatWake("msg-3", chatlet.OpPost, at(60), ada)
	if got := d.Dispatch(ctx, "swe", []*events.Event{marking}); got.Outcome != queue.OutcomeAck {
		t.Fatalf("the marking turn = %v, want an ack", got.Outcome)
	}
	r.result = turn.Result{Decision: phase.Done}

	const (
		postReason = "a later turn was shown this message waiting in its thread and answered it there"
		editReason = "a later turn was shown this edit waiting in its thread and answered it there"
	)
	cases := []struct {
		name   string
		ev     *events.Event
		reason string // empty: the wake runs a turn
	}{
		{"msg-1's post, delivered again", chatWake("msg-1", chatlet.OpPost, at(30), ada), postReason},
		{"the wake of edit e1, made for ada", chatWake("msg-1", chatlet.OpEdit, e1, ada), editReason},
		{"the wake of edit e1, made for omar", chatWake("msg-1", chatlet.OpEdit, e1, omar), ""},
		{"the wake of a later edit e2", chatWake("msg-1", chatlet.OpEdit, e2, ada), ""},
		{"an edit's wake with no trigger position", chatWake("msg-1", chatlet.OpEdit, "", ada), ""},
	}
	for _, tc := range cases {
		before := len(r.reqs)
		if got := d.Dispatch(ctx, "swe", []*events.Event{tc.ev}); got.Outcome != queue.OutcomeAck {
			t.Errorf("%s = %v, want an ack", tc.name, got.Outcome)
			continue
		}
		ran := len(r.reqs) > before
		switch {
		case tc.reason == "" && !ran:
			t.Errorf("%s was dropped, want a turn of its own: no turn was shown it working for "+
				"whom it was made for", tc.name)
		case tc.reason != "" && ran:
			t.Errorf("%s ran a turn, want it dropped: the marking turn answered it", tc.name)
		case tc.reason != "":
			var skipped *types.TurnTriggerSkipped
			for _, e := range seen {
				if s, ok := events.DataAs[*types.TurnTriggerSkipped](e); ok && s.TriggerID == tc.ev.ID.String() {
					skipped = s
				}
			}
			if skipped == nil || skipped.Reason != tc.reason {
				t.Errorf("%s was dropped with %+v, want the reason %q", tc.name, skipped, tc.reason)
			}
		}
	}
}
