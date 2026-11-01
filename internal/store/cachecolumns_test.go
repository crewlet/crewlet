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
		// THREE PROVIDER CALLS, so the producers' call counts are a
		// number worth agreeing on rather than the one either would
		// default to.
		Rounds:     []types.PhaseRound{{Round: 1}, {Round: 2}, {Round: 3}},
		RoundsUsed: 3,
	})
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

// THE LIVE WINDOW AND A STORED ONE HAND THE ROLLUP THE SAME AUXILIARY RECORD:
// the phase it is filed under, its purpose as the worker, its stage, its
// coalesced calls — and, for a person's, the role of their seat. One record
// through both producers, for the reason the phase case gives.
func TestTheLiveAndStoredProducersAgreeOnAnAuxiliaryRecord(t *testing.T) {
	t.Parallel()
	at := time.Now().UTC().Add(-time.Minute).Truncate(time.Second)
	for name, rec := range map[string]types.AuxiliarySpend{
		"a seat's": {Agent: "a-1", AgentHandle: "lead", RoleName: "Lead",
			Stage: types.AuxStageReflection, Purpose: types.AuxSkillRefiner,
			TurnID: "run-1", WorkKey: "wk-1", Model: "haiku", ProviderKey: "cheap",
			Calls: 4, InputTokens: 900, OutputTokens: 40, TotalTokens: 940,
			CacheReadTokens: 300},
		"a person's": {ActorSeat: "maya", ActorRole: "Founder",
			Stage: types.AuxStageOperator, Purpose: types.AuxAnswerKnowledge,
			Model: "haiku", ProviderKey: "cheap", Calls: 1, TotalTokens: 70},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			payload, err := json.Marshal(rec)
			if err != nil {
				t.Fatal(err)
			}
			log := open(t).Events()
			if err := log.Append(t.Context(), store.EventRecord{
				ID: "x1", Type: rec.EventType(), Source: "engine", Category: "system",
				// THE ENVELOPE'S ACTOR, as the publish listener writes it:
				// the person a person's record was spent for.
				Actor: rec.Actor(),
				Time:  at, Tags: store.ExtractTags(payload),
				Spend: store.SpendFor(rec.EventType(), payload), Payload: payload,
			}); err != nil {
				t.Fatal(err)
			}
			stored := phaseTokens(t, log)
			var body map[string]any
			if err := json.Unmarshal(payload, &body); err != nil {
				t.Fatal(err)
			}
			live := livestate.New()
			live.Apply(&livestate.Envelope{ID: "x1", Type: rec.EventType(),
				Timestamp: at.Format(time.RFC3339Nano), Category: "system", Payload: body})
			records := live.SpendRecords()
			if len(stored) != 1 || len(records) != 1 {
				t.Fatalf("stored %d, live %d records, want one each", len(stored), len(records))
			}
			if stored[0] != records[0] {
				t.Errorf("the producers disagree about one auxiliary record:\n stored %+v\n   live %+v",
					stored[0], records[0])
			}
			if stored[0].Calls != rec.Calls || stored[0].Phase != "auxiliary" {
				t.Errorf("the record is %+v, want the auxiliary phase with its %d calls",
					stored[0], rec.Calls)
			}
			if stored[0].Person != rec.ActorSeat {
				t.Errorf("the record names person %q, want %q", stored[0].Person, rec.ActorSeat)
			}
		})
	}
}

// appendPhase writes one phase completion the way the publish listener does:
// tags and spend derived from the serialized event.
func appendPhase(t *testing.T, log *store.EventLog, id string, at time.Time,
	payload []byte, spend *store.Spend) {

	t.Helper()
	appendSpend(t, log, "agent_phase_completed", id, at, payload, spend)
}

// appendSpend writes one spend record of eventType, with the spend its
// producer derived for it.
func appendSpend(t *testing.T, log *store.EventLog, eventType, id string, at time.Time,
	payload []byte, spend *store.Spend) {

	t.Helper()
	if err := log.Append(t.Context(), store.EventRecord{
		ID: id, Type: eventType, Source: "engine",
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

// A COLLECTED LAUNCH IS COUNTED AS THE EXECUTOR'S SPEND.
//
// A launch's own record is the only place its tokens reach a reader: the
// executor's resumed record states the executor's round with no tokens, and
// nothing else carries the run's. So the record has to fold through the
// stored rollup and the live one alike — under the executor's phase, since it
// names none of its own and what it spent is the executor's, and under its own
// model and price. A record either producer dropped would be a run's whole
// spend missing from the screen that exists to show spend; one either filed
// under a phase of its own would be drawn as spend nobody's executor made.
//
// Mutation: leave a launch out of the store's spend types, or out of the live
// projection's, and that producer's rollup has no execute row; file it under
// no phase, and the row is the unknown one.
func TestACollectedLaunchIsCountedAsTheExecutorsSpend(t *testing.T) {
	t.Parallel()
	payload, err := json.Marshal(types.ExecutorCollected{
		Agent: "a-1", RoleName: "Lead", TurnID: "run-1", WorkKey: "wk-1",
		Iteration: 2, LaunchID: "job-1",
		Harness: "claude-code", Model: "claude-sonnet-5", ProviderKey: "plan",
		InputTokens: 9000, OutputTokens: 700, TotalTokens: 9700,
		CacheReadTokens: 6000, CacheWriteTokens: 1500, CostUSD: 0.42, HarnessTurns: 31,
		ActivityTranscript: "[tool] bash: go test ./...",
	})
	if err != nil {
		t.Fatal(err)
	}
	const launchType = "executor_collected"
	at := time.Now().UTC().Add(-time.Minute).Truncate(time.Second)
	log := open(t).Events()
	appendSpend(t, log, launchType, "l1", at, payload, store.SpendFor(launchType, payload))

	var body map[string]any
	if err := json.Unmarshal(payload, &body); err != nil {
		t.Fatal(err)
	}
	live := livestate.New()
	live.Apply(&livestate.Envelope{ID: "l1", Type: launchType,
		Timestamp: at.Format(time.RFC3339Nano), Category: "system", Payload: body})

	stored, held := phaseTokens(t, log), live.SpendRecords()
	if len(stored) != 1 || len(held) != 1 || stored[0] != held[0] {
		t.Errorf("the producers disagree about one launch:\n stored %+v\n   live %+v", stored, held)
	}
	for name, records := range map[string][]tokens.Record{"stored": stored, "live": held} {
		roll := tokens.Aggregate(records, tokens.Options{})
		if len(roll.ByPhase) != 1 || roll.ByPhase[0].Phase != string(types.PhaseExecute) {
			t.Errorf("%s: the rollup's phases are %+v, want the launch under the executor's", name, roll.ByPhase)
			continue
		}
		execute := roll.ByPhase[0]
		if execute.TotalTokens != 9700 || execute.CacheReadTokens != 6000 ||
			execute.CostUSD != 0.42 || execute.Calls != 31 {
			t.Errorf("%s: the executor's row holds %+v, want the launch's 9700 tokens, 6000 cached, "+
				"$0.42 over its harness's 31 turns", name, execute.Bucket)
		}
		if len(roll.ByModel) != 1 || roll.ByModel[0].Model != "claude-sonnet-5" {
			t.Errorf("%s: models = %+v, want the launch's own", name, roll.ByModel)
		}
	}
}
