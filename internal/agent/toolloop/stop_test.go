package toolloop_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/agent/toolloop"
	"github.com/crewlet/crewlet/internal/providers/llm"
)

// A ROUND THE OUTPUT CAP CUT OFF ENDS THE PHASE BY NAME, AND ITS CALLS DO NOT
// RUN. The call the cap cut is not the call the model meant — on the Anthropic
// stream its arguments arrive as `{}` — so running it is a write nobody asked
// for. The round is still recorded and charged, with its stop reason, and its
// prose stays on the failure record as how far the model got.
func TestARoundCutOffAtTheCapRunsNothingAndFailsByName(t *testing.T) {
	t.Parallel()
	p := &scriptedProvider{turns: []llm.Completion{
		{Content: "Posting the summary now", StopReason: llm.StopMaxTokens,
			InputTokens: 10, OutputTokens: 5,
			ToolCalls: []llm.ToolCall{toolCall("1", "post")}},
	}}
	s := &fakeSurface{tools: []llm.ToolDef{def("post")}}
	m := &meter{}
	progress := &toolloop.Progress{}

	res, err := toolloop.Run(t.Context(), toolloop.Config{
		Provider: p, Surface: s, MaxRounds: 5, Budget: m, Progress: progress,
		Messages: []llm.Message{{Role: llm.RoleUser, Content: "go"}},
	})
	if res != nil || err == nil {
		t.Fatalf("Run = %+v, %v — want the phase to fail", res, err)
	}
	var stop *toolloop.StopError
	if !errors.As(err, &stop) || stop.Reason != llm.StopMaxTokens || stop.Calls != 1 || stop.Round != 1 {
		t.Fatalf("err = %v, want a StopError naming max_tokens, round 1 and its one call", err)
	}
	if len(s.ran) != 0 {
		t.Fatalf("ran %v — a call the cap cut off must not run", s.ran)
	}
	if p.calls != 1 {
		t.Fatalf("%d provider calls — a truncated round is not re-asked", p.calls)
	}
	if m.spent != 15 {
		t.Fatalf("charged %d, want the round's 15: it was billed", m.spent)
	}
	snap := progress.Snapshot()
	if len(snap.Rounds) != 1 || snap.Rounds[0].StopReason != llm.StopMaxTokens {
		t.Fatalf("rounds = %+v, want the cut round on the failure record with its stop reason", snap.Rounds)
	}
	if len(snap.Narration) != 1 || snap.Narration[0].Content != "Posting the summary now" {
		t.Fatalf("narration = %+v, want what the model wrote before the cut", snap.Narration)
	}
}

// EVERY STOP REASON THAT ENDS A PHASE ENDS IT AHEAD OF THE CORRECTIVES. An
// empty truncated round is otherwise an "empty answer", and the corrective
// re-asks the same model with a LONGER conversation — exactly wrong for a full
// context window — and a paused turn is a protocol state this engine never
// asked for. The controls are the ordinary reasons and the zero value, which
// a backend that reports none hands back: those run the loop as before.
func TestStopReasonsThatEndAPhaseComeBeforeTheCorrectives(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		reason llm.StopReason
		ends   bool
	}{
		{llm.StopMaxTokens, true},
		{llm.StopContextExceeded, true},
		{llm.StopPaused, true},
		{llm.StopEnd, false},
		{llm.StopToolUse, false},
		{"", false},
	} {
		t.Run(string(tc.reason)+"_", func(t *testing.T) {
			t.Parallel()
			// An empty round, in a loop with no terminator: the shape
			// that draws the empty-answer corrective.
			p := &scriptedProvider{turns: []llm.Completion{{StopReason: tc.reason}}}
			_, err := toolloop.Run(t.Context(), toolloop.Config{
				Provider: p, Surface: &fakeSurface{}, MaxRounds: 5,
				Messages: []llm.Message{{Role: llm.RoleUser, Content: "go"}},
			})
			var stop *toolloop.StopError
			if got := errors.As(err, &stop); got != tc.ends {
				t.Fatalf("err = %v, want a StopError: %v", err, tc.ends)
			}
			if tc.ends {
				if stop.Reason != tc.reason || !strings.Contains(err.Error(), string(tc.reason)) {
					t.Fatalf("err = %v, want it to name %s", err, tc.reason)
				}
				if p.calls != 1 {
					t.Fatalf("%d provider calls — the corrective fired on a round that cannot be corrected", p.calls)
				}
				return
			}
			if err != nil {
				t.Fatalf("Run: %v", err)
			}
			if p.calls != 2 {
				t.Fatalf("%d provider calls, want the empty-answer corrective's second", p.calls)
			}
		})
	}
}

