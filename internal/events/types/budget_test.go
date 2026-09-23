package types

import (
	"encoding/json"
	"strings"
	"testing"
)

// A BUDGET REFUSAL CARRIES THE WINDOW IT REFUSED IN, AND ONE FROM A BUILD THAT
// COUNTED NO WINDOW STILL DECODES.
//
// The window is what makes the record actionable — a day that resets tonight
// and a month that resets in three weeks are different decisions about raising
// a ceiling — so it has to survive the envelope both ways. And a rolling
// upgrade puts records from the lifetime-counter build on the same stream: one
// of those must decode with the window absent rather than failing, and must
// re-encode without inventing empty keys a reader would take for a window with
// no name.
func TestABudgetRefusalRoundTripsWithAndWithoutItsWindow(t *testing.T) {
	t.Parallel()
	refused := BudgetExhausted{
		Agent: "a-1", RoleName: "Lead", TurnID: "t-1", WorkKey: "wk-1",
		BudgetType: BudgetScopeOrg, UsedTokens: 99_000, MaxTokens: 100_000,
		Period: "day", Window: "2026-09-23", ResetsAt: "2026-09-24T07:00:00Z",
	}
	raw, err := json.Marshal(refused)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var back BudgetExhausted
	if err := json.Unmarshal(raw, &back); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if back != refused {
		t.Fatalf("round trip = %+v, want %+v", back, refused)
	}

	older := []byte(`{"agent_id":"a-1","role":"Lead","budget_type":"agent",` +
		`"used_tokens":10,"max_tokens":10}`)
	var fromOlder BudgetExhausted
	if err := json.Unmarshal(older, &fromOlder); err != nil {
		t.Fatalf("a record with no window did not decode: %v", err)
	}
	if fromOlder.Period != "" || fromOlder.Window != "" || fromOlder.ResetsAt != "" {
		t.Fatalf("a record with no window decoded one: %+v", fromOlder)
	}
	again, err := json.Marshal(fromOlder)
	if err != nil {
		t.Fatalf("re-marshal: %v", err)
	}
	for _, key := range []string{`"period"`, `"window"`, `"resets_at"`} {
		if strings.Contains(string(again), key) {
			t.Errorf("re-encoding a record with no window wrote %s: %s", key, again)
		}
	}
}
