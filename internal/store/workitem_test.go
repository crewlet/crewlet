package store_test

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/store"
)

// appendAsWritten stores one event the way the publish listener does: tags
// and spend derived from its serialized form, never set by hand.
func appendAsWritten(t *testing.T, log *store.EventLog, id, eventType string,
	at time.Time, payload map[string]any) {

	t.Helper()
	raw, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	if err := log.Append(t.Context(), store.EventRecord{
		ID: id, Type: eventType, Source: "engine", Category: "lifecycle",
		Time: at, Tags: store.ExtractTags(raw),
		Spend:   store.SpendFor(eventType, raw),
		Payload: raw,
	}); err != nil {
		t.Fatalf("append %s: %v", id, err)
	}
}

var nativeItem = map[string]string{
	"backend": "native", "id": "task-7", "key": "ENG-7", "project": "ENG",
}

// THE WORK ITEM IS PROMOTED FROM THE NESTED PAYLOAD.
//
// Every turn-level record carries `work_item` as an OBJECT, and the flat tag
// list reads top-level strings only — so without the one nested rule the
// column schema/0031 adds is empty on every row and "everything that happened
// on this item" is a json_extract over the whole window. The tag is the item's
// identity across trackers, never its key: a move rewrites the key.
func TestTheWorkItemIsPromotedFromTheNestedPayload(t *testing.T) {
	t.Parallel()
	raw, _ := json.Marshal(map[string]any{"turn_id": "run-1", "work_item": nativeItem})
	if got := store.ExtractTags(raw)["work_item"]; got != "native:task-7" {
		t.Fatalf("work_item tag = %q, want the item's ref native:task-7", got)
	}
	// BOTH HALVES OR NOTHING: `native:` would make every id-less record one
	// item.
	for name, item := range map[string]any{
		"no id":        map[string]string{"backend": "native", "key": "ENG-7"},
		"no backend":   map[string]string{"id": "task-7"},
		"not a object": "native:task-7",
	} {
		raw, _ := json.Marshal(map[string]any{"work_item": item})
		if got, ok := store.ExtractTags(raw)["work_item"]; ok {
			t.Errorf("%s: tagged %q, want no item", name, got)
		}
	}
	// A TRACKER THIS BUILD DOES NOT KNOW is still an item: a newer peer's
	// record is indexed exactly as a known one is.
	raw, _ = json.Marshal(map[string]any{
		"work_item": map[string]string{"backend": "linear", "id": "L-1"}})
	if got := store.ExtractTags(raw)["work_item"]; got != "linear:L-1" {
		t.Errorf("an unknown backend's item tagged %q, want linear:L-1", got)
	}

	// AND IT REACHES THE COLUMN the event filter reads.
	log := open(t).Events()
	at := time.Now().UTC().Add(-time.Minute)
	appendAsWritten(t, log, "on", "agent_turn_started", at,
		map[string]any{"turn_id": "run-1", "work_item": nativeItem})
	appendAsWritten(t, log, "off", "agent_turn_started", at.Add(time.Second),
		map[string]any{"turn_id": "run-2"})
	got, err := log.List(t.Context(), store.ListQuery{WorkItem: "native:task-7"})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].ID != "on" {
		t.Errorf("events on native:task-7 = %+v, want only the one naming it", got)
	}
	// The KEY is a label, not the identity, and matches nothing.
	if got, _ := log.List(t.Context(), store.ListQuery{WorkItem: "native:ENG-7"}); len(got) != 0 {
		t.Errorf("a filter on the key matched %d events", len(got))
	}
}

// THE ITEM FILTER SELECTS TURNS, NOT ROWS, so the row it lists is the same one
// the unfiltered list shows: a parked segment's completion, resolved before
// the sole write that named the item, still counts toward the turn.
func TestTheTurnListNarrowsToOneItemWithTheWholeTurn(t *testing.T) {
	t.Parallel()
	log := open(t).Events()
	at := time.Now().UTC().Add(-time.Hour)
	appendAsWritten(t, log, "p1", "agent_phase_completed", at, map[string]any{
		"turn_id": "run-1", "phase": "execute", "input_tokens": 10,
		"output_tokens": 2, "total_tokens": 12})
	appendAsWritten(t, log, "c1", "turn_completed", at.Add(time.Second), map[string]any{
		"turn_id": "run-1", "suspended": true, "duration_ms": 1000})
	appendAsWritten(t, log, "p2", "agent_phase_completed", at.Add(time.Minute), map[string]any{
		"turn_id": "run-1", "phase": "execute", "input_tokens": 5,
		"output_tokens": 1, "total_tokens": 6})
	appendAsWritten(t, log, "c2", "turn_completed", at.Add(2*time.Minute), map[string]any{
		"turn_id": "run-1", "duration_ms": 500, "work_item": nativeItem,
		"work_item_basis": "sole_write"})
	appendAsWritten(t, log, "x", "turn_completed", at.Add(3*time.Minute), map[string]any{
		"turn_id": "run-other", "duration_ms": 1})

	got, err := log.Turns(t.Context(), store.TurnQuery{WorkItem: "native:task-7"})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].TurnID != "run-1" {
		t.Fatalf("turns on native:task-7 = %+v, want run-1 alone", got)
	}
	if got[0].TotalTokens != 18 || got[0].Phases != 2 || got[0].DurationMS != 1500 {
		t.Errorf("run-1 = %d tokens, %d phases, %d ms; want 18, 2 and 1500 — "+
			"the filter folded only the rows naming the item",
			got[0].TotalTokens, got[0].Phases, got[0].DurationMS)
	}
}

