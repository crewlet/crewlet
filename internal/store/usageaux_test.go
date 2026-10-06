package store_test

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/store"
)

// A SEAT'S DAY IS EVERY SPEND IT MADE, the auxiliary model's included, and its
// calls are PROVIDER calls.
//
// The usage day was folded from phase records alone, so the replicated history
// every named spend window reads understated each seat by its prefetch, its
// compactions and its reflection — spend the counters had charged. An
// auxiliary record is a cell of the shape every build already reads (phase
// `auxiliary`, its purpose as the worker), its calls are the ones it
// coalesced, and landing one moves the day's fingerprint so the publisher
// re-derives it.
func TestAnAuxiliaryRecordIsPartOfItsSeatsDay(t *testing.T) {
	t.Parallel()
	db := open(t)
	log := db.Events()
	day := time.Now().UTC().Truncate(24 * time.Hour)
	w := store.UsageWindow{Start: day, End: day.Add(24 * time.Hour)}

	phase, err := json.Marshal(map[string]any{
		"agent_id": "seat-1", "role": "Dev", "phase": "execute", "model": "opus",
		"rounds": []any{map[string]any{}, map[string]any{}}, "input_tokens": 100,
		"output_tokens": 10, "total_tokens": 110,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := log.Append(t.Context(), store.EventRecord{
		ID: "phase", Type: "agent_phase_completed", Time: day.Add(time.Hour),
		Category: "system", Tags: map[string]string{"agent_id": "seat-1", "agent_role": "Dev"},
		Payload: phase,
	}); err != nil {
		t.Fatal(err)
	}
	before, err := db.UsageMark(t.Context(), w)
	if err != nil {
		t.Fatal(err)
	}

	aux, err := json.Marshal(types.AuxiliarySpend{
		Agent: "seat-1", RoleName: "Dev", Stage: types.AuxStageReflection,
		Purpose: types.AuxPersistDecider, Model: "haiku", ProviderKey: "cheap",
		Calls: 3, InputTokens: 900, OutputTokens: 30, TotalTokens: 930,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := log.Append(t.Context(), store.EventRecord{
		// STAMPED BEFORE THE PHASE, as a late flush is — its last call's
		// instant, not the flush's — so a newest-instant fingerprint alone
		// would not see it.
		ID: "aux", Type: types.AuxiliarySpend{}.EventType(), Time: day.Add(30 * time.Minute),
		Category: "system", Tags: map[string]string{"agent_id": "seat-1", "agent_role": "Dev"},
		Payload: aux,
	}); err != nil {
		t.Fatal(err)
	}
	after, err := db.UsageMark(t.Context(), w)
	if err != nil {
		t.Fatal(err)
	}
	if after == before {
		t.Error("the day's fingerprint did not move when its auxiliary record landed, " +
			"so the publisher would never re-derive the day")
	}

	got, err := db.UsageForDay(t.Context(), w)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Seats) != 1 {
		t.Fatalf("seats = %+v, want seat-1", got.Seats)
	}
	cells := map[string]store.UsageTokens{}
	for _, c := range got.Seats[0].Tokens {
		cells[c.Phase+"/"+c.Worker] = c
	}
	if c := cells["execute/"]; c.Total != 110 || c.Calls != 2 {
		t.Errorf("the phase's cell = %+v, want 110 tokens over its 2 rounds", c)
	}
	if c := cells["auxiliary/persist_decider"]; c.Total != 930 || c.Calls != 3 ||
		c.Model != "haiku" || c.ProviderKey != "cheap" {
		t.Errorf("the auxiliary cell = %+v, want 930 tokens over its 3 coalesced calls "+
			"on haiku via cheap", c)
	}
}
