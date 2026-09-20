package store_test

import (
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/store"
)

// READING ONE TURN MEANS EVERY ROW IT TOUCHED.
//
// `turn_id` is promoted out of the payload by the spend extractor, whose
// subject is SPEND — so it reads one event type and returns nil for the rest.
// That is right for the rollup and wrong for identity: a delivery, a tool call
// or an A2A ask carries a turn id without carrying a token count, and a trace
// built only from phase completions is missing most of what happened.
func TestATurnReadsEveryEventItTouchedNotOnlyItsPhases(t *testing.T) {
	t.Parallel()
	log := open(t).Events()
	at := time.Now().UTC().Add(-time.Minute)

	for i, rec := range []store.EventRecord{
		{
			ID: "e-phase", Type: "agent_phase_completed", Source: "engine",
			Category: "agent", Tags: map[string]string{"turn_id": "turn-1"},
			Payload: []byte(`{"turn_id":"turn-1","phase":"plan","total_tokens":12}`),
		},
		{
			ID: "e-delivery", Type: "message_delivered", Source: "engine",
			Category: "comms", Tags: map[string]string{"turn_id": "turn-1"},
			Payload: []byte(`{"turn_id":"turn-1"}`),
		},
		{
			ID: "e-other", Type: "message_delivered", Source: "engine",
			Category: "comms", Tags: map[string]string{"turn_id": "turn-2"},
			Payload: []byte(`{"turn_id":"turn-2"}`),
		},
	} {
		rec.Time = at.Add(time.Duration(i) * time.Second)
		if err := log.Append(t.Context(), rec); err != nil {
			t.Fatalf("append %s: %v", rec.ID, err)
		}
	}

	got, err := log.Turn(t.Context(), "turn-1")
	if err != nil {
		t.Fatalf("Turn: %v", err)
	}
	ids := make([]string, 0, len(got))
	for _, r := range got {
		ids = append(ids, r.ID)
	}
	if len(ids) != 2 {
		t.Fatalf("turn events = %v, want the phase AND the delivery", ids)
	}
	// Oldest first: a turn is read forwards.
	if ids[0] != "e-phase" || ids[1] != "e-delivery" {
		t.Errorf("turn events = %v, want [e-phase e-delivery]", ids)
	}
}

// A non-phase row must not acquire a phase's numbers on the way in: the
// identity fallback fills the identifier columns only.
//
// ASSERTED THROUGH A READ THAT SELECTS THE COLUMNS. It used to read
// `Turn(...)[0].Spend`, which a read NEVER populates — `finishRecord` does not
// set that field — so the guard short-circuited on nil and the test could not
// fail whatever the writer did. `Turns` sums `total_tokens` over every row of
// a turn regardless of type, so a delivery that wrongly acquired a phase's
// numbers shows up there and nowhere else.
func TestTheTurnFallbackDoesNotInventSpend(t *testing.T) {
	t.Parallel()
	log := open(t).Events()
	at := time.Now().UTC().Add(-time.Minute)
	// A real phase, so the assertion below is "only this one counted"
	// rather than "nothing counted", which a broken writer also satisfies.
	if err := log.Append(t.Context(), store.EventRecord{
		ID: "e-phase", Type: "agent_phase_completed", Source: "engine",
		Category: "lifecycle", Time: at,
		Tags:    map[string]string{"turn_id": "turn-1", "agent_role": "PM"},
		Payload: []byte(`{"turn_id":"turn-1","total_tokens":10,"model":"real"}`),
	}); err != nil {
		t.Fatalf("append the phase: %v", err)
	}
	if err := log.Append(t.Context(), store.EventRecord{
		ID: "e-1", Type: "message_delivered", Source: "engine", Category: "comms",
		Time: at.Add(time.Second),
		Tags: map[string]string{"turn_id": "turn-1"},
		// A payload that WOULD look like spend if anything read it here.
		Payload: []byte(`{"turn_id":"turn-1","total_tokens":999,"model":"ghost"}`),
	}); err != nil {
		t.Fatalf("append: %v", err)
	}
	rows, err := log.Turns(t.Context(), store.TurnQuery{})
	if err != nil {
		t.Fatalf("Turns: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("%d turn rows, want 1: %+v", len(rows), rows)
	}
	if rows[0].TotalTokens != 10 {
		t.Errorf("the turn totals %d tokens, want the phase's 10 — a delivery "+
			"was credited with a phase's numbers", rows[0].TotalTokens)
	}
	if rows[0].Models != "real" {
		t.Errorf("models = %q, want only the phase's", rows[0].Models)
	}
}
