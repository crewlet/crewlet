package queries_test

import (
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/api/queries"
	"github.com/crewlet/crewlet/internal/store"
)

// `work_item=` NARROWS BOTH THE EVENTS AND THE TURNS TO ONE ITEM, and a value
// that is not an item's identity is REFUSED rather than matched.
//
// A key ("ENG-4") or a bare id matches nothing, and an empty answer to it reads
// as "nothing happened on this item" — the one conclusion the caller who pasted
// the wrong half must not draw. The event axis compiles through the same
// filter reader, so it is held here too.
func TestTheWorkItemFilterNarrowsEventsAndTurnsAndRefusesAKey(t *testing.T) {
	t.Parallel()
	db := openStore(t)
	log := db.Events()
	at := time.Now().UTC().Add(-time.Hour)
	for i, body := range []map[string]any{
		{"turn_id": "run-1", "duration_ms": 10,
			"work_item": map[string]string{"backend": "jira", "id": "10042", "key": "ENG-4"}},
		{"turn_id": "run-2", "duration_ms": 10},
	} {
		raw, _ := json.Marshal(body)
		if err := log.Append(t.Context(), store.EventRecord{
			ID: []string{"on", "off"}[i], Type: "turn_completed", Source: "engine",
			Category: "lifecycle", Time: at.Add(time.Duration(i) * time.Second),
			Tags: store.ExtractTags(raw), Payload: raw,
		}); err != nil {
			t.Fatal(err)
		}
	}
	r := registryOver(t, queries.Sources{Events: fleetOf(log)})

	events := ask(t, r, "events", map[string]any{"work_item": "jira:10042"})["events"].([]store.EventRecord)
	if len(events) != 1 || events[0].ID != "on" {
		t.Errorf("events on jira:10042 = %+v, want the one naming it", events)
	}
	turns := ask(t, r, "turns", map[string]any{"work_item": "jira:10042"})["turns"].([]store.Turn)
	if len(turns) != 1 || turns[0].TurnID != "run-1" {
		t.Errorf("turns on jira:10042 = %+v, want run-1 alone", turns)
	}

	series := askRaw(t, r, "event_series", map[string]any{
		"work_item": "jira:10042", "bucket": "hour"}).(queries.SeriesAnswer)
	if series.Total != 1 {
		t.Errorf("the axis on jira:10042 counts %d events, want the one the list shows",
			series.Total)
	}

	for _, what := range []string{"events", "event_series", "turns"} {
		for _, bad := range []string{"ENG-4", "jira:", ":10042"} {
			// A VALID BUCKET, so the only thing wrong is the item.
			params := map[string]any{"work_item": bad, "bucket": "hour"}
			if _, err := r.Answer(t.Context(), what,
				params, ""); !errors.Is(err, queries.ErrBadParams) {
				t.Errorf("%s work_item=%q: err = %v, want ErrBadParams", what, bad, err)
			}
		}
	}
}
