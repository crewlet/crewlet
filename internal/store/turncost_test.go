package store_test

import (
	"encoding/json"
	"slices"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/store"
)

// A TURN'S COST IS ITS PHASES AND ITS IN-TURN AUXILIARY SPEND, never the
// reflection after it.
//
// Both kinds of auxiliary record carry the turn's id — the turn's page draws
// the reflection's in its Reflection lane — and only the first is what the
// work cost. Summed whole, the list's tokens column and its `sort=-tokens`
// would rank a turn by how much its seat had to remember, and name the
// reflection's model as one the turn ran on.
func TestATurnsCostLeavesItsReflectionOut(t *testing.T) {
	t.Parallel()
	log := open(t).Events()
	base := time.Now().UTC().Add(-time.Hour)
	seedTurn(t, log, "t-1", base, "PM", func(rec *store.EventRecord, _ int) {
		payload, err := json.Marshal(map[string]any{
			"turn_id": "t-1", "model": "claude-opus-5",
			"input_tokens": 100, "output_tokens": 10, "total_tokens": 110,
		})
		if err != nil {
			t.Fatal(err)
		}
		rec.Payload = payload
	})
	for _, aux := range []struct {
		id, model string
		stage     types.AuxStage
		at        time.Duration
		total     int
	}{
		{"in-turn", "claude-haiku-4-5", types.AuxStageTurn, 500 * time.Millisecond, 300},
		{"reflection", "a-reflection-model", types.AuxStageReflection, 10 * time.Second, 5000},
	} {
		payload, err := json.Marshal(types.AuxiliarySpend{
			RoleName: "PM", Stage: aux.stage, Purpose: types.AuxMemoryFilter,
			TurnID: "t-1", Model: aux.model, Calls: 1,
			InputTokens: aux.total, TotalTokens: aux.total,
		})
		if err != nil {
			t.Fatal(err)
		}
		if err := log.Append(t.Context(), store.EventRecord{
			ID: aux.id, Type: types.AuxiliarySpend{}.EventType(), Time: base.Add(aux.at),
			Category: "system", Actor: "PM",
			Tags:    map[string]string{"turn_id": "t-1", "agent_role": "PM"},
			Payload: payload,
		}); err != nil {
			t.Fatalf("append %s: %v", aux.id, err)
		}
	}

	got, err := log.Turns(t.Context(), store.TurnQuery{})
	if err != nil {
		t.Fatalf("Turns: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("turns = %+v, want the one", got)
	}
	one := got[0]
	if one.TotalTokens != 220+300 || one.InputTokens != 200+300 {
		t.Errorf("the turn costs %d (%d in), want 520 (500 in): its two phases and "+
			"its in-turn call, without the 5000 its reflection spent", one.TotalTokens,
			one.InputTokens)
	}
	named := splitModels(one.Models)
	slices.Sort(named)
	if want := []string{"claude-haiku-4-5", "claude-opus-5"}; !slices.Equal(named, want) {
		t.Errorf("models = %q, want exactly %q — the reflection's model is not one the "+
			"turn ran on", named, want)
	}
	if one.Phases != 2 {
		t.Errorf("phases = %d, want 2: an auxiliary record is no phase", one.Phases)
	}

	// AND THE ORDER BY TOKENS IS THE SAME SUM: a second turn that cost more
	// than this one's phases but less than its phases with the reflection
	// added ranks above it.
	seedTurn(t, log, "t-2", base.Add(time.Minute), "PM", func(rec *store.EventRecord, _ int) {
		payload, err := json.Marshal(map[string]any{
			"turn_id": "t-2", "model": "claude-opus-5",
			"input_tokens": 400, "output_tokens": 10, "total_tokens": 410,
		})
		if err != nil {
			t.Fatal(err)
		}
		rec.Payload = payload
	})
	ranked, err := log.Turns(t.Context(), store.TurnQuery{Sort: store.TurnSortTokens})
	if err != nil {
		t.Fatalf("Turns by tokens: %v", err)
	}
	if len(ranked) != 2 || ranked[0].TurnID != "t-2" {
		t.Errorf("by tokens = %v, want t-2 (820) above t-1 (520): the reflection's "+
			"5000 is no part of t-1's cost", turnIDs(ranked))
	}
}

func turnIDs(turns []store.Turn) []string {
	out := make([]string, 0, len(turns))
	for _, t := range turns {
		out = append(out, t.TurnID)
	}
	return out
}
