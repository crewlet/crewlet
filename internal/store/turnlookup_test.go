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
// fallback fills the identifier only.
func TestTheTurnFallbackDoesNotInventSpend(t *testing.T) {
	t.Parallel()
	log := open(t).Events()
	if err := log.Append(t.Context(), store.EventRecord{
		ID: "e-1", Type: "message_delivered", Source: "engine", Category: "comms",
		Time: time.Now().UTC().Add(-time.Minute),
		Tags: map[string]string{"turn_id": "turn-1"},
		// A payload that WOULD look like spend if anything read it here.
		Payload: []byte(`{"turn_id":"turn-1","total_tokens":999,"model":"ghost"}`),
	}); err != nil {
		t.Fatalf("append: %v", err)
	}
	got, err := log.Turn(t.Context(), "turn-1")
	if err != nil || len(got) != 1 {
		t.Fatalf("Turn = %v, %v", got, err)
	}
	if got[0].Spend != nil && got[0].Spend.TotalTokens != 0 {
		t.Errorf("a delivery was credited with %d tokens", got[0].Spend.TotalTokens)
	}
}
