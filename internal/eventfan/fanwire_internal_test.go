package eventfan

import (
	"encoding/json"
	"path/filepath"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/store"
)

// A PEER MEASURES A WINDOW FROM THE ASKER'S INSTANT, never its own clock.
//
// The store refuses a windowed read with no instant, so a peer has to hand it
// one; reading its own "now" there would be the second evaluation of the clock
// the asker's label was not taken from. Here the asker's instant is two hours
// back and the only record an hour back: measured from the asker, the window
// ends before the record; measured from the peer's clock, it would hold it.
//
// Mutation: answer from askedAt(time.Time{}) instead of the request's `at`,
// and the record is counted.
func TestAPeerMeasuresTheWindowFromTheAskersInstant(t *testing.T) {
	t.Parallel()
	db, err := store.Open(t.Context(), filepath.Join(t.TempDir(), "peer.db"), store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	now := time.Now().UTC()
	if err := db.Events().Append(t.Context(), store.EventRecord{
		ID: "an-hour-ago", Type: "agent_phase_completed", Category: "task",
		Time: now.Add(-time.Hour), Tags: map[string]string{"turn_id": "t-1"},
		Payload: json.RawMessage(`{}`),
		Spend:   &store.Spend{Phase: "execute", TurnID: "t-1", TotalTokens: 5},
	}); err != nil {
		t.Fatal(err)
	}
	ask := func(at time.Time) int {
		t.Helper()
		params, err := json.Marshal(phaseTokenParams{At: at})
		if err != nil {
			t.Fatal(err)
		}
		part, err := answer(t.Context(), db.Events(), QuestionPhaseTokens, params, nil)
		if err != nil {
			t.Fatal(err)
		}
		return len(part.(spendPart).Records)
	}
	if got := ask(now.Add(-2 * time.Hour)); got != 0 {
		t.Errorf("a window measured from two hours ago held %d records, want none — "+
			"the peer read its own clock rather than the asker's", got)
	}
	if got := ask(now); got != 1 {
		t.Errorf("a window measured from now held %d records, want the one", got)
	}
}
