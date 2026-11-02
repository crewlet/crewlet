package store_test

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/store"
)

// triggerFrom is a trigger as a turn's records carry it, naming its origin.
func triggerFrom(kind, id, reason string) map[string]any {
	return types.Trigger{ID: "wake-" + id, Type: "external_notification",
		Origin: types.TriggerOrigin{Kind: kind, ID: id, Reason: reason}}.Map()
}

// A TURN SAYS WHAT STARTED IT, AND IS FOUND BY IT.
//
// A turn's trigger named the waking EVENT (a delivery, minted per recipient
// and per attempt), so a person holding the chat message they wrote could not
// find the turn it started. The trigger now names its origin, the log files it
// as `<kind>:<id>` (the trigger_origin column, node/0059), the row carries it,
// and `origin=` selects the turns an object started, whole.
//
// Mutation: drop the nested read in ExtractTags and the row has no origin and
// the filter finds nothing.
func TestATurnSaysWhatStartedItAndIsFoundByIt(t *testing.T) {
	t.Parallel()
	log := open(t).Events()
	at := time.Now().UTC().Add(-time.Hour)
	started := triggerFrom(types.OriginChatMessage, "msg-1", "answerer")
	appendAsWritten(t, log, "s1", "agent_turn_started", at, map[string]any{
		"turn_id": "run-1", "trigger": started})
	appendAsWritten(t, log, "p1", "agent_phase_completed", at.Add(time.Second),
		map[string]any{"turn_id": "run-1", "phase": "execute", "total_tokens": 12,
			"trigger": started})
	// RESUMED BY ANOTHER MESSAGE: the turn is still filed under what
	// started it, and found by the message that resumed it too.
	appendAsWritten(t, log, "p2", "agent_phase_completed", at.Add(time.Minute),
		map[string]any{"turn_id": "run-1", "phase": "execute", "total_tokens": 6,
			"trigger": triggerFrom(types.OriginChatMessage, "msg-2", "dm")})
	appendAsWritten(t, log, "x", "agent_phase_completed", at.Add(2*time.Minute),
		map[string]any{"turn_id": "run-other", "phase": "execute",
			"trigger": triggerFrom(types.OriginSchedule, "daily standup", "")})

	got, err := log.Turns(t.Context(), store.TurnQuery{Origin: "chat_message:msg-1"})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].TurnID != "run-1" {
		t.Fatalf("turns msg-1 started = %+v, want run-1 alone", got)
	}
	want := types.TriggerOrigin{Kind: types.OriginChatMessage, ID: "msg-1", Reason: "answerer"}
	if got[0].Origin == nil || *got[0].Origin != want {
		t.Errorf("run-1 says it was started by %+v, want %+v (the earliest record's)",
			got[0].Origin, want)
	}
	if got[0].TotalTokens != 18 {
		t.Errorf("run-1 folds %d tokens, want 18: the filter selects turns whole",
			got[0].TotalTokens)
	}
	resumed, err := log.Turns(t.Context(), store.TurnQuery{Origin: "chat_message:msg-2"})
	if err != nil {
		t.Fatal(err)
	}
	if len(resumed) != 1 || resumed[0].TurnID != "run-1" {
		t.Errorf("turns msg-2 resumed = %+v, want run-1", resumed)
	}
	all, err := log.Turns(t.Context(), store.TurnQuery{})
	if err != nil {
		t.Fatal(err)
	}
	for _, turn := range all {
		if turn.TurnID == "run-other" && (turn.Origin == nil ||
			turn.Origin.Kind != types.OriginSchedule || turn.Origin.ID != "daily standup") {
			t.Errorf("the schedule's turn says it was started by %+v", turn.Origin)
		}
	}
}

// AN ORIGIN IS BOTH HALVES OR NOTHING, for the work item's reason: a kind with
// no id would file every such record under one object.
func TestAnOriginNamesBothHalvesOrNothing(t *testing.T) {
	t.Parallel()
	for name, origin := range map[string]types.TriggerOrigin{
		"no id":   {Kind: types.OriginChatMessage},
		"no kind": {ID: "msg-1"},
	} {
		raw, _ := json.Marshal(map[string]any{"trigger": types.Trigger{
			Type: "external_notification", Origin: origin}.Map()})
		if got, ok := store.ExtractTags(raw)["trigger_origin"]; ok {
			t.Errorf("%s: tagged %q, want no origin", name, got)
		}
	}
	raw, _ := json.Marshal(map[string]any{"trigger": triggerFrom("work_item", "ENG-7", "assignee")})
	tags := store.ExtractTags(raw)
	if tags["trigger_origin"] != "work_item:ENG-7" || tags["trigger_reason"] != "assignee" {
		t.Errorf("tags = %v, want work_item:ENG-7 under assignee", tags)
	}
}

// A FLEET'S PARTIALS KEEP THE EARLIEST ORIGIN, as one log holding every row
// would: the turn began on one node and resumed on another.
func TestCombinedPartialsKeepTheEarliestOrigin(t *testing.T) {
	t.Parallel()
	log := open(t).Events()
	at := time.Now().UTC().Add(-time.Hour)
	appendAsWritten(t, log, "late", "agent_phase_completed", at.Add(time.Minute),
		map[string]any{"turn_id": "run-1", "phase": "execute",
			"trigger": triggerFrom(types.OriginChatMessage, "msg-2", "dm")})
	other := open(t).Events()
	appendAsWritten(t, other, "early", "agent_phase_completed", at,
		map[string]any{"turn_id": "run-1", "phase": "execute",
			"trigger": triggerFrom(types.OriginChatMessage, "msg-1", "answerer")})
	q := store.TurnQuery{IDs: []string{"run-1"}}
	a, _, err := log.TurnPartials(t.Context(), q)
	if err != nil {
		t.Fatal(err)
	}
	b, _, err := other.TurnPartials(t.Context(), q)
	if err != nil {
		t.Fatal(err)
	}
	if len(a) != 1 || len(b) != 1 {
		t.Fatalf("partials = %d and %d, want one each", len(a), len(b))
	}
	for _, order := range [][]store.TurnPartial{{a[0], b[0]}, {b[0], a[0]}} {
		turn := store.CombineTurnPartials(order...).Turn()
		if turn.Origin == nil || turn.Origin.ID != "msg-1" {
			t.Errorf("combined origin = %+v, want msg-1, the earliest", turn.Origin)
		}
	}
}
