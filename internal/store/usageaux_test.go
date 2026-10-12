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
// auxiliary record is a cell of the shape a phase's is (phase `auxiliary`, its
// purpose as the worker), its calls are the ones it coalesced, and landing one
// moves the day's fingerprint so the publisher re-derives it.
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

// A PERSON'S SPEND IS THEIR OWN DAY, NEVER A SEAT'S. A question answered on
// the operator surface is spent for the person whose credential asked it: the
// record names no agent, so it is no seat's cell, and it names the person as
// its envelope's actor. Folded with the seats it would vanish (no agent id to
// file it under) — which is how a person's questions were missing from every
// named spend window — so the day carries it as the person's.
func TestAPersonsAuxiliarySpendIsTheirDayNotASeats(t *testing.T) {
	t.Parallel()
	db := open(t)
	log := db.Events()
	day := time.Now().UTC().Truncate(24 * time.Hour)
	w := store.UsageWindow{Start: day, End: day.Add(24 * time.Hour)}
	for i, rec := range []types.AuxiliarySpend{
		{ActorSeat: "maya", ActorRole: "Founder", Stage: types.AuxStageOperator,
			Purpose: types.AuxAnswerKnowledge, Model: "haiku", ProviderKey: "cheap",
			Calls: 1, InputTokens: 400, OutputTokens: 20, TotalTokens: 420},
		{ActorSeat: "maya", ActorRole: "Founder", Stage: types.AuxStageOperator,
			Purpose: types.AuxAnswerKnowledge, Model: "haiku", ProviderKey: "cheap",
			Calls: 2, InputTokens: 100, OutputTokens: 10, TotalTokens: 110},
		{Agent: "seat-1", RoleName: "Dev", Stage: types.AuxStageTurn,
			Purpose: types.AuxMemoryFilter, Model: "haiku", ProviderKey: "cheap",
			Calls: 1, InputTokens: 50, OutputTokens: 5, TotalTokens: 55},
	} {
		payload, err := json.Marshal(rec)
		if err != nil {
			t.Fatal(err)
		}
		if err := log.Append(t.Context(), store.EventRecord{
			ID: "aux" + string(rune('a'+i)), Type: rec.EventType(), Actor: rec.Actor(),
			Time: day.Add(time.Duration(i+1) * time.Hour), Category: "system",
			Tags: store.ExtractTags(payload), Payload: payload,
		}); err != nil {
			t.Fatal(err)
		}
	}
	got, err := db.UsageForDay(t.Context(), w)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Seats) != 1 || got.Seats[0].AgentID != "seat-1" {
		t.Fatalf("seats = %+v, want the one seat's day alone", got.Seats)
	}
	if len(got.People) != 1 {
		t.Fatalf("people = %+v, want maya's day", got.People)
	}
	maya := got.People[0]
	if maya.Handle != "maya" || maya.Role != "Founder" || len(maya.Tokens) != 1 {
		t.Fatalf("maya = %+v, want her one cell under her seat's role", maya)
	}
	cell := maya.Tokens[0]
	if cell.Phase != "auxiliary" || cell.Worker != string(types.AuxAnswerKnowledge) ||
		cell.Total != 530 || cell.Calls != 3 || cell.Input != 500 {
		t.Fatalf("maya's cell = %+v, want both questions' 530 tokens over 3 calls", cell)
	}
}
