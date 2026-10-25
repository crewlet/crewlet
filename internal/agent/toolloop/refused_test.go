package toolloop_test

import (
	"context"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/agent/execstate"
	"github.com/crewlet/crewlet/internal/agent/toolloop"
	"github.com/crewlet/crewlet/internal/providers/llm"
)

// recordingSurface is a [fakeSurface] that keeps a record of refusals, as
// the Dashboard Assistant's surface draws a refused step for each.
type recordingSurface struct {
	fakeSurface
	refused []string
}

func (s *recordingSurface) Refused(_ context.Context, call llm.ToolCall, reason string) {
	s.refused = append(s.refused, call.Name+": "+reason)
}

// A CALL THE LOOP DOES NOT RUN IS ON THE RECORD, never a round that silently
// asked for nothing: a reply the backend could not read names its calls
// ([llm.Completion.RefusedCalls]), and the surface is told each one did not
// run, before the corrective asks again; a call whose arguments did not parse
// is told the same way.
//
// Mutation: drop the refused call in the protocol branch, or in the
// ArgumentsError path, and the surface's record is empty.
func TestACallTheLoopDoesNotRunIsOnTheRecord(t *testing.T) {
	t.Parallel()
	p := &scriptedProvider{turns: []llm.Completion{
		{Content: "I've prepared a proposal.", ProtocolError: "it wrote a call outside its envelope",
			RefusedCalls: []llm.ToolCall{toolCall("cli_0", "propose")}},
		{ToolCalls: []llm.ToolCall{{ID: "cli_0", Name: "read", ArgumentsError: `"arguments" is a list`}}},
		{ToolCalls: []llm.ToolCall{toolCall("cli_0", "propose")}},
		{Content: "Proposed."},
	}}
	s := &recordingSurface{fakeSurface: fakeSurface{tools: []llm.ToolDef{def("read"), def("propose")}}}
	res, err := toolloop.Run(t.Context(), toolloop.Config{
		Provider: p, Surface: s, MaxRounds: 6,
		Messages: []llm.Message{{Role: llm.RoleUser, Content: "add a role"}},
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(s.refused) != 2 ||
		!strings.HasPrefix(s.refused[0], "propose: ") ||
		!strings.Contains(s.refused[0], "it wrote a call outside its envelope") ||
		!strings.HasPrefix(s.refused[1], "read: ") ||
		!strings.Contains(s.refused[1], `"arguments" is a list`) {
		t.Fatalf("refused %q, want the faulted reply's call and the unparsed one, each with why", s.refused)
	}
	if strings.Join(s.ran, ",") != "propose" {
		t.Errorf("ran %v, want only the call the model made again through the contract", s.ran)
	}
	for _, n := range res.Narration {
		if strings.Contains(n.Content, "prepared") {
			t.Errorf("the faulted reply's claim was narrated: %q", n.Content)
		}
	}
}

// EVERY CALL AFTER THE ONE THAT SUSPENDED ITS ROUND IS ANSWERED, as refused
// and with why, so the parked conversation leaves exactly the suspending
// call unanswered and the model, resumed, never believes the later ones ran.
//
// Mutation: return at the suspend without answering the rest, and the
// suspended state fails its invariant with more than one dangling call.
func TestCallsAfterASuspendAreAnsweredAsNotRun(t *testing.T) {
	t.Parallel()
	p := &scriptedProvider{turns: []llm.Completion{{ToolCalls: []llm.ToolCall{
		toolCall("c1", "read"), toolCall("c2", "propose"), toolCall("c3", "propose_more"),
	}}}}
	s := &recordingSurface{fakeSurface: fakeSurface{
		tools:   []llm.ToolDef{def("read"), def("propose"), def("propose_more")},
		results: map[string]toolloop.ToolResult{"propose": {Suspend: true, Output: "waiting"}},
	}}
	res, err := toolloop.Run(t.Context(), toolloop.Config{
		Provider: p, Surface: s, MaxRounds: 3, AllowSuspend: true,
		Messages: []llm.Message{{Role: llm.RoleUser, Content: "two changes"}},
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !res.Suspended || res.PendingToolCallID != "c2" {
		t.Fatalf("suspended %v on %q, want on c2", res.Suspended, res.PendingToolCallID)
	}
	if strings.Join(s.ran, ",") != "read,propose" {
		t.Errorf("ran %v, want nothing after the suspending call", s.ran)
	}
	if len(s.refused) != 1 || !strings.HasPrefix(s.refused[0], "propose_more: ") {
		t.Errorf("refused %q, want the call after the suspend on the record", s.refused)
	}
	var answer string
	for _, m := range res.Messages {
		if m.Role == llm.RoleTool && m.ToolCallID == "c3" {
			answer = m.Content
		}
	}
	if !holdsAll(answer, "propose_more was NOT run", "propose") {
		t.Errorf("c3 was answered %q, want it told it did not run and why", answer)
	}
	state := execstate.State{Version: execstate.Version, Messages: res.Messages,
		PendingCallID: res.PendingToolCallID, PendingCallName: res.PendingToolName}
	if err := state.Validate(); err != nil {
		t.Errorf("the suspended conversation is not resumable: %v", err)
	}
}

// holdsAll reports whether s holds every part.
func holdsAll(s string, parts ...string) bool {
	for _, p := range parts {
		if !strings.Contains(s, p) {
			return false
		}
	}
	return true
}
