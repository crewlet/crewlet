package observe_test

import (
	"testing"

	"github.com/crewlet/crewlet/internal/events"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/observe"
)

// THE SPEND COLUMNS ARE FILLED BY THE PRODUCTION PATH.
//
// observe.Record is the only production producer of a store.EventRecord — the
// wiring is observe.NewWriter — and it left Spend nil, so the store's
// "derive it when the caller did not" fallback was the only branch ever
// taken. That fallback re-decoded the engine's largest payload, a phase
// completion carrying the whole prompt and tool log, on the publishing
// goroutine of every LLM call.
func TestAPhaseCompletionCarriesItsSpend(t *testing.T) {
	t.Parallel()
	ev := events.New(types.AgentPhaseCompleted{
		Phase:        "execute",
		Model:        "claude-sonnet-5",
		TurnID:       "turn-1",
		Iteration:    2,
		InputTokens:  1200,
		OutputTokens: 340,
		TotalTokens:  1540,
	}, events.TraceContext{})

	rec, ok := observe.Record(ev)
	if !ok {
		t.Fatal("a phase completion is not persisted")
	}
	if rec.Spend == nil {
		t.Fatal("the record carries no spend, so the store must re-decode the " +
			"whole payload to recover it")
	}
	if rec.Spend.Model != "claude-sonnet-5" || rec.Spend.TurnID != "turn-1" {
		t.Errorf("spend identity = %+v", *rec.Spend)
	}
	if rec.Spend.InputTokens != 1200 || rec.Spend.OutputTokens != 340 ||
		rec.Spend.TotalTokens != 1540 {
		t.Errorf("spend tokens = %+v, want the event's own counts", *rec.Spend)
	}
}

// AND AN EVENT THAT IS NOT A PHASE COMPLETION CARRIES NONE, so the columns
// stay at their defaults rather than being filled with zeroes that look like
// a measured zero.
func TestANonPhaseEventCarriesNoSpend(t *testing.T) {
	t.Parallel()
	ev := events.New(types.AgentPhaseStarted{Phase: "plan", TurnID: "turn-1"},
		events.TraceContext{})
	rec, ok := observe.Record(ev)
	if !ok {
		t.Fatal("a phase start is not persisted")
	}
	if rec.Spend != nil {
		t.Errorf("a non-phase-completion event carried spend: %+v", *rec.Spend)
	}
}
