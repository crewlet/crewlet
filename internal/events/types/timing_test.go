package types

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// A PHASE RECORD FROM AN OLDER PEER HAS NO TIMING — and says so by omission.
//
// A rolling upgrade puts records from a build that measured nothing beside ones
// that did. Decoded here they carry no start and no rounds, and re-encoded
// (the store keeps the payload, a socket forwards it) they must stay that way:
// a zero time.Time marshals as the year 1, and a record reading "started
// 0001-01-01" is a timeline bar two thousand years long rather than an honest
// "not recorded".
func TestAPhaseRecordFromAnOlderPeerHasNoTiming(t *testing.T) {
	t.Parallel()
	older := `{"agent_id":"a1","role":"CTO","turn_id":"t1","iteration":1,"phase":"execute",` +
		`"model":"m","input_tokens":100,"output_tokens":10,"total_tokens":110,"rounds_used":2,` +
		`"duration_ms":4200,"tool_executions":[{"name":"read_file","round":1,"success":true}]}`

	var rec AgentPhaseCompleted
	if err := json.Unmarshal([]byte(older), &rec); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if !rec.StartedAt.IsZero() || rec.Rounds != nil || rec.WorkItem != nil ||
		rec.MaxRounds != 0 || rec.CacheReadTokens != 0 || rec.HostRound != 0 || rec.LaunchID != "" {
		t.Errorf("an older record decoded with timing it never carried: %+v", rec)
	}
	again, err := json.Marshal(rec)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	for _, key := range []string{`"started_at"`, `"rounds"`, `"work_item"`, `"max_rounds"`,
		`"round_ceiling"`, `"host_round"`, `"launch_id"`} {
		if strings.Contains(string(again), key) {
			t.Errorf("re-encoding an older record invented %s: %s", key, again)
		}
	}

	// The live frame and the prefetch summary keep the same promise.
	var frame AgentTurnProgress
	if err := json.Unmarshal([]byte(`{"agent_id":"a1","turn_id":"t1","round_num":0}`), &frame); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	blob, _ := json.Marshal(frame)
	for _, key := range []string{`"round_started_at"`, `"running_call"`, `"rounds"`, `"work_item"`} {
		if strings.Contains(string(blob), key) {
			t.Errorf("a frame with no round open states %s: %s", key, blob)
		}
	}
	blob, _ = json.Marshal(PrefetchSummary{TurnID: "t1"})
	if strings.Contains(string(blob), `"started_at"`) {
		t.Errorf("an unmeasured prefetch states a start: %s", blob)
	}
}

// AND ONE FROM THIS BUILD KEEPS ALL OF IT, through the JSON every store and
// socket puts it through.
func TestAPhaseRecordsTimingSurvivesTheWire(t *testing.T) {
	t.Parallel()
	at := time.Date(2026, 9, 24, 10, 0, 0, 123_000_000, time.UTC)
	rec := AgentPhaseCompleted{
		TurnID: "t1", Phase: PhaseExecute, StartedAt: at,
		CacheReadTokens: 800, CacheWriteTokens: 50, MaxRounds: 12, RoundCeiling: 48,
		LaunchID: "launch-2",
		WorkItem: &WorkItem{Backend: WorkNative, ID: "task-7", Key: "ENG-7"},
		Rounds: []PhaseRound{{
			Round: 1, StartedAt: at, DurationMS: 2300, Model: "m",
			InputTokens: 1000, OutputTokens: 40, CacheReadTokens: 800, CacheWriteTokens: 50, ToolCalls: 2,
		}},
	}
	blob, err := json.Marshal(rec)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	var back AgentPhaseCompleted
	if err := json.Unmarshal(blob, &back); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if !back.StartedAt.Equal(at) || len(back.Rounds) != 1 || back.Rounds[0] != rec.Rounds[0] ||
		back.CacheReadTokens != 800 || back.MaxRounds != 12 || back.RoundCeiling != 48 ||
		back.LaunchID != "launch-2" || back.WorkItem == nil || *back.WorkItem != *rec.WorkItem {
		t.Errorf("the record came back as %+v", back)
	}
}
