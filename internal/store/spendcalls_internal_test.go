package store

import "testing"

// A SPEND ROW COUNTS ITS PROVIDER CALLS: a phase its rounds where it lists
// them, its rounds_used where it does not (an agent-mode executor, whose
// rounds are the CLI's), and one where neither says — and an auxiliary record
// counts the calls it coalesced, filed as the auxiliary phase with its purpose
// as the worker and its stage beside it.
func TestASpendRowCountsItsProviderCalls(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, eventType, payload string
		want                     Spend
	}{{
		name: "a phase's rounds", eventType: "agent_phase_completed",
		payload: `{"phase":"execute","rounds":[{},{},{}],"rounds_used":9,"total_tokens":10}`,
		want:    Spend{Phase: "execute", TotalTokens: 10, Calls: 3},
	}, {
		name: "an agent-mode phase's rounds_used", eventType: "agent_phase_completed",
		payload: `{"phase":"execute","rounds_used":5}`,
		want:    Spend{Phase: "execute", Calls: 5},
	}, {
		name: "a judge's single call", eventType: "agent_phase_completed",
		payload: `{"phase":"judge","host_phase":"execute"}`,
		want:    Spend{Phase: "judge", HostPhase: "execute", Calls: 1},
	}, {
		name: "an auxiliary record", eventType: "auxiliary_spend",
		payload: `{"stage":"reflection","purpose":"persist_decider","calls":7,` +
			`"turn_id":"t-1","model":"haiku","provider_key":"cheap","total_tokens":900,` +
			`"input_tokens":800,"output_tokens":100,"cache_read_tokens":300}`,
		want: Spend{Phase: "auxiliary", Worker: "persist_decider", Stage: "reflection",
			TurnID: "t-1", Model: "haiku", ProviderKey: "cheap", TotalTokens: 900,
			InputTokens: 800, OutputTokens: 100, CacheReadTokens: 300, Calls: 7},
	}} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := SpendFor(tc.eventType, []byte(tc.payload))
			if got == nil || *got != tc.want {
				t.Errorf("spend = %+v, want %+v", got, tc.want)
			}
		})
	}
	if got := SpendFor("turn_completed", []byte(`{"total_tokens":5}`)); got != nil {
		t.Errorf("a record that is no call has spend %+v", got)
	}
}
