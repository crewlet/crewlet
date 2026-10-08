package stream_test

import (
	"encoding/json"
	"fmt"
	"maps"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/api/livestate"
	"github.com/crewlet/crewlet/internal/api/stream"
	"github.com/crewlet/crewlet/internal/events/types"
)

// longPhase is the frames one long executor phase publishes, as the runner
// builds them: an opening frame carrying a 30 KB system prompt and an 8 KB
// task, then thirty rounds, each streamed for eight seconds at the loop's five
// frames a second, committed with its narration, and followed by its tool
// calls — announced as they start, listed as they return with their result's
// tail.
func longPhase(t *testing.T) []livestate.Envelope {
	t.Helper()
	const (
		rounds      = 30
		streamed    = 40 // frames a round streams: eight seconds at five a second
		reasoning   = 1500
		content     = 300
		argBytes    = 400
		resultBytes = 3000
	)
	system := strings.Repeat("You are the lead engineer. ", 30_000/27)
	task := strings.Repeat("Fix the flaky test in the payments suite. ", 8_000/42)
	start := time.Date(2026, 10, 6, 9, 0, 0, 0, time.UTC)
	at := start
	var out []livestate.Envelope
	seq := 0
	frame := func(p types.AgentTurnProgress) {
		t.Helper()
		p.Agent, p.RoleName, p.TurnID, p.Phase, p.Iteration = "agent-1", "Lead", "turn-1", "execute", 0
		p.Model, p.MaxRounds, p.RoundCeiling = "claude-sonnet-4-5", 40, 60
		raw, err := json.Marshal(p)
		if err != nil {
			t.Fatal(err)
		}
		var payload map[string]any
		if err := json.Unmarshal(raw, &payload); err != nil {
			t.Fatal(err)
		}
		seq++
		at = at.Add(200 * time.Millisecond)
		out = append(out, livestate.Envelope{
			ID: fmt.Sprintf("p%d", seq), Type: p.EventType(), Timestamp: at.Format(time.RFC3339Nano),
			Category: "task", Payload: payload,
		})
	}
	frame(types.AgentTurnProgress{
		Prompt: task, RoundNum: -1,
		PromptMessages: []types.PromptMessage{{Role: "system", Content: system}, {Role: "user", Content: task}},
	})
	var (
		narration []types.RoundNarration
		execs     []types.ToolExecution
		timed     []types.PhaseRound
	)
	snapshot := func() types.AgentTurnProgress {
		return types.AgentTurnProgress{
			ToolExecutions: append([]types.ToolExecution(nil), execs...),
			RoundNarration: append([]types.RoundNarration(nil), narration...),
			Rounds:         append([]types.PhaseRound(nil), timed...),
			Response:       strings.Repeat("r", min(4000, 300*len(narration))),
		}
	}
	for round := 1; round <= rounds; round++ {
		roundAt := at
		for i := 1; i <= streamed; i++ {
			p := snapshot()
			p.RoundNum, p.RoundStartedAt = round-2, roundAt
			p.PartialRound = map[string]any{
				"round": round, "reasoning": strings.Repeat("t", reasoning*i/streamed),
				"content": strings.Repeat("c", content*i/streamed),
			}
			frame(p)
		}
		narration = append(narration, types.RoundNarration{
			"round": round, "reasoning": strings.Repeat("t", reasoning), "content": strings.Repeat("c", content),
		})
		timed = append(timed, types.PhaseRound{Round: round, StartedAt: roundAt, DurationMS: 8000, InputTokens: 40_000})
		p := snapshot()
		p.RoundNum, p.RoundStartedAt = round-1, roundAt
		frame(p)
		calls := 1 + round%3/2
		for range calls {
			args := `{"path":"` + strings.Repeat("a", argBytes) + `"}`
			p := snapshot()
			p.RoundNum, p.RoundStartedAt = round-1, roundAt
			p.RunningCall = &types.RunningCall{Round: round, Name: "read_file", Arguments: args, StartedAt: at}
			frame(p)
			execs = append(execs, types.ToolExecution{
				"name": "read_file", "arguments": args, "result": strings.Repeat("x", resultBytes),
				"success": true, "round": round,
			})
			p = snapshot()
			p.RoundNum, p.RoundStartedAt = round-1, roundAt
			frame(p)
		}
	}
	return out
}

