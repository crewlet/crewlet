package types

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// A PHASE RECORD THAT MEASURED NOTHING SAYS SO BY OMISSION.
//
// A coding run's own `sandbox` record can have no start this build measured,
// and has no rounds of its own and no round cap; a call whose arguments did
// not parse is one nothing timed.
// Decoded and re-encoded (the store keeps the payload, a socket forwards it)
// such a record must stay that way: a zero time.Time marshals as the year 1,
// and a record reading "started 0001-01-01" is a timeline bar two thousand
// years long rather than an honest "not recorded".
func TestAPhaseRecordThatMeasuredNothingSaysSoByOmission(t *testing.T) {
	t.Parallel()
	untimed := `{"agent_id":"a1","role":"CTO","turn_id":"t1","iteration":1,"phase":"sandbox",` +
		`"launch_id":"launch-1","model":"m","input_tokens":100,"output_tokens":10,` +
		`"total_tokens":110,"cache_read_tokens":0,"cache_write_tokens":0,"duration_ms":4200,` +
		`"tool_executions":[{"name":"read_file","round":1,"success":true}]}`

	var rec AgentPhaseCompleted
	if err := json.Unmarshal([]byte(untimed), &rec); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if !rec.StartedAt.IsZero() || rec.Rounds != nil || rec.WorkItem != nil ||
		rec.MaxRounds != 0 || rec.HostRound != 0 {
		t.Errorf("an untimed record decoded with timing it never carried: %+v", rec)
	}
	again, err := json.Marshal(rec)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	for _, key := range []string{`"started_at"`, `"rounds"`, `"invocations"`, `"work_item"`,
		`"max_rounds"`, `"round_ceiling"`, `"host_round"`} {
		if strings.Contains(string(again), key) {
			t.Errorf("re-encoding an untimed record invented %s: %s", key, again)
		}
	}

	// The live frame keeps the same promise between rounds.
	var frame AgentTurnProgress
	if err := json.Unmarshal([]byte(`{"agent_id":"a1","turn_id":"t1","round_num":0}`), &frame); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	blob, _ := json.Marshal(frame)
	for _, key := range []string{`"round_started_at"`, `"run_started_at"`, `"running_calls"`, `"rounds"`,
		`"invocations"`, `"work_item"`} {
		if strings.Contains(string(blob), key) {
			t.Errorf("a frame with no round open states %s: %s", key, blob)
		}
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
		WorkItem: &WorkItem{Backend: WorkWorklet, ID: "task-7", Key: "ENG-7"},
		Rounds: []PhaseRound{{
			Round: 1, StartedAt: at, DurationMS: 2300, Model: "m",
			InputTokens: 1000, OutputTokens: 40, CacheReadTokens: 800, CacheWriteTokens: 50, ToolCalls: 2,
			StopReason: "cap",
		}},
		Invocations: []PhaseInvocation{{
			StartedAt: at, DurationMS: 2500, ProviderKey: "claude-max", Model: "m", Outcome: "cap",
			FirstRound: 1, LastRound: 1, RequestsObserved: true,
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
		len(back.Invocations) != 1 || back.Invocations[0] != rec.Invocations[0] ||
		back.CacheReadTokens != 800 || back.MaxRounds != 12 || back.RoundCeiling != 48 ||
		back.LaunchID != "launch-2" || back.WorkItem == nil || *back.WorkItem != *rec.WorkItem {
		t.Errorf("the record came back as %+v", back)
	}
}

// A FRAME LISTS EVERY CALL IN FLIGHT, each by its own id, and a run that made
// no request states its rounds as zero rather than leaving them out.
//
// A coding CLI may run a response's calls at once, so a frame carries a list,
// and the id is what tells two calls of one tool apart and what the call's
// `tool_executions` row is matched to once it answers. A run's FirstRound and
// LastRound are the join to `rounds[]`: zero is a statement — this run made no
// request — and it must survive the wire as one, never as an absent key a
// reader could take for an older record.
//
// Mutation: make RunningCalls a single value again, drop the id's tag, or give
// FirstRound omitempty, and this fails.
func TestAFrameListsEveryCallInFlightAndEveryRunItsRounds(t *testing.T) {
	t.Parallel()
	at := time.Date(2026, 10, 25, 9, 0, 0, 0, time.UTC)
	frame := AgentTurnProgress{
		TurnID: "t1", Phase: PhaseExecute,
		RunningCalls: []RunningCall{
			{ID: "toolu_1", Round: 3, Name: "read_file", Arguments: `{"path":"a"}`, StartedAt: at},
			{ID: "toolu_2", Round: 3, Name: "read_file", Arguments: `{"path":"b"}`, StartedAt: at},
		},
		Invocations: []PhaseInvocation{{StartedAt: at, Outcome: "failed"}},
	}
	blob, err := json.Marshal(frame)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	var wire map[string]any
	if err := json.Unmarshal(blob, &wire); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	running, _ := wire["running_calls"].([]any)
	if len(running) != 2 {
		t.Fatalf("running_calls = %v, want both calls in flight", wire["running_calls"])
	}
	for i, want := range []string{"toolu_1", "toolu_2"} {
		if got := running[i].(map[string]any)["id"]; got != want {
			t.Errorf("running call %d has id %v, want %q", i, got, want)
		}
	}
	run, _ := wire["invocations"].([]any)
	if len(run) != 1 {
		t.Fatalf("invocations = %v, want the one run", wire["invocations"])
	}
	for _, key := range []string{"first_round", "last_round", "requests_observed"} {
		if v, ok := run[0].(map[string]any)[key]; !ok || v == nil {
			t.Errorf("a run that made no request leaves %s out (%v); want it stated", key, v)
		}
	}
	var back AgentTurnProgress
	if err := json.Unmarshal(blob, &back); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if len(back.RunningCalls) != 2 || back.RunningCalls[1] != frame.RunningCalls[1] ||
		len(back.Invocations) != 1 || back.Invocations[0] != frame.Invocations[0] {
		t.Errorf("the frame came back as %+v", back)
	}
}
