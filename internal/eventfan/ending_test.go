package eventfan_test

import (
	"encoding/json"
	"fmt"
	"slices"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/queue/memory"
	"github.com/crewlet/crewlet/internal/store"
)

// A LONG TURN'S RECOVERED ENDING HOLDS ITS REVIEW PHASE, behind the busiest
// tail a turn with learning enabled leaves.
//
// A turn past the read's cap loses its ending, and what is recovered beside
// its opening is its last [eventfan.TurnClosingEvents] rows. The review phase
// is the record a reader who came for "how did it end" wants, and everything
// stamped after it sits between it and the end: its card's rewrite, the stop's
// record, the two completions, a colleague's answer, a reflection pass over a
// trigger three people spoke in, what that pass cost on two models, and the
// conversation entry's two rewrites — twenty-two rows. Twenty recovered rows,
// this bound's old value, dropped the review for every one of them.
func TestALongTurnsRecoveredEndingHoldsItsReview(t *testing.T) {
	t.Parallel()
	broker := memory.NewBroker()
	a := newNode(t, broker, "node-a")
	base := time.Now().UTC().Add(-2 * time.Hour)
	row := func(i int, id, typ string) {
		appendTo(t, a, store.EventRecord{
			ID: id, Type: typ, Category: "task", Time: base.Add(time.Duration(i) * time.Second),
			Tags:    map[string]string{"turn_id": "long", "agent_role": "Lead"},
			Payload: json.RawMessage(`{"turn_id":"long"}`),
		})
	}
	opening := store.MaxTurnEvents + 40
	for i := range opening {
		row(i, fmt.Sprintf("open-%04d", i), "agent_phase_completed")
	}
	row(opening, "review", "agent_phase_completed")
	ending := []string{
		"auxiliary_spend",                        // the card's rewrite
		"turn.guard_breach",                      // the stop
		"agent_turn_completed", "turn_completed", // the completions
		"a2a_message",                         // the colleague's answer
		"episode_recorded", "persist_decided", // the reflection pass
		"counterparty_profiled", "counterparty_profiled", "counterparty_profiled",
		"skill_synthesized", "skill_refined", "skill_promoted",
		"reflection_completed",
		// What the pass cost: four purposes on one model, two on another.
		"auxiliary_spend", "auxiliary_spend", "auxiliary_spend", "auxiliary_spend",
		"auxiliary_spend", "auxiliary_spend",
		// The conversation entry's rewrites: an argument and an error.
		"auxiliary_spend", "auxiliary_spend",
	}
	for n, typ := range ending {
		row(opening+1+n, fmt.Sprintf("end-%02d", n), typ)
	}
	if len(ending) < 20 {
		t.Fatalf("the ending is %d rows behind the review, which the old bound already "+
			"held — the case exercises nothing", len(ending))
	}

	got, _, err := fanFrom(a, "node-a").Turn(t.Context(), "long")
	if err != nil {
		t.Fatalf("Turn: %v", err)
	}
	ids := make([]string, 0, len(got.Rows))
	for _, r := range got.Rows {
		ids = append(ids, r.ID)
	}
	if !slices.Contains(ids, "review") {
		t.Fatalf("the recovered ending is %v, without the review phase", ids[store.MaxTurnEvents:])
	}
	if got.Total != opening+1+len(ending) {
		t.Errorf("total = %d, want every row the turn holds", got.Total)
	}
}