// A RUNNING PHASE'S PUSHES CARRY WHAT MOVED, and a tab that applies them holds
// the whole call — the one the projection holds, field for field, after every
// frame.
//
// Every push used to carry the whole row: over this long phase, 185 MiB to
// each open tab, 144 KiB a push, nearly all of it the prompt, the committed
// rounds and the tool calls the tab already held, re-sent five times a second
// while a round streamed. The heavy fields now go only when their version
// moved, so a streaming frame carries the round being written and the call's
// light fields — and what the tab rebuilds from the pushes is still exactly
// what a snapshot would have sent it.
//
// Mutation: push the rows whole again, and the phase costs its tabs the whole
// call on every frame; leave a moved field out, and the tab's copy falls
// behind the projection's.
func TestARunningPhasePushesWhatMovedAndTheTabHoldsTheWholeCall(t *testing.T) {
	t.Parallel()
	frames := longPhase(t)
	s, c := newService(t, stream.Options{})
	var held map[string]any
	lean, whole, streaming, largestStreaming := 0, 0, 0, 0
	for i, env := range frames {
		s.Ingest(env)
		wholeRow, err := stream.Encode(stream.Push(stream.KindAgents, s.State().OverlayRows([]string{"Lead"}), clock))
		if err != nil {
			t.Fatal(err)
		}
		whole += len(wholeRow)
		for _, out := range drain(c) {
			if out.Kind != stream.KindAgents {
				continue
			}
			raw, err := stream.Encode(out)
			if err != nil {
				t.Fatal(err)
			}
			lean += len(raw)
			call := pushedCall(t, raw)
			if _, partial := env.Payload["partial_round"]; partial && !carriesDetail(call) {
				streaming++
				largestStreaming = max(largestStreaming, len(raw))
			}
			var behind bool
			if held, behind = mergeLikeTheDashboard(held, call); behind {
				t.Fatalf("frame %d: a tab that saw every push was told it is behind", i)
			}
		}
		want := wholeCall(t, s)
		if !reflect.DeepEqual(held, want) {
			t.Fatalf("after frame %d the tab holds a call other than the projection's:\n got %v\nwant %v",
				i, keysOf(held), keysOf(want))
		}
	}
	t.Logf("%d frames: %.1f MiB pushed, against %.1f MiB had every push carried the whole row; "+
		"a streaming frame at most %.1f KiB", len(frames), float64(lean)/(1<<20), float64(whole)/(1<<20),
		float64(largestStreaming)/1024)
	if streaming == 0 {
		t.Fatal("no streaming frame was pushed without a heavy field, so this certifies nothing")
	}
	if lean*10 > whole {
		t.Errorf("the phase pushed %d bytes, against %d whole; want under a tenth", lean, whole)
	}
	// A streaming frame is the round being written (two 4 KB tails at most)
	// and the call's light fields.
	if largestStreaming > 16<<10 {
		t.Errorf("a streaming frame weighed %d bytes; want the round being written and the light fields only",
			largestStreaming)
	}
}

// pushedCall is the one live call an `agents` frame carries, as JSON.
func pushedCall(t *testing.T, raw []byte) map[string]any {
	t.Helper()
	var env struct {
		Data []struct {
			LiveCall map[string]any `json:"live_call"`
		} `json:"data"`
	}
	if err := json.Unmarshal(raw, &env); err != nil || len(env.Data) != 1 {
		t.Fatalf("agents frame %s: %v", raw, err)
	}
	return env.Data[0].LiveCall
}

// carriesDetail reports whether a pushed call carries any heavy field.
func carriesDetail(call map[string]any) bool {
	for _, fields := range livestate.CallDetail {
		for _, f := range fields {
			if _, ok := call[f]; ok {
				return true
			}
		}
	}
	return false
}

// mergeLikeTheDashboard is the dashboard's `mergeLiveCall`
// (dashboard/src/protocol/store.ts), rule for rule: a heavy field the push
// carries is taken unless its version is older than the one held for the same
// call; one it leaves out is the copy held, for the same call, and nothing for
// a call of its own; and a push that leaves out a field at a version newer
// than the one held says the tab is behind.
func mergeLikeTheDashboard(held, next map[string]any) (map[string]any, bool) {
	if next == nil {
		return nil, false
	}
	same := held != nil && held["turn_id"] == next["turn_id"] && held["phase"] == next["phase"] &&
		held["iteration"] == next["iteration"]
	version := func(call map[string]any, detail string) float64 {
		v, _ := call["versions"].(map[string]any)[detail].(float64)
		return v
	}
	out := maps.Clone(next)
	versions := map[string]any{}
	behind := false
	for detail, fields := range livestate.CallDetail {
		pushed, have := version(next, detail), 0.0
		if same {
			have = version(held, detail)
		}
		carried := true
		for _, f := range fields {
			if _, ok := next[f]; !ok {
				carried = false
			}
		}
		if carried && pushed >= have {
			versions[detail] = pushed
			continue
		}
		for _, f := range fields {
			if same {
				out[f] = held[f]
			} else {
				delete(out, f)
			}
		}
		versions[detail] = have
		if !carried && pushed > have {
			behind = true
		}
	}
	out["versions"] = versions
	return out, behind
}

