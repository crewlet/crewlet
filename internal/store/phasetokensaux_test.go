package store_test

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/store"
	"github.com/crewlet/crewlet/internal/tokens"
)

// THE LIVE WINDOW'S SEED READS EVERY SPEND RECORD, the auxiliary ones with the
// stage and the calls that decide a turn's cost and a bucket's call count —
// and names a PERSON's by the role of their seat, since a person is no agent
// role. Read short, a restarted node's rollup would drop the auxiliary model
// from the live window that the stream then puts back record by record.
func TestThePhaseTokenReadCarriesAuxiliarySpend(t *testing.T) {
	t.Parallel()
	log := open(t).Events()
	at := time.Now().UTC().Add(-time.Hour)
	for i, rec := range []types.AuxiliarySpend{
		{Agent: "seat-1", RoleName: "Dev", Stage: types.AuxStageTurn,
			Purpose: types.AuxMemoryFilter, TurnID: "t-1", Model: "haiku", Calls: 2, TotalTokens: 40},
		{ActorSeat: "maya", ActorRole: "Founder", Stage: types.AuxStageOperator,
			Purpose: types.AuxAnswerKnowledge, Model: "haiku", Calls: 1, TotalTokens: 70},
	} {
		payload, err := json.Marshal(rec)
		if err != nil {
			t.Fatal(err)
		}
		tags := map[string]string{"turn_id": rec.TurnID}
		if rec.Agent != "" {
			tags["agent_id"], tags["agent_role"] = rec.Agent, rec.RoleName
		}
		if err := log.Append(t.Context(), store.EventRecord{
			ID: rec.ActorSeat + rec.Agent, Type: rec.EventType(),
			Time: at.Add(time.Duration(i) * time.Minute), Category: "system",
			Tags: tags, Payload: payload,
		}); err != nil {
			t.Fatal(err)
		}
	}
	got, err := log.PhaseTokens(t.Context(), store.PhaseTokenQuery{Since: at.Add(-time.Minute)})
	if err != nil {
		t.Fatalf("PhaseTokens: %v", err)
	}
	byID := map[string]tokens.Record{}
	for _, r := range got {
		byID[r.EventID] = r
	}
	if r := byID["seat-1"]; r.Phase != tokens.PhaseAuxiliary || r.Worker != "memory_filter" ||
		r.Stage != tokens.StageTurn || r.Calls != 2 || r.AgentRole != "Dev" || r.TurnID != "t-1" {
		t.Errorf("the seat's record = %+v", r)
	}
	if r := byID["maya"]; r.AgentRole != "Founder" || r.Stage != "operator" || r.Calls != 1 ||
		r.TotalTokens != 70 {
		t.Errorf("the person's record = %+v, want it named by their seat's role", r)
	}
}
