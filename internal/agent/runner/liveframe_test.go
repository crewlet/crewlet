package runner

import (
	"fmt"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/agent/phase"
	"github.com/crewlet/crewlet/internal/agent/toolloop"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/queue"
)

// progressFrame publishes one live frame for res through the emitter every
// phase uses, over pub, and returns the frame as a consumer decodes it and what
// it weighed on the wire — or fails the test when none was accepted.
func progressFrame(t *testing.T, pub *transport, res toolloop.Result) (*types.AgentTurnProgress, int) {
	t.Helper()
	emitterOver(pub).progress(t.Context(), phase.Execute, 1, res)
	pub.mu.Lock()
	defer pub.mu.Unlock()
	for _, a := range pub.attempts {
		if a.typ != (types.AgentTurnProgress{}).EventType() {
			continue
		}
		if a.err != nil {
			t.Fatalf("the live frame was refused at %d bytes: %v", a.bytes, a.err)
		}
		frame, ok := a.ev.Data.(*types.AgentTurnProgress)
		if !ok {
			t.Fatalf("the live frame decoded as %T", a.ev.Data)
		}
		return frame, a.bytes
	}
	t.Fatal("no live frame was published")
	return nil, 0
}

// A LIVE FRAME CARRIES THE PHASE'S LATEST CALLS AND ROUNDS, AND COUNTS THE
// REST.
//
// A frame is republished for the length of the phase, and a phase has no
// bound of its own on how many calls it makes or how large their arguments
// are. Carrying every call whole, a long phase's frame grows past what the
// transport carries, every frame after that is refused, and the live row
// freezes for the rest of the phase. So a frame carries the latest calls and
// the latest narrated rounds, says how many came before each, and stands a
// marker in for arguments too large to repeat, saying where they are whole.
func TestALiveFrameCarriesTheLatestCallsAndCountsTheRest(t *testing.T) {
	t.Parallel()
	const calls, rounds = 3_000, 100
	res := toolloop.Result{RoundsUsed: rounds, Model: "claude-sonnet-5", Text: "working"}
	for i := range calls {
		res.Executions = append(res.Executions, toolloop.Execution{
			Round: 1 + i*rounds/calls, Name: fmt.Sprintf("call_%04d", i),
			Args: map[string]any{"path": fmt.Sprintf("/f/%d", i)}, Output: strings.Repeat("r", 5_000),
		})
	}
	// The latest calls post documents: arguments of megabytes each, which
	// three frames' worth of would be past the transport's ceiling alone.
	big := strings.Repeat("d", 3<<20)
	for i := calls - 3; i < calls; i++ {
		res.Executions[i].Args = map[string]any{"text": big}
	}
	for i := range rounds {
		res.Narration = append(res.Narration, toolloop.Narration{
			Round: i + 1, Reasoning: strings.Repeat("t", 10_000), Content: fmt.Sprintf("round %d", i+1),
		})
	}
	// The premise: whole, this frame is past the ceiling.
	if calls*5_000+3*len(big) <= queue.MaxPayloadBytes {
		t.Fatal("the fixture's calls fit one event whole; the case would test nothing")
	}

	frame, _ := progressFrame(t, &transport{refuse: tooLargeAbove(queue.MaxPayloadBytes)}, res)
	if got := len(frame.ToolExecutions); got != liveWindow || frame.ToolExecutionsEarlier != calls-liveWindow {
		t.Fatalf("the frame carries %d calls and counts %d before them; want the latest %d and %d counted",
			got, frame.ToolExecutionsEarlier, liveWindow, calls-liveWindow)
	}
	for i, row := range frame.ToolExecutions {
		if want := fmt.Sprintf("call_%04d", calls-liveWindow+i); row["name"] != want {
			t.Fatalf("carried call %d is %v; want %s — the frame's calls are the latest, in order",
				i, row["name"], want)
		}
		if result, _ := row["result"].(string); len(result) > partialTail+len("…") {
			t.Errorf("call %v carries a result of %d bytes, past its tail", row["name"], len(result))
		}
	}
	wholeArgs := encodeArgs(map[string]any{"text": big})
	for _, row := range frame.ToolExecutions[liveWindow-3:] {
		if row["arguments"] != liveArgsMarker(len(wholeArgs)) {
			t.Errorf("call %v carries arguments of %d bytes; want the marker saying where its %d are whole",
				row["name"], len(fmt.Sprint(row["arguments"])), len(wholeArgs))
		}
	}
	if small := frame.ToolExecutions[0]["arguments"]; small != encodeArgs(res.Executions[calls-liveWindow].Args) {
		t.Errorf("a call's short arguments went out as %v; want them as they were", small)
	}
	if got := len(frame.RoundNarration); got != liveWindow || frame.RoundNarrationEarlier != rounds-liveWindow {
		t.Fatalf("the frame carries %d rounds and counts %d before them; want the latest %d and %d counted",
			got, frame.RoundNarrationEarlier, liveWindow, rounds-liveWindow)
	}
	for _, row := range frame.RoundNarration {
		if reasoning, _ := row["reasoning"].(string); len(reasoning) > partialTail+len("…") ||
			!strings.HasPrefix(reasoning, "…") {
			t.Errorf("round %v carries %d bytes of reasoning; want its marked tail", row["round"], len(reasoning))
		}
	}
	if last := frame.RoundNarration[liveWindow-1]; last["content"] != fmt.Sprintf("round %d", rounds) {
		t.Errorf("the frame's last round says %v; want the phase's latest", last["content"])
	}
}

