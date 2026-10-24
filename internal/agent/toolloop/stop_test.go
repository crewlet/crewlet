package toolloop_test

import (
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/agent/toolloop"
	"github.com/crewlet/crewlet/internal/providers/llm"
)

// ARGUMENTS THAT DID NOT PARSE ARE ANSWERED, NOT RUN. The surface never sees
// the call; the model reads why as a FAILED result (the flag rides the tool
// message, which the backend renders as a failure) and its corrected call
// runs.
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
// flag is what the backend renders the failure from.
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

// EVERY ROUND CARRIES ITS STOP REASON, so a reader can tell a round that
// ended to run its calls from one that answered without inferring it from
// what the round happens to hold.
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
