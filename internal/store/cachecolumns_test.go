package store_test

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/api/livestate"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/store"
	"github.com/crewlet/crewlet/internal/tokens"
)

// cachedPhase is one phase record as the runner publishes it: a cached prefix
// inside its input, and a provider key that is NOT the model it reported.
func cachedPhase(t *testing.T) []byte {
	t.Helper()
	raw, err := json.Marshal(types.AgentPhaseCompleted{
		Agent: "a-1", RoleName: "Lead", TurnID: "run-1", WorkKey: "wk-1",
		Phase: "execute", Iteration: 1,
		Model: "claude-sonnet-5", ProviderKey: "primary",
		InputTokens: 1000, OutputTokens: 200, TotalTokens: 1200,
		CacheReadTokens: 800, CacheWriteTokens: 150,
	})
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

// appendPhase writes one phase completion the way the publish listener does:
// tags and spend derived from the serialized event.
func appendPhase(t *testing.T, log *store.EventLog, id string, at time.Time,
	payload []byte, spend *store.Spend) {

	t.Helper()
	if err := log.Append(t.Context(), store.EventRecord{
		ID: id, Type: "agent_phase_completed", Source: "engine",
		Category: "agent", Time: at,
		Tags:    store.ExtractTags(payload),
		Spend:   spend,
		Payload: payload,
	}); err != nil {
		t.Fatalf("append %s: %v", id, err)
	}
}

func phaseTokens(t *testing.T, log *store.EventLog) []tokens.Record {
	t.Helper()
	got, err := log.PhaseTokens(t.Context(), store.PhaseTokenQuery{SinceDays: 1})
	if err != nil {
		t.Fatalf("phase tokens: %v", err)
	}
	return got
}

// THE CACHE COUNTS AND THE PROVIDER KEY REACH THE ROLLUP FROM THE STORE.
//
// The rollup reads COLUMNS (schema/0015), so a value the writer leaves in the
// payload is a value every stored window reads as zero — which is what the
// cache share did before schema/0032: summed by internal/tokens and filled by
// no producer, so every screen reported a cache that never hit.
func TestAPhasesCacheAndProviderKeyReachTheStoredRollup(t *testing.T) {
	t.Parallel()
	log := open(t).Events()
	payload := cachedPhase(t)
	appendPhase(t, log, "p1", time.Now().UTC().Add(-time.Minute), payload,
		store.SpendFor("agent_phase_completed", payload))

	got := phaseTokens(t, log)
	if len(got) != 1 {
		t.Fatalf("records = %d, want the one appended", len(got))
	}
	rec := got[0]
	if rec.CacheReadTokens != 800 || rec.CacheWriteTokens != 150 {
		t.Errorf("cache = %d read / %d written, want 800 / 150 — the writer "+
			"left the counts in the payload", rec.CacheReadTokens, rec.CacheWriteTokens)
	}
	if rec.ProviderKey != "primary" {
		t.Errorf("provider key = %q, want the entry that served the call", rec.ProviderKey)
	}
	if rec.Model != "claude-sonnet-5" {
		t.Errorf("model = %q: the key must not displace the reported model", rec.Model)
	}
	// And the rollup divides what it was given: the cache is a BREAKDOWN
	// of the input, so the totals are unchanged by it.
	roll := tokens.Aggregate(got, tokens.Options{})
	if roll.Totals.CacheReadTokens != 800 || roll.Totals.InputTokens != 1000 ||
		roll.Totals.TotalTokens != 1200 {
		t.Errorf("totals = %+v, want 800 cached of 1000 input and 1200 total",
			roll.Totals)
	}
}

// THE LIVE WINDOW AND A STORED ONE HAND THE ROLLUP THE SAME RECORD.
//
// internal/tokens folds both, so a field one producer carries and the other
// drops is a rollup whose numbers change when the window crosses the live
// edge — a dashboard that reads a cache share for the last day and none for
// the last week. One serialized phase record goes through both.
func TestTheLiveAndStoredProducersAgreeOnAPhase(t *testing.T) {
	t.Parallel()
	payload := cachedPhase(t)
	at := time.Now().UTC().Add(-time.Minute).Truncate(time.Second)

	log := open(t).Events()
	appendPhase(t, log, "p1", at, payload, store.SpendFor("agent_phase_completed", payload))
	stored := phaseTokens(t, log)
	if len(stored) != 1 {
		t.Fatalf("stored records = %d, want one", len(stored))
	}

	var body map[string]any
	if err := json.Unmarshal(payload, &body); err != nil {
		t.Fatal(err)
	}
	live := livestate.New()
	live.Apply(&livestate.Envelope{ID: "p1", Type: "agent_phase_completed",
		Timestamp: at.Format(time.RFC3339Nano), Category: "agent", Payload: body})
	records := live.SpendRecords()
	if len(records) != 1 {
		t.Fatalf("live records = %d, want one", len(records))
	}

	want, got := stored[0], records[0]
	if want != got {
		t.Errorf("the producers disagree about one phase:\n stored %+v\n   live %+v",
			want, got)
	}
}

// A COLLECTED CODING RUN IS COUNTED WHERE EVERY OTHER PHASE IS.
//
// The run's own record is the only place its tokens reach a reader: the
// executor's resumed record states the executor's rounds, and nothing else
// carries the run's. So the record has to fold through the stored rollup and
// the live one alike, under its own phase and its own model — a record either
// producer dropped would be a coding run's whole spend missing from the screen
// that exists to show spend.
func TestASandboxPhaseIsCountedInTheRollup(t *testing.T) {
	t.Parallel()
	payload, err := json.Marshal(types.AgentPhaseCompleted{
		Agent: "a-1", RoleName: "Lead", TurnID: "run-1", WorkKey: "wk-1",
		Phase: types.PhaseSandbox, Iteration: 2, LaunchID: "job-1",
		Backend: types.BackendSandbox, CodingAgent: "claude-code", Model: "claude-sonnet-5",
		InputTokens: 9000, OutputTokens: 700, TotalTokens: 9700,
		CacheReadTokens: 6000, CacheWriteTokens: 1500, CostUSD: 0.42,
		ActivityTranscript: "[tool] bash: go test ./...",
	})
	if err != nil {
		t.Fatal(err)
	}
	at := time.Now().UTC().Add(-time.Minute).Truncate(time.Second)
	log := open(t).Events()
	appendPhase(t, log, "p1", at, payload, store.SpendFor("agent_phase_completed", payload))

	var body map[string]any
	if err := json.Unmarshal(payload, &body); err != nil {
		t.Fatal(err)
	}
	live := livestate.New()
	live.Apply(&livestate.Envelope{ID: "p1", Type: "agent_phase_completed",
		Timestamp: at.Format(time.RFC3339Nano), Category: "agent", Payload: body})

	for name, records := range map[string][]tokens.Record{
		"stored": phaseTokens(t, log), "live": live.SpendRecords(),
	} {
		roll := tokens.Aggregate(records, tokens.Options{})
		var sandbox *tokens.PhaseRow
		for i := range roll.ByPhase {
			if roll.ByPhase[i].Phase == string(types.PhaseSandbox) {
				sandbox = &roll.ByPhase[i]
			}
		}
		if sandbox == nil {
			t.Errorf("%s: the rollup has no sandbox phase: %+v", name, roll.ByPhase)
			continue
		}
		if sandbox.TotalTokens != 9700 || sandbox.CacheReadTokens != 6000 || sandbox.CostUSD != 0.42 {
			t.Errorf("%s: the sandbox phase holds %+v, want the run's 9700 tokens, 6000 cached, $0.42",
				name, sandbox.Bucket)
		}
		if len(roll.ByModel) != 1 || roll.ByModel[0].Model != "claude-sonnet-5" {
			t.Errorf("%s: models = %+v, want the run's own", name, roll.ByModel)
		}
	}
}