// A LIVE FRAME CARRIES THE TAIL OF EACH ABANDONED ATTEMPT.
//
// The attempts a provider gave up on ride the round in flight, which is
// republished five times a second for the life of the round, and a stream
// that failed late holds as much text as the model managed to write. Each one
// is cut to its marked tail like every other text on the frame — the end of
// it, where a reader watching it die was looking — and its whole is on the
// phase's completed record.
func TestALiveFrameCarriesTheTailOfEachAbandonedAttempt(t *testing.T) {
	t.Parallel()
	long := func(i int) string {
		return strings.Repeat("то, что было написано ", 600) + fmt.Sprintf("[end of attempt %d]", i)
	}
	partial := &toolloop.Partial{Round: 3, Reasoning: "third try", Content: "going"}
	for i := range 3 {
		partial.Abandoned = append(partial.Abandoned, toolloop.Narration{
			Round: 3, Reasoning: long(i) + " (reasoning)", Content: long(i),
		})
	}
	res := toolloop.Result{RoundsUsed: 2, Model: "claude-sonnet-5", Partial: partial}

	frame, _ := progressFrame(t, &transport{refuse: tooLargeAbove(queue.MaxPayloadBytes)}, res)
	abandoned, _ := frame.PartialRound["abandoned"].([]any)
	if len(abandoned) != len(partial.Abandoned) {
		t.Fatalf("the frame carries %d abandoned attempts; want every one of the %d",
			len(abandoned), len(partial.Abandoned))
	}
	for i, raw := range abandoned {
		row, _ := raw.(map[string]any)
		whole := partial.Abandoned[i]
		for key, text := range map[string]string{"reasoning": whole.Reasoning, "content": whole.Content} {
			got, _ := row[key].(string)
			if len(got) > partialTail+len("…") || !strings.HasPrefix(got, "…") {
				t.Errorf("abandoned attempt %d carries %d bytes of %s; want its marked tail, at most %d",
					i, len(got), key, partialTail+len("…"))
			}
			if !strings.HasSuffix(text, strings.TrimPrefix(got, "…")) {
				t.Errorf("abandoned attempt %d's %s on the frame is not the end of the attempt", i, key)
			}
		}
	}
}

// A LIVE FRAME AT EVERY BOUND STAYS UNDER THE CEILING.
//
// The window and the per-text bounds are what keep a frame publishable,
// however long the phase runs. Held full — every carried text at its bound, in
// the characters JSON writes six bytes for, and every failed call carrying its
// output twice, as result and as error — the frame still goes out, well under
// what the transport carries.
func TestALiveFrameAtEveryBoundStaysUnderTheCeiling(t *testing.T) {
	t.Parallel()
	worst := strings.Repeat("<", partialTail+1_000)
	// Arguments one byte under their bound, made of the character that
	// escapes to the most in a JSON string holding JSON text: a quote,
	// already escaped once by the encoding of the arguments themselves.
	var quotes string
	for n := 1; ; n++ {
		if len(encodeArgs(map[string]any{"a": strings.Repeat(`"`, n)})) > liveArgsBytes {
			quotes = strings.Repeat(`"`, n-1)
			break
		}
	}
	res := toolloop.Result{RoundsUsed: 2 * liveWindow, Model: "claude-sonnet-5", Text: worst,
		Partial: &toolloop.Partial{Round: 2*liveWindow + 1, Reasoning: worst, Content: worst}}
	for i := range 4 {
		res.Partial.Abandoned = append(res.Partial.Abandoned, toolloop.Narration{
			Round: res.Partial.Round, Reasoning: worst, Content: worst + fmt.Sprint(i),
		})
	}
	for i := range 2 * liveWindow {
		res.Executions = append(res.Executions, toolloop.Execution{
			Round: i + 1, Name: strings.Repeat("n", 128), Args: map[string]any{"a": quotes},
			Output: worst, Failed: true,
		})
		res.Narration = append(res.Narration, toolloop.Narration{Round: i + 1, Reasoning: worst, Content: worst})
	}

	frame, bytes := progressFrame(t, &transport{refuse: tooLargeAbove(queue.MaxPayloadBytes)}, res)
	if len(frame.ToolExecutions) != liveWindow || len(frame.RoundNarration) != liveWindow {
		t.Fatalf("the frame carries %d calls and %d rounds; the case needs the window full of both",
			len(frame.ToolExecutions), len(frame.RoundNarration))
	}
	if frame.ToolExecutions[0]["arguments"] != encodeArgs(map[string]any{"a": quotes}) {
		t.Fatal("the arguments at their bound were not carried whole; the case needs them at the bound")
	}
	if bytes > queue.MaxPayloadBytes*3/4 {
		t.Errorf("a frame at every bound weighs %d bytes, past three quarters of the %d-byte ceiling",
			bytes, queue.MaxPayloadBytes)
	}
	t.Logf("a live frame at every bound weighs %d bytes", bytes)
}
