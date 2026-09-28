package tokens

import "testing"

// fields is a payload as [Fields], typed as a test states it.
type fields map[string]any

func (f fields) String(field string) string { s, _ := f[field].(string); return s }
func (f fields) Int(field string) int       { n, _ := f[field].(int); return n }
func (f fields) Float(field string) float64 { v, _ := f[field].(float64); return v }

// A SPEND EVENT BECOMES ONE RECORD BY ONE RULE, and nothing else becomes one.
//
// Both producers call this — the event store as it fills its columns, the live
// projection as it folds the stream — so what it answers is where a phase's or
// a run's spend stands in every view: a phase under its phase and the model
// that answered it, or the provider slot where it names no model; a coding run
// under execute and its coding agent, since the box reports no model and the
// engine's own model did not spend it; and every other event under nothing,
// since several carry a model, a turn or a token count without being spend.
func TestASpendEventBecomesOneRecordByOneRule(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name    string
		typ     string
		payload fields
		want    Record
		spend   bool
	}{
		{
			name: "a phase under its model",
			typ:  PhaseCompletedEvent,
			payload: fields{
				"phase": "auxiliary", "host_phase": "execute", "worker": "reflect",
				"model": "claude-sonnet-5", "provider_key": "primary",
				"turn_id": "turn-1", "work_key": "wk-1", "iteration": 2,
				"input_tokens": 100, "output_tokens": 20, "total_tokens": 120,
				"cost_usd": 0.25,
			},
			want: Record{
				Phase: "auxiliary", HostPhase: "execute", Worker: "reflect",
				Model: "claude-sonnet-5", TurnID: "turn-1", WorkKey: "wk-1",
				Iteration: 2, InputTokens: 100, OutputTokens: 20, TotalTokens: 120,
				CostUSD: 0.25,
			},
			spend: true,
		},
		{
			name:    "a phase that names no model, under its provider slot",
			typ:     PhaseCompletedEvent,
			payload: fields{"phase": "review", "provider_key": "fallback", "total_tokens": 9},
			want:    Record{Phase: "review", Model: "fallback", TotalTokens: 9},
			spend:   true,
		},
		{
			name: "a coding run under execute and its coding agent",
			typ:  RunUsageEvent,
			payload: fields{
				// A PHASE, A MODEL AND AN ITERATION a run's record does
				// not carry — and would not be read from if it did.
				"phase": "review", "model": "claude-haiku-5", "iteration": 3,
				"host_phase": "execute", "worker": "w",
				"coding_agent": "claude-code", "turn_id": "turn-2",
				"launch_id": "launch-1", "work_key": "wk-2",
				"input_tokens": 5000, "output_tokens": 700, "total_tokens": 5700,
				"cost_usd": 1.5,
			},
			want: Record{
				Phase: RunPhase, Model: "claude-code", TurnID: "turn-2",
				WorkKey: "wk-2", InputTokens: 5000, OutputTokens: 700,
				TotalTokens: 5700, CostUSD: 1.5,
			},
			spend: true,
		},
		{
			name:    "a turn's completion, which sums its phases",
			typ:     "agent_turn_completed",
			payload: fields{"model": "claude-sonnet-5", "turn_id": "turn-1", "total_tokens": 120},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, spend := Spent(tc.typ, tc.payload)
			if spend != tc.spend || spend != IsSpendEvent(tc.typ) {
				t.Fatalf("%s: Spent says %v and IsSpendEvent %v, want %v",
					tc.typ, spend, IsSpendEvent(tc.typ), tc.spend)
			}
			if got != tc.want {
				t.Errorf("%s read as\n  %+v\nwant\n  %+v", tc.typ, got, tc.want)
			}
		})
	}
}