// completion writes one turn_completed segment.
func completion(t *testing.T, log *store.EventLog, id, turn string, at time.Time,
	durationMS int, suspended *bool) {

	t.Helper()
	body := map[string]any{"turn_id": turn, "duration_ms": durationMS}
	if suspended != nil {
		body["suspended"] = *suspended
	}
	appendAsWritten(t, log, id, "turn_completed", at, body)
}

func onlyTurn(t *testing.T, log *store.EventLog) store.Turn {
	t.Helper()
	got, err := log.Turns(t.Context(), store.TurnQuery{})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("turns = %+v, want one", got)
	}
	return got[0]
}

// A PARKED TURN IS NOT COMPLETE.
//
// The segment that launches a detached coding run publishes turn_completed
// with `suspended`, and the turn completes again when the run is collected.
// Read as an end, this list called the turn finished the moment it parked.
func TestAParkedTurnIsNotComplete(t *testing.T) {
	t.Parallel()
	log := open(t).Events()
	yes := true
	completion(t, log, "c1", "run-1", time.Now().UTC().Add(-time.Hour), 3000, &yes)

	turn := onlyTurn(t, log)
	if turn.Complete || !turn.Parked {
		t.Errorf("complete=%v parked=%v, want a parked turn that has not ended",
			turn.Complete, turn.Parked)
	}
	if turn.DurationMS != 3000 {
		t.Errorf("duration = %d, want the segment it has worked so far", turn.DurationMS)
	}
}

// A SEGMENTED TURN SUMS ITS DURATIONS, and the newest segment says what it is.
//
// The duration was the first completion's alone, so a turn that parked after
// three seconds and worked another second on resume listed as three seconds
// long. The wait between the segments is the coding run's, not the turn's.
func TestASegmentedTurnSumsItsDurations(t *testing.T) {
	t.Parallel()
	yes := true
	base := time.Now().UTC().Add(-time.Hour)

	finished := open(t).Events()
	completion(t, finished, "c1", "run-1", base, 3000, &yes)
	completion(t, finished, "c2", "run-1", base.Add(10*time.Minute), 1200, nil)
	turn := onlyTurn(t, finished)
	if !turn.Complete || turn.Parked || turn.DurationMS != 4200 {
		t.Errorf("parked then finished: complete=%v parked=%v duration=%d, "+
			"want complete, not parked, 4200", turn.Complete, turn.Parked, turn.DurationMS)
	}

	// PARKED AGAIN after a resume is parked: the newest completion decides.
	again := open(t).Events()
	completion(t, again, "c1", "run-1", base, 3000, &yes)
	completion(t, again, "c2", "run-1", base.Add(10*time.Minute), 1200, &yes)
	turn = onlyTurn(t, again)
	if turn.Complete || !turn.Parked || turn.DurationMS != 4200 {
		t.Errorf("parked twice: complete=%v parked=%v duration=%d, "+
			"want parked, not complete, 4200", turn.Complete, turn.Parked, turn.DurationMS)
	}
}

// AN OLDER COMPLETION FOLDS AS BEFORE.
//
// `suspended` is omitempty, and a build that predates it never writes it: a
// completion naming no flag, or naming it false, is an END, exactly as it was
// read before the flag existed.
func TestAnOlderCompletionFoldsAsBefore(t *testing.T) {
	t.Parallel()
	no := false
	for name, flag := range map[string]*bool{"absent": nil, "false": &no} {
		log := open(t).Events()
		completion(t, log, "c1", "run-1", time.Now().UTC().Add(-time.Hour), 4200, flag)
		turn := onlyTurn(t, log)
		if !turn.Complete || turn.Parked || turn.DurationMS != 4200 {
			t.Errorf("suspended %s: complete=%v parked=%v duration=%d, want an "+
				"ended 4200 ms turn", name, turn.Complete, turn.Parked, turn.DurationMS)
		}
	}
}

// A TURN'S CACHE COUNTS ARE ITS PHASES', off the columns schema/0030 promoted,
// and they break the input down rather than add to the total.
func TestATurnSumsItsCacheCounts(t *testing.T) {
	t.Parallel()
	log := open(t).Events()
	at := time.Now().UTC().Add(-time.Hour)
	for i, cache := range []int{300, 500} {
		raw, err := json.Marshal(types.AgentPhaseCompleted{
			TurnID: "run-1", Phase: "execute", Iteration: i + 1,
			InputTokens: 1000, OutputTokens: 100, TotalTokens: 1100,
			CacheReadTokens: cache, CacheWriteTokens: 50,
		})
		if err != nil {
			t.Fatal(err)
		}
		var body map[string]any
		_ = json.Unmarshal(raw, &body)
		appendAsWritten(t, log, "p"+string(rune('1'+i)), "agent_phase_completed",
			at.Add(time.Duration(i)*time.Second), body)
	}
	turn := onlyTurn(t, log)
	if turn.CacheRead != 800 || turn.CacheWrite != 100 || turn.TotalTokens != 2200 {
		t.Errorf("cache %d/%d, total %d; want 800/100 and a total of 2200 the "+
			"cache does not change", turn.CacheRead, turn.CacheWrite, turn.TotalTokens)
	}
}
