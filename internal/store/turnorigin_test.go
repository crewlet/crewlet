package store_test

import (
	"encoding/json"
	"slices"
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

// A TURN SAYS WHAT STARTED IT, AND IS FOUND BY EVERY MESSAGE IT TOOK IN.
//
// A turn's trigger named the waking EVENT (a delivery, minted per recipient
// and per attempt), so a person holding the chat message they wrote could not
// find the turn it started. The trigger now names its origin, and a COALESCED
// trigger every message it took in: a burst of three merged into one turn is
// found from each of the three, which is exactly the busy case where a person
// asks. The row carries the primary origin (the latest constituent's), and
// `origin=` selects turns whole, off crewlet_turn_origins (node/0059).
//
// Mutation: index the primary origin alone in TurnOrigins and the two earlier
// messages find nothing.
func TestATurnIsFoundByEveryMessageItTookIn(t *testing.T) {
	t.Parallel()
	log := open(t).Events()
	at := time.Now().UTC().Add(-time.Hour)
	burst := triggerFrom(types.OriginChatMessage, "msg-3", "answerer")
	burst["origins"] = []any{
		map[string]any{"kind": types.OriginChatMessage, "id": "msg-1", "reason": "answerer"},
		map[string]any{"kind": types.OriginChatMessage, "id": "msg-2", "reason": "answerer"},
		map[string]any{"kind": types.OriginChatMessage, "id": "msg-3", "reason": "answerer"},
	}
	appendAsWritten(t, log, "s1", "agent_turn_started", at, map[string]any{
		"turn_id": "run-1", "trigger": burst})
	appendAsWritten(t, log, "p1", "agent_phase_completed", at.Add(time.Second),
		map[string]any{"turn_id": "run-1", "phase": "execute", "total_tokens": 12,
			"trigger": burst})
	// RESUMED BY ANOTHER MESSAGE: the segment's own start names it, so the
	// answer that resumed the turn finds it too, and the row stays filed
	// under what started it.
	resumed := triggerFrom(types.OriginChatMessage, "msg-4", "dm")
	appendAsWritten(t, log, "s2", "agent_turn_started", at.Add(time.Minute),
		map[string]any{"turn_id": "run-1", "resumed": true, "trigger": resumed})
	appendAsWritten(t, log, "p2", "agent_phase_completed", at.Add(2*time.Minute),
		map[string]any{"turn_id": "run-1", "phase": "execute", "total_tokens": 6,
			"trigger": resumed})
	appendAsWritten(t, log, "x", "agent_turn_started", at.Add(3*time.Minute),
		map[string]any{"turn_id": "run-other",
			"trigger": triggerFrom(types.OriginSchedule, "daily standup", "")})

	for _, message := range []string{"msg-1", "msg-2", "msg-3", "msg-4"} {
		got, err := log.Turns(t.Context(), store.TurnQuery{Origin: "chat_message:" + message})
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != 1 || got[0].TurnID != "run-1" {
			t.Fatalf("turns %s started = %+v, want run-1 alone", message, got)
		}
		if got[0].TotalTokens != 18 {
			t.Errorf("found by %s, run-1 folds %d tokens, want 18: the filter "+
				"selects turns whole", message, got[0].TotalTokens)
		}
		want := types.TriggerOrigin{Kind: types.OriginChatMessage, ID: "msg-3",
			Reason: "answerer"}
		if got[0].Origin == nil || *got[0].Origin != want {
			t.Errorf("found by %s, run-1 says it was started by %+v, want the "+
				"primary origin %+v", message, got[0].Origin, want)
		}
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
	if got, _ := log.Turns(t.Context(), store.TurnQuery{Origin: "chat_message:msg-9"}); len(got) != 0 {
		t.Errorf("a message that started nothing found %d turns", len(got))
	}
}

// ONLY A TURN'S START IS INDEXED, each origin once: its phase records repeat
// the trigger, and indexing them would write the same rows sixty times.
func TestOnlyATurnsStartNamesItsOrigins(t *testing.T) {
	t.Parallel()
	trigger := triggerFrom(types.OriginChatMessage, "msg-1", "mention")
	trigger["origins"] = []any{
		map[string]any{"kind": types.OriginChatMessage, "id": "msg-1"},
		map[string]any{"kind": types.OriginChatMessage, "id": "msg-0"},
		map[string]any{"kind": "chat_message"},
	}
	raw, _ := json.Marshal(map[string]any{"turn_id": "run-1", "trigger": trigger})
	got := store.TurnOrigins("agent_turn_started", raw)
	if want := []string{"chat_message:msg-1", "chat_message:msg-0"}; !slices.Equal(got, want) {
		t.Errorf("a start names %v, want %v (the primary first, each once, both halves)",
			got, want)
	}
	if got := store.TurnOrigins("agent_phase_completed", raw); got != nil {
		t.Errorf("a phase record names %v, want none", got)
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