// A TAB THAT MISSED A CALL'S CLEARING AND THE FIRST PUSH AFTER IT IS TOLD IT
// IS BEHIND when the call is built again under the same key. A suspended
// Execute phase does exactly that: its completion checkpoint clears the call,
// and its resumed rounds stream under the same turn, phase and iteration. The
// hub drops a slow tab's oldest envelopes, so the tab can lose both pushes —
// and with versions counted per call, the rebuilt call's were below the ones
// the tab held, so it kept the call from before the suspension as current and
// asked for nothing.
//
// Mutation: number a call of its own from one again, and the tab keeps the
// suspended call's response, narration and tool calls.
func TestATabThatMissedACallsClearingIsToldItIsBehind(t *testing.T) {
	t.Parallel()
	frames := longPhase(t)
	s, c := newService(t, stream.Options{})
	var held map[string]any
	apply := func() bool {
		t.Helper()
		behind := false
		for _, out := range drain(c) {
			if out.Kind != stream.KindAgents {
				continue
			}
			var stale bool
			held, stale = mergeLikeTheDashboard(held, pushedCall(t, mustEncode(t, out)))
			behind = behind || stale
		}
		return behind
	}
	// The phase runs until it suspends, and the tab sees all of it.
	suspendAt := len(frames) / 3
	for _, env := range frames[:suspendAt] {
		s.Ingest(env)
		apply()
	}
	if held == nil || held["response"] == nil {
		t.Fatal("the tab holds no call before the suspension; this case needs one")
	}
	// The checkpoint, and the resumed loop's first round: both pushes are
	// the ones the hub drops.
	checkpoint, err := time.Parse(time.RFC3339Nano, frames[suspendAt-1].Timestamp)
	if err != nil {
		t.Fatal(err)
	}
	s.Ingest(livestate.Envelope{
		ID: "checkpoint", Type: "agent_phase_completed", Timestamp: checkpoint.Add(time.Second).Format(time.RFC3339Nano),
		Category: "task", Payload: map[string]any{"role": "Lead", "turn_id": "turn-1", "phase": "execute", "iteration": float64(0)},
	})
	resumed := frames[2*len(frames)/3:]
	s.Ingest(resumed[0])
	drain(c)
	// The next push reaches the tab, and a tab told it is behind asks for
	// the call whole (`live_call`), which it merges like a push carrying
	// everything.
	s.Ingest(resumed[1])
	if apply() {
		var stale bool
		if held, stale = mergeLikeTheDashboard(held, wholeCall(t, s)); stale {
			t.Fatal("the whole call still left the tab behind")
		}
	}
	if want := wholeCall(t, s); !reflect.DeepEqual(held, want) {
		for _, fields := range livestate.CallDetail {
			for _, f := range fields {
				if !reflect.DeepEqual(held[f], want[f]) {
					t.Errorf("the tab holds %s from before the suspension", f)
				}
			}
		}
		if !t.Failed() {
			t.Errorf("the tab holds\n%v\nwant\n%v", held, want)
		}
	}
}

// wholeCall is the projection's call, as a snapshot carries it.
func wholeCall(t *testing.T, s *stream.Service) map[string]any {
	t.Helper()
	raw, err := json.Marshal(s.State().AgentOverlay("Lead").LiveCall)
	if err != nil {
		t.Fatal(err)
	}
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatal(err)
	}
	return out
}

func keysOf(m map[string]any) []string { return slices.Sorted(maps.Keys(m)) }

// A PUSH NAMES EVERY VERSION, so the tab that MISSED one knows from the next.
// The socket hub drops a slow tab's oldest envelope; the push that carried a
// field may be the one dropped, and the next one, leaving the field out, names
// a version past the copy that tab holds — which is what makes it ask for the
// call whole rather than draw an older copy as current.
func TestAPushAfterADroppedOneSaysWhatTheTabMissed(t *testing.T) {
	t.Parallel()
	s, c := newService(t, stream.Options{})
	frames := longPhase(t)
	for _, env := range frames[:3] {
		s.Ingest(env)
	}
	before := pushedCall(t, mustEncode(t, lastAgents(t, drain(c))))
	held := before["versions"].(map[string]any)["narration"].(float64)

	// The frame that commits the first round, whose push this tab never gets.
	commit := 0
	for i, env := range frames {
		if env.Payload["partial_round"] == nil && i > 0 {
			commit = i
			break
		}
	}
	for _, env := range frames[3 : commit+1] {
		s.Ingest(env)
	}
	drain(c) // dropped
	s.Ingest(frames[commit+1])
	next := pushedCall(t, mustEncode(t, lastAgents(t, drain(c))))
	if _, carried := next["round_narration"]; carried {
		t.Fatalf("the push after the commit carried the narration again; this case needs one that does not")
	}
	if got := next["versions"].(map[string]any)["narration"].(float64); got <= held {
		t.Errorf("the push after a dropped one names narration %v, the copy the tab holds is %v; "+
			"want a later version, so the tab knows it missed one", got, held)
	}
}

func lastAgents(t *testing.T, got []stream.Envelope) stream.Envelope {
	t.Helper()
	for i := len(got) - 1; i >= 0; i-- {
		if got[i].Kind == stream.KindAgents {
			return got[i]
		}
	}
	t.Fatal("no agents push")
	return stream.Envelope{}
}

func mustEncode(t *testing.T, env stream.Envelope) []byte {
	t.Helper()
	raw, err := stream.Encode(env)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}
