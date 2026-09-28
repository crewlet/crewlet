package observe_test

import (
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/api/livestate"
	"github.com/crewlet/crewlet/internal/events"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/observe"
	"github.com/crewlet/crewlet/internal/store"
	"github.com/crewlet/crewlet/internal/tokens"
)

// THE STORE AND THE LIVE PROJECTION READ ONE SPEND EVENT AS ONE RECORD.
//
// The two are the producers every spend view folds from — a window the live
// projection holds is answered from it, any other from the store's columns,
// and a restart seeds the one from the other — so a record they read two ways
// is a figure that moves between rows when the screen refreshes. They are fed
// here exactly as production feeds them: the publish listener's row
// ([observe.Record]) appended and read back through the token query, and the
// projector's envelope ([observe.Envelope]) folded into the projection.
//
// Each event is one the rule has something to decide about: a phase nested
// under another with its worker, iteration and price, and a coding agent it
// must NOT be read as; a phase that names no model, which stands under its
// provider slot; and a coding run's usage record, which stands under execute
// and its coding agent.
func TestBothProducersReadASpendEventAsOneRecord(t *testing.T) {
	t.Parallel()
	db := openStore(t)
	log := db.Events()
	live := livestate.New()
	at := time.Now().UTC().Add(-time.Hour).Truncate(time.Millisecond)

	nested := events.New(types.AgentPhaseCompleted{
		Agent: "a-1", RoleName: "Lead", TurnID: "turn-1", WorkKey: "wk-1",
		Iteration: 2, Phase: types.PhaseAuxiliary, HostPhase: types.PhaseExecute,
		Worker: "reflect", Model: "claude-sonnet-5", ProviderKey: "primary",
		CodingAgent: "claude-code", CostUSD: 0.25,
		InputTokens: 100, OutputTokens: 20, TotalTokens: 120,
	}, events.TraceContext{})
	unnamed := events.New(types.AgentPhaseCompleted{
		Agent: "a-1", RoleName: "Lead", TurnID: "turn-1", Phase: types.PhaseReview,
		ProviderKey: "fallback", InputTokens: 7, OutputTokens: 2, TotalTokens: 9,
	}, events.TraceContext{})
	run := events.New(types.SandboxRunUsage{
		Agent: "a-1", RoleName: "Lead", TurnID: "turn-1", LaunchID: "launch-1",
		WorkKey: "wk-1", SandboxID: "sb-1", CodingAgent: "claude-code",
		InputTokens: 5000, OutputTokens: 700, TotalTokens: 5700, CostUSD: 1.5,
	}, events.TraceContext{})

	for i, ev := range []*events.Event{nested, unnamed, run} {
		ev.Timestamp = at.Add(time.Duration(i) * time.Second)
		row, ok := observe.Record(ev)
		if !ok {
			t.Fatalf("a %s did not render as a store row", ev.Type)
		}
		if err := log.Append(t.Context(), row); err != nil {
			t.Fatalf("append a %s: %v", ev.Type, err)
		}
		env, ok := observe.Envelope(ev)
		if !ok {
			t.Fatalf("a %s did not render as an envelope", ev.Type)
		}
		live.Apply(&env)
	}

	stored, err := log.PhaseTokens(t.Context(), store.PhaseTokenQuery{
		Since: at.Add(-time.Minute), Until: at.Add(time.Minute),
	})
	if err != nil {
		t.Fatalf("read the stored records: %v", err)
	}
	streamed := live.SpendRecords()
	if len(stored) != 3 || len(streamed) != 3 {
		t.Fatalf("the store holds %d records and the projection %d, want the "+
			"three spend events in each", len(stored), len(streamed))
	}
	byID := map[string]tokens.Record{}
	for _, r := range streamed {
		byID[r.EventID] = r
	}
	for _, s := range stored {
		l, held := byID[s.EventID]
		if !held {
			t.Errorf("the projection holds no record %s, which the store does", s.EventID)
			continue
		}
		// THE STAMP IS THE ONE FIELD WRITTEN TWO WAYS ON PURPOSE: the
		// store formats the instant it keyed the row on, the projection
		// carries the envelope's text. Both name the same instant.
		sAt, err := time.Parse(time.RFC3339Nano, s.Timestamp)
		if err != nil {
			t.Fatalf("the store's stamp %q: %v", s.Timestamp, err)
		}
		lAt, err := time.Parse(time.RFC3339Nano, l.Timestamp)
		if err != nil {
			t.Fatalf("the projection's stamp %q: %v", l.Timestamp, err)
		}
		if !sAt.Equal(lAt) {
			t.Errorf("record %s: the store stamps %v and the projection %v",
				s.EventID, sAt, lAt)
		}
		s.Timestamp, l.Timestamp = "", ""
		if s != l {
			t.Errorf("record %s is read two ways:\n  store      %+v\n  projection %+v",
				s.EventID, s, l)
		}
	}

	// AND WHAT THE RULE DECIDED, named, so a failure above has a premise to
	// read against: the run under execute and its agent, the unnamed phase
	// under its slot, the nested one under its own model and not the agent.
	for id, want := range map[string]struct{ phase, model string }{
		run.ID.String():     {tokens.RunPhase, "claude-code"},
		unnamed.ID.String(): {string(types.PhaseReview), "fallback"},
		nested.ID.String():  {string(types.PhaseAuxiliary), "claude-sonnet-5"},
	} {
		if got := byID[id]; got.Phase != want.phase || got.Model != want.model {
			t.Errorf("record %s stands under %s/%s, want %s/%s",
				id, got.Phase, got.Model, want.phase, want.model)
		}
	}
}