// A REFUSAL ENDS THE PHASE AS THE PROVIDER'S OWN CLASSIFIED ERROR, with no
// corrective and no second ask — the model that declined is not asked to
// reconsider. The refused call was billed, so it is a round on the record,
// charged, with the stop reason that says what it was.
func TestARefusalEndsThePhaseWithoutAReprompt(t *testing.T) {
	t.Parallel()
	refused := &llm.Completion{Model: "claude-x", ProviderKey: "primary",
		InputTokens: 30, OutputTokens: 2, StopReason: llm.StopRefusal}
	p := &scriptedProvider{failAt: 1, err: llm.Refused("anthropic", "claude-x",
		&llm.Refusal{Category: "cyber", Completion: refused})}
	m := &meter{}
	progress := &toolloop.Progress{}

	_, err := toolloop.Run(t.Context(), toolloop.Config{
		Provider: p, Surface: &fakeSurface{tools: []llm.ToolDef{def("submit")}},
		MaxRounds: 5, Budget: m, Progress: progress, TerminateAfter: []string{"submit"},
		Messages: []llm.Message{{Role: llm.RoleUser, Content: "go"}},
	})
	if llm.KindOf(err) != llm.KindRefusal {
		t.Fatalf("err = %v (kind %s), want the refusal", err, llm.KindOf(err))
	}
	if p.calls != 1 {
		t.Fatalf("%d provider calls — a refusal was re-asked", p.calls)
	}
	if m.spent != 32 {
		t.Fatalf("charged %d, want the refused call's 32", m.spent)
	}
	snap := progress.Snapshot()
	if len(snap.Rounds) != 1 || snap.Rounds[0].StopReason != llm.StopRefusal ||
		snap.Rounds[0].Model != "claude-x" || snap.InputTokens != 30 {
		t.Fatalf("failure record = %+v, want the refused round on it", snap)
	}
	if snap.Model != "claude-x" || snap.ProviderKey != "primary" {
		t.Fatalf("served by %q/%q, want the refused call's model and entry", snap.Model, snap.ProviderKey)
	}
}

// A REFUSAL HANDED BACK AS AN ANSWER is still a refusal. The contract says a
// backend returns it as an error; a third-party provider that returns it as a
// completion must not have it read as a round of prose and corrected.
func TestARefusalReturnedAsACompletionIsStillARefusal(t *testing.T) {
	t.Parallel()
	p := &scriptedProvider{turns: []llm.Completion{
		{Content: "I can't help with that.", StopReason: llm.StopRefusal, InputTokens: 4},
	}}
	_, err := toolloop.Run(t.Context(), toolloop.Config{
		Provider: p, Surface: &fakeSurface{}, MaxRounds: 5,
		Messages: []llm.Message{{Role: llm.RoleUser, Content: "go"}},
	})
	if llm.KindOf(err) != llm.KindRefusal || p.calls != 1 {
		t.Fatalf("err = %v after %d calls, want one refusal", err, p.calls)
	}
}

// A refused charge on a refused round keeps BOTH facts, the refusal first: it
// is why the phase ended, and the budget's refusal is still findable.
func TestARefusalWhoseChargeIsRefusedKeepsBothFacts(t *testing.T) {
	t.Parallel()
	p := &scriptedProvider{failAt: 1, err: llm.Refused("anthropic", "m",
		&llm.Refusal{Completion: &llm.Completion{InputTokens: 50}})}
	_, err := toolloop.Run(t.Context(), toolloop.Config{
		Provider: p, Surface: &fakeSurface{}, MaxRounds: 5, Budget: &meter{refuseAt: 10},
		Messages: []llm.Message{{Role: llm.RoleUser, Content: "go"}},
	})
	if llm.KindOf(err) != llm.KindRefusal || !errors.Is(err, toolloop.ErrBudgetExhausted) {
		t.Fatalf("err = %v, want the refusal and the budget's refusal", err)
	}
}

