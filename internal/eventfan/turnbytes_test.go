package eventfan_test

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/eventfan"
	"github.com/crewlet/crewlet/internal/queue/memory"
	"github.com/crewlet/crewlet/internal/store"
)

// A PEER'S TURN ANSWER READS WITHIN ITS BOUND: a turn whose rows pass
// [eventfan.AnswerBytes] well short of the count cap is read to the bound,
// not whole, and answered with its opening, its ending (the newest row
// among it) and its whole count, the rows within the bound and one row.
//
// Mutations, each red: read the turn whole (the answer holds past the bound);
// ask the count only at the count cap (the ending and the total are lost).
func TestAPeersTurnAnswerReadsWithinItsBound(t *testing.T) {
	t.Parallel()
	broker := memory.NewBroker()
	a, b := newNode(t, broker, "node-a"), newNode(t, broker, "node-b")
	base := time.Now().UTC().Add(-time.Hour)
	const rows = 40
	for i := range rows {
		appendTo(t, b, store.EventRecord{
			ID: fmt.Sprintf("r-%02d", i), Type: "agent_turn_progress", Category: "task",
			Time: base.Add(time.Duration(i) * time.Second), Tags: map[string]string{"turn_id": "big"},
			Payload: json.RawMessage(`{"turn_id":"big","text":"` + strings.Repeat("x", 300<<10) + `"}`),
		})
	}
	if rows*300<<10 < 2*eventfan.AnswerBytes || rows >= store.MaxTurnEvents {
		t.Fatalf("the premise: the turn holds no more than twice the bound, or reaches the count cap")
	}
	got, coverage, err := fanFrom(a, "node-a", "node-b").Turn(t.Context(), "big")
	if err != nil || !coverage.Complete {
		t.Fatalf("Turn = %v, coverage %+v; want node-b's answer", err, coverage)
	}
	ids := make([]string, 0, len(got.Rows))
	held := 0
	for _, r := range got.Rows {
		ids = append(ids, r.ID)
		held += store.RecordBytes(r)
	}
	if !slices.Contains(ids, "r-00") || !slices.Contains(ids, fmt.Sprintf("r-%02d", rows-1)) || got.Total != rows {
		t.Errorf("the turn answered %v of %d rows, want its opening, its newest row and the count %d", ids,
			got.Total, rows)
	}
	if limit := eventfan.AnswerBytes + 400<<10; held > limit {
		t.Errorf("the turn's rows hold %d bytes, past the bound and one row, %d", held, limit)
	}
}
