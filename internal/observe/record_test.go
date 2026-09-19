package observe_test

import (
	"testing"

	"github.com/crewlet/crewlet/internal/events"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/observe"
)

// THE SPEND COLUMNS ARE FILLED BY THE PRODUCTION PATH.
//
// observe.Record is the only production producer of a store.EventRecord — the
// wiring is observe.NewWriter — and it left Spend nil, so the store's
// "derive it when the caller did not" fallback was the only branch ever
// taken. That fallback re-decoded the engine's largest payload, a phase
// completion carrying the whole prompt and tool log, on the publishing
// goroutine of every LLM call.
func TestAPhaseCompletionCarriesItsSpend(t *testing.T) {
	t.Parallel()
	ev := events.New(types.AgentPhaseCompleted{
		Phase:        "execute",
		Model:        "claude-sonnet-5",
		TurnID:       "turn-1",
		Iteration:    2,
		InputTokens:  1200,
		OutputTokens: 340,
		TotalTokens:  1540,
	}, events.TraceContext{})

	rec, ok := observe.Record(ev)
	if !ok {
		t.Fatal("a phase completion is not persisted")
	}
	if rec.Spend == nil {
		t.Fatal("the record carries no spend, so the store must re-decode the " +
			"whole payload to recover it")
	}
	if rec.Spend.Model != "claude-sonnet-5" || rec.Spend.TurnID != "turn-1" {
		t.Errorf("spend identity = %+v", *rec.Spend)
	}
	if rec.Spend.InputTokens != 1200 || rec.Spend.OutputTokens != 340 ||
		rec.Spend.TotalTokens != 1540 {
		t.Errorf("spend tokens = %+v, want the event's own counts", *rec.Spend)
	}
}

// AND AN EVENT THAT IS NOT A PHASE COMPLETION CARRIES NONE, so the columns
// stay at their defaults rather than being filled with zeroes that look like
// a measured zero.
func TestANonPhaseEventCarriesNoSpend(t *testing.T) {
	t.Parallel()
	ev := events.New(types.AgentPhaseStarted{Phase: "plan", TurnID: "turn-1"},
		events.TraceContext{})
	rec, ok := observe.Record(ev)
	if !ok {
		t.Fatal("a phase start is not persisted")
	}
	if rec.Spend != nil {
		t.Errorf("a non-phase-completion event carried spend: %+v", *rec.Spend)
	}
}

// THE INTEGRATIONS ROOM'S TWO COUNTS COME OFF A TAG, and this is the door
// they come through.
//
// A listing never selects the payload column, so `notification_source` is all
// a historical row carries about which third-party app an outcome concerns —
// and queries.integrationOf skips a row without it. The tag was declared in
// internal/store on a mapping function production does not call, while this
// package, which IS the publish listener, kept a second list that did not have
// it: every merge and every drop reached the store untagged, and a company
// whose apps were delivering fine read "0 dropped, 0 merged" for all of them.
func TestARecordNamesTheIntegrationAnOutcomeConcerns(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		ev   *events.Event
	}{
		{"coalesced", events.New(types.NotificationsCoalesced{
			AgentHandle: "swe", NotificationSource: "gitlab", Count: 3,
		}, events.TraceContext{})},
		{"skipped", events.New(types.NotificationSkipped{
			Handle: "swe", Reason: "self_action", NotificationSource: "gitlab",
		}, events.TraceContext{})},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			rec, ok := observe.Record(tc.ev)
			if !ok {
				t.Fatalf("%s is not persisted", tc.ev.Type)
			}
			if got := rec.Tags["notification_source"]; got != "gitlab" {
				t.Errorf("notification_source tag = %q, want gitlab (tags: %v)",
					got, rec.Tags)
			}
		})
	}
}

// AND THE TURN A ROW BELONGS TO, which is the other half of the divergence:
// this list had turn_id and the store's did not, so the copy a test exercised
// and the copy production ran were each missing what the other had.
func TestARecordNamesTheTurnItBelongsTo(t *testing.T) {
	t.Parallel()
	ev := events.New(types.AgentPhaseStarted{
		Phase: "execute", TurnID: "turn-7", RoleName: "SWE",
	}, events.TraceContext{})
	rec, ok := observe.Record(ev)
	if !ok {
		t.Fatal("a phase start is not persisted")
	}
	if rec.Tags["turn_id"] != "turn-7" {
		t.Errorf("turn_id tag = %q; [store.EventLog.Append] reads the column "+
			"back out of here (tags: %v)", rec.Tags["turn_id"], rec.Tags)
	}
}

// ONE TAG, ONE IDENTITY — pinned across two event types built from ONE
// trigger, on the one shape where the two keys differ.
//
// A direct message is one conversation however it is threaded, so a reply in
// a thread on that line has the bare channel as its identity and the thread as
// its inbox partition. Every event a turn publishes carries the identity in
// `conversation_key`; the coalescing record carries the partition, and carried
// it in that same field until this split. A filter on the tag then meant the
// thread a seat is talking on for one row and the batch a wake arrived in for
// the next, with nothing on either row to say which.
func TestTheConversationTagNeverHoldsAPartition(t *testing.T) {
	t.Parallel()
	const (
		identity  = "chat:D1"
		partition = "chat:D1:root-1"
	)
	turnRec, ok := observe.Record(events.New(types.AgentTurnCompleted{
		Agent: "a-1", RoleName: "SWE", ConversationKey: identity,
	}, events.TraceContext{}))
	if !ok {
		t.Fatal("a turn completion is not persisted")
	}
	mergeRec, ok := observe.Record(events.New(types.NotificationsCoalesced{
		AgentHandle: "swe", NotificationSource: "slack", Count: 2,
		PartitionKey: partition,
	}, events.TraceContext{}))
	if !ok {
		t.Fatal("a coalescing record is not persisted")
	}

	if turnRec.Tags["conversation_key"] != identity {
		t.Errorf("the turn's conversation_key tag = %q, want the identity %q",
			turnRec.Tags["conversation_key"], identity)
	}
	if got, present := mergeRec.Tags["conversation_key"]; present {
		t.Errorf("the merge tagged conversation_key = %q; the tag would then "+
			"name a thread for one row and a batch for the next", got)
	}
	if mergeRec.Tags["partition_key"] != partition {
		t.Errorf("the merge's partition_key tag = %q, want the batch %q — "+
			"its own tag, so the partition stays askable of history",
			mergeRec.Tags["partition_key"], partition)
	}
	if got, present := turnRec.Tags["partition_key"]; present {
		t.Errorf("the turn tagged partition_key = %q; only a coalescing "+
			"record has a batch to name", got)
	}
}