// ARGUMENTS THAT DID NOT PARSE ARE ANSWERED, NOT RUN. The surface never sees
// the call; the model reads why as a FAILED result (the flag rides the tool
// message, for a vendor with an is_error field) and its corrected call runs.
// The round's other call runs as normal.
func TestACallWhoseArgumentsDidNotParseIsAnsweredNotRun(t *testing.T) {
	t.Parallel()
	bad := llm.ToolCall{ID: "1", Name: "post", Arguments: map[string]any{},
		ArgumentsError: "unexpected end of JSON input"}
	p := &scriptedProvider{turns: []llm.Completion{
		{ToolCalls: []llm.ToolCall{bad, toolCall("2", "read")}},
		{ToolCalls: []llm.ToolCall{{ID: "3", Name: "post", Arguments: map[string]any{"body": "x"}}}},
		{Content: "done"},
	}}
	s := &fakeSurface{tools: []llm.ToolDef{def("post"), def("read")}}

	res, err := toolloop.Run(t.Context(), toolloop.Config{
		Provider: p, Surface: s, MaxRounds: 5,
		Messages: []llm.Message{{Role: llm.RoleUser, Content: "go"}},
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if strings.Join(s.ran, ",") != "read,post" {
		t.Fatalf("ran %v, want the unparsed call skipped and its retry run", s.ran)
	}
	first := res.Executions[0]
	if first.Name != "post" || !first.Failed || !strings.Contains(first.Output, "unexpected end of JSON input") {
		t.Fatalf("first execution = %+v, want a failed row naming why", first)
	}
	answer := p.seen[1].Messages[2]
	if answer.Role != llm.RoleTool || answer.ToolCallID != "1" || !answer.Failed ||
		!strings.Contains(answer.Content, "NOT run") {
		t.Fatalf("the model was answered %+v, want a failed tool result for call 1", answer)
	}
}

// A FAILED TOOL'S RESULT CARRIES THE FLAG, and a successful one does not: the
// flag is what a vendor's is_error is rendered from.
func TestAFailedToolResultIsFlagged(t *testing.T) {
	t.Parallel()
	p := &scriptedProvider{turns: []llm.Completion{
		{ToolCalls: []llm.ToolCall{toolCall("1", "bad"), toolCall("2", "good")}},
		{Content: "done"},
	}}
	s := &fakeSurface{
		tools:   []llm.ToolDef{def("bad"), def("good")},
		results: map[string]toolloop.ToolResult{"bad": {Output: "refused", Failed: true}},
	}
	if _, err := toolloop.Run(t.Context(), toolloop.Config{
		Provider: p, Surface: s, MaxRounds: 5,
		Messages: []llm.Message{{Role: llm.RoleUser, Content: "go"}},
	}); err != nil {
		t.Fatalf("Run: %v", err)
	}
	msgs := p.seen[1].Messages
	if !msgs[2].Failed || msgs[3].Failed {
		t.Fatalf("tool messages = %+v / %+v, want only the failed one flagged", msgs[2], msgs[3])
	}
}

// EVERY ROUND CARRIES ITS STOP REASON, so a reader can tell a round the model
// finished from one something cut short without inferring it.
func TestEveryRoundRecordsItsStopReason(t *testing.T) {
	t.Parallel()
	p := &scriptedProvider{turns: []llm.Completion{
		{StopReason: llm.StopToolUse, ToolCalls: []llm.ToolCall{toolCall("1", "read")}},
		{Content: "done", StopReason: llm.StopEnd},
	}}
	res, err := toolloop.Run(t.Context(), toolloop.Config{
		Provider: p, Surface: &fakeSurface{tools: []llm.ToolDef{def("read")}}, MaxRounds: 5,
		Messages: []llm.Message{{Role: llm.RoleUser, Content: "go"}},
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(res.Rounds) != 2 || res.Rounds[0].StopReason != llm.StopToolUse ||
		res.Rounds[1].StopReason != llm.StopEnd {
		t.Fatalf("rounds = %+v", res.Rounds)
	}
}
